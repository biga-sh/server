// Package events fans live session events out to the viewers watching
// each session, across every server instance.
package events

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"sync"
)

// Event is one server-sent event: Name is its SSE event name and Data its
// JSON payload.
type Event struct {
	Name string
	Data []byte
}

// New encodes payload as an event. Payloads are the SSE event objects the
// viewer parses, each with a "type" field equal to name.
func New(name string, payload any) (Event, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return Event{}, err
	}
	return Event{Name: name, Data: data}, nil
}

// bufferSize is how many events a viewer may fall behind before it is
// disconnected. Its browser reconnects and reloads the session state, which
// is cheaper than holding an unbounded backlog for a stalled reader.
const bufferSize = 64

// Bridge relays events between server instances.
type Bridge interface {
	Publish(ctx context.Context, msg []byte) error
	Run(ctx context.Context, deliver func(msg []byte))
}

// Hub delivers events to local subscribers and, through a bridge, to the
// subscribers of other instances.
type Hub struct {
	instance string
	bridge   Bridge
	log      *slog.Logger

	mu   sync.Mutex
	subs map[string]map[*Subscription]struct{}
}

// NewHub returns a hub. bridge may be nil for a single instance.
func NewHub(bridge Bridge, log *slog.Logger) *Hub {
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	return &Hub{
		instance: hex.EncodeToString(id),
		bridge:   bridge,
		log:      log,
		subs:     make(map[string]map[*Subscription]struct{}),
	}
}

// Subscription receives one session's events until closed. C is closed
// when the subscriber falls too far behind.
type Subscription struct {
	C <-chan Event

	ch        chan Event
	hub       *Hub
	sessionID string
	once      sync.Once
}

// Subscribe starts receiving a session's events.
func (h *Hub) Subscribe(sessionID string) *Subscription {
	ch := make(chan Event, bufferSize)
	s := &Subscription{C: ch, ch: ch, hub: h, sessionID: sessionID}
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.subs[sessionID]
	if set == nil {
		set = make(map[*Subscription]struct{})
		h.subs[sessionID] = set
	}
	set[s] = struct{}{}
	return s
}

// Close stops the subscription. It is safe to call more than once.
func (s *Subscription) Close() {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	s.removeLocked()
}

func (s *Subscription) removeLocked() {
	s.once.Do(func() {
		set := s.hub.subs[s.sessionID]
		delete(set, s)
		if len(set) == 0 {
			delete(s.hub.subs, s.sessionID)
		}
		close(s.ch)
	})
}

// Publish delivers an event to this instance's subscribers, then hands it
// to the bridge. Local delivery never waits on the bridge.
func (h *Hub) Publish(ctx context.Context, sessionID string, e Event) {
	h.deliver(sessionID, e)
	if h.bridge == nil {
		return
	}
	msg, err := json.Marshal(bridgeMessage{Instance: h.instance, SessionID: sessionID, Event: e.Data})
	if err != nil {
		h.log.Error("encode bridged event", "error", err)
		return
	}
	if err := h.bridge.Publish(ctx, msg); err != nil {
		h.log.Warn("bridge publish failed; other instances miss this event", "error", err)
	}
}

func (h *Hub) deliver(sessionID string, e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs[sessionID] {
		select {
		case s.ch <- e:
		default:
			h.log.Debug("dropping slow viewer", "session", sessionID)
			s.removeLocked()
		}
	}
}

// Subscribers counts the open subscriptions across all sessions.
func (h *Hub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, set := range h.subs {
		n += len(set)
	}
	return n
}

// Run relays events from other instances until ctx is done. It returns
// immediately without a bridge.
func (h *Hub) Run(ctx context.Context) {
	if h.bridge == nil {
		return
	}
	h.bridge.Run(ctx, func(raw []byte) {
		var msg bridgeMessage
		if err := json.Unmarshal(raw, &msg); err != nil || msg.SessionID == "" {
			h.log.Warn("ignoring malformed bridged event")
			return
		}
		if msg.Instance == h.instance {
			return // already delivered locally
		}
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(msg.Event, &head); err != nil || head.Type == "" {
			h.log.Warn("ignoring bridged event without a type")
			return
		}
		h.deliver(msg.SessionID, Event{Name: head.Type, Data: msg.Event})
	})
}

// bridgeMessage is the cross-instance envelope.
type bridgeMessage struct {
	Instance  string          `json:"instanceId"`
	SessionID string          `json:"sessionId"`
	Event     json.RawMessage `json:"event"`
}
