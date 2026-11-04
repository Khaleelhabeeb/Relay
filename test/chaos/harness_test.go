//go:build chaos

// Package chaos runs the real relay binary under failure: SIGKILL during
// ingest and delivery, a flaky receiver, and a sender that behaves like
// Paystack (resends until it gets a 200). Run with `make chaos`.
package chaos

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/khaleelhabeeb/relay/internal/store"
	"github.com/khaleelhabeeb/relay/provider/paystack"
)

const (
	paystackSecret = "sk_test_chaos_harness"
	appSecret      = "whsec_chaos_0123456789abcdef0123456789abcdef"
)

var relayBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "relay-chaos-bin")
	if err != nil {
		panic(err)
	}
	relayBin = filepath.Join(dir, "relay")
	build := exec.Command("go", "build", "-o", relayBin, "github.com/khaleelhabeeb/relay/cmd/relay")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// relayProc manages one relay installation (config + database) across
// process restarts.
type relayProc struct {
	t       *testing.T
	dir     string
	cfgPath string
	dsn     string
	url     string
	cmd     *exec.Cmd
	log     *os.File
	kills   int
}

func newRelay(t *testing.T, receiverURL string, extraDelivery string) *relayProc {
	t.Helper()
	dir := t.TempDir()
	port := freePort(t)
	r := &relayProc{t: t, dir: dir, cfgPath: filepath.Join(dir, "relay.yaml"), dsn: filepath.Join(dir, "relay.db"),
		url: fmt.Sprintf("http://127.0.0.1:%d", port)}
	cfg := fmt.Sprintf(`
server: {listen: "127.0.0.1:%d", max_concurrent_webhooks: 1000}
storage: {dsn: %s}
providers: {paystack: {secret_env: CHAOS_PAYSTACK}}
applications:
  app: {endpoint: %q, signing_secret_env: CHAOS_APP}
routes: [{application: app, match: {reference_prefix: "CH_"}}]
delivery: {allow_insecure_local: true, timeout_seconds: 2%s}
`, port, r.dsn, receiverURL, extraDelivery)
	if err := os.WriteFile(r.cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var err error
	if r.log, err = os.Create(filepath.Join(dir, "relay.log")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		r.stop()
		r.log.Close()
		if t.Failed() {
			b, _ := os.ReadFile(r.log.Name())
			lines := strings.Split(string(b), "\n")
			if len(lines) > 40 {
				lines = lines[len(lines)-40:]
			}
			t.Logf("relay log tail:\n%s", strings.Join(lines, "\n"))
		}
	})
	return r
}

func (r *relayProc) start() {
	r.t.Helper()
	r.cmd = exec.Command(relayBin, "serve", "-c", r.cfgPath, "--log-level", "warn")
	r.cmd.Env = append(os.Environ(), "CHAOS_PAYSTACK="+paystackSecret, "CHAOS_APP="+appSecret)
	r.cmd.Stdout, r.cmd.Stderr = r.log, r.log
	if err := r.cmd.Start(); err != nil {
		r.t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(r.url + "/healthz")
		if err == nil {
			resp.Body.Close()
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatal("relay did not come up")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// kill sends SIGKILL: no graceful shutdown, no lease release.
func (r *relayProc) kill() {
	if r.cmd != nil && r.cmd.Process != nil {
		_ = r.cmd.Process.Kill()
		_ = r.cmd.Wait()
		r.cmd = nil
		r.kills++
	}
}

func (r *relayProc) stop() {
	if r.cmd != nil && r.cmd.Process != nil {
		_ = r.cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { r.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			r.cmd.Process.Kill()
			<-done
		}
		r.cmd = nil
	}
}

func chargeBody(i int) []byte {
	return []byte(fmt.Sprintf(`{"event":"charge.success","data":{"id":%d,"reference":"CH_%06d","amount":%d}}`, 900000+i, i, 100*i))
}

// post sends one signed webhook the way Paystack would.
func post(client *http.Client, url string, body []byte) (int, error) {
	req, _ := http.NewRequest(http.MethodPost, url+"/webhooks/paystack", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-paystack-signature", paystack.Sign(paystackSecret, body))
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, nil
}

// receiver verifies Relay's signature and counts deliveries per event.
type receiver struct {
	mu       sync.Mutex
	seen     map[string]int // event ID → successful deliveries
	badSig   int
	failRate func() bool
	delay    func() time.Duration
}

func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	ts, ev, del := r.Header.Get("X-Relay-Timestamp"), r.Header.Get("X-Relay-Event-ID"), r.Header.Get("X-Relay-Delivery-ID")
	m := hmac.New(sha256.New, []byte(appSecret))
	m.Write([]byte("v1:" + ts + ":" + ev + ":" + del + ":"))
	m.Write(body)
	want := "v1=" + hex.EncodeToString(m.Sum(nil))
	if r.Header.Get("X-Relay-Signature") != want {
		rc.mu.Lock()
		rc.badSig++
		rc.mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if rc.delay != nil {
		time.Sleep(rc.delay())
	}
	if rc.failRate != nil && rc.failRate() {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	rc.mu.Lock()
	rc.seen[ev]++
	rc.mu.Unlock()
}

// waitAllDelivered polls the database (alongside the running server)
// until every delivery is delivered or the deadline passes.
func waitAllDelivered(t *testing.T, dsn string, want int, timeout time.Duration) map[string]string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		st, err := store.OpenTool(context.Background(), dsn)
		if err != nil {
			t.Fatal(err)
		}
		dels, err := st.ListDeliveries(context.Background(), store.DeliveryFilter{Limit: 1_000_000})
		st.Close()
		if err != nil {
			t.Fatal(err)
		}
		delivered := 0
		status := map[string]string{}
		for _, d := range dels {
			status[d.EventID] = string(d.Status)
			if d.Status == store.DeliveryDelivered {
				delivered++
			}
		}
		if delivered == want && len(dels) == want {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatalf("after %v: %d/%d deliveries delivered (%d rows)", timeout, delivered, want, len(dels))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }
