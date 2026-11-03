package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/khaleelhabeeb/relay/internal/config"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Configuration commands"}
	var configPath string
	validate := &cobra.Command{
		Use:   "validate",
		Short: "Load the configuration, resolve secrets from the environment, and report every problem",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadFile(configPath)
			if err != nil {
				return fmt.Errorf("invalid configuration:\n%w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "configuration is valid: %d application(s), %d route(s)\n",
				len(cfg.Applications), len(cfg.Routes))
			return nil
		},
	}
	validate.Flags().StringVarP(&configPath, "config", "c", "relay.yaml", "path to the configuration file")
	cmd.AddCommand(validate)
	return cmd
}
