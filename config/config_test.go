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
		"MONNIFY_SECRET_KEY":         "sk_test_monnify_xxx",
		"MONNIFY_API_KEY":            "MK_TEST_xxx",
		"STRIPE_WEBHOOK_SECRET":      "whsec_xxx",
		"STRIPE_WEBHOOK_SECRET_OLD":  "whsec_old",
		"STRIPE_TEST_KEY":            "rk_test_xxx",
		"STRIPE_LIVE_KEY":            "rk_live_xxx",
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

var withMonnify = strings.Replace(base, "providers: {paystack: {secret_env: PAYSTACK_SECRET_KEY}}", "providers: {paystack: {secret_env: PAYSTACK_SECRET_KEY}, monnify: {secret_key_env: MONNIFY_SECRET_KEY}}", 1)

// monnify returns withMonnify with the monnify block's fields replaced.
func monnify(fields string) string {
	return strings.Replace(withMonnify, "monnify: {secret_key_env: MONNIFY_SECRET_KEY}", "monnify: {secret_key_env: MONNIFY_SECRET_KEY, "+fields+"}", 1)
}

var withStripe = strings.Replace(base, "providers: {paystack: {secret_env: PAYSTACK_SECRET_KEY}}", "providers: {stripe: {webhook_secret_envs: [STRIPE_WEBHOOK_SECRET], livemode: false}}", 1)

// stripe returns withStripe with the stripe block's fields replaced.
func stripe(fields string) string {
	return strings.Replace(withStripe, "stripe: {webhook_secret_envs: [STRIPE_WEBHOOK_SECRET], livemode: false}", "stripe: {"+fields+"}", 1)
}

func loadYAML(t *testing.T, yaml string, env LookupEnv) (*Config, error) {
	t.Helper()
	return Load(strings.NewReader(yaml), env)
}

func TestExampleConfigLoads(t *testing.T) {
	f, err := os.Open("../examples/relay.yaml")
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
	if c.Delivery.MaxConcurrentPerApplication != 4 {
		t.Errorf("max_concurrent_per_application default with two applications = %d, want half of 8",
			c.Delivery.MaxConcurrentPerApplication)
	}
	if got := c.Delivery.RetryDelay(2); got != 30*time.Second {
		t.Errorf("RetryDelay(2) = %v", got)
	}
	if got := c.Delivery.RetryDelay(15); got != 8*time.Hour {
		t.Errorf("RetryDelay past schedule = %v, want last entry", got)
	}
}

func TestAdminToken(t *testing.T) {
	c, err := loadYAML(t, base+"server: {admin_token_env: RELAY_ADMIN_TOKEN}\n", testEnv(map[string]string{"RELAY_ADMIN_TOKEN": strings.Repeat("z", 32)}))
	if err != nil || c.Server.AdminToken != strings.Repeat("z", 32) {
		t.Fatalf("admin token not resolved: %v", err)
	}
	if c, err := loadYAML(t, base, testEnv(nil)); err != nil || c.Server.AdminToken != "" {
		t.Fatalf("admin API must be off by default: %v", err)
	}
}

func TestAlertsConfig(t *testing.T) {
	c, err := loadYAML(t, base, testEnv(nil))
	if err != nil || c.Alerts.URL != "" || c.Alerts.Reminder() != DefaultAlertReminderInterval ||
		time.Duration(c.Alerts.DigestInterval) != DefaultAlertDigestInterval || c.Retention.HeldDays != 0 {
		t.Fatalf("alerts must be off by default: %+v %v", c.Alerts, err)
	}
	c, err = loadYAML(t, base+"alerts: {url_env: ALERTS_URL, format: slack, reminder_interval: 0s}\nretention: {held_days: 30}\n",
		testEnv(map[string]string{"ALERTS_URL": "https://hooks.slack.com/services/x"}))
	if err != nil || c.Alerts.URL != "https://hooks.slack.com/services/x" || c.Alerts.Reminder() != 0 || c.Retention.HeldDays != 30 {
		t.Fatalf("slack alerts: %+v %v", c.Alerts, err)
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
		{"per-application cap above workers", base + "delivery: {worker_count: 4, max_concurrent_per_application: 5}\n", nil, "max_concurrent_per_application"},
		{"bad trusted proxy", base + "server: {trusted_proxies: [not-an-ip]}\n", nil, "trusted_proxies"},
		{"api_listen same as listen", base + "server: {listen: ':8080', api_listen: ':8080'}\n", nil, "api_listen"},
		{"alerts url unset", base + "alerts: {url_env: RELAY_ALERTS_URL, secret_env: RELAY_ALERTS_SECRET}\n", nil, "RELAY_ALERTS_URL is not set"},
		{"alerts json without secret", base + "alerts: {url_env: ALERTS_URL}\n", map[string]string{"ALERTS_URL": "https://alerts.example.com/h"}, "alerts.secret_env: required"},
		{"alerts short secret", base + "alerts: {url_env: ALERTS_URL, secret_env: ALERTS_SECRET}\n", map[string]string{"ALERTS_URL": "https://alerts.example.com/h", "ALERTS_SECRET": "short"}, "alerts.secret_env: secret must be"},
		{"alerts slack with secret", base + "alerts: {url_env: ALERTS_URL, format: slack, secret_env: X}\n", map[string]string{"ALERTS_URL": "https://hooks.slack.com/services/x"}, "slack format is not signed"},
		{"alerts http url", base + "alerts: {url_env: ALERTS_URL, format: slack}\n", map[string]string{"ALERTS_URL": "http://alerts.example.com"}, "alerts.url_env: scheme must be https"},
		{"alerts bad format", base + "alerts: {format: email}\n", nil, "alerts.format"},
		{"alerts digest too fast", base + "alerts: {digest_interval: 1s}\n", nil, "alerts.digest_interval"},
		{"alerts reminder too fast", base + "alerts: {reminder_interval: 5m}\n", nil, "alerts.reminder_interval"},
		{"negative held days", base + "retention: {held_days: -1}\n", nil, "retention.held_days"},
		{"unset admin token", base + "server: {admin_token_env: RELAY_ADMIN_TOKEN}\n", nil, "RELAY_ADMIN_TOKEN is not set"},
		{"short admin token", base + "server: {admin_token_env: RELAY_ADMIN_TOKEN}\n", map[string]string{"RELAY_ADMIN_TOKEN": "short"}, "server.admin_token_env: token must be at least 32 bytes"},
		{"admin token reuses registration token", base + "server: {admin_token_env: APP_A_RELAY_API_TOKEN}\n", nil, "the admin token must be its own"},
		{"unsupported version", "version: 2\n" + base, nil, "unsupported version"},
		{"no providers", strings.Replace(base, "providers: {paystack: {secret_env: PAYSTACK_SECRET_KEY}}", "", 1), nil, "at least one provider"},
		{"missing flutterwave hash", bothProviders, map[string]string{"FLW_SECRET_HASH": ""}, "FLW_SECRET_HASH is not set"},
		{"bad flutterwave allowlist", strings.Replace(base, "providers: {paystack: {secret_env: PAYSTACK_SECRET_KEY}}", "providers: {flutterwave: {secret_hash_env: FLW_SECRET_HASH, allowed_ips: [nope]}}", 1), nil, "providers.flutterwave.allowed_ips"},
		{"route provider not configured", base + "routes: [{provider: flutterwave, application: app_a, match: {reference_prefix: X_}}]\n", nil, `"flutterwave" is not a configured provider`},
		{"namespaces overlap", strings.Replace(strings.Replace(base, "    registration_token_env: APP_A_RELAY_API_TOKEN\n", "    registration_token_env: APP_A_RELAY_API_TOKEN\n    reference_prefixes: [APPA_]\n", 1), "    payload_format: envelope\n", "    payload_format: envelope\n    reference_prefixes: [APPA_X]\n", 1), nil, "overlap"},
		{"route overlaps namespace", strings.Replace(base, "    registration_token_env: APP_A_RELAY_API_TOKEN\n", "    registration_token_env: APP_A_RELAY_API_TOKEN\n    reference_prefixes: [APPA_]\n", 1) + "routes: [{application: app_b, match: {reference_prefix: APP}}]\n", nil, "overlapping"},
		{"blank namespace prefix", strings.Replace(base, "    payload_format: envelope\n", "    payload_format: envelope\n    reference_prefixes: [' ']\n", 1), nil, "reference_prefixes"},
		{"flutterwave confirm without secret key", strings.Replace(bothProviders, "flutterwave: {secret_hash_env: FLW_SECRET_HASH}", "flutterwave: {secret_hash_env: FLW_SECRET_HASH, confirm_with_api: true}", 1), nil, "secret_key_env: required"},
		{"flutterwave confirm with unset secret key", strings.Replace(bothProviders, "flutterwave: {secret_hash_env: FLW_SECRET_HASH}", "flutterwave: {secret_hash_env: FLW_SECRET_HASH, confirm_with_api: true, secret_key_env: FLW_SECRET_KEY}", 1), nil, "FLW_SECRET_KEY is not set"},
		{"missing monnify secret key", withMonnify, map[string]string{"MONNIFY_SECRET_KEY": ""}, "MONNIFY_SECRET_KEY is not set"},
		{"monnify confirm without api key", monnify("confirm_with_api: true"), nil, "providers.monnify.api_key_env: required"},
		{"monnify confirm with unset api key", monnify("confirm_with_api: true, api_key_env: NOPE"), nil, "NOPE is not set"},
		{"monnify http api url", monnify("api_base_url: 'http://sandbox.monnify.com'"), nil, "must be an absolute https URL"},
		{"monnify bad allowlist", monnify("allowed_ips: [nope]"), nil, "providers.monnify.allowed_ips"},
		{"unsigned sandbox without confirm", monnify("allow_unsigned_sandbox: true, api_base_url: 'https://sandbox.monnify.com'"), nil, "requires confirm_with_api"},
		{"unsigned sandbox against live", monnify("allow_unsigned_sandbox: true, confirm_with_api: true, api_key_env: MONNIFY_API_KEY"), nil, "never enable it against the live API"},
		{"unsigned sandbox against lookalike", monnify("allow_unsigned_sandbox: true, confirm_with_api: true, api_key_env: MONNIFY_API_KEY, api_base_url: 'https://sandbox.monnify.com.evil.example'"), nil, "never enable it against the live API"},
		{"stripe without secrets", stripe("livemode: false"), nil, "providers.stripe.webhook_secret_envs: required"},
		{"stripe unset secret", stripe("webhook_secret_envs: [NOPE], livemode: false"), nil, "NOPE is not set"},
		{"stripe without livemode", stripe("webhook_secret_envs: [STRIPE_WEBHOOK_SECRET]"), nil, "providers.stripe.livemode: required"},
		{"stripe confirm without api key", stripe("webhook_secret_envs: [STRIPE_WEBHOOK_SECRET], livemode: false, confirm_with_api: true"), nil, "providers.stripe.api_key_env: required"},
		{"stripe live key in test mode", stripe("webhook_secret_envs: [STRIPE_WEBHOOK_SECRET], livemode: false, confirm_with_api: true, api_key_env: STRIPE_LIVE_KEY"), nil, "is a live key but livemode is false"},
		{"stripe test key in live mode", stripe("webhook_secret_envs: [STRIPE_WEBHOOK_SECRET], livemode: true, confirm_with_api: true, api_key_env: STRIPE_TEST_KEY"), nil, "is a test-mode key but livemode is true"},
		{"stripe tolerance too long", stripe("webhook_secret_envs: [STRIPE_WEBHOOK_SECRET], livemode: false, tolerance: 1h"), nil, "providers.stripe.tolerance"},
		{"stripe http api url", stripe("webhook_secret_envs: [STRIPE_WEBHOOK_SECRET], livemode: false, api_base_url: 'http://api.stripe.com'"), nil, "providers.stripe.api_base_url"},
		{"stripe bad allowlist", stripe("webhook_secret_envs: [STRIPE_WEBHOOK_SECRET], livemode: false, allowed_ips: [nope]"), nil, "providers.stripe.allowed_ips"},
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
		"namespace with own route":                    strings.Replace(base, "    registration_token_env: APP_A_RELAY_API_TOKEN\n", "    registration_token_env: APP_A_RELAY_API_TOKEN\n    reference_prefixes: [APPA_]\n", 1) + "routes: [{application: app_a, match: {reference_prefix: APPA_}}]\n",
		"confirm with api":                            strings.Replace(bothProviders, "providers: {paystack: {secret_env: PAYSTACK_SECRET_KEY}, flutterwave: {secret_hash_env: FLW_SECRET_HASH}}", "providers: {paystack: {secret_env: PAYSTACK_SECRET_KEY, confirm_with_api: true}, flutterwave: {secret_hash_env: FLW_SECRET_HASH, confirm_with_api: true, secret_key_env: PAYSTACK_SECRET_KEY}}", 1),
		"flutterwave only":                            strings.Replace(base, "providers: {paystack: {secret_env: PAYSTACK_SECRET_KEY}}", "providers: {flutterwave: {secret_hash_env: FLW_SECRET_HASH}}", 1),
		"monnify":                                     withMonnify,
		"monnify confirm with api":                    monnify("confirm_with_api: true, api_key_env: MONNIFY_API_KEY"),
		"monnify unsigned sandbox":                    monnify("confirm_with_api: true, api_key_env: MONNIFY_API_KEY, allow_unsigned_sandbox: true, api_base_url: 'https://sandbox.monnify.com'"),
		"stripe":                                      withStripe,
		"stripe two secrets, live, confirm":           stripe("webhook_secret_envs: [STRIPE_WEBHOOK_SECRET, STRIPE_WEBHOOK_SECRET_OLD], livemode: true, confirm_with_api: true, api_key_env: STRIPE_LIVE_KEY, tolerance: 10m"),
		"stripe test confirm":                         stripe("webhook_secret_envs: [STRIPE_WEBHOOK_SECRET], livemode: false, confirm_with_api: true, api_key_env: STRIPE_TEST_KEY"),
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

func TestExtraProviders(t *testing.T) {
	y := base + "routes: [{provider: acme, application: app_a, match: {reference_prefix: A_}}]\n"
	if _, err := loadYAML(t, y, testEnv(nil)); err == nil {
		t.Fatal("route to an unknown provider was accepted")
	}
	if _, err := Load(strings.NewReader(y), testEnv(nil), "acme"); err != nil {
		t.Fatalf("declared extra provider: %v", err)
	}
	if _, err := load(strings.NewReader(y), nil, nil); err != nil {
		t.Fatalf("without secrets, routes may name any provider: %v", err)
	}
	noProviders := strings.Replace(base, "providers: {paystack: {secret_env: PAYSTACK_SECRET_KEY}}", "", 1)
	if _, err := Load(strings.NewReader(noProviders), testEnv(nil), "acme"); err != nil {
		t.Fatalf("custom provider alone satisfies the provider requirement: %v", err)
	}
}

func TestValidateGoConfig(t *testing.T) {
	c := &Config{}
	c.Storage.DSN = "./relay.db"
	c.Providers.Flutterwave = &FlutterwaveConfig{}
	c.Applications = map[string]*ApplicationConfig{"app_a": {Endpoint: "https://a.example.com/hook"}}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "SecretHash is required") || !strings.Contains(err.Error(), "applications.app_a: SigningSecret is required") {
		t.Fatalf("got %v", err)
	}
	c.Providers.Flutterwave.SecretHash = "h"
	c.Providers.Monnify = &MonnifyConfig{ConfirmWithAPI: true, SecretKey: "s"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "APIKey is required") {
		t.Fatalf("monnify without api key: %v", err)
	}
	c.Providers.Monnify.APIKey = "k"
	live := true
	c.Providers.Stripe = &StripeConfig{Livemode: &live, WebhookSecrets: []string{""}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "WebhookSecrets is required") {
		t.Fatalf("stripe without secret: %v", err)
	}
	c.Providers.Stripe.WebhookSecrets = []string{"whsec_x"}
	c.Applications["app_a"].SigningSecret = strings.Repeat("s", 32)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Applications["app_a"].ID != "app_a" || c.Applications["app_a"].PayloadFormat != PayloadOriginal || c.Delivery.MaxAttempts != DefaultMaxAttempts {
		t.Fatalf("defaults not applied: %+v %+v", c.Applications["app_a"], c.Delivery)
	}
}

func TestLoadWithoutSecrets(t *testing.T) {
	c, err := load(strings.NewReader(base), nil, nil)
	if err != nil {
		t.Fatalf("structure-only load must not need secrets: %v", err)
	}
	if c.Providers.Paystack.Secret != "" || c.Applications["app_a"].SigningSecret != "" {
		t.Fatal("secrets must not be resolved")
	}
	if _, err := load(strings.NewReader(base+"routes: [{application: nope, match: {reference_prefix: X}}]\n"), nil, nil); err == nil {
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

func TestPerApplicationCapDefaultsToAllWorkersForOneApplication(t *testing.T) {
	one := base[:strings.Index(base, "  app_b:")]
	c, err := loadYAML(t, one+"delivery: {worker_count: 6}\n", testEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Delivery.MaxConcurrentPerApplication != 6 {
		t.Fatalf("one application: cap = %d, want worker_count", c.Delivery.MaxConcurrentPerApplication)
	}
}
