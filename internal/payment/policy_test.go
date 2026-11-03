package payment

import (
	"sync"
	"testing"
	"time"
)

func TestTransitionPolicy(t *testing.T) {
	all := []Status{StatusPending, StatusPaid, StatusFailed, StatusDenied, StatusCancelled, StatusExpired}
	for _, cur := range all {
		for _, in := range append(all, "") {
			got := decide(cur, in)
			var want decision
			switch {
			case in == "":
				want = review
			case in == cur:
				want = keep
			case cur == StatusPending:
				want = move
			case in == StatusPaid:
				want = review
			default:
				want = ignore
			}
			if got != want {
				t.Errorf("decide(%s, %q) = %d, want %d", cur, in, got, want)
			}
		}
	}
	// The forbidden examples, spelled out.
	for _, c := range [][2]Status{{StatusPaid, StatusExpired}, {StatusPaid, StatusCancelled}, {StatusPaid, StatusPending}, {StatusExpired, StatusPending}} {
		if decide(c[0], c[1]) != ignore {
			t.Errorf("%s → %s must be ignored", c[0], c[1])
		}
	}
	if decide(StatusExpired, StatusPaid) != review || decide(StatusCancelled, StatusPaid) != review {
		t.Error("a payment after the attempt closed must be flagged, not applied")
	}
}

func TestProviderOrderID(t *testing.T) {
	if got := ProviderOrderID("ASH-20261103-000123", 2); got != "ASH-20261103-000123-P2" {
		t.Errorf("got %q", got)
	}
	if n := len(ProviderOrderID("ASH-20261103-1234567890", MaxAttempts)); n > 50 {
		t.Errorf("provider order id too long: %d", n)
	}
}

func TestAttemptWindow(t *testing.T) {
	now := time.Date(2026, 11, 3, 10, 15, 30, 500, time.UTC)
	start, exp, ok := attemptWindow(now, now.Add(23*time.Hour+59*time.Minute+50*time.Second))
	if !ok || !start.Equal(now.Truncate(time.Second)) || exp.Sub(start) != 23*time.Hour+59*time.Minute {
		t.Errorf("window: %v → %v %v", start, exp, ok)
	}
	if _, _, ok := attemptWindow(now, now.Add(50*time.Second)); ok {
		t.Error("less than a minute left must not open an attempt")
	}
}

func TestKeyedMutexSerializesPerKey(t *testing.T) {
	var k keyedMutex
	var wg sync.WaitGroup
	inside, maxInside := 0, 0
	var mu sync.Mutex
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := k.lock(7)
			mu.Lock()
			inside++
			maxInside = max(maxInside, inside)
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			inside--
			mu.Unlock()
			unlock()
		}()
	}
	wg.Wait()
	if maxInside != 1 || len(k.m) != 0 {
		t.Errorf("max concurrent holders = %d, leftover keys = %d", maxInside, len(k.m))
	}
}
