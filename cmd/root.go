package cmd

import (
	"github.com/grovetools/core/cli"
	"github.com/spf13/cobra"
)

var rootCmd *cobra.Command

func init() {
	rootCmd = cli.NewStandardCommand("groved", "Grove ecosystem background daemon")

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
	return rootCmd.Execute()
}
