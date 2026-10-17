package events

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestPublishReachesOnlyThatSession(t *testing.T) {
	t.Parallel()
	h := NewHub(nil, quiet)
	a, b := h.Subscribe("a"), h.Subscribe("b")
	defer a.Close()
	defer b.Close()

	h.Publish(t.Context(), "a", Event{Name: "checkpoint", Data: []byte(`{}`)})
	select {
	case e := <-a.C:
		if e.Name != "checkpoint" {
			t.Errorf("event = %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber a got nothing")
	}
	select {
	case e := <-b.C:
		t.Errorf("subscriber b got %+v", e)
	default:
	}
}

func TestSlowSubscriberIsDropped(t *testing.T) {
	t.Parallel()
	h := NewHub(nil, quiet)
	s := h.Subscribe("a")
	for range bufferSize + 1 {
		h.Publish(t.Context(), "a", Event{Name: "x", Data: []byte(`{}`)})
	}
	n := 0
	for range s.C {
		n++
	}
	if n != bufferSize {
		t.Errorf("received %d events before close, want %d", n, bufferSize)
	}
	if h.Subscribers() != 0 {
		t.Errorf("Subscribers() = %d after drop", h.Subscribers())
	}
	s.Close() // still safe
}

// loopBridge connects hubs in memory, like Redis would.
type loopBridge struct {
	mu    sync.Mutex
	peers []func([]byte)
}

func (l *loopBridge) Publish(_ context.Context, msg []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, p := range l.peers {
		p(msg)
	}
	return nil
}

func (l *loopBridge) Run(ctx context.Context, deliver func([]byte)) {
	l.mu.Lock()
	l.peers = append(l.peers, deliver)
	l.mu.Unlock()
	<-ctx.Done()
}

func TestBridgeDeliversToOtherInstancesOnce(t *testing.T) {
	t.Parallel()
	bridge := &loopBridge{}
	one, two := NewHub(bridge, quiet), NewHub(bridge, quiet)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go one.Run(ctx)
	go two.Run(ctx)
	for {
		bridge.mu.Lock()
		n := len(bridge.peers)
		bridge.mu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	local, remote := one.Subscribe("s"), two.Subscribe("s")
	defer local.Close()
	defer remote.Close()
	one.Publish(ctx, "s", Event{Name: "snapshot", Data: []byte(`{"type":"snapshot"}`)})

	for name, sub := range map[string]*Subscription{"local": local, "remote": remote} {
		select {
		case e := <-sub.C:
			if e.Name != "snapshot" {
				t.Errorf("%s event = %+v", name, e)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s subscriber got nothing", name)
		}
		select {
		case e := <-sub.C:
			t.Errorf("%s subscriber got a duplicate %+v", name, e)
		case <-time.After(20 * time.Millisecond):
		}
	}
}
