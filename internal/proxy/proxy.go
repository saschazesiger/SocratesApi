// Package proxy serves the public model API (/v1/*): it checks API keys,
// waits while the model loads, forwards to llama-server, streams the answer
// back and follows every request live for the interface.
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/saschazesiger/SocratesApi/internal/store"
)

// Engine is what the proxy needs from the engine.
type Engine interface {
	BaseURL() string
	WaitReady(ctx context.Context) error
	ActiveID() string
	Ready() bool
}

// Proxy forwards model requests.
type Proxy struct {
	Engine     Engine
	Store      *store.Store
	RequireKey func() bool
	// Changed is called when the activity changed (for live updates).
	Changed func()
	// RequestLogged is called after a request was written to the log.
	RequestLogged func()
	Logf          func(format string, args ...any)
	// LoadTimeout is how long a request waits for a model that is loading.
	LoadTimeout time.Duration

	client *http.Client
	seq    atomic.Int64

	mu       sync.Mutex
	inflight map[string]*Req
	last     *Summary
	polling  bool
}

// Req is a request in flight.
type Req struct {
	ID           string  `json:"id"`
	Source       string  `json:"source"` // api or chat
	Key          string  `json:"key"`
	Path         string  `json:"path"`
	Phase        string  `json:"phase"` // waiting, queued, prompt, thinking, generating
	PromptTokens int64   `json:"promptTokens"`
	PromptDone   int64   `json:"promptDone"`
	CachedTokens int64   `json:"cachedTokens"`
	Generated    int64   `json:"generated"`
	TPS          float64 `json:"tps"`
	StartedAt    int64   `json:"startedAt"`

	phaseAt    time.Time
	phaseStart int64 // tokens at the start of the phase, for the rate
	streamed   bool  // the stream reports the progress; the slots are only a fallback
}

// Summary is the last finished request.
type Summary struct {
	PromptTokens     int64   `json:"promptTokens"`
	CachedTokens     int64   `json:"cachedTokens"`
	CompletionTokens int64   `json:"completionTokens"`
	PromptTPS        float64 `json:"promptTps"`
	GenTPS           float64 `json:"genTps"`
	Ms               int64   `json:"ms"`
	At               int64   `json:"at"`
}

// Activity is what is going on right now.
type Activity struct {
	Running  int      `json:"running"`
	Queued   int      `json:"queued"`
	Requests []Req    `json:"requests"`
	Last     *Summary `json:"last"`
}

// New builds a proxy.
func New(e Engine, st *store.Store) *Proxy {
	return &Proxy{
		Engine: e, Store: st, LoadTimeout: 20 * time.Minute,
		RequireKey: func() bool { return true },
		client:     &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64, DisableCompression: true}},
		inflight:   map[string]*Req{},
		Logf:       func(string, ...any) {},
	}
}

// Activity returns a copy of the live activity.
func (p *Proxy) Activity() Activity {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := Activity{Requests: []Req{}}
	for _, r := range p.inflight {
		a.Requests = append(a.Requests, *r)
		switch r.Phase {
		case "waiting", "queued":
			a.Queued++
		default:
			a.Running++
		}
	}
	sort.Slice(a.Requests, func(i, j int) bool { return a.Requests[i].StartedAt < a.Requests[j].StartedAt })
	if p.last != nil {
		l := *p.last
		a.Last = &l
	}
	return a
}

func (p *Proxy) changed() {
	if p.Changed != nil {
		p.Changed()
	}
}

// --- keys --------------------------------------------------------------------

// HashKey is how API keys are stored.
func HashKey(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func bearer(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if k, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(k)
		}
		if k, ok := strings.CutPrefix(h, "bearer "); ok {
			return strings.TrimSpace(k)
		}
	}
	if k := r.Header.Get("x-api-key"); k != "" {
		return strings.TrimSpace(k) // Anthropic clients
	}
	return ""
}

// apiError writes an error in the OpenAI shape, which Anthropic clients
// also show readably.
func apiError(w http.ResponseWriter, code int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": code}})
}

// ServeHTTP serves /v1/*.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var key store.Key
	secret := bearer(r)
	if p.RequireKey() || secret != "" {
		if secret == "" {
			apiError(w, http.StatusUnauthorized, "authentication_error", "An API key is needed: send it as 'Authorization: Bearer sk-soc-...'. Create one on the API page of Socrates API.")
			return
		}
		k, err := p.Store.KeyByHash(HashKey(secret))
		if err != nil {
			if p.RequireKey() {
				apiError(w, http.StatusUnauthorized, "authentication_error", "This API key is not valid (revoked or mistyped).")
				return
			}
		} else {
			key = k
		}
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.URL.Path == "/v1/models" && r.Method == http.MethodGet {
		p.models(w)
		return
	}
	var body []byte
	if r.Body != nil {
		const maxBody = 256 << 20
		b, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
		if err != nil {
			apiError(w, http.StatusBadRequest, "invalid_request_error", "The request body could not be read.")
			return
		}
		if len(b) > maxBody {
			apiError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "The request is larger than 256 MB.")
			return
		}
		body = b
	}
	p.Forward(w, r, body, "api", key)
}

func (p *Proxy) models(w http.ResponseWriter) {
	id := p.Engine.ActiveID()
	data := []map[string]any{}
	if id != "" {
		data = append(data, map[string]any{"id": id, "object": "model", "created": time.Now().Unix(), "owned_by": "socrates-api"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// Forward sends a request to llama-server and streams the answer back.
// source is "api" or "chat"; for the chat of the interface the stream
// carries the prompt progress.
func (p *Proxy) Forward(w http.ResponseWriter, r *http.Request, body []byte, source string, key store.Key) {
	start := time.Now()
	id := "r" + strconv.FormatInt(p.seq.Add(1), 10)
	req := &Req{ID: id, Source: source, Key: key.Name, Path: r.URL.Path, Phase: "waiting", StartedAt: start.UnixMilli(), phaseAt: start}
	p.mu.Lock()
	p.inflight[id] = req
	p.mu.Unlock()
	p.changed()
	logged := store.Request{At: start.UnixMilli(), Source: source, KeyName: key.Name, KeyID: key.ID, Path: r.URL.Path, Model: p.Engine.ActiveID()}
	defer func() {
		p.mu.Lock()
		delete(p.inflight, id)
		p.mu.Unlock()
		logged.Ms = time.Since(start).Milliseconds()
		if r.URL.Path != "/v1/messages/count_tokens" && !strings.HasSuffix(r.URL.Path, "/tokenize") {
			if err := p.Store.LogRequest(logged); err != nil {
				p.Logf("request log: %v", err)
			}
			if p.RequestLogged != nil {
				p.RequestLogged()
			}
		}
		p.changed()
	}()

	stream := false
	strip := false
	if len(body) > 0 && body[0] == '{' {
		var probe map[string]json.RawMessage
		if json.Unmarshal(body, &probe) == nil {
			stream = string(probe["stream"]) == "true"
			// For the live status, llama.cpp reports the prompt progress in
			// the stream; for API clients those extra events are taken out
			// again, so they see a plain OpenAI stream.
			if stream && source == "api" && (r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/v1/completions") {
				if _, asked := probe["return_progress"]; !asked {
					probe["return_progress"] = json.RawMessage("true")
					if b, err := json.Marshal(probe); err == nil {
						body = b
						strip = true
					}
				}
			}
		}
	}

	// Wait for the model while it loads, instead of failing.
	if !p.Engine.Ready() {
		wctx, cancel := context.WithTimeout(r.Context(), p.LoadTimeout)
		err := p.Engine.WaitReady(wctx)
		cancel()
		if err != nil {
			msg := err.Error()
			if errors.Is(err, context.DeadlineExceeded) {
				msg = "The model is still loading; try again in a moment."
			}
			if r.Context().Err() != nil {
				logged.Status = 499
				logged.Error = "the client gave up while the model was loading"
				return
			}
			logged.Status = http.StatusServiceUnavailable
			logged.Error = msg
			apiError(w, http.StatusServiceUnavailable, "unavailable", msg)
			return
		}
	}
	p.setPhase(id, "queued", 0)

	up, err := http.NewRequestWithContext(r.Context(), r.Method, p.Engine.BaseURL()+r.URL.Path+qs(r), bytes.NewReader(body))
	if err != nil {
		apiError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	for _, h := range []string{"Content-Type", "Accept", "anthropic-version", "anthropic-beta"} {
		if v := r.Header.Get(h); v != "" {
			up.Header.Set(h, v)
		}
	}
	p.startPolling()
	resp, err := p.client.Do(up)
	if err != nil {
		if r.Context().Err() != nil {
			logged.Status = 499
			logged.Error = "the client closed the connection"
			return
		}
		logged.Status = http.StatusBadGateway
		logged.Error = "llama-server does not answer: " + err.Error()
		apiError(w, http.StatusBadGateway, "server_error", "The model engine does not answer (it may have crashed or be restarting). See the Models page of Socrates API.")
		return
	}
	defer resp.Body.Close()
	logged.Status = resp.StatusCode
	for k, vs := range resp.Header {
		switch strings.ToLower(k) {
		case "content-length", "connection", "transfer-encoding", "access-control-allow-origin":
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	isStream := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") || (stream && resp.StatusCode == http.StatusOK)
	if isStream {
		w.Header().Set("X-Accel-Buffering", "no")
		w.Header().Set("Cache-Control", "no-cache")
	}
	w.WriteHeader(resp.StatusCode)
	var u usage
	if isStream {
		p.copyStream(w, resp.Body, id, &u, strip)
	} else {
		b, _ := io.ReadAll(resp.Body)
		_, _ = w.Write(b)
		u.scan(b)
		if resp.StatusCode >= 400 {
			logged.Error = errorText(b)
		}
	}
	logged.PromptTokens, logged.CachedTokens, logged.CompletionTokens = u.prompt, u.cached, u.completion
	logged.PromptTPS, logged.GenTPS = u.promptTPS, u.genTPS
	if resp.StatusCode < 400 && (u.prompt > 0 || u.completion > 0) {
		p.mu.Lock()
		p.last = &Summary{PromptTokens: u.prompt, CachedTokens: u.cached, CompletionTokens: u.completion,
			PromptTPS: u.promptTPS, GenTPS: u.genTPS, Ms: time.Since(start).Milliseconds(), At: time.Now().UnixMilli()}
		p.mu.Unlock()
	}
}

func qs(r *http.Request) string {
	if r.URL.RawQuery == "" {
		return ""
	}
	return "?" + r.URL.RawQuery
}

func errorText(b []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// copyStream passes server-sent events through line by line, flushing
// each, and reads the progress and usage out of them on the way.
func (p *Proxy) copyStream(w http.ResponseWriter, body io.Reader, id string, u *usage, strip bool) {
	flusher, _ := w.(http.Flusher)
	br := bufio.NewReaderSize(body, 64<<10)
	skipBlank := false
	sentRole := false
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			blank := len(bytes.TrimSpace(line)) == 0
			data, isData := bytes.CutPrefix(line, []byte("data:"))
			if isData {
				data = bytes.TrimSpace(data)
				p.chunk(id, data, u)
			}
			drop := false
			if strip && isData && bytes.Contains(data, []byte(`"prompt_progress"`)) && progressOnly(data) {
				// The first of them is the usual role chunk; the repeats are ours.
				drop = sentRole
				sentRole = true
			}
			switch {
			case drop:
				skipBlank = true
			case blank && skipBlank:
				skipBlank = false
			default:
				skipBlank = false
				if _, werr := w.Write(line); werr != nil {
					return
				}
				if blank && flusher != nil {
					flusher.Flush()
				}
			}
		}
		if err != nil {
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
	}
}

// progressOnly reports whether a stream chunk carries nothing but the prompt
// progress (and the empty role delta).
func progressOnly(data []byte) bool {
	var c struct {
		Choices []struct {
			Delta map[string]json.RawMessage `json:"delta"`
			Text  string                     `json:"text"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &c) != nil {
		return false
	}
	for _, ch := range c.Choices {
		if ch.Text != "" {
			return false
		}
		for k, v := range ch.Delta {
			if k != "role" && string(v) != "null" && string(v) != `""` {
				return false
			}
		}
	}
	return true
}

// chunk updates the live state of a request from one stream event.
func (p *Proxy) chunk(id string, data []byte, u *usage) {
	if len(data) == 0 || data[0] != '{' {
		return
	}
	var c struct {
		Choices []struct {
			Delta struct {
				Content          *string           `json:"content"`
				ReasoningContent *string           `json:"reasoning_content"`
				ToolCalls        []json.RawMessage `json:"tool_calls"`
			} `json:"delta"`
			Text *string `json:"text"`
		} `json:"choices"`
		PromptProgress *struct {
			Total     int64 `json:"total"`
			Cache     int64 `json:"cache"`
			Processed int64 `json:"processed"`
		} `json:"prompt_progress"`
		Timings *timings `json:"timings"`
		Type    string   `json:"type"` // Anthropic events
		Delta   *struct {
			Type string `json:"type"`
		} `json:"delta"`
	}
	if json.Unmarshal(data, &c) != nil {
		return
	}
	u.scan(data)
	phase := ""
	gen := int64(0)
	if c.PromptProgress != nil {
		p.mu.Lock()
		if r := p.inflight[id]; r != nil {
			r.streamed = true
			if r.Phase != "prompt" {
				r.Phase, r.phaseAt, r.phaseStart = "prompt", time.Now(), c.PromptProgress.Processed
			}
			r.PromptTokens = c.PromptProgress.Total
			r.CachedTokens = c.PromptProgress.Cache
			r.PromptDone = c.PromptProgress.Processed + c.PromptProgress.Cache
			if dt := time.Since(r.phaseAt).Seconds(); dt > 0.2 {
				r.TPS = float64(c.PromptProgress.Processed-r.phaseStart) / dt
			}
		}
		p.mu.Unlock()
		p.changed()
		return
	}
	for _, ch := range c.Choices {
		switch {
		case ch.Delta.ReasoningContent != nil && *ch.Delta.ReasoningContent != "":
			phase = "thinking"
			gen++
		case (ch.Delta.Content != nil && *ch.Delta.Content != "") || len(ch.Delta.ToolCalls) > 0 || (ch.Text != nil && *ch.Text != ""):
			phase = "generating"
			gen++
		}
	}
	if c.Type == "content_block_delta" && c.Delta != nil {
		gen++
		phase = "generating"
		if c.Delta.Type == "thinking_delta" {
			phase = "thinking"
		}
	}
	if phase == "" {
		return
	}
	p.mu.Lock()
	if r := p.inflight[id]; r != nil {
		if r.Phase != phase {
			if r.Phase != "thinking" && r.Phase != "generating" {
				r.phaseAt, r.phaseStart = time.Now(), r.Generated
			}
			r.Phase = phase
		}
		if c.Timings != nil && c.Timings.PredictedN > 0 {
			r.Generated = c.Timings.PredictedN
		} else {
			r.Generated += gen
		}
		if dt := time.Since(r.phaseAt).Seconds(); dt > 0.3 {
			r.TPS = float64(r.Generated-r.phaseStart) / dt
		}
	}
	p.mu.Unlock()
	p.changed()
}

func (p *Proxy) setPhase(id, phase string, tokens int64) {
	p.mu.Lock()
	if r := p.inflight[id]; r != nil && r.Phase != phase {
		r.Phase, r.phaseAt, r.phaseStart = phase, time.Now(), tokens
	}
	p.mu.Unlock()
	p.changed()
}

type timings struct {
	CacheN      int64   `json:"cache_n"`
	PromptN     int64   `json:"prompt_n"`
	PromptPS    float64 `json:"prompt_per_second"`
	PredictedN  int64   `json:"predicted_n"`
	PredictedPS float64 `json:"predicted_per_second"`
}

// usage collects token counts from llama.cpp's timings and from the
// usage objects of the OpenAI and Anthropic formats.
type usage struct {
	prompt, cached, completion int64
	promptTPS, genTPS          float64
	haveTimings                bool
}

func (u *usage) scan(b []byte) {
	if !bytes.Contains(b, []byte("timings")) && !bytes.Contains(b, []byte("usage")) {
		return
	}
	var v struct {
		Timings *timings `json:"timings"`
		Usage   *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			InputTokens      int64 `json:"input_tokens"`
			OutputTokens     int64 `json:"output_tokens"`
			Details          *struct {
				Cached int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
		Message *struct {
			Usage *struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(b, &v) != nil {
		return
	}
	if t := v.Timings; t != nil && (t.PromptN > 0 || t.PredictedN > 0) {
		u.haveTimings = true
		u.prompt = t.PromptN + t.CacheN
		u.cached = t.CacheN
		u.completion = t.PredictedN
		u.promptTPS = t.PromptPS
		u.genTPS = t.PredictedPS
	}
	if u.haveTimings {
		return
	}
	if us := v.Usage; us != nil {
		if us.PromptTokens+us.InputTokens > 0 {
			u.prompt = us.PromptTokens + us.InputTokens
		}
		if us.CompletionTokens+us.OutputTokens > 0 {
			u.completion = us.CompletionTokens + us.OutputTokens
		}
		if us.Details != nil {
			u.cached = us.Details.Cached
		}
	}
	if v.Message != nil && v.Message.Usage != nil && v.Message.Usage.InputTokens > 0 {
		u.prompt = v.Message.Usage.InputTokens
	}
}

// --- slots -------------------------------------------------------------------

// startPolling follows llama-server's slots while requests are in flight:
// it is the only way to see the prompt progress of API requests and to
// tell queued requests from running ones.
func (p *Proxy) startPolling() {
	p.mu.Lock()
	if p.polling {
		p.mu.Unlock()
		return
	}
	p.polling = true
	p.mu.Unlock()
	go func() {
		defer func() {
			p.mu.Lock()
			p.polling = false
			p.mu.Unlock()
		}()
		for {
			time.Sleep(700 * time.Millisecond)
			p.mu.Lock()
			n := len(p.inflight)
			p.mu.Unlock()
			if n == 0 {
				return
			}
			p.pollSlots()
		}
	}()
}

type slot struct {
	ID           int   `json:"id"`
	IsProcessing bool  `json:"is_processing"`
	IDTask       int64 `json:"id_task"`
	NPrompt      int64 `json:"n_prompt_tokens"`
	NPromptDone  int64 `json:"n_prompt_tokens_processed"`
	NPromptCache int64 `json:"n_prompt_tokens_cache"`
	NextToken    []struct {
		NDecoded int64 `json:"n_decoded"`
	} `json:"next_token"`
}

func (p *Proxy) pollSlots() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.Engine.BaseURL()+"/slots", nil)
	resp, err := p.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var slots []slot
	if json.NewDecoder(resp.Body).Decode(&slots) != nil {
		return
	}
	busy := []slot{}
	for _, s := range slots {
		if s.IsProcessing {
			busy = append(busy, s)
		}
	}
	sort.Slice(busy, func(i, j int) bool { return busy[i].IDTask < busy[j].IDTask })
	p.mu.Lock()
	reqs := make([]*Req, 0, len(p.inflight))
	for _, r := range p.inflight {
		if r.Phase != "waiting" {
			reqs = append(reqs, r)
		}
	}
	sort.Slice(reqs, func(i, j int) bool { return reqs[i].StartedAt < reqs[j].StartedAt })
	// The oldest requests hold the busy slots; the others wait in the queue.
	for i, r := range reqs {
		if i >= len(busy) {
			if r.Phase == "prompt" || r.Phase == "queued" {
				r.Phase = "queued"
			}
			continue
		}
		s := busy[i]
		decoded := int64(0)
		if len(s.NextToken) > 0 {
			decoded = s.NextToken[0].NDecoded
		}
		if r.streamed {
			continue
		}
		if decoded == 0 {
			// While the prompt is read, n_prompt_tokens grows batch by batch:
			// the total is not known here, only what was read so far.
			if r.Phase != "prompt" {
				r.Phase, r.phaseAt, r.phaseStart = "prompt", time.Now(), s.NPromptDone
			}
			r.PromptTokens = 0
			r.CachedTokens = s.NPromptCache
			r.PromptDone = s.NPromptDone + s.NPromptCache
			if dt := time.Since(r.phaseAt).Seconds(); dt > 0.5 {
				r.TPS = float64(s.NPromptDone-r.phaseStart) / dt
			}
		} else if r.Phase == "queued" || r.Phase == "prompt" {
			r.PromptTokens = s.NPrompt
			r.CachedTokens = s.NPromptCache
			r.PromptDone = s.NPrompt
			r.Phase, r.phaseAt, r.phaseStart = "generating", time.Now(), r.Generated
		}
	}
	p.mu.Unlock()
	p.changed()
}

// String is for logs.
func (r Req) String() string { return fmt.Sprintf("%s %s %s", r.ID, r.Source, r.Phase) }
