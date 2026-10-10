package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
)

// The tombstone reconciler (#1211) finishes project deletes that were started
// and not completed. It is ON wherever the project provisioner exists, because
// a tombstoned project that nothing finishes stays refused-for-new-work for
// ever. The flag exists to switch it off on a replica that should not run it
// (an incident, a migration window), not to opt in.
const (
	projectDeletionReconcilerEnv = "ELITEA_PROJECT_DELETION_RECONCILER_ENABLED"
	projectDeletionGraceEnv      = "ELITEA_PROJECT_DELETION_RECONCILER_GRACE"
)

type projectDeletionReconcilerSettings struct {
	Enabled bool
	Grace   time.Duration
}

func projectDeletionReconcilerFromEnv(lookup func(string) (string, bool)) (projectDeletionReconcilerSettings, error) {
	settings := projectDeletionReconcilerSettings{
		Enabled: true,
		Grace:   runtimecomposition.DefaultProjectDeletionGracePeriod,
	}
	if raw, ok := lookup(projectDeletionReconcilerEnv); ok && strings.TrimSpace(raw) != "" {
		enabled, err := strconv.ParseBool(strings.TrimSpace(raw))
		if err != nil {
			return settings, fmt.Errorf("%s must be a boolean, got %q", projectDeletionReconcilerEnv, raw)
		}
		settings.Enabled = enabled
	}
	if raw, ok := lookup(projectDeletionGraceEnv); ok && strings.TrimSpace(raw) != "" {
		grace, err := time.ParseDuration(strings.TrimSpace(raw))
		if err != nil || grace <= 0 {
			return settings, fmt.Errorf("%s must be a positive duration such as 5m, got %q", projectDeletionGraceEnv, raw)
		}
		settings.Grace = grace
	}
	return settings, nil
}

// startProjectDeletionReconciler runs the reconciler over the one shared project
// provisioner until ctx ends. A construction error is returned so the caller can
// fail startup; a loop that stops is logged, not fatal, as for the other
// background loops.
func startProjectDeletionReconciler(
	ctx context.Context,
	settings projectDeletionReconcilerSettings,
	provisioner *projectprovisioning.Provisioner,
	pool *pgxpool.Pool,
	logger *slog.Logger,
) error {
	if !settings.Enabled {
		logger.Info("project deletion reconciler is disabled", "env", projectDeletionReconcilerEnv)
		return nil
	}
	reconciler, err := runtimecomposition.NewProjectDeletionReconciler(
		pool, provisioner,
		runtimecomposition.ProjectDeletionReconcilerConfig{GracePeriod: settings.Grace}, logger)
	if err != nil {
		return err
	}
	go func() {
		if err := reconciler.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Error("the project deletion reconciler stopped", "err", err)
		}
	}()
	return nil
}
