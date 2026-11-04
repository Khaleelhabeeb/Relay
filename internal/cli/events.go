package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/khaleelhabeeb/relay/internal/reprocess"
	"github.com/khaleelhabeeb/relay/internal/routing"
	"github.com/khaleelhabeeb/relay/internal/store"
)

func newEventsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "events", Short: "Inspect and act on received events"}
	addConfigFlag(cmd)
	cmd.AddCommand(newEventsListCmd(), newEventsInspectCmd(), newEventsReprocessCmd(), newEventsRerouteCmd())
	return cmd
}

func newEventsListCmd() *cobra.Command {
	var (
		f      store.EventFilter
		since  time.Duration
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List events, newest first",
		Args:  cobra.NoArgs,
		RunE: withTool(func(ctx context.Context, env *toolEnv, _ []string) error {
			if since > 0 {
				f.Since = time.Now().Add(-since)
			}
			events, err := env.store.ListEvents(ctx, f)
			if err != nil {
				return err
			}
			if asJSON {
				if events == nil {
					events = []store.EventSummary{}
				}
				return env.json(events)
			}
			env.table("ID\tRECEIVED\tTYPE\tSTATUS\tAPPLICATION\tREFERENCE\tDUPLICATES", func(w io.Writer) {
				for _, e := range events {
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%d\n", e.ID, ts(e.ReceivedAt), e.EventType, e.RoutingStatus,
						dash(e.ApplicationID), dash(e.Reference), e.DuplicateCount)
				}
			})
			return nil
		}),
	}
	cmd.Flags().StringVar(&f.Status, "status", "", "routing status: matched, unresolved, ambiguous, unsupported")
	cmd.Flags().StringVar(&f.EventType, "type", "", "event type, e.g. charge.success")
	cmd.Flags().DurationVar(&since, "since", 0, "only events received within this duration, e.g. 24h")
	cmd.Flags().IntVar(&f.Limit, "limit", 50, "maximum rows")
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
			e, err := env.store.GetEvent(ctx, args[0])
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("event %s not found", args[0])
			}
			if err != nil {
				return err
			}
			decisions, err := env.store.RoutingDecisionsForEvent(ctx, e.ID)
			if err != nil {
				return err
			}
			deliveries, err := env.store.DeliveriesForEvent(ctx, e.ID)
			if err != nil {
				return err
			}
			conflicts, err := env.store.DedupConflictsForEvent(ctx, e.ID)
			if err != nil {
				return err
			}
			attempts := map[string][]store.Attempt{}
			for _, d := range deliveries {
				if attempts[d.ID], err = env.store.AttemptsForDelivery(ctx, d.ID); err != nil {
					return err
				}
			}

			if asJSON {
				out := map[string]any{
					"id": e.ID, "type": e.EventType, "dedup_key": e.DedupKey, "routing_status": e.RoutingStatus,
					"application": e.ApplicationID, "reference": e.Reference, "dedicated_account": e.DedicatedAccount,
					"received_at": e.ReceivedAt, "last_seen_at": e.LastSeenAt, "duplicate_count": e.DuplicateCount,
					"payload_bytes": len(e.RawPayload), "payload_sha256": e.PayloadSHA256,
					"routing_decisions": decisions, "deliveries": deliveries, "attempts": attempts,
					"dedup_conflicts": conflicts,
				}
				if showPayload && e.RawPayload != nil {
					out["payload"] = json.RawMessage(e.RawPayload)
				}
				return env.json(out)
			}

			w := env.out
			fmt.Fprintf(w, "Event        %s\n", e.ID)
			fmt.Fprintf(w, "Type         %s\n", e.EventType)
			fmt.Fprintf(w, "Status       %s\n", e.RoutingStatus)
			fmt.Fprintf(w, "Application  %s\n", dash(e.ApplicationID))
			fmt.Fprintf(w, "Reference    %s\n", dash(e.Reference))
			if e.DedicatedAccount != "" {
				fmt.Fprintf(w, "DVA          %s\n", e.DedicatedAccount)
			}
			fmt.Fprintf(w, "Dedup key    %s\n", e.DedupKey)
			fmt.Fprintf(w, "Received     %s (seen again %d times, last %s)\n", ts(e.ReceivedAt), e.DuplicateCount, ts(e.LastSeenAt))
			if e.RawPayload == nil {
				fmt.Fprintf(w, "Payload      purged by retention (sha256 %s)\n", e.PayloadSHA256)
			} else {
				fmt.Fprintf(w, "Payload      %d bytes, sha256 %s\n", len(e.RawPayload), e.PayloadSHA256)
			}

			fmt.Fprintln(w, "\nRouting decisions")
			env.table("  WHEN\tBY\tOUTCOME\tAPPLICATION\tREASON\tEVIDENCE", func(tw io.Writer) {
				for _, d := range decisions {
					var ev []string
					for _, c := range d.Candidates {
						ev = append(ev, c.Application+"←"+c.Source+" "+c.Detail)
					}
					fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n", ts(d.DecidedAt), d.DecidedBy, d.Outcome,
						dash(d.ApplicationID), d.Reason, dash(strings.Join(ev, "; ")))
				}
			})

			if len(deliveries) > 0 {
				fmt.Fprintln(w, "\nDeliveries")
				for _, d := range deliveries {
					fmt.Fprintf(w, "  %s → %s: %s, %d attempt(s), next %s\n", d.ID, d.ApplicationID, d.Status, d.AttemptCount, ts(d.NextAttemptAt))
					env.table("    #\tSTARTED\tHTTP\tERROR\tMS", func(tw io.Writer) {
						for _, a := range attempts[d.ID] {
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
			if len(conflicts) > 0 {
				fmt.Fprintln(w, "\nConflicting duplicates (same dedup key, different identifiers: possible forgery)")
				env.table("  RECEIVED\tREFERENCE\tDVA\tSHA256", func(tw io.Writer) {
					for _, c := range conflicts {
						fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", ts(c.ReceivedAt), dash(c.Reference), dash(c.DedicatedAccount), c.PayloadSHA256)
					}
				})
			}
			if showPayload && e.RawPayload != nil {
				fmt.Fprintf(w, "\nPayload\n%s\n", e.RawPayload)
				for _, c := range conflicts {
					if c.RawPayload != nil {
						fmt.Fprintf(w, "\nConflicting payload %s\n%s\n", c.ID, c.RawPayload)
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
	var since time.Duration
	cmd := &cobra.Command{
		Use:   "reprocess [event-id...]",
		Short: "Re-run routing for unresolved events with the current rules and registrations",
		Long: "Re-run routing for unresolved events using the current configuration and registrations.\n\n" +
			"With event IDs, only those events are reprocessed; otherwise every unresolved event " +
			"(optionally limited by --since). Events that are already routed are never changed.",
		RunE: withTool(func(ctx context.Context, env *toolEnv, args []string) error {
			rp := &reprocess.Reprocessor{Store: env.store, Engine: routing.NewEngine(routing.RulesFromConfig(env.cfg.Routes)).WithNamespaces(routing.NamespacesFromConfig(env.cfg.Applications)), Logger: quietLogger()}
			var (
				res reprocess.Result
				err error
			)
			if len(args) > 0 {
				for _, id := range args {
					res.Considered++
					outcome, err := rp.Event(ctx, id, store.DecidedByReprocess)
					if err != nil {
						return fmt.Errorf("%s: %w", id, err)
					}
					fmt.Fprintf(env.out, "%s: %s\n", id, outcome)
					switch outcome {
					case routing.Matched:
						res.Matched++
					case routing.Ambiguous:
						res.Ambiguous++
					}
				}
			} else {
				var from time.Time
				if since > 0 {
					from = time.Now().Add(-since)
				}
				if res, err = rp.Unresolved(ctx, from, store.DecidedByReprocess); err != nil {
					return err
				}
			}
			fmt.Fprintf(env.out, "reprocessed %d event(s): %d now matched, %d ambiguous, %d still unresolved\n",
				res.Considered, res.Matched, res.Ambiguous, res.Considered-res.Matched-res.Ambiguous)
			return nil
		}),
	}
	cmd.Flags().DurationVar(&since, "since", 0, "only events received within this duration, e.g. 72h")
	return cmd
}

func newEventsRerouteCmd() *cobra.Command {
	var application, reason string
	cmd := &cobra.Command{
		Use:   "reroute <event-id> --application <id> --reason <text>",
		Short: "Assign an owner to an unresolved or ambiguous event (audited)",
		Args:  cobra.ExactArgs(1),
		RunE: withTool(func(ctx context.Context, env *toolEnv, args []string) error {
			if _, ok := env.cfg.Applications[application]; !ok {
				return fmt.Errorf("unknown application %q", application)
			}
			rp := &reprocess.Reprocessor{Store: env.store, Logger: quietLogger()}
			if err := rp.Reroute(ctx, args[0], application, reason); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("event %s not found", args[0])
				}
				return err
			}
			fmt.Fprintf(env.out, "%s rerouted to %s; delivery queued\n", args[0], application)
			return nil
		}),
	}
	cmd.Flags().StringVar(&application, "application", "", "owning application (required)")
	cmd.Flags().StringVar(&reason, "reason", "", "why this owner is correct; stored in the audit trail (required)")
	_ = cmd.MarkFlagRequired("application")
	_ = cmd.MarkFlagRequired("reason")
	return cmd
}
