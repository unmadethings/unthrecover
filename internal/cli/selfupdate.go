package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/unmadethings/unthrecover/internal/buildinfo"
	"github.com/unmadethings/unthrecover/internal/selfupdate"
)

// newSelfUpdateCmd installs a newer official release (root). unth-update.service
// runs it with --request-dir when the agent asks for an update.
func newSelfUpdateCmd(app *App) *cobra.Command {
	var version, requestDir string
	cmd := &cobra.Command{
		Use:   "self-update",
		Short: "Install a newer official release of unth (run as root)",
		Long: "Downloads the release from GitHub, checks it against the release's SHA256SUMS and installs it the\n" +
			"way unth was installed (.deb, .rpm or the binary), then restarts unth-agent. Older or equal versions are refused.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if (version == "") == (requestDir == "") {
				return &ExitError{Code: ExitUsage, Err: errors.New("use exactly one of --version or --request-dir")}
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 20*time.Minute)
			defer cancel()
			u := &selfupdate.Updater{Current: buildinfo.Version}
			var err error
			if requestDir != "" {
				err = u.ApplyRequest(ctx, requestDir)
			} else {
				err = u.Apply(ctx, version)
			}
			if err != nil {
				return err
			}
			if version != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "installed %s\n", version)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&version, "version", "", "release tag to install, e.g. v0.6.0")
	cmd.Flags().StringVar(&requestDir, "request-dir", "", "agent directory with an update-request file (unth-update.service)")
	_ = app
	return cmd
}
