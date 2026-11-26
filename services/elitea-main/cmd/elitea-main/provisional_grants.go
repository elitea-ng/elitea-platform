package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/provisionalgrants"
)

// provisionalGrantsProbeInterval is how often a boot that found no table
// looks again.
const provisionalGrantsProbeInterval = time.Minute

// enableProvisionalGrants switches on the readers of the provisional grants
// (ADR-0059 R2.12, shared 0284): the resolver's unions and the project access
// predicate read elitea_auth.admin_provisional_grants only once this found
// the table. Without it (a database the shared corpus has not reached yet) a
// provisional grant grants nothing, which is the safe direction, and the
// boot says so. It then looks again every minute until the table exists, so
// the migrations need no restart; the writers check too
// (provisionalgrants.EnsureEnabled) and refuse a provisional write until it
// does.
func enableProvisionalGrants(ctx context.Context, q provisionalgrants.Querier, logger *slog.Logger) {
	present, err := provisionalgrants.EnableIfPresent(ctx, q)
	switch {
	case err != nil:
		logger.Warn("provisional grants stay off for now: the table could not be read", "err", err)
	case !present:
		logger.Warn("provisional grants stay off for now: " + provisionalgrants.Table + " does not exist yet; " +
			"a role or membership given under an elevation is refused until the shared migrations create it")
	default:
		logger.Info("provisional grants on")
		return
	}
	go probeProvisionalGrants(ctx, q, logger, provisionalGrantsProbeInterval)
}

// probeProvisionalGrants looks for the table every interval until it exists
// (or a writer found it first) or ctx ends.
func probeProvisionalGrants(ctx context.Context, q provisionalgrants.Querier, logger *slog.Logger, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := provisionalgrants.EnsureEnabled(ctx, q); err == nil {
			logger.Info("provisional grants on")
			return
		}
	}
}
