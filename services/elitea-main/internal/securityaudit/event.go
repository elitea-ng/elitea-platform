// Package securityaudit is the durable, non-dropping security audit path of
// ADR-0039 (issue #1114).
//
// Every governed decision, credential redemption, grant, binding, mandate or
// delegation change and epoch bump writes one SecurityEvent through an
// Appender, in the SAME transaction as the change. The event goes into
// elitea_runtime.security_audit_outbox with a per-partition hash chain. The
// Relay copies it, in seq order, to the append-only durable store
// elitea_identity.security_audit_events and then to an optional Exporter.
// Nothing on this path drops an event: an append error fails the caller, a
// relay or exporter error leaves the row pending, and the backlog metric
// raises an alert above the policy threshold.
//
// The HTTP access audit (internal/audit) is a different path. It favours
// request availability and may drop events. It is not changed here.
//
// PostgreSQL is the authority. No event goes on NATS.
//
// An event carries references and digests only. It never holds a token, a
// secret, an assertion or a provider response (spec-identity-tenant-boundary,
// "Error and audit boundary").
package securityaudit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// EventType is the CloudEvents `type` attribute.
type EventType string

const (
	TypeDecision          EventType = "ai.elitea.security.v1.decision"
	TypeCredentialRedeem  EventType = "ai.elitea.security.v1.credential.redeemed"
	TypeGrantChanged      EventType = "ai.elitea.security.v1.grant.changed"
	TypeBindingChanged    EventType = "ai.elitea.security.v1.binding.changed"
	TypeMandateChanged    EventType = "ai.elitea.security.v1.mandate.changed"
	TypeDelegationChanged EventType = "ai.elitea.security.v1.delegation.changed"
	TypeEpochBumped       EventType = "ai.elitea.security.v1.epoch.bumped"
	TypeProbe             EventType = "ai.elitea.security.v1.probe"
)

// OCSF class and category uids (https://schema.ocsf.io).
const (
	OCSFCategoryIAM                 = 3
	OCSFCategoryApplicationActivity = 6

	OCSFClassEntityManagement     = 3004
	OCSFClassAuthorizeSession     = 3003
	OCSFClassUserAccessManagement = 3005
	OCSFClassAPIActivity          = 6003
)

// ocsfClasses is the OCSF class of each event type. An event type with no
// entry is unknown and refused.
var ocsfClasses = map[EventType]int{
	TypeDecision:          OCSFClassAuthorizeSession,
	TypeCredentialRedeem:  OCSFClassAuthorizeSession,
	TypeGrantChanged:      OCSFClassUserAccessManagement,
	TypeMandateChanged:    OCSFClassUserAccessManagement,
	TypeDelegationChanged: OCSFClassUserAccessManagement,
	TypeBindingChanged:    OCSFClassEntityManagement,
	TypeEpochBumped:       OCSFClassEntityManagement,
	TypeProbe:             OCSFClassAPIActivity,
}

// OCSFClassUID returns the OCSF class of t and whether t is known.
func OCSFClassUID(t EventType) (int, bool) {
	class, ok := ocsfClasses[t]
	return class, ok
}

// Valid reports whether t is a known event type.
func (t EventType) Valid() bool {
	_, ok := ocsfClasses[t]
	return ok
}

// Effect is the result of a decision.
type Effect string

const (
	EffectAllow Effect = "allow"
	EffectDeny  Effect = "deny"
)

// Arrangement names the authority that decided (ADR-0039 decision 9).
type Arrangement string

const (
	ArrangementA Arrangement = "A"
	ArrangementB Arrangement = "B"
	ArrangementC Arrangement = "C"
)

// ActorFidelity is the normalised actor fidelity (ADR-0039 decision 5).
type ActorFidelity string

const (
	FidelityNativeAct     ActorFidelity = "native_act"
	FidelityConfiguredAct ActorFidelity = "configured_act"
	FidelityAZPOnly       ActorFidelity = "azp_only"
	FidelityAttributed    ActorFidelity = "attributed"
	FidelityNone          ActorFidelity = "none"
)

const (
	// SpecVersion is the CloudEvents version of the envelope.
	SpecVersion = "1.0"
	// DataSchema identifies the data payload schema.
	DataSchema = "urn:elitea:security-audit:v1"
	// DataContentType is the content type of the data payload.
	DataContentType = "application/json"

	maxStringBytes = 256
	maxListItems   = 32
	// sequenceDigits zero-pads the CloudEvents `sequence` extension, which
	// orders as a string.
	sequenceDigits = 20
)

// SecurityEvent is one governed security event. The caller fills the fields;
// the appender assigns the chain position and the hashes.
//
// Every field is a reference, an identifier, a digest or a bounded enum. Add
// no field that holds a credential value: TestSecurityEventHasNoSecretLikeFields
// refuses such names, and Validate refuses values shaped like a bearer token.
type SecurityEvent struct {
	// ID is the CloudEvents id, a UUID. The appender assigns a v7 UUID when
	// it is empty.
	ID string
	// Type is the CloudEvents type.
	Type EventType
	// Source is the CloudEvents source, "elitea-main/<instance>". The
	// appender fills its own when it is empty.
	Source string
	// Time is the event time. The appender uses its clock when it is zero.
	Time time.Time
	// TenantProjectID is the chain partition. 0 is the platform partition
	// (events with no tenant, for example a policy revision change).
	TenantProjectID int64

	// Action is the governed operation ("credential.use", "grant.revoke").
	Action string
	// Effect is the decision result. Required for TypeDecision.
	Effect Effect
	// Reasons are stable reason codes, never free text from a provider.
	Reasons []string

	// PrincipalRef is the authenticated principal (AuthZEN subject).
	PrincipalRef string
	// ActorChain is the RFC 8693 actor chain, outermost first, as references.
	ActorChain []string
	// ActorFidelity is the normalised actor fidelity.
	ActorFidelity ActorFidelity
	// CallingApplicationID is the inbound client id (gate D8).
	CallingApplicationID string
	// AuthenticationRef is the auth, session or PAT identifier. It is the row
	// id, never the credential value.
	AuthenticationRef string
	// AgentBindingID is the AgentIdentityBinding reference.
	AgentBindingID string
	// AuthorityID names the external authority that was consulted.
	AuthorityID string
	// MandateRef and DelegationRef name the authority for the use.
	MandateRef    string
	DelegationRef string
	// GrantRef names the CredentialUseGrant (id or digest).
	GrantRef string
	// WorkloadID is the workload SPIFFE id.
	WorkloadID   string
	ExecutionID  string
	InvocationID string
	DecisionID   string
	ResourceType string
	ResourceID   string

	PolicyRevisionID   string
	AuthorizationEpoch int64
	RevocationEpoch    int64
	Arrangement        Arrangement
	// CorrelationID is the trace or request correlation id.
	CorrelationID string
}

// PartitionKey is the chain partition of the event.
func (e SecurityEvent) PartitionKey() string {
	return PartitionKeyFor(e.TenantProjectID)
}

// PartitionKeyFor is the chain partition of a tenant project id.
func PartitionKeyFor(tenantProjectID int64) string {
	return strconv.FormatInt(tenantProjectID, 10)
}

// ErrInvalidEvent refuses an event that does not match the contract. Its
// wrapped text names the field, never the value.
var ErrInvalidEvent = errors.New("security audit event is invalid")

var (
	reasonPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.:-]{0,127}$`)
	// jwtLike is three base64url segments starting with a JSON header.
	jwtLike = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.`)
)

// Validate checks the event before it is chained. The appender calls it.
func (e SecurityEvent) Validate() error {
	if parsed, err := uuid.Parse(e.ID); err != nil || parsed.String() != e.ID {
		return fmt.Errorf("%w: id must be a lower-case hyphenated UUID", ErrInvalidEvent)
	}
	if !e.Type.Valid() {
		return fmt.Errorf("%w: unknown type", ErrInvalidEvent)
	}
	if e.Source == "" {
		return fmt.Errorf("%w: source is required", ErrInvalidEvent)
	}
	if e.Time.IsZero() {
		return fmt.Errorf("%w: time is required", ErrInvalidEvent)
	}
	if e.TenantProjectID < 0 {
		return fmt.Errorf("%w: tenant_project_id must not be negative", ErrInvalidEvent)
	}
	if e.Action == "" {
		return fmt.Errorf("%w: action is required", ErrInvalidEvent)
	}
	switch e.Effect {
	case EffectAllow, EffectDeny:
	case "":
		if e.Type == TypeDecision {
			return fmt.Errorf("%w: a decision requires an effect", ErrInvalidEvent)
		}
	default:
		return fmt.Errorf("%w: unknown effect", ErrInvalidEvent)
	}
	switch e.Arrangement {
	case "", ArrangementA, ArrangementB, ArrangementC:
	default:
		return fmt.Errorf("%w: unknown arrangement", ErrInvalidEvent)
	}
	switch e.ActorFidelity {
	case "", FidelityNativeAct, FidelityConfiguredAct, FidelityAZPOnly, FidelityAttributed, FidelityNone:
	default:
		return fmt.Errorf("%w: unknown actor fidelity", ErrInvalidEvent)
	}
	if e.AuthorizationEpoch < 0 || e.RevocationEpoch < 0 {
		return fmt.Errorf("%w: an epoch must not be negative", ErrInvalidEvent)
	}
	if len(e.Reasons) > maxListItems || len(e.ActorChain) > maxListItems {
		return fmt.Errorf("%w: a list exceeds %d items", ErrInvalidEvent, maxListItems)
	}
	for _, reason := range e.Reasons {
		if !reasonPattern.MatchString(reason) {
			return fmt.Errorf("%w: a reason is not a reason code", ErrInvalidEvent)
		}
	}
	for name, value := range e.stringFields() {
		if err := checkString(name, value); err != nil {
			return err
		}
	}
	for _, actor := range e.ActorChain {
		if actor == "" {
			return fmt.Errorf("%w: actor_chain has an empty item", ErrInvalidEvent)
		}
		if err := checkString("actor_chain", actor); err != nil {
			return err
		}
	}
	return nil
}

func (e SecurityEvent) stringFields() map[string]string {
	return map[string]string{
		"source":                 e.Source,
		"action":                 e.Action,
		"principal_ref":          e.PrincipalRef,
		"calling_application_id": e.CallingApplicationID,
		"authentication_ref":     e.AuthenticationRef,
		"agent_binding_id":       e.AgentBindingID,
		"authority_id":           e.AuthorityID,
		"mandate_ref":            e.MandateRef,
		"delegation_ref":         e.DelegationRef,
		"grant_ref":              e.GrantRef,
		"workload_id":            e.WorkloadID,
		"execution_id":           e.ExecutionID,
		"invocation_id":          e.InvocationID,
		"decision_id":            e.DecisionID,
		"resource_type":          e.ResourceType,
		"resource_id":            e.ResourceID,
		"policy_revision_id":     e.PolicyRevisionID,
		"correlation_id":         e.CorrelationID,
	}
}

// checkString bounds one value. It refuses control characters (jsonb cannot
// hold NUL), invalid UTF-8 (the encoder would rewrite it, so the chain would
// not verify) and values shaped like a bearer credential.
func checkString(name, value string) error {
	if len(value) > maxStringBytes {
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrInvalidEvent, name, maxStringBytes)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%w: %s is not valid UTF-8", ErrInvalidEvent, name)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: %s has a control character", ErrInvalidEvent, name)
		}
	}
	if jwtLike.MatchString(value) || strings.HasPrefix(strings.ToLower(value), "bearer ") {
		return fmt.Errorf("%w: %s looks like a credential value; record a reference", ErrInvalidEvent, name)
	}
	return nil
}

// envelope is the CloudEvents 1.0 structured-mode JSON of one event. Its
// encoding is the canonical bytes the chain hashes, so its field order and
// tags are part of the chain contract: change them only with a new
// DataSchema.
type envelope struct {
	SpecVersion     string `json:"specversion"`
	ID              string `json:"id"`
	Source          string `json:"source"`
	Type            string `json:"type"`
	Subject         string `json:"subject,omitempty"`
	Time            string `json:"time"`
	DataContentType string `json:"datacontenttype"`
	DataSchema      string `json:"dataschema"`
	// partitionkey and sequence are the CloudEvents partitioning and
	// sequence extensions.
	PartitionKey    string    `json:"partitionkey"`
	Sequence        string    `json:"sequence"`
	OCSFClassUID    int       `json:"ocsfclassuid"`
	OCSFCategoryUID int       `json:"ocsfcategoryuid"`
	Data            eventData `json:"data"`
}

type eventData struct {
	TenantProjectID      int64    `json:"tenant_project_id"`
	Action               string   `json:"action"`
	Effect               string   `json:"effect,omitempty"`
	Reasons              []string `json:"reasons"`
	PrincipalRef         string   `json:"principal_ref,omitempty"`
	ActorChain           []string `json:"actor_chain"`
	ActorFidelity        string   `json:"actor_fidelity,omitempty"`
	CallingApplicationID string   `json:"calling_application_id,omitempty"`
	AuthenticationRef    string   `json:"authentication_ref,omitempty"`
	AgentBindingID       string   `json:"agent_binding_id,omitempty"`
	AuthorityID          string   `json:"authority_id,omitempty"`
	MandateRef           string   `json:"mandate_ref,omitempty"`
	DelegationRef        string   `json:"delegation_ref,omitempty"`
	GrantRef             string   `json:"grant_ref,omitempty"`
	WorkloadID           string   `json:"workload_id,omitempty"`
	ExecutionID          string   `json:"execution_id,omitempty"`
	InvocationID         string   `json:"invocation_id,omitempty"`
	DecisionID           string   `json:"decision_id,omitempty"`
	ResourceType         string   `json:"resource_type,omitempty"`
	ResourceID           string   `json:"resource_id,omitempty"`
	PolicyRevisionID     string   `json:"policy_revision_id,omitempty"`
	AuthorizationEpoch   int64    `json:"authorization_epoch"`
	RevocationEpoch      int64    `json:"revocation_epoch"`
	Arrangement          string   `json:"arrangement,omitempty"`
	CorrelationID        string   `json:"correlation_id,omitempty"`
}

// FormatSequence renders a partition sequence as the CloudEvents `sequence`
// extension value.
func FormatSequence(partitionSeq int64) string {
	return fmt.Sprintf("%0*d", sequenceDigits, partitionSeq)
}

// Canonical returns the event's canonical CloudEvents JSON at chain position
// partitionSeq. It validates the event first.
func (e SecurityEvent) Canonical(partitionSeq int64) ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	if partitionSeq < 1 {
		return nil, fmt.Errorf("%w: partition sequence must be positive", ErrInvalidEvent)
	}
	class := ocsfClasses[e.Type]
	subject := ""
	if e.ResourceType != "" && e.ResourceID != "" {
		subject = e.ResourceType + "/" + e.ResourceID
	}
	env := envelope{
		SpecVersion:     SpecVersion,
		ID:              e.ID,
		Source:          e.Source,
		Type:            string(e.Type),
		Subject:         subject,
		Time:            e.Time.UTC().Format(time.RFC3339Nano),
		DataContentType: DataContentType,
		DataSchema:      DataSchema,
		PartitionKey:    e.PartitionKey(),
		Sequence:        FormatSequence(partitionSeq),
		OCSFClassUID:    class,
		OCSFCategoryUID: class / 1000,
		Data: eventData{
			TenantProjectID:      e.TenantProjectID,
			Action:               e.Action,
			Effect:               string(e.Effect),
			Reasons:              nonNil(e.Reasons),
			PrincipalRef:         e.PrincipalRef,
			ActorChain:           nonNil(e.ActorChain),
			ActorFidelity:        string(e.ActorFidelity),
			CallingApplicationID: e.CallingApplicationID,
			AuthenticationRef:    e.AuthenticationRef,
			AgentBindingID:       e.AgentBindingID,
			AuthorityID:          e.AuthorityID,
			MandateRef:           e.MandateRef,
			DelegationRef:        e.DelegationRef,
			GrantRef:             e.GrantRef,
			WorkloadID:           e.WorkloadID,
			ExecutionID:          e.ExecutionID,
			InvocationID:         e.InvocationID,
			DecisionID:           e.DecisionID,
			ResourceType:         e.ResourceType,
			ResourceID:           e.ResourceID,
			PolicyRevisionID:     e.PolicyRevisionID,
			AuthorizationEpoch:   e.AuthorizationEpoch,
			RevocationEpoch:      e.RevocationEpoch,
			Arrangement:          string(e.Arrangement),
			CorrelationID:        e.CorrelationID,
		},
	}
	return marshalEnvelope(env)
}

func marshalEnvelope(env envelope) ([]byte, error) {
	encoded, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("encode security audit event: %w", err)
	}
	return encoded, nil
}

// recanonicalize decodes a stored event (jsonb reorders keys and changes
// whitespace) and re-encodes it canonically. An unknown field, trailing data
// or a wrong type is an error: a tampered row must not decode into the same
// bytes by dropping what was added.
func recanonicalize(stored []byte) (envelope, []byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(stored))
	decoder.DisallowUnknownFields()
	var env envelope
	if err := decoder.Decode(&env); err != nil {
		return envelope{}, nil, errors.New("stored event does not decode as a security audit envelope")
	}
	if decoder.More() {
		return envelope{}, nil, errors.New("stored event has trailing data")
	}
	env.Data.Reasons = nonNil(env.Data.Reasons)
	env.Data.ActorChain = nonNil(env.Data.ActorChain)
	canonical, err := marshalEnvelope(env)
	if err != nil {
		return envelope{}, nil, err
	}
	return env, canonical, nil
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
