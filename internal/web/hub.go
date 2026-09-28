package web

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

// hub fans "something changed" signals out to the browsers that are connected
// to an event stream. It carries no data: a signal just tells the page to
// fetch itself again, so what each viewer sees is always decided by the
// normal access checks.
//
// Streams are keyed by whose timesheet they watch: your own pages watch
// yours, and a shared week's viewers watch the timesheet of whoever shared it.
type hub struct {
	mu   sync.Mutex
	subs map[*subscriber]struct{}
	// closing is closed when the server shuts down, which ends every stream.
	closing   chan struct{}
	closeOnce sync.Once
}

type subscriber struct {
	owner int64         // whose timesheet this stream is about
	who   string        // who is watching, for the cap
	ch    chan struct{} // buffered(1): bursts of changes coalesce into one signal
}

// maxStreams is how many live-update connections one viewer may hold open at
// once. Each is a goroutine and a connection for as long as it lasts, so
// without a cap one token could open thousands. This is more tabs and devices
// than anybody uses; a page over the limit still works, it just stops updating
// by itself until another tab is closed.
const maxStreams = 16

// maxViewers is the same for one shared link, whose viewers are nobody in
// particular and so are counted together. It is enough for everyone a week
// is likely to be sent to, with several tabs each, and still a limit.
const maxViewers = 200

func newHub() *hub { return &hub{subs: map[*subscriber]struct{}{}, closing: make(chan struct{})} }

// close ends every open stream. Streams never finish by themselves, so a
// graceful shutdown would otherwise wait for them until it gave up.
func (h *hub) close() { h.closeOnce.Do(func() { close(h.closing) }) }

// subscribe registers a stream watching owner's timesheet on behalf of who,
// or returns nil if who already has limit open.
func (h *hub) subscribe(owner int64, who string, limit int) *subscriber {
	h.mu.Lock()
	defer h.mu.Unlock()
	open := 0
	for s := range h.subs {
		if s.who == who {
			open++
		}
	}
	if open >= limit {
		return nil
	}
	s := &subscriber{owner: owner, who: who, ch: make(chan struct{}, 1)}
	h.subs[s] = struct{}{}
	return s
}

func (h *hub) unsubscribe(s *subscriber) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
}

// publish signals everyone watching owner's timesheet. It never blocks.
func (h *hub) publish(owner int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		if s.owner == owner {
			select {
			case s.ch <- struct{}{}:
			default: // a signal is already pending
			}
		}
	}
}

// stream sends change signals for owner's timesheet to one browser as
// server-sent events, at most limit at once for who. When still is set, it
// is asked on every signal whether the browser may go on watching; once it
// says no, that signal is the last, so the page fetches itself, finds out
// why, and the stream ends.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, owner int64, who string, limit int, still func() bool) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	sub := s.hub.subscribe(owner, who, limit)
	if sub == nil {
		// A browser's EventSource gives up for good on an error status rather
		// than retrying, so this does not turn into a reconnect storm.
		http.Error(w, "too many open live-update connections", http.StatusTooManyRequests)
		return
	}
	defer s.hub.unsubscribe(sub)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // don't let a reverse proxy buffer the stream

	fmt.Fprint(w, "retry: 3000\n\n")
	fl.Flush()

	ping := time.NewTicker(25 * time.Second) // keeps idle connections open
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.hub.closing:
			return
		case <-sub.ch:
			fmt.Fprint(w, "event: changed\ndata: {}\n\n")
			if still != nil && !still() {
				fl.Flush()
				return
			}
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
		}
		fl.Flush()
	}
}
