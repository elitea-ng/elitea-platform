package configurations

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
)

const tracingConfigurationCategory = "tracing"

var tracingConfigurationTypeFallback = map[string]struct{}{
	"langfuse": {},
}

type tracingAccessChecker interface {
	CanManageTracing(context.Context, int64, int64) (bool, error)
}

type tracingAccessQueries interface {
	CanManageCurrentTracingConfiguration(
		context.Context,
		sqlcgen.CanManageCurrentTracingConfigurationParams,
	) (bool, error)
}

type postgresTracingAccessChecker struct {
	queries tracingAccessQueries
}

func (checker postgresTracingAccessChecker) CanManageTracing(
	ctx context.Context,
	projectID int64,
	userID int64,
) (bool, error) {
	if checker.queries == nil || projectID <= 0 || userID <= 0 {
		return false, errors.New("tracing access context is unavailable")
	}
	return checker.queries.CanManageCurrentTracingConfiguration(
		ctx,
		sqlcgen.CanManageCurrentTracingConfigurationParams{
			ProjectID: projectID,
			UserID:    userID,
		},
	)
}

func (h *Handler) isTracingConfigurationType(typeName string) bool {
	typeName = strings.TrimSpace(typeName)
	if _, fallback := tracingConfigurationTypeFallback[typeName]; fallback {
		return true
	}
	if h.catalog == nil {
		return false
	}
	entry, ok := h.catalog.EntryByType(typeName)
	return ok && currentConfigurationEntryHasCategory(entry, tracingConfigurationCategory)
}

func currentConfigurationEntryHasCategory(
	entry configurationapp.CurrentAvailableConfigurationType,
	category string,
) bool {
	var schema map[string]any
	if json.Unmarshal(entry.ConfigSchema, &schema) != nil {
		return false
	}
	properties, _ := schema["properties"].(map[string]any)
	data, _ := properties["data"].(map[string]any)
	metadata, _ := data["metadata"].(map[string]any)
	categories, _ := metadata["categories"].([]any)
	for _, raw := range categories {
		value, ok := raw.(string)
		if ok && strings.EqualFold(value, category) {
			return true
		}
	}
	return false
}

func (h *Handler) actorCanManageTracing(ctx context.Context, projectID int64) bool {
	user, ok := auth.UserFromContext(ctx)
	if !ok {
		return false
	}
	userID, ok := user.OwningUserID()
	if !ok || h.tracingAccess == nil {
		return false
	}
	allowed, err := h.tracingAccess.CanManageTracing(ctx, projectID, userID)
	if err != nil {
		slog.WarnContext(ctx, "configuration tracing access check failed",
			"project_id", projectID, "user_id", userID, "err", err)
		return false
	}
	return allowed
}

func (h *Handler) tracingWriteForbidden(ctx context.Context, projectID int64, typeName string) bool {
	return h.isTracingConfigurationType(typeName) && !h.actorCanManageTracing(ctx, projectID)
}

func (h *Handler) filterAvailableTracingTypes(
	ctx context.Context,
	projectID int64,
	entries []configurationapp.CurrentAvailableConfigurationType,
) []configurationapp.CurrentAvailableConfigurationType {
	if projectID <= 0 || h.actorCanManageTracing(ctx, projectID) {
		return entries
	}
	filtered := make([]configurationapp.CurrentAvailableConfigurationType, 0, len(entries))
	for _, entry := range entries {
		if !h.isTracingConfigurationType(entry.Type) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func availableProjectID(raw string) int64 {
	projectID, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || projectID <= 0 {
		return 0
	}
	return projectID
}

func (h *Handler) storedConfigurationType(
	ctx context.Context,
	schema string,
	configID string,
) (string, error) {
	var typeName string
	err := h.pool.QueryRow(ctx,
		"SELECT type FROM "+schema+".configuration WHERE "+configurationIDColumn(configID)+" = $1",
		configID,
	).Scan(&typeName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return typeName, nil
}
