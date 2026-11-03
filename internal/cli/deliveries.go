package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/khaleelhabeeb/relay/internal/store"
)

func newDeliveriesCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "deliveries", Short: "Inspect and replay deliveries"}
	addConfigFlag(cmd)
	cmd.AddCommand(newDeliveriesListCmd(), newDeliveriesRetryCmd())
	return cmd
}

func newDeliveriesListCmd() *cobra.Command {
	var (
		f      store.DeliveryFilter
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List deliveries, most recently updated first",
		Args:  cobra.NoArgs,
		RunE: withTool(func(ctx context.Context, env *toolEnv, _ []string) error {
			dels, err := env.store.ListDeliveries(ctx, f)
			if err != nil {
				return err
			}
			if asJSON {
				if dels == nil {
					dels = []store.DeliverySummary{}
				}
				return env.json(dels)
			}
			env.table("ID\tEVENT\tAPPLICATION\tSTATUS\tATTEMPTS\tNEXT\tLAST ERROR", func(w io.Writer) {
				for _, d := range dels {
					next := "-"
					if d.Status == store.DeliveryPending {
						next = ts(d.NextAttemptAt)
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", d.ID, d.EventID, d.ApplicationID, d.Status, d.AttemptCount, next, dash(d.LastError))
				}
			})
			return nil
		}),
	}
	cmd.Flags().StringVar(&f.Status, "status", "", "pending, in_flight, delivered, exhausted")
	cmd.Flags().StringVar(&f.Application, "application", "", "application ID")
	cmd.Flags().IntVar(&f.Limit, "limit", 50, "maximum rows")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	return cmd
}

func newDeliveriesRetryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "retry <delivery-id>...",
		Short: "Replay exhausted deliveries to the same owner with a fresh attempt budget",
		Args:  cobra.MinimumNArgs(1),
		RunE: withTool(func(ctx context.Context, env *toolEnv, args []string) error {
			var failed int
			for _, id := range args {
				err := env.store.RetryDelivery(ctx, id, time.Now())
				switch {
				case errors.Is(err, store.ErrNotFound):
					fmt.Fprintf(env.out, "%s: not found\n", id)
					failed++
				case err != nil:
					fmt.Fprintf(env.out, "%s: %v\n", id, err)
					failed++
				default:
					fmt.Fprintf(env.out, "%s: queued for redelivery\n", id)
				}
			}
			if failed > 0 {
				return fmt.Errorf("%d of %d deliveries not retried", failed, len(args))
			}
			return nil
		}),
	}
}
