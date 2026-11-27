package api

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// recheck is how often the long-lived requests are checked again without a
// sign-out: an address taken off the tunnel's list ends no session, so
// nothing tells the server.
const recheck = 30 * time.Second

// watcher ends the long-lived requests (the status stream, a chat) of
// people who are no longer signed in: a sign-out, a new password that signs
// the other browsers out, an address taken off the tunnel's list.
type watcher struct {
	still func(r *http.Request) bool
	mu    sync.Mutex
	reqs  map[*http.Request]context.CancelFunc
}

func newWatcher(still func(r *http.Request) bool) *watcher {
	return &watcher{still: still, reqs: map[*http.Request]context.CancelFunc{}}
}

// watch returns r with a context that ends when its person is signed out;
// done must be called when the request ends.
func (w *watcher) watch(r *http.Request) (*http.Request, func()) {
	ctx, cancel := context.WithCancel(r.Context())
	r2 := r.WithContext(ctx)
	w.mu.Lock()
	w.reqs[r] = cancel
	w.mu.Unlock()
	return r2, func() {
		w.mu.Lock()
		delete(w.reqs, r)
		w.mu.Unlock()
		cancel()
	}
}

// check ends every watched request whose person is no longer signed in.
func (w *watcher) check() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for r, cancel := range w.reqs {
		if !w.still(r) {
			cancel()
			delete(w.reqs, r)
		}
	}
}

// SignedOut is the server's OnSignOut: sessions ended, so the streams of
// those people end too.
func (s *Server) SignedOut() { s.watch.check() }
