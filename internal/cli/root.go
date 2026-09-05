// Package cli is proxio's command line: the daemon, and the few things an operator needs a
// shell for.
package cli

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"proxio/internal/app"
	"proxio/internal/config"
	"proxio/internal/tokens"
)

func root() *cobra.Command {
	cmd := &cobra.Command{
		Use:   app.Name,
		Short: "An HTTP relay that makes somebody else's request",
		Long: "proxio takes a token and an encoded URL and makes that request on the\n" +
			"caller's behalf, streaming the answer back with the method, the headers\n" +
			"and the body intact.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	// Cobra's Print helpers write to stderr unless an output is set, which would make
	// `TOKEN=$(proxio token create x)` capture nothing at all — the one thing anybody wants
	// to do with that command.
	cmd.SetOut(os.Stdout)
	cmd.AddCommand(serveCmd(), tokenCmd(), healthcheckCmd(), versionCmd())
	return cmd
}

// Execute runs the command line and returns a process exit code.
func Execute() int {
	if err := root().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, app.Name+":", err)
		return 1
	}
	return 0
}

// setup is what every command that touches data needs: the environment, and the tokens.
//
// config.Load is where the data directory is proved to exist and be writable, so a container
// started without a volume fails here — on `serve` and on every `token` subcommand alike —
// rather than at the first write.
func setup() (*config.Config, *tokens.Store, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	st, err := tokens.Open(cfg.DataDir)
	if err != nil {
		return nil, nil, err
	}
	return cfg, st, nil
}

// logger is JSON on stdout.
//
// JSON because these lines are an access log and something will parse them. stdout because
// proxio's stdout carries nothing else — the one command that prints a secret, `token
// create`, is a different process — and an access log on stdout is what every shipper
// expects to find.
func logger(cfg *config.Config) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version this binary was built from",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.Println(app.Version)
			return nil
		},
	}
}
