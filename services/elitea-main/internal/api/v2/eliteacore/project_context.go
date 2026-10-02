package eliteacore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

const (
	projectContextMaxContentRunes          = 2500
	projectContextMaxActivationRunes       = 300
	projectContextMaxRequestBytes    int64 = 1 << 20
)

type projectContextDetail struct {
	ID                    *int32  `json:"id"`
	Content               string  `json:"content"`
	Enabled               bool    `json:"enabled"`
	ActivationDescription *string `json:"activation_description"`
	UpdatedAt             *string `json:"updated_at"`
}

type projectContextUpdate struct {
	content                  string
	enabled                  bool
	activationDescription    *string
	activationDescriptionSet bool
}

func defaultProjectContextDetail() projectContextDetail {
	return projectContextDetail{Content: "", Enabled: true}
}

// ProjectContext mirrors the current project-context builder read. Runtime
// delivery of the stored value (including progressive disclosure) is a
// separate concern and is deliberately untouched by this handler.
func (h *Handler) ProjectContext(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		writeJSON(w, http.StatusOK, defaultProjectContextDetail())
		return
	}

	projectID := chi.URLParam(r, "projectID")
	schema, ok := tenantSchema(w, projectID)
	if !ok {
		return
	}
	numericProjectID, err := strconv.ParseInt(projectID, 10, 32)
	if err != nil || numericProjectID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid project id"})
		return
	}

	detail, err := h.readProjectContext(r.Context(), schema, int32(numericProjectID))
	if err != nil {
		slog.ErrorContext(r.Context(), "project context read failed", "project_id", projectID, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to read project context"})
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (h *Handler) readProjectContext(
	ctx context.Context,
	schema string,
	projectID int32,
) (projectContextDetail, error) {
	query := fmt.Sprintf(`
		SELECT id, data, updated_at
		FROM %s.configuration
		WHERE project_id = $1 AND type = 'project_context'
		LIMIT 1`, schema)
	var (
		id        int32
		data      []byte
		updatedAt *time.Time
	)
	err := h.pool.QueryRow(ctx, query, projectID).Scan(&id, &data, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return defaultProjectContextDetail(), nil
	}
	if err != nil {
		return projectContextDetail{}, err
	}
	return decodeProjectContextDetail(id, data, updatedAt)
}

// UpdateProjectContext applies the current replacement contract: content and
// enabled default on every PUT, while an omitted activation_description keeps
// the existing non-empty value and an explicit null/blank removes it.
func (h *Handler) UpdateProjectContext(w http.ResponseWriter, r *http.Request) {
	update, err := decodeProjectContextUpdate(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if h.pool == nil {
		detail := defaultProjectContextDetail()
		detail.Content = update.content
		detail.Enabled = update.enabled
		detail.ActivationDescription = update.activationDescription
		writeJSON(w, http.StatusOK, detail)
		return
	}

	projectID := chi.URLParam(r, "projectID")
	schema, ok := tenantSchema(w, projectID)
	if !ok {
		return
	}
	numericProjectID, err := strconv.ParseInt(projectID, 10, 32)
	if err != nil || numericProjectID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid project id"})
		return
	}

	detail, err := h.writeProjectContext(r, schema, int32(numericProjectID), update)
	if err != nil {
		slog.ErrorContext(r.Context(), "project context write failed", "project_id", projectID, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to update project context"})
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (h *Handler) writeProjectContext(
	r *http.Request,
	schema string,
	projectID int32,
	update projectContextUpdate,
) (projectContextDetail, error) {
	tx, err := h.pool.Begin(r.Context())
	if err != nil {
		return projectContextDetail{}, err
	}
	defer func() { _ = tx.Rollback(r.Context()) }()

	selectQuery := fmt.Sprintf(`
		SELECT id, data
		FROM %s.configuration
		WHERE project_id = $1 AND type = 'project_context'
		LIMIT 1
		FOR UPDATE`, schema)
	var (
		id           int32
		existingData []byte
	)
	err = tx.QueryRow(r.Context(), selectQuery, projectID).Scan(&id, &existingData)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return projectContextDetail{}, err
	}
	existingFound := err == nil

	activation := update.activationDescription
	if !update.activationDescriptionSet && existingFound {
		existing, decodeErr := decodeProjectContextDetail(id, existingData, nil)
		if decodeErr != nil {
			return projectContextDetail{}, decodeErr
		}
		if existing.ActivationDescription != nil && *existing.ActivationDescription != "" {
			activation = existing.ActivationDescription
		}
	}
	data := map[string]any{"content": update.content, "enabled": update.enabled}
	if activation != nil && *activation != "" {
		data["activation_description"] = *activation
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return projectContextDetail{}, err
	}

	var (
		storedID        int32
		storedData      []byte
		storedUpdatedAt *time.Time
	)
	if !existingFound {
		insertQuery := fmt.Sprintf(`
			INSERT INTO %s.configuration
				(project_id, label, elitea_title, type, section, data, meta, shared,
				 status_ok, source, created_at)
			VALUES ($1::integer, 'Project Context', 'project_context_' || $1::text, 'project_context',
				'project_settings', $2, '{}'::jsonb, false, true, 'system', NOW())
			RETURNING id, data, updated_at`, schema)
		err = tx.QueryRow(r.Context(), insertQuery, projectID, encoded).
			Scan(&storedID, &storedData, &storedUpdatedAt)
	} else {
		updateQuery := fmt.Sprintf(`
			UPDATE %s.configuration
			SET data = $1, updated_at = NOW()
			WHERE id = $2 AND project_id = $3 AND type = 'project_context'
			RETURNING id, data, updated_at`, schema)
		err = tx.QueryRow(r.Context(), updateQuery, encoded, id, projectID).
			Scan(&storedID, &storedData, &storedUpdatedAt)
	}
	if err != nil {
		return projectContextDetail{}, err
	}
	if err := tx.Commit(r.Context()); err != nil {
		return projectContextDetail{}, err
	}
	return decodeProjectContextDetail(storedID, storedData, storedUpdatedAt)
}

func (h *Handler) DeleteProjectContext(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "Project context not found"})
		return
	}
	projectID := chi.URLParam(r, "projectID")
	schema, ok := tenantSchema(w, projectID)
	if !ok {
		return
	}
	numericProjectID, err := strconv.ParseInt(projectID, 10, 32)
	if err != nil || numericProjectID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid project id"})
		return
	}

	query := fmt.Sprintf(`
		WITH target AS (
			SELECT id FROM %s.configuration
			WHERE project_id = $1 AND type = 'project_context'
			LIMIT 1
		)
		DELETE FROM %s.configuration AS configuration
		USING target
		WHERE configuration.id = target.id`, schema, schema)
	result, err := h.pool.Exec(r.Context(), query, int32(numericProjectID))
	if err != nil {
		slog.ErrorContext(r.Context(), "project context delete failed", "project_id", projectID, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to delete project context"})
		return
	}
	if result.RowsAffected() == 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "Project context not found"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func decodeProjectContextUpdate(body io.Reader) (projectContextUpdate, error) {
	raw, err := io.ReadAll(io.LimitReader(body, projectContextMaxRequestBytes+1))
	if err != nil || int64(len(raw)) > projectContextMaxRequestBytes {
		return projectContextUpdate{}, errors.New("invalid project context request")
	}
	var fields map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&fields); err != nil || fields == nil {
		return projectContextUpdate{}, errors.New("invalid project context request")
	}
	if err := requireProjectContextJSONEnd(decoder); err != nil {
		return projectContextUpdate{}, errors.New("invalid project context request")
	}

	result := projectContextUpdate{content: "", enabled: true}
	if value, present := fields["content"]; present {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) ||
			json.Unmarshal(value, &result.content) != nil ||
			utf8.RuneCountInString(result.content) > projectContextMaxContentRunes {
			return projectContextUpdate{}, errors.New("content must be a string of at most 2500 characters")
		}
	}
	if value, present := fields["enabled"]; present {
		enabled, ok := parseProjectContextBoolean(value)
		if !ok {
			return projectContextUpdate{}, errors.New("enabled must be a boolean")
		}
		result.enabled = enabled
	}
	if value, present := fields["activation_description"]; present {
		result.activationDescriptionSet = true
		if !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			var description string
			if json.Unmarshal(value, &description) != nil ||
				utf8.RuneCountInString(description) > projectContextMaxActivationRunes {
				return projectContextUpdate{}, errors.New("activation_description must be null or a string of at most 300 characters")
			}
			description = strings.Join(strings.Fields(description), " ")
			if description != "" {
				result.activationDescription = &description
			}
		}
	}
	return result, nil
}

func decodeProjectContextDetail(id int32, data []byte, updatedAt *time.Time) (projectContextDetail, error) {
	var fields map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&fields); err != nil || fields == nil || requireProjectContextJSONEnd(decoder) != nil {
		return projectContextDetail{}, errors.New("stored project context is invalid")
	}

	detail := defaultProjectContextDetail()
	detail.ID = &id
	if raw, present := fields["content"]; present {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &detail.Content) != nil {
			return projectContextDetail{}, errors.New("stored project context content is invalid")
		}
	}
	if raw, present := fields["enabled"]; present {
		enabled, ok := parseProjectContextBoolean(raw)
		if !ok {
			return projectContextDetail{}, errors.New("stored project context enabled state is invalid")
		}
		detail.Enabled = enabled
	}
	if raw, present := fields["activation_description"]; present &&
		!bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		var description string
		if json.Unmarshal(raw, &description) != nil {
			return projectContextDetail{}, errors.New("stored project context activation description is invalid")
		}
		detail.ActivationDescription = &description
	}
	if updatedAt != nil {
		formatted := formatProjectContextTimestamp(*updatedAt)
		detail.UpdatedAt = &formatted
	}
	return detail, nil
}

func parseProjectContextBoolean(raw json.RawMessage) (bool, bool) {
	trimmed := bytes.TrimSpace(raw)
	if bytes.Equal(trimmed, []byte("true")) {
		return true, true
	}
	if bytes.Equal(trimmed, []byte("false")) {
		return false, true
	}
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var value string
		if json.Unmarshal(trimmed, &value) != nil {
			return false, false
		}
		switch strings.ToLower(value) {
		case "1", "on", "t", "true", "y", "yes":
			return true, true
		case "0", "off", "f", "false", "n", "no":
			return false, true
		default:
			return false, false
		}
	}
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	if decoder.Decode(&number) != nil || requireProjectContextJSONEnd(decoder) != nil {
		return false, false
	}
	value, err := strconv.ParseFloat(number.String(), 64)
	if err != nil {
		return false, false
	}
	if value == 0 {
		return false, true
	}
	if value == 1 {
		return true, true
	}
	return false, false
}

func requireProjectContextJSONEnd(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("project context request has trailing JSON")
	}
	return err
}

func formatProjectContextTimestamp(value time.Time) string {
	base := value.Format("2006-01-02T15:04:05")
	if microseconds := value.Nanosecond() / 1000; microseconds != 0 {
		return base + fmt.Sprintf(".%06d", microseconds)
	}
	return base
}
