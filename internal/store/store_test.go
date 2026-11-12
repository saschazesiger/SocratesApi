package store

import "testing"

func TestModelsAndKeys(t *testing.T) {
	st, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.PutModel(Model{ID: "m", Name: "M", State: "downloading", Files: []File{{Path: "a.gguf", Size: 1}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetModelState("m", "ready", ""); err != nil {
		t.Fatal(err)
	}
	// A rename must not bring back the old state.
	if err := st.EditModel("m", "New", []byte(`{"context":8192}`)); err != nil {
		t.Fatal(err)
	}
	m, err := st.Model("m")
	if err != nil || m.State != "ready" || m.Name != "New" || string(m.Settings) != `{"context":8192}` || len(m.Files) != 1 {
		t.Fatalf("got %+v %v", m, err)
	}
	if err := st.EditModel("gone", "x", []byte("{}")); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	k, err := st.AddKey("opencode", "sk-soc-abcd", "hash1")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.LogRequest(Request{At: 1, Source: "api", KeyID: k.ID, KeyName: "opencode", Path: "/v1/chat/completions", PromptTokens: 10, CompletionTokens: 5}); err != nil {
		t.Fatal(err)
	}
	got, err := st.KeyByHash("hash1")
	if err != nil || got.Requests != 1 || got.TokensIn != 10 || got.TokensOut != 5 {
		t.Fatalf("got %+v %v", got, err)
	}
	if ok, _ := st.SetJSONIfMissing("auth.password", "x"); !ok {
		t.Fatal("first set must win")
	}
	if ok, _ := st.SetJSONIfMissing("auth.password", "y"); ok {
		t.Fatal("second set must lose")
	}
}
