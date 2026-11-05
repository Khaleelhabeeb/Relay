<p align="center">
  <a href="../README.md">
    <picture>
      <source media="(prefers-color-scheme: dark)" srcset="../assets/relay-logo-dark.png">
      <img src="../assets/relay-logo.png" alt="Relay" width="160">
    </picture>
  </a>
</p>

# Using Relay as a Go library

The `relay` command is a thin wrapper around a Go package. You can import that package and run
Relay inside your own program. You can also import the smaller packages on their own: to verify
Relay's deliveries, to add a payment provider, or to verify provider webhooks yourself.

```bash
go get github.com/khaleelhabeeb/relay@latest
```

| Package | Use it to |
|---|---|
| [`relay`](#embedding-relay) | Run Relay inside your program: webhook handlers, delivery workers, in-process registration |
| [`relay/config`](#configuration-in-go) | Load `relay.yaml`, or build and validate a configuration in Go |
| [`relay/receiver`](#receiving-deliveries-in-go) | Verify Relay's signed deliveries in a Go application |
| [`relay/provider`](#adding-a-payment-provider) | The `Adapter` interface for adding a payment provider |
| `relay/provider/paystack`, `relay/provider/flutterwave`, `relay/provider/monnify` | The built-in adapters. You can also use them on their own to verify and parse those providers' webhooks. |

> **Stability:** the API is pre-1.0. It may change between minor versions until 1.0, and every
> change is recorded in [CHANGELOG.md](../CHANGELOG.md). Everything under `internal/` is private
> and can change at any time.

## Embedding Relay

```go
cfg, err := config.LoadFile("relay.yaml")
if err != nil {
	log.Fatal(err)
}
r, err := relay.New(ctx, cfg, relay.WithLogger(slog.Default()))
if err != nil {
	log.Fatal(err)
}
defer r.Close()
```

`New` validates the configuration, opens the SQLite database, applies migrations and builds the
pipeline. Then pick how Relay is served.

**Let Relay run the listeners** in `cfg.Server`. This is what `relay serve` does:

```go
err = r.ListenAndServe(ctx) // returns after ctx is cancelled and shutdown completes
```

**Or mount it in your own server,** and run the background workers yourself:

```go
mux.Handle("/webhooks/", r.WebhookHandler())     // POST /webhooks/<provider>
privateMux.Handle("/v1/", r.APIHandler())        // registration API, keep it private
go func() { log.Print(r.Run(ctx)) }()            // delivery and retention workers
```

| Method | What it does |
|---|---|
| `Handler()` / `ServeHTTP` | Everything the main listener serves: webhooks, `/healthz`, `/readyz` and, unless `server.api_listen` is set, the API and `/metrics` |
| `WebhookHandler()` | `POST /webhooks/<provider>` only. Keep the `/webhooks/` prefix, because that's the URL providers post to. |
| `APIHandler()` | `/v1/registrations`, authenticated with each application's registration token |
| `Run(ctx)` | Delivers stored events and applies retention until `ctx` is cancelled |
| `ListenAndServe(ctx)` | `Run`, plus the HTTP listeners from `cfg.Server` |
| `RegisterReference`, `RegisterDedicatedAccount`, `Deactivate` | Claim and release identifiers in-process, without the HTTP API |
| `Providers()` | The provider names Relay accepts webhooks from |
| `Close()` | Closes the database. Call it after `Run` or `ListenAndServe` returns. |

### Things to know

- **One Relay per database.** Relay holds a lock on `storage.dsn`. A second `New` on the same
  database, in this process or another, fails with `relay.ErrLocked`.
- **Webhooks are stored without `Run`, but nothing is delivered until `Run` runs.** Run it exactly
  once. A second call returns `relay.ErrRunning`.
- **The guarantees don't change.** The provider gets `200` only after the event and its routing
  decision are committed. Delivery is still at least once.
- **Logging is off by default.** Pass `relay.WithLogger`. Set the User-Agent of outbound
  deliveries with `relay.WithUserAgent`.

### Registering ownership in-process

If your program creates the orders, it can claim their references directly:

```go
reg, routed, err := r.RegisterReference(ctx, "billing", "paystack", "ORDER_1001")
switch {
case errors.Is(err, relay.ErrConflict):
	// another application owns ORDER_1001
case errors.Is(err, relay.ErrInvalid):
	// unknown application or provider, or a malformed reference
case err != nil:
	// storage error
}
// routed counts earlier unresolved events this registration just routed: from the last 72 hours
// inside the application's reference_prefixes, otherwise from at most 15 minutes before it.
```

The same rules apply as for the [HTTP API](../README.md#registering-ownership). References never
move to another application. Dedicated accounts move only after `Deactivate`.

## Configuration in Go

`config.LoadFile` and `config.Load` read YAML and resolve `*_env` secrets from the environment.
To skip YAML, build a `config.Config` and set the secret fields directly. `relay.New` applies the
defaults and validates it, and reports every problem at once:

```go
cfg := &config.Config{}
cfg.Storage.DSN = "/var/lib/myapp/relay.db"
cfg.Providers.Paystack = &config.PaystackConfig{Secret: os.Getenv("PAYSTACK_SECRET_KEY")}
cfg.Applications = map[string]*config.ApplicationConfig{
	"billing": {
		Endpoint:      "https://billing.internal.example.com/relay",
		SigningSecret: os.Getenv("BILLING_RELAY_SECRET"),
	},
}
cfg.Routes = []config.RouteConfig{
	{Application: "billing", Match: config.MatchConfig{ReferencePrefix: "BILL_"}},
}
```

Call `cfg.Validate()` to check a configuration without opening anything. All the options are
described in the [configuration reference](../README.md#configuration-reference). The Go field names match the YAML
keys.

## Receiving deliveries in Go

Package `receiver` verifies Relay's signed POSTs. It doesn't open a database or start any
workers.

```go
http.Handle("/webhooks/relay", receiver.Handler(secret, func(ctx context.Context, d *receiver.Delivery) error {
	if alreadyProcessed(d.EventID) { // delivery is at least once
		return nil
	}
	// d.Body is the provider's payload, exactly as sent. With payload_format: envelope,
	// d.Envelope() decodes the wrapper.
	return fulfil(ctx, d) // an error makes Relay retry
}))
```

- **Responses:** `Handler` answers `401` for a bad signature or stale timestamp, `413` for an
  oversized body, `500` when your function returns an error, and `200` otherwise.
- **Verifying yourself:** to verify inside your own handler, use
  `receiver.VerifyRequest(r, secrets, opts)`, or `receiver.Verify(header, body, secrets, opts)`.
- **Rotating secrets:** pass both the old and new secret to `receiver.HandlerWith`.

## Adding a payment provider

A provider is an implementation of `provider.Adapter`:

```go
type Adapter interface {
	Name() string                            // "acme": stored with every event, served at /webhooks/acme
	Verify(h http.Header, body []byte) error // authenticate the exact raw body
	Parse(raw []byte) (*provider.Event, error)
	ExtractIdentifiers(e *provider.Event) provider.Identifiers
	DedupKey(e *provider.Event, ids provider.Identifiers) string
	Routes(eventType string) bool // which event types are routed to an application
}
```

Pass it to `relay.New` with `relay.WithProvider(myAdapter{...})`. If your YAML routes name the
provider (`provider: acme`), tell the loader it exists: `config.LoadFile("relay.yaml", "acme")`.
An adapter named after a built-in (`paystack`, `flutterwave`, `monnify`) replaces that built-in.

Guidelines:

- **Verify against the exact bytes,** in constant time. Return an error for a missing or invalid
  signature. Never parse the body before it's verified.
- **`Parse`, `ExtractIdentifiers`, `DedupKey` and `Routes` must not need secrets.** Relay also
  calls them to re-read stored events when it reprocesses. All methods must be safe for
  concurrent use.
- **The dedup key must be stable** for every delivery of the same occurrence. Prefer a provider
  ID, such as `"<type>:<transaction id>"`. Fall back to a hash of the body.
- **Route only what you've checked.** Return `true` from `Routes` only for event types whose
  reference and metadata fields you've verified against real payloads. Other events are stored
  as `unsupported`.
- **Reference and metadata** are what routes and registrations match on. Leave
  `DedicatedAccount` nil unless the provider has stable virtual-account identifiers.
- **Name is permanent.** It's stored with every event and registration. Use lower case letters,
  digits, `-` or `_`.
- **Optionally,** implement `provider.Hinter` to give operators a provider-specific reason when an
  event can't be routed.

`provider/paystack`, `provider/flutterwave` and `provider/monnify` are complete examples. `relay_test.go` contains a
small custom adapter with an end-to-end test.

## The `relay` CLI with an embedded Relay

The operator commands (`relay events`, `relay deliveries`, `relay registrations`) work against
any Relay database, embedded or not. Point them at a YAML file with the same `storage.dsn` and
applications. Its routes may name custom providers. To check such a file, declare the providers:

```bash
relay config validate -c relay.yaml --provider acme
```

There's one limit. `relay events reprocess` re-reads stored events with the built-in adapters,
so it can't reprocess events from a custom provider. Reprocess those from your program, or let
in-process registration route them.
