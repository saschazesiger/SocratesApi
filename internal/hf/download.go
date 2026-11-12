package hf

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Progress is the state of one file being downloaded.
type Progress struct {
	ModelID     string  `json:"modelId"`
	File        string  `json:"file"`
	FileIndex   int     `json:"fileIndex"`
	FileCount   int     `json:"fileCount"`
	Done        int64   `json:"done"`  // bytes of all files of the model
	Total       int64   `json:"total"` // bytes of all files of the model
	BytesPerSec float64 `json:"bytesPerSec"`
	State       string  `json:"state"` // queued, downloading, verifying
}

// Job is one model to download.
type Job struct {
	ModelID  string
	Repo     string
	Revision string
	Dir      string
	Files    []RepoFile
}

// Downloader fetches models file by file, several ranges in parallel, and
// resumes where it stopped (also after a restart of the container).
type Downloader struct {
	Client *Client
	// Done is called when a job finished; err is nil on success.
	Done func(job Job, err error)
	// Changed is called when the progress moved.
	Changed func()

	mu      sync.Mutex
	queue   []Job
	current *active
	cancels map[string]bool
	wake    chan struct{}
}

type active struct {
	job       Job
	fileIndex atomic.Int32
	done      atomic.Int64 // bytes of the model done
	total     int64
	state     atomic.Value
	cancel    context.CancelFunc
	rate      float64
	lastBytes int64
	lastAt    time.Time
}

// NewDownloader returns a downloader; start it with Run.
func NewDownloader(c *Client) *Downloader {
	return &Downloader{Client: c, cancels: map[string]bool{}, wake: make(chan struct{}, 1)}
}

// Add queues a job.
func (d *Downloader) Add(j Job) {
	d.mu.Lock()
	delete(d.cancels, j.ModelID)
	d.queue = append(d.queue, j)
	d.mu.Unlock()
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Cancel stops and forgets the job of a model.
func (d *Downloader) Cancel(modelID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cancels[modelID] = true
	q := d.queue[:0]
	for _, j := range d.queue {
		if j.ModelID != modelID {
			q = append(q, j)
		}
	}
	d.queue = q
	if d.current != nil && d.current.job.ModelID == modelID {
		d.current.cancel()
	}
}

// Progress lists what is downloading or queued.
func (d *Downloader) Progress() []Progress {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []Progress{}
	if a := d.current; a != nil {
		done := a.done.Load()
		now := time.Now()
		if dt := now.Sub(a.lastAt).Seconds(); dt >= 1 {
			inst := float64(done-a.lastBytes) / dt
			if a.rate == 0 {
				a.rate = inst
			} else {
				a.rate = 0.6*a.rate + 0.4*inst
			}
			a.lastBytes, a.lastAt = done, now
		}
		st, _ := a.state.Load().(string)
		out = append(out, Progress{ModelID: a.job.ModelID, File: a.job.Files[a.fileIndex.Load()].Name, FileIndex: int(a.fileIndex.Load()),
			FileCount: len(a.job.Files), Done: done, Total: a.total, BytesPerSec: a.rate, State: st})
	}
	for _, j := range d.queue {
		var total int64
		for _, f := range j.Files {
			total += f.Size
		}
		out = append(out, Progress{ModelID: j.ModelID, File: j.Files[0].Name, FileCount: len(j.Files), Total: total, State: "queued"})
	}
	return out
}

// Run works through the queue until ctx ends.
func (d *Downloader) Run(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				d.mu.Lock()
				busy := d.current != nil
				d.mu.Unlock()
				if busy && d.Changed != nil {
					d.Changed()
				}
			}
		}
	}()
	for {
		d.mu.Lock()
		var job *Job
		if len(d.queue) > 0 {
			j := d.queue[0]
			d.queue = d.queue[1:]
			job = &j
		}
		d.mu.Unlock()
		if job == nil {
			select {
			case <-ctx.Done():
				return
			case <-d.wake:
			}
			continue
		}
		err := d.run(ctx, *job)
		d.mu.Lock()
		d.current = nil
		cancelled := d.cancels[job.ModelID]
		delete(d.cancels, job.ModelID)
		d.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		if cancelled {
			continue
		}
		if d.Done != nil {
			d.Done(*job, err)
		}
		if d.Changed != nil {
			d.Changed()
		}
	}
}

func (d *Downloader) run(parent context.Context, j Job) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	a := &active{job: j, cancel: cancel, lastAt: time.Now()}
	a.state.Store("downloading")
	for _, f := range j.Files {
		a.total += f.Size
	}
	d.mu.Lock()
	if d.cancels[j.ModelID] { // deleted while it was taken from the queue
		d.mu.Unlock()
		return errors.New("cancelled")
	}
	d.current = a
	d.mu.Unlock()
	var before int64
	for i, f := range j.Files {
		a.fileIndex.Store(int32(i))
		a.state.Store("downloading")
		if err := d.file(ctx, j, f, a, before); err != nil {
			if ctx.Err() != nil && parent.Err() == nil {
				return errors.New("cancelled")
			}
			return fmt.Errorf("%s: %w", f.Name, err)
		}
		before += f.Size
		a.done.Store(before)
	}
	return nil
}

const (
	chunkSize   = 64 << 20
	connections = 6
)

// partState is the sidecar of a .part file: which chunks are complete.
type partState struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Chunks []bool `json:"chunks"`
}

func (d *Downloader) file(ctx context.Context, j Job, f RepoFile, a *active, before int64) error {
	dst := filepath.Join(j.Dir, filepath.FromSlash(f.Name))
	if !strings.HasPrefix(filepath.Clean(dst), filepath.Clean(j.Dir)+string(os.PathSeparator)) {
		return errors.New("unsafe file name")
	}
	if st, err := os.Stat(dst); err == nil && (f.Size == 0 || st.Size() == f.Size) {
		return nil // done earlier
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	part := dst + ".part"
	sidecar := dst + ".part.json"
	size := f.Size
	if size == 0 {
		var err error
		if size, err = d.headSize(ctx, j, f.Name); err != nil {
			return err
		}
	}
	nChunks := int((size + chunkSize - 1) / chunkSize)
	ps := partState{Size: size, SHA256: f.SHA256, Chunks: make([]bool, nChunks)}
	if b, err := os.ReadFile(sidecar); err == nil {
		var old partState
		if json.Unmarshal(b, &old) == nil && old.Size == size && old.SHA256 == f.SHA256 && len(old.Chunks) == nChunks {
			ps = old
		}
	}
	fh, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer fh.Close()
	if err := fh.Truncate(size); err != nil {
		return fmt.Errorf("cannot reserve %d bytes on disk: %w", size, err)
	}
	var doneBytes atomic.Int64
	for i, ok := range ps.Chunks {
		if ok {
			doneBytes.Add(chunkLen(i, size))
		}
	}
	a.done.Store(before + doneBytes.Load())

	var mu sync.Mutex
	save := func() {
		b, _ := json.Marshal(ps)
		_ = os.WriteFile(sidecar, b, 0o644)
	}
	next := 0
	take := func() int {
		mu.Lock()
		defer mu.Unlock()
		for next < nChunks && ps.Chunks[next] {
			next++
		}
		if next >= nChunks {
			return -1
		}
		i := next
		next++
		return i
	}
	errs := make(chan error, connections)
	var wg sync.WaitGroup
	for w := 0; w < connections; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := take()
				if i < 0 {
					return
				}
				var err error
				for attempt := 0; attempt < 6; attempt++ {
					var got int64
					got, err = d.chunk(ctx, j, f.Name, fh, int64(i)*chunkSize, chunkLen(i, size), func(n int64) {
						a.done.Store(before + doneBytes.Add(n))
					})
					if err == nil {
						break
					}
					doneBytes.Add(-got)
					a.done.Store(before + doneBytes.Load())
					if ctx.Err() != nil {
						break
					}
					select {
					case <-ctx.Done():
					case <-time.After(time.Duration(attempt+1) * 2 * time.Second):
					}
				}
				if err != nil {
					errs <- err
					return
				}
				mu.Lock()
				ps.Chunks[i] = true
				save()
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			mu.Lock()
			save()
			mu.Unlock()
			return err
		}
	}
	if err := fh.Sync(); err != nil {
		return err
	}
	if f.SHA256 != "" {
		a.state.Store("verifying")
		if _, err := fh.Seek(0, io.SeekStart); err != nil {
			return err
		}
		h := sha256.New()
		if _, err := io.Copy(h, &ctxReader{ctx: ctx, r: fh}); err != nil {
			return err
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != f.SHA256 {
			_ = os.Remove(part)
			_ = os.Remove(sidecar)
			return errors.New("the downloaded file is damaged (SHA-256 does not match); retry the download")
		}
	}
	fh.Close()
	if err := os.Rename(part, dst); err != nil {
		return err
	}
	_ = os.Remove(sidecar)
	return nil
}

func chunkLen(i int, size int64) int64 {
	start := int64(i) * chunkSize
	if start+chunkSize > size {
		return size - start
	}
	return chunkSize
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

func (d *Downloader) fileURL(j Job, name string) string {
	rev := j.Revision
	if rev == "" {
		rev = "main"
	}
	parts := strings.Split(name, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return d.Client.Base + "/" + j.Repo + "/resolve/" + url.PathEscape(rev) + "/" + strings.Join(parts, "/")
}

// No overall timeout (files are large), but a server that accepts and then
// says nothing is given up on.
var dlClient = &http.Client{Transport: &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	ResponseHeaderTimeout: 60 * time.Second,
	IdleConnTimeout:       90 * time.Second,
	MaxIdleConnsPerHost:   connections * 2,
}}

func (d *Downloader) request(ctx context.Context, method, u string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "socrates-api")
	if t := d.Client.token(); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	return req, nil
}

func (d *Downloader) headSize(ctx context.Context, j Job, name string) (int64, error) {
	req, err := d.request(ctx, http.MethodHead, d.fileURL(j, name))
	if err != nil {
		return 0, err
	}
	resp, err := dlClient.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return 0, httpError(resp.StatusCode)
	}
	if resp.ContentLength <= 0 {
		return 0, errors.New("Hugging Face did not say how large the file is")
	}
	return resp.ContentLength, nil
}

func httpError(code int) error {
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("access refused (%d): the model is gated or private; accept its terms on huggingface.co and set a token in the API settings", code)
	case http.StatusNotFound:
		return errors.New("the file is not on Hugging Face (any more)")
	}
	return fmt.Errorf("Hugging Face answered %d", code)
}

// chunk downloads one range into the file; it returns the bytes written.
func (d *Downloader) chunk(ctx context.Context, j Job, name string, fh *os.File, off, n int64, progress func(int64)) (int64, error) {
	req, err := d.request(ctx, http.MethodGet, d.fileURL(j, name))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+n-1))
	// A stalled connection is retried instead of hanging forever.
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req = req.WithContext(rctx)
	resp, err := dlClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		if resp.StatusCode == http.StatusOK && off == 0 && resp.ContentLength == n {
			// Fine: the whole (small) file.
		} else if resp.StatusCode >= 300 {
			return 0, httpError(resp.StatusCode)
		} else {
			return 0, fmt.Errorf("the server ignored the byte range (answer %d)", resp.StatusCode)
		}
	}
	buf := make([]byte, 1<<20)
	var got int64
	stall := time.AfterFunc(60*time.Second, cancel)
	defer stall.Stop()
	for got < n {
		k, rerr := resp.Body.Read(buf)
		if k > 0 {
			stall.Reset(60 * time.Second)
			if int64(k) > n-got {
				k = int(n - got)
			}
			if _, werr := fh.WriteAt(buf[:k], off+got); werr != nil {
				return got, werr
			}
			got += int64(k)
			progress(int64(k))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return got, rerr
		}
	}
	if got != n {
		return got, fmt.Errorf("the connection ended early (%d of %d bytes)", got, n)
	}
	return got, nil
}
