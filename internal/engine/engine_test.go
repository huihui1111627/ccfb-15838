package engine

import (
	"testing"
	"time"
)

func TestOrderingAndVirtualTime(t *testing.T) {
	e := New()
	var got []string
	e.At(30*time.Millisecond, func() { got = append(got, "c") })
	e.At(10*time.Millisecond, func() { got = append(got, "a") })
	// Same timestamp: scheduling order must be preserved.
	e.At(20*time.Millisecond, func() { got = append(got, "b1") })
	e.At(20*time.Millisecond, func() { got = append(got, "b2") })
	e.RunUntil(time.Second)
	want := []string{"a", "b1", "b2", "c"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order %v, want %v", got, want)
		}
	}
	if e.Now() != time.Second {
		t.Fatalf("clock %v", e.Now())
	}
}

func TestCancel(t *testing.T) {
	e := New()
	fired := false
	ev := e.At(time.Second, func() { fired = true })
	ev.Cancel()
	e.RunUntil(2 * time.Second)
	if fired {
		t.Fatal("cancelled event fired")
	}
}

func TestPastEventsRunImmediately(t *testing.T) {
	e := New()
	e.RunUntil(10 * time.Second)
	var at time.Duration
	e.At(-time.Second, func() { at = e.Now() })
	e.RunUntil(11 * time.Second)
	if at != 10*time.Second {
		t.Fatalf("past event ran at %v", at)
	}
}

func TestEvery(t *testing.T) {
	e := New()
	n := 0
	e.Every(10*time.Millisecond, func() bool {
		n++
		return n < 3
	})
	e.RunUntil(time.Second)
	if n != 3 {
		t.Fatalf("callback ran %d times", n)
	}
}

func TestRandomDeterminism(t *testing.T) {
	a := NewRand(42, 7)
	b := NewRand(42, 7)
	for i := 0; i < 100; i++ {
		if a.Float64() != b.Float64() {
			t.Fatal("same seed diverged")
		}
	}
	c := NewRand(42, 8)
	if a.Float64() == c.Float64() {
		t.Log("different salt produced the same value once (possible but unlikely)")
	}
}

func TestExpMean(t *testing.T) {
	r := NewRand(1, 2)
	var sum float64
	const n = 100000
	for i := 0; i < n; i++ {
		sum += float64(r.Exp(100 * time.Millisecond))
	}
	mean := sum / n
	if mean < 95e6 || mean > 105e6 { // nanoseconds
		t.Fatalf("mean %.0fns, want ~100ms", mean)
	}
}
