package ratelimit

import (
	"testing"
	"time"
)

func TestMemoryWindow(t *testing.T) {
	t.Parallel()
	now := time.Unix(0, 0)
	m := NewMemory()
	m.now = func() time.Time { return now }

	for i := range 3 {
		if ok, _ := m.Allow(t.Context(), "k", 3, time.Minute); !ok {
			t.Fatalf("request %d rejected within the limit", i+1)
		}
	}
	ok, retry := m.Allow(t.Context(), "k", 3, time.Minute)
	if ok || retry != time.Minute {
		t.Fatalf("4th request: ok=%v retry=%v, want rejected with 1m", ok, retry)
	}
	if ok, _ := m.Allow(t.Context(), "other", 3, time.Minute); !ok {
		t.Error("keys should not share a budget")
	}

	if over, wait := m.Exceeded(t.Context(), "k", 3); !over || wait != time.Minute {
		t.Errorf("Exceeded = %v, %v; want true, 1m", over, wait)
	}
	if over, _ := m.Exceeded(t.Context(), "other", 3); over {
		t.Error("Exceeded counted a fresh key as over")
	}

	now = now.Add(time.Minute)
	if over, _ := m.Exceeded(t.Context(), "k", 3); over {
		t.Error("Exceeded should reset with the window")
	}
	if ok, _ := m.Allow(t.Context(), "k", 3, time.Minute); !ok {
		t.Error("a new window should reset the budget")
	}
}
