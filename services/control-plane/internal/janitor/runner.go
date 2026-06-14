package janitor

import (
	"context"
	"log/slog"
	"time"
)

// sweepInterval is how often the janitor wakes to re-evaluate staging.
const sweepInterval = time.Hour

// Registry is the slice of the OCI registry client the janitor needs.
type Registry interface {
	ListStagingPluginIDs(ctx context.Context) ([]string, error)
	ListStagingVersions(ctx context.Context, id string) ([]string, error)
	ListVersions(ctx context.Context, id string) ([]string, error)
	StagingTagSigned(ctx context.Context, id, tag string) (bool, error)
	DeleteStagingTag(ctx context.Context, id, tag string) error
	CanPruneStaging() bool
}

// Janitor prunes unsigned staging artifacts on a ticker. Construct with New
// and run with Run.
type Janitor struct {
	reg    Registry
	logger *slog.Logger
}

// New builds a Janitor.
func New(reg Registry, logger *slog.Logger) *Janitor {
	return &Janitor{reg: reg, logger: logger}
}

// Run sweeps staging once shortly after boot, then on a ticker, until ctx is
// cancelled.
func (j *Janitor) Run(ctx context.Context) {
	// One sweep a bit after boot, then on a ticker.
	select {
	case <-ctx.Done():
		return
	case <-time.After(15 * time.Second):
		j.Sweep(ctx)
	}

	t := time.NewTicker(sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			j.Sweep(ctx)
		}
	}
}

// Sweep enumerates every staged <id>:<tag>, computes its stagedTag, and prunes
// the ones shouldPrune selects. Fail-safe: one id/tag erroring out logs and
// continues. NEVER touches the trusted namespace.
func (j *Janitor) Sweep(ctx context.Context) {
	canPrune := j.reg.CanPruneStaging()
	ids, err := j.reg.ListStagingPluginIDs(ctx)
	if err != nil {
		j.logger.Warn("janitor: list staging plugin ids", "err", err)
		return
	}
	var pruned, kept int
	for _, id := range ids {
		tags, err := j.reg.ListStagingVersions(ctx, id)
		if err != nil {
			j.logger.Warn("janitor: list staging versions", "plugin", id, "err", err)
			continue
		}
		if len(tags) == 0 {
			continue
		}
		trusted, err := j.trustedSet(ctx, id)
		if err != nil {
			j.logger.Warn("janitor: list trusted versions", "plugin", id, "err", err)
			continue
		}
		for _, tag := range tags {
			st, ok := j.evaluate(ctx, id, tag, trusted)
			if !ok {
				continue
			}
			if !shouldPrune(st) {
				kept++
				continue
			}
			if !canPrune {
				j.logger.Info("janitor: would prune (no delete token wired)",
					"plugin", id, "version", tag,
					"signed", st.Signed, "promoted", st.Promoted)
				continue
			}
			if err := j.reg.DeleteStagingTag(ctx, id, tag); err != nil {
				j.logger.Warn("janitor: delete staging tag", "plugin", id, "version", tag, "err", err)
				continue
			}
			pruned++
			j.logger.Info("janitor: pruned staging artifact",
				"plugin", id, "version", tag,
				"signed", st.Signed, "promoted", st.Promoted,
				"reason", "unsigned")
		}
	}
	j.logger.Info("janitor: sweep complete", "plugins", len(ids), "pruned", pruned, "kept", kept, "delete_enabled", canPrune)
}

// evaluate computes the stagedTag for <id>:<tag>. ok=false on a registry error
// (logged) so the caller skips the tag rather than acting on partial data.
func (j *Janitor) evaluate(ctx context.Context, id, tag string, trusted map[string]bool) (stagedTag, bool) {
	signed, err := j.reg.StagingTagSigned(ctx, id, tag)
	if err != nil {
		j.logger.Warn("janitor: signature check", "plugin", id, "version", tag, "err", err)
		return stagedTag{}, false
	}
	return stagedTag{
		Signed:   signed,
		Promoted: trusted[tag],
	}, true
}

// trustedSet returns the set of promoted (trusted-namespace) tags for id.
func (j *Janitor) trustedSet(ctx context.Context, id string) (map[string]bool, error) {
	tags, err := j.reg.ListVersions(ctx, id)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(tags))
	for _, t := range tags {
		set[t] = true
	}
	return set, nil
}
