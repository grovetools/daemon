package cmd

import (
	"github.com/grovetools/core/cli"
	"github.com/grovetools/core/version"
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

	// Support --version on the root command (same pattern as grove/flow/nb).
	// Without it the flag parsed as unknown, so scripts probing a binary's
	// version got a usage error from a flag every sibling accepts.
	vInfo := version.GetInfo()
	rootCmd.Version = vInfo.Version
	cli.SetVersionTemplate(rootCmd, cli.VersionInfo{
		Version:   vInfo.Version,
		Commit:    vInfo.Commit,
		BuildDate: vInfo.BuildDate,
		BuildArch: vInfo.Platform,
	})

	// Add commands
	rootCmd.AddCommand(newVersionCmd())

	// Mount relocated daemon commands
	rootCmd.AddCommand(newGrovedStartCmd())
	rootCmd.AddCommand(newGrovedStopCmd())
	rootCmd.AddCommand(newGrovedUpgradeCmd())
	rootCmd.AddCommand(newGrovedStatusCmd())
	rootCmd.AddCommand(newGrovedKillCmd())
	rootCmd.AddCommand(newGrovedClawsCmd())
	rootCmd.AddCommand(newGrovedConfigCmd())
	rootCmd.AddCommand(newGrovedMonitorCmd())
	rootCmd.AddCommand(newGrovedHealthCmd())
}

func Execute() error {
	return cli.Execute(rootCmd)
}
