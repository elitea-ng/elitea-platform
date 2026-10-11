package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
)

// The project-deletion reconciler (#1211) drains the cleanup journal
// (centry.project_deletions): it finishes the cleanup of deleted projects whose
// steps did not all complete. It is ON wherever the project provisioner exists,
// because a journal row that nothing drains leaves the project's schema, bytes
// or PgVector database behind for ever. The flag exists to switch it off on a
// replica that should not run it (an incident, a migration window), not to opt
// in.
const (
	projectDeletionReconcilerEnv = "ELITEA_PROJECT_DELETION_RECONCILER_ENABLED"
	projectDeletionGraceEnv      = "ELITEA_PROJECT_DELETION_RECONCILER_GRACE"
	// projectDeleteBudgetEnv is how long DELETE of a project waits for the slow
	// cleanup before it answers 202 and leaves the rest to the journal.
	projectDeleteBudgetEnv = "ELITEA_PROJECT_DELETE_REQUEST_BUDGET"
)

// projectDeleteBudgetFromEnv reads the in-request cleanup budget. Unset or
// empty is zero, which the route reads as its default (20s).
func projectDeleteBudgetFromEnv(lookup func(string) (string, bool)) (time.Duration, error) {
	raw, ok := lookup(projectDeleteBudgetEnv)
	if !ok || strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	budget, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || budget <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration such as 20s, got %q", projectDeleteBudgetEnv, raw)
	}
	return budget, nil
}

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
	logger *slog.Logger,
) error {
	if !settings.Enabled {
		logger.Info("project deletion reconciler is disabled", "env", projectDeletionReconcilerEnv)
		return nil
	}
	reconciler, err := runtimecomposition.NewProjectDeletionReconciler(
		provisioner,
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
