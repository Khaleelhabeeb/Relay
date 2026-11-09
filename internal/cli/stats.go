package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/khaleelhabeeb/relay/internal/store"
)

// errNotFound matches every not-found error from the ops core.
var errNotFound = store.ErrNotFound

func newStatsCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Show events needing attention and the delivery backlog",
		Args:  cobra.NoArgs,
		RunE: withTool(func(ctx context.Context, env *toolEnv, _ []string) error {
			st, err := env.ops.Stats(ctx)
			if err != nil {
				return err
			}
			if asJSON {
				return env.json(st)
			}
			age := func(t time.Time) string {
				if t.IsZero() {
					return ""
				}
				return fmt.Sprintf(" (oldest %s, %s ago)", ts(t), time.Since(t).Round(time.Second))
			}
			w := env.out
			e, d := st.Events, st.Deliveries
			fmt.Fprintf(w, "Held events          %d unresolved, %d ambiguous%s\n", e.Unresolved, e.Ambiguous, age(e.OldestHeldAt))
			fmt.Fprintf(w, "Confirmations        %d pending, %d failed\n", e.ConfirmationPending, e.ConfirmationFailed)
			fmt.Fprintf(w, "Deliveries           %d pending, %d in flight, %d delivered, %d exhausted%s\n",
				d.Pending, d.InFlight, d.Delivered, d.Exhausted, age(d.OldestPendingAt))
			if e.Unresolved+e.Ambiguous > 0 {
				fmt.Fprintln(w, "\nFind held events with: relay events list --held")
			}
			if d.Exhausted > 0 {
				fmt.Fprintln(w, "Find exhausted deliveries with: relay deliveries list --status exhausted")
			}
			if e.ConfirmationFailed > 0 {
				fmt.Fprintln(w, "Find failed confirmations with: relay events list --confirmation failed")
			}
			return nil
		}),
	}
	addConfigFlag(cmd)
	cmd.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	return cmd
}
