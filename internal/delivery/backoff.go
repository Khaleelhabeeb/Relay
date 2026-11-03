package delivery

import (
	"math/rand/v2"
	"time"
)

// jitter returns d scaled by a random factor in [0.8, 1.2). rnd returns a
// float in [0, 1); nil uses math/rand/v2.
func jitter(d time.Duration, rnd func() float64) time.Duration {
	if d <= 0 {
		return 0
	}
	if rnd == nil {
		rnd = rand.Float64
	}
	return time.Duration(float64(d) * (0.8 + 0.4*rnd()))
}
