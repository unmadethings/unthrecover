package offsite

import (
	"context"
	"fmt"

	"github.com/unmadethings/unthrecover/internal/config"
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
	be, err := OpenBackend(ctx, cfg.Offsite)
	if err != nil {
		return nil, err
	}
	return &Copier{Backend: be, Keys: keys, Prefix: cfg.Offsite.Prefix, Store: store, Repo: r, Actor: actor}, nil
}
