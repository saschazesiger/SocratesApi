package engine

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/saschazesiger/SocratesApi/internal/gguf"
)

// Meta is what Socrates API knows about a model from its GGUF header.
type Meta struct {
	Arch         string `json:"arch"`
	Name         string `json:"name"`
	Params       int64  `json:"params"`
	CtxTrain     int64  `json:"ctxTrain"`
	Layers       int    `json:"layers"`
	Experts      int    `json:"experts"`
	ExpertsUsed  int    `json:"expertsUsed"`
	MoE          bool   `json:"moe"`
	ExpertBytes  int64  `json:"expertBytes"` // bytes of all routed-expert tensors
	Vision       bool   `json:"vision"`      // an mmproj is there
	MTP          bool   `json:"mtp"`         // multi-token prediction layers are in the file
	ChatTemplate bool   `json:"chatTemplate"`
	Embedding    bool   `json:"embedding"`
	FileType     string `json:"fileType"`
}

// ReadMeta inspects the GGUF file(s) of a model.
func ReadMeta(paths []string, hasMMProj bool) (Meta, *gguf.File, error) {
	f, err := gguf.Open(paths...)
	if err != nil {
		return Meta{}, nil, err
	}
	m := Meta{
		Arch:        f.Arch(),
		Name:        f.String("general.name"),
		CtxTrain:    int64(f.Uint(".context_length")),
		Layers:      int(f.Uint(".block_count")),
		Experts:     int(f.Uint(".expert_count")),
		ExpertsUsed: int(f.Uint(".expert_used_count")),
		Vision:      hasMMProj,
		MTP:         f.Uint(".nextn_predict_layers") > 0,
		Embedding:   f.Has(f.Arch()+".pooling_type") && !f.Has("tokenizer.chat_template"),
	}
	m.ChatTemplate = f.Has("tokenizer.chat_template")
	for _, t := range f.Tensors {
		n := int64(1)
		for _, d := range t.Dims {
			n *= int64(d)
		}
		m.Params += n
		if t.IsExpert() {
			m.ExpertBytes += t.Size
		}
	}
	m.MoE = m.Experts > 1 && m.ExpertBytes > 0
	return m, f, nil
}

// Plan is where a model will live, decided before anything is loaded.
type Plan struct {
	OK        bool       `json:"ok"`
	Error     string     `json:"error"`
	Ctx       int        `json:"ctx"`
	GPULayers int        `json:"gpuLayers"` // as llama.cpp counts them: Layers+1 means all, output included
	CPUMoE    int        `json:"cpuMoe"`    // layers whose experts stay in RAM
	Overrides string     `json:"overrides"` // -ot value
	Args      []string   `json:"args"`
	Placement *Placement `json:"placement"`
	Fits      string     `json:"fits"` // gpu, hybrid, disk
	Summary   string     `json:"summary"`
	Speed     string     `json:"tokensPerSecondHint"` // fast, medium, slow
	TookMs    int64      `json:"tookMs"`
}

// Placement says how many bytes of a model are where.
type Placement struct {
	Source    string `json:"source"` // estimated or measured
	FileBytes int64  `json:"fileBytes"`
	Layers    struct {
		Total             int  `json:"total"`
		GPU               int  `json:"gpu"`
		ExpertLayersOnCPU int  `json:"expertLayersOnCpu"`
		MoE               bool `json:"moe"`
	} `json:"layers"`
	GPU struct {
		Weights int64 `json:"weights"`
		KV      int64 `json:"kv"`
		Compute int64 `json:"compute"`
		Extra   int64 `json:"extra"` // vision projector, MTP context
	} `json:"gpu"`
	RAM struct {
		Weights         int64 `json:"weights"`
		WeightsResident int64 `json:"weightsResident"`
		KV              int64 `json:"kv"`
		Compute         int64 `json:"compute"`
	} `json:"ram"`
	Disk struct {
		Weights int64 `json:"weights"`
		// Lookup tables (token embeddings, per-layer embeddings) in RAM's
		// share of the file: only the rows of the tokens at hand are read,
		// so they need not be in RAM and are not counted there.
		Lookup int64 `json:"lookup"`
	} `json:"disk"`
	Notes []string `json:"notes"`

	cpuRanges []fileRange // byte ranges of weights that live in host memory
	loadMode  string
}

type fileRange struct {
	file       int
	start, end int64
}

// Clone copies a placement, so a published status is never changed later.
func (p *Placement) Clone() *Placement {
	if p == nil {
		return nil
	}
	c := *p
	c.Notes = append([]string(nil), p.Notes...)
	return &c
}

// Planner runs llama.cpp's own fitting (llama fit-params) to decide the
// placement, so the plan matches what llama.cpp would do itself.
type Planner struct {
	LlamaBin string // the combined llama binary with the fit-params command
}

// System is the memory of the machine, for the verdict of a plan.
type System struct {
	GPUTotal     int64
	GPUFree      int64
	RAMTotal     int64
	RAMAvailable int64 // including what the current engine would give back
}

var fitOut = regexp.MustCompile(`-c (\d+) -ngl (-?\d+)(?: -ts [\d,]+)?(?: -ot "([^"]*)")?`)

// baseArgs are the model and memory flags shared by fit-params and the server.
func baseArgs(paths []string, s Settings, m Meta) []string {
	a := []string{"-m", paths[0]}
	a = append(a, "-c", strconv.Itoa(max(s.Context, 0))) // 0: the fit picks the largest that fits
	if s.GPULayers.Set {
		a = append(a, "-ngl", strconv.Itoa(s.GPULayers.Value))
	}
	if m.MoE && s.CPUMoE.Set {
		a = append(a, "--n-cpu-moe", strconv.Itoa(s.CPUMoE.Value))
	}
	a = append(a, "-ctk", s.KVType, "-ctv", s.KVType, "-fa", s.FlashAttn,
		"-np", strconv.Itoa(s.Parallel), "-b", strconv.Itoa(s.Batch), "-ub", strconv.Itoa(s.UBatch))
	switch s.LoadMode {
	case "mlock":
		a = append(a, "-lm", "mmap+mlock")
	case "ram":
		a = append(a, "-lm", "none")
	default:
		a = append(a, "-lm", "mmap")
	}
	if s.Threads > 0 {
		a = append(a, "-t", strconv.Itoa(s.Threads))
	}
	return a
}

const (
	minAutoCtx = 32768
	maxAutoCtx = 131072
)

func minCtx(m Meta) int {
	if m.CtxTrain > 0 && m.CtxTrain < minAutoCtx {
		return int(m.CtxTrain)
	}
	return minAutoCtx
}

// extraVRAM is what the fit of fit-params does not know about but the
// server will allocate on the GPU: the vision projector and the MTP context.
func extraVRAM(s Settings, m Meta, mmprojSize int64) int64 {
	var extra int64
	if *s.Vision && mmprojSize > 0 {
		extra += mmprojSize + 400<<20
	}
	if s.Speculative == "mtp" && m.MTP {
		extra += 600 << 20
	}
	return extra
}

// Make computes the plan for a model. It takes a few seconds: llama.cpp
// reads the header and computes the buffers without allocating them.
func (p *Planner) Make(ctx context.Context, paths []string, mmprojSize int64, raw Settings, f *gguf.File, m Meta, sys System) Plan {
	start := time.Now()
	s := raw.withDefaults(m)
	plan := Plan{}
	reserve := int64(s.GPUReserveMB)<<20 + extraVRAM(s, m, mmprojSize)

	fit := func(s Settings) ([]string, error) {
		args := append([]string{"fit-params"}, baseArgs(paths, s, m)...)
		args = append(args, "--fit-target", strconv.FormatInt(reserve>>20, 10))
		if s.Context == 0 {
			// Automatic context: the fit gives up context before it moves
			// weights off the GPU, but never below this.
			args = append(args, "--fit-ctx", strconv.Itoa(minCtx(m)))
		}
		out, err := p.run(ctx, args)
		if err != nil {
			return nil, err
		}
		g := fitOut.FindStringSubmatch(out)
		if g == nil {
			return nil, errors.New("llama.cpp did not find a placement: " + lastLine(out))
		}
		return g, nil
	}
	g, err := fit(s)
	if err == nil && s.Context == 0 {
		// A context larger than 128k mostly costs memory; use it when asked for.
		if n, _ := strconv.Atoi(g[1]); n > maxAutoCtx {
			s2 := s
			s2.Context = maxAutoCtx
			g, err = fit(s2)
		}
	}
	if err != nil {
		plan.Error = err.Error()
		return plan
	}
	plan.Ctx, _ = strconv.Atoi(g[1])
	plan.GPULayers, _ = strconv.Atoi(g[2])
	plan.Overrides = g[3]
	if plan.GPULayers < 0 || plan.GPULayers > m.Layers {
		plan.GPULayers = m.Layers + 1
	}
	if m.MoE && s.CPUMoE.Set && plan.Overrides == "" {
		plan.CPUMoE = s.CPUMoE.Value
	}

	// The memory llama.cpp expects with exactly these values.
	printArgs := []string{"fit-params", "--fit-print", "on"}
	printArgs = append(printArgs, p.resolved(paths, s, m, plan)...)
	pout, err := p.run(ctx, printArgs)
	if err != nil {
		plan.Error = err.Error()
		return plan
	}
	mem := parseFitPrint(pout)

	pl := placeTensors(f, m, plan)
	pl.Source = "estimated"
	pl.loadMode = s.LoadMode
	for _, d := range mem {
		if d.name == "Host" {
			pl.RAM.KV = d.ctx
			pl.RAM.Compute = d.compute
			if d.model-pl.Disk.Lookup > 0 {
				pl.RAM.Weights = d.model - pl.Disk.Lookup
			}
		} else {
			pl.GPU.Weights += d.model
			pl.GPU.KV += d.ctx
			pl.GPU.Compute += d.compute
		}
	}
	pl.GPU.Extra = extraVRAM(s, m, mmprojSize)
	if plan.CPUMoE == 0 {
		plan.CPUMoE = pl.Layers.ExpertLayersOnCPU
	}
	plan.Args = p.resolved(paths, s, m, plan)
	plan.Placement = pl
	plan.Fits, plan.Summary, plan.Speed = verdict(pl, m, sys)
	switch {
	case s.LoadMode == "mlock" && plan.Fits == "disk":
		pl.Notes = append(pl.Notes, "mlock pins the weights in RAM, but they do not fit: loading will fail. Use mmap, so the rest is read from the NVMe.")
	case s.LoadMode == "ram" && plan.Fits == "disk":
		pl.Notes = append(pl.Notes, "\"Copy into RAM\" needs all CPU weights in RAM, but they do not fit. Use mmap.")
	case plan.Fits == "disk":
		pl.Notes = append(pl.Notes, "Close other programs that use RAM, choose a smaller quantisation, or a shorter context, to keep more of it in RAM.")
	}
	plan.OK = true
	plan.TookMs = time.Since(start).Milliseconds()
	return plan
}

// resolved are the memory flags with the values the fit chose; the server
// then runs with --fit off, so it does exactly what was planned.
func (p *Planner) resolved(paths []string, s Settings, m Meta, plan Plan) []string {
	s2 := s
	s2.Context = plan.Ctx
	s2.GPULayers = Auto{Set: true, Value: plan.GPULayers}
	if plan.Overrides != "" {
		s2.CPUMoE = Auto{}
	}
	a := baseArgs(paths, s2, m)
	if plan.Overrides != "" {
		a = append(a, "-ot", plan.Overrides)
	}
	return append(a, "--fit", "off")
}

func (p *Planner) run(ctx context.Context, args []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.LlamaBin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", errors.New("llama.cpp took longer than 3 minutes to plan the placement")
		}
		return "", fmt.Errorf("llama.cpp could not plan the placement: %s", explainLlamaError(string(out)))
	}
	return string(out), nil
}

type devMem struct {
	name                string
	model, ctx, compute int64
}

// parseFitPrint reads lines like "CUDA0 203 288 304" (MiB).
func parseFitPrint(out string) []devMem {
	var res []devMem
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 4 {
			continue
		}
		var v [3]int64
		ok := true
		for i := 0; i < 3; i++ {
			n, err := strconv.ParseInt(f[i+1], 10, 64)
			if err != nil {
				ok = false
				break
			}
			v[i] = n << 20
		}
		if ok {
			res = append(res, devMem{name: f[0], model: v[0], ctx: v[1], compute: v[2]})
		}
	}
	return res
}

// placeTensors decides for every tensor whether it is on the GPU, by the
// rules of llama.cpp: the last n layers on the GPU, the output layer with
// them when all are, the token embeddings always on the CPU, and the
// overrides on top.
func placeTensors(f *gguf.File, m Meta, plan Plan) *Placement {
	pl := &Placement{}
	for _, sz := range f.Sizes {
		pl.FileBytes += sz
	}
	n := m.Layers
	ngl := plan.GPULayers
	firstGPU := n - ngl
	if firstGPU < 0 {
		firstGPU = 0
	}
	overrides := gguf.ParseOverrides(plan.Overrides)
	expertCPULayers := map[int]bool{}
	gpuLayers := map[int]bool{}
	for _, t := range f.Tensors {
		onGPU := false
		layer := t.Layer()
		switch {
		case layer >= 0:
			onGPU = layer >= firstGPU && ngl > 0
		case strings.Contains(t.Name, "token_embd"):
			// Input embeddings (also per-layer ones) stay on the CPU: llama.cpp
			// reads one row per token.
			onGPU = false
		default:
			onGPU = ngl > n
		}
		for _, o := range overrides {
			if o.Re.MatchString(t.Name) {
				onGPU = !strings.Contains(strings.ToUpper(o.Target), "CPU")
				break
			}
		}
		if plan.CPUMoE > 0 && t.IsExpert() && layer >= 0 && layer < plan.CPUMoE {
			onGPU = false
		}
		if t.IsExpert() && !onGPU && layer >= 0 {
			expertCPULayers[layer] = true
		}
		if onGPU && layer >= 0 && layer < n {
			gpuLayers[layer] = true
		}
		if !onGPU {
			if isLookup(t) {
				pl.Disk.Lookup += t.Size
				continue
			}
			pl.RAM.Weights += t.Size
			pl.cpuRanges = appendRange(pl.cpuRanges, fileRange{file: t.File, start: t.Offset, end: t.Offset + t.Size})
		}
	}
	pl.Layers.Total = n
	pl.Layers.GPU = len(gpuLayers)
	pl.Layers.ExpertLayersOnCPU = len(expertCPULayers)
	pl.Layers.MoE = m.MoE
	return pl
}

// isLookup reports whether a tensor is an embedding table that is read row
// by row (only the rows of the tokens at hand): large ones can stay on the
// NVMe without slowing the model down much.
func isLookup(t gguf.Tensor) bool {
	return strings.Contains(t.Name, "token_embd") && t.Size > 1<<30
}

func appendRange(rs []fileRange, r fileRange) []fileRange {
	if k := len(rs) - 1; k >= 0 && rs[k].file == r.file && rs[k].end >= r.start-64 {
		if r.end > rs[k].end {
			rs[k].end = r.end
		}
		return rs
	}
	return append(rs, r)
}

const gb = 1 << 30

func fmtGB(b int64) string {
	v := float64(b) / gb
	if v >= 100 {
		return fmt.Sprintf("%.0f GB", v)
	}
	return fmt.Sprintf("%.1f GB", v)
}

// verdict sums a placement up for the person.
func verdict(pl *Placement, m Meta, sys System) (fits, summary, speed string) {
	gpuUse := pl.GPU.Weights + pl.GPU.KV + pl.GPU.Compute + pl.GPU.Extra
	hostWeights := pl.RAM.Weights
	ram := sys.RAMAvailable - pl.RAM.KV - pl.RAM.Compute - 2*gb
	switch {
	case hostWeights < 2*gb && pl.Layers.GPU >= pl.Layers.Total:
		fits = "gpu"
		speed = "fast"
		summary = fmt.Sprintf("Everything runs on the GPU: %s of VRAM.", fmtGB(gpuUse))
	case hostWeights <= ram:
		fits = "hybrid"
		if m.MoE && pl.Layers.GPU >= pl.Layers.Total {
			speed = "medium"
			summary = fmt.Sprintf("GPU + RAM: %s on the GPU, the experts of %d layers (%s) in RAM.",
				fmtGB(gpuUse), pl.Layers.ExpertLayersOnCPU, fmtGB(hostWeights))
		} else {
			speed = "slow"
			summary = fmt.Sprintf("GPU + RAM: %d of %d layers on the GPU (%s), %s of weights in RAM.",
				pl.Layers.GPU, pl.Layers.Total, fmtGB(gpuUse), fmtGB(hostWeights))
		}
	default:
		fits = "disk"
		speed = "slow"
		spill := hostWeights - max64(ram, 0)
		summary = fmt.Sprintf("Larger than the free RAM: about %s of weights are read from the NVMe while it runs. It works, but slowly.",
			fmtGB(spill))
	}
	return
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) == 0 {
		return ""
	}
	return strings.TrimSpace(lines[len(lines)-1])
}

// explainLlamaError turns the last lines of a llama.cpp failure into a
// sentence a person can act on.
func explainLlamaError(out string) string {
	if s := knownLlamaError(out); s != "" {
		return s
	}
	return lastLine(out)
}

// knownLlamaError is explainLlamaError without the fallback: "" when the
// output says nothing specific.
func knownLlamaError(out string) string {
	low := strings.ToLower(out)
	switch {
	case strings.Contains(low, "unknown model architecture"):
		return "this llama.cpp version does not know the model's architecture. Update the Socrates API image, or pick another model."
	case strings.Contains(low, "failed to fit") || strings.Contains(low, "could not find allocations"):
		return "the model does not fit, not even with everything movable in RAM. Lower the context or choose a smaller quantisation."
	case strings.Contains(low, "no such file"):
		return "a model file is missing. Delete the model and download it again."
	case strings.Contains(low, "out of memory") || strings.Contains(low, "cudamalloc failed"):
		return "the GPU ran out of memory. Lower the context, keep more VRAM free (GPU reserve), or put fewer layers on the GPU."
	}
	// The last error-looking line.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := lines[i]
		if strings.Contains(strings.ToLower(l), "error") || strings.Contains(l, " E ") {
			return strings.TrimSpace(l)
		}
	}
	return ""
}

// MakeVirtual plans like Make, but against a VRAM budget instead of what is
// free right now: for a preview while the engine itself holds the GPU.
// llama.cpp measures the buffers of one configuration (--fit-print does
// not look at free memory); the search over context, experts and layers
// is done here with the exact tensor sizes of the file, then the chosen
// configuration is measured again.
func (p *Planner) MakeVirtual(ctx context.Context, paths []string, mmprojSize int64, raw Settings, f *gguf.File, m Meta, sys System, freeVRAM int64) Plan {
	start := time.Now()
	s := raw.withDefaults(m)
	plan := Plan{}
	budget := freeVRAM - int64(s.GPUReserveMB)<<20 - extraVRAM(s, m, mmprojSize)

	measure := func(pl Plan) (gpuModel, gpuCtx, gpuCompute int64, ok bool) {
		args := append([]string{"fit-params", "--fit-print", "on"}, p.resolved(paths, s, m, pl)...)
		out, err := p.run(ctx, args)
		if err != nil {
			plan.Error = err.Error()
			return 0, 0, 0, false
		}
		for _, d := range parseFitPrint(out) {
			if d.name != "Host" {
				gpuModel += d.model
				gpuCtx += d.ctx
				gpuCompute += d.compute
			}
		}
		return gpuModel, gpuCtx, gpuCompute, true
	}

	// Per layer: the bytes of its experts and of everything else.
	n := m.Layers
	expert := make([]int64, n+1)
	dense := make([]int64, n+1)
	for _, t := range f.Tensors {
		l := t.Layer()
		if l < 0 || l >= n {
			continue
		}
		if t.IsExpert() {
			expert[l] += t.Size
		} else {
			dense[l] += t.Size
		}
	}

	baseCtx := s.Context
	if baseCtx == 0 {
		baseCtx = minCtx(m)
	}
	all := Plan{Ctx: baseCtx, GPULayers: n + 1}
	gModel, gCtx, gCompute, ok := measure(all)
	if !ok {
		return plan
	}
	kvPerToken := float64(gCtx) / float64(baseCtx)
	ctxTokens := baseCtx
	choice := all
	switch need := gModel + gCtx + gCompute; {
	case need <= budget:
		if s.Context == 0 {
			// Automatic context: as much as the rest of the budget holds.
			c := int(float64(budget-gModel-gCompute) / kvPerToken)
			c = c / 256 * 256
			if lim := int(m.CtxTrain); lim > 0 && c > lim {
				c = lim
			}
			ctxTokens = min(max(c, baseCtx), maxAutoCtx)
			choice.Ctx = ctxTokens
		}
	default:
		kv := int64(kvPerToken * float64(ctxTokens))
		weights := gModel
		cpuMoe := 0
		if m.MoE && !s.CPUMoE.Set {
			for cpuMoe < n && weights+kv+gCompute > budget {
				weights -= expert[cpuMoe]
				cpuMoe++
			}
			choice.CPUMoE = cpuMoe
		}
		if s.CPUMoE.Set {
			choice.CPUMoE = s.CPUMoE.Value
		}
		layers := n + 1
		if !s.GPULayers.Set {
			// Still too large: whole layers leave the GPU, from the first one;
			// their share of the KV cache goes with them.
			first := 0
			for first < n && weights+kv+gCompute > budget {
				w := dense[first]
				if first >= cpuMoe {
					w += expert[first]
				}
				weights -= w
				kv -= int64(float64(kv) / float64(n-first))
				first++
			}
			if first > 0 {
				layers = n - first
			}
		} else {
			layers = s.GPULayers.Value
		}
		choice.GPULayers = layers
	}
	if s.GPULayers.Set {
		choice.GPULayers = s.GPULayers.Value
	}
	if choice.CPUMoE > 0 {
		var parts []string
		for i := 0; i < choice.CPUMoE; i++ {
			parts = append(parts, strconv.Itoa(i))
		}
		choice.Overrides = `blk\.(` + strings.Join(parts, "|") + `)\.ffn_(up|down|gate|gate_up)_(ch|)exps=CPU`
	}
	plan.Ctx, plan.GPULayers, plan.CPUMoE, plan.Overrides = choice.Ctx, choice.GPULayers, choice.CPUMoE, choice.Overrides

	printArgs := append([]string{"fit-params", "--fit-print", "on"}, p.resolved(paths, s, m, plan)...)
	pout, err := p.run(ctx, printArgs)
	if err != nil {
		plan.Error = err.Error()
		return plan
	}
	pl := placeTensors(f, m, plan)
	pl.Source = "estimated"
	pl.loadMode = s.LoadMode
	for _, d := range parseFitPrint(pout) {
		if d.name == "Host" {
			pl.RAM.KV, pl.RAM.Compute = d.ctx, d.compute
			if d.model-pl.Disk.Lookup > 0 {
				pl.RAM.Weights = d.model - pl.Disk.Lookup
			}
		} else {
			pl.GPU.Weights += d.model
			pl.GPU.KV += d.ctx
			pl.GPU.Compute += d.compute
		}
	}
	pl.GPU.Extra = extraVRAM(s, m, mmprojSize)
	plan.Args = p.resolved(paths, s, m, plan)
	plan.Placement = pl
	plan.Fits, plan.Summary, plan.Speed = verdict(pl, m, sys)
	if plan.Fits == "disk" {
		pl.Notes = append(pl.Notes, "Close other programs that use RAM, choose a smaller quantisation, or a shorter context, to keep more of it in RAM.")
	}
	pl.Notes = append(pl.Notes, "Planned against the VRAM that is free once the running model is unloaded.")
	plan.OK = true
	plan.TookMs = time.Since(start).Milliseconds()
	return plan
}
