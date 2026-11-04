package config

import (
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/khaleelhabeeb/relay/internal/netguard"
)

// MinSecretLen is the shortest accepted outbound signing secret or
// registration token, in bytes.
const MinSecretLen = 32

var appIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

func (c *Config) sortedAppIDs() []string {
	ids := make([]string, 0, len(c.Applications))
	for id := range c.Applications {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (c *Config) validate(extraProviders []string) []error {
	var errs []error
	fail := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if c.Version != 1 {
		fail("version: unsupported version %d (expected 1)", c.Version)
	}

	if c.Server.APIListen != "" && c.Server.APIListen == c.Server.Listen {
		fail("server.api_listen: must differ from server.listen (omit it to serve the API on the main listener)")
	}
	if c.Server.MaxConcurrentWebhooks < 1 || c.Server.MaxConcurrentWebhooks > 10000 {
		fail("server.max_concurrent_webhooks: must be between 1 and 10000")
	}
	if c.Server.MaxBodyBytes < 1 || c.Server.MaxBodyBytes > 16<<20 {
		fail("server.max_body_bytes: must be between 1 and %d", 16<<20)
	}
	if c.Server.PublicBaseURL != "" {
		if u, err := url.Parse(c.Server.PublicBaseURL); err != nil || u.Scheme == "" || u.Host == "" {
			fail("server.public_base_url: must be an absolute URL")
		}
	}
	for _, p := range c.Server.TrustedProxies {
		if !validIPOrPrefix(p) {
			fail("server.trusted_proxies: %q is not an IP address or CIDR", p)
		}
	}
	providers := append(c.Providers.Names(), extraProviders...)
	if len(providers) == 0 {
		fail("providers: at least one provider (paystack or flutterwave) is required")
	}
	for _, name := range c.Providers.Names() {
		for _, p := range c.Providers.AllowedIPs(name) {
			if !validIPOrPrefix(p) {
				fail("providers.%s.allowed_ips: %q is not an IP address or CIDR", name, p)
			}
		}
	}

	if c.Storage.Driver != "sqlite" {
		fail("storage.driver: unsupported driver %q (only sqlite)", c.Storage.Driver)
	}
	if c.Storage.DSN == "" {
		fail("storage.dsn: required")
	}

	if len(c.Applications) == 0 {
		fail("applications: at least one application is required")
	}
	signingSecrets := map[string]string{}
	tokens := map[string]string{}
	for _, id := range c.sortedAppIDs() {
		app := c.Applications[id]
		p := "applications." + id
		if app == nil {
			fail("%s: empty definition", p)
			continue
		}
		if !appIDPattern.MatchString(id) {
			fail("%s: id must match %s", p, appIDPattern)
		}
		if err := CheckEndpoint(app.Endpoint, c.Delivery.AllowInsecureLocal); err != nil {
			fail("%s.endpoint: %v", p, err)
		}
		if app.PayloadFormat != PayloadOriginal && app.PayloadFormat != PayloadEnvelope {
			fail("%s.payload_format: must be %q or %q, got %q", p, PayloadOriginal, PayloadEnvelope, app.PayloadFormat)
		}
		checkSecret := func(field, v string) {
			if v != "" && len(v) < MinSecretLen {
				fail("%s.%s: secret must be at least %d bytes", p, field, MinSecretLen)
			}
		}
		checkSecret("signing_secret_env", app.SigningSecret)
		checkSecret("previous_signing_secret_env", app.PreviousSigningSecret)
		checkSecret("registration_token_env", app.RegistrationToken)
		if s := app.SigningSecret; s != "" {
			if other, dup := signingSecrets[s]; dup {
				fail("%s.signing_secret_env: same secret as applications.%s; each application needs its own", p, other)
			}
			signingSecrets[s] = id
		}
		if t := app.RegistrationToken; t != "" {
			if other, dup := tokens[t]; dup {
				fail("%s.registration_token_env: same token as applications.%s; tokens must identify one application", p, other)
			}
			tokens[t] = id
		}
	}

	for i, r := range c.Routes {
		p := fmt.Sprintf("routes[%d]", i)
		if r.Provider != "" && !slices.Contains(providers, r.Provider) {
			fail("%s.provider: %q is not a configured provider", p, r.Provider)
		}
		if _, ok := c.Applications[r.Application]; !ok {
			fail("%s.application: unknown application %q", p, r.Application)
		}
		if r.Match.ReferencePrefix == "" && len(r.Match.Metadata) == 0 {
			fail("%s.match: needs reference_prefix and/or metadata", p)
		}
		for k, v := range r.Match.Metadata {
			if strings.TrimSpace(k) == "" || v == "" {
				fail("%s.match.metadata: keys and values must be non-empty", p)
			}
		}
	}
	for i := range c.Routes {
		for j := i + 1; j < len(c.Routes); j++ {
			a, b := c.Routes[i], c.Routes[j]
			sameProvider := a.Provider == "" || b.Provider == "" || a.Provider == b.Provider
			if a.Application != b.Application && sameProvider && (implies(a.Match, b.Match) || implies(b.Match, a.Match)) {
				fail("routes[%d] (%s) and routes[%d] (%s) overlap: an event can match both, so ownership would be ambiguous",
					i, a.Application, j, b.Application)
			}
		}
	}

	d := c.Delivery
	if d.MaxAttempts < 1 || d.MaxAttempts > 50 {
		fail("delivery.max_attempts: must be between 1 and 50")
	}
	if d.TimeoutSeconds < 1 || d.TimeoutSeconds > 120 {
		fail("delivery.timeout_seconds: must be between 1 and 120")
	}
	if d.WorkerCount < 1 || d.WorkerCount > 256 {
		fail("delivery.worker_count: must be between 1 and 256")
	}
	for i, s := range d.RetrySchedule {
		if s < 0 || time.Duration(s) > 7*24*time.Hour {
			fail("delivery.retry_schedule[%d]: must be between 0s and 168h", i)
		}
	}

	if c.Retention.PayloadDays < 1 {
		fail("retention.payload_days: must be at least 1")
	}
	if c.Retention.AttemptDays < 1 {
		fail("retention.attempt_days: must be at least 1")
	}
	return errs
}

// implies reports whether every event matching b also matches a: a's prefix
// is a prefix of b's (or a has none) and a's metadata is a subset of b's.
// Two routes to different applications where one implies the other are a
// statically detectable conflict. Partial overlaps (e.g. a prefix-only route
// and a metadata-only route) can only be detected per event, at runtime.
func implies(a, b MatchConfig) bool {
	if a.ReferencePrefix != "" && !strings.HasPrefix(b.ReferencePrefix, a.ReferencePrefix) {
		return false
	}
	for k, v := range a.Metadata {
		if bv, ok := b.Metadata[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// CheckEndpoint validates a delivery destination. Without allowInsecure it
// requires https and rejects localhost names and non-public IP literals.
// Hostnames that resolve to non-public addresses are blocked at connect time.
func CheckEndpoint(raw string, allowInsecure bool) error {
	if raw == "" {
		return fmt.Errorf("required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("scheme must be https")
	}
	if u.Host == "" || u.Hostname() == "" {
		return fmt.Errorf("missing host")
	}
	if u.User != nil {
		return fmt.Errorf("credentials in the URL are not allowed")
	}
	if u.Fragment != "" {
		return fmt.Errorf("fragments are not allowed")
	}
	if allowInsecure {
		return nil
	}
	if u.Scheme != "https" {
		return fmt.Errorf("scheme must be https (set delivery.allow_insecure_local for development)")
	}
	host := u.Hostname()
	if netguard.IsLocalHostname(host) {
		return fmt.Errorf("local destination %q not allowed (set delivery.allow_insecure_local for development)", host)
	}
	if addr, err := netip.ParseAddr(host); err == nil && !netguard.IsPublic(addr) {
		return fmt.Errorf("non-public address %s not allowed (set delivery.allow_insecure_local for development)", host)
	}
	return nil
}

func validIPOrPrefix(s string) bool {
	if _, err := netip.ParseAddr(s); err == nil {
		return true
	}
	_, err := netip.ParsePrefix(s)
	return err == nil
}
