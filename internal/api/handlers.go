package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/saschazesiger/SocratesApi/internal/convert"
	"github.com/saschazesiger/SocratesApi/internal/engine"
	"github.com/saschazesiger/SocratesApi/internal/hf"
	"github.com/saschazesiger/SocratesApi/internal/proxy"
	"github.com/saschazesiger/SocratesApi/internal/store"
	"github.com/saschazesiger/SocratesApi/internal/sysinfo"
)

// --- status ------------------------------------------------------------------

type statusView struct {
	Engine    engine.Status      `json:"engine"`
	Activity  proxy.Activity     `json:"activity"`
	Placement *engine.Placement  `json:"placement"`
	System    sysinfo.Snapshot   `json:"system"`
	Downloads []hf.Progress      `json:"downloads"`
	Converts  []convert.Progress `json:"converts"`
}

func (s *Server) status() statusView {
	v := statusView{
		Engine:    s.d.Engine.Status(),
		Activity:  s.d.Proxy.Activity(),
		Placement: s.d.Engine.Placement(),
		System:    s.d.Engine.System(),
		Downloads: s.d.Downloader.Progress(),
		Converts:  []convert.Progress{},
	}
	if s.d.Converter != nil {
		v.Converts = s.d.Converter.Progress()
	}
	return v
}

// --- settings ----------------------------------------------------------------

type settings struct {
	HFToken    string `json:"hfToken"`
	RequireKey *bool  `json:"requireKey"`
}

func (s *Server) settings() settings {
	var st settings
	_ = s.d.Store.GetJSON("settings", &st)
	if st.RequireKey == nil {
		t := true
		st.RequireKey = &t
	}
	return st
}

// HFToken is the Hugging Face token, for the client.
func (s *Server) HFToken() string { return s.settings().HFToken }

func (s *Server) requireKey() bool { return *s.settings().RequireKey }

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	st := s.settings()
	tok := ""
	if st.HFToken != "" {
		tok = "set"
	}
	writeJSON(w, 200, map[string]any{"hfToken": tok, "requireKey": *st.RequireKey, "modelsDir": s.d.ModelsDir})
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		HFToken    *string `json:"hfToken"`
		RequireKey *bool   `json:"requireKey"`
	}
	if !decode(w, r, &req) {
		return
	}
	st := s.settings()
	if req.HFToken != nil {
		t := strings.TrimSpace(*req.HFToken)
		if t != "" && !strings.HasPrefix(t, "hf_") {
			writeJSON(w, 400, apiError{Error: "A Hugging Face token starts with hf_.", Fields: map[string]string{"hfToken": "A Hugging Face token starts with hf_."}})
			return
		}
		st.HFToken = t
	}
	if req.RequireKey != nil {
		st.RequireKey = req.RequireKey
	}
	if err := s.d.Store.SetJSON("settings", st); err != nil {
		s.fail(w, err)
		return
	}
	s.hub.notify("settings")
	s.getSettings(w, r)
}

// --- models ------------------------------------------------------------------

type modelView struct {
	store.Model
	Progress *float64        `json:"progress"`
	Active   bool            `json:"active"`
	Settings engine.Settings `json:"settings"`
}

func (s *Server) view(m store.Model, active string) modelView {
	v := modelView{Model: m, Active: m.ID == active, Settings: engine.ParseSettings(m.Settings)}
	for _, p := range s.d.Downloader.Progress() {
		if p.ModelID == m.ID && p.Total > 0 {
			f := float64(p.Done) / float64(p.Total)
			v.Progress = &f
		}
	}
	if s.d.Converter != nil {
		for _, p := range s.d.Converter.Progress() {
			if p.ModelID == m.ID {
				f := p.Progress
				v.Progress = &f
			}
		}
	}
	return v
}

func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	ms, err := s.d.Store.Models()
	if err != nil {
		s.fail(w, err)
		return
	}
	active := s.d.Engine.ActiveID()
	out := []modelView{}
	for _, m := range ms {
		out = append(out, s.view(m, active))
	}
	snap := s.d.Engine.System()
	writeJSON(w, 200, map[string]any{"models": out, "activeId": active, "dataDir": s.d.ModelsDir, "freeBytes": snap.Disk.Free})
}

var slugRe = regexp.MustCompile(`[^a-z0-9._-]+`)

// newModelID makes a readable, unique id from the repository and the quant:
// "huihui-qwen3.8-27b-abliterated-q4_k".
func (s *Server) newModelID(repo, quant string) string {
	name := strings.ToLower(path.Base(repo))
	name = strings.TrimSuffix(strings.TrimSuffix(name, "-gguf"), "_gguf")
	base := slugRe.ReplaceAllString(name, "-")
	if q := strings.ToLower(quant); q != "" && !strings.Contains(base, q) {
		base += "-" + slugRe.ReplaceAllString(q, "-")
	}
	base = strings.Trim(base, "-.")
	if len(base) > 80 {
		base = base[:80]
	}
	if base == "" {
		base = "model"
	}
	id := base
	for i := 2; ; i++ {
		if _, err := s.d.Store.Model(id); errors.Is(err, store.ErrNotFound) {
			return id
		}
		id = base + "-" + strconv.Itoa(i)
	}
}

func (s *Server) addModel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo   string   `json:"repo"`
		Files  []string `json:"files"`
		MMProj string   `json:"mmproj"`
		Name   string   `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	repoID := hf.ParseID(req.Repo)
	if repoID == "" || !hf.ValidID(repoID) {
		writeJSON(w, 400, apiError{Error: "Enter a Hugging Face model as owner/name.", Fields: map[string]string{"repo": "Enter a Hugging Face model as owner/name."}})
		return
	}
	if len(req.Files) == 0 {
		writeError(w, 400, "Choose a file (a quantisation) to download.")
		return
	}
	sort.Strings(req.Files) // split models: part 1 first, llama.cpp opens the rest itself
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	repo, err := s.d.HF.Repo(ctx, repoID)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	var files []store.File
	var jobFiles []hf.RepoFile
	var total int64
	for _, name := range req.Files {
		f, ok := repo.File(name)
		if !ok || !strings.HasSuffix(strings.ToLower(name), ".gguf") {
			writeError(w, 400, fmt.Sprintf("%s is not a GGUF file of %s.", name, repoID))
			return
		}
		kind := ""
		if hf.IsShared(name) && len(req.Files) > 1 {
			kind = "shared" // linked as the missing part once downloaded
		}
		files = append(files, store.File{Path: f.Name, Size: f.Size, SHA256: f.SHA256, Kind: kind})
		jobFiles = append(jobFiles, f)
		total += f.Size
	}
	mmproj := ""
	if req.MMProj != "" {
		f, ok := repo.File(req.MMProj)
		if !ok {
			writeError(w, 400, fmt.Sprintf("%s is not a file of %s.", req.MMProj, repoID))
			return
		}
		mmproj = f.Name
		files = append(files, store.File{Path: f.Name, Size: f.Size, SHA256: f.SHA256, Kind: "mmproj"})
		jobFiles = append(jobFiles, f)
		total += f.Size
	}
	snap := s.d.Engine.System()
	if snap.Disk.Free > 0 && total > snap.Disk.Free-2<<30 {
		writeError(w, 400, fmt.Sprintf("Not enough disk space: the model needs %.1f GB, %.1f GB are free.", float64(total)/(1<<30), float64(snap.Disk.Free)/(1<<30)))
		return
	}
	quant := hf.QuantOf(req.Files[0])
	id := s.newModelID(repoID, quant)
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = strings.TrimSuffix(strings.TrimSuffix(path.Base(repoID), "-GGUF"), "-gguf") + " · " + quant
	}
	m := store.Model{ID: id, Name: name, Repo: repoID, Revision: "main", Files: files, MMProj: mmproj, Quant: quant,
		Size: total, State: "downloading"}
	if err := s.d.Store.PutModel(m); err != nil {
		s.fail(w, err)
		return
	}
	s.d.Downloader.Add(hf.Job{ModelID: id, Repo: repoID, Revision: "main", Dir: s.d.Engine.ModelDir(id), Files: jobFiles})
	s.d.Logf("model %s: downloading %s from %s", id, strings.Join(req.Files, ", "), repoID)
	s.hub.notify("models")
	m, _ = s.d.Store.Model(id)
	writeJSON(w, 200, s.view(m, s.d.Engine.ActiveID()))
}

// jobOf rebuilds the download job of a model from its row.
func (s *Server) jobOf(m store.Model) hf.Job {
	j := hf.Job{ModelID: m.ID, Repo: m.Repo, Revision: m.Revision, Dir: s.d.Engine.ModelDir(m.ID)}
	haveMM := false
	for _, f := range m.Files {
		j.Files = append(j.Files, hf.RepoFile{Name: f.Path, Size: f.Size, SHA256: f.SHA256})
		haveMM = haveMM || f.Kind == "mmproj"
	}
	if m.MMProj != "" && !haveMM {
		j.Files = append(j.Files, hf.RepoFile{Name: m.MMProj})
	}
	return j
}

// resume continues downloads and conversions after a restart.
func (s *Server) resume() {
	ms, err := s.d.Store.Models()
	if err != nil {
		return
	}
	for _, m := range ms {
		switch m.State {
		case "downloading":
			s.d.Downloader.Add(s.jobOf(m))
		case "converting":
			if s.d.Converter != nil {
				s.d.Converter.Resume(m)
			}
		}
	}
}

func (s *Server) downloadDone(j hf.Job, err error) {
	m, gerr := s.d.Store.Model(j.ModelID)
	if gerr != nil {
		return
	}
	if err != nil {
		s.d.Logf("model %s: download failed: %v", m.ID, err)
		_ = s.d.Store.SetModelState(m.ID, "failed", "Download failed: "+err.Error())
		s.hub.notify("models")
		return
	}
	s.finishModel(m)
}

// finishModel reads the metadata of a model whose files are complete and
// makes it the active one if none is.
func (s *Server) finishModel(m store.Model) {
	if err := s.linkShared(&m); err != nil {
		_ = s.d.Store.SetModelState(m.ID, "failed", "Linking the shared part failed: "+err.Error())
		s.hub.notify("models")
		return
	}
	meta, err := s.d.Engine.ModelMeta(m)
	if err != nil {
		_ = s.d.Store.SetModelState(m.ID, "failed", "The downloaded file cannot be read: "+err.Error())
		s.hub.notify("models")
		return
	}
	b, _ := json.Marshal(meta)
	m.Meta = b
	m.State = "ready"
	m.Error = ""
	if err := s.d.Store.PutModel(m); err != nil {
		s.d.Logf("model %s: %v", m.ID, err)
		return
	}
	s.d.Logf("model %s is ready (%s, %d layers)", m.ID, meta.Arch, meta.Layers)
	if s.d.Engine.ActiveID() == "" {
		_ = s.d.Engine.Activate(m.ID)
	} else {
		s.d.Engine.Reconcile()
	}
	s.hub.notify("models")
}

// linkShared links a shared file (the Engram tables of DeepSeek V4.1) as
// the part a split build is missing, as llama.cpp expects it.
func (s *Server) linkShared(m *store.Model) error {
	var shared *store.File
	parts := map[int]bool{}
	first := ""
	total := 0
	for i := range m.Files {
		f := &m.Files[i]
		switch f.Kind {
		case "shared":
			shared = f
		case "":
			if p, n := hf.SplitParts(f.Path); n > 0 {
				parts[p] = true
				total = n
				if p == 1 {
					first = f.Path
				}
			}
		}
	}
	if shared == nil || first == "" {
		return nil
	}
	dir := s.d.Engine.ModelDir(m.ID)
	for p := 1; p <= total; p++ {
		if parts[p] {
			continue
		}
		name := strings.Replace(first, fmt.Sprintf("-%05d-of-%05d.gguf", 1, total), fmt.Sprintf("-%05d-of-%05d.gguf", p, total), 1)
		link := filepath.Join(dir, filepath.FromSlash(name))
		target, err := filepath.Rel(filepath.Dir(link), filepath.Join(dir, filepath.FromSlash(shared.Path)))
		if err != nil {
			return err
		}
		_ = os.Remove(link)
		if err := os.Symlink(target, link); err != nil {
			return err
		}
		m.Files = append(m.Files, store.File{Path: name, Size: shared.Size})
	}
	sort.SliceStable(m.Files, func(i, j int) bool {
		if (m.Files[i].Kind == "") != (m.Files[j].Kind == "") {
			return m.Files[i].Kind == ""
		}
		return m.Files[i].Path < m.Files[j].Path
	})
	return nil
}

func (s *Server) convertDone(m store.Model, err error) {
	if err != nil {
		_ = s.d.Store.SetModelState(m.ID, "failed", err.Error())
		s.hub.notify("models")
		return
	}
	s.finishModel(m)
}

func (s *Server) model(w http.ResponseWriter, r *http.Request) (store.Model, bool) {
	m, err := s.d.Store.Model(r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, 404, "This model does not exist (any more).")
		return m, false
	}
	if err != nil {
		s.fail(w, err)
		return m, false
	}
	return m, true
}

func (s *Server) editModel(w http.ResponseWriter, r *http.Request) {
	m, ok := s.model(w, r)
	if !ok {
		return
	}
	var req struct {
		Name     *string          `json:"name"`
		Settings *engine.Settings `json:"settings"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Name != nil {
		n := strings.TrimSpace(*req.Name)
		if n == "" || len(n) > 120 {
			writeJSON(w, 400, apiError{Error: "Give the model a name (up to 120 characters).", Fields: map[string]string{"name": "Give the model a name."}})
			return
		}
		m.Name = n
	}
	if req.Settings != nil {
		if errs := req.Settings.Validate(); len(errs) > 0 {
			writeJSON(w, 400, apiError{Error: "Some settings are not valid.", Fields: errs})
			return
		}
		b, _ := json.Marshal(req.Settings)
		m.Settings = b
	}
	if err := s.d.Store.EditModel(m.ID, m.Name, m.Settings); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, 404, "This model does not exist (any more).")
			return
		}
		s.fail(w, err)
		return
	}
	s.d.Engine.Reconcile()
	s.hub.notify("models")
	m, _ = s.d.Store.Model(m.ID)
	writeJSON(w, 200, s.view(m, s.d.Engine.ActiveID()))
}

func (s *Server) deleteModel(w http.ResponseWriter, r *http.Request) {
	m, ok := s.model(w, r)
	if !ok {
		return
	}
	s.d.Downloader.Cancel(m.ID)
	if s.d.Converter != nil {
		s.d.Converter.Cancel(m.ID)
	}
	if s.d.Engine.ActiveID() == m.ID {
		_ = s.d.Engine.Stop()
	}
	if err := s.d.Store.DeleteModel(m.ID); err != nil {
		s.fail(w, err)
		return
	}
	dir := s.d.Engine.ModelDir(m.ID)
	if filepath.Dir(dir) == filepath.Clean(s.d.ModelsDir) {
		go func() {
			// The engine lets go of the files first; Linux keeps them while mapped anyway.
			time.Sleep(time.Second)
			if err := os.RemoveAll(dir); err != nil {
				s.d.Logf("remove %s: %v", dir, err)
			}
			s.hub.statusChanged()
		}()
	}
	s.d.Logf("model %s deleted", m.ID)
	s.hub.notify("models")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) activateModel(w http.ResponseWriter, r *http.Request) {
	m, ok := s.model(w, r)
	if !ok {
		return
	}
	if m.State != "ready" {
		writeError(w, 409, "This model is not ready yet: wait until its download has finished.")
		return
	}
	if err := s.d.Engine.Activate(m.ID); err != nil {
		s.fail(w, err)
		return
	}
	s.hub.notify("models")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) planModel(w http.ResponseWriter, r *http.Request) {
	m, ok := s.model(w, r)
	if !ok {
		return
	}
	if m.State != "ready" {
		writeError(w, 409, "The placement can be planned once the model is downloaded.")
		return
	}
	var req struct {
		Settings engine.Settings `json:"settings"`
	}
	if !decode(w, r, &req) {
		return
	}
	if errs := req.Settings.Validate(); len(errs) > 0 {
		writeJSON(w, 400, apiError{Error: "Some settings are not valid.", Fields: errs})
		return
	}
	plan, err := s.d.Engine.Plan(r.Context(), m, req.Settings)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, plan)
}

func (s *Server) retryModel(w http.ResponseWriter, r *http.Request) {
	m, ok := s.model(w, r)
	if !ok {
		return
	}
	if m.State != "failed" {
		writeError(w, 409, "Only a failed download or conversion can be retried.")
		return
	}
	if strings.HasPrefix(m.Repo, "convert:") || m.Quant != "" && s.d.Converter != nil && s.d.Converter.Owns(m) {
		s.d.Converter.Resume(m)
	} else {
		_ = s.d.Store.SetModelState(m.ID, "downloading", "")
		s.d.Downloader.Add(s.jobOf(m))
	}
	s.hub.notify("models")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) stopEngine(w http.ResponseWriter, r *http.Request) {
	if err := s.d.Engine.Stop(); err != nil {
		s.fail(w, err)
		return
	}
	s.hub.notify("models")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) restartEngine(w http.ResponseWriter, r *http.Request) {
	if s.d.Engine.ActiveID() == "" {
		writeError(w, 409, "No model is active: activate one first.")
		return
	}
	s.d.Engine.Restart()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) convertModel(w http.ResponseWriter, r *http.Request) {
	if s.d.Converter == nil {
		writeError(w, 501, "Converting is not available in this build.")
		return
	}
	var req struct {
		Repo  string `json:"repo"`
		Quant string `json:"quant"`
		Name  string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	repoID := hf.ParseID(req.Repo)
	if !hf.ValidID(repoID) {
		writeError(w, 400, "Enter a Hugging Face model as owner/name.")
		return
	}
	quant := strings.ToUpper(strings.TrimSpace(req.Quant))
	if !convert.ValidQuant(quant) {
		writeError(w, 400, "Choose one of: "+strings.Join(convert.Quants, ", ")+".")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	repo, err := s.d.HF.Repo(ctx, repoID)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if !repo.Convertible {
		writeError(w, 400, "This repository has no safetensors weights that llama.cpp could convert.")
		return
	}
	snap := s.d.Engine.System()
	need := repo.Safetensors*2 + repo.Safetensors/3
	if snap.Disk.Free > 0 && need > snap.Disk.Free {
		writeError(w, 400, fmt.Sprintf("Converting needs about %.0f GB of free disk space for a while (the original weights, a 16-bit copy and the result); %.0f GB are free.",
			float64(need)/(1<<30), float64(snap.Disk.Free)/(1<<30)))
		return
	}
	id := s.newModelID(repoID, quant)
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = path.Base(repoID) + " · " + quant
	}
	m := store.Model{ID: id, Name: name, Repo: repoID, Revision: "main", Quant: quant, State: "converting", Size: repo.Safetensors}
	if err := s.d.Store.PutModel(m); err != nil {
		s.fail(w, err)
		return
	}
	s.d.Converter.Start(m, repo)
	s.hub.notify("models")
	writeJSON(w, 200, s.view(m, s.d.Engine.ActiveID()))
}

// --- Hugging Face --------------------------------------------------------------

func (s *Server) hfSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, 200, map[string]any{"models": []any{}})
		return
	}
	if id := hf.ParseID(q); id != "" && strings.Contains(q, "/") && hf.ValidID(id) {
		q = id
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	res, err := s.d.HF.Search(ctx, q)
	if err != nil {
		writeError(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"models": res})
}

func (s *Server) hfRepo(w http.ResponseWriter, r *http.Request) {
	id := hf.ParseID(r.URL.Query().Get("id"))
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	repo, err := s.d.HF.Repo(ctx, id)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	snap := s.d.Engine.System()
	var vram int64
	for _, g := range snap.GPUs {
		vram += g.Total
	}
	var mm int64
	if len(repo.MMProj) > 0 {
		mm = repo.MMProj[0].Size
	}
	ram := snap.RAM.Available + snap.RAM.EngineAnon
	hf.Assess(repo.Variants, hf.Machine{VRAM: vram, RAM: ram, DiskFree: snap.Disk.Free}, mm, repo.MoE)
	if repo.Variants == nil {
		repo.Variants = []hf.Variant{}
	}
	if repo.Quantized == nil {
		repo.Quantized = []hf.Related{}
	}
	writeJSON(w, 200, repo)
}

// --- chat --------------------------------------------------------------------

// chat is the chat of the interface: an OpenAI chat request that streams,
// with the prompt progress and timings turned on.
func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if !decode(w, r, &body) {
		return
	}
	if body == nil {
		body = map[string]any{}
	}
	body["stream"] = true
	body["return_progress"] = true
	body["timings_per_token"] = true
	body["stream_options"] = map[string]any{"include_usage": true}
	if _, ok := body["model"]; !ok {
		body["model"] = s.d.Engine.ActiveID()
	}
	b, _ := json.Marshal(body)
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/v1/chat/completions"
	r2.URL.RawQuery = ""
	r2.Header.Set("Content-Type", "application/json")
	s.d.Proxy.Forward(w, r2, b, "chat", store.Key{})
}

// --- API keys ----------------------------------------------------------------

const keyAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func newSecret() string {
	var b strings.Builder
	b.WriteString("sk-soc-")
	for i := 0; i < 40; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(keyAlphabet))))
		if err != nil {
			panic(err)
		}
		b.WriteByte(keyAlphabet[n.Int64()])
	}
	return b.String()
}

func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/v1"
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.d.Store.Keys()
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"keys": keys, "baseUrl": baseURL(r), "model": s.d.Engine.ActiveID(), "requireKey": s.requireKey()})
}

func (s *Server) addKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > 60 {
		writeJSON(w, 400, apiError{Error: "Give the key a name, for example the app that uses it.", Fields: map[string]string{"name": "Give the key a name (up to 60 characters)."}})
		return
	}
	secret := newSecret()
	k, err := s.d.Store.AddKey(name, secret[:11], proxy.HashKey(secret))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.hub.notify("keys")
	writeJSON(w, 200, map[string]any{"key": k, "secret": secret})
}

func (s *Server) deleteKey(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "Unknown key.")
		return
	}
	if err := s.d.Store.DeleteKey(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, 404, "This key does not exist (any more).")
			return
		}
		s.fail(w, err)
		return
	}
	s.hub.notify("keys")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) listRequests(w http.ResponseWriter, r *http.Request) {
	rs, err := s.d.Store.Requests(100)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"requests": rs})
}
