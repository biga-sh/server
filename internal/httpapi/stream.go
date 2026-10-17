package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/biga-sh/server/internal/wire"
)

// Live-stream timing.
const (
	keepaliveEvery = 15 * time.Second
	// streamWriteTimeout cuts off a viewer that stops reading.
	streamWriteTimeout = 10 * time.Second
)

// streamGate caps open streams, overall and per client, so one client
// cannot hold every connection the server has.
type streamGate struct {
	mu       sync.Mutex
	max, per int
	total    int
	byClient map[string]int
}

func newStreamGate(max, per int) *streamGate {
	return &streamGate{max: max, per: per, byClient: make(map[string]int)}
}

func (g *streamGate) acquire(client string) (ok, full bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.total >= g.max {
		return false, true
	}
	if g.byClient[client] >= g.per {
		return false, false
	}
	g.total++
	g.byClient[client]++
	return true, false
}

func (g *streamGate) release(client string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.total--
	if g.byClient[client]--; g.byClient[client] <= 0 {
		delete(g.byClient, client)
	}
}

// stream sends a session's live events as server-sent events.
func (a *API) stream(w http.ResponseWriter, r *http.Request) {
	id, ok := sessionID(w, r)
	if !ok {
		return
	}
	sess, err := a.svc.Session(r.Context(), id)
	if err != nil {
		a.serviceError(w, r, err, "invalid_state", "")
		return
	}

	client := clientKey(clientAddr(r.Context()))
	ok, full := a.streams.acquire(client)
	if !ok {
		retryAfter(w, 30*time.Second)
		if full {
			fail(w, r, http.StatusServiceUnavailable, "server_busy", "The server is at its live-stream capacity.", nil)
		} else {
			fail(w, r, http.StatusTooManyRequests, "too_many_streams", "Too many open live streams from this client.", nil)
		}
		return
	}
	defer a.streams.release(client)

	// Subscribe before the first write, so no event slips between the two.
	sub := a.hub.Subscribe(id)
	defer sub.Close()

	rc := http.NewResponseController(w)
	// No read deadline: net/http cancels the request context when one
	// expires, which would end every stream. Headers are already bounded by
	// ReadHeaderTimeout, and each write below carries its own deadline.
	_ = rc.SetReadDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no") // nginx: do not buffer
	w.WriteHeader(http.StatusOK)

	send := func(chunk []byte) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(streamWriteTimeout))
		if _, err := w.Write(chunk); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	event := func(name string, data []byte) bool {
		return send(fmt.Appendf(nil, "event: %s\ndata: %s\n\n", name, data))
	}
	ended := func() bool {
		data, _ := json.Marshal(map[string]any{"type": "session_ended", "sessionId": id, "timestamp": time.Now().UnixMilli()})
		return event("session_ended", data)
	}

	connected, _ := json.Marshal(map[string]any{"type": "connected", "sessionId": id, "timestamp": time.Now().UnixMilli()})
	if !event("connected", connected) || sess.Status != wire.StatusActive {
		return
	}

	tick := time.NewTicker(keepaliveEvery)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-a.done:
			return
		case e, ok := <-sub.C:
			if !ok {
				return // fell too far behind; the browser reconnects and resyncs
			}
			if !event(e.Name, e.Data) || e.Name == "session_ended" {
				return
			}
		case <-tick.C:
			stale, err := a.svc.Stale(r.Context(), id)
			if err != nil {
				a.log.Warn("stream staleness check failed", "session", id, "error", err)
				return
			}
			if stale {
				ended()
				return
			}
			if !send(fmt.Appendf(nil, ": keepalive %d\n\n", time.Now().UnixMilli())) {
				return
			}
		}
	}
}
