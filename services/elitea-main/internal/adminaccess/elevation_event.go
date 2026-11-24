package adminaccess

// The security audit event of the elevation ledger (ADR-0059 decision 5,
// "Fail closed on audit", and decision 8, "The actions of admin.elevation";
// story R2.3, issue 2517).
//
// Every step of an elevation is one chained ai.elitea.security.v1.admin.elevation
// event, appended on the SAME transaction as the ledger write. StartElevation
// and EndElevation append theirs themselves, under a savepoint with the row,
// so a step whose event cannot be written leaves no row even if the caller
// commits after the error. The later stories (the request routes, the
// review, the sealed credential, the local sign-in) write theirs through
// AppendElevationEvent on their own transaction, before they commit.
//
// The event carries the reason CODE and never the reason text; a review note
// is its id and its length, never its text. NewElevationEvent drops both
// texts whatever the caller passes, and the securityaudit payload has no
// field that could hold them.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// ElevationEvent is one step of an elevation, for its admin.elevation event.
type ElevationEvent struct {
	// Action is one of the closed list (securityaudit.ElevationActions).
	Action securityaudit.ElevationAction
	// Source is how the step was taken: securityaudit.ElevationSourceConsole,
	// ...API, ...Operator (a process command) or ...System. Required.
	Source string
	// Elevation is the ledger row the step is about; the zero value for a
	// step about no elevation (a sealed credential issued or revoked, a
	// local sign-in). Its id, person, level, path, reason code, expiry and
	// end reason are recorded. ITS REASON TEXT IS NEVER COPIED.
	Elevation ElevationRecord
	// UserID is the person of a step about no elevation (local_admin_signin);
	// 0 takes the elevation's person.
	UserID int64
	// ActorUserID is who took the step; 0 for the platform or an operator.
	ActorUserID int64
	// AuthenticationRef is the session or token row behind the step, never
	// the credential value ("browser_session:<ref>").
	AuthenticationRef string
	// CorrelationID ties the event to the request (the trace id).
	CorrelationID string
	// NoteID and NoteText are the review note of note_added. Only the id and
	// the length of the text in characters are recorded; THE TEXT IS NEVER
	// COPIED.
	NoteID   string
	NoteText string
	// CredentialID is the sealed credential of the credential actions.
	CredentialID string
	// Reasons are further reason codes (for example the end reason the
	// caller asked for, when the ledger stored another).
	Reasons []string
}

// ErrElevationEvent wraps every refusal of NewElevationEvent: the step is
// not one of the list, or a value is not one the ledger knows.
var ErrElevationEvent = errors.New("elevation event refused")

// NewElevationEvent builds the admin.elevation event of one step and checks
// it the way the appender will: an unknown action, source, level, path,
// reason code or end reason is an error here, before anything is written.
func NewElevationEvent(ev ElevationEvent) (securityaudit.SecurityEvent, error) {
	if !ev.Action.Valid() {
		return securityaudit.SecurityEvent{}, fmt.Errorf("%w: unknown action %q", ErrElevationEvent, ev.Action)
	}
	record := ev.Elevation
	// A fixed order, so the refusal names the same field every time.
	for _, check := range []struct {
		name  string
		known bool
	}{
		{"level", record.Level == "" || record.Level == ElevationLevelAdmin || record.Level == ElevationLevelSuperAdmin},
		{"path", record.Path == "" || record.Path == ElevationPathApproved || record.Path == ElevationPathSelf || record.Path == ElevationPathSealed},
		{"reason code", record.ReasonCode == "" || knownElevationReasonCode(record.ReasonCode)},
		{"end reason", record.EndReason == "" || knownElevationEndReason(record.EndReason)},
	} {
		if !check.known {
			return securityaudit.SecurityEvent{}, fmt.Errorf("%w: unknown %s", ErrElevationEvent, check.name)
		}
	}
	change := securityaudit.ElevationChange{
		Action:       ev.Action,
		Source:       ev.Source,
		UserID:       ev.UserID,
		Level:        record.Level,
		Path:         record.Path,
		ReasonCode:   record.ReasonCode,
		CredentialID: ev.CredentialID,
	}
	if record.ID != uuid.Nil {
		change.ElevationID = record.ID.String()
	}
	if change.UserID == 0 {
		change.UserID = record.UserID
	}
	if !record.StartedAt.IsZero() {
		change.ExpiresAt = record.ExpiresAt
	}
	if ev.Action == securityaudit.ElevationEnded || ev.Action == securityaudit.ElevationExpired {
		change.EndReason = record.EndReason
	}
	if ev.Action == securityaudit.ElevationNoteAdded {
		change.NoteID, change.NoteLength = ev.NoteID, utf8.RuneCountInString(ev.NoteText)
	}
	event := securityaudit.NewAdminElevation(securityaudit.AdminElevation{
		ActorUserID:       ev.ActorUserID,
		AuthenticationRef: ev.AuthenticationRef,
		CorrelationID:     ev.CorrelationID,
		Change:            change,
	})
	event.Reasons = append(event.Reasons, ev.Reasons...)
	// Validate as the appender will, with the fields it assigns stood in.
	probe := event
	probe.ID, probe.Source, probe.Time = uuid.NewString(), "elitea-main/check", time.Unix(1, 0)
	if err := probe.Validate(); err != nil {
		return securityaudit.SecurityEvent{}, fmt.Errorf("%w: %w", ErrElevationEvent, err)
	}
	return event, nil
}

// AppendElevationEvent writes the admin.elevation event of one step on tx,
// the transaction of the step's ledger write. An error means the caller
// rolls back: the step does not happen without its evidence.
func AppendElevationEvent(ctx context.Context, tx pgx.Tx, appender securityaudit.Appender, ev ElevationEvent) error {
	if appender == nil {
		return ErrElevationNoAppender
	}
	if tx == nil {
		return errors.New("append the elevation event: no transaction")
	}
	event, err := NewElevationEvent(ev)
	if err != nil {
		return err
	}
	if err := appender.Append(ctx, tx, event); err != nil {
		return fmt.Errorf("append the elevation event: %w", err)
	}
	return nil
}

// withSavepoint runs write on a savepoint of tx and releases it, or rolls it
// back when write fails: the ledger row and its event are kept together or
// dropped together, even when the caller commits tx after the error.
func withSavepoint(ctx context.Context, tx pgx.Tx, write func(pgx.Tx) error) error {
	savepoint, err := tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("elevation savepoint: %w", err)
	}
	if err := write(savepoint); err != nil {
		if rollbackErr := savepoint.Rollback(context.WithoutCancel(ctx)); rollbackErr != nil {
			return errors.Join(err, fmt.Errorf("roll back the elevation savepoint: %w", rollbackErr))
		}
		return err
	}
	if err := savepoint.Commit(ctx); err != nil {
		return fmt.Errorf("release the elevation savepoint: %w", err)
	}
	return nil
}

// sessionAuthenticationRef is the AuthenticationRef of a step taken from the
// browser session an elevation is bound to.
func sessionAuthenticationRef(ref int64) string {
	if ref <= 0 {
		return ""
	}
	return "browser_session:" + strconv.FormatInt(ref, 10)
}

func knownElevationSource(source string) bool {
	switch source {
	case securityaudit.ElevationSourceConsole, securityaudit.ElevationSourceAPI,
		securityaudit.ElevationSourceOperator, securityaudit.ElevationSourceSystem:
		return true
	}
	return false
}

func knownElevationReasonCode(code string) bool {
	switch code {
	case ElevationReasonIncident, ElevationReasonIdentityOutage, ElevationReasonRecovery, ElevationReasonOther:
		return true
	}
	return false
}

func knownElevationEndReason(reason string) bool {
	switch reason {
	case ElevationEndHolder, ElevationEndApprover, ElevationEndOperator, ElevationEndExpired,
		ElevationEndLogout, ElevationEndSessionRevoked, ElevationEndUserSuspended,
		ElevationEndUserDeleted, ElevationEndSCIMDeactivated:
		return true
	}
	return false
}

// defaultEndSource is the source of an end the caller gave none for: the
// platform ends an elevation on expiry and on an account or session event,
// an operator by the operator end, a person in the console otherwise.
func defaultEndSource(reason string) string {
	switch reason {
	case ElevationEndExpired, ElevationEndSessionRevoked, ElevationEndUserSuspended,
		ElevationEndUserDeleted, ElevationEndSCIMDeactivated:
		return securityaudit.ElevationSourceSystem
	case ElevationEndOperator:
		return securityaudit.ElevationSourceOperator
	}
	return securityaudit.ElevationSourceConsole
}
