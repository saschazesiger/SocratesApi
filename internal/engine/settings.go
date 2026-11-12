package engine

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Settings are the per-model choices of the person. Zero values mean
// "the default", so an empty object is a good start for any model.
type Settings struct {
	Context         int      `json:"context,omitempty"`   // tokens; 0 = default (32768 or less if the model is smaller); -1 = largest that fits
	GPULayers       Auto     `json:"gpuLayers,omitempty"` // auto or a number
	CPUMoE          Auto     `json:"cpuMoe,omitempty"`    // auto or a number of layers whose experts stay in RAM
	KVType          string   `json:"kvType,omitempty"`    // f16, q8_0, q4_0
	FlashAttn       string   `json:"flashAttn,omitempty"` // auto, on, off
	LoadMode        string   `json:"loadMode,omitempty"`  // mmap, mlock, ram
	Parallel        int      `json:"parallel,omitempty"`
	Threads         int      `json:"threads,omitempty"`
	Batch           int      `json:"batch,omitempty"`
	UBatch          int      `json:"ubatch,omitempty"`
	Speculative     string   `json:"speculative,omitempty"` // off, mtp
	Vision          *bool    `json:"vision,omitempty"`
	Reasoning       string   `json:"reasoning,omitempty"` // auto, on, off
	ReasoningBudget *int     `json:"reasoningBudget,omitempty"`
	GPUReserveMB    int      `json:"gpuReserveMb,omitempty"`
	Preload         *bool    `json:"preload,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	TopK            *int     `json:"topK,omitempty"`
	MinP            *float64 `json:"minP,omitempty"`
	PresencePenalty *float64 `json:"presencePenalty,omitempty"`
	RepeatPenalty   *float64 `json:"repeatPenalty,omitempty"`
	ExtraArgs       string   `json:"extraArgs,omitempty"`
}

// Auto is "auto" or a non-negative number in JSON.
type Auto struct {
	Set   bool
	Value int
}

func (a Auto) MarshalJSON() ([]byte, error) {
	if !a.Set {
		return []byte(`"auto"`), nil
	}
	return []byte(strconv.Itoa(a.Value)), nil
}

func (a *Auto) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "auto" || s == "null" {
		*a = Auto{}
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return fmt.Errorf("expected \"auto\" or a number, got %s", string(b))
	}
	*a = Auto{Set: true, Value: n}
	return nil
}

// ParseSettings reads stored settings; damaged settings become defaults.
func ParseSettings(raw json.RawMessage) Settings {
	var s Settings
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}

// Validate checks the values a person can type and says what is wrong
// with which field.
func (s Settings) Validate() map[string]string {
	errs := map[string]string{}
	if s.Context < -1 || s.Context > 4<<20 {
		errs["context"] = "Use a context between 512 and 4 194 304 tokens, or 0 for the default."
	} else if s.Context > 0 && s.Context < 512 {
		errs["context"] = "Use at least 512 tokens."
	}
	switch s.KVType {
	case "", "f16", "bf16", "q8_0", "q5_1", "q5_0", "q4_1", "q4_0":
	default:
		errs["kvType"] = "Choose f16, q8_0 or q4_0."
	}
	switch s.FlashAttn {
	case "", "auto", "on", "off":
	default:
		errs["flashAttn"] = "Choose auto, on or off."
	}
	switch s.LoadMode {
	case "", "mmap", "mlock", "ram":
	default:
		errs["loadMode"] = "Choose mmap, mlock or ram."
	}
	switch s.Speculative {
	case "", "off", "mtp":
	default:
		errs["speculative"] = "Choose off or mtp."
	}
	switch s.Reasoning {
	case "", "auto", "on", "off":
	default:
		errs["reasoning"] = "Choose auto, on or off."
	}
	if s.Parallel < 0 || s.Parallel > 64 {
		errs["parallel"] = "Use 1 to 64."
	}
	if s.Threads < 0 || s.Threads > 1024 {
		errs["threads"] = "Use 0 (automatic) or a number of threads."
	}
	if s.Batch != 0 && (s.Batch < 32 || s.Batch > 65536) {
		errs["batch"] = "Use 32 to 65 536."
	}
	if s.UBatch != 0 && (s.UBatch < 32 || s.UBatch > 65536) {
		errs["ubatch"] = "Use 32 to 65 536."
	}
	if s.GPUReserveMB < 0 || s.GPUReserveMB > 64<<10 {
		errs["gpuReserveMb"] = "Use 0 to 65 536 MiB."
	}
	if _, err := SplitArgs(s.ExtraArgs); err != nil {
		errs["extraArgs"] = err.Error()
	}
	for _, a := range mustSplit(s.ExtraArgs) {
		switch a {
		case "--host", "--port", "-m", "--model", "--api-key", "--api-key-file":
			errs["extraArgs"] = a + " is set by Socrates API; leave it out."
		}
	}
	return errs
}

func mustSplit(s string) []string { a, _ := SplitArgs(s); return a }

// SplitArgs splits a command line like a shell would, with quotes, but
// without any expansion.
func SplitArgs(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	in := false
	var quote rune
	have := false
	for _, c := range s {
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				cur.WriteRune(c)
			}
		case c == '"' || c == '\'':
			quote = c
			have = true
		case c == ' ' || c == '\t' || c == '\n':
			if have || in {
				out = append(out, cur.String())
				cur.Reset()
				have, in = false, false
			}
		default:
			cur.WriteRune(c)
			in = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("a quote is not closed")
	}
	if have || in {
		out = append(out, cur.String())
	}
	return out, nil
}

// withDefaults fills in what the person left open, for a model with meta.
func (s Settings) withDefaults(m Meta) Settings {
	if s.Context == 0 {
		s.Context = 32768
		if m.CtxTrain > 0 && int(m.CtxTrain) < s.Context {
			s.Context = int(m.CtxTrain)
		}
	}
	if s.KVType == "" {
		s.KVType = "q8_0"
	}
	if s.FlashAttn == "" {
		s.FlashAttn = "auto"
	}
	if s.LoadMode == "" {
		s.LoadMode = "mmap"
	}
	if s.Parallel == 0 {
		s.Parallel = 1
	}
	if s.Batch == 0 {
		s.Batch = 2048
	}
	if s.UBatch == 0 {
		s.UBatch = 512
	}
	if s.Speculative == "" {
		s.Speculative = "off"
		if m.MTP {
			s.Speculative = "mtp"
		}
	}
	if s.Reasoning == "" {
		s.Reasoning = "auto"
	}
	if s.GPUReserveMB == 0 {
		s.GPUReserveMB = 1024
	}
	if s.Preload == nil {
		t := true
		s.Preload = &t
	}
	if s.Vision == nil {
		v := m.Vision
		s.Vision = &v
	}
	return s
}
