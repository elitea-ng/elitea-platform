package adminaccess

// The notes on an elevation's record and the acknowledgement of an elevation
// nobody independent could review (ADR-0059 decision 5, "Review after use",
// "Notes"; story R2.9c, issue 2604; shared 0285).
//
//   - A note is attached in any state and never changes it. The operator
//     writes one with `elitea-main break-glass note <elevation id>` (source
//     operator, no person); the text stays in the items table and its
//     note_added event carries the note's id and length only.
//   - A standing global administrator acknowledges an
//     unreviewed_no_reviewer elevation through the console, with a note
//     marked as the acknowledgement: once per elevation, and the record
//     keeps saying that no independent review happened (the review state is
//     not touched).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// The sources of a note (admin_elevation_items.note_source).
const (
	NoteSourceOperator = "operator"
	NoteSourceConsole  = "console"
)

// ReasonAcknowledgement is the reason code of the note_added event of an
// acknowledgement.
const ReasonAcknowledgement = "acknowledgement"

var (
	// ErrElevationNotAcknowledgeable: only an unreviewed_no_reviewer
	// elevation is acknowledged.
	ErrElevationNotAcknowledgeable = errors.New(
		"elevation acknowledgement refused: only an elevation nobody independent could review is acknowledged")
	// ErrElevationAcknowledged: the elevation was acknowledged already.
	ErrElevationAcknowledged = errors.New("elevation acknowledgement refused: the elevation was acknowledged already")
)

// ElevationNote is one note to attach.
type ElevationNote struct {
	ID uuid.UUID
	// Text is the note; required, at most MaxReviewNoteLength characters.
	Text string
	// Source is NoteSourceOperator (AuthorUserID 0) or NoteSourceConsole
	// (AuthorUserID the person).
	Source       string
	AuthorUserID int64
	// Acknowledge marks the note as the acknowledgement of an
	// unreviewed_no_reviewer elevation (console only).
	Acknowledge bool
	At          time.Time
	// AuthenticationRef and CorrelationID are the event's.
	AuthenticationRef string
	CorrelationID     string
}

// NoteResult is what AddElevationNote wrote.
type NoteResult struct {
	NoteID    uuid.UUID
	Elevation ElevationRecord
}

func (n ElevationNote) validate() error {
	text := strings.TrimSpace(n.Text)
	switch {
	case text == "":
		return fmt.Errorf("%w: a note needs its text", ErrElevationInvalid)
	case utf8.RuneCountInString(text) > MaxReviewNoteLength:
		return fmt.Errorf("%w: the note is longer than %d characters", ErrElevationInvalid, MaxReviewNoteLength)
	case n.At.IsZero():
		return ErrElevationNoTimestamp
	case n.Source == NoteSourceOperator && (n.AuthorUserID != 0 || n.Acknowledge):
		return fmt.Errorf("%w: an operator note names no person and acknowledges nothing", ErrElevationInvalid)
	case n.Source == NoteSourceConsole && n.AuthorUserID <= 0:
		return fmt.Errorf("%w: a console note names its person", ErrElevationInvalid)
	case n.Source != NoteSourceOperator && n.Source != NoteSourceConsole:
		return fmt.Errorf("%w: unknown note source %q", ErrElevationInvalid, n.Source)
	}
	return nil
}

// AddElevationNote attaches the note to the elevation on tx and appends its
// note_added event. The elevation row is locked and never changed. An error
// means the caller rolls back.
func AddElevationNote(ctx context.Context, tx pgx.Tx, appender securityaudit.Appender, note ElevationNote) (NoteResult, error) {
	if err := note.validate(); err != nil {
		return NoteResult{}, err
	}
	if appender == nil {
		return NoteResult{}, ErrElevationNoAppender
	}
	if tx == nil {
		return NoteResult{}, errors.New("add an elevation note: no transaction")
	}
	elevation, err := scanElevation(tx.QueryRow(ctx, `
SELECT `+elevationColumns+` FROM elitea_auth.admin_elevations WHERE id = $1 FOR UPDATE`, note.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return NoteResult{}, ErrElevationNotFound
	}
	if err != nil {
		return NoteResult{}, fmt.Errorf("add an elevation note: %w", err)
	}
	if note.Acknowledge {
		if elevation.ReviewState != ElevationUnreviewedNoReviewer {
			return NoteResult{}, ErrElevationNotAcknowledgeable
		}
		var acknowledged bool
		if err := tx.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM elitea_auth.admin_elevation_items WHERE elevation_id = $1 AND acknowledgement)`,
			note.ID).Scan(&acknowledged); err != nil {
			return NoteResult{}, fmt.Errorf("add an elevation note: read the acknowledgement: %w", err)
		}
		if acknowledged {
			return NoteResult{}, ErrElevationAcknowledged
		}
	}
	text := strings.TrimSpace(note.Text)
	var author *int64
	if note.AuthorUserID > 0 {
		author = &note.AuthorUserID
	}
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `
INSERT INTO elitea_auth.admin_elevation_items
    (elevation_id, kind, user_id, note, note_source, acknowledgement, created_at)
VALUES ($1, '`+ReviewItemNote+`', $2, $3, $4, $5, $6)
RETURNING id`, note.ID, author, text, note.Source, note.Acknowledge, note.At.UTC()).Scan(&id); err != nil {
		return NoteResult{}, fmt.Errorf("add an elevation note: %w", err)
	}
	source := securityaudit.ElevationSourceOperator
	if note.Source == NoteSourceConsole {
		source = securityaudit.ElevationSourceConsole
	}
	var reasons []string
	if note.Acknowledge {
		reasons = []string{ReasonAcknowledgement}
	}
	if err := AppendElevationEvent(ctx, tx, appender, ElevationEvent{
		Action:            securityaudit.ElevationNoteAdded,
		Source:            source,
		Elevation:         elevation,
		ActorUserID:       note.AuthorUserID,
		AuthenticationRef: note.AuthenticationRef,
		CorrelationID:     note.CorrelationID,
		NoteID:            id.String(),
		NoteText:          text,
		Reasons:           reasons,
	}); err != nil {
		return NoteResult{}, err
	}
	return NoteResult{NoteID: id, Elevation: elevation}, nil
}

// ElevationNoteRecord is one note of an elevation.
type ElevationNoteRecord struct {
	ID              uuid.UUID
	Source          string
	AuthorUserID    int64
	Text            string
	Acknowledgement bool
	CreatedAt       time.Time
}

// ElevationNotes answers the notes of the elevation, oldest first.
func ElevationNotes(ctx context.Context, q Querier, id uuid.UUID) ([]ElevationNoteRecord, error) {
	rows, err := q.Query(ctx, `
SELECT id, note_source, COALESCE(user_id, 0), note, acknowledgement, created_at
FROM elitea_auth.admin_elevation_items
WHERE elevation_id = $1 AND kind = '`+ReviewItemNote+`'
ORDER BY created_at, id`, id)
	if err != nil {
		return nil, fmt.Errorf("read the elevation notes: %w", err)
	}
	notes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ElevationNoteRecord, error) {
		var n ElevationNoteRecord
		err := row.Scan(&n.ID, &n.Source, &n.AuthorUserID, &n.Text, &n.Acknowledgement, &n.CreatedAt)
		n.CreatedAt = n.CreatedAt.UTC()
		return n, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the elevation notes: %w", err)
	}
	return notes, nil
}

// ElevationAcknowledgement answers the note that acknowledged the elevation,
// and whether there is one.
func ElevationAcknowledgement(ctx context.Context, q Querier, id uuid.UUID) (uuid.UUID, bool, error) {
	var note uuid.UUID
	err := q.QueryRow(ctx, `
SELECT id FROM elitea_auth.admin_elevation_items WHERE elevation_id = $1 AND acknowledgement`, id).Scan(&note)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("read the acknowledgement: %w", err)
	}
	return note, true, nil
}

// ElevationSummary is one elevation as the operator's list prints it: its
// id, person, level, path and state. No reason text.
type ElevationSummary struct {
	ID          uuid.UUID
	UserID      int64
	Level       string
	Path        string
	State       string
	RequestedAt time.Time
}

// RecentElevations answers the newest elevations, at most limit, with their
// states at now.
func RecentElevations(ctx context.Context, q Querier, limit int, now time.Time) ([]ElevationSummary, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := q.Query(ctx, `
SELECT `+elevationColumns+` FROM elitea_auth.admin_elevations
ORDER BY requested_at DESC, id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list the elevations: %w", err)
	}
	records, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ElevationRecord, error) {
		return scanElevation(row)
	})
	if err != nil {
		return nil, fmt.Errorf("list the elevations: %w", err)
	}
	out := make([]ElevationSummary, 0, len(records))
	for _, e := range records {
		out = append(out, ElevationSummary{ID: e.ID, UserID: e.UserID, Level: e.Level, Path: e.Path,
			State: e.State(now), RequestedAt: e.RequestedAt})
	}
	return out, nil
}
