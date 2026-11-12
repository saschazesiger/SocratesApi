// Package convert makes a GGUF from a Hugging Face repository that only has
// safetensors: it downloads the original weights, converts them with
// llama.cpp's convert_hf_to_gguf.py to a 16-bit GGUF and quantises that.
// The Python tools are installed into the data volume the first time.
package convert

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/saschazesiger/SocratesApi/internal/hf"
	"github.com/saschazesiger/SocratesApi/internal/store"
)

// Quants are the quantisations offered for a conversion.
var Quants = []string{"Q4_K_M", "Q5_K_M", "Q6_K", "Q8_0", "IQ4_XS", "Q3_K_M"}

// ValidQuant reports whether q is offered.
func ValidQuant(q string) bool {
	for _, x := range Quants {
		if x == q {
			return true
		}
	}
	return false
}

// Config says where the tools are.
type Config struct {
	Python     string // python3
	ScriptsDir string // holds convert_hf_to_gguf.py, conversion/ and gguf-py
	ToolsDir   string // where the Python environment is created (in the data volume)
	LlamaBin   string // the combined llama binary (quantize)
	ModelsDir  string
}

// Progress is the state of a conversion.
type Progress struct {
	ModelID  string  `json:"modelId"`
	Step     int     `json:"step"`  // 1..5
	Steps    int     `json:"steps"` // 5
	Title    string  `json:"title"`
	Progress float64 `json:"progress"` // 0..1 over all steps
	Message  string  `json:"message"`
}

// Converter runs one conversion at a time.
type Converter struct {
	cfg Config
	st  *store.Store
	dl  *hf.Downloader

	Changed func()
	Done    func(m store.Model, err error)
	Log     func(lines []string)

	mu      sync.Mutex
	jobs    map[string]*job
	queue   chan *job
	running bool
}

type job struct {
	model  store.Model
	repo   *hf.Repo
	cancel context.CancelFunc
	ctx    context.Context
	p      Progress
	dlDone chan error
}

// New builds a converter with its own downloader for the source weights.
func New(cfg Config, st *store.Store, client *hf.Client) *Converter {
	c := &Converter{cfg: cfg, st: st, jobs: map[string]*job{}, queue: make(chan *job, 64)}
	c.dl = hf.NewDownloader(client)
	c.dl.Done = func(j hf.Job, err error) {
		c.mu.Lock()
		jb := c.jobs[j.ModelID]
		c.mu.Unlock()
		if jb != nil {
			select {
			case jb.dlDone <- err:
			default:
			}
		}
	}
	c.dl.Changed = func() { c.changed() }
	return c
}

// Available reports whether the tools are in the image.
func (c *Converter) Available() bool {
	_, err := os.Stat(filepath.Join(c.cfg.ScriptsDir, "convert_hf_to_gguf.py"))
	return err == nil
}

// Run processes conversions until ctx ends.
func (c *Converter) Run(ctx context.Context) {
	go c.dl.Run(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-c.queue:
			if j.ctx.Err() != nil {
				continue
			}
			err := c.run(j)
			c.mu.Lock()
			if c.jobs[j.model.ID] == j { // a retry may have replaced it
				delete(c.jobs, j.model.ID)
			}
			c.mu.Unlock()
			if errors.Is(j.ctx.Err(), context.Canceled) {
				continue
			}
			m, gerr := c.st.Model(j.model.ID)
			if gerr != nil {
				continue
			}
			if err == nil {
				m.Files = j.model.Files
				m.MMProj = j.model.MMProj
				m.Size = j.model.Size
			}
			if c.Done != nil {
				c.Done(m, err)
			}
			c.changed()
		}
	}
}

func (c *Converter) changed() {
	if c.Changed != nil {
		c.Changed()
	}
}

func (c *Converter) log(format string, args ...any) {
	if c.Log != nil {
		c.Log([]string{"── convert: " + fmt.Sprintf(format, args...)})
	}
}

// Start queues a conversion.
func (c *Converter) Start(m store.Model, repo *hf.Repo) {
	ctx, cancel := context.WithCancel(context.Background())
	j := &job{model: m, repo: repo, ctx: ctx, cancel: cancel, dlDone: make(chan error, 1),
		p: Progress{ModelID: m.ID, Step: 0, Steps: 5, Title: "Waiting", Message: "Queued"}}
	c.mu.Lock()
	if old := c.jobs[m.ID]; old != nil {
		old.cancel()
		c.dl.Cancel(m.ID)
	}
	c.jobs[m.ID] = j
	c.mu.Unlock()
	_ = c.st.SetModelState(m.ID, "converting", "")
	c.queue <- j
	c.changed()
}

// Resume restarts a conversion after a restart or a failure; finished
// steps are skipped and the download continues where it stopped.
func (c *Converter) Resume(m store.Model) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		repo, err := c.dl.Client.Repo(ctx, m.Repo)
		if err != nil {
			_ = c.st.SetModelState(m.ID, "failed", "Resuming the conversion failed: "+err.Error())
			c.changed()
			return
		}
		c.Start(m, repo)
	}()
}

// Owns reports whether a model was made by conversion (it has a source folder or no files).
func (c *Converter) Owns(m store.Model) bool {
	if len(m.Files) == 0 {
		return true
	}
	_, err := os.Stat(filepath.Join(c.cfg.ModelsDir, m.ID, "source"))
	return err == nil
}

// Cancel stops the conversion of a model.
func (c *Converter) Cancel(id string) {
	c.mu.Lock()
	j := c.jobs[id]
	delete(c.jobs, id)
	c.mu.Unlock()
	if j != nil {
		j.cancel()
		c.dl.Cancel(id)
	}
}

// Progress lists the conversions.
func (c *Converter) Progress() []Progress {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []Progress{}
	for _, j := range c.jobs {
		p := j.p
		if p.Step == 3 {
			for _, d := range c.dl.Progress() {
				if d.ModelID == j.model.ID && d.Total > 0 {
					f := float64(d.Done) / float64(d.Total)
					p.Message = fmt.Sprintf("%.1f of %.1f GB", float64(d.Done)/(1<<30), float64(d.Total)/(1<<30))
					p.Progress = (2 + f) / 5
				}
			}
		}
		out = append(out, p)
	}
	return out
}

func (c *Converter) step(j *job, n int, title, msg string, frac float64) {
	c.mu.Lock()
	j.p.Step, j.p.Title, j.p.Message = n, title, msg
	j.p.Progress = (float64(n-1) + frac) / float64(j.p.Steps)
	c.mu.Unlock()
	c.changed()
}

func (c *Converter) venvPython() string {
	return filepath.Join(c.cfg.ToolsDir, "convert-venv", "bin", "python")
}

func (c *Converter) run(j *job) error {
	if !c.Available() {
		return errors.New("the conversion tools are not in this image")
	}
	ctx := j.ctx
	dir := filepath.Join(c.cfg.ModelsDir, j.model.ID)
	src := filepath.Join(dir, "source")
	if err := os.MkdirAll(src, 0o755); err != nil {
		return err
	}

	// 1. The disk space check happened when it was queued; 2. the Python tools.
	c.step(j, 1, "Checking", "Checking the repository", 0.5)
	c.step(j, 2, "Preparing the converter", "Installing Python packages (first time only, about 1 GB)", 0)
	if err := c.ensureVenv(ctx); err != nil {
		return fmt.Errorf("preparing the converter failed: %w", err)
	}

	// 3. The original weights.
	c.step(j, 3, "Downloading the original weights", "", 0)
	var files []hf.RepoFile
	for _, f := range j.repo.Files {
		low := strings.ToLower(f.Name)
		if strings.Contains(f.Name, "/") && !strings.HasSuffix(low, ".safetensors") {
			continue
		}
		switch {
		case strings.HasSuffix(low, ".safetensors"), strings.HasSuffix(low, ".json"), strings.HasSuffix(low, ".model"),
			strings.HasSuffix(low, ".tiktoken"), strings.HasSuffix(low, ".jinja"), low == "merges.txt", low == "vocab.txt":
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		return errors.New("the repository has no weights to convert")
	}
	c.dl.Add(hf.Job{ModelID: j.model.ID, Repo: j.repo.ID, Revision: "main", Dir: src, Files: files})
	select {
	case err := <-j.dlDone:
		if err != nil {
			return fmt.Errorf("downloading the original weights failed: %w", err)
		}
	case <-ctx.Done():
		return ctx.Err()
	}

	// 4. Convert to a 16-bit GGUF (and the vision projector, if there is one).
	bf16 := filepath.Join(dir, "model-bf16.gguf")
	c.step(j, 4, "Converting to GGUF", "Writing a 16-bit GGUF", 0)
	if _, err := os.Stat(bf16 + ".done"); err != nil {
		err := c.exec(ctx, j, 4, c.venvPython(), filepath.Join(c.cfg.ScriptsDir, "convert_hf_to_gguf.py"), src, "--outfile", bf16, "--outtype", "bf16")
		if err != nil {
			return fmt.Errorf("converting failed: %w", err)
		}
		_ = os.WriteFile(bf16+".done", nil, 0o644)
	}
	mmproj := ""
	if strings.Contains(j.repo.Pipeline, "image") {
		mm := filepath.Join(dir, "mmproj-f16.gguf")
		err := c.exec(ctx, j, 4, c.venvPython(), filepath.Join(c.cfg.ScriptsDir, "convert_hf_to_gguf.py"), src, "--mmproj", "--outfile", mm, "--outtype", "f16")
		if err == nil {
			mmproj = "mmproj-f16.gguf"
		} else {
			c.log("no vision projector: %v", err)
		}
	}

	// 5. Quantise.
	quant := j.model.Quant
	out := "model-" + quant + ".gguf"
	c.step(j, 5, "Quantising to "+quant, "", 0)
	if err := c.exec(ctx, j, 5, c.cfg.LlamaBin, "quantize", bf16, filepath.Join(dir, out), quant); err != nil {
		return fmt.Errorf("quantising failed: %w", err)
	}
	st, err := os.Stat(filepath.Join(dir, out))
	if err != nil {
		return err
	}
	_ = os.RemoveAll(src)
	_ = os.Remove(bf16)
	_ = os.Remove(bf16 + ".done")
	j.model.Files = []store.File{{Path: out, Size: st.Size()}}
	j.model.MMProj = mmproj
	j.model.Size = st.Size()
	if mmproj != "" {
		if mst, err := os.Stat(filepath.Join(dir, mmproj)); err == nil {
			j.model.Size += mst.Size()
		}
	}
	c.log("%s: done, %s (%.1f GB)", j.model.ID, out, float64(st.Size())/(1<<30))
	return nil
}

func (c *Converter) ensureVenv(ctx context.Context) error {
	marker := filepath.Join(c.cfg.ToolsDir, "convert-venv", ".ready")
	if _, err := os.Stat(marker); err == nil {
		return nil
	}
	venv := filepath.Join(c.cfg.ToolsDir, "convert-venv")
	_ = os.RemoveAll(venv)
	if err := os.MkdirAll(c.cfg.ToolsDir, 0o755); err != nil {
		return err
	}
	if err := c.exec(ctx, nil, 0, c.cfg.Python, "-m", "venv", venv); err != nil {
		return err
	}
	pip := filepath.Join(venv, "bin", "pip")
	// Torch for the CPU only: the conversion does not need CUDA, and this saves gigabytes.
	if err := c.exec(ctx, nil, 0, pip, "install", "--no-cache-dir", "--extra-index-url", "https://download.pytorch.org/whl/cpu",
		"-r", filepath.Join(c.cfg.ScriptsDir, "requirements", "requirements-convert_hf_to_gguf.txt")); err != nil {
		return err
	}
	return os.WriteFile(marker, nil, 0o644)
}

var (
	pctRe   = regexp.MustCompile(`(\d{1,3})%\|`)
	quantRe = regexp.MustCompile(`\[\s*(\d+)/\s*(\d+)\]`)
)

// exec runs a tool, sends its output to the engine log and reads progress
// (tqdm percentages of the converter, [n/m] tensors of quantize).
func (c *Converter) exec(ctx context.Context, j *job, step int, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "PYTHONUNBUFFERED=1", "TQDM_MININTERVAL=1")
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	c.log("%s %s", filepath.Base(name), strings.Join(args, " "))
	if err := cmd.Start(); err != nil {
		return err
	}
	tail := make([]string, 0, 20)
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		sc.Split(scanLinesOrCR)
		last := time.Time{}
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			if len(tail) == 20 {
				tail = tail[1:]
			}
			tail = append(tail, line)
			if j != nil {
				frac := -1.0
				if m := quantRe.FindStringSubmatch(line); m != nil {
					a, _ := strconv.Atoi(m[1])
					b, _ := strconv.Atoi(m[2])
					if b > 0 {
						frac = float64(a) / float64(b)
					}
				} else if m := pctRe.FindStringSubmatch(line); m != nil {
					n, _ := strconv.Atoi(m[1])
					frac = float64(n) / 100
				}
				if frac >= 0 {
					c.mu.Lock()
					j.p.Progress = (float64(step-1) + frac) / float64(j.p.Steps)
					j.p.Message = fmt.Sprintf("%d %%", int(frac*100))
					c.mu.Unlock()
					c.changed()
				}
			}
			// Progress bars redraw often; the log gets one line per second of them.
			if strings.Contains(line, "%|") || quantRe.MatchString(line) {
				if time.Since(last) < time.Second {
					continue
				}
				last = time.Now()
			}
			if c.Log != nil {
				c.Log([]string{line})
			}
		}
	}()
	err := cmd.Wait()
	pw.Close()
	<-done
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		msg := ""
		if len(tail) > 0 {
			msg = tail[len(tail)-1]
		}
		return fmt.Errorf("%s: %s", err, msg)
	}
	return nil
}

func scanLinesOrCR(data []byte, atEOF bool) (int, []byte, error) {
	for i, b := range data {
		if b == '\n' || b == '\r' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}
