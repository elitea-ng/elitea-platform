// Package credentialbroker is the CredentialBroker of ADR-0039 decision 10
// (issue #1118). A governed worker redeems the material for ONE bound tool
// invocation through RuntimeContentService.RedeemCredential. The content
// server proves the claim binding (verified peer, claim id, fence, live
// workload session, RUNNING job) and hands the broker the claimed run; the
// broker then, in order:
//
//  1. requires the run's pin to be governed (NOT_GOVERNED);
//  2. requires the request's security context digest to equal the stored
//     context's digest (CONTEXT_MISMATCH);
//  3. records the bound tool invocation, unique per (execution, generation,
//     invocation id); a replay with any field changed is INVOCATION_MISMATCH;
//  4. builds the operation request from the stored context and the
//     invocation and asks the AND-composed decider (#1116);
//  5. appends the decision to the security audit outbox in the SAME
//     transaction as the invocation row; an append failure is a deny with no
//     material;
//  6. on a permit, opens (first redemption) or reuses the run-scoped agent
//     session, then acquires the material by credential class: a static
//     secret is read from its SecretStore at the reference's generation, an
//     oauth_delegated credential is exchanged on behalf of the user; every
//     other class is CLASS_NOT_SUPPORTED;
//  7. keeps only the material keys the operation catalogue names for the
//     provider and class, records the redemption (never the material) with
//     its audit event, and returns the envelope.
//
// The broker never logs material, never stores it, and zeroes the byte
// slices it holds once the envelope is serialized.
package credentialbroker

import (
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
)

// DenialCode is the safe, stable code of a refusal
// (CredentialRedeemDenialV1.code).
type DenialCode string

// Denial codes. Add a code; never rename one.
const (
	CodeDenied                DenialCode = "DENIED"
	CodeInvocationMismatch    DenialCode = "INVOCATION_MISMATCH"
	CodeClassNotSupported     DenialCode = "CLASS_NOT_SUPPORTED"
	CodeBackendUnavailable    DenialCode = "BACKEND_UNAVAILABLE"
	CodeAuthorizationRequired DenialCode = "AUTHORIZATION_REQUIRED"
	CodeContextMismatch       DenialCode = "CONTEXT_MISMATCH"
	CodeNotGoverned           DenialCode = "NOT_GOVERNED"
)

// EnvelopeKind is the kind of released material.
type EnvelopeKind string

const (
	KindStatic       EnvelopeKind = "static"
	KindOAuthAccess  EnvelopeKind = "oauth_access"
	KindDynamicLease EnvelopeKind = "dynamic_lease"
)

// ErrInvalidRequest is a malformed redemption request. The content server
// answers InvalidArgument.
var ErrInvalidRequest = errors.New("credential broker: invalid redemption request")

// ErrInvocationMismatch is a replayed invocation id with another field.
var ErrInvocationMismatch = errors.New("credential broker: the invocation id was used with other fields")

// ErrReferenceUnresolved is a Store answer: the run has no single usable
// grant to name the credential reference of an invocation that sent none.
var ErrReferenceUnresolved = errors.New("credential broker: no single grant names the credential reference")

// ErrAuditFailed wraps a security audit append failure. The broker denies.
var ErrAuditFailed = errors.New("credential broker: security audit append failed")

// ClaimedRun is what the content server's claim binding proved about the
// caller: the claimed execution, the verified workload and the stored
// security context. The request can choose none of it.
type ClaimedRun struct {
	ExecutionID string
	Generation  uint64
	ClaimID     string
	// WorkloadID is the SPIFFE id of the verified peer certificate.
	WorkloadID      string
	TenantProjectID int64
	// ActorID is execution_jobs.actor_id.
	ActorID string
	// ConversationUUID and ResponseMessageID name the chat turn of an agent
	// run; empty for any other run. An authorization requirement needs both.
	ConversationUUID  string
	ResponseMessageID string
	// ContextBytes are the stored ExecutionSecurityContextV1 bytes; nil when
	// the run has none.
	ContextBytes  []byte
	ContextDigest [32]byte
}

// BoundToolInvocation mirrors BoundToolInvocationV1.
type BoundToolInvocation struct {
	InvocationID          string
	ToolCallID            string
	ToolkitID             int64
	OperationID           string
	ResourceRef           string
	ArgumentsDigest       [32]byte
	CredentialReferenceID string
}

const (
	maxToolCallIDBytes  = 256
	maxResourceRefBytes = 512
)

func (i BoundToolInvocation) validate() error {
	id, err := uuid.Parse(i.InvocationID)
	if err != nil || id.String() != i.InvocationID {
		return ErrInvalidRequest
	}
	// Empty asks Main to resolve the reference from the run's grants.
	if i.CredentialReferenceID != "" {
		if _, err := uuid.Parse(i.CredentialReferenceID); err != nil {
			return ErrInvalidRequest
		}
	}
	switch {
	case i.ToolCallID == "", len(i.ToolCallID) > maxToolCallIDBytes, !printable(i.ToolCallID),
		i.ToolkitID <= 0,
		!credentialgrant.OperationID(i.OperationID).WellFormed(),
		len(i.ResourceRef) > maxResourceRefBytes, !printable(i.ResourceRef),
		i.ArgumentsDigest == [32]byte{}:
		return ErrInvalidRequest
	}
	return nil
}

func printable(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// RedeemRequest is one redemption.
type RedeemRequest struct {
	Run        ClaimedRun
	Invocation BoundToolInvocation
	// SecurityContextDigest is the digest the worker's signed command
	// carries (WorkerCommandV1 field 65).
	SecurityContextDigest [32]byte
}

// Envelope is the released material for one invocation.
type Envelope struct {
	RedemptionID string
	Kind         EnvelopeKind
	// Material holds the catalogue's keys only. Destroy zeroes it.
	Material       map[string][]byte
	NotAfter       time.Time
	TargetIdentity string
	Obligations    []string
	DecisionID     string
}

// Destroy zeroes every material value. The caller calls it once the
// envelope is serialized.
func (e *Envelope) Destroy() {
	if e == nil {
		return
	}
	for key, value := range e.Material {
		clear(value)
		delete(e.Material, key)
	}
}

// String keeps material out of logs.
func (e *Envelope) String() string { return "credentialbroker.Envelope{material=[redacted]}" }

// GoString keeps material out of %#v.
func (e *Envelope) GoString() string { return e.String() }

// Denial is a refusal with a safe code.
type Denial struct {
	Code       DenialCode
	DecisionID string
	// AuthorizationReference is the opaque reference of an
	// AUTHORIZATION_REQUIRED denial (#1117).
	AuthorizationReference string
	// reason is the metric label: a bounded, stable code.
	reason string
}

// Reason is the bounded reason the metric records.
func (d *Denial) Reason() string { return d.reason }

// Result is exactly one of Envelope and Denial.
type Result struct {
	Envelope *Envelope
	Denial   *Denial
}

func deny(code DenialCode, reason, decisionID string) Result {
	return Result{Denial: &Denial{Code: code, DecisionID: decisionID, reason: reason}}
}
