// Package api serves the web interface, its JSON API under /api/ and the
// public model API under /v1/ on one port.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/saschazesiger/SocratesApi/internal/convert"
	"github.com/saschazesiger/SocratesApi/internal/engine"
	"github.com/saschazesiger/SocratesApi/internal/hf"
	"github.com/saschazesiger/SocratesApi/internal/proxy"
	"github.com/saschazesiger/SocratesApi/internal/store"
	"github.com/saschazesiger/SocratesApi/web"
)

// Deps is what the server is built from.
type Deps struct {
	Store      *store.Store
	Engine     *engine.Engine
	Proxy      *proxy.Proxy
	HF         *hf.Client
	Downloader *hf.Downloader
	Converter  *convert.Converter
	Version    string
	ModelsDir  string
	// ResetPassword, if set, becomes the admin password at start.
	ResetPassword string
	// Hosts are extra host names the interface answers to.
	Hosts  []string
	Assets fs.FS
	Logf   func(format string, args ...any)
}

// Server serves everything.
type Server struct {
	d      Deps
	auth   *auth
	hub    *hub
	assets *assets
}

// New builds the server and hooks it to the live sources.
func New(d Deps) *Server {
	if d.Logf == nil {
		d.Logf = func(string, ...any) {}
	}
	if d.Assets == nil {
		d.Assets = web.FS
	}
	s := &Server{d: d, hub: newHub()}
	s.auth = newAuth(d.Store, d.Logf)
	if d.ResetPassword != "" {
		if err := s.auth.setPassword(d.ResetPassword); err != nil {
			d.Logf("SOCRATES_ADMIN_PASSWORD: %v", err)
		} else {
			s.auth.dropSessions("")
			d.Logf("The admin password was set from SOCRATES_ADMIN_PASSWORD; everybody was signed out")
		}
	}
	a, err := loadAssets(d.Assets)
	if err != nil {
		d.Logf("web interface: %v", err)
	}
	s.assets = a
	d.Engine.OnChange = s.hub.statusChanged
	d.Engine.OnLog = s.hub.log
	d.Proxy.Changed = s.hub.statusChanged
	d.Proxy.RequestLogged = func() { s.hub.notify("requests", "keys") }
	d.Proxy.RequireKey = s.requireKey
	d.Downloader.Changed = func() { s.hub.statusChanged(); s.hub.notify("models") }
	d.Downloader.Done = s.downloadDone
	if d.Converter != nil {
		d.Converter.Changed = func() { s.hub.statusChanged(); s.hub.notify("models") }
		d.Converter.Done = s.convertDone
		d.Converter.Log = s.hub.log
	}
	return s
}

// Run does the periodic work: a status push every 2 s, so meters move.
func (s *Server) Run(ctx context.Context) {
	s.resume()
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.hub.close()
			return
		case <-t.C:
			s.hub.statusChanged()
		}
	}
}

// Handler is the one handler of the port.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	ui := http.NewServeMux()
	s.routes(ui)
	uiH := s.localHost(s.headers(ui))

	mux.Handle("/v1/", cors(s.d.Proxy))
	mux.HandleFunc("GET /health", s.publicHealth)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	mux.Handle("/", uiH)
	return mux
}

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

func (s *Server) routes(mux *http.ServeMux) {
	mux.Handle("/", s.assets)
	mux.HandleFunc("GET /api/session", s.session)
	mux.HandleFunc("POST /api/login", s.guard(s.login))
	mux.HandleFunc("POST /api/setup", s.guard(s.firstPassword))
	mux.HandleFunc("POST /api/logout", s.guard(s.logout))
	mux.HandleFunc("POST /api/password", s.private(s.changePassword))
	mux.HandleFunc("GET /api/events", s.private(s.events))
	mux.HandleFunc("GET /api/status", s.private(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, s.status()) }))

	mux.HandleFunc("GET /api/models", s.private(s.listModels))
	mux.HandleFunc("POST /api/models", s.private(s.addModel))
	mux.HandleFunc("POST /api/models/convert", s.private(s.convertModel))
	mux.HandleFunc("PATCH /api/models/{id}", s.private(s.editModel))
	mux.HandleFunc("DELETE /api/models/{id}", s.private(s.deleteModel))
	mux.HandleFunc("POST /api/models/{id}/activate", s.private(s.activateModel))
	mux.HandleFunc("POST /api/models/{id}/plan", s.private(s.planModel))
	mux.HandleFunc("POST /api/models/{id}/retry", s.private(s.retryModel))
	mux.HandleFunc("POST /api/engine/stop", s.private(s.stopEngine))
	mux.HandleFunc("POST /api/engine/restart", s.private(s.restartEngine))
	mux.HandleFunc("GET /api/logs", s.private(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"lines": s.d.Engine.Logs()})
	}))

	mux.HandleFunc("GET /api/hf/search", s.private(s.hfSearch))
	mux.HandleFunc("GET /api/hf/repo", s.private(s.hfRepo))

	mux.HandleFunc("POST /api/chat", s.private(s.chat))

	mux.HandleFunc("GET /api/keys", s.private(s.listKeys))
	mux.HandleFunc("POST /api/keys", s.private(s.addKey))
	mux.HandleFunc("DELETE /api/keys/{id}", s.private(s.deleteKey))
	mux.HandleFunc("GET /api/requests", s.private(s.listRequests))
	mux.HandleFunc("GET /api/settings", s.private(s.getSettings))
	mux.HandleFunc("PUT /api/settings", s.private(s.putSettings))

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "This API address does not exist.")
	})
}

// localHost refuses host names the person did not name, so a web page on
// the internet cannot reach the interface through DNS rebinding (and set
// the first password). IP addresses and local names always work.
func (s *Server) localHost(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !localName(r.Host, s.d.Hosts) {
			http.Error(w, "Socrates API does not know the name "+strconv.Quote(r.Host)+
				". Open it by its IP address, or add the name to the environment variable SOCRATES_HOSTS.", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func localName(hostport string, extra []string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if host == "" || net.ParseIP(host) != nil || !strings.Contains(host, ".") {
		return host != ""
	}
	for _, suffix := range []string{".localhost", ".local", ".lan", ".internal", ".home.arpa", ".home", ".intranet", ".ts.net"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	for _, e := range extra {
		if e = strings.TrimSpace(e); e == "*" || strings.EqualFold(e, host) {
			return true
		}
	}
	return false
}

const csp = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; font-src 'self'; " +
	"connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'; object-src 'none'"

func (s *Server) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// guard refuses changes that did not come from the interface (CSRF).
func (s *Server) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get("X-Socrates") != "1" {
				writeError(w, http.StatusForbidden, "This request was refused: it did not come from the Socrates API interface. Reload the page and try again.")
				return
			}
			if o := r.Header.Get("Origin"); o != "" {
				u, err := url.Parse(o)
				if err != nil || !strings.EqualFold(u.Host, r.Host) {
					writeError(w, http.StatusForbidden, "This request was refused: it came from another website.")
					return
				}
			}
		}
		h(w, r)
	}
}

func (s *Server) private(h http.HandlerFunc) http.HandlerFunc {
	return s.guard(func(w http.ResponseWriter, r *http.Request) {
		if !s.auth.check(r) {
			writeError(w, http.StatusUnauthorized, "You are not signed in. Reload the page and sign in.")
			return
		}
		h(w, r)
	})
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
