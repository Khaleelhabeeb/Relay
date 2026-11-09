package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/khaleelhabeeb/relay/admin"
	"github.com/khaleelhabeeb/relay/internal/ops"
)

func newEventsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "events", Short: "Inspect and act on received events"}
	addConfigFlag(cmd)
	cmd.AddCommand(newEventsListCmd(), newEventsInspectCmd(), newEventsReprocessCmd(), newEventsRerouteCmd(),
		newEventsDismissCmd(), newEventsConfirmCmd())
	return cmd
}

func newEventsListCmd() *cobra.Command {
	var (
		f      admin.EventFilter
		since  time.Duration
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List events, newest first",
		Args:  cobra.NoArgs,
		RunE: withTool(func(ctx context.Context, env *toolEnv, _ []string) error {
			f.Since = sinceFlag(since)
			page, err := env.ops.ListEvents(ctx, f)
			if err != nil {
				return err
			}
			if asJSON {
				return env.json(page)
			}
			env.table("ID\tRECEIVED\tPROVIDER\tTYPE\tSTATUS\tAPPLICATION\tREFERENCE\tDUPLICATES\tCONFIRMATION", func(w io.Writer) {
				for _, e := range page.Events {
					conf := e.Confirmation
					if conf == admin.ConfirmationNone {
						conf = ""
					}
					status := e.RoutingStatus
					if !e.DismissedAt.IsZero() {
						status += " (dismissed)"
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n", e.ID, ts(e.ReceivedAt), e.Provider, e.EventType,
						status, dash(e.ApplicationID), dash(e.Reference), e.DuplicateCount, dash(conf))
				}
			})
			if page.Next != "" {
				fmt.Fprintf(env.out, "\nmore events: add --after %s\n", page.Next)
			}
			return nil
		}),
	}
	cmd.Flags().StringVar(&f.Status, "status", "", "routing status: matched, unresolved, ambiguous, unsupported")
	cmd.Flags().StringVar(&f.Confirmation, "confirmation", "", "provider API confirmation: pending, failed, rejected, confirmed, none")
	cmd.Flags().StringVar(&f.EventType, "type", "", "event type, e.g. charge.success")
	cmd.Flags().StringVar(&f.Provider, "provider", "", "provider, e.g. paystack")
	cmd.Flags().StringVar(&f.ApplicationID, "application", "", "owning application")
	cmd.Flags().BoolVar(&f.Held, "held", false, "only events needing an owner decided (unresolved or ambiguous, not dismissed)")
	cmd.Flags().DurationVar(&since, "since", 0, "only events received within this duration, e.g. 24h")
	cmd.Flags().IntVar(&f.Limit, "limit", 50, fmt.Sprintf("maximum rows (at most %d)", admin.MaxLimit))
	cmd.Flags().StringVar(&f.After, "after", "", "continue a previous list after this event ID")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	return cmd
}

func newEventsInspectCmd() *cobra.Command {
	var showPayload, asJSON bool
	cmd := &cobra.Command{
		Use:   "inspect <event-id>",
		Short: "Show an event's routing history, deliveries and attempts",
		Long: "Show an event's routing history, deliveries and attempts.\n\n" +
			"The raw payment payload is withheld by default (only its size and SHA-256 are shown); " +
			"pass --show-payload to print it.",
		Args: cobra.ExactArgs(1),
		RunE: withTool(func(ctx context.Context, env *toolEnv, args []string) error {
			e, err := env.ops.GetEvent(ctx, args[0], showPayload)
			if err != nil {
				return err
			}
			if asJSON {
				return env.json(e)
			}

			w := env.out
			fmt.Fprintf(w, "Event        %s\n", e.ID)
			fmt.Fprintf(w, "Provider     %s\n", e.Provider)
			fmt.Fprintf(w, "Type         %s\n", e.EventType)
			fmt.Fprintf(w, "Status       %s\n", e.RoutingStatus)
			fmt.Fprintf(w, "Application  %s\n", dash(e.ApplicationID))
			fmt.Fprintf(w, "Reference    %s\n", dash(e.Reference))
			if e.DedicatedAccount != "" {
				fmt.Fprintf(w, "DVA          %s\n", e.DedicatedAccount)
			}
			fmt.Fprintf(w, "Dedup key    %s\n", e.DedupKey)
			if !e.DismissedAt.IsZero() {
				fmt.Fprintf(w, "Dismissed    %s by %s: %s\n", ts(e.DismissedAt), e.DismissedBy, e.DismissReason)
			}
			switch e.Confirmation {
			case admin.ConfirmationPending:
				fmt.Fprintf(w, "Confirmation pending: not delivered yet (attempts: %d, next: %s, last error: %s)\n",
					e.ConfirmAttempts, ts(e.ConfirmNextAt), dash(e.ConfirmError))
			case admin.ConfirmationFailed:
				fmt.Fprintf(w, "Confirmation failed: not delivered (attempts: %d, last error: %s); retry with `relay events confirm %s`\n",
					e.ConfirmAttempts, dash(e.ConfirmError), e.ID)
			case admin.ConfirmationRejected:
				fmt.Fprintf(w, "Confirmation rejected by the provider API: never delivered (%s)\n", dash(e.ConfirmError))
			case admin.ConfirmationConfirmed:
				fmt.Fprintf(w, "Confirmation confirmed by the provider API\n")
			}
			fmt.Fprintf(w, "Received     %s (seen again %d times, last %s)\n", ts(e.ReceivedAt), e.DuplicateCount, ts(e.LastSeenAt))
			if e.PayloadBytes == 0 {
				fmt.Fprintf(w, "Payload      purged by retention (sha256 %s)\n", e.PayloadSHA256)
			} else {
				fmt.Fprintf(w, "Payload      %d bytes, sha256 %s\n", e.PayloadBytes, e.PayloadSHA256)
			}

			fmt.Fprintln(w, "\nRouting decisions")
			env.table("  WHEN\tBY\tOUTCOME\tAPPLICATION\tREASON\tEVIDENCE", func(tw io.Writer) {
				for _, d := range e.RoutingDecisions {
					var ev []string
					for _, c := range d.Candidates {
						ev = append(ev, c.Application+"←"+c.Source+" "+c.Detail)
					}
					fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n", ts(d.DecidedAt), d.DecidedBy, d.Outcome,
						dash(d.ApplicationID), d.Reason, dash(strings.Join(ev, "; ")))
				}
			})

			if len(e.Deliveries) > 0 {
				fmt.Fprintln(w, "\nDeliveries")
				for _, d := range e.Deliveries {
					fmt.Fprintf(w, "  %s → %s: %s, %d attempt(s), next %s\n", d.ID, d.ApplicationID, d.Status, d.AttemptCount, ts(d.NextAttemptAt))
					env.table("    #\tSTARTED\tHTTP\tERROR\tMS", func(tw io.Writer) {
						for _, a := range d.Attempts {
							httpStatus := "-"
							if a.HTTPStatus != 0 {
								httpStatus = fmt.Sprint(a.HTTPStatus)
							}
							code := dash(a.ErrorCode)
							if !a.Finished {
								code = "(unfinished)"
							}
							fmt.Fprintf(tw, "    %d\t%s\t%s\t%s\t%d\n", a.AttemptNo, ts(a.StartedAt), httpStatus, code, a.DurationMS)
						}
					})
				}
			}
			if len(e.DedupConflicts) > 0 {
				fmt.Fprintln(w, "\nConflicting duplicates (same dedup key, different identifiers: possible forgery)")
				env.table("  RECEIVED\tREFERENCE\tDVA\tSHA256", func(tw io.Writer) {
					for _, c := range e.DedupConflicts {
						fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", ts(c.ReceivedAt), dash(c.Reference), dash(c.DedicatedAccount), c.PayloadSHA256)
					}
				})
			}
			if e.Payload != nil {
				fmt.Fprintf(w, "\nPayload\n%s\n", e.Payload)
				for _, c := range e.DedupConflicts {
					if c.Payload != nil {
						fmt.Fprintf(w, "\nConflicting payload %s\n%s\n", c.ID, c.Payload)
					}
				}
			}
			return nil
		}),
	}
	cmd.Flags().BoolVar(&showPayload, "show-payload", false, "print the raw payment payload (sensitive)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	return cmd
}

func newEventsReprocessCmd() *cobra.Command {
	var (
		since  time.Duration
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "reprocess [event-id...]",
		Short: "Re-run routing for unresolved events with the current rules and registrations",
		Long: "Re-run routing for unresolved events using the current configuration and registrations.\n\n" +
			"With event IDs, only those events are reprocessed; otherwise every unresolved event " +
			"(optionally limited by --since). Events that are already routed are never changed.",
		RunE: withTool(func(ctx context.Context, env *toolEnv, args []string) error {
			if len(args) > 0 && since > 0 {
				return errors.New("give event IDs or --since, not both")
			}
			res, err := env.ops.Reprocess(ctx, admin.ReprocessRequest{EventIDs: args, Since: sinceFlag(since)})
			if err != nil {
				return err
			}
			if asJSON {
				if err := env.json(res); err != nil {
					return err
				}
			} else {
				for _, e := range res.Events {
					switch {
					case e.Error != "":
						fmt.Fprintf(env.out, "%s: error: %s\n", e.ID, e.Error)
					case len(args) > 0:
						fmt.Fprintf(env.out, "%s: %s\n", e.ID, e.Status)
					}
				}
				fmt.Fprintf(env.out, "reprocessed %d event(s): %d now matched, %d ambiguous, %d still unresolved\n",
					res.Considered, res.Matched, res.Ambiguous, res.Unresolved)
			}
			if res.Failed > 0 {
				return fmt.Errorf("%d of %d events could not be reprocessed", res.Failed, res.Considered)
			}
			return nil
		}),
	}
	cmd.Flags().DurationVar(&since, "since", 0, "only events received within this duration, e.g. 72h")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	return cmd
}

func newEventsRerouteCmd() *cobra.Command {
	var application, reason string
	cmd := &cobra.Command{
		Use:   "reroute <event-id> --application <id> --reason <text>",
		Short: "Assign an owner to an unresolved or ambiguous event (audited)",
		Args:  cobra.ExactArgs(1),
		RunE: withTool(func(ctx context.Context, env *toolEnv, args []string) error {
			res, err := env.ops.Reroute(ctx, args[0], application, reason, ops.ViaCLI)
			if err != nil {
				return err
			}
			msg := "delivery queued"
			if res.AwaitingConfirmation {
				msg = "delivery waits for provider API confirmation"
			}
			fmt.Fprintf(env.out, "%s rerouted to %s; %s\n", args[0], application, msg)
			return nil
		}),
	}
	cmd.Flags().StringVar(&application, "application", "", "owning application (required)")
	cmd.Flags().StringVar(&reason, "reason", "", "why this owner is correct; stored in the audit trail (required)")
	_ = cmd.MarkFlagRequired("application")
	_ = cmd.MarkFlagRequired("reason")
	return cmd
}

func newEventsDismissCmd() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "dismiss <event-id> --reason <text>",
		Short: "Close an unresolved or ambiguous event without an owner (audited)",
		Long: "Close a held event that belongs to none of your applications. Nothing is delivered and it\n" +
			"stops counting as held in stats, metrics and reminders. The reason is kept on the event.\n" +
			"Rerouting it later clears the dismissal.",
		Args: cobra.ExactArgs(1),
		RunE: withTool(func(ctx context.Context, env *toolEnv, args []string) error {
			if _, err := env.ops.Dismiss(ctx, args[0], reason, ops.ViaCLI); err != nil {
				return err
			}
			fmt.Fprintf(env.out, "%s dismissed\n", args[0])
			return nil
		}),
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why nobody owns this event; stored on the event (required)")
	_ = cmd.MarkFlagRequired("reason")
	return cmd
}

func newEventsConfirmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "confirm <event-id>...",
		Short: "Retry provider API confirmation now for pending or failed events",
		Long: "Retry provider API confirmation now for events whose check against the provider's API\n" +
			"failed (confirmation pending or failed). The running relay serve picks them up within\n" +
			"seconds and delivers each one once the provider confirms it; one the provider disowns is\n" +
			"rejected and never delivered. Find them with: relay events list --confirmation failed",
		Args: cobra.MinimumNArgs(1),
		RunE: withTool(func(ctx context.Context, env *toolEnv, args []string) error {
			var errs []error
			for _, id := range args {
				if err := env.ops.RetryConfirmation(ctx, id); err != nil {
					errs = append(errs, err)
					continue
				}
				fmt.Fprintf(env.out, "%s: confirmation queued\n", id)
			}
			return errors.Join(errs...)
		}),
	}
}
