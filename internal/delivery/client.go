package delivery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"

	"github.com/khaleelhabeeb/relay/internal/netguard"
)

// ErrBlockedDestination means a destination resolved to a non-public
// address and the connection was refused before it was made.
var ErrBlockedDestination = errors.New("delivery: destination address not allowed")

// NewHTTPClient returns the client used for deliveries. It never follows
// redirects and ignores proxy environment variables. Unless allowInsecure
// is set, every connection is checked after DNS resolution, at connect
// time, so a hostname that resolves (or re-resolves, as in DNS rebinding)
// to a private, loopback, link-local or metadata address is refused.
func NewHTTPClient(allowInsecure bool) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if !allowInsecure {
		dialer.Control = guardControl
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 0, // bounded by the per-attempt context
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func guardControl(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrBlockedDestination, address)
	}
	if !netguard.IsPublic(ap.Addr()) {
		return fmt.Errorf("%w: %s", ErrBlockedDestination, ap.Addr())
	}
	return nil
}

// classify maps a transport error to an attempt error code.
func classify(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, ErrBlockedDestination):
		return "blocked_destination"
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "aborted"
	default:
		var nerr net.Error
		if errors.As(err, &nerr) && nerr.Timeout() {
			return "timeout"
		}
		return "connection_error"
	}
}
