// Package cli implements the `unth` command-line interface.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/unmadethings/unthrecover/internal/docker"
	"github.com/unmadethings/unthrecover/internal/failure"
	"github.com/unmadethings/unthrecover/internal/metadata"
	"github.com/unmadethings/unthrecover/internal/repo"
	"github.com/unmadethings/unthrecover/internal/security"
)

// Exit codes.
const (
	ExitOK          = 0
	ExitFailure     = 1
	ExitUsage       = 2
	ExitNotVerified = 3
	ExitCancelled   = 130
)

// ExitError carries an exit code.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("exit %d", e.Code)
	}
	return e.Err.Error()
}
func (e *ExitError) Unwrap() error { return e.Err }

// App holds global state shared by commands.
type App struct {
	DataDir  string
	LogLevel string
	Out      io.Writer
	Err      io.Writer
	Redactor *security.Redactor
	Log      *slog.Logger
}

// DefaultDataDir returns $UNTH_DATA_DIR or ~/.local/share/unthrecover.
func DefaultDataDir() string {
	if d := os.Getenv("UNTH_DATA_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "unthrecover")
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".local", "share", "unthrecover")
	}
	return "unth-data"
}

// NewRootCmd builds the command tree.
func NewRootCmd(out, errOut io.Writer) (*cobra.Command, *App) {
	app := &App{Out: out, Err: errOut, Redactor: security.NewRedactor()}
	root := &cobra.Command{
		Use:   "unth",
		Short: "Unth Recover — prove you can recover, not just that you backed up",
		Long: `Unth Recover backs up PostgreSQL databases and PROVES they can be recovered by
restoring every verified backup into a fresh, isolated container and checking
the data, schema and constraints against the source snapshot.

Your backup is not your recovery.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			var lvl slog.Level
			if err := lvl.UnmarshalText([]byte(app.LogLevel)); err != nil {
				return &ExitError{Code: ExitUsage, Err: fmt.Errorf("invalid --log-level %q", app.LogLevel)}
			}
			app.Log = slog.New(newRedactingHandler(slog.NewTextHandler(app.Err, &slog.HandlerOptions{Level: lvl}), app.Redactor))
			return nil
		},
	}
	root.PersistentFlags().StringVar(&app.DataDir, "data-dir", DefaultDataDir(), "data directory for metadata, backups and reports (env UNTH_DATA_DIR)")
	root.PersistentFlags().StringVar(&app.LogLevel, "log-level", "warn", "log level: debug, info, warn, error")
	root.SetOut(out)
	root.SetErr(errOut)

	root.AddCommand(newVersionCmd(app), newDoctorCmd(app), newDiscoverCmd(app), newBackupCmd(app),
		newVerifyCmd(app), newReportCmd(app), newCleanupCmd(app), newAuditCmd(app), newRunCmd(app), newPruneCmd(app), newServeCmd(app), newScheduleCmd(app), newKeysCmd(app), newOffsiteCmd(app), newAgentCmd(app))
	return root, app
}

// Execute runs the CLI and returns the process exit code.
func Execute(ctx context.Context, args []string, out, errOut io.Writer) int {
	root, app := NewRootCmd(out, errOut)
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return ExitOK
	}
	msg := err.Error()
	if app.Redactor != nil {
		msg = app.Redactor.Redact(msg)
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		if ee.Err != nil {
			fmt.Fprintln(errOut, "Error:", msg)
			if h := failure.HintOf(ee.Err); h != "" {
				fmt.Fprintln(errOut, "Hint: ", h)
			}
		}
		return ee.Code
	}
	fmt.Fprintln(errOut, "Error:", msg)
	if h := failure.HintOf(err); h != "" {
		fmt.Fprintln(errOut, "Hint: ", h)
	}
	if errors.Is(err, context.Canceled) {
		return ExitCancelled
	}
	return ExitFailure
}

func (a *App) openStore(ctx context.Context) (*metadata.Store, *repo.Repo, error) {
	r, err := repo.Open(a.DataDir)
	if err != nil {
		return nil, nil, err
	}
	s, err := metadata.Open(ctx, r.Root)
	if err != nil {
		return nil, nil, err
	}
	return s, r, nil
}

func dockerClient() (*docker.Client, error) { return docker.NewFromEnv() }

// redactingHandler redacts secrets from log messages and string attributes.
type redactingHandler struct {
	h slog.Handler
	r *security.Redactor
}

func newRedactingHandler(h slog.Handler, r *security.Redactor) slog.Handler {
	return &redactingHandler{h: h, r: r}
}

func (h *redactingHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.h.Enabled(ctx, l)
}

func (h *redactingHandler) Handle(ctx context.Context, rec slog.Record) error {
	nr := slog.NewRecord(rec.Time, rec.Level, h.r.Redact(rec.Message), rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		nr.AddAttrs(h.redactAttr(a))
		return true
	})
	return h.h.Handle(ctx, nr)
}

func (h *redactingHandler) redactAttr(a slog.Attr) slog.Attr {
	switch a.Value.Kind() {
	case slog.KindString:
		return slog.String(a.Key, h.r.Redact(a.Value.String()))
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok {
			return slog.String(a.Key, h.r.RedactError(err))
		}
		return slog.String(a.Key, h.r.Redact(fmt.Sprint(a.Value.Any())))
	}
	return a
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	red := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		red[i] = h.redactAttr(a)
	}
	return &redactingHandler{h: h.h.WithAttrs(red), r: h.r}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{h: h.h.WithGroup(name), r: h.r}
}
