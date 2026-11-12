package hf

import "testing"

func TestGroupVariants(t *testing.T) {
	files := []RepoFile{
		{Name: "M-27B-UD-Q4_K_XL.gguf", Size: 16 << 30},
		{Name: "M-27B-Q8_0.gguf", Size: 27 << 30},
		{Name: "M-27B-UD-DW-Q4_K_M.gguf", Size: 15 << 30},
		{Name: "patch/M-27B-patch-1.gguf", Size: 1 << 30},
		{Name: "BF16/M-27B-BF16-00001-of-00003.gguf", Size: 10 << 20},
		{Name: "BF16/M-27B-BF16-00002-of-00003.gguf", Size: 25 << 30},
		{Name: "BF16/M-27B-BF16-00003-of-00003.gguf", Size: 25<<30 - 10<<20},
		{Name: "MTP/mtp-M-27B-Q8_0.gguf", Size: 2 << 30},
		{Name: "BF16/imatrix_micro3.gguf", Size: 1 << 30},
	}
	vs := groupVariants(files)
	if len(vs) != 4 {
		t.Fatalf("got %d variants: %+v", len(vs), vs)
	}
	want := map[string]int64{"UD-DW-Q4_K_M": 15 << 30, "UD-Q4_K_XL": 16 << 30, "Q8_0": 27 << 30, "BF16": 50 << 30}
	for _, v := range vs {
		if want[v.Quant] != v.Size {
			t.Errorf("variant %q size %d", v.Quant, v.Size)
		}
	}
	if vs[3].Quant != "BF16" || len(vs[3].Files) != 3 || vs[3].Files[0] != "BF16/M-27B-BF16-00001-of-00003.gguf" {
		t.Errorf("split files not grouped: %+v", vs[3])
	}
	if vs[1].Bits == 0 {
		t.Errorf("no bits for %s", vs[1].Quant)
	}
}

func TestParseID(t *testing.T) {
	for in, want := range map[string]string{
		"huihui-ai/Huihui-Qwen3.8-27B-abliterated":                  "huihui-ai/Huihui-Qwen3.8-27B-abliterated",
		"https://huggingface.co/unsloth/Qwen3.8-27B-GGUF/tree/main": "unsloth/Qwen3.8-27B-GGUF",
		"hf.co/unsloth/Qwen3.8-27B-GGUF:UD-Q4_K_XL":                 "unsloth/Qwen3.8-27B-GGUF",
		"qwen": "",
	} {
		if got := ParseID(in); got != want {
			t.Errorf("ParseID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAssessRecommendsGPUQuant(t *testing.T) {
	vs := groupVariants([]RepoFile{
		{Name: "M-Q2_K.gguf", Size: 10 << 30},
		{Name: "M-Q4_K_M.gguf", Size: 16 << 30},
		{Name: "M-Q6_K.gguf", Size: 22 << 30},
		{Name: "M-Q8_0.gguf", Size: 28 << 30},
	})
	Assess(vs, Machine{VRAM: 24 << 30, RAM: 40 << 30, DiskFree: 1 << 40}, 0, false)
	for _, v := range vs {
		if v.Recommended && v.Quant != "Q4_K_M" {
			t.Errorf("recommended %s", v.Quant)
		}
		if v.Quant == "Q8_0" && v.Fit != "hybrid" {
			t.Errorf("Q8_0 fit %s", v.Fit)
		}
	}
}

func TestDuplicateLabels(t *testing.T) {
	vs := groupVariants([]RepoFile{
		{Name: "a/M-UD-Q4_K_XL-00001-of-00002.gguf", Size: 10 << 20},
		{Name: "a/M-UD-Q4_K_XL-00002-of-00002.gguf", Size: 50 << 30},
		{Name: "b-V2/M-UD-Q4_K_XL-00001-of-00002.gguf", Size: 10 << 20},
		{Name: "b-V2/M-UD-Q4_K_XL-00002-of-00002.gguf", Size: 50 << 30},
		{Name: "c/M-UD-IQ4_XS.gguf", Size: 40 << 30},
	})
	labels := map[string]bool{}
	for _, v := range vs {
		if labels[v.Quant] {
			t.Fatalf("duplicate label %q", v.Quant)
		}
		labels[v.Quant] = true
	}
	if !labels["Q4_K_XL (b-V2)"] {
		t.Fatalf("labels %v", labels)
	}
}

func TestSharedEngramShard(t *testing.T) {
	vs := groupVariants([]RepoFile{
		{Name: "DS-1017-IQ2_XXS-00001-of-00004.gguf", Size: 10 << 20},
		{Name: "DS-1017-IQ2_XXS-00002-of-00004.gguf", Size: 40 << 30},
		{Name: "DS-1017-IQ2_XXS-00003-of-00004.gguf", Size: 40 << 30},
		{Name: "DS-Engram-IQ4_NL.gguf", Size: 110 << 30},
	})
	if len(vs) != 1 || len(vs[0].Files) != 4 || vs[0].Files[3] != "DS-Engram-IQ4_NL.gguf" {
		t.Fatalf("got %+v", vs)
	}
}

func TestAssessMoEPrefersQuality(t *testing.T) {
	vs := groupVariants([]RepoFile{
		{Name: "M-35B-A3B-Q2_K.gguf", Size: 13e9},
		{Name: "M-35B-A3B-Q3_K.gguf", Size: 17e9},
		{Name: "M-35B-A3B-Q4_K.gguf", Size: 21.7e9},
		{Name: "M-35B-A3B-Q5_K.gguf", Size: 25.3e9},
		{Name: "M-35B-A3B-Q8_0.gguf", Size: 37.8e9},
	})
	Assess(vs, Machine{VRAM: 24 << 30, RAM: 48 << 30, DiskFree: 1 << 40}, 9e8, true)
	for _, v := range vs {
		if v.Recommended && v.Quant != "Q4_K" {
			t.Errorf("recommended %s", v.Quant)
		}
	}
	if !activeRe.MatchString("huihui-ai/Huihui-Qwen3.6-35B-A3B-abliterated-MTP-GGUF") {
		t.Error("A3B not recognised")
	}
}
