package cli

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"proxio/internal/app"
	"proxio/internal/proxy"
)

// shutdownGrace matches Docker's default stop timeout. A longer one would only postpone the
// same SIGKILL for a request that is still streaming, and Docker's kill is the backstop
// either way.
const shutdownGrace = 10 * time.Second

func serveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the proxy",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, st, err := setup()
			if err != nil {
				return err
			}
			log := logger(cfg)

			list, err := st.List()
			if err != nil {
				return err
			}
			if len(list) == 0 {
				// A warning rather than a refusal. Refusing would crash-loop the container
				// before anybody could exec into it to create the first token, which is the
				// only way one is ever made.
				log.Warn("no tokens exist, so every request will be refused",
					"fix", "docker exec "+app.Name+" "+app.Name+" token create <label>",
					"path", st.Path())
			}

			srv := &http.Server{
				Addr: app.ListenAddr,
				Handler: proxy.New(proxy.Options{
					Tokens:          st,
					Log:             log,
					DialTimeout:     cfg.DialTimeout,
					ResponseTimeout: cfg.ResponseTimeout,
					MaxRedirects:    cfg.MaxRedirects,
				}),

				// ReadTimeout and WriteTimeout stay zero, and that is the streaming
				// requirement showing up in the server config: either one is a ceiling on
				// how long a proxied request or response may last. What is left bounds the
				// things that genuinely should be bounded — a client that opens a connection
				// and dawdles over its headers, and one that goes idle afterwards.
				ReadHeaderTimeout: 10 * time.Second,
				IdleTimeout:       120 * time.Second,
				ErrorLog:          nil,
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			errc := make(chan error, 1)
			go func() { errc <- srv.ListenAndServe() }()

			log.Info("listening",
				"addr", app.ListenAddr,
				"version", app.Version,
				"tokens", len(list),
				"max_redirects", cfg.MaxRedirects,
			)

			select {
			case err := <-errc:
				if errors.Is(err, http.ErrServerClosed) {
					return nil
				}
				return err
			case <-ctx.Done():
				stop()
				log.Info("shutting down", "grace", shutdownGrace.String())
				sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
				defer cancel()
				return srv.Shutdown(sctx)
			}
		},
	}
}
