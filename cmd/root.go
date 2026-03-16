package cmd

import (
	"github.com/grovetools/core/cli"
	"github.com/spf13/cobra"
)

var rootCmd *cobra.Command

func init() {
	rootCmd = cli.NewStandardCommand("groved", "Grove ecosystem background daemon")

	// Set long description
	rootCmd.Long = `The Grove daemon runs in the background to provide:
  • Automatic skill synchronization when config or skill files change
  • Workspace discovery and state tracking
  • Git status monitoring and session collection
  • Event hooks for custom automation`

	// Add commands
	rootCmd.AddCommand(newVersionCmd())

	// Mount relocated daemon commands
	rootCmd.AddCommand(newGrovedStartCmd())
	rootCmd.AddCommand(newGrovedStopCmd())
	rootCmd.AddCommand(newGrovedStatusCmd())
	rootCmd.AddCommand(newGrovedConfigCmd())
	rootCmd.AddCommand(newGrovedMonitorCmd())
}

func Execute() error {
	return cli.Execute(rootCmd)
}
