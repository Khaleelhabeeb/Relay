package delivery

import (
	"net/http"
	"time"
)

// BreakerSettings configure the per-application circuit breaker. After
// Threshold consecutive failed attempts that suggest the endpoint is down
// (timeouts, connection errors, HTTP 5xx and 429), the application's
// circuit opens: no deliveries to it are claimed, so they keep their
// retry budget and leave the workers to healthy applications. After
// Cooldown one probe delivery is let through. If it succeeds the circuit
// closes; if it fails the cooldown doubles, up to MaxCooldown.
//
// Threshold <= 0 disables the breaker.
type BreakerSettings struct {
	Threshold   int
	Cooldown    time.Duration
	MaxCooldown time.Duration
}

// circuit is one application's breaker state. Guarded by Dispatcher.mu.
type circuit struct {
	failures  int           // consecutive unhealthy results while closed
	open      bool          // deliveries are paused
	openUntil time.Time     // while open: when a probe may start
	cooldown  time.Duration // the cooldown that set openUntil
	probe     string        // delivery ID of the probe in flight; "" if none
}

// health is what one attempt says about an endpoint.
type health int

const (
	neutral   health = iota // says nothing (e.g. a payload error, shutdown)
	healthy                 // the endpoint answered: it is up
	unhealthy               // the endpoint is down, overloaded or unreachable
)

// healthOf classifies an attempt result for the breaker. Any response
// other than 5xx or 429 shows the endpoint is up, even a 4xx: retrying a
// rejected event is the retry schedule's job, not the breaker's.
func healthOf(httpStatus int, errorCode string) health {
	switch errorCode {
	case "":
		return healthy
	case "timeout", "connection_error":
		return unhealthy
	case "http_status", "redirect_not_followed":
		if httpStatus >= 500 || httpStatus == http.StatusTooManyRequests {
			return unhealthy
		}
		return healthy
	default:
		return neutral
	}
}

// breakerCapacity limits n, an application's remaining concurrency, by
// its circuit: 0 while open, 1 for a probe once the cooldown has passed.
// Callers hold d.mu.
func (d *Dispatcher) breakerCapacity(app string, n int, now time.Time) int {
	c := d.circuits[app]
	if c == nil || !c.open {
		return n
	}
	if c.probe != "" || now.Before(c.openUntil) {
		return 0
	}
	return min(n, 1)
}

// markProbe records a delivery claimed while app's circuit is open as its
// probe. Callers hold d.mu.
func (d *Dispatcher) markProbe(app, deliveryID string, now time.Time) {
	if c := d.circuits[app]; c != nil && c.open && c.probe == "" && !now.Before(c.openUntil) {
		c.probe = deliveryID
	}
}

// breakerEvent is a circuit state change to log and export.
type breakerEvent int

const (
	breakerNone     breakerEvent = iota
	breakerOpened                // closed → open
	breakerReopened              // a probe failed
	breakerClosed                // the endpoint recovered
)

// recordHealth updates app's circuit with one attempt's result and
// reports any state change, with the cooldown now in force.
func (d *Dispatcher) recordHealth(app, deliveryID string, h health, now time.Time) (breakerEvent, time.Duration) {
	if d.Breaker.Threshold <= 0 {
		return breakerNone, 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.circuits[app]
	if h == neutral {
		// A probe that proved nothing frees the slot for another.
		if c != nil && c.probe == deliveryID {
			c.probe = ""
		}
		return breakerNone, 0
	}
	if h == healthy {
		if c == nil {
			return breakerNone, 0
		}
		delete(d.circuits, app)
		if c.open {
			return breakerClosed, 0
		}
		return breakerNone, 0
	}

	if c == nil {
		c = &circuit{}
		d.circuits[app] = c
	}
	if c.open {
		// Only the probe's failure counts; attempts started before the
		// circuit opened add nothing new.
		if c.probe != deliveryID {
			return breakerNone, 0
		}
		c.probe = ""
		c.cooldown = min(2*c.cooldown, d.maxCooldown())
		c.openUntil = now.Add(c.cooldown)
		return breakerReopened, c.cooldown
	}
	c.failures++
	if c.failures < d.Breaker.Threshold {
		return breakerNone, 0
	}
	c.open, c.failures, c.probe = true, 0, ""
	c.cooldown = min(d.cooldown(), d.maxCooldown())
	c.openUntil = now.Add(c.cooldown)
	return breakerOpened, c.cooldown
}

const (
	defaultBreakerCooldown    = 30 * time.Second
	defaultBreakerMaxCooldown = 10 * time.Minute
)

func (d *Dispatcher) cooldown() time.Duration {
	if d.Breaker.Cooldown > 0 {
		return d.Breaker.Cooldown
	}
	return defaultBreakerCooldown
}

func (d *Dispatcher) maxCooldown() time.Duration {
	if d.Breaker.MaxCooldown > 0 {
		return max(d.Breaker.MaxCooldown, d.cooldown())
	}
	return max(defaultBreakerMaxCooldown, d.cooldown())
}
