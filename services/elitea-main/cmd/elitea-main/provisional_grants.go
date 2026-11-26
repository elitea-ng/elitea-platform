package main

import (
	"context"
	"log/slog"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/provisionalgrants"
)

// enableProvisionalGrants switches on the readers of the provisional grants
// (ADR-0059 R2.12, shared 0284): the resolver's unions and the project access
// predicate read elitea_auth.admin_provisional_grants only once this found
// the table. Without it (a database the shared corpus has not reached yet) a
// provisional grant grants nothing, which is the safe direction, and the
// boot says so; a restart after the migration switches them on.
func enableProvisionalGrants(ctx context.Context, q provisionalgrants.Querier, logger *slog.Logger) {
	present, err := provisionalgrants.EnableIfPresent(ctx, q)
	switch {
	case err != nil:
		logger.Warn("provisional grants stay off: the table could not be read", "err", err)
	case !present:
		logger.Warn("provisional grants stay off: " + provisionalgrants.Table + " does not exist yet; " +
			"a role or membership given under an elevation grants nothing until the shared migrations ran and the service restarted")
	default:
		logger.Info("provisional grants on")
	}
}
