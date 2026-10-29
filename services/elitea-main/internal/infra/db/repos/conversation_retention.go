package repos

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	retentionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/conversationretention"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// Conversation retention (ADR-0042 decision 4, issue #1495).
//
// The sweep in internal/application/conversationretention reads candidates
// here and deletes them one at a time through DeleteExpiredConversation, which
// runs the SAME statement sequence as a user's delete (deleteConversationInTx)
// and, in the same transaction, records the stored attachment objects in the
// row-first journal elitea_runtime.conversation_attachment_cleanup. The bytes
// are deleted afterwards by the journal drain, so a failed object delete is
// retried instead of being lost the way a 500 after a commit would lose it.

// conversationActivitySQL is a conversation's age: its last message, or its
// creation when it has none. Not `updated_at`: a rename and a settings write
// stamp that column too, and rows written before tenant 0145 never had it
// bumped by a message. The (conversation_id) index of tenant 0123 serves the
// max(). Both columns are UTC wall-clock `timestamp`s.
const conversationActivitySQL = `COALESCE(
	(SELECT max(g.created_at) FROM %[1]s.chat_message_group g WHERE g.conversation_id = c.id),
	c.created_at)`

// conversationHeldSQL is true when an active legal hold covers conversation c
// of project $1: a hold on the project, on its owner (author_id), or on an
// agent (application participant) of it. An agent is keyed by its OWN
// project, which may differ from the conversation's for a public agent; a
// participant row without a project id is the conversation's project's.
const conversationHeldSQL = `EXISTS (
	SELECT 1 FROM elitea_runtime.legal_holds h
	 WHERE h.released_at IS NULL
	   AND (   (h.scope = 'project' AND h.project_id = $1)
	        OR (h.scope = 'user' AND h.user_id = c.author_id)
	        OR (h.scope = 'agent' AND EXISTS (
	               SELECT 1 FROM %[1]s.chat_participant_mapping m
	                 JOIN %[1]s.chat_participants p ON p.id = m.participant_id
	                WHERE m.conversation_id = c.id
	                  AND p.entity_name = 'application'
	                  AND p.entity_meta->>'id' = h.agent_id::text
	                  AND COALESCE(p.entity_meta->>'project_id', $1::text) = h.project_id::text))))`

// holdQuerier is the read a hold check needs: a pool for a route that writes
// in one statement, the caller's transaction otherwise.
type holdQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// legalHoldRefusal is the answer every user-driven destructive path gives on a
// held conversation (ADR-0042 decision 4): delete, clear, delete a message,
// delete its attachments, carve a canvas out of a message, and regenerate or
// edit a turn. One message and one status, so the client renders one thing.
func legalHoldRefusal() error {
	return apierr.Conflict("conversation is under legal hold and cannot be changed")
}

// legalHoldsPresent reports whether shared 0195 has run. Repository fixtures
// build tenant tables without the shared schema, the same reason
// deleteConversationPin guards social_pins.
func legalHoldsPresent(ctx context.Context, q holdQuerier) (bool, error) {
	var present bool
	if err := q.QueryRow(ctx, `SELECT to_regclass('elitea_runtime.legal_holds') IS NOT NULL`).Scan(&present); err != nil {
		return false, fmt.Errorf("conversations: legal hold lookup: %w", err)
	}
	return present, nil
}

// conversationHeld reports whether an active legal hold covers the
// conversation.
func conversationHeld(ctx context.Context, tx holdQuerier, projectID string, id int64) (bool, error) {
	present, err := legalHoldsPresent(ctx, tx)
	if err != nil || !present {
		return false, err
	}
	project, err := strconv.ParseInt(projectID, 10, 64)
	if err != nil {
		return false, nil // not a tenant id: no hold can name it
	}
	var held bool
	q := fmt.Sprintf(`SELECT `+conversationHeldSQL+` FROM %[1]s.chat_conversations c WHERE c.id = $2`, schema(projectID))
	if err := tx.QueryRow(ctx, q, project, id).Scan(&held); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("conversations: legal hold lookup: %w", err)
	}
	return held, nil
}

// conversationHeldByUUID is conversationHeld for a conversation addressed by
// its uuid; a conversation that does not exist is not held.
func conversationHeldByUUID(ctx context.Context, q holdQuerier, projectID int64, conversationUUID string) (bool, error) {
	project := strconv.FormatInt(projectID, 10)
	var id int64
	err := q.QueryRow(ctx, fmt.Sprintf(`SELECT id FROM %s.chat_conversations WHERE uuid = $1::uuid`, schema(project)),
		conversationUUID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("conversations: legal hold lookup: %w", err)
	}
	return conversationHeld(ctx, q, project, id)
}

// refuseIfHeld answers legalHoldRefusal when an active hold covers the
// conversation, nil otherwise.
func refuseIfHeld(ctx context.Context, q holdQuerier, projectID string, id int64) error {
	held, err := conversationHeld(ctx, q, projectID, id)
	if err != nil {
		return err
	}
	if held {
		return legalHoldRefusal()
	}
	return nil
}

// ConversationHeld reports whether an active legal hold covers the
// conversation (numeric id or uuid). The conversation-wide attachment delete
// asks before it removes any bytes, because it removes them before the row
// write that would otherwise refuse.
func (r *ConversationsRepo) ConversationHeld(ctx context.Context, projectID, conversationID string) (bool, error) {
	id, err := r.resolveConversationID(ctx, projectID, conversationID)
	if err != nil {
		return false, err
	}
	return conversationHeld(ctx, r.pool, projectID, id)
}

// RetentionTenantProjects lists the projects whose tenant schema carries the
// chat tables, in id order. The `^p_[0-9]+$` match is the tenant naming every
// other catalogue scan uses.
func (r *ConversationsRepo) RetentionTenantProjects(ctx context.Context) ([]int64, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT substring(namespace.nspname FROM 3)::bigint AS project_id
		  FROM pg_class AS relation
		  JOIN pg_namespace AS namespace ON namespace.oid = relation.relnamespace
		 WHERE relation.relname = 'chat_conversations'
		   AND relation.relkind = 'r'
		   AND namespace.nspname ~ '^p_[0-9]+$'
		 ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("conversation retention: list tenants: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// ExpiredConversations returns up to limit conversations of the project whose
// last activity is before cutoff and that no active legal hold covers, oldest
// first.
func (r *ConversationsRepo) ExpiredConversations(ctx context.Context, projectID int64, cutoff time.Time, limit int) ([]retentionapp.Candidate, error) {
	s := schema(strconv.FormatInt(projectID, 10))
	activity := fmt.Sprintf(conversationActivitySQL, s)
	held := fmt.Sprintf(conversationHeldSQL, s)

	rows, err := r.pool.Query(ctx, fmt.Sprintf(`
		SELECT id, last_activity FROM (
		    SELECT c.id, %[2]s AS last_activity, %[3]s AS held
		      FROM %[1]s.chat_conversations c
		) expired
		 WHERE last_activity < ($2::timestamptz AT TIME ZONE 'UTC') AND NOT held
		 ORDER BY last_activity, id
		 LIMIT $3`, s, activity, held), projectID, cutoff.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("conversation retention: candidates: %w", err)
	}
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (retentionapp.Candidate, error) {
		var c retentionapp.Candidate
		err := row.Scan(&c.ID, &c.LastActivity)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("conversation retention: candidates: %w", err)
	}
	return candidates, nil
}

// HeldExpiredConversations counts the project's conversations whose last
// activity is before cutoff and that an active legal hold keeps. It reads
// the whole tenant, so the sweep asks once per project per pass.
func (r *ConversationsRepo) HeldExpiredConversations(ctx context.Context, projectID int64, cutoff time.Time) (int64, error) {
	s := schema(strconv.FormatInt(projectID, 10))
	var heldCount int64
	if err := r.pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT count(*) FROM (
		    SELECT %[2]s AS last_activity, %[3]s AS held
		      FROM %[1]s.chat_conversations c
		) expired
		 WHERE last_activity < ($2::timestamptz AT TIME ZONE 'UTC') AND held`,
		s, fmt.Sprintf(conversationActivitySQL, s), fmt.Sprintf(conversationHeldSQL, s)),
		projectID, cutoff.UTC()).Scan(&heldCount); err != nil {
		return 0, fmt.Errorf("conversation retention: held count: %w", err)
	}
	return heldCount, nil
}

// DeleteExpiredConversation deletes one conversation the sweep selected,
// after re-checking under the row lock that it is still expired and still
// not held, and journals its stored attachments in the same transaction.
// It returns the outcome and the number of attachment objects journalled.
func (r *ConversationsRepo) DeleteExpiredConversation(ctx context.Context, projectID, conversationID int64, cutoff time.Time) (retentionapp.Outcome, int, error) {
	project := strconv.FormatInt(projectID, 10)
	s := schema(project)

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("conversation retention: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The row lock fences a concurrent user delete and a second replica;
	// SKIP LOCKED leaves a row someone else holds to them.
	var expired bool
	err = tx.QueryRow(ctx, fmt.Sprintf(`
		SELECT %[2]s < ($2::timestamptz AT TIME ZONE 'UTC')
		  FROM %[1]s.chat_conversations c
		 WHERE c.id = $1
		   FOR UPDATE OF c SKIP LOCKED`, s, fmt.Sprintf(conversationActivitySQL, s)),
		conversationID, cutoff.UTC()).Scan(&expired)
	if errors.Is(err, pgx.ErrNoRows) {
		return retentionapp.Gone, 0, nil
	}
	if err != nil {
		return "", 0, fmt.Errorf("conversation retention: lock: %w", err)
	}
	if !expired {
		return retentionapp.Active, 0, nil
	}
	held, err := conversationHeld(ctx, tx, project, conversationID)
	if err != nil {
		return "", 0, err
	}
	if held {
		return retentionapp.Held, 0, nil
	}

	attachments, err := deleteConversationInTx(ctx, tx, project, conversationID)
	if err != nil {
		return "", 0, err
	}
	journalled := 0
	for _, attachment := range attachments {
		if attachment.Bucket == "" || attachment.Name == "" {
			continue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO elitea_runtime.conversation_attachment_cleanup (project_id, conversation_id, bucket, name)
			VALUES ($1, $2, $3, $4)`, projectID, conversationID, attachment.Bucket, attachment.Name); err != nil {
			return "", 0, fmt.Errorf("conversation retention: journal attachment: %w", err)
		}
		journalled++
	}
	if err := tx.Commit(ctx); err != nil {
		return "", 0, fmt.Errorf("conversation retention: commit: %w", err)
	}
	return retentionapp.Deleted, journalled, nil
}

// ClaimAttachmentCleanup claims up to limit due journal rows for lease: their
// next attempt moves past the lease, so a crashed drain's rows come back, and
// a second replica skips them.
func (r *ConversationsRepo) ClaimAttachmentCleanup(ctx context.Context, limit int, lease time.Duration) ([]retentionapp.Cleanup, error) {
	rows, err := r.pool.Query(ctx, `
		UPDATE elitea_runtime.conversation_attachment_cleanup AS j
		   SET next_attempt_at = now() + make_interval(secs => $2::double precision), attempts = j.attempts + 1
		 WHERE j.id IN (
		     SELECT id FROM elitea_runtime.conversation_attachment_cleanup
		      WHERE next_attempt_at <= now()
		      ORDER BY next_attempt_at, id
		      LIMIT $1
		        FOR UPDATE SKIP LOCKED)
		RETURNING j.id, j.project_id, j.conversation_id, j.bucket, j.name, j.attempts`,
		limit, lease.Seconds())
	if err != nil {
		return nil, fmt.Errorf("conversation retention: claim journal: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (retentionapp.Cleanup, error) {
		var c retentionapp.Cleanup
		err := row.Scan(&c.ID, &c.ProjectID, &c.ConversationID, &c.Attachment.Bucket, &c.Attachment.Name, &c.Attempts)
		return c, err
	})
}

// CompleteAttachmentCleanup removes finished journal rows.
func (r *ConversationsRepo) CompleteAttachmentCleanup(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := r.pool.Exec(ctx, `DELETE FROM elitea_runtime.conversation_attachment_cleanup WHERE id = ANY($1)`, ids); err != nil {
		return fmt.Errorf("conversation retention: complete journal: %w", err)
	}
	return nil
}

// FailAttachmentCleanup records a failed attempt and when to retry.
func (r *ConversationsRepo) FailAttachmentCleanup(ctx context.Context, id int64, cause string, retryAt time.Time) error {
	if len(cause) > 1000 {
		cause = cause[:1000]
	}
	if _, err := r.pool.Exec(ctx, `
		UPDATE elitea_runtime.conversation_attachment_cleanup
		   SET last_error = $2, next_attempt_at = $3
		 WHERE id = $1`, id, cause, retryAt.UTC()); err != nil {
		return fmt.Errorf("conversation retention: fail journal: %w", err)
	}
	return nil
}

// AttachmentCleanupBacklog is the journal's size, for the lag metric.
func (r *ConversationsRepo) AttachmentCleanupBacklog(ctx context.Context) (int64, error) {
	var n int64
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.conversation_attachment_cleanup`).Scan(&n); err != nil {
		return 0, fmt.Errorf("conversation retention: journal backlog: %w", err)
	}
	return n, nil
}

var _ retentionapp.Store = (*ConversationsRepo)(nil)
