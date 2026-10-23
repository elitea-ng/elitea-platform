// revalidate.go implements POST /revalidate/[{mode}/]{projectID}/{configID}:
// re-run the ADMISSION decision for one stored row and persist its status_ok.
//
// # Admission is not a provider contact, and that is why this is its own route
//
// status_ok answers "may a runtime use this row" — its declared references
// expand, its hidden secrets redeem, and policy admits the project to own it.
// Nothing in that decision dials a provider; the baseline platform set the
// column from its own events too (application/configurations/provider_admission.go
// records the two Python call sites). The LIVE provider round trip is the
// separate, user-started check in stored_check.go, and it stores NOTHING.
//
// Merging the two would break both halves:
//
//   - A provider outage would flip status_ok to false for every credential in
//     the project, withdrawing rows the gateway was serving correctly, because
//     every reader of a provider row selects on status_ok = true.
//   - A row whose secret no longer redeems would keep status_ok = true as long
//     as the provider still answered a key the platform can no longer read.
//
// So: this route writes the column and never dials; the stored check dials and
// never writes. The comment is here, and in stored_check.go, so a later edit
// that "simplifies" the pair into one route has to delete a stated reason.
//
// # Why the route exists at all
//
// Admission runs when a row is WRITTEN (provider_admission.go, #457). Nothing
// re-runs it afterwards, and the inputs are not owned by the row: the
// credential it references can be deleted, renamed, or have its vault secret
// removed. status_ok then describes a row that no longer resolves, and the
// only repair available to a user was to open the credential and save it
// again — a write, with all a write's risks, to fix a status column.
//
// # What it writes
//
// EXACTLY what Update writes, through the same function: admitConfiguration,
// which writes status_ok and nothing else. No field of the row is touched — no
// data, no meta, no updated_at. status_logs is deliberately NOT written: the
// admission decision carries no message (CurrentProviderAdmissionDecision is
// {Managed, StatusOK}), the lifecycle's own status effect writes status_ok
// alone as well (repos.SetCurrentConfigurationLifecycleStatus), and inventing
// a log line here would make this route the only writer of a column two other
// paths leave alone. The stored value is read back and returned unchanged.
//
// An UNMANAGED row — a generic SDK configuration, or an imported model with no
// ai_credentials — is left exactly as it is: admitConfiguration returns the
// stored value when the decision reports Managed = false, so this route cannot
// change the status of a row whose status it does not own.
package configurations

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
	"github.com/go-chi/chi/v5"
)

// RevalidateConfiguration re-runs admission for one stored row and returns the
// row as it now stands.
//
// The response is the same Configuration object the detail route returns, so
// the browser can replace the row it holds without a second read.
func (h *Handler) RevalidateConfiguration(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	configID := chi.URLParam(r, "configID")
	schema, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	ctx := r.Context()

	pID, convErr := strconv.Atoi(projectID)
	if convErr != nil {
		apierr.WriteStatus(w, http.StatusBadRequest, "invalid project")
		return
	}
	if h.pool == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "the configuration store is not available")
		return
	}
	// A missing admission is refused rather than answered with the unchanged
	// row and HTTP 200. Both look identical to a client, and the second is the
	// shape this repository keeps being bitten by: a control that reports
	// success while doing nothing at all. It cannot panic either way —
	// admitConfiguration nil-tests the same field — but "revalidated" must not
	// be said by a build that cannot revalidate.
	if h.providerAdmission == nil {
		slog.ErrorContext(ctx, "revalidate: no provider admission composed",
			"project_id", projectID, "configuration_id", configID)
		apierr.WriteStatus(w, http.StatusServiceUnavailable,
			"configuration revalidation is not available")
		return
	}

	c, found, err := h.readConfigurationForRevalidation(ctx, schema, configID)
	if err != nil {
		slog.ErrorContext(ctx, "revalidate: read the configuration row failed",
			"project_id", projectID, "configuration_id", configID, "err", err)
		apierr.WriteStatus(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if !found {
		// The schema is the one {projectID} named, so a configID owned by
		// another project names no row here and answers 404 — the same answer
		// the detail route gives, and the same reason.
		apierr.WriteStatus(w, http.StatusNotFound, "configuration not found")
		return
	}

	snapshot, ok := configurationAdmissionSnapshot(
		c.ID, c.UUID, pID, c.Name, c.Type, c.Section, c.Data, c.AuthorID,
	)
	if !ok {
		// configurationAdmissionSnapshot refuses an id or a project id outside
		// int32, because the status write compares both in its WHERE clause
		// and a truncated value points that write at a different row. Refusing
		// keeps the decision absent; truncating would make it wrong and
		// silent.
		slog.ErrorContext(ctx, "revalidate: the row cannot be addressed by the admission decision",
			"project_id", projectID, "configuration_id", configID)
		apierr.WriteStatus(w, http.StatusInternalServerError, "invalid stored configuration")
		return
	}
	c.StatusOK = h.admitConfiguration(ctx, schema, c.StatusOK, snapshot)
	writeJSON(w, http.StatusOK, c)
}

// readConfigurationForRevalidation reads the row the decision is made about.
//
// It reads the SAME columns the detail route reads, so the response of this
// route and of GET /configuration/{projectID}/{configID} cannot describe the
// same row differently.
func (h *Handler) readConfigurationForRevalidation(
	ctx context.Context,
	schema string,
	configID string,
) (Configuration, bool, error) {
	query := fmt.Sprintf(`
		SELECT id, COALESCE(uuid::text, ''), project_id, COALESCE(label, ''), elitea_title, type, section,
			data, meta, shared, status_ok, COALESCE(status_logs, ''), source, author_id,
			created_at, updated_at
		FROM %s.configuration WHERE %s = $1
	`, schema, configurationIDColumn(configID))

	var c Configuration
	var data, meta []byte
	var createdAt, updatedAt *time.Time
	if err := h.pool.QueryRow(ctx, query, configID).Scan(
		&c.ID, &c.UUID, &c.ProjectID, &c.Label, &c.Name, &c.Type, &c.Section,
		&data, &meta, &c.Shared, &c.StatusOK, &c.StatusLogs, &c.Source, &c.AuthorID,
		&createdAt, &updatedAt,
	); err != nil {
		if configurationRowAbsent(err) {
			return Configuration{}, false, nil
		}
		return Configuration{}, false, err
	}
	if err := json.Unmarshal(data, &c.Data); err != nil {
		return Configuration{}, false, err
	}
	if err := json.Unmarshal(meta, &c.Meta); err != nil {
		return Configuration{}, false, err
	}
	if createdAt != nil {
		c.CreatedAt = createdAt.Format(time.RFC3339)
	}
	if updatedAt != nil {
		c.UpdatedAt = updatedAt.Format(time.RFC3339)
	}
	return c, true, nil
}
