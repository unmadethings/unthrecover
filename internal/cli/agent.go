package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/unmadethings/unthrecover/internal/agent"
	"github.com/unmadethings/unthrecover/internal/config"
	"github.com/unmadethings/unthrecover/internal/repo"
	"github.com/unmadethings/unthrecover/internal/runner"
	"github.com/unmadethings/unthrecover/internal/verify"
)

func newAgentCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{Use: "agent", Short: "Connect this server to Unth Recover Cloud (or another control plane)"}

	var url, token string
	enroll := &cobra.Command{
		Use:   "enroll",
		Short: "Register this server with a one-time token from the control plane (Add server)",
		Long: `Enroll creates this server's keys (in <data-dir>/agent, 0600) and registers its public keys
with the control plane using the one-time token shown by "Add server". From then on every request
is signed with the server's own key; no password or token is stored.

The token can be passed with --token or UNTH_ENROLL_TOKEN (keeps it out of the process list).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if token == "" {
				token = os.Getenv("UNTH_ENROLL_TOKEN")
			}
			if url == "" || token == "" {
				return &ExitError{Code: ExitUsage, Err: errors.New("--url and --token (or UNTH_ENROLL_TOKEN) are required")}
			}
			r, err := repo.Open(app.DataDir)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
			defer cancel()
			st, err := agent.Enroll(ctx, r.Root, url, token, &http.Client{Timeout: 30 * time.Second})
			if err != nil {
				return &ExitError{Code: ExitFailure, Err: err}
			}
			fmt.Fprintf(app.Out, "Enrolled as %s in %q at %s.\nStart the agent: unth agent run  (systemd: systemctl enable --now unth-agent)\n",
				st.AgentID, st.OrgName, st.URL)
			return nil
		},
	}
	enroll.Flags().StringVar(&url, "url", "", "control plane URL, e.g. https://app.unthrecover.com")
	enroll.Flags().StringVar(&token, "token", "", "one-time enrollment token")

	var once bool
	run := &cobra.Command{
		Use:   "run",
		Short: "Run the agent: heartbeat, jobs and commands from the control plane, local schedule",
		Long: `Run keeps this server connected: it sends a heartbeat (outbound HTTPS only), syncs the jobs
managed in the control plane, runs them on their schedule — also while the control plane is
unreachable — executes "Run now" / "Verify now" commands and reports results (summaries only:
statuses, sizes, durations; never backup data). Secrets arrive encrypted to this server's key and
are decrypted only for the duration of a run.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := repo.Open(app.DataDir)
			if err != nil {
				return err
			}
			a, err := agent.New(r.Root, agentHooks(app), app.Log, app.Redactor.Redact)
			if a != nil {
				a.Secrets = app.Redactor.Add
			}
			if err != nil {
				return &ExitError{Code: ExitUsage, Err: err}
			}
			if !once {
				fmt.Fprintf(app.Out, "Agent %s connected to %s\n", a.Client.State.AgentID, a.Client.State.URL)
			}
			return a.Run(cmd.Context(), once)
		},
	}
	run.Flags().BoolVar(&once, "once", false, "one cycle: heartbeat, commands, flush (for tests and cron)")

	status := &cobra.Command{
		Use:   "status",
		Short: "Show the enrollment of this server",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			r, err := repo.Open(app.DataDir)
			if err != nil {
				return err
			}
			st, err := agent.LoadState(r.Root)
			if err != nil {
				return &ExitError{Code: ExitUsage, Err: err}
			}
			return writeJSON(app.Out, st)
		},
	}
	cmd.AddCommand(enroll, run, status)
	return cmd
}

// agentHooks connects the agent to the real engines.
func agentHooks(app *App) agent.Hooks {
	toMap := func(v any) map[string]any {
		b, _ := json.Marshal(v)
		var m map[string]any
		_ = json.Unmarshal([]byte(app.Redactor.Redact(string(b))), &m)
		return m
	}
	return agent.Hooks{
		RunJob: func(ctx context.Context, cfg *config.Config) map[string]any {
			store, r, err := app.openStore(ctx)
			if err != nil {
				return map[string]any{"outcome": "failure", "error_message": app.Redactor.RedactError(err)}
			}
			defer store.Close()
			rn := &runner.Runner{Store: store, Repo: r, Docker: dockerClient, Redactor: app.Redactor, Log: app.Log}
			return toMap(rn.Run(ctx, cfg))
		},
		Verify: func(ctx context.Context, backupID string) map[string]any {
			store, r, err := app.openStore(ctx)
			if err != nil {
				return map[string]any{"backup_id": backupID, "final_status": "FAILED", "error_message": app.Redactor.RedactError(err)}
			}
			defer store.Close()
			eng := &verify.Engine{Store: store, Repo: r, Docker: dockerClient, Redactor: app.Redactor, Log: app.Log}
			rep, err := eng.Run(ctx, backupID, verify.Options{})
			if rep == nil {
				return map[string]any{"backup_id": backupID, "final_status": "FAILED", "error_message": app.Redactor.RedactError(err)}
			}
			return map[string]any{"backup_id": backupID, "run_id": rep.RunID, "final_status": rep.FinalStatus,
				"error_category": rep.ErrorCategory, "error_message": app.Redactor.Redact(rep.ErrorMessage)}
		},
		Status: func(ctx context.Context) (string, int64) {
			free, _ := repo.FreeBytes(app.DataDir)
			dc, err := dockerClient()
			if err != nil {
				return "unreachable", free
			}
			c, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			v, err := dc.Negotiate(c)
			if err != nil {
				return "unreachable", free
			}
			return "Engine " + v.Version, free
		},
	}
}
