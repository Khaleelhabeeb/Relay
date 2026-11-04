package cli

import (
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/khaleelhabeeb/relay"
	"github.com/khaleelhabeeb/relay/config"
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

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			r, err := relay.New(ctx, cfg, relay.WithLogger(logger), relay.WithUserAgent("Relay/"+Version))
			if err != nil {
				return err
			}
			defer r.Close()
			return r.ListenAndServe(ctx)
		},
	}
	cmd.Flags().StringVarP(&configPath, "config", "c", "relay.yaml", "path to the configuration file")
	cmd.Flags().StringVar(&logLevel, "log-level", "info", "log level: debug, info, warn, error")
	return cmd
}
