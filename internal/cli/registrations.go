package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/khaleelhabeeb/relay/internal/store"
)

func newRegistrationsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "registrations", Short: "Inspect ownership registrations"}
	addConfigFlag(cmd)

	var (
		f      store.RegistrationFilter
		active string
		asJSON bool
	)
	list := &cobra.Command{
		Use:   "list",
		Short: "List registrations in creation order",
		Args:  cobra.NoArgs,
		RunE: withTool(func(ctx context.Context, env *toolEnv, _ []string) error {
			if active != "" {
				b, err := strconv.ParseBool(active)
				if err != nil {
					return fmt.Errorf("--active must be true or false")
				}
				f.Active = &b
			}
			regs, err := env.store.ListRegistrations(ctx, f)
			if err != nil {
				return err
			}
			if asJSON {
				if regs == nil {
					regs = []store.Registration{}
				}
				return env.json(regs)
			}
			env.table("ID\tAPPLICATION\tTYPE\tVALUE\tACTIVE\tCREATED\tDEACTIVATED", func(w io.Writer) {
				for _, r := range regs {
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%t\t%s\t%s\n", r.ID, r.ApplicationID, r.IdentifierType, r.IdentifierValue,
						r.Active, ts(r.CreatedAt), ts(r.DeactivatedAt))
				}
			})
			return nil
		}),
	}
	list.Flags().StringVar(&f.Application, "application", "", "application ID")
	list.Flags().StringVar(&f.IdentifierType, "type", "", "transaction_reference or dedicated_account")
	list.Flags().StringVar(&active, "active", "", "true or false")
	list.Flags().IntVar(&f.Limit, "limit", 100, "maximum rows")
	list.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	cmd.AddCommand(list)
	return cmd
}
