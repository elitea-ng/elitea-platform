package repos

// The desktop computer use reads of a local turn (ADR-0047 decision 9,
// #1585): the project's `computer_use` setting and the turn a screenshot
// upload belongs to.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/localturn"
)

// UploadScreenshots reads the project's `computer_use` setting. A project
// with no row (or no tenant table) keeps screenshots on the device. An
// undecodable row is an error, never "on".
func (r *LocalTurnsRepo) UploadScreenshots(ctx context.Context, projectID int64) (bool, error) {
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return false, err
	}
	var raw []byte
	err = r.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT data FROM %s.configuration WHERE type = $1::text ORDER BY id LIMIT 1`, schema),
		configurationapp.ComputerUseConfigurationType).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) || undefinedTable(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("computer use: read the project setting: %w", err)
	}
	var setting struct {
		UploadScreenshots bool `json:"upload_screenshots"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &setting); err != nil {
			return false, fmt.Errorf("computer use: the project setting is corrupt: %w", err)
		}
	}
	return setting.UploadScreenshots, nil
}

// ScreenshotTurn answers the conversation of the caller's started turn while
// it may take screenshot uploads: not committed, not expired, and started by
// credential.
func (r *LocalTurnsRepo) ScreenshotTurn(
	ctx context.Context, projectID, actorUserID int64, executionID string, credential localturn.Credential,
) (string, error) {
	var (
		conversationUUID   string
		committed, expired bool
		stored             localturn.Credential
	)
	err := r.pool.QueryRow(ctx, `
SELECT conversation_uuid::text, committed_at IS NOT NULL, expires_at <= clock_timestamp(),
       token_id, native_client_id
FROM elitea_runtime.local_turn_executions
WHERE execution_id = $1::text AND project_id = $2::integer AND actor_id = $3::text`,
		executionID, projectID, strconv.FormatInt(actorUserID, 10)).
		Scan(&conversationUUID, &committed, &expired, &stored.TokenID, &stored.NativeClientID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && stored != credential) {
		return "", localturn.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("local turn: read the screenshot turn: %w", err)
	}
	switch {
	case committed:
		return "", localturn.ErrAlreadyCommitted
	case expired:
		return "", localturn.ErrExpired
	}
	return conversationUUID, nil
}
