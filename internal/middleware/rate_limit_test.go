package middleware

import (
	"testing"
	"time"
)

func TestFixedWindow(t *testing.T) {
	l := &fixedWindow{limit: 2, window: time.Minute, hits: make(map[string]*windowCount)}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for i, want := range []bool{true, true, false} {
		if ok, _ := l.allow("1.1.1.1", t0); ok != want {
			t.Fatalf("request %d: allow = %v, want %v", i+1, ok, want)
		}
	}
	if ok, _ := l.allow("2.2.2.2", t0); !ok {
		t.Error("another IP must have its own budget")
	}
	if ok, _ := l.allow("1.1.1.1", t0.Add(time.Minute)); !ok {
		t.Error("budget must reset after the window")
	}

	l.allow("3.3.3.3", t0)
	l.allow("1.1.1.1", t0.Add(3*time.Minute)) // triggers a sweep
	if _, kept := l.hits["3.3.3.3"]; kept {
		t.Error("expired entries must be swept")
	}
}
