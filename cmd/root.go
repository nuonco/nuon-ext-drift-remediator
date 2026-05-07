// Package cmd wires the cobra command tree for nuon-ext-drift-remediator.
package cmd

import "github.com/spf13/cobra"

// BuildVersion is stamped at build time via goreleaser ldflags. It defaults
// to "dev" for local builds.
var BuildVersion = "dev"

// Execute runs the root command and returns any error reported by cobra.
func Execute() error {
	return newRootCommand().Execute()
}

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "nuon-ext-drift-remediator",
		Short: "Webhook receiver that auto-remediates Nuon drift-detected events",
		Long: `nuon-ext-drift-remediator listens for ctl-api lifecycle webhooks and, when an
event carries the 'event:drift.detected' interest slug, triggers the
appropriate remediation against the public Nuon API:

  * sandbox drift  → POST /v1/installs/{install_id}/reprovision-sandbox
  * component drift → POST /v1/installs/{install_id}/components/{component_id}/deploys

Remediation runs unconditionally — there is no plan-only safety net. The
extension is intended for environments where any detected drift should be
reverted automatically.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       BuildVersion,
	}

	root.AddCommand(newServeCommand())

	return root
}
