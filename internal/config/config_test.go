package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func testEnv(overrides map[string]string) LookupEnv {
	env := map[string]string{
		"PAYSTACK_SECRET_KEY":        "sk_test_xxx",
		"FLW_SECRET_HASH":            "flw_hash_xxx",
		"FLUTTERWAVE_SECRET_HASH":    "flw_hash_xxx",
		"APP_A_RELAY_SIGNING_SECRET": strings.Repeat("a", 32),
		"APP_A_RELAY_API_TOKEN":      strings.Repeat("b", 32),
		"APP_B_RELAY_SIGNING_SECRET": strings.Repeat("c", 32),
		"APP_B_RELAY_API_TOKEN":      strings.Repeat("d", 32),
	}
	for k, v := range overrides {
		if v == "" {
			delete(env, k)
		} else {
			env[k] = v
		}
	}
	return func(k string) (string, bool) { v, ok := env[k]; return v, ok }
}

const base = `
storage: {dsn: ./relay.db}
providers: {paystack: {secret_env: PAYSTACK_SECRET_KEY}}
applications:
  app_a:
    endpoint: https://app-a.example.com/hook
    signing_secret_env: APP_A_RELAY_SIGNING_SECRET
    registration_token_env: APP_A_RELAY_API_TOKEN
  app_b:
    endpoint: https://uni.example.com/hook
    signing_secret_env: APP_B_RELAY_SIGNING_SECRET
    registration_token_env: APP_B_RELAY_API_TOKEN
    payload_format: envelope
`

var bothProviders = strings.Replace(base, "providers: {paystack: {secret_env: PAYSTACK_SECRET_KEY}}", "providers: {paystack: {secret_env: PAYSTACK_SECRET_KEY}, flutterwave: {secret_hash_env: FLW_SECRET_HASH}}", 1)

func loadYAML(t *testing.T, yaml string, env LookupEnv) (*Config, error) {
	t.Helper()
	return Load(strings.NewReader(yaml), env)
}

func TestExampleConfigLoads(t *testing.T) {
	f, err := os.Open("../../examples/relay.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c, err := Load(f, testEnv(nil))
	if err != nil {
		t.Fatalf("example config: %v", err)
	}
	if c.Applications["app_b"].PayloadFormat != PayloadEnvelope || c.Applications["app_a"].ID != "app_a" {
		t.Fatalf("unexpected applications: %+v", c.Applications)
	}
	if c.Providers.Paystack.Secret != "sk_test_xxx" {
		t.Fatal("paystack secret not resolved")
	}
}

func TestDefaults(t *testing.T) {
	c, err := loadYAML(t, base, testEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.Listen != ":8080" || c.Server.MaxBodyBytes != 1<<20 || c.Delivery.MaxAttempts != 10 ||
		c.Applications["app_a"].PayloadFormat != PayloadOriginal || c.Retention.PayloadDays != 90 {
		t.Fatalf("defaults not applied: %+v", c)
	}
	if got := c.Delivery.RetryDelay(2); got != 30*time.Second {
		t.Errorf("RetryDelay(2) = %v", got)
	}
	if got := c.Delivery.RetryDelay(15); got != 8*time.Hour {
		t.Errorf("RetryDelay past schedule = %v, want last entry", got)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		env  map[string]string
		want string
	}{
		{"unknown field", base + "bogus: 1\n", nil, "field bogus not found"},
		{"duplicate application id", base + "  app_a:\n    endpoint: https://x.example.com\n", nil, "already defined"},
		{"missing paystack secret", base, map[string]string{"PAYSTACK_SECRET_KEY": ""}, "PAYSTACK_SECRET_KEY is not set"},
		{"missing signing secret", base, map[string]string{"APP_B_RELAY_SIGNING_SECRET": ""}, "APP_B_RELAY_SIGNING_SECRET is not set"},
		{"short signing secret", base, map[string]string{"APP_B_RELAY_SIGNING_SECRET": "short"}, "at least 32 bytes"},
		{"shared signing secret", base, map[string]string{"APP_B_RELAY_SIGNING_SECRET": strings.Repeat("a", 32)}, "each application needs its own"},
		{"shared token", base, map[string]string{"APP_B_RELAY_API_TOKEN": strings.Repeat("b", 32)}, "tokens must identify one application"},
		{"bad payload format", strings.Replace(base, "payload_format: envelope", "payload_format: xml", 1), nil, "payload_format"},
		{"http endpoint", strings.Replace(base, "https://uni.example.com", "http://uni.example.com", 1), nil, "scheme must be https"},
		{"private ip endpoint", strings.Replace(base, "uni.example.com", "10.0.0.5", 1), nil, "non-public address"},
		{"metadata ip endpoint", strings.Replace(base, "uni.example.com", "169.254.169.254", 1), nil, "non-public address"},
		{"localhost endpoint", strings.Replace(base, "uni.example.com", "localhost:9000", 1), nil, "local destination"},
		{"credentials in endpoint", strings.Replace(base, "uni.example.com", "u:p@uni.example.com", 1), nil, "credentials"},
		{"unknown route app", base + "routes: [{application: nope, match: {reference_prefix: X_}}]\n", nil, `unknown application "nope"`},
		{"empty route match", base + "routes: [{application: app_a, match: {}}]\n", nil, "needs reference_prefix"},
		{"overlapping prefixes", base + "routes:\n- {application: app_a, match: {reference_prefix: APPA_}}\n- {application: app_b, match: {reference_prefix: APPA_ED_}}\n", nil, "overlap"},
		{"identical metadata", base + "routes:\n- {application: app_a, match: {metadata: {app: x}}}\n- {application: app_b, match: {metadata: {app: x}}}\n", nil, "overlap"},
		{"prefix implies prefix+metadata", base + "routes:\n- {application: app_a, match: {reference_prefix: S}}\n- {application: app_b, match: {reference_prefix: S, metadata: {app: x}}}\n", nil, "overlap"},
		{"empty application", base + "  x: null\n", nil, "empty definition"},
		{"bad storage driver", strings.Replace(base, "storage: {dsn: ./relay.db}", "storage: {driver: postgres, dsn: x}", 1), nil, "unsupported driver"},
		{"missing dsn", strings.Replace(base, "storage: {dsn: ./relay.db}", "", 1), nil, "storage.dsn"},
		{"bad retry duration", base + "delivery: {retry_schedule: [10parsecs]}\n", nil, "unknown unit"},
		{"max attempts range", base + "delivery: {max_attempts: 500}\n", nil, "max_attempts"},
		{"bad trusted proxy", base + "server: {trusted_proxies: [not-an-ip]}\n", nil, "trusted_proxies"},
		{"api_listen same as listen", base + "server: {listen: ':8080', api_listen: ':8080'}\n", nil, "api_listen"},
		{"unsupported version", "version: 2\n" + base, nil, "unsupported version"},
		{"no providers", strings.Replace(base, "providers: {paystack: {secret_env: PAYSTACK_SECRET_KEY}}", "", 1), nil, "at least one provider"},
		{"missing flutterwave hash", bothProviders, map[string]string{"FLW_SECRET_HASH": ""}, "FLW_SECRET_HASH is not set"},
		{"bad flutterwave allowlist", strings.Replace(base, "providers: {paystack: {secret_env: PAYSTACK_SECRET_KEY}}", "providers: {flutterwave: {secret_hash_env: FLW_SECRET_HASH, allowed_ips: [nope]}}", 1), nil, "providers.flutterwave.allowed_ips"},
		{"route provider not configured", base + "routes: [{provider: flutterwave, application: app_a, match: {reference_prefix: X_}}]\n", nil, `"flutterwave" is not a configured provider`},
		{"unscoped route overlaps scoped", bothProviders + "routes:\n- {provider: flutterwave, application: app_a, match: {reference_prefix: A}}\n- {application: app_b, match: {reference_prefix: A}}\n", nil, "overlap"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadYAML(t, tc.yaml, testEnv(tc.env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestValidationAccepts(t *testing.T) {
	cases := map[string]string{
		"prefix vs metadata (runtime ambiguity only)": base + "routes:\n- {application: app_a, match: {reference_prefix: APPA_}}\n- {application: app_b, match: {metadata: {application: app_b}}}\n",
		"disjoint prefixes":                           base + "routes:\n- {application: app_a, match: {reference_prefix: APPA_}}\n- {application: app_b, match: {reference_prefix: ED_}}\n",
		"same app overlapping":                        base + "routes:\n- {application: app_a, match: {reference_prefix: S}}\n- {application: app_a, match: {reference_prefix: APPA_}}\n",
		"same prefix, different providers":            bothProviders + "routes:\n- {provider: paystack, application: app_a, match: {reference_prefix: A}}\n- {provider: flutterwave, application: app_b, match: {reference_prefix: A}}\n",
		"flutterwave only":                            strings.Replace(base, "providers: {paystack: {secret_env: PAYSTACK_SECRET_KEY}}", "providers: {flutterwave: {secret_hash_env: FLW_SECRET_HASH}}", 1),
		"insecure local opt-in":                       strings.Replace(base, "https://uni.example.com", "http://127.0.0.1:9000", 1) + "delivery: {allow_insecure_local: true}\n",
	}
	for name, y := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := loadYAML(t, y, testEnv(nil)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReportsAllErrors(t *testing.T) {
	_, err := loadYAML(t, base, testEnv(map[string]string{"PAYSTACK_SECRET_KEY": "", "APP_B_RELAY_SIGNING_SECRET": ""}))
	if err == nil || !strings.Contains(err.Error(), "PAYSTACK_SECRET_KEY") || !strings.Contains(err.Error(), "APP_B_RELAY_SIGNING_SECRET") {
		t.Fatalf("expected both errors, got %v", err)
	}
}

func TestLoadWithoutSecrets(t *testing.T) {
	c, err := load(strings.NewReader(base), nil)
	if err != nil {
		t.Fatalf("structure-only load must not need secrets: %v", err)
	}
	if c.Providers.Paystack.Secret != "" || c.Applications["app_a"].SigningSecret != "" {
		t.Fatal("secrets must not be resolved")
	}
	if _, err := load(strings.NewReader(base+"routes: [{application: nope, match: {reference_prefix: X}}]\n"), nil); err == nil {
		t.Fatal("structural validation must still run")
	}
}

func TestParsePrefixes(t *testing.T) {
	got := ParsePrefixes([]string{"52.31.139.75", "10.0.0.0/8", "10.1.2.3/8", "::ffff:1.2.3.4", "2001:db8::/32"})
	want := []string{"52.31.139.75/32", "10.0.0.0/8", "10.0.0.0/8", "1.2.3.4/32", "2001:db8::/32"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Errorf("[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}
