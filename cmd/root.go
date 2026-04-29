// Package cmd implements the gmcli command tree.
package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/fdsouvenir/gmcli/internal/logging"
	"github.com/fdsouvenir/gmcli/internal/paths"
)

// globalFlags is populated by persistent flags on the root command and
// consumed by subcommands via context or direct reference.
type globalFlags struct {
	storeDir string
	logLevel string
	jsonOut  bool
	readOnly bool
}

var flags globalFlags

// Root constructs the top-level *cobra.Command.
func Root() *cobra.Command {
	root := &cobra.Command{
		Use:           "gmcli",
		Short:         "Local-first Google Messages CLI and archive",
		Long:          "gmcli pairs with Google Messages, archives conversations into SQLite, and exposes them through a CLI.\nLicensed under AGPL-3.0; see `gmcli version` for attribution details.",
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.PersistentFlags().StringVar(&flags.storeDir, "store", "", "data directory (default: $XDG_DATA_HOME/gmcli)")
	root.PersistentFlags().StringVar(&flags.logLevel, "log-level", "info", "log verbosity: trace, debug, info, warn, error")
	root.PersistentFlags().BoolVar(&flags.jsonOut, "json", false, "emit machine-readable JSON output where applicable")
	root.PersistentFlags().BoolVar(&flags.readOnly, "read-only", true, "block any operation that would write to the phone")

	root.AddCommand(authCmd())
	root.AddCommand(syncCmd())
	root.AddCommand(versionCmd())
	root.AddCommand(doctorCmd())
	return root
}

// resolveLayout applies --store and ensures the directory tree exists.
func resolveLayout() (paths.Layout, error) {
	layout, err := paths.Resolve(flags.storeDir)
	if err != nil {
		return paths.Layout{}, err
	}
	if err := layout.EnsureDirs(); err != nil {
		return paths.Layout{}, err
	}
	return layout, nil
}

func newLogger() zerolog.Logger {
	return logging.Default(flags.logLevel, flags.jsonOut)
}

// signalContext returns a context cancelled on SIGINT/SIGTERM. Callers must
// invoke the returned cancel to release the signal handler.
func signalContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-sigs:
			cancel()
		case <-ctx.Done():
		}
		signal.Stop(sigs)
	}()
	return ctx, cancel
}

// dieIfErr writes err to stderr and exits non-zero. Used as the very last
// step in command Runs so cobra doesn't print its own usage on operational
// failures.
func dieIfErr(err error) {
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "gmcli:", err)
	os.Exit(1)
}
