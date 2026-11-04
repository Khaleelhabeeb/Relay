package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/khaleelhabeeb/relay/config"
	"github.com/khaleelhabeeb/relay/internal/store"
)

// toolEnv is what operator commands share: config (without secrets) and a
// lock-free store connection that works alongside a running server.
type toolEnv struct {
	cfg   *config.Config
	store *store.Store
	out   io.Writer
}

func addConfigFlag(cmd *cobra.Command) {
	cmd.PersistentFlags().StringP("config", "c", "relay.yaml", "path to the configuration file")
}

func openTool(cmd *cobra.Command) (*toolEnv, error) {
	path, _ := cmd.Flags().GetString("config")
	cfg, err := config.LoadFileWithoutSecrets(path)
	if err != nil {
		return nil, err
	}
	st, err := store.OpenTool(cmd.Context(), cfg.Storage.DSN)
	if err != nil {
		return nil, err
	}
	return &toolEnv{cfg: cfg, store: st, out: cmd.OutOrStdout()}, nil
}

// withTool opens the tool environment around fn.
func withTool(fn func(ctx context.Context, env *toolEnv, args []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		env, err := openTool(cmd)
		if err != nil {
			return err
		}
		defer env.store.Close()
		return fn(cmd.Context(), env, args)
	}
}

func (e *toolEnv) table(header string, rows func(w io.Writer)) {
	w := tabwriter.NewWriter(e.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, header)
	rows(w)
	w.Flush()
}

func (e *toolEnv) json(v any) error {
	enc := json.NewEncoder(e.out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func ts(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// quietLogger is for commands that reuse server components but report
// through their own output.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}
