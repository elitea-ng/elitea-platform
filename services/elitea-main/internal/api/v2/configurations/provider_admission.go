package configurations

import (
	"context"
	"fmt"
	"log/slog"
	"math"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

// ProviderAdmission decides the status_ok column for a configuration row this
// handler writes (#457).
//
// This handler is the write path every deployed stack actually serves. The
// reviewed mutation route needs ELITEA_CONFIGURATIONS_MUTATION_ENABLED, and no
// deployment file turns that on, so nothing else answers
// POST /api/v2/configurations/configurations/{projectID} in a shipped install.
// The write starts status_ok at false, and the LLM gateway admits only
// status_ok = true. A saved credential therefore stays invisible until the
// admission decision accepts it.
//
// The remedy is not to store true. It is to make this route reach the same
// answer the configuration lifecycle reaches: expand the row's declared
// references, redeem its hidden secrets, and apply the project-own-LLM policy.
// The application type behind this interface holds that single decision.
type ProviderAdmission interface {
	AdmitCurrentProviderConfiguration(
		context.Context,
		configurationapp.CurrentConfigurationLifecycleSnapshot,
	) (configurationapp.CurrentProviderAdmissionDecision, error)
}

// WithProviderAdmission wires the status_ok decision (#457). nil leaves every
// written row at the pending false value, which is the behaviour before this
// option existed. It is nil only where the Configurations runtime itself is
// absent, because that runtime owns the vault and the expander the decision
// reads.
func WithProviderAdmission(admission ProviderAdmission) Option {
	return func(handler *Handler) {
		handler.providerAdmission = admission
	}
}

// admitConfiguration resolves the status_ok value for one stored row and
// writes it when it differs from what the row already holds.
//
// It runs after the row is stored, not before. The lifecycle uses the same
// order — it writes a pending status, resolves, then writes the healthy
// status — and running the resolution on the stored snapshot means the
// decision is made about the exact row a reader will read. The resolution
// reads other rows and the project vault through the same pool, so it is
// deliberately not held inside the write transaction.
//
// The return value is the status_ok the caller must report. A resolution that
// fails is not an error to the caller: the row is stored, and it is stored as
// not usable.
func (h *Handler) admitConfiguration(
	ctx context.Context,
	schema string,
	stored bool,
	snapshot configurationapp.CurrentConfigurationLifecycleSnapshot,
) bool {
	if h == nil || h.providerAdmission == nil || h.pool == nil {
		return stored
	}
	decision, err := h.providerAdmission.AdmitCurrentProviderConfiguration(ctx, snapshot)
	if err != nil {
		slog.WarnContext(ctx, "configuration provider admission did not complete",
			"project_id", snapshot.ProjectID, "section", snapshot.Section, "err", err)
		return stored
	}
	if !decision.Managed || decision.StatusOK == stored {
		return stored
	}
	query := fmt.Sprintf(
		`UPDATE %s.configuration SET status_ok = $1 WHERE id = $2 AND project_id = $3`,
		schema,
	)
	if _, err := h.pool.Exec(ctx, query, decision.StatusOK, snapshot.ID, snapshot.ProjectID); err != nil {
		slog.ErrorContext(ctx, "configuration status write failed",
			"project_id", snapshot.ProjectID, "configuration_id", snapshot.ID, "err", err)
		return stored
	}
	return decision.StatusOK
}

// configurationAdmissionSnapshot builds the immutable row description the
// admission decision reads. It carries no resolved secret: Data holds the
// stored references, exactly as the lifecycle snapshot does.
//
// The snapshot holds int32 ids because id, project_id and author_id are
// INTEGER columns. `projectID` arrives from the request path through
// strconv.Atoi, which accepts every value an `int` holds. A value above
// math.MaxInt32 therefore truncates in `int32(projectID)`. admitConfiguration
// then compares snapshot.ProjectID in the WHERE clause of the status write, so
// a truncated value points that write at a different project. The row id
// carries the same risk.
//
// A row id or a project id outside int32 names no row this schema holds. The
// function reports false, and the caller does not admit. Refusal keeps the
// decision absent. Truncation would make the decision wrong and silent.
func configurationAdmissionSnapshot(
	id int,
	uuid string,
	projectID int,
	title string,
	configType string,
	section string,
	data map[string]any,
	authorID *int,
) (configurationapp.CurrentConfigurationLifecycleSnapshot, bool) {
	if id <= 0 || id > math.MaxInt32 || projectID <= 0 || projectID > math.MaxInt32 {
		return configurationapp.CurrentConfigurationLifecycleSnapshot{}, false
	}
	snapshot := configurationapp.CurrentConfigurationLifecycleSnapshot{
		ID:          int32(id),
		UUID:        uuid,
		ProjectID:   int32(projectID),
		EliteaTitle: title,
		Type:        configType,
		Section:     section,
		Data:        data,
	}
	// The author does not identify the row. An author id outside the
	// author_id column is therefore left out, not refused. Refusal would drop
	// the status_ok decision for the whole row. A row with no decision stays
	// invisible to the LLM gateway (#457), which is worse than a row that
	// carries no author.
	if authorID != nil && *authorID > 0 && *authorID <= math.MaxInt32 {
		author := int32(*authorID)
		snapshot.AuthorID = &author
	}
	if snapshot.Data == nil {
		snapshot.Data = map[string]any{}
	}
	return snapshot, true
}
