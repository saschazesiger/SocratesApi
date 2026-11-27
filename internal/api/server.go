// Package api is Socrates API's own part of the server: the interface's
// JSON API under /api/, its live status stream, and the public model API
// under /v1/, registered on the Z-Dash server (github.com/saschazesiger/
// Z-Dash/server). The server brings the rest: the listeners, the sign-in,
// the guards, the change notices, the Cloudflare tunnel, Z-Dash and the
// interface's files.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/saschazesiger/Z-Dash/server"

	"github.com/saschazesiger/SocratesApi/internal/convert"
	"github.com/saschazesiger/SocratesApi/internal/engine"
	"github.com/saschazesiger/SocratesApi/internal/hf"
	"github.com/saschazesiger/SocratesApi/internal/proxy"
	"github.com/saschazesiger/SocratesApi/internal/store"
)

// Deps is what the server is built from.
type Deps struct {
	// App is the Z-Dash server Socrates API runs on; New registers the
	// routes on it.
	App        *server.App
	Store      *store.Store
	Engine     *engine.Engine
	Proxy      *proxy.Proxy
	HF         *hf.Client
	Downloader *hf.Downloader
	Converter  *convert.Converter
	Version    string
	ModelsDir  string
	Logf       func(format string, args ...any)
}

// Server is Socrates API's API on the Z-Dash server.
type Server struct {
	d     Deps
	hub   *hub
	watch *watcher
}

// New registers the routes on d.App and hooks the server to the live
// sources.
func New(d Deps) *Server {
	if d.Logf == nil {
		d.Logf = func(string, ...any) {}
	}
	s := &Server{d: d, hub: newHub(), watch: newWatcher(d.App.StillSignedIn)}
	d.Engine.OnChange = s.hub.statusChanged
	d.Engine.OnLog = s.hub.log
	d.Proxy.Changed = s.hub.statusChanged
	d.Proxy.RequestLogged = func() { s.notify("requests", "keys") }
	d.Proxy.RequireKey = s.keyNeeded
	d.Downloader.Changed = func() { s.hub.statusChanged(); s.notify("models") }
	d.Downloader.Done = s.downloadDone
	if d.Converter != nil {
		d.Converter.Changed = func() { s.hub.statusChanged(); s.notify("models") }
		d.Converter.Done = s.convertDone
		d.Converter.Log = s.hub.log
	}
	s.routes()
	return s
}

// notify tells every open page that lists of these topics changed; the
// page refetches them (the server's change notices at /api/events).
func (s *Server) notify(topics ...string) { s.d.App.Notify(topics...) }

// Run does the periodic work: a status push every 2 s, so meters move,
// and a look at who is still signed in.
func (s *Server) Run(ctx context.Context) {
	s.resume()
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	again := time.NewTicker(recheck)
	defer again.Stop()
	for {
		select {
		case <-ctx.Done():
			s.hub.close()
			return
		case <-t.C:
			s.hub.statusChanged()
		case <-again.C:
			s.watch.check()
		}
	}
}

// ModelAPI is the model API at /v1, for server.Config.Bypass: Cloudflare
// Access lets it through without a login, the local listener answers it
// by any name, and nothing asks for a session or the CSRF header. The
// proxy's API keys protect it; through the tunnel a key is always needed
// (keyNeeded).
func ModelAPI(p *proxy.Proxy) http.Handler { return cors(p) }

// ModelAPIPath is the model API's bypass path.
const ModelAPIPath = "/v1"

// cors lets browser-based clients call the model API; it is protected by
// API keys, not by cookies, so any origin is fine.
func cors(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, x-api-key, anthropic-version, anthropic-beta")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// routes registers Socrates API's routes. The model API (/v1) is a bypass
// path of the server (ModelAPI, given to server.New). /health is
// unguarded: monitoring calls it by any host name, without a session;
// through the tunnel it needs the Access login. Every /api/ route is for
// signed-in people and passes the server's CSRF guard.
func (s *Server) routes() {
	app := s.d.App
	app.Unguarded("GET /health", s.publicHealth)

	// The live status and the engine log; the change notices of the lists
	// are the server's (/api/events).
	app.HandleFunc("GET /api/stream", s.events)
	app.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, s.status()) })

	app.HandleFunc("GET /api/models", s.listModels)
	app.HandleFunc("POST /api/models", s.addModel)
	app.HandleFunc("POST /api/models/convert", s.convertModel)
	app.HandleFunc("PATCH /api/models/{id}", s.editModel)
	app.HandleFunc("DELETE /api/models/{id}", s.deleteModel)
	app.HandleFunc("POST /api/models/{id}/activate", s.activateModel)
	app.HandleFunc("POST /api/models/{id}/plan", s.planModel)
	app.HandleFunc("POST /api/models/{id}/retry", s.retryModel)
	app.HandleFunc("POST /api/engine/stop", s.stopEngine)
	app.HandleFunc("POST /api/engine/restart", s.restartEngine)
	app.HandleFunc("GET /api/logs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"lines": s.d.Engine.Logs()})
	})

	app.HandleFunc("GET /api/hf/search", s.hfSearch)
	app.HandleFunc("GET /api/hf/repo", s.hfRepo)

	app.HandleFunc("POST /api/chat", s.chat)

	app.HandleFunc("GET /api/keys", s.listKeys)
	app.HandleFunc("POST /api/keys", s.addKey)
	app.HandleFunc("DELETE /api/keys/{id}", s.deleteKey)
	app.HandleFunc("GET /api/requests", s.listRequests)
	app.HandleFunc("GET /api/settings", s.getSettings)
	app.HandleFunc("PUT /api/settings", s.putSettings)
}

type apiError struct {
	Error  string            `json:"error"`
	Fields map[string]string `json:"fields,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, apiError{Error: msg})
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.d.Logf("error: %v", err)
	writeError(w, http.StatusInternalServerError, "Something went wrong: "+err.Error())
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<20))
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "The request is not valid JSON: "+err.Error())
		return false
	}
	return true
}

// publicHealth is for monitoring and load balancers: no secrets in it.
func (s *Server) publicHealth(w http.ResponseWriter, r *http.Request) {
	st := s.d.Engine.Status()
	code := http.StatusOK
	if st.State != engine.Ready {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{"status": st.State, "model": st.ModelID})
}
