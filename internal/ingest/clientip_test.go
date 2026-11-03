package ingest

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/khaleelhabeeb/relay/internal/config"
	"github.com/khaleelhabeeb/relay/internal/paystack"
)

func TestIPFilter(t *testing.T) {
	paystackIPs := config.ParsePrefixes([]string{"52.31.139.75", "52.49.173.169", "52.214.14.220"})
	proxies := config.ParsePrefixes([]string{"10.0.0.0/8"})
	direct := &IPFilter{Allowed: paystackIPs}
	proxied := &IPFilter{Allowed: paystackIPs, TrustedProxies: proxies}

	cases := []struct {
		name   string
		filter *IPFilter
		remote string
		xff    []string
		allow  bool
	}{
		{"no filter", nil, "203.0.113.9:1234", nil, true},
		{"direct paystack", direct, "52.31.139.75:443", nil, true},
		{"direct stranger", direct, "203.0.113.9:443", nil, false},
		{"direct ignores XFF", direct, "203.0.113.9:443", []string{"52.31.139.75"}, false},
		{"via proxy, paystack", proxied, "10.1.2.3:5555", []string{"52.49.173.169"}, true},
		{"via proxy, stranger", proxied, "10.1.2.3:5555", []string{"203.0.113.9"}, false},
		{"spoofed leftmost entry", proxied, "10.1.2.3:5555", []string{"52.31.139.75, 203.0.113.9"}, false},
		{"chain of proxies", proxied, "10.1.2.3:5555", []string{"52.214.14.220, 10.9.9.9"}, true},
		{"multiple XFF headers", proxied, "10.1.2.3:5555", []string{"52.214.14.220", "10.9.9.9"}, true},
		{"proxy without XFF", proxied, "10.1.2.3:5555", nil, false},
		{"malformed XFF", proxied, "10.1.2.3:5555", []string{"not-an-ip"}, false},
		{"untrusted peer spoofing XFF", proxied, "203.0.113.9:5555", []string{"52.31.139.75"}, false},
		{"ipv4-mapped peer", direct, "[::ffff:52.31.139.75]:443", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/webhooks/paystack", nil)
			r.RemoteAddr = tc.remote
			for _, v := range tc.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if _, ok := tc.filter.Allows(r); ok != tc.allow {
				t.Fatalf("allowed = %v, want %v", ok, tc.allow)
			}
		})
	}
}

// blockingAcceptor holds every Accept until released.
type blockingAcceptor struct {
	entered chan struct{}
	release chan struct{}
}

func (b *blockingAcceptor) Accept(ctx context.Context, _ *Accepted) error {
	b.entered <- struct{}{}
	<-b.release
	return nil
}

func TestHandlerLimitsAndAllowlist(t *testing.T) {
	const secret = "sk_test_limits"
	body := []byte(`{"event":"charge.success","data":{"id":1,"reference":"X"}}`)
	sig := paystack.Sign(secret, body)
	acc := &blockingAcceptor{entered: make(chan struct{}, 10), release: make(chan struct{})}
	h := &Handler{Secret: secret, MaxBodyBytes: 1 << 20, Acceptor: acc, MaxConcurrent: 2,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	send := func() int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/webhooks/paystack", bytes.NewReader(body))
		req.Header.Set("x-paystack-signature", sig)
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); codes <- send() }()
	}
	for range 2 {
		select {
		case <-acc.entered:
		case <-time.After(2 * time.Second):
			t.Fatal("requests never reached the acceptor")
		}
	}
	if code := send(); code != http.StatusServiceUnavailable {
		t.Fatalf("third concurrent request: %d, want 503", code)
	}
	close(acc.release)
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != http.StatusOK {
			t.Fatalf("admitted request: %d", c)
		}
	}
	if code := send(); code != http.StatusOK {
		t.Fatalf("after load drains: %d", code)
	}

	h.IPFilter = &IPFilter{Allowed: config.ParsePrefixes([]string{"52.31.139.75"})}
	if code := send(); code != http.StatusForbidden { // httptest RemoteAddr is 192.0.2.1
		t.Fatalf("disallowed source: %d, want 403", code)
	}
}
