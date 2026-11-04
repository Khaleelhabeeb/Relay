// Package server wires Relay's HTTP endpoints and runs the listeners with
// graceful shutdown.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/khaleelhabeeb/relay/config"
	"github.com/khaleelhabeeb/relay/internal/apierr"
	"github.com/khaleelhabeeb/relay/internal/ingest"
	"github.com/khaleelhabeeb/relay/internal/metrics"
	"github.com/khaleelhabeeb/relay/provider"
	"github.com/khaleelhabeeb/relay/provider/flutterwave"
	"github.com/khaleelhabeeb/relay/provider/paystack"
)

const shutdownTimeout = 15 * time.Second

type Server struct {
	logger *slog.Logger
	main   *http.Server
	api    *http.Server // nil when the API shares the main listener
}

// Deps are the components the HTTP layer serves.
type Deps struct {
	Acceptor ingest.Acceptor
	API      http.Handler                    // /v1/ registration API; optional
	Ready    func(ctx context.Context) error // readiness check; nil means always ready
	Metrics  *metrics.Metrics                // served at /metrics when non-nil
	// Adapters are the providers whose webhooks are served at
	// /webhooks/<name>; nil means Adapters(cfg).
	Adapters []provider.Adapter
}

// New builds the HTTP servers. The API (and metrics) are mounted on the main
// listener unless cfg.Server.APIListen names a separate, private address.
func New(cfg *config.Config, logger *slog.Logger, deps Deps) *Server {
	mux := http.NewServeMux()
	registerWebhooks(mux, cfg, logger, deps)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		apierr.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if deps.Ready != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()
			if err := deps.Ready(ctx); err != nil {
				logger.Error("readiness check failed", "error", err)
				apierr.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
				return
			}
		}
		apierr.WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})

	s := &Server{logger: logger, main: newHTTPServer(cfg.Server.Listen, mux, logger)}
	private := mux
	if cfg.Server.APIListen != "" && (deps.API != nil || deps.Metrics != nil) {
		private = http.NewServeMux()
		s.api = newHTTPServer(cfg.Server.APIListen, private, logger)
	}
	if deps.API != nil {
		private.Handle("/v1/", deps.API)
	}
	if deps.Metrics != nil {
		private.Handle("GET /metrics", deps.Metrics.Handler())
	}
	return s
}

// Webhooks returns a handler serving only POST /webhooks/<provider>, for
// mounting in another server.
func Webhooks(cfg *config.Config, logger *slog.Logger, deps Deps) http.Handler {
	mux := http.NewServeMux()
	registerWebhooks(mux, cfg, logger, deps)
	return mux
}

func registerWebhooks(mux *http.ServeMux, cfg *config.Config, logger *slog.Logger, deps Deps) {
	// One concurrency budget across providers: they share the database writer.
	var sem chan struct{}
	if cfg.Server.MaxConcurrentWebhooks > 0 {
		sem = make(chan struct{}, cfg.Server.MaxConcurrentWebhooks)
	}
	adapters := deps.Adapters
	if adapters == nil {
		adapters = Adapters(cfg)
	}
	for _, ad := range adapters {
		mux.Handle("POST /webhooks/"+ad.Name(), &ingest.Handler{
			Adapter:      ad,
			MaxBodyBytes: cfg.Server.MaxBodyBytes,
			Acceptor:     deps.Acceptor,
			Logger:       logger,
			Metrics:      deps.Metrics,
			Sem:          sem,
			IPFilter: &ingest.IPFilter{
				Allowed:        config.ParsePrefixes(cfg.Providers.AllowedIPs(ad.Name())),
				TrustedProxies: config.ParsePrefixes(cfg.Server.TrustedProxies),
			},
		})
	}
}

// Adapters returns the webhook adapters of the enabled providers.
func Adapters(cfg *config.Config) []provider.Adapter {
	var out []provider.Adapter
	if p := cfg.Providers.Paystack; p != nil {
		out = append(out, paystack.Adapter{Secret: p.Secret, ConfirmWithAPI: p.ConfirmWithAPI})
	}
	if p := cfg.Providers.Flutterwave; p != nil {
		out = append(out, flutterwave.Adapter{SecretHash: p.SecretHash, ConfirmWithAPI: p.ConfirmWithAPI, SecretKey: p.SecretKey})
	}
	return out
}

func newHTTPServer(addr string, h http.Handler, logger *slog.Logger) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
}

// Handler returns the main listener's handler (webhooks, health and, when
// not separated, the API).
func (s *Server) Handler() http.Handler { return s.main.Handler }

// APIHandler returns the separate API listener's handler, or nil.
func (s *Server) APIHandler() http.Handler {
	if s.api == nil {
		return nil
	}
	return s.api.Handler
}

// Run listens on every configured address until ctx is cancelled, then
// shuts all of them down gracefully.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.main.Addr)
	if err != nil {
		return err
	}
	if s.api == nil {
		return s.Serve(ctx, ln)
	}
	apiLn, err := net.Listen("tcp", s.api.Addr)
	if err != nil {
		ln.Close()
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg   sync.WaitGroup
		errs = make([]error, 2)
	)
	wg.Add(2)
	go func() { defer wg.Done(); defer cancel(); errs[0] = serve(ctx, s.main, ln, s.logger, "webhooks") }()
	go func() {
		defer wg.Done()
		defer cancel()
		errs[1] = serve(ctx, s.api, apiLn, s.logger, "registration API")
	}()
	wg.Wait()
	return errors.Join(errs...)
}

// Serve runs the main server on an existing listener.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	return serve(ctx, s.main, ln, s.logger, "relay")
}

func serve(ctx context.Context, srv *http.Server, ln net.Listener, logger *slog.Logger, name string) error {
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	logger.Info(name+" listening", "addr", ln.Addr().String())

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	logger.Info(name + " shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
