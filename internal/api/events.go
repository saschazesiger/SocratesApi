package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

// coalesce is how long a stream waits after the first change before it
// writes, so a burst of changes is one message (at most 4 per second).
const coalesce = 250 * time.Millisecond

// hub fans changes out to the open event streams: topics to refetch, the
// live status, and new engine log lines.
type hub struct {
	mu      sync.Mutex
	clients map[*streamClient]struct{}
	closed  bool
}

type streamClient struct {
	wake   chan struct{}
	mu     sync.Mutex
	topics map[string]bool
	status bool
	logs   []string
	done   chan struct{}
}

func newHub() *hub { return &hub{clients: map[*streamClient]struct{}{}} }

func (h *hub) each(fn func(c *streamClient)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		c.mu.Lock()
		fn(c)
		c.mu.Unlock()
		select {
		case c.wake <- struct{}{}:
		default:
		}
	}
}

func (h *hub) notify(topics ...string) {
	h.each(func(c *streamClient) {
		for _, t := range topics {
			c.topics[t] = true
		}
	})
}

func (h *hub) statusChanged() { h.each(func(c *streamClient) { c.status = true }) }

func (h *hub) log(lines []string) {
	h.each(func(c *streamClient) {
		c.logs = append(c.logs, lines...)
		if len(c.logs) > 2000 {
			c.logs = c.logs[len(c.logs)-2000:]
		}
	})
}

func (h *hub) add() *streamClient {
	c := &streamClient{wake: make(chan struct{}, 1), topics: map[string]bool{}, status: true, done: make(chan struct{})}
	h.mu.Lock()
	if h.closed {
		close(c.done)
	} else {
		h.clients[c] = struct{}{}
	}
	h.mu.Unlock()
	return c
}

func (h *hub) remove(c *streamClient) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

func (h *hub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for c := range h.clients {
		close(c.done)
		delete(h.clients, c)
	}
}

func (c *streamClient) take() (topics []string, status bool, logs []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for t := range c.topics {
		topics = append(topics, t)
	}
	sort.Strings(topics)
	c.topics = map[string]bool{}
	status, c.status = c.status, false
	logs, c.logs = c.logs, nil
	return
}

// events is the server-sent event stream of the interface.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "Live updates are not supported by this connection.")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "retry: 3000\n\n")
	flusher.Flush()

	c := s.hub.add()
	c.wake <- struct{}{}
	defer s.hub.remove(c)
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	ctx := r.Context()
	write := func(event string, v any) bool {
		b, _ := json.Marshal(v)
		_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		return err == nil
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-c.wake:
			select {
			case <-time.After(coalesce):
			case <-ctx.Done():
				return
			case <-c.done:
				return
			}
			topics, status, logs := c.take()
			if status && !write("status", s.status()) {
				return
			}
			if len(topics) > 0 && !write("change", map[string]any{"topics": topics}) {
				return
			}
			if len(logs) > 0 && !write("log", map[string]any{"lines": logs}) {
				return
			}
			flusher.Flush()
		}
	}
}
