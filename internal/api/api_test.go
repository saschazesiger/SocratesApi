package api

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/saschazesiger/Z-Dash/server"
	"github.com/saschazesiger/Z-Dash/server/cloudflare"
	zsecret "github.com/saschazesiger/Z-Dash/server/secret"

	"github.com/saschazesiger/SocratesApi/internal/engine"
	"github.com/saschazesiger/SocratesApi/internal/hf"
	"github.com/saschazesiger/SocratesApi/internal/proxy"
	"github.com/saschazesiger/SocratesApi/internal/store"
	"github.com/saschazesiger/SocratesApi/internal/sysinfo"
	"github.com/saschazesiger/SocratesApi/web"
)

// rig is Socrates API on the Z-Dash server as main builds it, with a demo
// tunnel, on one database.
type rig struct {
	t      *testing.T
	st     *store.Store
	app    *server.App
	srv    *Server
	h      http.Handler
	cookie *http.Cookie
}

func newRig(t *testing.T, st *store.Store) *rig {
	t.Helper()
	dir := t.TempDir()
	box, err := zsecret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	var built atomic.Pointer[Server]
	app, err := server.New(server.Config{
		Name: "Socrates API", Slug: "socrates", Version: "test", DataDir: dir,
		Assets: web.FS, KV: store.ServerKV{St: st}, Sealer: box, Tunnel: cloudflare.NewDemo("Socrates API", nil), Logf: t.Logf,
		OnSignOut: func() {
			if s := built.Load(); s != nil {
				s.SignedOut()
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	eng := engine.New(engine.Config{ModelsDir: dir}, st, &sysinfo.Reader{DataDir: dir})
	hfc := hf.NewClient(func() string { return "" })
	srv := New(Deps{App: app, Store: st, Engine: eng, Proxy: proxy.New(eng, st), HF: hfc, Downloader: hf.NewDownloader(hfc),
		Version: "test", ModelsDir: dir, Logf: t.Logf})
	built.Store(srv)
	return &rig{t: t, st: st, app: app, srv: srv, h: app.Handler()}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

type resp struct {
	code   int
	body   map[string]any
	raw    string
	header http.Header
}

// do sends a request as the interface does, from a browser on the local
// network; mods change it.
func (r *rig) do(method, path string, body any, mods ...func(*http.Request)) resp {
	r.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req := httptest.NewRequest(method, path, rd)
	req.Host = "192.168.1.20:8080"
	req.RemoteAddr = "192.168.1.5:5555"
	if method != http.MethodGet {
		req.Header.Set(server.CSRFHeader, "1")
	}
	if r.cookie != nil {
		req.AddCookie(r.cookie)
	}
	for _, m := range mods {
		m(req)
	}
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	out := resp{code: rec.Code, raw: rec.Body.String(), header: rec.Header()}
	_ = json.Unmarshal(rec.Body.Bytes(), &out.body)
	for _, c := range rec.Result().Cookies() {
		if c.Name == "socrates_session" {
			if c.MaxAge < 0 {
				r.cookie = nil
			} else {
				r.cookie = c
			}
		}
	}
	return out
}

func (r *rig) expect(got resp, code int) resp {
	r.t.Helper()
	if got.code != code {
		r.t.Fatalf("want %d, got %d: %s", code, got.code, got.raw)
	}
	return got
}

func host(name string) func(*http.Request) { return func(req *http.Request) { req.Host = name } }

// TestEarlierPasswordAndSessionsKeepWorking writes the password and a
// session as Socrates API stored them before it ran on the Z-Dash server
// (its own auth.go: an Argon2id hash as JSON under auth.password, the
// SHA-256 of each session token under auth.sessions): both still sign in.
func TestEarlierPasswordAndSessionsKeepWorking(t *testing.T) {
	st := openStore(t)
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	type passwordHash struct {
		Salt    []byte `json:"salt"`
		Hash    []byte `json:"hash"`
		Time    uint32 `json:"time"`
		Memory  uint32 `json:"memory"`
		Threads uint8  `json:"threads"`
	}
	pw := "an earlier admin password"
	if err := st.SetJSON("auth.password", passwordHash{Salt: salt, Time: 2, Memory: 19 * 1024, Threads: 1,
		Hash: argon2.IDKey([]byte(pw), salt, 2, 19*1024, 1, 32)}); err != nil {
		t.Fatal(err)
	}
	tok := make([]byte, 32)
	_, _ = rand.Read(tok)
	token := base64.RawURLEncoding.EncodeToString(tok)
	sum := sha256.Sum256([]byte(token))
	type session struct {
		Expires int64 `json:"expires"`
	}
	if err := st.SetJSON("auth.sessions", map[string]session{
		hex.EncodeToString(sum[:]): {Expires: time.Now().Add(24 * time.Hour).UnixMilli()},
	}); err != nil {
		t.Fatal(err)
	}

	r := newRig(t, st)
	// The browser that was signed in stays signed in.
	r.cookie = &http.Cookie{Name: "socrates_session", Value: token}
	s := r.expect(r.do("GET", "/api/session", nil), 200)
	if s.body["signedIn"] != true || s.body["needsPassword"] != false || s.body["env"] != "SOCRATES" {
		t.Fatalf("session: %v", s.body)
	}
	r.expect(r.do("GET", "/api/settings", nil), 200)

	// The password signs in a new browser.
	r.cookie = nil
	r.expect(r.do("GET", "/api/settings", nil), 401)
	r.expect(r.do("POST", "/api/login", map[string]string{"password": "not the password"}), 400)
	r.expect(r.do("POST", "/api/login", map[string]string{"password": pw}), 200)
	if r.cookie == nil {
		t.Fatal("no socrates_session cookie after the sign-in")
	}
	r.expect(r.do("GET", "/api/settings", nil), 200)

	// A restart (a new server on the same database) keeps both sessions.
	r2 := newRig(t, st)
	r2.cookie = r.cookie
	r2.expect(r2.do("GET", "/api/settings", nil), 200)
	r2.cookie = &http.Cookie{Name: "socrates_session", Value: token}
	r2.expect(r2.do("GET", "/api/settings", nil), 200)
}

func TestFirstPasswordAndGuards(t *testing.T) {
	r := newRig(t, openStore(t))
	s := r.expect(r.do("GET", "/api/session", nil), 200)
	if s.body["needsPassword"] != true || s.body["signedIn"] != false || s.body["name"] != "Socrates API" {
		t.Fatalf("session: %v", s.body)
	}
	r.expect(r.do("GET", "/api/models", nil), 401)
	r.expect(r.do("GET", "/api/stream", nil), 401)
	r.expect(r.do("POST", "/api/setup/password", map[string]string{"password": "correct horse battery"}), 200)
	r.expect(r.do("GET", "/api/models", nil), 200)

	// A change without the interface's header, or from another site, is refused.
	r.expect(r.do("PUT", "/api/settings", map[string]any{"requireKey": false}, func(req *http.Request) { req.Header.Del(server.CSRFHeader) }), 403)
	r.expect(r.do("PUT", "/api/settings", map[string]any{"requireKey": false}, func(req *http.Request) { req.Header.Set("Origin", "https://evil.example") }), 403)
	got := r.expect(r.do("PUT", "/api/settings", map[string]any{"requireKey": false}), 200)
	if got.body["requireKey"] != false {
		t.Fatalf("settings: %v", got.body)
	}
	// The interface answers only to local names (DNS rebinding).
	r.expect(r.do("GET", "/api/models", nil, host("socrates.example.com")), 403)
	r.expect(r.do("GET", "/", nil, host("socrates.example.com")), 403)
	// The page and Z-Dash are served, with the server's policy.
	page := r.expect(r.do("GET", "/", nil), 200)
	if !strings.Contains(page.raw, `src="/z-dash/ds.min.js"`) {
		t.Fatalf("index.html: %.200s", page.raw)
	}
	if !strings.Contains(page.header.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatalf("CSP: %q", page.header.Get("Content-Security-Policy"))
	}
	r.expect(r.do("GET", "/z-dash/ds.min.js", nil), 200)
	r.expect(r.do("GET", "/z-dash/server/shell.js", nil), 200)
	r.expect(r.do("GET", "/healthz", nil), 200)
}

// TestModelAPIIsUnguarded: API clients call /v1/ and /health by any name,
// without a session and without the CSRF header; the proxy's own keys
// decide.
func TestModelAPIIsUnguarded(t *testing.T) {
	r := newRig(t, openStore(t))
	public := host("socrates.example.com")
	noCSRF := func(req *http.Request) { req.Header.Del(server.CSRFHeader) }

	got := r.expect(r.do("GET", "/v1/models", nil, public), 401)
	if !strings.Contains(got.raw, "authentication_error") {
		t.Fatalf("not the proxy's answer: %s", got.raw)
	}
	secret := "sk-soc-test0123456789"
	if _, err := r.st.AddKey("test", "sk-soc-test", proxy.HashKey(secret)); err != nil {
		t.Fatal(err)
	}
	withKey := func(req *http.Request) { req.Header.Set("Authorization", "Bearer "+secret) }
	got = r.expect(r.do("GET", "/v1/models", nil, public, withKey), 200)
	if got.body["object"] != "list" || got.header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("models: %s %v", got.raw, got.header)
	}
	// A browser's preflight, and a POST without the interface's header.
	pre := r.expect(r.do("OPTIONS", "/v1/chat/completions", nil, public, noCSRF, func(req *http.Request) {
		req.Header.Set("Origin", "https://app.example")
		req.Header.Set("Access-Control-Request-Method", "POST")
	}), 204)
	if pre.header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("preflight: %v", pre.header)
	}
	got = r.do("POST", "/v1/chat/completions", map[string]any{"messages": []any{}}, public, noCSRF, withKey,
		func(req *http.Request) { req.Header.Set("Origin", "https://app.example") })
	if got.code == http.StatusForbidden || got.code == http.StatusUnauthorized || !strings.Contains(got.raw, `"type"`) {
		t.Fatalf("a keyless POST of the API was refused by the server's guards: %d %s", got.code, got.raw)
	}

	// The health check for monitoring: no engine runs here.
	h := r.expect(r.do("GET", "/health", nil, public), 503)
	if _, ok := h.body["status"]; !ok {
		t.Fatalf("health: %s", h.raw)
	}
}

// TestLiveUpdates: a changed list reaches the server's change notices, the
// status its own stream.
func TestLiveUpdates(t *testing.T) {
	r := newRig(t, openStore(t))
	r.expect(r.do("POST", "/api/setup/password", map[string]string{"password": "correct horse battery"}), 200)
	ts := httptest.NewServer(r.h)
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	open := func(path string) *bufio.Reader {
		req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+path, nil)
		req.AddCookie(r.cookie)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { res.Body.Close() })
		if res.StatusCode != 200 {
			t.Fatalf("%s: %d", path, res.StatusCode)
		}
		return bufio.NewReader(res.Body)
	}
	until := func(rd *bufio.Reader, want string) {
		t.Helper()
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				t.Fatalf("waiting for %q: %v", want, err)
			}
			if strings.Contains(line, want) {
				return
			}
		}
	}
	stream := open("/api/stream")
	until(stream, "event: status")
	until(stream, `"engine"`)

	events := open("/api/events")
	r.srv.notify("settings")
	until(events, `"settings"`)
}

// TestStreamEndsOnSignOut: the status stream of a browser ends when its
// session ends, not only at the next reconnect.
func TestStreamEndsOnSignOut(t *testing.T) {
	r := newRig(t, openStore(t))
	r.expect(r.do("POST", "/api/setup/password", map[string]string{"password": "correct horse battery"}), 200)
	ts := httptest.NewServer(r.h)
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/stream", nil)
	req.AddCookie(r.cookie)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	rd := bufio.NewReader(res.Body)
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(line, "event: status") {
			break
		}
	}
	r.expect(r.do("POST", "/api/logout", nil), 200)
	if _, err := io.Copy(io.Discard, rd); err != nil {
		t.Fatalf("the stream did not end after the sign-out: %v", err)
	}
}
