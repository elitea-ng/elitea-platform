package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/libs/go/routines"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker/epoch"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialuse"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	dbrepos "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// The routines of credential and access review (ADR-0044 decision 5,
// #1501), on the routine registry (admin Schedules page), one replica per
// pass, both paused in maintenance.
const (
	credentialStalenessRoutineID = "main.credential_staleness"
	permissionTrimmingRoutineID  = "main.permission_trimming"
)

// credentialReview is the composed evidence stores.
type credentialReview struct {
	review   *dbrepos.CredentialReview
	trimming *dbrepos.PermissionTrimming
}

// composeCredentialReview builds the stores, installs the process-wide
// credential use recorder and starts its flush loop. A use grant is
// revoked through the grant service with the broker's epoch invalidation,
// as the grant revoke route does.
func composeCredentialReview(ctx context.Context, pool *pgxpool.Pool, invalidations *epoch.Hub, logger *slog.Logger) (credentialReview, error) {
	if pool == nil {
		return credentialReview{}, nil
	}
	appender, err := securityaudit.NewPostgresAppender("elitea-main")
	if err != nil {
		return credentialReview{}, fmt.Errorf("credential review: security audit appender: %w", err)
	}
	grants := dbrepos.NewCredentialGrants(pool, appender)
	if invalidations != nil {
		grants.WithNotifier(invalidations)
	}
	service, err := credentialgrant.NewService(credentialgrant.ServiceConfig{Store: grants})
	if err != nil {
		return credentialReview{}, err
	}
	revoke := func(ctx context.Context, tenantProjectID int64, grantID, actor string) error {
		_, err := service.TransitionGrant(ctx, tenantProjectID, grantID, credentialgrant.ActionRevoke, actor)
		return err
	}
	review, err := dbrepos.NewCredentialReview(pool, appender, revoke)
	if err != nil {
		return credentialReview{}, err
	}
	trimming, err := dbrepos.NewPermissionTrimming(review)
	if err != nil {
		return credentialReview{}, err
	}
	recorder := credentialuse.New(review, logger)
	credentialuse.Install(recorder)
	go recorder.Run(ctx)
	return credentialReview{review: review, trimming: trimming}, nil
}

// startCredentialReviewRoutines runs main.credential_staleness (daily) and
// main.permission_trimming (weekly).
func startCredentialReviewRoutines(ctx context.Context, pool *pgxpool.Pool, stores credentialReview, logger *slog.Logger) {
	if pool == nil || stores.review == nil {
		return
	}
	// Fail closed: an unreadable maintenance switch stops the pass.
	inMaintenance := func(ctx context.Context) (bool, error) {
		state, err := platformconfig.LoadMaintenance(ctx, pool)
		if err != nil {
			return false, err
		}
		return state.Enabled, nil
	}
	for _, routine := range []struct {
		id, description, schedule string
		minInterval               time.Duration
		pass                      func(context.Context) (string, error)
	}{
		{credentialStalenessRoutineID,
			"Flags credentials agents can use that were not used for the Credential Review stale_after_days (default 90) and notifies their owners; with stale_action revoke it also revokes stale personal access tokens and credential use grants (ADR-0044).",
			"24h", time.Hour,
			func(ctx context.Context) (string, error) {
				summary, err := stores.review.StalenessPass(ctx)
				if err != nil {
					return routines.OutcomeError, err
				}
				logger.Info("credential staleness pass", "summary", summary.String())
				return routines.OutcomeOK, nil
			}},
		{permissionTrimmingRoutineID,
			"Recomputes the permission trimming report (grants unused in the Credential Review window, default 30 days) and notifies agent owners of new removal candidates (ADR-0044).",
			"168h", time.Hour,
			func(ctx context.Context) (string, error) {
				summary, err := stores.trimming.TrimmingPass(ctx)
				if err != nil {
					return routines.OutcomeError, err
				}
				logger.Info("permission trimming pass", "summary", summary.String())
				return routines.OutcomeOK, nil
			}},
	} {
		loop := &routines.Loop{
			Backend: routines.NewStore(pool),
			Def: routines.Definition{
				ID: routine.id, Owner: routines.OwnerMain, Description: routine.description,
				DefaultSchedule: routine.schedule, MinInterval: routine.minInterval,
			},
			Logger: logger,
		}
		pass := routine.pass
		id := routine.id
		go loop.Run(ctx, func(ctx context.Context) (string, error) {
			maintenance, err := inMaintenance(ctx)
			if err != nil {
				logger.Error("credential review routine: maintenance switch unreadable", "routine", id, "err", err)
				return routines.OutcomeError, err
			}
			if maintenance {
				return routines.OutcomeMaintenance, nil
			}
			return pass(ctx)
		})
	}
}
