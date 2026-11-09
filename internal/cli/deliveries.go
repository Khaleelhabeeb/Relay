package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/khaleelhabeeb/relay/admin"
)

func newDeliveriesCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "deliveries", Short: "Inspect and replay deliveries"}
	addConfigFlag(cmd)
	cmd.AddCommand(newDeliveriesListCmd(), newDeliveriesRetryCmd())
	return cmd
}

func newDeliveriesListCmd() *cobra.Command {
	var (
		f      admin.DeliveryFilter
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List deliveries, most recently updated first",
		Args:  cobra.NoArgs,
		RunE: withTool(func(ctx context.Context, env *toolEnv, _ []string) error {
			dels, err := env.ops.ListDeliveries(ctx, f)
			if err != nil {
				return err
			}
			if asJSON {
				return env.json(struct {
					Data []admin.Delivery `json:"data"`
				}{dels})
			}
			env.table("ID\tEVENT\tAPPLICATION\tSTATUS\tATTEMPTS\tNEXT\tLAST ERROR", func(w io.Writer) {
				for _, d := range dels {
					next := "-"
					if d.Status == admin.DeliveryPending {
						next = ts(d.NextAttemptAt)
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", d.ID, d.EventID, d.ApplicationID, d.Status, d.AttemptCount, next, dash(d.LastError))
				}
			})
			return nil
		}),
	}
	cmd.Flags().StringVar(&f.Status, "status", "", "pending, in_flight, delivered, exhausted")
	cmd.Flags().StringVar(&f.ApplicationID, "application", "", "application ID")
	cmd.Flags().StringVar(&f.EventID, "event", "", "event ID")
	cmd.Flags().IntVar(&f.Limit, "limit", 50, fmt.Sprintf("maximum rows (at most %d)", admin.MaxLimit))
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
				err := env.ops.RetryDelivery(ctx, id)
				switch {
				case errors.Is(err, errNotFound):
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
