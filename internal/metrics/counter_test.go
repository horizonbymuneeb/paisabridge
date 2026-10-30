package metrics

import (
	"math"
	"sync"
	"testing"
)

// TestCounterAddAccumulates pins the bug where Add treated the IEEE-754 bits as
// an integer: the first increment decoded back to 1 by luck, and every later one
// produced an exponent of all ones (2^1023), so a scrape reported 8.9e307 for a
// counter that had counted two requests.
func TestCounterAddAccumulates(t *testing.T) {
	r := New()
	c := r.Counter("test_counter_total", "help")
	c.Inc()
	if got := c.With().Value(); got != 1 {
		t.Fatalf("after one Inc: %v, want 1", got)
	}
	c.Inc()
	if got := c.With().Value(); got != 2 {
		t.Fatalf("after two Inc: %v, want 2 (bit-add bug returns 2^1023)", got)
	}
	for i := 0; i < 100; i++ {
		c.Inc()
	}
	if got := c.With().Value(); got != 102 {
		t.Fatalf("after 102 Inc: %v, want 102", got)
	}
	if got := c.With().Value(); math.IsInf(got, 0) || got > 1e6 {
		t.Fatalf("counter exploded: %v", got)
	}
}

// TestCounterAddFractional: fractional amounts still accumulate exactly.
func TestCounterAddFractional(t *testing.T) {
	r := New()
	c := r.Counter("test_bytes_total", "help")
	c.With().Add(0.25)
	c.With().Add(0.5)
	if got := c.With().Value(); got != 0.75 {
		t.Fatalf("got %v, want 0.75", got)
	}
}

// TestCounterConcurrentAdds: no lost increments under contention.
func TestCounterConcurrentAdds(t *testing.T) {
	r := New()
	c := r.Counter("test_concurrent_total", "help")
	const workers, per = 8, 250
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < per; j++ {
				c.Inc()
			}
		}()
	}
	wg.Wait()
	if got, want := c.With().Value(), float64(workers*per); got != want {
		t.Fatalf("concurrent total = %v, want %v", got, want)
	}
}
