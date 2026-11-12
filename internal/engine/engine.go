// Package engine runs llama.cpp's llama-server for the active model and
// keeps it running: it plans the placement, starts the process, follows
// its loading, restarts it after a crash and measures where the model is.
package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/saschazesiger/SocratesApi/internal/gguf"
	"github.com/saschazesiger/SocratesApi/internal/store"
	"github.com/saschazesiger/SocratesApi/internal/sysinfo"
)

// Config is how the engine finds llama.cpp and where models live.
type Config struct {
	ServerBin string // llama-server
	LlamaBin  string // the combined llama binary (fit-params)
	Port      int    // the port llama-server listens on, on 127.0.0.1
	ModelsDir string
}

// States of the engine.
const (
	Stopped  = "stopped"
	Planning = "planning"
	Starting = "starting"
	Loading  = "loading"
	Ready    = "ready"
	Failed   = "error"
	Crashed  = "crashed"
)

// Status is what the interface shows about the engine.
type Status struct {
	State     string   `json:"state"`
	Message   string   `json:"message"`
	Progress  *float64 `json:"progress"`
	Stage     string   `json:"stage"`
	Since     int64    `json:"since"`
	Error     string   `json:"error"`
	ModelID   string   `json:"modelId"`
	ModelName string   `json:"modelName"`
	Ctx       int      `json:"ctx"`
	Slots     int      `json:"slots"`
	Restarts  int      `json:"restarts"`
	UptimeMs  int64    `json:"uptimeMs"`
	Args      []string `json:"args"`
	Fits      string   `json:"fits"`
	Summary   string   `json:"summary"`
	Version   string   `json:"version"`
}

// Engine supervises one llama-server.
type Engine struct {
	cfg     Config
	st      *store.Store
	sys     *sysinfo.Reader
	planner *Planner

	// OnChange is called when the status changed; OnLog with new log lines.
	OnChange func()
	OnLog    func(lines []string)
	Logf     func(format string, args ...any)

	wake chan struct{}

	mu         sync.Mutex
	status     Status
	readyAt    time.Time
	placement  *Placement
	paths      []string // GGUF files of the running model
	pid        int
	run        *run
	snapshot   sysinfo.Snapshot
	logs       []string
	restartReq bool

	childProgress float64 // llama.cpp's own loading progress
	readStart     time.Time
	readPrev      int64
	readPrevAt    time.Time
	readRate      float64
	version       string
}

type run struct {
	modelID  string
	cancel   context.CancelFunc
	done     chan struct{}
	settings string // settings JSON it was started with, to notice changes
}

// New builds an engine.
func New(cfg Config, st *store.Store, sys *sysinfo.Reader) *Engine {
	return &Engine{
		cfg: cfg, st: st, sys: sys,
		planner: &Planner{LlamaBin: cfg.LlamaBin},
		wake:    make(chan struct{}, 1),
		status:  Status{State: Stopped, Message: "No model is active.", Since: store.Now()},
		Logf:    func(string, ...any) {},
	}
}

// --- what the rest of the program calls -----------------------------------

const activeKey = "engine.active"

// ActiveID returns the id of the active model, "" for none.
func (e *Engine) ActiveID() string {
	var id string
	_ = e.st.GetJSON(activeKey, &id)
	return id
}

// Activate makes a model the active one and (re)starts the engine with it.
func (e *Engine) Activate(id string) error {
	if err := e.st.SetJSON(activeKey, id); err != nil {
		return err
	}
	e.mu.Lock()
	e.status.Restarts = 0
	e.restartReq = true // also when it is the active model already (after an error)
	e.mu.Unlock()
	e.kick()
	return nil
}

// Stop unloads the model; no model is active afterwards.
func (e *Engine) Stop() error {
	if err := e.st.SetJSON(activeKey, ""); err != nil {
		return err
	}
	e.kick()
	return nil
}

// Restart starts the active model again, also after an error.
func (e *Engine) Restart() {
	e.mu.Lock()
	e.restartReq = true
	e.status.Restarts = 0
	e.mu.Unlock()
	e.kick()
}

// Reconcile tells the engine that models or settings may have changed.
func (e *Engine) Reconcile() { e.kick() }

func (e *Engine) kick() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Status returns a copy of the status.
func (e *Engine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.status
	if s.State == Ready && !e.readyAt.IsZero() {
		s.UptimeMs = time.Since(e.readyAt).Milliseconds()
	}
	s.Args = append([]string(nil), s.Args...)
	s.Version = e.version
	return s
}

// Placement returns where the active model is, or nil.
func (e *Engine) Placement() *Placement {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.placement.Clone()
}

// System returns the last snapshot of the machine.
func (e *Engine) System() sysinfo.Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.snapshot
}

// Logs returns the last lines of the engine log.
func (e *Engine) Logs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.logs...)
}

// Ready reports whether requests can be served now.
func (e *Engine) Ready() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.status.State == Ready
}

// BaseURL is where llama-server listens.
func (e *Engine) BaseURL() string { return "http://127.0.0.1:" + strconv.Itoa(e.cfg.Port) }

// WaitReady blocks until the engine is ready, the context ends, or no model
// is active (then it returns an error at once).
func (e *Engine) WaitReady(ctx context.Context) error {
	for {
		st := e.Status()
		switch st.State {
		case Ready:
			return nil
		case Stopped:
			if e.ActiveID() == "" {
				return errors.New("no model is active: activate one on the Models page of Socrates API")
			}
		case Failed:
			return fmt.Errorf("the model could not be started: %s", st.Error)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// Plan computes a placement for a model with the given settings without
// loading it (for the preview in the settings).
func (e *Engine) Plan(ctx context.Context, m store.Model, s Settings) (Plan, error) {
	paths, mmproj := e.files(m)
	meta, f, err := ReadMeta(paths, mmproj != "")
	if err != nil {
		return Plan{}, fmt.Errorf("the model file cannot be read: %v", err)
	}
	sys := e.systemFor(m.ID)
	if held := e.engineVRAM(); held > 0 {
		// The running engine holds the GPU; the model would replace it.
		return e.planner.MakeVirtual(ctx, paths, fileSize(mmproj), s, f, meta, sys, sys.GPUFree+held), nil
	}
	return e.planner.Make(ctx, paths, fileSize(mmproj), s, f, meta, sys), nil
}

// engineVRAM is what the running llama-server holds on the GPU, with its
// CUDA context; 0 when nothing runs.
func (e *Engine) engineVRAM() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pid == 0 || e.placement == nil {
		return 0
	}
	g := e.placement.GPU
	return g.Weights + g.KV + g.Compute + g.Extra + 400<<20
}

// ModelMeta reads a model's metadata from its files.
func (e *Engine) ModelMeta(m store.Model) (Meta, error) {
	paths, mmproj := e.files(m)
	meta, _, err := ReadMeta(paths, mmproj != "")
	return meta, err
}

// systemFor is the memory a model could use, counting what the engine holds
// now if it is to be replaced by (or restarted with) this model.
func (e *Engine) systemFor(modelID string) System {
	snap := e.sys.Read(e.currentPID())
	s := System{RAMTotal: snap.RAM.Total, RAMAvailable: snap.RAM.Available + snap.RAM.EngineAnon}
	for _, g := range snap.GPUs {
		s.GPUTotal += g.Total
		s.GPUFree += g.Free
	}
	return s
}

func (e *Engine) currentPID() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.pid
}

// files returns the absolute paths of a model's GGUF files and its mmproj.
func (e *Engine) files(m store.Model) ([]string, string) {
	dir := filepath.Join(e.cfg.ModelsDir, m.ID)
	var paths []string
	for _, f := range m.Files {
		if f.Kind == "" {
			paths = append(paths, filepath.Join(dir, f.Path))
		}
	}
	mm := ""
	if m.MMProj != "" {
		mm = filepath.Join(dir, m.MMProj)
	}
	return paths, mm
}

// ModelDir is the folder of a model's files.
func (e *Engine) ModelDir(id string) string { return filepath.Join(e.cfg.ModelsDir, id) }

func fileSize(p string) int64 {
	if p == "" {
		return 0
	}
	st, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return st.Size()
}

// --- the supervisor --------------------------------------------------------

// Run supervises until ctx ends; then it stops llama-server.
func (e *Engine) Run(ctx context.Context) {
	e.detectVersion()
	go e.monitor(ctx)
	backoff := time.Duration(0)
	var retryAt time.Time
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		active := e.ActiveID()
		var model store.Model
		var err error
		if active != "" {
			model, err = e.st.Model(active)
			if err != nil || model.State != "ready" {
				active = ""
			}
		}

		e.mu.Lock()
		cur := e.run
		restart := e.restartReq
		e.restartReq = false
		state := e.status.State
		e.mu.Unlock()

		switch {
		case active == "":
			if cur != nil {
				e.stopping()
				e.stopRun(cur)
			}
			if state != Stopped {
				msg := "No model is active."
				if id := e.ActiveID(); id != "" {
					msg = "The active model is not ready yet (still downloading?)."
				}
				e.set(func(s *Status) { *s = Status{State: Stopped, Message: msg, Since: store.Now()} })
				e.clearPlacement()
			}
		case cur == nil || cur.modelID != active || cur.settings != string(model.Settings) || restart:
			if cur != nil {
				e.stopping()
				e.stopRun(cur)
			}
			if restart || cur == nil || cur.modelID != active || cur.settings != string(model.Settings) {
				backoff, retryAt = 0, time.Time{}
				if cur == nil || cur.modelID != active {
					e.set(func(s *Status) { s.Restarts = 0 })
				}
			}
			e.startRun(ctx, model)
		default:
			// The run ended by itself: a crash. Retry with a growing pause.
			select {
			case <-cur.done:
				st := e.Status()
				if st.State == Failed {
					break
				}
				if retryAt.IsZero() {
					switch {
					case backoff == 0:
						backoff = 5 * time.Second
					case backoff < 2*time.Minute:
						backoff *= 3
					}
					if st.Restarts >= 5 {
						e.set(func(s *Status) {
							s.State = Failed
							s.Message = "Stopped after 5 crashes in a row. Check the engine log, change the settings, then restart."
							s.Since = store.Now()
						})
						break
					}
					retryAt = time.Now().Add(backoff)
					e.set(func(s *Status) {
						s.Message = fmt.Sprintf("Crashed: %s. Restarting in %d s.", s.Error, int(backoff.Seconds()))
					})
				} else if time.Now().After(retryAt) {
					retryAt = time.Time{}
					e.set(func(s *Status) { s.Restarts++ })
					e.startRun(ctx, model)
				}
			default:
				// Running: after 10 healthy minutes, earlier crashes are forgotten.
				if st := e.Status(); st.State == Ready && st.Restarts > 0 && st.UptimeMs > 10*60*1000 {
					backoff = 0
					e.set(func(s *Status) { s.Restarts = 0 })
				}
			}
		}

		select {
		case <-ctx.Done():
			e.mu.Lock()
			cur := e.run
			e.mu.Unlock()
			if cur != nil {
				e.stopRun(cur)
			}
			return
		case <-e.wake:
		case <-tick.C:
		}
	}
}

// stopping marks the engine as not ready before the running server is
// stopped, so requests wait for the next one instead of reaching it.
func (e *Engine) stopping() {
	e.set(func(s *Status) {
		if s.State == Ready || s.State == Loading || s.State == Starting {
			s.State = Starting
			s.Message = "Stopping the running model"
			s.Progress = nil
			s.Since = store.Now()
		}
	})
}

func (e *Engine) set(fn func(s *Status)) {
	e.mu.Lock()
	fn(&e.status)
	e.mu.Unlock()
	if e.OnChange != nil {
		e.OnChange()
	}
}

func (e *Engine) clearPlacement() {
	e.mu.Lock()
	e.placement = nil
	e.paths = nil
	e.mu.Unlock()
}

func (e *Engine) stopRun(r *run) {
	r.cancel()
	<-r.done
	e.mu.Lock()
	if e.run == r {
		e.run = nil
	}
	e.mu.Unlock()
}

func (e *Engine) startRun(parent context.Context, m store.Model) {
	ctx, cancel := context.WithCancel(parent)
	r := &run{modelID: m.ID, cancel: cancel, done: make(chan struct{}), settings: string(m.Settings)}
	e.mu.Lock()
	e.run = r
	e.mu.Unlock()
	go func() {
		defer close(r.done)
		e.runModel(ctx, m)
	}()
}

func progress(v float64) *float64 { return &v }

// runModel plans, starts and follows one llama-server until it exits.
func (e *Engine) runModel(ctx context.Context, m store.Model) {
	e.set(func(s *Status) {
		restarts := s.Restarts
		*s = Status{State: Planning, Message: "Planning where the model goes: GPU, RAM or NVMe", Since: store.Now(),
			ModelID: m.ID, ModelName: m.Name, Restarts: restarts}
	})
	paths, mmproj := e.files(m)
	settings := ParseSettings(m.Settings)
	e.dropOtherModels(m.ID)
	meta, f, err := ReadMeta(paths, mmproj != "")
	if err != nil {
		e.fail(fmt.Sprintf("The model file cannot be read (%v). Delete the model and download it again.", err))
		return
	}
	plan := e.planner.Make(ctx, paths, fileSize(mmproj), settings, f, meta, e.systemFor(m.ID))
	if ctx.Err() != nil {
		return
	}
	if !plan.OK {
		e.fail("Planning failed: " + plan.Error)
		return
	}
	s := settings.withDefaults(meta)
	args := append([]string{}, plan.Args...)
	args = append(args, serverArgs(m, s, meta, mmproj, e.cfg.Port)...)
	extra, _ := SplitArgs(s.ExtraArgs)
	args = append(args, extra...)

	e.mu.Lock()
	e.placement = plan.Placement
	e.paths = paths
	e.mu.Unlock()
	e.set(func(st *Status) {
		st.State = Starting
		st.Message = "Starting llama.cpp"
		st.Since = store.Now()
		st.Ctx = plan.Ctx
		st.Slots = s.Parallel
		st.Args = append([]string{e.cfg.ServerBin}, args...)
		st.Fits = plan.Fits
		st.Summary = plan.Summary
		st.Error = ""
	})
	e.appendLog([]string{"── socrates: " + e.cfg.ServerBin + " " + quoteArgs(args)})

	cmd := exec.Command(e.cfg.ServerBin, args...)
	// Child mode makes llama-server report its loading progress and memory
	// as JSON lines on stdout; logs go to stderr.
	cmd.Env = append(os.Environ(), "LLAMA_SERVER_ROUTER_PORT="+strconv.Itoa(e.cfg.Port), "LLAMA_ARG_LOG_COLORS=off")
	// No Pdeathsig: Go may end the OS thread that started the child, which would kill it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		e.fail("llama-server could not be started: " + err.Error())
		return
	}
	e.mu.Lock()
	e.pid = cmd.Process.Pid
	e.childProgress = 0
	e.readStart = time.Now()
	e.readPrev, e.readPrevAt, e.readRate = 0, time.Time{}, 0
	e.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(2)
	tail := &lineTail{max: 60}
	go func() { defer wg.Done(); e.readState(stdout, m, plan) }()
	go func() { defer wg.Done(); e.readLog(stderr, tail) }()

	exited := make(chan error, 1)
	go func() { wg.Wait(); exited <- cmd.Wait() }()

	stopPreload := make(chan struct{})
	defer close(stopPreload)
	go e.afterReady(ctx, plan, paths, s, stopPreload)

	select {
	case err := <-exited:
		e.mu.Lock()
		e.pid = 0
		e.mu.Unlock()
		reason := knownLlamaError(strings.Join(tail.lines(), "\n"))
		if reason == "" {
			reason = "llama-server stopped unexpectedly (" + exitText(err) + ")"
			if strings.Contains(exitText(err), "killed") {
				reason += "; if nobody stopped it, the system may have run out of RAM"
			}
		}
		e.set(func(st *Status) {
			st.State = Crashed
			st.Error = reason
			st.Message = "Crashed: " + reason
			st.Progress = nil
			st.Since = store.Now()
		})
		e.appendLog([]string{"── socrates: llama-server exited (" + exitText(err) + ")"})
	case <-ctx.Done():
		// Ask politely (the child protocol's exit command, then SIGTERM), then force.
		_, _ = io.WriteString(stdin, "cmd_router_to_child:exit\n")
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(15 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-exited
		}
		e.mu.Lock()
		e.pid = 0
		e.mu.Unlock()
		e.appendLog([]string{"── socrates: llama-server stopped"})
	}
	_ = stdin.Close()
}

func exitText(err error) string {
	if err == nil {
		return "exit code 0"
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return "killed by " + ws.Signal().String()
		}
		return "exit code " + strconv.Itoa(ee.ExitCode())
	}
	return err.Error()
}

func (e *Engine) fail(msg string) {
	e.set(func(s *Status) {
		s.State = Failed
		if s.Restarts > 0 {
			// While recovering from a crash, a failed start is retried too
			// (the GPU may not have been free yet).
			s.State = Crashed
		}
		s.Error = msg
		s.Message = msg
		s.Progress = nil
		s.Since = store.Now()
	})
	e.appendLog([]string{"── socrates: " + msg})
}

// serverArgs are the flags of the server itself (not of the placement).
func serverArgs(m store.Model, s Settings, meta Meta, mmproj string, port int) []string {
	a := []string{"--host", "127.0.0.1", "--port", strconv.Itoa(port), "--alias", m.ID,
		"--no-webui", "--metrics", "--slots", "--log-colors", "off"}
	if mmproj != "" && s.Vision != nil && *s.Vision {
		a = append(a, "--mmproj", mmproj)
	} else {
		a = append(a, "--no-mmproj")
	}
	if s.Speculative == "mtp" && meta.MTP {
		a = append(a, "--spec-type", "draft-mtp", "--spec-draft-n-max", "2")
	}
	if s.Reasoning != "" && s.Reasoning != "auto" {
		a = append(a, "-rea", s.Reasoning)
	}
	if s.ReasoningBudget != nil {
		a = append(a, "--reasoning-budget", strconv.Itoa(*s.ReasoningBudget))
	}
	f := func(flag string, v *float64) {
		if v != nil {
			a = append(a, flag, strconv.FormatFloat(*v, 'f', -1, 64))
		}
	}
	f("--temp", s.Temperature)
	f("--top-p", s.TopP)
	f("--min-p", s.MinP)
	f("--presence-penalty", s.PresencePenalty)
	f("--repeat-penalty", s.RepeatPenalty)
	if s.TopK != nil {
		a = append(a, "--top-k", strconv.Itoa(*s.TopK))
	}
	if meta.Embedding {
		a = append(a, "--embeddings")
	}
	return a
}

func quoteArgs(args []string) string {
	var b strings.Builder
	for i, a := range args {
		if i > 0 {
			b.WriteByte(' ')
		}
		if a == "" || strings.ContainsAny(a, " \"'\\|()*") {
			b.WriteString(strconv.Quote(a))
		} else {
			b.WriteString(a)
		}
	}
	return b.String()
}

// childMsg is one state line of llama-server's child protocol.
type childMsg struct {
	State   string          `json:"state"`
	Payload json.RawMessage `json:"payload"`
}

type memoryReport struct {
	NCtx    int `json:"n_ctx"`
	Devices []struct {
		Kind    string `json:"kind"`
		Name    string `json:"name"`
		Model   int64  `json:"model"`
		Context int64  `json:"context"`
		Compute int64  `json:"compute"`
		Total   int64  `json:"total"`
		Free    int64  `json:"free"`
	} `json:"devices"`
}

const childPrefix = "cmd_child_to_router:state:"

var stageText = map[string]string{
	"text_model":   "Loading the weights",
	"spec_model":   "Loading the MTP draft",
	"mmproj_model": "Loading the vision projector",
}

func (e *Engine) readState(r io.Reader, m store.Model, plan Plan) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		line := sc.Text()
		i := strings.Index(line, childPrefix)
		if i < 0 {
			if strings.TrimSpace(line) != "" {
				e.appendLog([]string{line})
			}
			continue
		}
		var msg childMsg
		if json.Unmarshal([]byte(line[i+len(childPrefix):]), &msg) != nil {
			continue
		}
		switch msg.State {
		case "loading":
			var p struct {
				Stages  []string `json:"stages"`
				Current string   `json:"current"`
				Value   float64  `json:"value"`
			}
			_ = json.Unmarshal(msg.Payload, &p)
			// One bar over all stages.
			total := float64(len(p.Stages))
			pos := 0.0
			for k, st := range p.Stages {
				if st == p.Current {
					pos = float64(k)
				}
			}
			overall := p.Value
			if total > 1 {
				overall = (pos + p.Value) / total
			}
			text := stageText[p.Current]
			if text == "" {
				text = "Loading"
			}
			e.mu.Lock()
			e.childProgress = overall
			e.mu.Unlock()
			if overall == 0 {
				// Before llama.cpp counts, it maps the files and reads them
				// in; monitor() shows that phase from the page cache.
				e.set(func(s *Status) {
					if s.State != Loading {
						s.Since = store.Now()
						s.State = Loading
						s.Stage = "reading"
						s.Progress = progress(0)
						s.Message = "Opening the model files"
					}
				})
				continue
			}
			e.set(func(s *Status) {
				if s.State != Loading {
					s.Since = store.Now()
				}
				s.State = Loading
				s.Stage = p.Current
				s.Progress = progress(overall)
				s.Message = fmt.Sprintf("%s · %d %%", text, int(overall*100))
			})
		case "ready":
			var p struct {
				Memory memoryReport `json:"memory"`
			}
			_ = json.Unmarshal(msg.Payload, &p)
			e.measured(p.Memory, plan)
			e.mu.Lock()
			e.readyAt = time.Now()
			e.mu.Unlock()
			e.set(func(s *Status) {
				s.State = Ready
				s.Stage = ""
				s.Progress = nil
				s.Since = store.Now()
				s.Message = "Ready"
				if p.Memory.NCtx > 0 {
					s.Ctx = p.Memory.NCtx
				}
			})
		case "sleeping":
			e.set(func(s *Status) { s.Message = "Sleeping" })
		}
	}
}

// measured replaces the estimate with what llama.cpp allocated.
func (e *Engine) measured(mem memoryReport, plan Plan) {
	if len(mem.Devices) == 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.placement == nil {
		return
	}
	pl := e.placement
	pl.Source = "measured"
	pl.GPU.Weights, pl.GPU.KV, pl.GPU.Compute = 0, 0, 0
	for _, d := range mem.Devices {
		switch d.Kind {
		case "device":
			pl.GPU.Weights += d.Model
			pl.GPU.KV += d.Context
			pl.GPU.Compute += d.Compute
		case "host":
			pl.RAM.KV = d.Context
			pl.RAM.Compute = d.Compute
			if d.Model-pl.Disk.Lookup > 0 {
				pl.RAM.Weights = d.Model - pl.Disk.Lookup
			}
		}
	}
}

func (e *Engine) readLog(r io.Reader, tail *lineTail) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	batch := []string{}
	flush := time.Now()
	for sc.Scan() {
		line := sc.Text()
		tail.add(line)
		batch = append(batch, line)
		if len(batch) >= 50 || time.Since(flush) > 200*time.Millisecond {
			e.appendLog(batch)
			batch = []string{}
			flush = time.Now()
		}
	}
	if len(batch) > 0 {
		e.appendLog(batch)
	}
}

const keepLog = 1000

func (e *Engine) appendLog(lines []string) {
	e.mu.Lock()
	e.logs = append(e.logs, lines...)
	if len(e.logs) > keepLog {
		e.logs = append([]string(nil), e.logs[len(e.logs)-keepLog:]...)
	}
	e.mu.Unlock()
	if e.OnLog != nil {
		e.OnLog(lines)
	}
}

type lineTail struct {
	mu  sync.Mutex
	max int
	l   []string
}

func (t *lineTail) add(s string) {
	t.mu.Lock()
	t.l = append(t.l, s)
	if len(t.l) > t.max {
		t.l = t.l[1:]
	}
	t.mu.Unlock()
}

func (t *lineTail) lines() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.l...)
}

// afterReady waits for the model to be ready, then frees the page cache of
// weights that went to the GPU (they are not needed in RAM any more) and
// reads the weights that stay in RAM, so the first request does not wait
// for the NVMe.
func (e *Engine) afterReady(ctx context.Context, plan Plan, paths []string, s Settings, stop <-chan struct{}) {
	for !e.Ready() {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
	pl := plan.Placement
	if pl == nil || s.LoadMode != "mmap" {
		return
	}
	if plan.Fits != "gpu" {
		dropGPUPages(paths, pl)
	}
	// Read in what fits into the free RAM (keeping 4 GB for everything
	// else): more would only push out pages that are in use.
	if s.Preload != nil && *s.Preload && len(pl.cpuRanges) > 0 {
		have := resident(paths, pl.cpuRanges)
		if have >= pl.RAM.Weights*95/100 {
			return
		}
		snap := e.sys.Read(0)
		// MemAvailable counts clean file pages as free, ours included.
		room := snap.RAM.Available - have - 4*gb
		if room <= 0 {
			return
		}
		limit := have + room // pages already cached are read without I/O
		e.appendLog([]string{fmt.Sprintf("── socrates: reading up to %s more of the RAM weights into the page cache (%s are there)",
			fmtGB(min64(room, pl.RAM.Weights-have)), fmtGB(have))})
		preload(paths, pl.cpuRanges, limit, stop)
		e.appendLog([]string{fmt.Sprintf("── socrates: %s of %s RAM weights are in RAM", fmtGB(resident(paths, pl.cpuRanges)), fmtGB(pl.RAM.Weights))})
	}
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// dropGPUPages tells the kernel it may drop the cached file pages of the
// weights that were copied to the GPU, so RAM goes to the weights that
// are used from RAM.
func dropGPUPages(paths []string, pl *Placement) {
	for idx, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		st, _ := f.Stat()
		var off int64
		for _, r := range pl.cpuRanges {
			if r.file != idx {
				continue
			}
			if r.start > off {
				fadviseDontNeed(f, off, r.start-off)
			}
			off = r.end
		}
		if st != nil && st.Size() > off {
			fadviseDontNeed(f, off, st.Size()-off)
		}
		f.Close()
	}
}

// dropOtherModels lets the kernel drop the cached pages of the files of
// all other models: after a switch they only take RAM from the weights of
// the active model, and the kernel would rather drop those (newer) pages.
func (e *Engine) dropOtherModels(activeID string) {
	entries, err := os.ReadDir(e.cfg.ModelsDir)
	if err != nil {
		return
	}
	for _, d := range entries {
		if !d.IsDir() || d.Name() == activeID {
			continue
		}
		_ = filepath.WalkDir(filepath.Join(e.cfg.ModelsDir, d.Name()), func(p string, de os.DirEntry, err error) error {
			if err != nil || de.IsDir() || !strings.HasSuffix(p, ".gguf") {
				return nil
			}
			if f, err := os.Open(p); err == nil {
				fadviseDontNeed(f, 0, 0) // length 0: to the end of the file
				f.Close()
			}
			return nil
		})
	}
}

func fadviseDontNeed(f *os.File, off, n int64) {
	const posixFadvDontneed = 4
	_, _, _ = syscall.Syscall6(syscall.SYS_FADVISE64, f.Fd(), uintptr(off), uintptr(n), posixFadvDontneed, 0, 0)
}

// monitor refreshes the machine snapshot and the residency of the weights.
func (e *Engine) monitor(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		pid := e.currentPID()
		snap := e.sys.Read(pid)
		e.mu.Lock()
		pl := e.placement
		paths := append([]string(nil), e.paths...)
		e.mu.Unlock()
		var res int64 = -1
		if pl != nil && len(paths) > 0 {
			switch pl.loadMode {
			case "mmap":
				res = resident(paths, pl.cpuRanges)
			default:
				res = pl.RAM.Weights
			}
		}
		e.readingProgress(pid, pl, paths)
		e.mu.Lock()
		e.snapshot = snap
		if e.placement != nil && e.placement == pl && res >= 0 {
			if res > pl.RAM.Weights {
				res = pl.RAM.Weights
			}
			pl.RAM.WeightsResident = res
			pl.Disk.Weights = pl.RAM.Weights - res
		}
		e.mu.Unlock()
		if e.OnChange != nil {
			e.OnChange()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// readingProgress shows the phase before llama.cpp reports progress: the
// model files are mapped and read from the NVMe into the page cache. What
// is read is the larger of the process's disk reads and the share of the
// files already in the page cache (a warm cache is read at once).
func (e *Engine) readingProgress(pid int, pl *Placement, paths []string) {
	e.mu.Lock()
	reading := e.status.State == Loading && e.status.Stage == "reading" && e.childProgress == 0 && pid > 0
	e.mu.Unlock()
	if !reading || pl == nil {
		return
	}
	var whole []fileRange
	for i, p := range paths {
		if st, err := os.Stat(p); err == nil {
			whole = append(whole, fileRange{file: i, start: 0, end: st.Size()})
		}
	}
	done := resident(paths, whole)
	if rb := readBytes(pid); rb > done {
		done = rb
	}
	total := pl.FileBytes
	if total <= 0 {
		return
	}
	now := time.Now()
	e.mu.Lock()
	if !e.readPrevAt.IsZero() {
		if dt := now.Sub(e.readPrevAt).Seconds(); dt > 0 {
			inst := float64(done-e.readPrev) / dt
			if inst >= 0 {
				e.readRate = 0.5*e.readRate + 0.5*inst
			}
		}
	}
	e.readPrev, e.readPrevAt = done, now
	rate := e.readRate
	e.mu.Unlock()
	frac := float64(done) / float64(total)
	if frac > 0.99 {
		frac = 0.99
	}
	msg := fmt.Sprintf("Reading the model from the NVMe · %s of %s", fmtGB(done), fmtGB(total))
	if rate > 50<<20 {
		msg += fmt.Sprintf(" · %.1f GB/s", rate/gb)
	}
	e.set(func(s *Status) {
		if s.State == Loading && s.Stage == "reading" {
			s.Progress = progress(frac)
			s.Message = msg
		}
	})
}

// readBytes is what a process read from storage so far (/proc/<pid>/io).
func readBytes(pid int) int64 {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/io")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "read_bytes: "); ok {
			n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			return n
		}
	}
	return 0
}

func (e *Engine) detectVersion() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, e.cfg.ServerBin, "--version").CombinedOutput()
	if err != nil {
		return
	}
	for _, l := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(l, "version:") {
			e.mu.Lock()
			e.version = strings.TrimSpace(strings.TrimPrefix(l, "version:"))
			e.mu.Unlock()
		}
	}
}

// Health asks llama-server directly whether it answers.
func (e *Engine) Health(ctx context.Context) bool {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, e.BaseURL()+"/health", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// keep the import used when building without the placement helpers
var _ = gguf.ParseOverrides
