package securityaudit

import (
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// TypeAdminElevation is one step of a break-glass elevation (ADR-0059
// decision 5, "Fail closed on audit", and decision 8, "The actions of
// admin.elevation"; story R2.3, issue 2517), appended in the transaction of
// the step: a step whose event cannot be written does not happen. Action is
// "admin.elevation.<action>" for one of the ElevationAction values, and
// Elevation carries the step. The event names references and codes only: the
// reason CODE and never the reason text, a note's id and length and never its
// text, a sealed credential's id and never the credential.
const TypeAdminElevation EventType = "ai.elitea.security.v1.admin.elevation"

// ElevationAction is the action of an admin.elevation event. The list is
// closed: an action outside it is refused by Validate, so no free string
// reaches the chain. This story owns the list; ADR-0059 decision 8 repeats
// it.
type ElevationAction string

// The actions, with the story that produces each.
const (
	// ElevationRequested: a person asked for an elevation (R2.2b).
	ElevationRequested ElevationAction = "requested"
	// ElevationApproved, ElevationDenied: the approver decided (R2.2b).
	ElevationApproved ElevationAction = "approved"
	ElevationDenied   ElevationAction = "denied"
	// ElevationRequestExpired: a request was not decided in time (R2.16).
	ElevationRequestExpired ElevationAction = "request_expired"
	// ElevationStarted: an elevation started (R2.2c, R2.7b); written by
	// adminaccess.StartElevation.
	ElevationStarted ElevationAction = "started"
	// ElevationEnded: an elevation ended before its expiry (R2.2c, R2.7b);
	// written by adminaccess.EndElevation.
	ElevationEnded ElevationAction = "ended"
	// ElevationExpired: an elevation reached its expiry (R2.16); written by
	// adminaccess.EndElevation when the stored end reason is `expired`.
	ElevationExpired ElevationAction = "expired"
	// ElevationCriticalChange: a `critical` action made under an elevation
	// (R2.8b).
	ElevationCriticalChange ElevationAction = "critical_change"
	// ElevationReviewed: an ended elevation was reviewed (R2.9a).
	ElevationReviewed ElevationAction = "reviewed"
	// ElevationUnreviewed: the move to unreviewed_no_reviewer (R2.9b).
	ElevationUnreviewed ElevationAction = "unreviewed"
	// ElevationNoteAdded: a review note was added (R2.9c). It carries the
	// note's id and length, never its text.
	ElevationNoteAdded ElevationAction = "note_added"
	// ElevationCredentialIssued, ElevationCredentialUsed,
	// ElevationCredentialRevoked: a sealed credential (R2.6a, R2.7b).
	ElevationCredentialIssued  ElevationAction = "credential_issued"
	ElevationCredentialUsed    ElevationAction = "credential_used"
	ElevationCredentialRevoked ElevationAction = "credential_revoked"
	// ElevationLocalAdminSignin: a holder of an administration role signed
	// in with Form while an identity provider is enabled (R2.15).
	ElevationLocalAdminSignin ElevationAction = "local_admin_signin"
)

// ElevationActions lists every action, in the order of decision 8, for the
// table test that drives one event per action and for the docs.
func ElevationActions() []ElevationAction {
	return []ElevationAction{
		ElevationRequested, ElevationApproved, ElevationDenied, ElevationRequestExpired,
		ElevationStarted, ElevationEnded, ElevationExpired, ElevationCriticalChange,
		ElevationReviewed, ElevationUnreviewed, ElevationNoteAdded,
		ElevationCredentialIssued, ElevationCredentialUsed, ElevationCredentialRevoked,
		ElevationLocalAdminSignin,
	}
}

var elevationActions = func() map[ElevationAction]bool {
	known := map[ElevationAction]bool{}
	for _, action := range ElevationActions() {
		known[action] = true
	}
	return known
}()

// Valid reports whether a is a known action.
func (a ElevationAction) Valid() bool { return elevationActions[a] }

// EventAction answers the Action of an admin.elevation event of action a.
func (a ElevationAction) EventAction() string { return "admin.elevation." + string(a) }

// elevationBound reports an action about one elevation row: it names the
// elevation. The credential actions and the local sign-in are about a
// credential or a sign-in; credential_used names the elevation it started
// when there is one.
func (a ElevationAction) elevationBound() bool {
	switch a {
	case ElevationCredentialIssued, ElevationCredentialUsed, ElevationCredentialRevoked, ElevationLocalAdminSignin:
		return false
	}
	return true
}

func (a ElevationAction) credential() bool {
	switch a {
	case ElevationCredentialIssued, ElevationCredentialUsed, ElevationCredentialRevoked:
		return true
	}
	return false
}

// The sources of an admin.elevation event: how the step was taken.
const (
	ElevationSourceConsole = "console"
	ElevationSourceAPI     = "api"
	// ElevationSourceOperator: a process command of the operator.
	ElevationSourceOperator = "operator"
	// ElevationSourceSystem: the platform itself (the reconciler's expiry,
	// the move to unreviewed_no_reviewer).
	ElevationSourceSystem = "system"
)

// ElevationChange is the payload of an admin.elevation event. Every value is
// an id, a code or a number: no free text. THERE IS NO REASON TEXT FIELD AND
// NO NOTE TEXT FIELD, and there must never be one (decision 8: the text stays
// in the ledger; TestElevationChangeHasNoFreeTextField asserts it on the Go
// type and on the export schema).
type ElevationChange struct {
	Action ElevationAction
	// Source is how the step was taken: console, api, operator or system.
	Source string
	// ElevationID is the elevation row (elitea_auth.admin_elevations.id).
	ElevationID string
	// UserID is the person elevated, or who signed in.
	UserID int64
	// Level, Path and ReasonCode are the elevation's level (admin,
	// super_admin), path (approved, self, sealed) and reason code, when
	// known at the step.
	Level      string
	Path       string
	ReasonCode string
	// ExpiresAt is the elevation's expiry, on a step after the start.
	ExpiresAt time.Time
	// EndReason is the stored end reason, on ended and expired.
	EndReason string
	// NoteID and NoteLength are a review note's id and its length in
	// characters, on note_added only.
	NoteID     string
	NoteLength int
	// CredentialID is the sealed credential, on the credential actions only.
	CredentialID string
}

// elevationData is the canonical JSON of ElevationChange, inside eventData.
// Its field order and tags are part of the chain contract.
type elevationData struct {
	Action       string `json:"action"`
	Source       string `json:"source"`
	ElevationID  string `json:"elevation_id,omitempty"`
	UserID       int64  `json:"user_id,omitempty"`
	Level        string `json:"level,omitempty"`
	Path         string `json:"path,omitempty"`
	ReasonCode   string `json:"reason_code,omitempty"`
	ExpiresAt    string `json:"expires_at,omitempty"`
	EndReason    string `json:"end_reason,omitempty"`
	NoteID       string `json:"note_id,omitempty"`
	NoteLength   *int   `json:"note_length,omitempty"`
	CredentialID string `json:"credential_id,omitempty"`
}

func (c *ElevationChange) data() *elevationData {
	if c == nil {
		return nil
	}
	d := &elevationData{
		Action:       string(c.Action),
		Source:       c.Source,
		ElevationID:  c.ElevationID,
		UserID:       c.UserID,
		Level:        c.Level,
		Path:         c.Path,
		ReasonCode:   c.ReasonCode,
		EndReason:    c.EndReason,
		NoteID:       c.NoteID,
		CredentialID: c.CredentialID,
	}
	if !c.ExpiresAt.IsZero() {
		d.ExpiresAt = c.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	if c.Action == ElevationNoteAdded {
		length := c.NoteLength
		d.NoteLength = &length
	}
	return d
}

// validate checks the payload against its event.
func (c *ElevationChange) validate(e SecurityEvent) error {
	if e.Type != TypeAdminElevation {
		if c != nil {
			return fmt.Errorf("%w: an elevation step belongs to %s only", ErrInvalidEvent, TypeAdminElevation)
		}
		return nil
	}
	if c == nil {
		return fmt.Errorf("%w: %s requires an elevation step", ErrInvalidEvent, TypeAdminElevation)
	}
	if !c.Action.Valid() {
		return fmt.Errorf("%w: unknown elevation action", ErrInvalidEvent)
	}
	if e.Action != c.Action.EventAction() {
		return fmt.Errorf("%w: the action of an elevation step is admin.elevation.<action>", ErrInvalidEvent)
	}
	switch c.Source {
	case ElevationSourceConsole, ElevationSourceAPI, ElevationSourceOperator, ElevationSourceSystem:
	default:
		return fmt.Errorf("%w: unknown elevation source", ErrInvalidEvent)
	}
	if c.ElevationID != "" {
		if parsed, err := uuid.Parse(c.ElevationID); err != nil || parsed.String() != c.ElevationID {
			return fmt.Errorf("%w: elevation.elevation_id must be a lower-case hyphenated UUID", ErrInvalidEvent)
		}
	}
	if c.Action.elevationBound() && c.ElevationID == "" {
		return fmt.Errorf("%w: %s names its elevation", ErrInvalidEvent, c.Action)
	}
	if c.UserID < 0 {
		return fmt.Errorf("%w: elevation.user_id must not be negative", ErrInvalidEvent)
	}
	for name, code := range map[string]string{
		"level": c.Level, "path": c.Path, "reason_code": c.ReasonCode, "end_reason": c.EndReason,
	} {
		if code != "" && !reasonPattern.MatchString(code) {
			return fmt.Errorf("%w: elevation.%s is not a code", ErrInvalidEvent, name)
		}
	}
	if (c.EndReason != "") != (c.Action == ElevationEnded || c.Action == ElevationExpired) {
		return fmt.Errorf("%w: an end reason belongs to ended and expired, and they carry one", ErrInvalidEvent)
	}
	if c.Action == ElevationNoteAdded {
		if c.NoteID == "" || c.NoteLength <= 0 {
			return fmt.Errorf("%w: note_added carries the note's id and length", ErrInvalidEvent)
		}
	} else if c.NoteID != "" || c.NoteLength != 0 {
		return fmt.Errorf("%w: a note belongs to note_added only", ErrInvalidEvent)
	}
	if c.Action.credential() != (c.CredentialID != "") {
		return fmt.Errorf("%w: a credential id belongs to the credential actions, and they carry one", ErrInvalidEvent)
	}
	for name, value := range map[string]string{"note_id": c.NoteID, "credential_id": c.CredentialID} {
		if value != "" && !reasonPattern.MatchString(value) {
			return fmt.Errorf("%w: elevation.%s is not an id", ErrInvalidEvent, name)
		}
	}
	return nil
}

// AdminElevation is the input of NewAdminElevation: who took which step.
type AdminElevation struct {
	// ActorUserID is the person who took the step (the requester, the
	// approver, the holder, the reviewer), 0 for the platform or an
	// operator command.
	ActorUserID int64
	// AuthenticationRef is the session or token row behind the step, never
	// the credential value.
	AuthenticationRef string
	// CorrelationID ties the event to the request (the trace id).
	CorrelationID string
	Change        ElevationChange
}

// NewAdminElevation builds the admin.elevation event of one step, on the
// platform partition. The appender assigns the id, the source and the time,
// and validates it.
func NewAdminElevation(c AdminElevation) SecurityEvent {
	change := c.Change
	principal := ""
	if c.ActorUserID > 0 {
		principal = "user:" + strconv.FormatInt(c.ActorUserID, 10)
	}
	effect := EffectAllow
	if change.Action == ElevationDenied {
		effect = EffectDeny
	}
	reasons := []string{"action:" + string(change.Action), "source:" + change.Source}
	if change.ReasonCode != "" {
		reasons = append(reasons, "reason_code:"+change.ReasonCode)
	}
	event := SecurityEvent{
		Type:              TypeAdminElevation,
		Action:            change.Action.EventAction(),
		Effect:            effect,
		Reasons:           reasons,
		PrincipalRef:      principal,
		AuthenticationRef: c.AuthenticationRef,
		CorrelationID:     c.CorrelationID,
		Elevation:         &change,
	}
	switch {
	case change.ElevationID != "":
		event.ResourceType, event.ResourceID = "elevation", change.ElevationID
	case change.CredentialID != "":
		event.ResourceType, event.ResourceID = "sealed_credential", change.CredentialID
	case change.UserID > 0:
		event.ResourceType, event.ResourceID = "user", strconv.FormatInt(change.UserID, 10)
	}
	return event
}
