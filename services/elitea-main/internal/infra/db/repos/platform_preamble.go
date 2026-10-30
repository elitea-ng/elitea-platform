package repos

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// The platform preamble's revisions (ADR-0043 decision 3, shared 0216).

// platformPreambleRevisionLock serializes the revision numbering of
// concurrent saves (a transaction-scoped advisory lock).
const platformPreambleRevisionLock = int64(0x0216_0043)

// ActionPlatformPreambleRevised is the action of the security audit event a
// new preamble revision writes.
const ActionPlatformPreambleRevised = "config.platform_preamble.revised"

var (
	preambleAuditOnce     sync.Once
	preambleAuditAppender *securityaudit.PostgresAppender
	preambleAuditErr      error
)

func platformPreambleAppender() (*securityaudit.PostgresAppender, error) {
	preambleAuditOnce.Do(func() {
		preambleAuditAppender, preambleAuditErr = securityaudit.NewPostgresAppender("elitea-main")
	})
	return preambleAuditAppender, preambleAuditErr
}

// PlatformPreambleRevision is the outcome of one save.
type PlatformPreambleRevision struct {
	// Revision is the newest revision after the save; 0 when there is none.
	Revision int
	// Recorded reports that the save wrote a new revision.
	Recorded bool
}

// RecordPlatformPreambleRevisionTx writes a new revision of the platform
// preamble and its security audit event (ai.elitea.security.v1.config.changed,
// ActionPlatformPreambleRevised) in tx, the transaction of the section save,
// when text differs from the newest revision's text. A first save of an
// empty text writes no revision. The event carries the revision and the
// text's sha256, never the text.
func RecordPlatformPreambleRevisionTx(ctx context.Context, tx pgx.Tx, text, author string) (PlatformPreambleRevision, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, platformPreambleRevisionLock); err != nil {
		return PlatformPreambleRevision{}, fmt.Errorf("platform preamble: lock the revisions: %w", err)
	}
	var (
		latest     int
		latestText string
	)
	err := tx.QueryRow(ctx, `
SELECT revision, text FROM elitea_runtime.platform_preamble_revisions
ORDER BY revision DESC LIMIT 1`).Scan(&latest, &latestText)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return PlatformPreambleRevision{}, fmt.Errorf("platform preamble: read the newest revision: %w", err)
	}
	if text == latestText {
		return PlatformPreambleRevision{Revision: latest}, nil
	}
	appender, err := platformPreambleAppender()
	if err != nil {
		return PlatformPreambleRevision{}, fmt.Errorf("platform preamble: the audit appender: %w", err)
	}
	digest := sha256.Sum256([]byte(text))
	sum := hex.EncodeToString(digest[:])
	revision := latest + 1
	record, err := appender.AppendRecord(ctx, tx, securityaudit.SecurityEvent{
		Type:         securityaudit.TypeConfigChanged,
		Action:       ActionPlatformPreambleRevised,
		Effect:       securityaudit.EffectAllow,
		Reasons:      []string{"revision:" + strconv.Itoa(revision), "sha256:" + sum},
		PrincipalRef: author,
		ResourceType: "platform_preamble",
		ResourceID:   strconv.Itoa(revision),
	})
	if err != nil {
		return PlatformPreambleRevision{}, fmt.Errorf("platform preamble: append the audit event: %w", err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.platform_preamble_revisions (revision, text, sha256, saved_by, audit_event_id)
VALUES ($1, $2, $3, $4, $5::uuid)`, revision, text, sum, author, record.EventID); err != nil {
		return PlatformPreambleRevision{}, fmt.Errorf("platform preamble: write the revision: %w", err)
	}
	return PlatformPreambleRevision{Revision: revision, Recorded: true}, nil
}
