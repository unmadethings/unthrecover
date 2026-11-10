package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/unmadethings/unthrecover/internal/security"
	"github.com/unmadethings/unthrecover/internal/web"
)

func newServeCmd(app *App) *cobra.Command {
	var listen, certFile, keyFile, tokenFile string
	var behindProxy bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the authenticated web management panel",
		Long: `Serves the web panel (overview, workloads, backups, verification history, reports,
activity) and its JSON API. Authentication is always required: the admin token is read from
--token-file (0600) or the UNTH_UI_TOKEN environment variable and must be at least 24
characters. Plain HTTP is only allowed on loopback addresses; anything else needs
--tls-cert and --tls-key.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			token := os.Getenv("UNTH_UI_TOKEN")
			if tokenFile != "" {
				t, err := security.ReadSecretFile(tokenFile)
				if err != nil {
					return &ExitError{Code: ExitUsage, Err: err}
				}
				token = t
			}
			token = strings.TrimSpace(token)
			if token == "" {
				return &ExitError{Code: ExitUsage, Err: errors.New("no UI token: set UNTH_UI_TOKEN or --token-file (e.g. `openssl rand -hex 32 > ui.token && chmod 600 ui.token`)")}
			}
			if err := web.ValidateToken(token); err != nil {
				return &ExitError{Code: ExitUsage, Err: err}
			}
			app.Redactor.Add(token)
			tls := certFile != "" || keyFile != ""
			if tls && (certFile == "" || keyFile == "") {
				return &ExitError{Code: ExitUsage, Err: errors.New("--tls-cert and --tls-key must be used together")}
			}
			if err := web.CheckListen(listen, tls, behindProxy); err != nil {
				return &ExitError{Code: ExitUsage, Err: err}
			}
			store, r, err := app.openStore(cmd.Context())
			if err != nil {
				return err
			}
			defer store.Close()
			s := &web.Server{Store: store, Repo: r, Docker: dockerClient, Redactor: app.Redactor, Log: app.Log, Token: token, TLS: tls, BehindProxy: behindProxy}
			srv := &http.Server{Addr: listen, Handler: s.Handler(cmd.Context()), ReadHeaderTimeout: 10 * time.Second,
				ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second}
			// Bind before announcing readiness, so "panel on ..." means the port is open.
			ln, err := net.Listen("tcp", listen)
			if err != nil {
				return fmt.Errorf("listen on %s: %w", listen, err)
			}
			errCh := make(chan error, 1)
			go func() {
				if tls {
					errCh <- srv.ServeTLS(ln, certFile, keyFile)
				} else {
					errCh <- srv.Serve(ln)
				}
			}()
			scheme := "http"
			if tls {
				scheme = "https"
			}
			fmt.Fprintf(app.Out, "Unth Recover panel on %s://%s (data dir %s)\n", scheme, listen, r.Root)
			if behindProxy && !tls {
				fmt.Fprintln(app.Out, "Plain HTTP behind a reverse proxy: TLS must be terminated by the proxy; do not publish this port to a network.")
			}
			select {
			case err := <-errCh:
				if errors.Is(err, http.ErrServerClosed) {
					return nil
				}
				return err
			case <-cmd.Context().Done():
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = srv.Shutdown(ctx)
				return nil
			}
		},
	}
	cmd.Flags().StringVar(&listen, "listen", "127.0.0.1:8484", "address to listen on (non-loopback requires TLS)")
	cmd.Flags().StringVar(&certFile, "tls-cert", "", "TLS certificate file")
	cmd.Flags().StringVar(&keyFile, "tls-key", "", "TLS private key file")
	cmd.Flags().BoolVar(&behindProxy, "behind-proxy", false, "allow plain HTTP on any address because a reverse proxy terminates TLS (never expose the port directly)")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "file containing the admin token (0600); default: $UNTH_UI_TOKEN")
	return cmd
}
