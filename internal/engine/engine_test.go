package engine

import (
	"encoding/json"
	"testing"

	"github.com/saschazesiger/SocratesApi/internal/gguf"
)

func TestFitOutput(t *testing.T) {
	g := fitOut.FindStringSubmatch("0.00.659 I llama_fit_params: printing\n-c 32768 -ngl 49 -ot \"blk\\.(0|1|2)\\.ffn_(up|down|gate)_exps=CPU\"\n")
	if g == nil || g[1] != "32768" || g[2] != "49" || g[3] != `blk\.(0|1|2)\.ffn_(up|down|gate)_exps=CPU` {
		t.Fatalf("got %q", g)
	}
	if g := fitOut.FindStringSubmatch("-c 40960 -ngl -1\n"); g == nil || g[2] != "-1" || g[3] != "" {
		t.Fatalf("got %q", g)
	}
}

func TestFitPrint(t *testing.T) {
	mem := parseFitPrint("x I printing\nCUDA0 203 288 304 \nHost 290 608 16\n")
	if len(mem) != 2 || mem[0].name != "CUDA0" || mem[0].model != 203<<20 || mem[1].ctx != 608<<20 {
		t.Fatalf("got %+v", mem)
	}
}

func TestPlaceTensors(t *testing.T) {
	f := &gguf.File{Sizes: []int64{1000}}
	add := func(name string, size int64) {
		off := int64(0)
		if n := len(f.Tensors); n > 0 {
			off = f.Tensors[n-1].Offset + f.Tensors[n-1].Size
		}
		f.Tensors = append(f.Tensors, gguf.Tensor{Name: name, Offset: off, Size: size})
	}
	add("token_embd.weight", 10)
	for _, l := range []string{"0", "1", "2", "3"} {
		add("blk."+l+".attn_q.weight", 5)
		add("blk."+l+".ffn_up_exps.weight", 20)
	}
	add("output.weight", 10)
	m := Meta{Layers: 4, MoE: true}
	// All layers on the GPU, the experts of layers 0 and 1 in RAM.
	pl := placeTensors(f, m, Plan{GPULayers: 5, Overrides: `blk\.(0|1)\.ffn_(up|down|gate)_exps=CPU`})
	if pl.RAM.Weights != 10+40 || pl.Layers.GPU != 4 || pl.Layers.ExpertLayersOnCPU != 2 {
		t.Fatalf("got ram %d gpu layers %d expert layers %d", pl.RAM.Weights, pl.Layers.GPU, pl.Layers.ExpertLayersOnCPU)
	}
	// Two layers on the GPU: layers 0 and 1 and the output stay on the CPU.
	pl = placeTensors(f, m, Plan{GPULayers: 2})
	if pl.RAM.Weights != 10+25+25+10 || pl.Layers.GPU != 2 {
		t.Fatalf("got ram %d gpu layers %d", pl.RAM.Weights, pl.Layers.GPU)
	}
}

func TestSettings(t *testing.T) {
	var s Settings
	if err := json.Unmarshal([]byte(`{"gpuLayers":"auto","cpuMoe":12,"context":65536}`), &s); err != nil {
		t.Fatal(err)
	}
	if s.GPULayers.Set || !s.CPUMoE.Set || s.CPUMoE.Value != 12 {
		t.Fatalf("got %+v", s)
	}
	if errs := s.Validate(); len(errs) != 0 {
		t.Fatalf("unexpected errors %v", errs)
	}
	s.ExtraArgs = "--port 1"
	if errs := s.Validate(); errs["extraArgs"] == "" {
		t.Fatal("--port must be refused")
	}
	args, err := SplitArgs(`--override-kv "a=b c" -x 'q r'`)
	if err != nil || len(args) != 4 || args[1] != "a=b c" || args[3] != "q r" {
		t.Fatalf("got %q %v", args, err)
	}
}
