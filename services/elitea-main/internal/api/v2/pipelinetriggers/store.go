package pipelinetriggers

// The tenant rows behind both entry points. See 0133_pipeline_triggers_and_
// schedules.sql for what each column means and why the secret is not here.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	v2secrets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
)

// ErrNotFound is "no such row", distinguished from a database that would not
// answer. Collapsing the two is how "you have not configured this" gets
// reported to a user as a broken deployment, and the reverse.
var ErrNotFound = errors.New("pipelinetriggers: not found")

// vaultFeature namespaces this package's hidden-bucket entries.
const vaultFeature = "pipeline_trigger"

// vaultField names the one field stored per trigger.
const vaultField = "secret"

// tokenIDBytes and tokenSecretBytes size the two halves of a credential.
//
// The ID is 16 bytes because it is a lookup key that appears in a URL: it must
// be unguessable enough that an inbound caller cannot enumerate a deployment's
// triggers, and no longer than that. The SECRET is 32 bytes because it is the
// credential itself; it is what the SHA-256 in the row is a digest of.
const (
	tokenIDBytes     = 16
	tokenSecretBytes = 32
)

// triggerRow is one `pipeline_triggers` row.
type triggerRow struct {
	ID            int64
	ApplicationID int64
	VersionID     int64
	TokenID       string
	TokenHash     []byte
	SecretName    string
	CreatedBy     int64
	CreatedAt     time.Time
	RotatedAt     *time.Time
	RevokedAt     *time.Time
	LastUsedAt    *time.Time
}

// scheduleRow is one `pipeline_schedules` row.
type scheduleRow struct {
	ID              int64
	ApplicationID   int64
	VersionID       int64
	Cron            string
	Active          bool
	UserInput       string
	AuthorID        int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
	LastRun         *time.Time
	LastResult      *string
	LastResultAt    *time.Time
	LastResultText  *string
	LastExecutionID *string
}

// The last_result vocabulary. The column CHECK constraint holds the same set,
// so a value invented here is refused by the database rather than rendered as
// an empty cell by the settings tab.
const (
	resultDispatched     = "dispatched"
	resultSkippedOverlap = "skipped_overlap"
	resultSkippedAuth    = "skipped_unauthorized"
	resultSkippedMissing = "skipped_missing_version"
	resultFailed         = "failed"
)

// newCredential mints one trigger credential.
//
// Both halves come from crypto/rand and neither is derived from the other: an
// id derived from the secret would let anyone holding a URL recover a
// constraint on the secret, and a secret derived from the id would be no secret
// at all.
func newCredential() (tokenID, secret string, hash []byte, err error) {
	idBytes := make([]byte, tokenIDBytes)
	if _, err = rand.Read(idBytes); err != nil {
		return "", "", nil, fmt.Errorf("pipelinetriggers: mint token id: %w", err)
	}
	secretBytes := make([]byte, tokenSecretBytes)
	if _, err = rand.Read(secretBytes); err != nil {
		return "", "", nil, fmt.Errorf("pipelinetriggers: mint token secret: %w", err)
	}
	digest := sha256.Sum256(secretBytes)
	return hex.EncodeToString(idBytes),
		base64.RawURLEncoding.EncodeToString(secretBytes),
		digest[:],
		nil
}

// secretDigest is the one place a presented secret becomes a comparable value.
//
// It hashes the RAW REQUEST STRING rather than decoding it first. A decode step
// would give a malformed secret a different code path — and a different
// duration — from a well-formed wrong one, which is the timing branch rule 3 in
// the package doc forbids.
func secretDigest(presented string) []byte {
	digest := sha256.Sum256([]byte(presented))
	return digest[:]
}

// vaultSecretName derives the hidden-bucket name for one trigger.
//
// The SUBJECT is the token id, which is unique per credential by construction,
// so `AdminHiddenSecretName`'s digest has a genuinely injective subject to work
// from and a rotation cannot collide with the entry it replaces.
func vaultSecretName(tokenID string) string {
	return v2secrets.AdminHiddenSecretName(vaultFeature, tokenID, vaultField)
}

const triggerColumns = `id, application_id, version_id, token_id, token_hash, secret_name,
	created_by, created_at, rotated_at, revoked_at, last_used_at`

func scanTrigger(row pgx.Row) (triggerRow, error) {
	var trigger triggerRow
	if err := row.Scan(
		&trigger.ID, &trigger.ApplicationID, &trigger.VersionID, &trigger.TokenID,
		&trigger.TokenHash, &trigger.SecretName, &trigger.CreatedBy, &trigger.CreatedAt,
		&trigger.RotatedAt, &trigger.RevokedAt, &trigger.LastUsedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return triggerRow{}, ErrNotFound
		}
		return triggerRow{}, err
	}
	return trigger, nil
}

func (h *Handler) triggerByVersion(ctx context.Context, schema string, versionID int64) (triggerRow, error) {
	return scanTrigger(h.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT %s FROM %s.pipeline_triggers WHERE version_id = $1`, triggerColumns, schema), versionID))
}

// triggerByTokenID is the inbound lookup.
//
// It selects on the token id ALONE within the schema the path named. The hash
// is compared afterwards, in Go, in constant time — deliberately not in SQL,
// where `=` on bytea is not constant time and where a non-matching row would be
// indistinguishable from a missing one for a different reason.
func (h *Handler) triggerByTokenID(ctx context.Context, schema, tokenID string) (triggerRow, error) {
	return scanTrigger(h.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT %s FROM %s.pipeline_triggers WHERE token_id = $1`, triggerColumns, schema), tokenID))
}

// upsertTrigger creates the row or rotates the existing one.
//
// ON CONFLICT names the COLUMN and not the constraint: a ledgered tenant
// database carries a different generated constraint name from the bootstrap
// schema, and naming it answered 500 on every real deployment the last time
// this repository tried (repos/conversations.go).
func (h *Handler) upsertTrigger(
	ctx context.Context, schema string, applicationID, versionID, actorID int64,
	tokenID string, hash []byte, secretName string,
) (triggerRow, error) {
	return scanTrigger(h.pool.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.pipeline_triggers
	(application_id, version_id, token_id, token_hash, secret_name, created_by)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (version_id) DO UPDATE SET
	token_id = EXCLUDED.token_id,
	token_hash = EXCLUDED.token_hash,
	secret_name = EXCLUDED.secret_name,
	application_id = EXCLUDED.application_id,
	rotated_at = now(),
	revoked_at = NULL,
	revoked_by = NULL
RETURNING %s`, schema, triggerColumns),
		applicationID, versionID, tokenID, hash, secretName, actorID))
}

// revokeTrigger stamps the row rather than deleting it. It is idempotent: a
// second revoke of an already revoked trigger keeps the FIRST timestamp, which
// is the one that answers "when did this stop working".
func (h *Handler) revokeTrigger(ctx context.Context, schema string, versionID, actorID int64) (triggerRow, error) {
	return scanTrigger(h.pool.QueryRow(ctx, fmt.Sprintf(`
UPDATE %s.pipeline_triggers
   SET revoked_at = COALESCE(revoked_at, now()),
       revoked_by = COALESCE(revoked_by, $2)
 WHERE version_id = $1
RETURNING %s`, schema, triggerColumns), versionID, actorID))
}

func (h *Handler) stampTriggerUse(ctx context.Context, schema string, triggerID int64) {
	// The stamp belongs to the row, not to the request: a caller that hangs up
	// the moment the run is admitted must still leave the evidence that the
	// trigger was used.
	ctx = context.WithoutCancel(ctx)
	if _, err := h.pool.Exec(ctx, fmt.Sprintf(
		`UPDATE %s.pipeline_triggers SET last_used_at = now() WHERE id = $1`, schema), triggerID); err != nil {
		h.log().Warn("pipelinetriggers: could not stamp trigger use", "err", err)
	}
}

const scheduleColumns = `id, application_id, version_id, cron, active, user_input, author_id,
	created_at, updated_at, last_run, last_result, last_result_at, last_result_detail, last_execution_id`

func scanSchedule(row pgx.Row) (scheduleRow, error) {
	var schedule scheduleRow
	if err := row.Scan(
		&schedule.ID, &schedule.ApplicationID, &schedule.VersionID, &schedule.Cron,
		&schedule.Active, &schedule.UserInput, &schedule.AuthorID,
		&schedule.CreatedAt, &schedule.UpdatedAt, &schedule.LastRun,
		&schedule.LastResult, &schedule.LastResultAt, &schedule.LastResultText,
		&schedule.LastExecutionID,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return scheduleRow{}, ErrNotFound
		}
		return scheduleRow{}, err
	}
	return schedule, nil
}

func (h *Handler) scheduleByVersion(ctx context.Context, schema string, versionID int64) (scheduleRow, error) {
	return scanSchedule(h.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT %s FROM %s.pipeline_schedules WHERE version_id = $1`, scheduleColumns, schema), versionID))
}

// upsertSchedule writes the schedule and RESETS the author to the caller.
//
// That reset is the security-relevant part of this statement. `author_id` is
// who the unattended run executes as, so leaving it at the original author
// while someone else edits the cron would let a caller retarget somebody
// else's identity onto a new time. The person who last saved the schedule owns
// what it does, and they had to hold the write permission to save it.
func (h *Handler) upsertSchedule(
	ctx context.Context, schema string, applicationID, versionID, authorID int64,
	cron string, active bool, userInput string,
) (scheduleRow, error) {
	return scanSchedule(h.pool.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.pipeline_schedules
	(application_id, version_id, cron, active, user_input, author_id)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (version_id) DO UPDATE SET
	cron = EXCLUDED.cron,
	active = EXCLUDED.active,
	user_input = EXCLUDED.user_input,
	author_id = EXCLUDED.author_id,
	application_id = EXCLUDED.application_id,
	updated_at = now()
RETURNING %s`, schema, scheduleColumns),
		applicationID, versionID, cron, active, userInput, authorID))
}

func (h *Handler) deleteSchedule(ctx context.Context, schema string, versionID int64) error {
	tag, err := h.pool.Exec(ctx, fmt.Sprintf(
		`DELETE FROM %s.pipeline_schedules WHERE version_id = $1`, schema), versionID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// activeSchedules is the scheduler scan for one tenant.
func (h *Handler) activeSchedules(ctx context.Context, schema string) ([]scheduleRow, error) {
	rows, err := h.pool.Query(ctx, fmt.Sprintf(
		`SELECT %s FROM %s.pipeline_schedules WHERE active ORDER BY id`, scheduleColumns, schema))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var schedules []scheduleRow
	for rows.Next() {
		schedule, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		schedules = append(schedules, schedule)
	}
	return schedules, rows.Err()
}

// recordScheduleOutcome writes what happened.
//
// `last_run` moves ONLY on a dispatch. Every other outcome leaves it alone, so
// `next(last_run) <= now` still answers true on the next tick and the schedule
// runs once — promptly, and once only — when the reason clears. That is
// elitea-scheduler's rule for a suppressed tick, applied to every non-dispatch
// outcome for the same reason: `last_run` is the record that a run HAPPENED,
// and writing it for one that did not is issue 305 reached by another route.
func (h *Handler) recordScheduleOutcome(
	ctx context.Context, schema string, scheduleID int64,
	result, detail, executionID string, ranAt time.Time,
) error {
	ctx = context.WithoutCancel(ctx)
	var detailValue, executionValue *string
	if detail != "" {
		detailValue = &detail
	}
	if executionID != "" {
		executionValue = &executionID
	}
	if result == resultDispatched {
		_, err := h.pool.Exec(ctx, fmt.Sprintf(`
UPDATE %s.pipeline_schedules
   SET last_run = $2, last_result = $3, last_result_detail = $4,
       last_result_at = $2, last_execution_id = $5
 WHERE id = $1`, schema), scheduleID, ranAt, result, detailValue, executionValue)
		return err
	}
	_, err := h.pool.Exec(ctx, fmt.Sprintf(`
UPDATE %s.pipeline_schedules
   SET last_result = $2, last_result_detail = $3, last_result_at = $4
 WHERE id = $1`, schema), scheduleID, result, detailValue, ranAt)
	return err
}
