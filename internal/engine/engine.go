// Package engine implements a deterministic discrete-event simulation clock.
//
// All simulator components run inside a single goroutine and interact only
// through scheduled callbacks. Virtual time advances only when the event queue
// is drained, so a 3-minute peak-load scenario with millions of DNS packets
// runs in a fraction of a second and is fully reproducible for a given seed.
package engine

import (
	"container/heap"
	"fmt"
	"math"
	"math/rand/v2"
	"time"
)

// Event is a scheduled callback. Events returned to callers can be cancelled
// before they fire.
type Event struct {
	at        int64
	seq       uint64
	fn        func()
	cancelled bool
}

// Cancel prevents the event from firing. It is safe to call after firing; it
// then has no effect.
func (e *Event) Cancel() {
	if e != nil {
		e.cancelled = true
	}
}

// Engine is the virtual clock and event queue.
type Engine struct {
	now  int64
	seq  uint64
	heap eventHeap
}

// New returns an engine whose clock starts at zero.
func New() *Engine {
	return &Engine{}
}

// Now returns the current virtual time.
func (e *Engine) Now() time.Duration { return time.Duration(e.now) }

// At schedules fn to run after d (d <= 0 runs at the next drain step).
func (e *Engine) At(d time.Duration, fn func()) *Event {
	return e.AtAbs(time.Duration(e.now)+d, fn)
}

// AtAbs schedules fn to run at absolute virtual time t.
func (e *Engine) AtAbs(t time.Duration, fn func()) *Event {
	at := int64(t)
	if at < e.now {
		at = e.now
	}
	e.seq++
	ev := &Event{at: at, seq: e.seq, fn: fn}
	heap.Push(&e.heap, ev)
	return ev
}

// Every schedules a repeating callback; it stops when fn returns false.
func (e *Engine) Every(period time.Duration, fn func() bool) *Event {
	var ev *Event
	ev = e.At(period, func() {
		if fn() {
			ev = e.At(period, ev.fn)
		}
	})
	return ev
}

// RunUntil processes every event with time <= end, then advances the clock to
// end. It panics if an event schedules work in the past, which would indicate a
// bug in the simulator.
func (e *Engine) RunUntil(end time.Duration) {
	limit := int64(end)
	for e.heap.Len() > 0 {
		ev := e.heap[0]
		if ev.at > limit {
			break
		}
		heap.Pop(&e.heap)
		if ev.cancelled {
			continue
		}
		e.now = ev.at
		ev.fn()
	}
	e.now = limit
}

// RunUntilIdle drains the queue regardless of time.
func (e *Engine) RunUntilIdle() {
	for e.heap.Len() > 0 {
		ev := e.heap[0]
		heap.Pop(&e.heap)
		if ev.cancelled {
			continue
		}
		e.now = ev.at
		ev.fn()
	}
}

// Pending reports the number of queued events (including cancelled ones).
func (e *Engine) Pending() int { return e.heap.Len() }

// Rand is a deterministic random source with helpers for simulation.
type Rand struct {
	r *rand.Rand
}

// NewRand creates an independent random stream. Use a distinct salt per
// subsystem so that tweaking one component does not perturb the others'
// random draws.
func NewRand(seed uint64, salt uint64) *Rand {
	return &Rand{r: rand.New(rand.NewPCG(seed, salt))}
}

// Float64 returns a uniform value in [0,1).
func (r *Rand) Float64() float64 { return r.r.Float64() }

// IntN returns a uniform integer in [0,n).
func (r *Rand) IntN(n int) int { return r.r.IntN(n) }

// IntRange returns a uniform integer in [lo,hi].
func (r *Rand) IntRange(lo, hi int) int {
	if hi <= lo {
		return lo
	}
	return lo + r.r.IntN(hi-lo+1)
}

// Chance reports whether a uniform draw is below p.
func (r *Rand) Chance(p float64) bool { return r.r.Float64() < p }

// Exp returns an exponentially distributed duration with the given mean.
func (r *Rand) Exp(mean time.Duration) time.Duration {
	u := r.r.Float64()
	if u <= 0 {
		u = 1e-12
	}
	return time.Duration(float64(mean) * -math.Log(u))
}

// UniformDuration returns a duration uniformly in [lo,hi].
func (r *Rand) UniformDuration(lo, hi time.Duration) time.Duration {
	if hi <= lo {
		return lo
	}
	return lo + time.Duration(r.r.Float64()*float64(hi-lo))
}

// NormalDuration returns a normal duration truncated at one standard
// deviation and clamped to [min,max].
func (r *Rand) NormalDuration(mean, stddev, min, max time.Duration) time.Duration {
	z := r.r.NormFloat64()
	if z < -3 {
		z = -3
	} else if z > 3 {
		z = 3
	}
	d := time.Duration(float64(mean) + z*float64(stddev))
	if d < min {
		d = min
	}
	if d > max {
		d = max
	}
	return d
}

// Pick returns a random element index weighted by weights.
func (r *Rand) Pick(weights []float64) int {
	total := 0.0
	for _, w := range weights {
		total += w
	}
	if total <= 0 {
		return 0
	}
	x := r.r.Float64() * total
	for i, w := range weights {
		x -= w
		if x < 0 {
			return i
		}
	}
	return len(weights) - 1
}

// WeightedName is a name with a sampling weight.
type WeightedName struct {
	Name   string
	Weight float64
}

// PickName samples from a weighted name list.
func (r *Rand) PickName(names []WeightedName) string {
	w := make([]float64, len(names))
	for i := range names {
		w[i] = names[i].Weight
	}
	return names[r.Pick(w)].Name
}

// eventHeap orders events by time, then by scheduling order, which makes the
// simulation fully deterministic.
type eventHeap []*Event

func (h eventHeap) Len() int { return len(h) }
func (h eventHeap) Less(i, j int) bool {
	if h[i].at != h[j].at {
		return h[i].at < h[j].at
	}
	return h[i].seq < h[j].seq
}
func (h eventHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *eventHeap) Push(x any)   { *h = append(*h, x.(*Event)) }
func (h *eventHeap) Pop() any {
	old := *h
	n := len(old)
	ev := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return ev
}

// String renders an event for debugging.
func (e *Event) String() string {
	return fmt.Sprintf("event@%s#%d", time.Duration(e.at), e.seq)
}
