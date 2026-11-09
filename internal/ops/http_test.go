package ops

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/khaleelhabeeb/relay/admin"
	"github.com/khaleelhabeeb/relay/config"
	"github.com/khaleelhabeeb/relay/internal/ingest"
	"github.com/khaleelhabeeb/relay/internal/reprocess"
	"github.com/khaleelhabeeb/relay/internal/routing"
	"github.com/khaleelhabeeb/relay/internal/store"
	"github.com/khaleelhabeeb/relay/provider/paystack"
)

const token = "admin-token-0123456789-0123456789-abcdef"

type fixture struct {
	t       *testing.T
	st      *store.Store
	svc     *Service
	h       http.Handler
	ids     map[string]string
	nudges  int
	confirm int
}

// newFixture stores one matched (APPA_1) and two unresolved (ZZ_2, ZZ_3)
// Paystack events.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := routing.NewEngine(routing.RulesFromConfig([]config.RouteConfig{
		{Application: "app_a", Match: config.MatchConfig{ReferencePrefix: "APPA_"}}}))
	acc := &ingest.StoreAcceptor{Store: st, Logger: logger, Engine: engine}
	f := &fixture{t: t, st: st, ids: map[string]string{}}
	base := time.Now().UTC().Add(-time.Hour)
	for i, ref := range []string{"APPA_1", "ZZ_2", "ZZ_3"} {
		e, _ := paystack.Parse([]byte(`{"event":"charge.success","data":{"id":` + string(rune('1'+i)) + `,"reference":"` + ref + `"}}`))
		idents := paystack.ExtractIdentifiers(e)
		a := &ingest.Accepted{Event: e, Identifiers: idents, DedupKey: paystack.DedupKey(e, idents),
			ReceivedAt: base.Add(time.Duration(i) * time.Minute)}
		if err := acc.Accept(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		_ = st.WithTx(context.Background(), func(tx *store.Tx) error {
			f.ids[ref], _, err = tx.EventIDByDedupKey(context.Background(), "paystack", a.DedupKey)
			return err
		})
	}
	f.svc = &Service{
		Store:        st,
		Reprocessor:  &reprocess.Reprocessor{Store: st, Engine: engine, Logger: logger},
		Applications: []string{"app_a", "app_b"},
		OnDelivery:   func() { f.nudges++ },
		OnConfirm:    func() { f.confirm++ },
	}
	f.h = NewHandler(f.svc, token, logger)
	return f
}

type response struct {
	status int
	body   map[string]any
	raw    string
}

func (r response) code() string {
	e, _ := r.body["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func (f *fixture) do(method, path, tok, body string) response {
	f.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	out := response{status: rec.Code, raw: rec.Body.String()}
	_ = json.Unmarshal(rec.Body.Bytes(), &out.body)
	return out
}

func TestAdminDisabledWithoutToken(t *testing.T) {
	f := newFixture(t)
	h := NewHandler(f.svc, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/admin/stats", nil)
	req.Header.Set("Authorization", "Bearer ")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "admin_disabled") {
		t.Fatalf("disabled admin API: %d %s", rec.Code, rec.Body)
	}
}

func TestAdminAuthentication(t *testing.T) {
	f := newFixture(t)
	for _, tok := range []string{"", "wrong", token + "x"} {
		if r := f.do("GET", "/v1/admin/stats", tok, ""); r.status != http.StatusUnauthorized || r.code() != "unauthorized" {
			t.Errorf("token %q: %d %s", tok, r.status, r.raw)
		}
	}
	if r := f.do("GET", "/v1/admin/stats", token, ""); r.status != http.StatusOK {
		t.Fatalf("stats: %d %s", r.status, r.raw)
	}
	if r := f.do("GET", "/v1/admin/nope", token, ""); r.status != http.StatusNotFound {
		t.Errorf("unknown endpoint: %d", r.status)
	}
}

func TestAdminListAndInspect(t *testing.T) {
	f := newFixture(t)
	r := f.do("GET", "/v1/admin/events?status=unresolved", token, "")
	data, _ := r.body["data"].([]any)
	if r.status != http.StatusOK || len(data) != 2 {
		t.Fatalf("list unresolved: %d %s", r.status, r.raw)
	}
	// Newest first.
	if data[0].(map[string]any)["id"] != f.ids["ZZ_3"] {
		t.Errorf("order: %s", r.raw)
	}

	r = f.do("GET", "/v1/admin/events?limit=1", token, "")
	if next, _ := r.body["next"].(string); next != f.ids["ZZ_3"] {
		t.Fatalf("first page next: %s", r.raw)
	}
	r = f.do("GET", "/v1/admin/events?limit=5&after="+f.ids["ZZ_3"], token, "")
	if data, _ := r.body["data"].([]any); len(data) != 2 || r.body["next"] != nil {
		t.Fatalf("second page: %s", r.raw)
	}

	for _, q := range []string{"status=bogus", "limit=0", "limit=501", "since=yesterday", "after=evt_missing", "confirmation=x"} {
		if r := f.do("GET", "/v1/admin/events?"+q, token, ""); r.status != http.StatusUnprocessableEntity || r.code() != "validation_failed" {
			t.Errorf("%s: %d %s", q, r.status, r.raw)
		}
	}
	since := time.Now().UTC().Add(-90 * time.Second).Format(time.RFC3339)
	if r := f.do("GET", "/v1/admin/events?since="+since, token, ""); len(r.body["data"].([]any)) != 0 {
		t.Errorf("since filter: %s", r.raw)
	}
	if r := f.do("GET", "/v1/admin/events?application=app_a", token, ""); len(r.body["data"].([]any)) != 1 {
		t.Errorf("application filter: %s", r.raw)
	}

	r = f.do("GET", "/v1/admin/events/"+f.ids["APPA_1"], token, "")
	if r.status != http.StatusOK || r.body["payload"] != nil || len(r.body["deliveries"].([]any)) != 1 ||
		len(r.body["routing_decisions"].([]any)) != 1 {
		t.Fatalf("inspect: %d %s", r.status, r.raw)
	}
	r = f.do("GET", "/v1/admin/events/"+f.ids["APPA_1"]+"?include_payload=true", token, "")
	if !strings.Contains(r.raw, `"reference":"APPA_1"}`) {
		t.Fatalf("payload not included: %s", r.raw)
	}
	if r := f.do("GET", "/v1/admin/events/evt_missing", token, ""); r.status != http.StatusNotFound || r.code() != "not_found" {
		t.Errorf("missing event: %d %s", r.status, r.raw)
	}
}

func TestAdminReroute(t *testing.T) {
	f := newFixture(t)
	path := "/v1/admin/events/" + f.ids["ZZ_2"] + "/reroute"
	for body, want := range map[string]int{
		`{"application":"app_b"}`:                       http.StatusUnprocessableEntity,
		`{"application":"app_b","reason":"   "}`:        http.StatusUnprocessableEntity,
		`{"application":"nope","reason":"x"}`:           http.StatusUnprocessableEntity,
		`{"application":"app_b","reason":"x","more":1}`: http.StatusBadRequest,
		`not json`: http.StatusBadRequest,
	} {
		if r := f.do("POST", path, token, body); r.status != want {
			t.Errorf("%s: %d, want %d: %s", body, r.status, want, r.raw)
		}
	}
	if r := f.do("POST", "/v1/admin/events/"+f.ids["APPA_1"]+"/reroute", token, `{"application":"app_b","reason":"x"}`); r.status != http.StatusConflict || r.code() != "invalid_state" {
		t.Errorf("matched event: %d %s", r.status, r.raw)
	}
	if r := f.do("POST", "/v1/admin/events/evt_missing/reroute", token, `{"application":"app_b","reason":"x"}`); r.status != http.StatusNotFound {
		t.Errorf("missing event: %d %s", r.status, r.raw)
	}

	r := f.do("POST", path, token, `{"application":"app_b","reason":"OPS-7 legacy reference"}`)
	ev, _ := r.body["event"].(map[string]any)
	if r.status != http.StatusOK || ev["routing_status"] != "matched" || ev["application_id"] != "app_b" || r.body["awaiting_confirmation"] != false {
		t.Fatalf("reroute: %d %s", r.status, r.raw)
	}
	d, err := f.svc.GetEvent(context.Background(), f.ids["ZZ_2"], false)
	if err != nil {
		t.Fatal(err)
	}
	last := d.RoutingDecisions[len(d.RoutingDecisions)-1]
	if last.DecidedBy != "operator" || !strings.Contains(last.Reason, "via admin api: OPS-7 legacy reference") || len(d.Deliveries) != 1 {
		t.Fatalf("audit trail: %+v", d)
	}
	if r := f.do("POST", path, token, `{"application":"app_b","reason":"again"}`); r.status != http.StatusConflict {
		t.Errorf("second reroute: %d %s", r.status, r.raw)
	}
}

func TestAdminReprocessConfirmAndDeliveries(t *testing.T) {
	f := newFixture(t)
	r := f.do("POST", "/v1/admin/events/reprocess", token, `{"event_ids":["`+f.ids["ZZ_2"]+`","evt_missing"]}`)
	if r.status != http.StatusOK || r.body["considered"] != 2.0 || r.body["unresolved"] != 1.0 || r.body["failed"] != 1.0 {
		t.Fatalf("reprocess ids: %d %s", r.status, r.raw)
	}
	if r := f.do("POST", "/v1/admin/events/reprocess", token, ""); r.status != http.StatusOK || r.body["considered"] != 2.0 {
		t.Fatalf("reprocess all: %d %s", r.status, r.raw)
	}
	if r := f.do("POST", "/v1/admin/events/reprocess", token, `{"event_ids":["x"],"since":"2026-01-01T00:00:00Z"}`); r.status != http.StatusUnprocessableEntity {
		t.Errorf("ids and since: %d %s", r.status, r.raw)
	}

	if r := f.do("POST", "/v1/admin/events/"+f.ids["APPA_1"]+"/confirm", token, ""); r.status != http.StatusConflict || f.confirm != 0 {
		t.Errorf("confirm without confirmation: %d %s", r.status, r.raw)
	}

	r = f.do("GET", "/v1/admin/deliveries?event="+f.ids["APPA_1"], token, "")
	data, _ := r.body["data"].([]any)
	if r.status != http.StatusOK || len(data) != 1 {
		t.Fatalf("deliveries: %d %s", r.status, r.raw)
	}
	id := data[0].(map[string]any)["id"].(string)
	if r := f.do("POST", "/v1/admin/deliveries/"+id+"/retry", token, ""); r.status != http.StatusConflict {
		t.Errorf("retry pending delivery: %d %s", r.status, r.raw)
	}
	if r := f.do("POST", "/v1/admin/deliveries/del_missing/retry", token, ""); r.status != http.StatusNotFound {
		t.Errorf("retry missing: %d %s", r.status, r.raw)
	}
	if r := f.do("GET", "/v1/admin/deliveries?status=lost", token, ""); r.status != http.StatusUnprocessableEntity {
		t.Errorf("bad delivery status: %d", r.status)
	}

	// Exhaust it, then retry.
	now := time.Now()
	c, err := f.st.ClaimDue(context.Background(), "w", now, now.Add(time.Minute), 1)
	if err != nil || len(c) != 1 {
		t.Fatal(c, err)
	}
	if err := f.st.CompleteAttempt(context.Background(), store.AttemptResult{DeliveryID: c[0].ID, AttemptID: c[0].AttemptID,
		Owner: "w", FinishedAt: now, HTTPStatus: 500, ErrorCode: "http_status", Status: store.DeliveryExhausted}); err != nil {
		t.Fatal(err)
	}
	st, _ := f.svc.Stats(context.Background())
	if st.Deliveries.Exhausted != 1 || st.Events.Unresolved != 2 || st.Events.OldestHeldAt.IsZero() {
		t.Fatalf("stats: %+v", st)
	}
	if r := f.do("POST", "/v1/admin/deliveries/"+id+"/retry", token, ""); r.status != http.StatusAccepted || f.nudges != 1 {
		t.Fatalf("retry exhausted: %d %s (nudges %d)", r.status, r.raw, f.nudges)
	}
	dels, _ := f.svc.ListDeliveries(context.Background(), admin.DeliveryFilter{Status: admin.DeliveryPending})
	if len(dels) != 1 || dels[0].ID != id {
		t.Fatalf("after retry: %+v", dels)
	}
}

func TestAdminDismiss(t *testing.T) {
	f := newFixture(t)
	path := "/v1/admin/events/" + f.ids["ZZ_2"] + "/dismiss"
	if r := f.do("POST", path, token, `{}`); r.status != http.StatusUnprocessableEntity {
		t.Errorf("dismiss without reason: %d %s", r.status, r.raw)
	}
	if r := f.do("POST", "/v1/admin/events/"+f.ids["APPA_1"]+"/dismiss", token, `{"reason":"x"}`); r.status != http.StatusConflict {
		t.Errorf("dismiss matched event: %d %s", r.status, r.raw)
	}
	r := f.do("POST", path, token, `{"reason":"test payment from the provider dashboard"}`)
	if r.status != http.StatusOK || r.body["dismissed_at"] == nil || r.body["routing_status"] != "unresolved" {
		t.Fatalf("dismiss: %d %s", r.status, r.raw)
	}
	if r := f.do("POST", path, token, `{"reason":"again"}`); r.status != http.StatusConflict {
		t.Errorf("second dismiss: %d %s", r.status, r.raw)
	}

	// No longer held: out of stats, the held filter and bulk reprocess.
	if r := f.do("GET", "/v1/admin/events?held=true", token, ""); len(r.body["data"].([]any)) != 1 {
		t.Errorf("held filter: %s", r.raw)
	}
	if st, _ := f.svc.Stats(context.Background()); st.Events.Unresolved != 1 {
		t.Errorf("stats count a dismissed event: %+v", st)
	}
	if r := f.do("POST", "/v1/admin/events/reprocess", token, "{}"); r.body["considered"] != 1.0 {
		t.Errorf("bulk reprocess must skip dismissed events: %s", r.raw)
	}
	d, _ := f.svc.GetEvent(context.Background(), f.ids["ZZ_2"], false)
	if d.DismissedBy != "operator via admin api" || d.DismissReason != "test payment from the provider dashboard" {
		t.Fatalf("dismissal not recorded: %+v", d)
	}

	// An operator can still reroute it, which clears the dismissal.
	r = f.do("POST", "/v1/admin/events/"+f.ids["ZZ_2"]+"/reroute", token, `{"application":"app_b","reason":"found the owner"}`)
	if r.status != http.StatusOK || r.body["event"].(map[string]any)["dismissed_at"] != nil {
		t.Fatalf("reroute after dismiss: %d %s", r.status, r.raw)
	}
}
