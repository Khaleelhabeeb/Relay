package relay_test

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"

	"github.com/khaleelhabeeb/relay"
	"github.com/khaleelhabeeb/relay/config"
)

// Run Relay on the listeners in relay.yaml, as the relay command does.
func Example() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cfg, err := config.LoadFile("relay.yaml")
	if err != nil {
		log.Fatal(err)
	}
	r, err := relay.New(ctx, cfg, relay.WithLogger(slog.Default()))
	if err != nil {
		log.Fatal(err)
	}
	defer r.Close()
	if err := r.ListenAndServe(ctx); err != nil {
		log.Fatal(err)
	}
}

// Mount Relay's webhook endpoints in an existing server, with a
// configuration built in Go instead of YAML.
func ExampleRelay_WebhookHandler() {
	ctx := context.Background()

	cfg := &config.Config{}
	cfg.Storage.DSN = "/var/lib/myapp/relay.db"
	cfg.Providers.Paystack = &config.PaystackConfig{Secret: os.Getenv("PAYSTACK_SECRET_KEY")}
	cfg.Applications = map[string]*config.ApplicationConfig{
		"billing": {Endpoint: "https://billing.internal.example.com/relay", SigningSecret: os.Getenv("BILLING_RELAY_SECRET")},
	}
	cfg.Routes = []config.RouteConfig{{Application: "billing", Match: config.MatchConfig{ReferencePrefix: "BILL_"}}}

	r, err := relay.New(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer r.Close()
	go func() {
		if err := r.Run(ctx); err != nil { // deliveries and retention
			log.Print(err)
		}
	}()

	mux := http.NewServeMux()
	mux.Handle("/webhooks/", r.WebhookHandler()) // POST /webhooks/paystack
	log.Fatal(http.ListenAndServe(":8080", mux))
}

// Claim a reference from the same process instead of the HTTP API, e.g.
// when an order is created.
func ExampleRelay_RegisterReference() {
	var r *relay.Relay // from relay.New
	ctx := context.Background()

	reg, routed, err := r.RegisterReference(ctx, "billing", "paystack", "ORDER_1001")
	switch {
	case errors.Is(err, relay.ErrConflict):
		log.Print("another application owns ORDER_1001")
	case err != nil:
		log.Fatal(err)
	default:
		log.Printf("registered %s; routed %d waiting event(s)", reg.ID, routed)
	}
}

// Add a payment provider Relay does not ship: implement provider.Adapter
// (see the acme adapter in relay_test.go) and pass it with WithProvider.
// Routes can then name it, and LoadFile must be told it exists.
func ExampleWithProvider() {
	ctx := context.Background()
	cfg, err := config.LoadFile("relay.yaml", "acme") // routes may say provider: acme
	if err != nil {
		log.Fatal(err)
	}
	r, err := relay.New(ctx, cfg, relay.WithProvider(acme{token: os.Getenv("ACME_WEBHOOK_TOKEN")}))
	if err != nil {
		log.Fatal(err)
	}
	defer r.Close()
	log.Print(r.Providers()) // webhooks arrive at /webhooks/acme
}
