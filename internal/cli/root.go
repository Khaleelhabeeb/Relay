// Package cli defines the relay command-line interface.
package cli

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
)

// Version is set at build time with -ldflags "-X github.com/khaleelhabeeb/relay/internal/cli.Version=...".
var Version = "dev"

func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "relay",
		Short:         "Self-hosted Paystack webhook router",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newServeCmd(), newConfigCmd(), newEventsCmd(), newDeliveriesCmd(), newRegistrationsCmd())
	return root
}

// Execute runs the CLI and returns the process exit code.
func Execute() int {
	if err := NewRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

func newLogger(level string) (*slog.Logger, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid --log-level %q", level)
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: l})), nil
}
