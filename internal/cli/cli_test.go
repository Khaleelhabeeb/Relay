package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/khaleelhabeeb/relay/config"
	"github.com/khaleelhabeeb/relay/internal/ingest"
	"github.com/khaleelhabeeb/relay/internal/routing"
	"github.com/khaleelhabeeb/relay/internal/store"
	"github.com/khaleelhabeeb/relay/provider/paystack"
)

// fixture writes a config (whose secrets are deliberately NOT in the
// environment) and a database with one matched, one unresolved and one
// exhausted-delivery event.
func setup(t *testing.T) (cfgPath string, ids map[string]string) {
	t.Helper()
	dir := t.TempDir()
	dsn := filepath.Join(dir, "relay.db")
	cfgPath = filepath.Join(dir, "relay.yaml")
	yaml := fmt.Sprintf(`
storage: {dsn: %s}
providers: {paystack: {secret_env: CLI_TEST_UNSET_PAYSTACK}}
applications:
  app_a: {endpoint: "https://app-a.example.com/h", signing_secret_env: CLI_TEST_UNSET_A}
  app_b: {endpoint: "https://uni.example.com/h", signing_secret_env: CLI_TEST_UNSET_B}
routes:
  - {application: app_a, match: {reference_prefix: APPA_}}
  - {application: app_b, match: {reference_prefix: ED_}}
`, dsn)
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// Ingest with an engine that only knows APPA_, so the ED_ event is
	// unresolved until reprocessed with the full config.
	acc := &ingest.StoreAcceptor{Store: st, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Engine: routing.NewEngine(routing.RulesFromConfig([]config.RouteConfig{{Application: "app_a", Match: config.MatchConfig{ReferencePrefix: "APPA_"}}}))}
	ids = map[string]string{}
	for name, body := range map[string]string{
		"matched":    `{"event":"charge.success","data":{"id":1,"reference":"APPA_1"}}`,
		"unresolved": `{"event":"charge.success","data":{"id":2,"reference":"ED_2"}}`,
		"orphan":     `{"event":"charge.success","data":{"id":3,"reference":"ZZ_3"}}`,
	} {
		e, _ := paystack.Parse([]byte(body))
		idents := paystack.ExtractIdentifiers(e)
		a := &ingest.Accepted{Event: e, Identifiers: idents, DedupKey: paystack.DedupKey(e, idents), ReceivedAt: time.Now().UTC()}
		if err := acc.Accept(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		_ = st.WithTx(context.Background(), func(tx *store.Tx) error {
			ids[name], _, err = tx.EventIDByDedupKey(context.Background(), "paystack", a.DedupKey)
			return err
		})
	}
	dels, _ := st.DeliveriesForEvent(context.Background(), ids["matched"])
	ids["delivery"] = dels[0].ID
	// Exhaust the matched event's delivery.
	now := time.Now()
	c, _ := st.ClaimDue(context.Background(), "w", now, now.Add(time.Minute), 1)
	if err := st.CompleteAttempt(context.Background(), store.AttemptResult{DeliveryID: c[0].ID, AttemptID: c[0].AttemptID,
		Owner: "w", FinishedAt: now, HTTPStatus: 500, ErrorCode: "http_status", Status: store.DeliveryExhausted}); err != nil {
		t.Fatal(err)
	}
	return cfgPath, ids
}

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewRoot()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

func TestEventsListAndInspect(t *testing.T) {
	cfg, ids := setup(t)
	out, err := run(t, "events", "list", "-c", cfg, "--status", "unresolved")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, ids["unresolved"]) || strings.Contains(out, ids["matched"]) {
		t.Fatalf("list output:\n%s", out)
	}

	out, err = run(t, "events", "list", "-c", cfg, "--json")
	var page struct {
		Data []map[string]any `json:"data"`
		Next string           `json:"next"`
	}
	if err != nil || json.Unmarshal([]byte(out), &page) != nil || len(page.Data) != 3 || page.Data[0]["id"] == nil || page.Next != "" {
		t.Fatalf("json list: %v\n%s", err, out)
	}

	// Paging: two pages of two and one, newest first, no overlap.
	out, err = run(t, "events", "list", "-c", cfg, "--json", "--limit", "2")
	if err != nil || json.Unmarshal([]byte(out), &page) != nil || len(page.Data) != 2 || page.Next == "" {
		t.Fatalf("first page: %v\n%s", err, out)
	}
	first := page.Data
	page.Next, page.Data = "", nil
	out, err = run(t, "events", "list", "-c", cfg, "--json", "--limit", "2", "--after", first[1]["id"].(string))
	if err != nil || json.Unmarshal([]byte(out), &page) != nil || len(page.Data) != 1 || page.Next != "" ||
		page.Data[0]["id"] == first[0]["id"] || page.Data[0]["id"] == first[1]["id"] {
		t.Fatalf("second page: %v\n%s", err, out)
	}
	if _, err := run(t, "events", "list", "-c", cfg, "--status", "bogus"); err == nil {
		t.Error("unknown status must be rejected")
	}

	out, err = run(t, "stats", "-c", cfg)
	if err != nil || !strings.Contains(out, "2 unresolved, 0 ambiguous") || !strings.Contains(out, "1 exhausted") {
		t.Fatalf("stats: %v\n%s", err, out)
	}

	out, err = run(t, "events", "inspect", "-c", cfg, ids["matched"])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Status       matched", "app_a", "exhausted", "http_status", "sha256"} {
		if !strings.Contains(out, want) {
			t.Errorf("inspect missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, `"reference":"APPA_1"`) {
		t.Error("payload must be withheld without --show-payload")
	}
	if out, _ := run(t, "events", "inspect", "-c", cfg, ids["matched"], "--show-payload"); !strings.Contains(out, `"reference":"APPA_1"`) {
		t.Errorf("--show-payload must print the payload:\n%s", out)
	}
	if _, err := run(t, "events", "inspect", "-c", cfg, "evt_missing"); err == nil {
		t.Error("missing event must error")
	}
}

func TestEventsReprocessAndReroute(t *testing.T) {
	cfg, ids := setup(t)
	out, err := run(t, "events", "reprocess", "-c", cfg)
	if err != nil || !strings.Contains(out, "reprocessed 2 event(s): 1 now matched") {
		t.Fatalf("reprocess: %v\n%s", err, out)
	}
	out, _ = run(t, "events", "inspect", "-c", cfg, ids["unresolved"])
	if !strings.Contains(out, "Status       matched") || !strings.Contains(out, "reprocess") {
		t.Fatalf("after reprocess:\n%s", out)
	}

	if _, err := run(t, "events", "reroute", "-c", cfg, ids["orphan"], "--application", "app_b"); err == nil {
		t.Fatal("reroute without --reason must fail")
	}
	if _, err := run(t, "events", "reroute", "-c", cfg, ids["orphan"], "--application", "nope", "--reason", "x"); err == nil {
		t.Fatal("reroute to unknown application must fail")
	}
	if _, err := run(t, "events", "reroute", "-c", cfg, ids["matched"], "--application", "app_b", "--reason", "x"); err == nil {
		t.Fatal("matched events must not be rerouted")
	}
	out, err = run(t, "events", "reroute", "-c", cfg, ids["orphan"], "--application", "app_b", "--reason", "OPS-42 legacy ref")
	if err != nil {
		t.Fatalf("reroute: %v\n%s", err, out)
	}
	out, _ = run(t, "events", "inspect", "-c", cfg, ids["orphan"])
	if !strings.Contains(out, "operator") || !strings.Contains(out, "OPS-42 legacy ref") || !strings.Contains(out, "app_b") {
		t.Fatalf("reroute audit:\n%s", out)
	}
}

func TestEventsDismiss(t *testing.T) {
	cfg, ids := setup(t)
	if _, err := run(t, "events", "dismiss", "-c", cfg, ids["orphan"]); err == nil {
		t.Fatal("dismiss without --reason must fail")
	}
	out, err := run(t, "events", "dismiss", "-c", cfg, ids["orphan"], "--reason", "not ours")
	if err != nil || !strings.Contains(out, "dismissed") {
		t.Fatalf("dismiss: %v\n%s", err, out)
	}
	out, _ = run(t, "events", "inspect", "-c", cfg, ids["orphan"])
	if !strings.Contains(out, "operator via cli: not ours") {
		t.Fatalf("inspect after dismiss:\n%s", out)
	}
	out, _ = run(t, "events", "list", "-c", cfg, "--held")
	if strings.Contains(out, ids["orphan"]) || !strings.Contains(out, ids["unresolved"]) {
		t.Fatalf("--held:\n%s", out)
	}
	if out, _ := run(t, "stats", "-c", cfg); !strings.Contains(out, "1 unresolved") {
		t.Fatalf("stats after dismiss:\n%s", out)
	}
}

func TestDeliveriesAndRegistrations(t *testing.T) {
	cfg, ids := setup(t)
	out, err := run(t, "deliveries", "list", "-c", cfg, "--status", "exhausted")
	if err != nil || !strings.Contains(out, ids["delivery"]) || !strings.Contains(out, "exhausted") {
		t.Fatalf("deliveries list: %v\n%s", err, out)
	}
	out, err = run(t, "deliveries", "retry", "-c", cfg, ids["delivery"], "del_missing")
	if err == nil || !strings.Contains(out, "queued for redelivery") || !strings.Contains(out, "del_missing: not found") {
		t.Fatalf("retry: %v\n%s", err, out)
	}
	if out, _ := run(t, "deliveries", "list", "-c", cfg, "--status", "pending"); !strings.Contains(out, ids["delivery"]) {
		t.Fatalf("retried delivery must be pending:\n%s", out)
	}
	if out, err := run(t, "registrations", "list", "-c", cfg, "--json"); err != nil || strings.TrimSpace(out) != "[]" {
		t.Fatalf("registrations list: %v %q", err, out)
	}
}
