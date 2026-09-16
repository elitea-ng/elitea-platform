package repos

// The personal-access-token expiry notice (issue #940 A3, ELITEA-0750/0751/
// 0752/0754/0755/0756).
//
// # The shape, and why it is not the bucket producer's
//
// artifact_retention_notification.go is the sibling pattern: same
// centry.notifications table, a deterministic UUID, `ON CONFLICT (uuid) DO
// NOTHING`. That dedupe is idempotent for a RETRIED tick and nothing more. A
// user who deletes the notification gets it back on the next sweep, and with a
// 15-minute cadence across a 24-hour window that is the same notification up to
// ninety-six times. So the mark lives in a table the recipient cannot reach:
// elitea_identity.token_lifecycle (shared migration 0125), keyed by token and
// stamped with the `expires` it was made about.
//
// # Raw pgx, not sqlc
//
// Every statement here reads or writes two tables — one pylon-owned
// (public.auth_core__token), one this corpus owns — and the sweep's predicate
// is the feature. Keeping it in one file, readable beside the rule it
// implements, is worth more than a generated binding; admin_background_jobs.go
// makes the same choice for the same reason.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/patexpiry"
)

// PATExpiryNotificationEventType is the event_type the web client already
// resolves to Settings › Tokens — `features/notifications/lib/routes.ts`'s
// HREF_RESOLVERS maps it to `/settings/tokens` with no project prefix, and
// `legacyText.ts` carries its copy. The producer is the half that was missing,
// not the rendering.
const PATExpiryNotificationEventType = "personal_access_token_expiring"

// The candidate type is patexpiry.Candidate itself, not a repository twin of
// it: this repository IS patexpiry.Store, and a second struct with the same
// five fields would only be a place for the two to drift. repos already
// imports the application layer this way (notifications.go).
//
// The fields worth naming, since the type is declared elsewhere:
//
//	Name       can be empty — `name` is nullable on auth_core__token and the
//	           create route does not require one.
//	UserID     is the OWNER, and the ONLY recipient: the notification reader
//	           filters on user_id alone (queries/notifications.sql), so
//	           ELITEA-0756's scoping is a property of this value and nothing
//	           else.
//	ProjectID  satisfies centry.notifications.project_id, which is NOT NULL. It
//	           carries no meaning for this event — Settings › Tokens is not
//	           project-scoped and the client's href resolver ignores it.

// PATExpiryNotificationRepository reads the tokens due a warning and writes
// the notification plus its mark.
type PATExpiryNotificationRepository struct {
	pool *pgxpool.Pool
}

// The compiler, not a wiring test, is what keeps this repository and the
// notifier's Store in step.
var _ patexpiry.Store = (*PATExpiryNotificationRepository)(nil)

func NewPATExpiryNotificationRepository(pool *pgxpool.Pool) (*PATExpiryNotificationRepository, error) {
	if pool == nil {
		return nil, errors.New("personal access token expiry notification database is required")
	}
	return &PATExpiryNotificationRepository{pool: pool}, nil
}

// patExpiryCandidateQuery selects the tokens whose owners are due a warning.
//
// Each predicate answers one case, and each is the difference between the
// feature and a plausible-looking version of it:
//
//	expires IS NOT NULL             a key that never expires is never warned
//	                                about.
//	expires > $1                    an ALREADY expired key is not "expiring";
//	                                warning about it is a different event this
//	                                platform does not produce.
//	expires <= $1 + $2              the look-ahead window, 24 hours in
//	                                production (ELITEA-0752).
//	notified_for_expires IS DISTINCT the dedupe (ELITEA-0754). DISTINCT FROM,
//	FROM token.expires              not <>, because a NULL mark must compare
//	                                as "not yet notified" rather than as NULL.
//	issued_at IS NULL OR            ELITEA-0755: a key whose WHOLE lifetime is
//	expires - issued_at >           24 hours or less is never warned about —
//	INTERVAL '24 hours'             it would fire the instant it was minted.
//	                                The bound is the CONSTANT 24 hours, not
//	                                `$2`: tie it to the look-ahead instead and
//	                                no freshly minted token can ever be
//	                                eligible under any window, because for a
//	                                fresh key "time left" and "whole lifetime"
//	                                are the same number. An UNKNOWN issued_at
//	                                (a pre-0125 token, or one pylon minted)
//	                                counts as old enough: the warning is the
//	                                safe side of that doubt.
//	user_id IS NOT NULL             there is no one to notify otherwise, and
//	                                notifications.user_id is NOT NULL.
//
// The project id is the token's bound project when it has one, else any
// project its owner owns, else any project its owner is a member of, else 0 —
// see the candidate-type note above for why the value carries no meaning here.
// It must be SOMETHING because the column is NOT NULL.
const patExpiryCandidateQuery = `
SELECT
    token.id,
    COALESCE(token.name, '')::text AS name,
    token.user_id,
    COALESCE(
        binding.project_id,
        (SELECT owned.id FROM centry.project AS owned
          WHERE owned.owner_id = token.user_id ORDER BY owned.id LIMIT 1),
        (SELECT assignment.project_id FROM public.auth_core__project_user_role AS assignment
          WHERE assignment.user_id = token.user_id ORDER BY assignment.project_id LIMIT 1),
        0
    )::integer AS project_id,
    token.expires
FROM public.auth_core__token AS token
LEFT JOIN elitea_identity.token_lifecycle AS lifecycle
       ON lifecycle.token_id = token.id
LEFT JOIN elitea_identity.token_project_binding AS binding
       ON binding.token_id = token.id
WHERE token.expires IS NOT NULL
  AND token.user_id IS NOT NULL
  AND token.expires > $1
  AND token.expires <= $1 + $2::interval
  AND lifecycle.notified_for_expires IS DISTINCT FROM token.expires
  AND (lifecycle.issued_at IS NULL
       OR token.expires - (lifecycle.issued_at AT TIME ZONE 'UTC') > INTERVAL '24 hours')
ORDER BY token.expires, token.id
LIMIT $3`

// ListPATsNeedingExpiryNotice answers the tokens due a warning at `now`, for a
// look-ahead of `within`.
//
// `within` is a parameter rather than a constant because it is BOTH the
// production rule (24 hours) and the only honest way to exercise the feature
// end to end: a key minted through the API has the same "time left" as
// "lifetime", so nothing freshly created is ever eligible under the 24-hour
// window. A test — and an operator asking "who would be warned next week" —
// widens the look-ahead. Widening it never weakens the lifetime rule above,
// which is pinned to a constant 24 hours precisely so it cannot be widened
// along with the window.
func (r *PATExpiryNotificationRepository) ListPATsNeedingExpiryNotice(
	ctx context.Context,
	now time.Time,
	within time.Duration,
	limit int32,
) ([]patexpiry.Candidate, error) {
	rows, err := r.pool.Query(ctx, patExpiryCandidateQuery,
		now.UTC(), intervalArgument(within), limit)
	if err != nil {
		return nil, fmt.Errorf("list tokens needing an expiry notice: %w", err)
	}
	defer rows.Close()

	candidates := make([]patexpiry.Candidate, 0)
	for rows.Next() {
		var candidate patexpiry.Candidate
		if err := rows.Scan(
			&candidate.TokenID, &candidate.Name, &candidate.UserID,
			&candidate.ProjectID, &candidate.Expires,
		); err != nil {
			return nil, fmt.Errorf("scan token needing an expiry notice: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read tokens needing an expiry notice: %w", err)
	}
	return candidates, nil
}

// NotifyPATExpiring writes one notification and its mark, atomically, and says
// whether it actually produced one.
//
// THE MARK IS CLAIMED FIRST, inside the same transaction, and the insert is
// conditional on the claim. Two replicas sweeping at once both see the same
// candidate — the read above takes no lock — so the write is what decides.
// `WHERE notified_for_expires IS DISTINCT FROM $2` makes the UPSERT return no
// row for the loser, which reports `false` and writes no notification. Writing
// the notification first and marking afterwards would produce the duplicate
// ELITEA-0754 forbids on exactly that interleaving.
func (r *PATExpiryNotificationRepository) NotifyPATExpiring(
	ctx context.Context,
	candidate patexpiry.Candidate,
	now time.Time,
) (bool, error) {
	meta, err := patExpiryNotificationMeta(candidate)
	if err != nil {
		return false, err
	}

	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("begin token expiry notice: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var claimedToken int64
	claimErr := tx.QueryRow(ctx, `
INSERT INTO elitea_identity.token_lifecycle (token_id, notified_for_expires, notified_at)
VALUES ($1, $2, $3)
ON CONFLICT (token_id) DO UPDATE
    SET notified_for_expires = EXCLUDED.notified_for_expires,
        notified_at          = EXCLUDED.notified_at
    WHERE token_lifecycle.notified_for_expires IS DISTINCT FROM EXCLUDED.notified_for_expires
RETURNING token_id`, candidate.TokenID, candidate.Expires, now.UTC()).Scan(&claimedToken)
	if errors.Is(claimErr, pgx.ErrNoRows) {
		// Another replica, or an earlier tick, already announced this expiry.
		return false, nil
	}
	if claimErr != nil {
		return false, fmt.Errorf("mark token %d notified: %w", candidate.TokenID, claimErr)
	}

	if _, err := tx.Exec(ctx, `
INSERT INTO centry.notifications (is_seen, project_id, user_id, meta, event_type)
VALUES (FALSE, $1, $2, $3::jsonb, $4)`,
		candidate.ProjectID, candidate.UserID, meta, PATExpiryNotificationEventType,
	); err != nil {
		return false, fmt.Errorf("insert token %d expiry notification: %w", candidate.TokenID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit token %d expiry notice: %w", candidate.TokenID, err)
	}
	return true, nil
}

// RecordPATIssued stamps when this platform minted a token, on the executor
// the caller is already inside — the token INSERT's own transaction, so a
// token can never exist without its issue stamp.
//
// It is what makes ELITEA-0755 answerable at all: auth_core__token carries no
// creation timestamp and this corpus does not add columns to a pylon-owned
// table (0071's header, 0125's header).
func RecordPATIssued(ctx context.Context, executor PATLifecycleExecutor, tokenID int64, issuedAt time.Time) error {
	if _, err := executor.Exec(ctx, `
INSERT INTO elitea_identity.token_lifecycle (token_id, issued_at)
VALUES ($1, $2)
ON CONFLICT (token_id) DO UPDATE SET issued_at = EXCLUDED.issued_at`,
		tokenID, issuedAt.UTC()); err != nil {
		return fmt.Errorf("record token %d issue time: %w", tokenID, err)
	}
	return nil
}

// PATLifecycleExecutor is the one method RecordPATIssued needs, so a caller
// can pass its open transaction (pgx.Tx) or a pool.
type PATLifecycleExecutor interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// patExpiryNotificationMeta builds the stored `meta` blob.
//
// `message` is the PRE-FORMATTED string the client renders when it is present
// (`NotificationMessage.tsx`: `message ? … : <LegacyNotificationMessage/>`),
// with the `[text]()` empty-href link syntax the same file parses and resolves
// — `routes.ts` turns an empty href on this event type into
// `/settings/tokens`, no project prefix (ELITEA-0751).
//
// `token_name` is written as well, and is not redundant: it is what the
// LEGACY renderer reads (`normalize.ts` maps it to `meta.tokenName`,
// `legacyText.ts`'s parsePersonalAccessTokenExpiring uses it), so a client
// that takes the fallback branch still names the right key. Writing only one
// of the two produces a notification that renders correctly in one of the two
// paths and blankly in the other.
func patExpiryNotificationMeta(candidate patexpiry.Candidate) ([]byte, error) {
	name := candidate.Name
	if name == "" {
		name = "(unnamed)"
	}
	message := fmt.Sprintf(
		"Your personal access token %s will expire in 24 hours. After expiration, it will no longer "+
			"work. You can delete and recreate a new token if needed. [Manage Personal Access Tokens]()",
		name)
	meta, err := json.Marshal(map[string]any{
		"message":    message,
		"token_name": name,
		"token_id":   candidate.TokenID,
		"expires_at": candidate.Expires.UTC().Format(time.RFC3339),
	})
	if err != nil {
		return nil, fmt.Errorf("marshal token expiry notification metadata: %w", err)
	}
	return meta, nil
}

// intervalArgument renders a Go duration as a PostgreSQL interval literal.
// Seconds, not a formatted string: `$2::interval` is compared against a
// timestamp difference, and a unit PostgreSQL parses differently from Go
// (`24h` is a valid Go duration and not a valid interval) would silently
// change the window.
func intervalArgument(within time.Duration) string {
	return fmt.Sprintf("%d seconds", int64(within/time.Second))
}
