// Package config loads and validates Relay's YAML configuration.
//
// Secrets never appear in the file. The file names environment variables
// (the *_env fields), and Load resolves them into the yaml:"-" fields.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Payload formats an application can receive.
const (
	PayloadOriginal = "original"
	PayloadEnvelope = "envelope"
)

// Defaults applied when a field is omitted.
const (
	DefaultListen         = ":8080"
	DefaultMaxBodyBytes   = 1 << 20
	DefaultMaxConcurrent  = 256
	DefaultMaxAttempts    = 10
	DefaultTimeoutSeconds = 10
	DefaultWorkerCount    = 8
	DefaultPayloadDays    = 90
	DefaultAttemptDays    = 90
)

// DefaultRetrySchedule is the delay before each attempt (index 0 = first
// attempt). When max_attempts exceeds its length, the last delay repeats.
var DefaultRetrySchedule = []Duration{
	0,
	Duration(30 * time.Second),
	Duration(2 * time.Minute),
	Duration(10 * time.Minute),
	Duration(30 * time.Minute),
	Duration(time.Hour),
	Duration(2 * time.Hour),
	Duration(4 * time.Hour),
	Duration(8 * time.Hour),
	Duration(8 * time.Hour),
}

type Config struct {
	Version      int                           `yaml:"version"`
	Server       ServerConfig                  `yaml:"server"`
	Storage      StorageConfig                 `yaml:"storage"`
	Providers    ProvidersConfig               `yaml:"providers"`
	Applications map[string]*ApplicationConfig `yaml:"applications"`
	Routes       []RouteConfig                 `yaml:"routes"`
	Delivery     DeliveryConfig                `yaml:"delivery"`
	Retention    RetentionConfig               `yaml:"retention"`
}

type ServerConfig struct {
	Listen string `yaml:"listen"`
	// APIListen, if set, serves the registration API on its own address
	// (e.g. a private interface) instead of alongside the webhook endpoint.
	APIListen string `yaml:"api_listen"`
	// Metrics enables GET /metrics, served on api_listen when set,
	// otherwise on the main listener. Restrict access at your proxy.
	Metrics       bool   `yaml:"metrics"`
	PublicBaseURL string `yaml:"public_base_url"`
	MaxBodyBytes  int64  `yaml:"max_body_bytes"`
	// MaxConcurrentWebhooks caps webhooks processed at once; excess get 503.
	MaxConcurrentWebhooks int      `yaml:"max_concurrent_webhooks"`
	TrustedProxies        []string `yaml:"trusted_proxies"`
}

type StorageConfig struct {
	Driver string `yaml:"driver"`
	DSN    string `yaml:"dsn"`
}

type ProvidersConfig struct {
	Paystack PaystackConfig `yaml:"paystack"`
}

type PaystackConfig struct {
	SecretEnv  string   `yaml:"secret_env"`
	AllowedIPs []string `yaml:"allowed_ips"`

	Secret string `yaml:"-"`
}

type ApplicationConfig struct {
	Endpoint                 string `yaml:"endpoint"`
	SigningSecretEnv         string `yaml:"signing_secret_env"`
	PreviousSigningSecretEnv string `yaml:"previous_signing_secret_env"`
	RegistrationTokenEnv     string `yaml:"registration_token_env"`
	PayloadFormat            string `yaml:"payload_format"`

	ID                    string `yaml:"-"`
	SigningSecret         string `yaml:"-"`
	PreviousSigningSecret string `yaml:"-"`
	RegistrationToken     string `yaml:"-"`
}

// RouteConfig sends events to Application when every set matcher holds:
// the reference starts with ReferencePrefix (if set) and each Metadata key
// equals its value (if any are set).
type RouteConfig struct {
	Application string      `yaml:"application"`
	Match       MatchConfig `yaml:"match"`
}

type MatchConfig struct {
	ReferencePrefix string            `yaml:"reference_prefix"`
	Metadata        map[string]string `yaml:"metadata"`
}

type DeliveryConfig struct {
	MaxAttempts    int        `yaml:"max_attempts"`
	TimeoutSeconds int        `yaml:"timeout_seconds"`
	WorkerCount    int        `yaml:"worker_count"`
	RetrySchedule  []Duration `yaml:"retry_schedule"`
	// AllowInsecureLocal permits http:// and loopback/private destinations.
	// Development only.
	AllowInsecureLocal bool `yaml:"allow_insecure_local"`
}

type RetentionConfig struct {
	PayloadDays int `yaml:"payload_days"`
	AttemptDays int `yaml:"attempt_days"`
}

// Duration is a time.Duration written in YAML as a Go duration string ("30s").
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = Duration(v)
	return nil
}

// LookupEnv matches os.LookupEnv, so tests can substitute a map.
type LookupEnv func(key string) (string, bool)

// LoadFile reads, resolves and validates the config at path using the process environment.
func LoadFile(path string) (*Config, error) {
	f, err := os.Open(path) // #nosec G304 -- operator-supplied config path
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Load(f, os.LookupEnv)
}

// LoadFileWithoutSecrets reads and validates the config at path but does
// not resolve secrets. Operator commands use it: they need storage and
// application names, not signing keys.
func LoadFileWithoutSecrets(path string) (*Config, error) {
	f, err := os.Open(path) // #nosec G304 -- operator-supplied config path
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return load(f, nil)
}

// Load parses YAML from r, applies defaults, resolves secrets from env and
// validates the result. It returns every validation problem it finds at once.
func Load(r io.Reader, env LookupEnv) (*Config, error) {
	if env == nil {
		return nil, errors.New("config: env lookup is required")
	}
	return load(r, env)
}

// load does the work of Load; a nil env skips secret resolution.
func load(r io.Reader, env LookupEnv) (*Config, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("config: file is empty")
		}
		return nil, fmt.Errorf("config: %w", err)
	}
	c.applyDefaults()
	var resolveErrs []error
	if env != nil {
		resolveErrs = c.resolveSecrets(env)
	}
	if err := errors.Join(append(resolveErrs, c.validate()...)...); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Version == 0 {
		c.Version = 1
	}
	if c.Server.Listen == "" {
		c.Server.Listen = DefaultListen
	}
	if c.Server.MaxBodyBytes == 0 {
		c.Server.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if c.Server.MaxConcurrentWebhooks == 0 {
		c.Server.MaxConcurrentWebhooks = DefaultMaxConcurrent
	}
	if c.Storage.Driver == "" {
		c.Storage.Driver = "sqlite"
	}
	for id, app := range c.Applications {
		if app == nil {
			continue
		}
		app.ID = id
		if app.PayloadFormat == "" {
			app.PayloadFormat = PayloadOriginal
		}
	}
	d := &c.Delivery
	if d.MaxAttempts == 0 {
		d.MaxAttempts = DefaultMaxAttempts
	}
	if d.TimeoutSeconds == 0 {
		d.TimeoutSeconds = DefaultTimeoutSeconds
	}
	if d.WorkerCount == 0 {
		d.WorkerCount = DefaultWorkerCount
	}
	if len(d.RetrySchedule) == 0 {
		d.RetrySchedule = append([]Duration(nil), DefaultRetrySchedule...)
	}
	if c.Retention.PayloadDays == 0 {
		c.Retention.PayloadDays = DefaultPayloadDays
	}
	if c.Retention.AttemptDays == 0 {
		c.Retention.AttemptDays = DefaultAttemptDays
	}
}

func (c *Config) resolveSecrets(env LookupEnv) []error {
	var errs []error
	get := func(field, name string, required bool) string {
		if name == "" {
			if required {
				errs = append(errs, fmt.Errorf("%s: required", field))
			}
			return ""
		}
		v, ok := env(name)
		if !ok || v == "" {
			errs = append(errs, fmt.Errorf("%s: environment variable %s is not set", field, name))
			return ""
		}
		return v
	}
	c.Providers.Paystack.Secret = get("providers.paystack.secret_env", c.Providers.Paystack.SecretEnv, true)
	for _, id := range c.sortedAppIDs() {
		app := c.Applications[id]
		if app == nil {
			continue
		}
		p := "applications." + id + "."
		app.SigningSecret = get(p+"signing_secret_env", app.SigningSecretEnv, true)
		app.PreviousSigningSecret = get(p+"previous_signing_secret_env", app.PreviousSigningSecretEnv, false)
		app.RegistrationToken = get(p+"registration_token_env", app.RegistrationTokenEnv, false)
	}
	return errs
}

// RetryDelay returns the delay before the given attempt (1-based).
func (d DeliveryConfig) RetryDelay(attempt int) time.Duration {
	if attempt < 1 || len(d.RetrySchedule) == 0 {
		return 0
	}
	i := min(attempt-1, len(d.RetrySchedule)-1)
	return time.Duration(d.RetrySchedule[i])
}

// Timeout returns the per-attempt delivery timeout.
func (d DeliveryConfig) Timeout() time.Duration {
	return time.Duration(d.TimeoutSeconds) * time.Second
}

// ParsePrefixes converts IPs and CIDRs (already validated) to prefixes; a
// bare IP becomes a single-address prefix.
func ParsePrefixes(list []string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(list))
	for _, s := range list {
		if a, err := netip.ParseAddr(s); err == nil {
			out = append(out, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
		} else if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p.Masked())
		}
	}
	return out
}
