package runtimecomposition

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// logAdmissionCaps states, once at startup, this replica's configured
// admission cap next to the cap persisted for every capability (#966). Every
// capability admits under the one ELITEA_RUNTIME_MAX_OUTSTANDING, and every
// admission fails closed while the two differ, so a mismatch is logged as an
// error naming the one-step fix before the first start meets it. A read
// failure is logged and never fails startup: the admission path still fails
// closed on its own.
func logAdmissionCaps(ctx context.Context, logger *slog.Logger, pool *pgxpool.Pool, configured int64) {
	if logger == nil || pool == nil {
		return
	}
	policies, err := repos.ListAdmissionPolicies(ctx, pool)
	if err != nil {
		logger.Error("runtime admission caps could not be read", "configured", configured, "err", err)
		return
	}
	reportAdmissionCaps(logger, configured, policies)
}

func reportAdmissionCaps(logger *slog.Logger, configured int64, policies []repos.AdmissionPolicy) {
	if len(policies) == 0 {
		logger.Info("runtime admission cap: no capability has a persisted cap yet; the first start of each persists the configured one",
			"configured", configured)
		return
	}
	for _, policy := range policies {
		if policy.MaxOutstanding == configured {
			logger.Info("runtime admission cap",
				"capability", policy.CapabilityID, "configured", configured, "persisted", policy.MaxOutstanding)
			continue
		}
		logger.Error("runtime admission cap MISMATCH: every start of this capability fails closed until the replicas and the persisted cap agree",
			"capability", policy.CapabilityID, "configured", configured, "persisted", policy.MaxOutstanding,
			"fix", "set ELITEA_RUNTIME_MAX_OUTSTANDING to one value on every elitea-main replica, then run `"+repos.AdmissionCapCommand+"` once with it")
	}
}
