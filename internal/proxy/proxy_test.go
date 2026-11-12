package proxy

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCopyStreamStripsProgress(t *testing.T) {
	in := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":null}}],"prompt_progress":{"total":10,"cache":0,"processed":0}}`, ``,
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":null}}],"prompt_progress":{"total":10,"cache":0,"processed":5}}`, ``,
		`data: {"choices":[{"index":0,"delta":{"content":"Hi"}}]}`, ``,
		`data: {"choices":[{"index":0,"delta":{}}],"timings":{"prompt_n":10,"cache_n":2,"predicted_n":1,"prompt_per_second":100,"predicted_per_second":20}}`, ``,
		`data: [DONE]`, ``, ``}, "\n")
	p := New(nil, nil)
	p.inflight["r1"] = &Req{ID: "r1"}
	w := httptest.NewRecorder()
	var u usage
	p.copyStream(w, strings.NewReader(in), "r1", &u, true)
	out := w.Body.String()
	if n := strings.Count(out, "prompt_progress"); n != 1 {
		t.Errorf("want only the first role chunk, got %d progress chunks:\n%s", n, out)
	}
	if !strings.Contains(out, `"content":"Hi"`) || !strings.Contains(out, "[DONE]") {
		t.Errorf("content lost:\n%s", out)
	}
	if strings.Contains(out, "\n\n\n") {
		t.Errorf("blank lines of dropped events left behind:\n%q", out)
	}
	if u.prompt != 12 || u.cached != 2 || u.completion != 1 || u.genTPS != 20 {
		t.Errorf("usage %+v", u)
	}
	if r := p.inflight["r1"]; r.PromptTokens != 10 || r.Generated != 1 {
		t.Errorf("live state %+v", r)
	}
}

func TestBearer(t *testing.T) {
	r := httptest.NewRequest("GET", "/v1/models", nil)
	r.Header.Set("x-api-key", "sk-soc-abc")
	if bearer(r) != "sk-soc-abc" {
		t.Fatal("x-api-key not read")
	}
	r.Header.Set("Authorization", "Bearer sk-soc-def")
	if bearer(r) != "sk-soc-def" {
		t.Fatal("bearer not preferred")
	}
}
