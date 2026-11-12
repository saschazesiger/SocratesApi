package api

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"

	"github.com/saschazesiger/SocratesApi/internal/store"
)

// The admin password and the browser sessions. Adapted from CI-Plane (MIT).

const (
	cookieName     = "socrates_session"
	sessionMaxAge  = 30 * 24 * time.Hour
	minPasswordLen = 10
	maxPasswordLen = 256
)

type passwordHash struct {
	Salt    []byte `json:"salt"`
	Hash    []byte `json:"hash"`
	Time    uint32 `json:"time"`
	Memory  uint32 `json:"memory"`
	Threads uint8  `json:"threads"`
}

const (
	argonTime    = 2
	argonMemory  = 19 * 1024
	argonThreads = 1
	argonKeyLen  = 32
)

func hashPassword(pw string) passwordHash {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		panic(err)
	}
	return passwordHash{Salt: salt, Time: argonTime, Memory: argonMemory, Threads: argonThreads,
		Hash: argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)}
}

func (p passwordHash) matches(pw string) bool {
	if len(p.Hash) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(pw), p.Salt, p.Time, p.Memory, p.Threads, uint32(len(p.Hash)))
	return subtle.ConstantTimeCompare(got, p.Hash) == 1
}

func checkNewPassword(pw string) error {
	switch n := utf8.RuneCountInString(pw); {
	case n < minPasswordLen:
		return fmt.Errorf("Use at least %d characters.", minPasswordLen)
	case len(pw) > maxPasswordLen:
		return fmt.Errorf("Use at most %d characters.", maxPasswordLen)
	}
	return nil
}

type session struct {
	Expires int64 `json:"expires"`
}

type auth struct {
	st   *store.Store
	logf func(string, ...any)

	mu       sync.Mutex
	sessions map[string]session // by token hash
	attempts map[string]*attempt
}

type attempt struct {
	fails int
	until time.Time
	last  time.Time
}

func newAuth(st *store.Store, logf func(string, ...any)) *auth {
	a := &auth{st: st, logf: logf, sessions: map[string]session{}, attempts: map[string]*attempt{}}
	if err := st.GetJSON("auth.sessions", &a.sessions); err != nil && !errors.Is(err, store.ErrNotFound) {
		logf("sessions: %v", err)
	}
	if a.sessions == nil {
		a.sessions = map[string]session{}
	}
	return a
}

func (a *auth) hasPassword() bool {
	var h passwordHash
	return a.st.GetJSON("auth.password", &h) == nil
}

func (a *auth) setPassword(pw string) error {
	if err := checkNewPassword(pw); err != nil {
		return err
	}
	return a.st.SetJSON("auth.password", hashPassword(pw))
}

// setFirstPassword sets the password only if none is set, so two first
// visitors cannot both win.
func (a *auth) setFirstPassword(pw string) (bool, error) {
	if err := checkNewPassword(pw); err != nil {
		return false, err
	}
	return a.st.SetJSONIfMissing("auth.password", hashPassword(pw))
}

var hashing = make(chan struct{}, 4)

func (a *auth) verify(pw string) bool {
	var h passwordHash
	if err := a.st.GetJSON("auth.password", &h); err != nil {
		hashPassword(pw)
		return false
	}
	hashing <- struct{}{}
	defer func() { <-hashing }()
	return h.matches(pw)
}

// limited reports how long a client still has to wait: five failures are
// free, then the wait doubles from 30 seconds up to 15 minutes.
func (a *auth) limited(ip string) time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	if at := a.attempts[ip]; at != nil {
		if d := at.until.Sub(now); d > 0 {
			return d
		}
	}
	at := a.attempts[ip]
	if at == nil || now.Sub(at.last) > time.Hour {
		at = &attempt{}
		a.attempts[ip] = at
	}
	at.fails++
	at.last = now
	if at.fails >= 5 {
		d := 30 * time.Second << (at.fails - 5)
		if d > 15*time.Minute || d <= 0 {
			d = 15 * time.Minute
		}
		at.until = now.Add(d)
	}
	return 0
}

func (a *auth) succeeded(ip string) {
	a.mu.Lock()
	delete(a.attempts, ip)
	a.mu.Unlock()
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (a *auth) startSession(w http.ResponseWriter, r *http.Request) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	exp := time.Now().Add(sessionMaxAge)
	a.mu.Lock()
	a.sessions[tokenHash(token)] = session{Expires: exp.UnixMilli()}
	a.saveLocked()
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: token, Path: "/", Expires: exp, MaxAge: int(sessionMaxAge / time.Second),
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil})
}

func (a *auth) endSession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		a.mu.Lock()
		delete(a.sessions, tokenHash(c.Value))
		a.saveLocked()
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

func (a *auth) dropSessions(keepToken string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	keep := ""
	if keepToken != "" {
		keep = tokenHash(keepToken)
	}
	for k := range a.sessions {
		if k != keep {
			delete(a.sessions, k)
		}
	}
	a.saveLocked()
}

func (a *auth) check(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[tokenHash(c.Value)]
	if !ok {
		return false
	}
	if s.Expires < time.Now().UnixMilli() {
		delete(a.sessions, tokenHash(c.Value))
		a.saveLocked()
		return false
	}
	return true
}

func (a *auth) saveLocked() {
	now := time.Now().UnixMilli()
	for k, s := range a.sessions {
		if s.Expires < now {
			delete(a.sessions, k)
		}
	}
	if err := a.st.SetJSON("auth.sessions", a.sessions); err != nil {
		a.logf("save sessions: %v", err)
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func roundWait(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d seconds", int(d.Seconds()+0.99))
	}
	if m := int(d.Minutes() + 0.99); m > 1 {
		return fmt.Sprintf("%d minutes", m)
	}
	return "a minute"
}

// --- handlers ----------------------------------------------------------------

type passwordReq struct {
	Password string `json:"password"`
	Current  string `json:"current"`
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"signedIn":      s.auth.check(r),
		"needsPassword": !s.auth.hasPassword(),
		"version":       s.d.Version,
	})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if d := s.auth.limited(ip); d > 0 {
		writeError(w, http.StatusTooManyRequests, fmt.Sprintf("Too many wrong passwords. Wait %s and try again.", roundWait(d)))
		return
	}
	var req passwordReq
	if !decode(w, r, &req) {
		return
	}
	if !s.auth.hasPassword() {
		writeError(w, http.StatusConflict, "No password is set yet. Reload the page to set one.")
		return
	}
	if !s.auth.verify(req.Password) {
		s.d.Logf("login: wrong password from %s", ip)
		writeError(w, http.StatusUnauthorized, "The password is wrong.")
		return
	}
	s.auth.succeeded(ip)
	s.auth.startSession(w, r)
	writeJSON(w, http.StatusOK, map[string]bool{"signedIn": true})
}

func (s *Server) firstPassword(w http.ResponseWriter, r *http.Request) {
	var req passwordReq
	if !decode(w, r, &req) {
		return
	}
	won, err := s.auth.setFirstPassword(req.Password)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: err.Error(), Fields: map[string]string{"password": err.Error()}})
		return
	}
	if !won {
		writeError(w, http.StatusConflict, "A password was set already. Sign in with it.")
		return
	}
	s.d.Logf("The admin password was set by the first visitor (%s)", clientIP(r))
	s.auth.startSession(w, r)
	writeJSON(w, http.StatusOK, map[string]bool{"signedIn": true})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.auth.endSession(w, r)
	writeJSON(w, http.StatusOK, map[string]bool{"signedIn": false})
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	var req passwordReq
	if !decode(w, r, &req) {
		return
	}
	if err := checkNewPassword(req.Password); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: err.Error(), Fields: map[string]string{"password": err.Error()}})
		return
	}
	ip := clientIP(r)
	if d := s.auth.limited(ip); d > 0 {
		writeError(w, http.StatusTooManyRequests, fmt.Sprintf("Too many wrong passwords. Wait %s and try again.", roundWait(d)))
		return
	}
	if !s.auth.verify(req.Current) {
		msg := "The current password is wrong."
		writeJSON(w, http.StatusBadRequest, apiError{Error: msg, Fields: map[string]string{"current": msg}})
		return
	}
	s.auth.succeeded(ip)
	if err := s.auth.setPassword(req.Password); err != nil {
		s.fail(w, err)
		return
	}
	keep := ""
	if c, err := r.Cookie(cookieName); err == nil {
		keep = c.Value
	}
	s.auth.dropSessions(keep)
	writeJSON(w, http.StatusOK, map[string]string{"message": "Password changed. Other browsers were signed out."})
}
