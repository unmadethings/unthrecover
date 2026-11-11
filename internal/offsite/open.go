package offsite

import (
	"context"
	"fmt"

	"github.com/unmadethings/unthrecover/internal/config"
	"github.com/unmadethings/unthrecover/internal/connections"
	"github.com/unmadethings/unthrecover/internal/failure"
	"github.com/unmadethings/unthrecover/internal/metadata"
	"github.com/unmadethings/unthrecover/internal/repo"
)

// OpenBackend creates the backend configured in o.
func OpenBackend(ctx context.Context, o config.Offsite) (Backend, error) {
	switch o.Type {
	case config.OffsiteS3:
		s, err := NewS3(o)
		if err != nil {
			return nil, failure.New(failure.ConfigInvalid, "offsite", err)
		}
		if err := s.Check(ctx); err != nil {
			return nil, failure.New(failure.OffsiteFailed, "offsite target", err).
				WithHint("check offsite.endpoint, offsite.bucket, the access keys and network access to the endpoint")
		}
		return s, nil
	case config.OffsiteGDrive:
		return nil, fmt.Errorf("offsite: type gdrive needs the data directory (use Open)")
	case config.OffsiteRclone:
		r, err := NewRclone(o)
		if err != nil {
			return nil, failure.New(failure.ConfigInvalid, "offsite", err)
		}
		if err := r.Check(ctx); err != nil {
			return nil, failure.New(failure.OffsiteFailed, "offsite target", err).
				WithHint("check offsite.remote and the rclone configuration (rclone listremotes, rclone lsd <remote>)")
		}
		return r, nil
	}
	return nil, fmt.Errorf("offsite: unknown type %q", o.Type)
}

// Open builds a Copier from a job configuration.
func Open(ctx context.Context, cfg *config.Config, store *metadata.Store, r *repo.Repo, actor string) (*Copier, error) {
	if !cfg.Offsite.Enabled() {
		return nil, fmt.Errorf("no offsite target configured (offsite: in the job configuration)")
	}
	keys, err := LoadKeys(cfg.Encryption.IdentityFile, cfg.Encryption.Recipients, cfg.Signing.KeyFile, cfg.Signing.PublicKeyFile)
	if err != nil {
		return nil, failure.New(failure.ConfigInvalid, "keys", err)
	}
	var be Backend
	if cfg.Offsite.Type == config.OffsiteGDrive {
		gd, gerr := NewGDrive(connections.DefaultGoogle(), connections.NewStore(r.Root), cfg.Offsite.Connection)
		if gerr != nil {
			return nil, failure.New(failure.ConfigInvalid, "offsite", gerr)
		}
		if gerr := gd.Check(ctx); gerr != nil {
			return nil, failure.New(failure.OffsiteFailed, "google drive", gerr).
				WithHint("check the connection in the panel (Settings → Connections); connect Google Drive again if access was removed")
		}
		be = gd
	} else if be, err = OpenBackend(ctx, cfg.Offsite); err != nil {
		return nil, err
	}
	tz, err := cfg.Offsite.Location()
	if err != nil {
		return nil, failure.New(failure.ConfigInvalid, "offsite.time_zone", err)
	}
	return &Copier{Backend: be, Keys: keys, Prefix: cfg.Offsite.Prefix, Store: store, Repo: r, Actor: actor, TZ: tz}, nil
}
