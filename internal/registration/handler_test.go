package registration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/khaleelhabeeb/relay/config"
	"github.com/khaleelhabeeb/relay/internal/ingest"
	"github.com/khaleelhabeeb/relay/internal/reprocess"
	"github.com/khaleelhabeeb/relay/internal/routing"
	"github.com/khaleelhabeeb/relay/internal/store"
	"github.com/khaleelhabeeb/relay/provider/paystack"
)

var (
	tokenA = strings.Repeat("s", 40)
	tokenB = strings.Repeat("e", 40)
	tokenC = strings.Repeat("c", 40) // app_c owns the APPC_ namespace
)

// clock is a settable time source shared by the handler, reprocessor and ingest.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Set(t time.Time)     { c.mu.Lock(); c.t = t; c.mu.Unlock() }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type env struct {
	t     *testing.T
	h     *Handler
	srv   *httptest.Server
	store *store.Store
	acc   *ingest.StoreAcceptor
	clock *clock
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	clk := &clock{t: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	engine := routing.NewEngine([]routing.Rule{{Name: "routes[0]", Application: "app_a", ReferencePrefix: "APPA_"}}).
		WithNamespaces(map[string][]string{"app_c": {"APPC_"}})
	cfg := &config.Config{Applications: map[string]*config.ApplicationConfig{
		"app_a":    {RegistrationToken: tokenA},
		"app_b":    {RegistrationToken: tokenB},
		"app_c":    {RegistrationToken: tokenC},
		"no_token": {},
	}}
	cfg.Providers.Paystack = &config.PaystackConfig{}
	cfg.Providers.Flutterwave = &config.FlutterwaveConfig{}
	cfg.Providers.Monnify = &config.MonnifyConfig{}
	rp := &reprocess.Reprocessor{Store: st, Engine: engine, Logger: logger, Now: clk.Now}
	h := NewHandler(cfg, st, rp, logger)
	h.now = clk.Now
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &env{t: t, h: h, srv: srv, store: st, clock: clk,
		acc: &ingest.StoreAcceptor{Store: st, Engine: engine, Logger: logger}}
}

type response struct {
	status int
	header http.Header
	body   map[string]any
}

func (e *env) do(method, path, token, body string) response {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := response{status: resp.StatusCode, header: resp.Header}
	_ = json.NewDecoder(resp.Body).Decode(&out.body)
	return out
}

func (r response) code() string {
	if e, ok := r.body["error"].(map[string]any); ok {
		return e["code"].(string)
	}
	return ""
}

func refBody(v string) string {
	return fmt.Sprintf(`{"provider":"paystack","type":"transaction_reference","value":%q}`, v)
}

func dvaBody(bank, acct string) string {
	return fmt.Sprintf(`{"provider":"paystack","type":"dedicated_account","bank":%q,"account_number":%q}`, bank, acct)
}

func TestAuthentication(t *testing.T) {
	e := newEnv(t)
	for name, token := range map[string]string{"missing": "", "wrong": strings.Repeat("x", 40), "prefix of real": tokenA[:20]} {
		r := e.do("GET", "/v1/registrations", token, "")
		if r.status != 401 || r.code() != "unauthorized" || r.header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s token: %d %s", name, r.status, r.code())
		}
	}
	req, _ := http.NewRequest("GET", e.srv.URL+"/v1/registrations", nil)
	req.Header.Set("Authorization", "Basic "+tokenA)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Error("non-bearer scheme must be rejected")
	}
	if r := e.do("GET", "/v1/registrations", tokenA, ""); r.status != 200 {
		t.Errorf("valid token: %d", r.status)
	}
}

func TestReferenceLifecycle(t *testing.T) {
	e := newEnv(t)
	r := e.do("POST", "/v1/registrations", tokenA, refBody("APPA_01JABC"))
	if r.status != 201 || r.body["application"] != "app_a" || r.body["value"] != "APPA_01JABC" || r.body["active"] != true {
		t.Fatalf("create: %d %v", r.status, r.body)
	}
	id := r.body["id"].(string)

	if r := e.do("POST", "/v1/registrations", tokenA, refBody("APPA_01JABC")); r.status != 200 || r.body["id"] != id {
		t.Fatalf("idempotent re-register: %d %v", r.status, r.body)
	}
	if r := e.do("POST", "/v1/registrations", tokenB, refBody("APPA_01JABC")); r.status != 409 || r.code() != "registration_conflict" {
		t.Fatalf("other app: %d %v", r.status, r.body)
	}
	// Token scope: the other app can neither see nor deactivate it.
	if r := e.do("DELETE", "/v1/registrations/"+id, tokenB, ""); r.status != 404 {
		t.Fatalf("cross-app delete: %d", r.status)
	}
	if r := e.do("DELETE", "/v1/registrations/"+id, tokenA, ""); r.status != 200 || r.body["active"] != false || r.body["deactivated_at"] == nil {
		t.Fatalf("delete: %d %v", r.status, r.body)
	}
	if r := e.do("DELETE", "/v1/registrations/"+id, tokenA, ""); r.status != 200 {
		t.Fatalf("repeat delete must be idempotent: %d", r.status)
	}
	// References are never reassignable to another application...
	if r := e.do("POST", "/v1/registrations", tokenB, refBody("APPA_01JABC")); r.status != 409 {
		t.Fatalf("reference reuse by other app after deactivation: %d", r.status)
	}
	// ...but the original owner may claim it again.
	if r := e.do("POST", "/v1/registrations", tokenA, refBody("APPA_01JABC")); r.status != 201 || r.body["id"] == id {
		t.Fatalf("owner re-claim: %d %v", r.status, r.body)
	}
}

// Monnify reserved accounts are dedicated accounts too, registered per
// provider: the same account under paystack is a different claim.
func TestMonnifyReservedAccount(t *testing.T) {
	e := newEnv(t)
	body := `{"provider":"monnify","type":"dedicated_account","bank":"Sterling bank","account_number":"6000140770"}`
	r := e.do("POST", "/v1/registrations", tokenA, body)
	if r.status != 201 || r.body["value"] != "sterling bank:6000140770" || r.body["provider"] != "monnify" {
		t.Fatalf("create: %d %v", r.status, r.body)
	}
	if r := e.do("POST", "/v1/registrations", tokenB, dvaBody("Sterling bank", "6000140770")); r.status != 201 {
		t.Fatalf("same account under paystack: %d %v", r.status, r.body)
	}
}

func TestDedicatedAccountReassignment(t *testing.T) {
	e := newEnv(t)
	r := e.do("POST", "/v1/registrations", tokenA, dvaBody("  Test  BANK ", "993-000-0902"))
	if r.status != 201 || r.body["value"] != "test bank:9930000902" {
		t.Fatalf("create: %d %v", r.status, r.body)
	}
	id := r.body["id"].(string)
	if r := e.do("POST", "/v1/registrations", tokenB, dvaBody("Test Bank", "9930000902")); r.status != 409 {
		t.Fatalf("active conflict: %d", r.status)
	}
	e.do("DELETE", "/v1/registrations/"+id, tokenA, "")
	if r := e.do("POST", "/v1/registrations", tokenB, dvaBody("Test Bank", "9930000902")); r.status != 201 || r.body["application"] != "app_b" {
		t.Fatalf("reassignment after deactivation: %d %v", r.status, r.body)
	}
}

func TestValidation(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name, body string
		status     int
		code       string
	}{
		{"bad provider", `{"provider":"squadco","type":"transaction_reference","value":"X"}`, 422, "validation_failed"},
		{"dva on stripe", `{"provider":"stripe","type":"dedicated_account","bank":"b","account_number":"1"}`, 422, "validation_failed"},
		{"dva on flutterwave", `{"provider":"flutterwave","type":"dedicated_account","bank":"b","account_number":"1"}`, 422, "validation_failed"},
		{"bad type", `{"provider":"paystack","type":"customer","value":"X"}`, 422, "validation_failed"},
		{"empty value", refBody(""), 422, "validation_failed"},
		{"whitespace in value", refBody("SCH 1"), 422, "validation_failed"},
		{"too long", refBody(strings.Repeat("A", 101)), 422, "validation_failed"},
		{"dva via value", `{"provider":"paystack","type":"dedicated_account","value":"x:1"}`, 422, "validation_failed"},
		{"dva missing digits", dvaBody("Test Bank", "abc"), 422, "validation_failed"},
		{"reference with bank", `{"provider":"paystack","type":"transaction_reference","value":"X","bank":"b"}`, 422, "validation_failed"},
		{"unknown field", `{"provider":"paystack","type":"transaction_reference","value":"X","application":"app_b"}`, 400, "invalid_payload"},
		{"two objects", refBody("X") + refBody("Y"), 400, "invalid_payload"},
		{"not json", `nope`, 400, "invalid_payload"},
		{"too large", `{"provider":"` + strings.Repeat("a", 5000) + `"}`, 413, "payload_too_large"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := e.do("POST", "/v1/registrations", tokenA, tc.body)
			if r.status != tc.status || r.code() != tc.code {
				t.Fatalf("got %d %q, want %d %q (%v)", r.status, r.code(), tc.status, tc.code, r.body)
			}
		})
	}
}

func TestListScopeFiltersAndPagination(t *testing.T) {
	e := newEnv(t)
	for i := range 5 {
		e.do("POST", "/v1/registrations", tokenA, refBody(fmt.Sprintf("APPA_%d", i)))
	}
	e.do("POST", "/v1/registrations", tokenA, dvaBody("Test Bank", "111"))
	e.do("POST", "/v1/registrations", tokenB, refBody("ED_1"))

	all := e.do("GET", "/v1/registrations", tokenA, "")
	if n := len(all.body["data"].([]any)); n != 6 {
		t.Fatalf("app_a sees %d registrations, want 6", n)
	}
	if r := e.do("GET", "/v1/registrations?type=dedicated_account", tokenA, ""); len(r.body["data"].([]any)) != 1 {
		t.Fatalf("type filter: %v", r.body)
	}

	var seen []string
	next := ""
	for page := 0; page < 10; page++ {
		path := "/v1/registrations?type=transaction_reference&limit=2"
		if next != "" {
			path += "&after=" + next
		}
		r := e.do("GET", path, tokenA, "")
		for _, d := range r.body["data"].([]any) {
			seen = append(seen, d.(map[string]any)["value"].(string))
		}
		n, _ := r.body["next"].(string)
		if n == "" {
			break
		}
		next = n
	}
	if strings.Join(seen, ",") != "APPA_0,APPA_1,APPA_2,APPA_3,APPA_4" {
		t.Fatalf("paginated = %v", seen)
	}

	id := all.body["data"].([]any)[0].(map[string]any)["id"].(string)
	e.do("DELETE", "/v1/registrations/"+id, tokenA, "")
	if r := e.do("GET", "/v1/registrations?active=false", tokenA, ""); len(r.body["data"].([]any)) != 1 {
		t.Fatalf("active=false: %v", r.body)
	}
	for _, bad := range []string{"?limit=0", "?limit=501", "?active=maybe", "?type=customer"} {
		if r := e.do("GET", "/v1/registrations"+bad, tokenA, ""); r.status != 422 {
			t.Errorf("%s: %d", bad, r.status)
		}
	}
}

func TestRateLimit(t *testing.T) {
	e := newEnv(t)
	e.h.ratePerSec, e.h.burst = 0, 2
	for i := range 2 {
		if r := e.do("GET", "/v1/registrations", tokenA, ""); r.status != 200 {
			t.Fatalf("request %d: %d", i, r.status)
		}
	}
	r := e.do("GET", "/v1/registrations", tokenA, "")
	if r.status != 429 || r.code() != "rate_limited" || r.header.Get("Retry-After") == "" {
		t.Fatalf("over limit: %d %v", r.status, r.body)
	}
	if r := e.do("GET", "/v1/registrations", tokenB, ""); r.status != 200 {
		t.Fatal("limits are per application")
	}
}

// ingestAt stores a webhook received at the clock's current time.
func (e *env) ingestAt(raw []byte) string {
	e.t.Helper()
	evt, err := paystack.Parse(raw)
	if err != nil {
		e.t.Fatal(err)
	}
	ids := paystack.ExtractIdentifiers(evt)
	a := &ingest.Accepted{Event: evt, Identifiers: ids, DedupKey: paystack.DedupKey(evt, ids), ReceivedAt: e.clock.Now()}
	if err := e.acc.Accept(context.Background(), a); err != nil {
		e.t.Fatal(err)
	}
	var id string
	_ = e.store.WithTx(context.Background(), func(tx *store.Tx) error {
		id, _, err = tx.EventIDByDedupKey(context.Background(), "paystack", a.DedupKey)
		return err
	})
	return id
}

func dvaDeposit(id int) []byte {
	return []byte(fmt.Sprintf(`{"event":"charge.success","data":{"id":%d,"reference":"T%d","channel":"dedicated_nuban",
		"authorization":{"channel":"dedicated_nuban","receiver_bank":"Test Bank","receiver_bank_account_number":"9930000902"}}}`, id, id))
}

func (e *env) status(eventID string) (routing.Outcome, string) {
	ev, err := e.store.GetEvent(context.Background(), eventID)
	if err != nil {
		e.t.Fatal(err)
	}
	return ev.RoutingStatus, ev.ApplicationID
}

func TestRegistrationRoutesEarlierWebhooks(t *testing.T) {
	e := newEnv(t)
	dva := e.ingestAt(dvaDeposit(1))
	ref := e.ingestAt([]byte(`{"event":"charge.success","data":{"id":2,"reference":"ED_77"}}`))
	old := e.ingestAt([]byte(`{"event":"charge.success","data":{"id":3,"reference":"ED_OLD"}}`))
	if s, _ := e.status(dva); s != routing.Unresolved {
		t.Fatalf("precondition: %s", s)
	}

	e.clock.Add(10 * time.Minute)
	r := e.do("POST", "/v1/registrations", tokenB, dvaBody("Test Bank", "9930000902"))
	if r.status != 201 || r.body["routed_events"] != float64(1) {
		t.Fatalf("dva registration: %d %v", r.status, r.body)
	}
	if s, app := e.status(dva); s != routing.Matched || app != "app_b" {
		t.Fatalf("dva event after registration: %s %s", s, app)
	}
	dels, _ := e.store.DeliveriesForEvent(context.Background(), dva)
	decs, _ := e.store.RoutingDecisionsForEvent(context.Background(), dva)
	if len(dels) != 1 || len(decs) != 2 || decs[1].DecidedBy != store.DecidedByRegistration {
		t.Fatalf("deliveries %v, decisions %v", dels, decs)
	}

	if r := e.do("POST", "/v1/registrations", tokenB, refBody("ED_77")); r.body["routed_events"] != float64(1) {
		t.Fatalf("reference registration: %v", r.body)
	}
	if s, app := e.status(ref); s != routing.Matched || app != "app_b" {
		t.Fatalf("ref event: %s %s", s, app)
	}

	// Outside the 72h window, events are left for the operator.
	e.clock.Add(73 * time.Hour)
	if r := e.do("POST", "/v1/registrations", tokenB, refBody("ED_OLD")); r.body["routed_events"] != float64(0) {
		t.Fatalf("old event must not auto-route: %v", r.body)
	}
	if s, _ := e.status(old); s != routing.Unresolved {
		t.Fatalf("old event: %s", s)
	}
	// An idempotent repeat does not reprocess again.
	if r := e.do("POST", "/v1/registrations", tokenB, refBody("ED_77")); r.status != 200 || r.body["routed_events"] != float64(0) {
		t.Fatalf("repeat: %d %v", r.status, r.body)
	}
}

func TestDedicatedAccountTimeWindow(t *testing.T) {
	e := newEnv(t)
	r := e.do("POST", "/v1/registrations", tokenA, dvaBody("Test Bank", "9930000902"))
	appAReg := r.body["id"].(string)

	e.clock.Add(time.Minute)
	first := e.ingestAt(dvaDeposit(1)) // owned by app_a

	e.clock.Add(time.Minute)
	e.do("DELETE", "/v1/registrations/"+appAReg, tokenA, "")

	e.clock.Add(time.Minute)
	gap := e.ingestAt(dvaDeposit(2)) // nobody owns the account right now

	e.clock.Add(time.Minute)
	r = e.do("POST", "/v1/registrations", tokenB, dvaBody("Test Bank", "9930000902"))
	if r.status != 201 || r.body["routed_events"] != float64(0) {
		t.Fatalf("app_b claim: %d %v", r.status, r.body)
	}

	e.clock.Add(time.Minute)
	later := e.ingestAt(dvaDeposit(3)) // owned by app_b

	if s, app := e.status(first); s != routing.Matched || app != "app_a" {
		t.Errorf("first deposit: %s %s, want app_a", s, app)
	}
	if s, _ := e.status(gap); s != routing.Unresolved {
		t.Errorf("gap deposit: %s, want unresolved (never handed to the new owner)", s)
	}
	if s, app := e.status(later); s != routing.Matched || app != "app_b" {
		t.Errorf("later deposit: %s %s, want app_b", s, app)
	}
}

func TestRegistrationCannotClaimAnotherApplicationsPrefix(t *testing.T) {
	e := newEnv(t)
	// APPA_ is routed to app_a by a rule; APPC_ is app_c's namespace.
	for _, ref := range []string{"APPA_SHARED", "APPC_1"} {
		if r := e.do("POST", "/v1/registrations", tokenB, refBody(ref)); r.status != 409 || r.code() != "registration_conflict" {
			t.Errorf("app_b registering %s: %d %v, want 409", ref, r.status, r.body)
		}
	}
	// A namespaced application may register only inside its namespace.
	if r := e.do("POST", "/v1/registrations", tokenC, refBody("OTHER_1")); r.status != 422 {
		t.Errorf("app_c outside its namespace: %d %v, want 422", r.status, r.body)
	}
	if r := e.do("POST", "/v1/registrations", tokenC, refBody("APPC_1")); r.status != 201 {
		t.Errorf("app_c inside its namespace: %d %v, want 201", r.status, r.body)
	}
	// Dedicated accounts have no namespace.
	if r := e.do("POST", "/v1/registrations", tokenC, dvaBody("Test Bank", "9930000902")); r.status != 201 {
		t.Errorf("app_c dedicated account: %d %v, want 201", r.status, r.body)
	}
}

// A registration made long after a webhook claims it only inside the
// registering application's own namespace.
func TestRetroactiveClaimNeedsNamespace(t *testing.T) {
	e := newEnv(t)
	foreign := e.ingestAt([]byte(`{"event":"charge.success","data":{"id":1,"reference":"SOMEONES_1"}}`))
	own := e.ingestAt([]byte(`{"event":"charge.success","data":{"id":2,"reference":"APPC_9"}}`))
	dva := e.ingestAt(dvaDeposit(3))

	e.clock.Add(time.Hour)
	if r := e.do("POST", "/v1/registrations", tokenB, refBody("SOMEONES_1")); r.status != 201 || r.body["routed_events"] != float64(0) {
		t.Fatalf("late foreign claim: %d %v", r.status, r.body)
	}
	if s, _ := e.status(foreign); s != routing.Unresolved {
		t.Errorf("foreign event: %s, want unresolved", s)
	}
	if r := e.do("POST", "/v1/registrations", tokenB, dvaBody("Test Bank", "9930000902")); r.body["routed_events"] != float64(0) {
		t.Fatalf("late dva claim: %v", r.body)
	}
	if s, _ := e.status(dva); s != routing.Unresolved {
		t.Errorf("dva event: %s, want unresolved", s)
	}
	if r := e.do("POST", "/v1/registrations", tokenC, refBody("APPC_9")); r.body["routed_events"] != float64(1) {
		t.Fatalf("namespaced claim: %v", r.body)
	}
	if s, app := e.status(own); s != routing.Matched || app != "app_c" {
		t.Errorf("namespaced event: %s %s, want app_c", s, app)
	}
}

func TestConcurrentClaimsOneWinner(t *testing.T) {
	e := newEnv(t)
	e.h.ratePerSec, e.h.burst = 1000, 1000
	var wg sync.WaitGroup
	statuses := make(chan int, 20)
	for i := range 20 {
		token := tokenA
		if i%2 == 1 {
			token = tokenB
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("POST", e.srv.URL+"/v1/registrations", bytes.NewBufferString(refBody("RACE_1")))
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
			statuses <- resp.StatusCode
		}()
	}
	wg.Wait()
	close(statuses)
	count := map[int]int{}
	for s := range statuses {
		count[s]++
	}
	if count[201] != 1 || count[200] != 9 || count[409] != 10 {
		t.Fatalf("status counts = %v, want one 201, nine 200 (same app), ten 409", count)
	}
}
