package cli

import (
	"context"
	"errors"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/khaleelhabeeb/relay/internal/config"
	"github.com/khaleelhabeeb/relay/internal/delivery"
	"github.com/khaleelhabeeb/relay/internal/ingest"
	"github.com/khaleelhabeeb/relay/internal/metrics"
	"github.com/khaleelhabeeb/relay/internal/registration"
	"github.com/khaleelhabeeb/relay/internal/reprocess"
	"github.com/khaleelhabeeb/relay/internal/retention"
	"github.com/khaleelhabeeb/relay/internal/routing"
	"github.com/khaleelhabeeb/relay/internal/server"
	"github.com/khaleelhabeeb/relay/internal/store"
)

func newServeCmd() *cobra.Command {
	var configPath, logLevel string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the webhook receiver and delivery workers",
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger, err := newLogger(logLevel)
			if err != nil {
				return err
			}
			cfg, err := config.LoadFile(configPath)
			if err != nil {
				return err
			}
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
			if cfg.Delivery.AllowInsecureLocal {
				logger.Warn("delivery.allow_insecure_local is enabled: http:// and private destinations are allowed; development only")
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			st, err := store.Open(ctx, cfg.Storage.DSN)
			if err != nil {
				return err
			}
			defer st.Close()
			version, err := st.SchemaVersion(ctx)
			if err != nil {
				return err
			}
			logger.Info("database ready", "dsn", cfg.Storage.DSN, "schema_version", version)

			var m *metrics.Metrics
			if cfg.Server.Metrics {
				m = metrics.New(st)
			}
			dispatcher := delivery.NewDispatcher(cfg, st, logger, "Relay/"+Version)
			dispatcher.Metrics = m
			engine := routing.NewEngine(routing.RulesFromConfig(cfg.Routes))
			acceptor := &ingest.StoreAcceptor{Store: st, Engine: engine, Logger: logger, Metrics: m, OnRouted: dispatcher.Nudge}
			reprocessor := &reprocess.Reprocessor{Store: st, Engine: engine, Logger: logger, Metrics: m, OnRouted: dispatcher.Nudge}
			purger := &retention.Purger{Store: st, PayloadDays: cfg.Retention.PayloadDays, AttemptDays: cfg.Retention.AttemptDays, Logger: logger}
			deps := server.Deps{
				Acceptor: acceptor,
				API:      registration.NewHandler(cfg, st, reprocessor, logger),
				Metrics:  m,
				Ready: func(ctx context.Context) error {
					if err := st.Ping(ctx); err != nil {
						return err
					}
					return st.Heartbeat(ctx, time.Now())
				},
			}

			// Server and dispatcher share ctx. If either fails, stop the other.
			runCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			var (
				wg   sync.WaitGroup
				mu   sync.Mutex
				errs []error
			)
			run := func(f func(context.Context) error) {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if err := f(runCtx); err != nil {
						mu.Lock()
						errs = append(errs, err)
						mu.Unlock()
					}
					cancel()
				}()
			}
			run(server.New(cfg, logger, deps).Run)
			run(dispatcher.Run)
			run(purger.Run)
			wg.Wait()
			return errors.Join(errs...)
		},
	}
	cmd.Flags().StringVarP(&configPath, "config", "c", "relay.yaml", "path to the configuration file")
	cmd.Flags().StringVar(&logLevel, "log-level", "info", "log level: debug, info, warn, error")
	return cmd
}
