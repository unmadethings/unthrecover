package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/unmadethings/unthrecover/internal/config"
	"github.com/unmadethings/unthrecover/internal/failure"
	"github.com/unmadethings/unthrecover/internal/metadata"
	"github.com/unmadethings/unthrecover/internal/offsite"
	"github.com/unmadethings/unthrecover/internal/repo"
)

func newKeysCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{Use: "keys", Short: "Manage encryption and signing keys for offsite copies"}
	var dir, format string
	gen := &cobra.Command{
		Use:   "generate",
		Short: "Create an age encryption identity and an Ed25519 signing key",
		Long: `Generate writes, into --dir (created 0700 if missing):
  age.key   age X25519 identity (secret, 0600) -> encryption.identity_file
  sign.key  Ed25519 private key (secret, 0600)  -> signing.key_file
  sign.pub  Ed25519 public key                  -> signing.public_key_file on a restore machine

Existing files are never overwritten. Keep a copy of age.key somewhere other than this server:
without it, encrypted offsite copies cannot be restored. For an offline recovery key, run
generate on another machine and add its public key to encryption.recipients.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkFormat(format); err != nil {
				return err
			}
			g, err := offsite.GenerateKeys(dir)
			if err != nil {
				return &ExitError{Code: ExitFailure, Err: err}
			}
			if format == "json" {
				return writeJSON(app.Out, g)
			}
			fmt.Fprintf(app.Out, "Keys written to %s\n", g.Dir)
			fmt.Fprintf(app.Out, "  age public key:   %s\n", g.AgeRecipient)
			fmt.Fprintf(app.Out, "  signing key:      %s\n\n", g.SignPublic)
			fmt.Fprintf(app.Out, "In the job configuration:\n  encryption:\n    identity_file: %s/%s\n  signing:\n    key_file: %s/%s\n\n",
				g.Dir, offsite.AgeKeyFile, g.Dir, offsite.SignKeyFile)
			fmt.Fprintln(app.Out, "IMPORTANT: store a copy of age.key outside this server (password manager, offline media).")
			fmt.Fprintln(app.Out, "Without it, encrypted offsite copies cannot be restored.")
			return nil
		},
	}
	gen.Flags().StringVar(&dir, "dir", "/etc/unth/keys", "directory for the key files")
	gen.Flags().StringVar(&format, "format", "text", "output format: text|json")
	cmd.AddCommand(gen)
	return cmd
}

func newOffsiteCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{Use: "offsite", Short: "List, check, copy, fetch and prune offsite copies (S3, Google Cloud Storage, Google Drive via rclone, ...)"}
	var cfgPath, format string
	var dryRun bool
	open := func(ctx context.Context) (*config.Config, *metadata.Store, *offsite.Copier, error) {
		if err := checkFormat(format); err != nil {
			return nil, nil, nil, err
		}
		cfg, err := loadConfig(cfgPath)
		if err != nil {
			return nil, nil, nil, err
		}
		if !cfg.Offsite.Enabled() {
			return nil, nil, nil, &ExitError{Code: ExitUsage, Err: errors.New("the job has no offsite: section")}
		}
		store, r, err := app.openStore(ctx)
		if err != nil {
			return nil, nil, nil, err
		}
		cp, err := offsite.Open(ctx, cfg, store, r, repo.Actor())
		if err != nil {
			_ = store.Close()
			if failure.CategoryOf(err) == failure.ConfigInvalid {
				return nil, nil, nil, &ExitError{Code: ExitUsage, Err: err}
			}
			return nil, nil, nil, err
		}
		return cfg, store, cp, nil
	}
	withTimeout := func(ctx context.Context, cfg *config.Config) (context.Context, context.CancelFunc) {
		return context.WithTimeout(ctx, cfg.Offsite.Timeout.D())
	}
	addFlags := func(c *cobra.Command) {
		c.Flags().StringVarP(&cfgPath, "config", "c", "", "job configuration file (required)")
		c.Flags().StringVar(&format, "format", "text", "output format: text|json")
		_ = c.MarkFlagRequired("config")
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List the complete copies of this job's workload on the offsite target",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, store, cp, err := open(cmd.Context())
			if err != nil {
				return err
			}
			defer store.Close()
			copies, err := cp.List(cmd.Context(), cfg.Source.Name)
			if err != nil {
				return err
			}
			if format == "json" {
				if copies == nil {
					copies = []offsite.RemoteCopy{}
				}
				return writeJSON(app.Out, copies)
			}
			fmt.Fprintf(app.Out, "Offsite target: %s\n", cp.Backend.Target())
			if len(copies) == 0 {
				fmt.Fprintln(app.Out, "No copies.")
				return nil
			}
			tw := tabwriter.NewWriter(app.Out, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "BACKUP\tCREATED\tSIZE\tENCRYPTED\tSIGNED\tSTATE")
			for _, c := range copies {
				state := "complete"
				if c.Error != "" {
					state = "UNREADABLE: " + c.Error
				}
				fmt.Fprintf(tw, "%s\t%s\t%d\t%v\t%v\t%s\n", c.BackupID, c.CreatedAt.Format(time.RFC3339), c.SizeBytes, c.Encrypted, c.Signed, state)
			}
			return tw.Flush()
		},
	}
	addFlags(list)

	check := &cobra.Command{
		Use:   "check <backup-id>",
		Short: "Download a copy again and prove it (signature, hashes, decryption)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, store, cp, err := open(cmd.Context())
			if err != nil {
				return err
			}
			defer store.Close()
			b, err := store.GetBackup(cmd.Context(), args[0])
			if err != nil {
				return &ExitError{Code: ExitUsage, Err: err}
			}
			ctx, cancel := withTimeout(cmd.Context(), cfg)
			defer cancel()
			res, err := cp.Check(ctx, b)
			if err != nil {
				fmt.Fprintf(app.Out, "Offsite copy of %s: NOT OK — %s\n", b.ID, app.Redactor.RedactError(err))
				if h := failure.HintOf(err); h != "" {
					fmt.Fprintf(app.Out, "Suggested fix: %s\n", h)
				}
				return &ExitError{Code: ExitNotVerified, Err: err}
			}
			if format == "json" {
				return writeJSON(app.Out, res)
			}
			fmt.Fprintf(app.Out, "Offsite copy of %s: REMOTE_VERIFIED\n  target:    %s/%s\n  encrypted: %v\n  signature: %s\n",
				b.ID, res.Target, res.Location, res.Encrypted, sigText(res))
			return nil
		},
	}
	addFlags(check)

	push := &cobra.Command{
		Use:   "push <backup-id>",
		Short: "Copy an existing VERIFIED backup offsite (unth run does this automatically)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, store, cp, err := open(cmd.Context())
			if err != nil {
				return err
			}
			defer store.Close()
			b, err := store.GetBackup(cmd.Context(), args[0])
			if err != nil {
				return &ExitError{Code: ExitUsage, Err: err}
			}
			if b.WorkloadID != cfg.Source.Name {
				return &ExitError{Code: ExitUsage, Err: fmt.Errorf("backup %s belongs to workload %q, not %q", b.ID, b.WorkloadID, cfg.Source.Name)}
			}
			ctx, cancel := withTimeout(cmd.Context(), cfg)
			defer cancel()
			rec, err := cp.Copy(ctx, b)
			if format == "json" && rec != nil {
				_ = writeJSON(app.Out, rec)
			}
			if err != nil {
				fmt.Fprintf(app.Out, "Offsite copy FAILED: %s\n", app.Redactor.RedactError(err))
				return &ExitError{Code: ExitOffsiteFailed, Err: err}
			}
			if format == "text" {
				fmt.Fprintf(app.Out, "Copied and proven: %s/%s (encrypted=%v signed=%v)\n", rec.Target, rec.Location, rec.Encrypted, rec.Signed)
			}
			return nil
		},
	}
	addFlags(push)

	fetch := &cobra.Command{
		Use:   "fetch <backup-id>",
		Short: "Disaster recovery: download a copy, prove it and import it as a local backup",
		Long: `Fetch downloads a copy from the offsite target, checks signature (when signing.key_file or
signing.public_key_file is set), hashes and decryption, and imports it into the local data
directory as a NOT_VERIFIED backup. Run "unth verify <backup-id>" next to prove it restores.
Use it on a new machine after the original server is lost: same job file, same keys.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, store, cp, err := open(cmd.Context())
			if err != nil {
				return err
			}
			defer store.Close()
			ctx, cancel := withTimeout(cmd.Context(), cfg)
			defer cancel()
			b, res, err := cp.Fetch(ctx, cfg.Source.Name, args[0])
			if err != nil {
				fmt.Fprintf(app.Out, "Fetch FAILED: %s\n", app.Redactor.RedactError(err))
				return &ExitError{Code: ExitNotVerified, Err: err}
			}
			if format == "json" {
				return writeJSON(app.Out, map[string]any{"backup_id": b.ID, "sha256": b.SHA256, "size_bytes": b.SizeBytes,
					"signature_checked": res.SignatureChecked, "encrypted": res.Encrypted, "verification_status": b.VerificationStatus})
			}
			fmt.Fprintf(app.Out, "Fetched %s (%d bytes, sha256 %s)\n  encrypted: %v\n  signature: %s\n\nNext: unth verify %s\n",
				b.ID, b.SizeBytes, b.SHA256, res.Encrypted, sigText(res), b.ID)
			return nil
		},
	}
	addFlags(fetch)

	prune := &cobra.Command{
		Use:   "prune",
		Short: "Apply offsite retention (offsite.retention, default: the job's retention)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, store, cp, err := open(cmd.Context())
			if err != nil {
				return err
			}
			defer store.Close()
			if !cfg.Offsite.Retention.Enabled() {
				return &ExitError{Code: ExitUsage, Err: errors.New("no retention rules (offsite.retention or retention)")}
			}
			ctx, cancel := withTimeout(cmd.Context(), cfg)
			defer cancel()
			res, err := cp.Prune(ctx, cfg.Source.Name, cfg.Offsite.Retention, dryRun)
			if format == "json" && res != nil {
				_ = writeJSON(app.Out, res)
			} else if res != nil {
				verb := "Deleted"
				if dryRun {
					verb = "Would delete"
				}
				fmt.Fprintf(app.Out, "%s %d offsite copy(ies) on %s: %s\n", verb, len(res.Deleted), res.Target, strings.Join(res.Deleted, ", "))
			}
			return err
		},
	}
	addFlags(prune)
	prune.Flags().BoolVar(&dryRun, "dry-run", false, "only show what would be deleted")

	cmd.AddCommand(list, check, push, fetch, prune)
	return cmd
}

func sigText(r *offsite.CheckResult) string {
	switch {
	case r.SignatureChecked:
		return "valid (" + r.SignedBy + ")"
	case r.Descriptor != nil && r.Descriptor.Signed:
		return "present but NOT checked (set signing.key_file or signing.public_key_file)"
	default:
		return "not signed"
	}
}
