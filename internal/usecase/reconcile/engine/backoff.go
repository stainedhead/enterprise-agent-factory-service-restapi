package engine

import "time"

// Backoff is capped exponential backoff: Base, 2×Base, 4×Base, ... up to Max.
type Backoff struct {
	Base time.Duration
	Max  time.Duration
}

// DefaultBackoff is 1 s growing to 5 min (RC-4).
var DefaultBackoff = Backoff{Base: time.Second, Max: 5 * time.Minute}

// Delay returns the wait before retry number failures (1 for the first retry).
func (b Backoff) Delay(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	d := b.Base
	for i := 1; i < failures; i++ {
		if d >= b.Max/2 {
			return b.Max
		}
		d *= 2
	}
	if d > b.Max {
		return b.Max
	}
	return d
}
