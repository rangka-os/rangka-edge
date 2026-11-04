package runner

import (
	"math/rand/v2"
	"time"
)

// Backoff computes retry delays. Zero values mean: Min 1 s, Max 5 min, Jitter 0.2, Rand from math/rand/v2.
// A negative Jitter turns jitter off.
type Backoff struct {
	Min, Max time.Duration
	Jitter   float64
	Rand     func() float64 // in [0, 1)
}

// Delay returns how long to wait before retry number attempt (0 is the first retry). The delay doubles
// from Min, is capped at Max and jittered, then raised to retryAfter (at most 1 h) when the kernel asked for more.
func (b Backoff) Delay(attempt int, retryAfter time.Duration) time.Duration {
	lo, hi, jitter, rnd := b.Min, b.Max, b.Jitter, b.Rand
	if lo <= 0 {
		lo = time.Second
	}
	if hi <= 0 {
		hi = 5 * time.Minute
	}
	if jitter == 0 {
		jitter = 0.2
	}
	if rnd == nil {
		rnd = rand.Float64
	}
	d := lo
	for i := 0; i < attempt && d < hi; i++ {
		d *= 2
	}
	d = min(d, hi)
	if jitter > 0 {
		d = time.Duration(float64(d) * (1 + jitter*(2*rnd()-1)))
	}
	// The kernel's Retry-After is a floor that neither the cap nor jitter may undercut (at most 1 h).
	return max(d, min(retryAfter, time.Hour))
}
