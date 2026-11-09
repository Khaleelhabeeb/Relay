// Package relay embeds Relay, a payment webhook router, in a Go program.
//
// A payment provider sends every webhook for an account to one URL. Relay
// receives them, verifies each one, decides which of your applications owns
// it, stores it durably in SQLite and delivers it to that application with
// its own signature, retrying until it is accepted. Events with no owner, or
// more than one, are held for an operator; Relay never guesses.
//
// The relay command runs the same code as a standalone service. Embed it
// instead when you want Relay inside an existing server, or need a provider
// Relay does not ship (see WithProvider and package provider):
//
//	cfg, err := config.LoadFile("relay.yaml")
//	if err != nil { ... }
//	r, err := relay.New(ctx, cfg, relay.WithLogger(logger))
//	if err != nil { ... }
//	defer r.Close()
//
//	mux.Handle("/webhooks/", r.WebhookHandler()) // in your own server
//	go r.Run(ctx)                                // delivery, confirmation and retention workers
//
// or let Relay serve the listeners in cfg.Server itself with ListenAndServe.
//
// # Operating Relay
//
// Events no application can be identified as owning (unresolved), or that
// more than one claims (ambiguous), are held. The operator methods (Events,
// Event, Reroute, Dismiss, Reprocess, RetryConfirmation, Deliveries,
// RetryDelivery and Stats) find and resolve them, and exhausted deliveries, from Go. They do
// what the relay CLI and the HTTP admin API (AdminHandler) do, with the same
// rules, and return the record types of package admin.
//
// Applications receiving Relay's deliveries in Go can verify them with
// package receiver.
//
// The API is pre-1.0: it may change between minor versions, and the
// CHANGELOG records every change.
package relay

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/khaleelhabeeb/relay/admin"
	"github.com/khaleelhabeeb/relay/config"
	"github.com/khaleelhabeeb/relay/internal/alert"
	"github.com/khaleelhabeeb/relay/internal/confirm"
	"github.com/khaleelhabeeb/relay/internal/delivery"
	"github.com/khaleelhabeeb/relay/internal/ingest"
	"github.com/khaleelhabeeb/relay/internal/metrics"
	"github.com/khaleelhabeeb/relay/internal/ops"
	"github.com/khaleelhabeeb/relay/internal/registration"
	"github.com/khaleelhabeeb/relay/internal/reprocess"
	"github.com/khaleelhabeeb/relay/internal/retention"
	"github.com/khaleelhabeeb/relay/internal/routing"
	"github.com/khaleelhabeeb/relay/internal/server"
	"github.com/khaleelhabeeb/relay/internal/store"
	"github.com/khaleelhabeeb/relay/provider"
	"github.com/khaleelhabeeb/relay/provider/flutterwave"
	"github.com/khaleelhabeeb/relay/provider/monnify"
	"github.com/khaleelhabeeb/relay/provider/paystack"
	"github.com/khaleelhabeeb/relay/provider/stripe"
)

// Errors returned by Relay. Compare with errors.Is.
var (
	// ErrLocked means another Relay process holds the database.
	ErrLocked = store.ErrLocked
	// ErrInvalid means a registration or operator request failed
	// validation, e.g. an unknown application or a reroute without a reason.
	ErrInvalid = registration.ErrInvalid
	// ErrConflict means another application owns the identifier.
	ErrConflict = store.ErrRegistrationConflict
	// ErrNotFound means no such registration exists for the application,
	// or no such event or delivery exists.
	ErrNotFound = store.ErrNotFound
	// ErrWrongState means an event or delivery is not in a state that allows
	// the operator action, e.g. rerouting an event that already has an
	// owner or retrying a delivery that has not been exhausted.
	ErrWrongState = ops.ErrWrongState
	// ErrRunning means Run or ListenAndServe was already called.
	ErrRunning = errors.New("relay: already running")
)

var providerNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// Option configures New.
type Option func(*options)

type options struct {
	logger    *slog.Logger
	adapters  []provider.Adapter
	userAgent string
	alerts    []func(context.Context, admin.AlertDigest) error
}

// WithLogger sets the structured logger. The default discards logs.
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// WithProvider adds a provider, served at /webhooks/<a.Name()>. An adapter
// named like a built-in provider ("paystack", "flutterwave", "monnify",
// "stripe") replaces the one configured in cfg.Providers. Routes may name the
// provider in routes[].provider.
func WithProvider(a provider.Adapter) Option {
	return func(o *options) { o.adapters = append(o.adapters, a) }
}

// WithAlertHandler calls fn with each operator alert digest: the held
// events, exhausted deliveries, failed or rejected confirmations,
// conflicting duplicates and expired events since the last digest, at
// most once per cfg.Alerts.DigestInterval, plus a reminder every
// cfg.Alerts.ReminderInterval while anything still needs attention. It
// runs alongside, or instead of, cfg.Alerts.URL. When fn returns an error,
// the same alerts are offered again at the next interval.
func WithAlertHandler(fn func(ctx context.Context, d admin.AlertDigest) error) Option {
	return func(o *options) { o.alerts = append(o.alerts, fn) }
}

// WithUserAgent sets the User-Agent of outbound deliveries (default "Relay").
func WithUserAgent(ua string) Option { return func(o *options) { o.userAgent = ua } }

// Relay is an embedded instance. Create it with New, start its workers with
// Run (or ListenAndServe), and Close it when they have returned. Its methods
// are safe for concurrent use.
type Relay struct {
	cfg          *config.Config
	logger       *slog.Logger
	store        *store.Store
	adapters     []provider.Adapter
	server       *server.Server
	webhooks     http.Handler
	registration *registration.Handler
	ops          *ops.Service
	admin        *ops.Handler
	dispatcher   *delivery.Dispatcher
	purger       *retention.Purger
	confirmer    *confirm.Worker
	alerter      *alert.Alerter // nil when no alert sink is configured
	running      atomic.Bool
}

// New validates cfg (applying defaults), opens its database and builds the
// pipeline. Built-in providers come from cfg.Providers; add others with
// WithProvider. Only one Relay, in any process, may use a database at a time:
// a second New on the same storage.dsn fails with ErrLocked.
func New(ctx context.Context, cfg *config.Config, opts ...Option) (*Relay, error) {
	o := options{logger: slog.New(slog.DiscardHandler), userAgent: "Relay"}
	for _, opt := range opts {
		opt(&o)
	}
	adapters := server.Adapters(cfg)
	var extra []string
	for _, a := range o.adapters {
		if a == nil || !providerNamePattern.MatchString(a.Name()) {
			return nil, fmt.Errorf("relay: invalid provider name %q (want %s)", nameOf(a), providerNamePattern)
		}
		if missingSecret(a) {
			return nil, fmt.Errorf("relay: provider %q added with WithProvider has an empty secret", a.Name())
		}
		if i := slices.IndexFunc(adapters, func(b provider.Adapter) bool { return b.Name() == a.Name() }); i >= 0 {
			adapters[i] = a
			continue
		}
		adapters = append(adapters, a)
		extra = append(extra, a.Name())
	}
	if err := cfg.Validate(extra...); err != nil {
		return nil, err
	}
	logger := o.logger

	st, err := store.Open(ctx, cfg.Storage.DSN)
	if err != nil {
		return nil, err
	}
	version, err := st.SchemaVersion(ctx)
	if err != nil {
		st.Close()
		return nil, err
	}
	logger.Info("database ready", "dsn", cfg.Storage.DSN, "schema_version", version)
	logWarnings(cfg, logger)

	var m *metrics.Metrics
	if cfg.Server.Metrics {
		m = metrics.New(st)
	}
	dispatcher := delivery.NewDispatcher(cfg, st, logger, o.userAgent)
	dispatcher.Metrics = m
	engine := routing.NewEngine(routing.RulesFromConfig(cfg.Routes), adapters...).WithNamespaces(routing.NamespacesFromConfig(cfg.Applications))
	confirmer := confirm.New(st, adapters, logger, m)
	confirmer.OnDelivery = dispatcher.Nudge
	acceptor := &ingest.StoreAcceptor{Store: st, Engine: engine, Logger: logger, Metrics: m,
		OnRouted: dispatcher.Nudge, OnPending: confirmer.Nudge}
	reprocessor := &reprocess.Reprocessor{Store: st, Engine: engine, Logger: logger, Metrics: m, OnRouted: dispatcher.Nudge}
	reg := registration.NewHandler(cfg, st, reprocessor, logger)
	svc := &ops.Service{Store: st, Reprocessor: reprocessor, Applications: slices.Sorted(maps.Keys(cfg.Applications)),
		OnDelivery: dispatcher.Nudge, OnConfirm: confirmer.Nudge}
	adminAPI := ops.NewHandler(svc, cfg.Server.AdminToken, logger)
	var sinks []alert.Sink
	if a := cfg.Alerts; a.URL != "" {
		sinks = append(sinks, &alert.Webhook{URL: a.URL, Format: a.Format, Secret: a.Secret,
			Client: delivery.NewHTTPClient(cfg.Delivery.AllowInsecureLocal), UserAgent: o.userAgent})
	}
	for _, fn := range o.alerts {
		if fn != nil {
			sinks = append(sinks, alert.Func(fn))
		}
	}
	var alerter *alert.Alerter
	if len(sinks) > 0 {
		alerter = &alert.Alerter{Store: st, Sinks: sinks, Interval: time.Duration(cfg.Alerts.DigestInterval),
			Reminder: cfg.Alerts.Reminder(), StatsFn: svc.Stats, Logger: logger, Metrics: m}
	}
	deps := server.Deps{
		Acceptor: acceptor,
		API:      reg,
		Admin:    adminAPI,
		Metrics:  m,
		Adapters: adapters,
		Ready: func(ctx context.Context) error {
			if err := st.Ping(ctx); err != nil {
				return err
			}
			return st.Heartbeat(ctx, time.Now())
		},
	}
	return &Relay{
		cfg:          cfg,
		logger:       logger,
		store:        st,
		adapters:     adapters,
		server:       server.New(cfg, logger, deps),
		webhooks:     server.Webhooks(cfg, logger, deps),
		registration: reg,
		ops:          svc,
		admin:        adminAPI,
		dispatcher:   dispatcher,
		confirmer:    confirmer,
		purger: &retention.Purger{Store: st, PayloadDays: cfg.Retention.PayloadDays,
			AttemptDays: cfg.Retention.AttemptDays, HeldDays: cfg.Retention.HeldDays, Logger: logger},
		alerter: alerter,
	}, nil
}

func nameOf(a provider.Adapter) string {
	if a == nil {
		return "<nil>"
	}
	return a.Name()
}

// missingSecret reports a built-in adapter without its secret, e.g. one
// built from an unset environment variable. Its Verify would reject every
// webhook; failing here surfaces the mistake at startup instead.
func missingSecret(a provider.Adapter) bool {
	switch a := a.(type) {
	case paystack.Adapter:
		return a.Secret == ""
	case *paystack.Adapter:
		return a.Secret == ""
	case flutterwave.Adapter:
		return a.SecretHash == "" || (a.ConfirmWithAPI && a.SecretKey == "")
	case *flutterwave.Adapter:
		return a.SecretHash == "" || (a.ConfirmWithAPI && a.SecretKey == "")
	case monnify.Adapter:
		return a.SecretKey == "" || (a.ConfirmWithAPI && a.APIKey == "")
	case *monnify.Adapter:
		return a.SecretKey == "" || (a.ConfirmWithAPI && a.APIKey == "")
	case stripe.Adapter:
		return stripeMissingSecret(a)
	case *stripe.Adapter:
		return stripeMissingSecret(*a)
	}
	return false
}

func stripeMissingSecret(a stripe.Adapter) bool {
	return !slices.ContainsFunc(a.WebhookSecrets, func(s string) bool { return s != "" }) || (a.ConfirmWithAPI && a.APIKey == "")
}

func logWarnings(cfg *config.Config, logger *slog.Logger) {
	for _, name := range cfg.Providers.Names() {
		allowed := cfg.Providers.AllowedIPs(name)
		if len(allowed) == 0 {
			continue
		}
		logger.Info("webhook source allowlist enabled", "provider", name, "allowed", len(allowed),
			"trusted_proxies", len(cfg.Server.TrustedProxies))
		if len(cfg.Server.TrustedProxies) == 0 {
			logger.Warn("allowed_ips is set without server.trusted_proxies: behind a reverse proxy every webhook will appear to come from the proxy and be rejected", "provider", name)
		}
	}
	if len(cfg.Applications) > 1 {
		if p := cfg.Providers.Paystack; p != nil && !p.ConfirmWithAPI {
			logger.Warn("providers.paystack.confirm_with_api is off: any application holding the Paystack secret key can forge webhooks that Relay delivers to the others")
		}
		if p := cfg.Providers.Flutterwave; p != nil && !p.ConfirmWithAPI {
			logger.Warn("providers.flutterwave.confirm_with_api is off: anyone who knows the secret hash can forge webhooks that Relay delivers")
		}
		if p := cfg.Providers.Monnify; p != nil && !p.ConfirmWithAPI {
			logger.Warn("providers.monnify.confirm_with_api is off: any application holding the Monnify secret key can forge webhooks that Relay delivers to the others")
		}
		if p := cfg.Providers.Stripe; p != nil && !p.ConfirmWithAPI {
			logger.Warn("providers.stripe.confirm_with_api is off: anyone who obtains the endpoint signing secret can forge webhooks that Relay delivers")
		}
	}
	if p := cfg.Providers.Monnify; p != nil && p.AllowUnsignedSandbox {
		logger.Warn("providers.monnify.allow_unsigned_sandbox is enabled: unsigned webhooks are accepted when the Monnify sandbox API confirms them; sandbox only")
	}
	if cfg.Server.AdminToken != "" && cfg.Server.APIListen == "" {
		logger.Warn("server.admin_token_env is set without server.api_listen: ListenAndServe does not serve the admin API on the public listener; set api_listen, or mount AdminHandler on a private listener yourself")
	}
	if cfg.Delivery.AllowInsecureLocal {
		logger.Warn("delivery.allow_insecure_local is enabled: http:// and private destinations are allowed; development only")
	}
}

// Providers returns the names of the providers Relay accepts webhooks from.
func (r *Relay) Providers() []string {
	names := make([]string, len(r.adapters))
	for i, a := range r.adapters {
		names[i] = a.Name()
	}
	return names
}

// Handler serves everything the main listener would: POST
// /webhooks/<provider>, GET /healthz and /readyz and, unless
// cfg.Server.APIListen is set, the /v1/registrations API and /metrics.
func (r *Relay) Handler() http.Handler { return r.server.Handler() }

// ServeHTTP makes Relay an http.Handler; it is Handler().
func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.server.Handler().ServeHTTP(w, req)
}

// WebhookHandler serves only POST /webhooks/<provider>, for mounting in your
// own server, e.g. mux.Handle("/webhooks/", r.WebhookHandler()). Keep the
// path: providers are told to post to /webhooks/<name>.
func (r *Relay) WebhookHandler() http.Handler { return r.webhooks }

// APIHandler serves the /v1/registrations API, which applications call with
// their registration tokens. Mount it on a private listener.
func (r *Relay) APIHandler() http.Handler { return r.registration }

// AdminHandler serves the /v1/admin/ operator API, authorized by
// cfg.Server.AdminToken; without a token it refuses every request. Mount it
// on a private listener, e.g. mux.Handle("/v1/admin/", r.AdminHandler()).
// ListenAndServe serves it on cfg.Server.APIListen when that is set.
func (r *Relay) AdminHandler() http.Handler { return r.admin }

// Events lists events, newest first. Filter by Status: admin.StatusUnresolved
// or admin.StatusAmbiguous to find held events.
func (r *Relay) Events(ctx context.Context, f admin.EventFilter) (admin.EventPage, error) {
	return r.ops.ListEvents(ctx, f)
}

// Event returns an event with its routing history, deliveries, attempts and
// conflicting duplicates. The raw payment payload is included only if
// includePayload. Errors: ErrNotFound.
func (r *Relay) Event(ctx context.Context, id string, includePayload bool) (admin.EventDetail, error) {
	return r.ops.GetEvent(ctx, id, includePayload)
}

// Reroute assigns application as the owner of an unresolved or ambiguous
// event and queues its delivery (once confirmed, if the provider API must
// confirm it). The decision and reason are kept in the event's routing
// history. Errors: ErrNotFound, ErrInvalid (unknown application, empty
// reason), ErrWrongState (the event is not held, or its payload was purged).
func (r *Relay) Reroute(ctx context.Context, id, application, reason string) (admin.RerouteResult, error) {
	return r.ops.Reroute(ctx, id, application, reason, ops.ViaGoAPI)
}

// Dismiss closes a held event without an owner, e.g. a payment that belongs
// to none of your applications: nothing is delivered and it no longer
// counts as held. The reason is kept on the event; rerouting it later
// clears the dismissal. Errors: ErrNotFound, ErrInvalid (empty reason),
// ErrWrongState (not held, or already dismissed).
func (r *Relay) Dismiss(ctx context.Context, id, reason string) (admin.Event, error) {
	return r.ops.Dismiss(ctx, id, reason, ops.ViaGoAPI)
}

// Reprocess routes unresolved events again with the current rules and
// registrations, e.g. after adding a route. A failure on one event is
// reported in its result entry and does not stop the others.
func (r *Relay) Reprocess(ctx context.Context, req admin.ReprocessRequest) (admin.ReprocessResult, error) {
	return r.ops.Reprocess(ctx, req)
}

// RetryConfirmation retries provider API confirmation now, with a fresh
// retry window, for an event whose confirmation is pending or failed; it is
// delivered once confirmed. Errors: ErrNotFound, ErrWrongState.
func (r *Relay) RetryConfirmation(ctx context.Context, id string) error {
	return r.ops.RetryConfirmation(ctx, id)
}

// Deliveries lists deliveries, most recently updated first. Filter by
// Status: admin.DeliveryExhausted to find deliveries that gave up.
func (r *Relay) Deliveries(ctx context.Context, f admin.DeliveryFilter) ([]admin.Delivery, error) {
	return r.ops.ListDeliveries(ctx, f)
}

// RetryDelivery replays an exhausted delivery to the same owner with a
// fresh attempt budget. Errors: ErrNotFound, ErrWrongState.
func (r *Relay) RetryDelivery(ctx context.Context, id string) error {
	return r.ops.RetryDelivery(ctx, id)
}

// Stats returns the counts an operator watches: held events, confirmations
// pending or failed, and deliveries by status.
func (r *Relay) Stats(ctx context.Context) (admin.Stats, error) {
	return r.ops.Stats(ctx)
}

// Run delivers stored events, finishes provider API confirmations that
// failed at ingest, applies retention and, when configured, sends operator
// alerts until ctx is cancelled.
// Webhooks are accepted and stored without it, but nothing is delivered
// until it runs. It returns ErrRunning if already running.
func (r *Relay) Run(ctx context.Context) error {
	if !r.running.CompareAndSwap(false, true) {
		return ErrRunning
	}
	return runAll(ctx, r.workers()...)
}

func (r *Relay) workers() []func(context.Context) error {
	w := []func(context.Context) error{r.dispatcher.Run, r.confirmer.Run, r.purger.Run}
	if r.alerter != nil {
		w = append(w, r.alerter.Run)
	}
	return w
}

// ListenAndServe runs the workers and serves cfg.Server.Listen (and
// APIListen, if set) until ctx is cancelled, then shuts down gracefully.
func (r *Relay) ListenAndServe(ctx context.Context) error {
	if !r.running.CompareAndSwap(false, true) {
		return ErrRunning
	}
	return runAll(ctx, append(r.workers(), r.server.Run)...)
}

// runAll runs fns with a shared context; when one returns, the rest stop.
func runAll(ctx context.Context, fns ...func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, f := range fns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer cancel()
			if err := f(ctx); err != nil && !errors.Is(err, context.Canceled) {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// Close closes the database. Call it after Run or ListenAndServe returns.
func (r *Relay) Close() error { return r.store.Close() }

// Registration is an application's claim on an identifier.
type Registration struct {
	ID            string
	Provider      string
	Type          string // IdentifierReference or IdentifierDedicatedAccount
	Value         string // canonical: a reference, or "bank:account_number"
	Application   string
	Active        bool
	CreatedAt     time.Time
	DeactivatedAt time.Time // zero while active
}

// Identifier types for registrations.
const (
	IdentifierReference        = "transaction_reference"
	IdentifierDedicatedAccount = "dedicated_account"
)

func toRegistration(r *store.Registration) Registration {
	return Registration{ID: r.ID, Provider: r.Provider, Type: r.IdentifierType, Value: r.IdentifierValue,
		Application: r.ApplicationID, Active: r.Active, CreatedAt: r.CreatedAt, DeactivatedAt: r.DeactivatedAt}
}

func (r *Relay) checkApp(application string) error {
	if _, ok := r.cfg.Applications[application]; !ok {
		return fmt.Errorf("%w: unknown application %q", ErrInvalid, application)
	}
	return nil
}

// RegisterReference claims a transaction reference of providerName for
// application, as an application can over the registration API. Events with
// that reference received in the last 72 hours that are still unresolved
// are routed at once; routed counts them. Registering a reference the
// application already owns is a no-op. Errors: ErrInvalid, ErrConflict.
func (r *Relay) RegisterReference(ctx context.Context, application, providerName, reference string) (reg Registration, routed int, err error) {
	return r.register(ctx, application, registration.CreateRequest{
		Provider: providerName, Type: IdentifierReference, Value: reference})
}

// RegisterDedicatedAccount claims a Paystack dedicated virtual account,
// identified by bank name and account number, for application. Otherwise as
// RegisterReference.
func (r *Relay) RegisterDedicatedAccount(ctx context.Context, application, bank, accountNumber string) (reg Registration, routed int, err error) {
	return r.register(ctx, application, registration.CreateRequest{
		Provider: provider.Paystack, Type: IdentifierDedicatedAccount, Bank: bank, AccountNumber: accountNumber})
}

func (r *Relay) register(ctx context.Context, application string, req registration.CreateRequest) (Registration, int, error) {
	if err := r.checkApp(application); err != nil {
		return Registration{}, 0, err
	}
	reg, _, routed, err := r.registration.Register(ctx, application, req)
	if err != nil {
		return Registration{}, 0, err
	}
	return toRegistration(reg), routed, nil
}

// Deactivate ends one of application's registrations. Errors: ErrNotFound.
func (r *Relay) Deactivate(ctx context.Context, application, registrationID string) (Registration, error) {
	if err := r.checkApp(application); err != nil {
		return Registration{}, err
	}
	reg, err := r.registration.Deactivate(ctx, application, registrationID)
	if err != nil {
		return Registration{}, err
	}
	return toRegistration(reg), nil
}
