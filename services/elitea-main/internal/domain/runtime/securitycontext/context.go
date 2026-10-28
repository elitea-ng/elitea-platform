// Package securitycontext is the typed security context of one governed
// execution (ADR-0039 decision 6, issue #1115).
//
// Main builds a Context at admission, stores its deterministic bytes beside
// the execution and puts a Reference (content id + digest) on the signed
// worker command as WorkerCommandV1.security_context_ref. The worker fetches
// the bytes under its claim through RuntimeContentService.GetInput and checks
// the digest before it trusts a field.
//
// A Context holds references, epochs and digests only: never a token, a
// secret, an assertion, an email or a display name.
package securitycontext

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

const (
	// SchemaRevisionV1 is the only context revision this release writes.
	SchemaRevisionV1 = "elitea.runtime.security-context.v1"
	// GovernedProfile is the profile every context names.
	GovernedProfile = "elitea.credential-profile.governed.v1"
	// MediaType is the media type GetInput serves the context bytes with.
	MediaType = "application/x-protobuf"
	// ContentIDPrefix namespaces the context's GetInput content id. Input
	// bundle entries never use it, so the content server routes on it.
	ContentIDPrefix = "elitea.security-context/"
	// MaxActorChain bounds the RFC 8693 actor chain.
	MaxActorChain = 8
	// MaxCredentialReferences bounds the frozen credential references: one
	// per credentialed toolkit of the bound agent.
	MaxCredentialReferences = 64
	// MaxBytes bounds the marshalled context.
	MaxBytes = 16 << 10

	maxRefBytes = 128
)

var (
	// ErrInvalidContext refuses a context that does not match the schema.
	ErrInvalidContext = errors.New("execution security context is invalid")
	// ErrDigestMismatch is stored bytes that do not hash to the reference.
	ErrDigestMismatch = errors.New("execution security context digest mismatch")
)

// Caller names how the subject reached Elitea.
type Caller struct {
	// AuthenticationSource is an auth.AuthenticationSource name ("session",
	// "token", "api_key"); empty for a run no request started.
	AuthenticationSource string
	// CallingApplicationID is the native client id or API client id.
	CallingApplicationID string
}

// ActorLink is one RFC 8693 actor link.
type ActorLink struct {
	Kind string
	Ref  string
}

// Context is ExecutionSecurityContextV1 as a validated value.
type Context struct {
	PolicyRevisionID string
	PolicyDigest     runtimedomain.Digest
	TenantProjectID  int64
	// SubjectPrincipalRef is the Elitea user id; empty for an autonomous run.
	SubjectPrincipalRef string
	AgentBindingID      string
	AgentBindingEpoch   int64
	MandateID           string
	MandateEpoch        int64
	DelegationID        string
	DelegationEpoch     int64
	Caller              Caller
	// DefinitionDigest is the bound agent definition's digest; zero when the
	// run has no agent binding.
	DefinitionDigest runtimedomain.Digest
	// ActorChain is outermost last, at most MaxActorChain links.
	ActorChain []ActorLink
	IssuedAt   time.Time
	// AgentSessionRef is the opaque run-scoped agent session (#1151).
	AgentSessionRef string
	// ApplicationID and ApplicationVersionID name the version the run
	// executes (#1119); zero for a run with none.
	ApplicationID        int64
	ApplicationVersionID int64
	// CredentialReferences is the governed-credential-references entry
	// (#1119): one frozen reference per credentialed toolkit, toolkit ids
	// ascending.
	CredentialReferences []CredentialReference
}

// CredentialReference is one toolkit's credential reference, resolved and
// frozen at admission. A bound toolkit whose stored settings name no
// configuration reference is frozen at its digest alone: the broker then
// resolves its reference from the one usable grant, and refuses any other.
type CredentialReference struct {
	ToolkitID             int64
	CredentialReferenceID string
	TargetBindingID       string
	Generation            int64
	ToolkitDigest         runtimedomain.Digest
}

// Reference is the bus reference to a stored context.
type Reference struct {
	ContentID      string
	Digest         runtimedomain.Digest
	SchemaRevision string
}

// ContentID is the GetInput content id of an execution's context.
func ContentID(executionID string) string {
	return ContentIDPrefix + executionID
}

// IsContentID reports whether a GetInput content id names a context.
func IsContentID(contentID string) bool {
	return strings.HasPrefix(contentID, ContentIDPrefix)
}

// ImmutableVersion is the GetInput immutable version of context bytes: their
// digest, so a different context can never answer the same request.
func (r Reference) ImmutableVersion() string {
	return r.Digest.String()
}

// Validate checks the context against SchemaRevisionV1.
func (c Context) Validate() error {
	if !validRef(c.PolicyRevisionID, true) || c.PolicyDigest == (runtimedomain.Digest{}) {
		return fmt.Errorf("%w: a policy revision and its digest are required", ErrInvalidContext)
	}
	if c.TenantProjectID <= 0 {
		return fmt.Errorf("%w: tenant project id must be positive", ErrInvalidContext)
	}
	for _, ref := range []string{c.SubjectPrincipalRef, c.AgentBindingID, c.MandateID, c.DelegationID, c.Caller.AuthenticationSource, c.Caller.CallingApplicationID, c.AgentSessionRef} {
		if !validRef(ref, false) {
			return fmt.Errorf("%w: a reference is longer than %d bytes or not printable", ErrInvalidContext, maxRefBytes)
		}
	}
	for _, pair := range []struct {
		id    string
		epoch int64
	}{{c.AgentBindingID, c.AgentBindingEpoch}, {c.MandateID, c.MandateEpoch}, {c.DelegationID, c.DelegationEpoch}} {
		if pair.epoch < 0 || (pair.id == "" && pair.epoch != 0) {
			return fmt.Errorf("%w: an epoch without its id, or a negative epoch", ErrInvalidContext)
		}
	}
	if (c.AgentBindingID == "") != (c.DefinitionDigest == runtimedomain.Digest{}) {
		return fmt.Errorf("%w: a binding and its definition digest come together", ErrInvalidContext)
	}
	if len(c.ActorChain) > MaxActorChain {
		return fmt.Errorf("%w: the actor chain has more than %d links", ErrInvalidContext, MaxActorChain)
	}
	for _, link := range c.ActorChain {
		if !validRef(link.Kind, true) || !validRef(link.Ref, true) {
			return fmt.Errorf("%w: an actor link needs a kind and a ref", ErrInvalidContext)
		}
	}
	if c.IssuedAt.IsZero() || c.IssuedAt.UnixMilli() <= 0 {
		return fmt.Errorf("%w: issued at is required", ErrInvalidContext)
	}
	if c.ApplicationID < 0 || c.ApplicationVersionID < 0 || (c.ApplicationID == 0) != (c.ApplicationVersionID == 0) {
		return fmt.Errorf("%w: an application and its version come together", ErrInvalidContext)
	}
	if len(c.CredentialReferences) > MaxCredentialReferences {
		return fmt.Errorf("%w: more than %d credential references", ErrInvalidContext, MaxCredentialReferences)
	}
	if len(c.CredentialReferences) > 0 && c.AgentBindingID == "" {
		return fmt.Errorf("%w: credential references need an agent binding", ErrInvalidContext)
	}
	for i, ref := range c.CredentialReferences {
		if ref.ToolkitID <= 0 || (i > 0 && ref.ToolkitID <= c.CredentialReferences[i-1].ToolkitID) ||
			!validRef(ref.CredentialReferenceID, false) || !validRef(ref.TargetBindingID, false) ||
			(ref.CredentialReferenceID == "") != (ref.TargetBindingID == "") ||
			ref.Generation < 0 || ref.ToolkitDigest == (runtimedomain.Digest{}) {
			return fmt.Errorf("%w: a credential reference is malformed or out of order", ErrInvalidContext)
		}
	}
	return nil
}

// CredentialReferenceFor answers the frozen credential reference of a
// toolkit.
func CredentialReferenceFor(message *runtimev1.ExecutionSecurityContextV1, toolkitID int64) (CredentialReference, bool) {
	for _, ref := range message.GetCredentialReferences() {
		if ref.GetToolkitId() == toolkitID {
			return CredentialReferenceOf(ref)
		}
	}
	return CredentialReference{}, false
}

// CredentialReferenceOf converts one frozen reference; false when its
// toolkit digest is not a SHA-256.
func CredentialReferenceOf(ref *runtimev1.GovernedCredentialReferenceV1) (CredentialReference, bool) {
	digest := ref.GetToolkitDigest()
	if digest.GetAlgorithm() != runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256 ||
		len(digest.GetValue()) != len(runtimedomain.Digest{}) {
		return CredentialReference{}, false
	}
	out := CredentialReference{
		ToolkitID: ref.GetToolkitId(), CredentialReferenceID: ref.GetCredentialReferenceId(),
		TargetBindingID: ref.GetTargetBindingId(), Generation: ref.GetGeneration(),
	}
	copy(out.ToolkitDigest[:], digest.GetValue())
	return out, true
}

func validRef(value string, required bool) bool {
	if value == "" {
		return !required
	}
	if len(value) > maxRefBytes || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r == 0x7f {
			return false
		}
	}
	return true
}

// Proto is the wire message. Empty references stay empty fields.
func (c Context) Proto() *runtimev1.ExecutionSecurityContextV1 {
	message := &runtimev1.ExecutionSecurityContextV1{
		SchemaRevision:      SchemaRevisionV1,
		Profile:             GovernedProfile,
		PolicyRevisionId:    c.PolicyRevisionID,
		PolicyDigest:        digestProto(c.PolicyDigest),
		TenantProjectId:     c.TenantProjectID,
		SubjectPrincipalRef: c.SubjectPrincipalRef,
		AgentBindingId:      c.AgentBindingID,
		AgentBindingEpoch:   c.AgentBindingEpoch,
		MandateId:           c.MandateID,
		MandateEpoch:        c.MandateEpoch,
		DelegationId:        c.DelegationID,
		DelegationEpoch:     c.DelegationEpoch,
		Caller: &runtimev1.CallerContextV1{
			AuthenticationSource: c.Caller.AuthenticationSource,
			CallingApplicationId: c.Caller.CallingApplicationID,
		},
		IssuedAtUnixMillis:   c.IssuedAt.UTC().UnixMilli(),
		AgentSessionRef:      c.AgentSessionRef,
		ApplicationId:        c.ApplicationID,
		ApplicationVersionId: c.ApplicationVersionID,
	}
	for _, ref := range c.CredentialReferences {
		message.CredentialReferences = append(message.CredentialReferences, &runtimev1.GovernedCredentialReferenceV1{
			ToolkitId: ref.ToolkitID, CredentialReferenceId: ref.CredentialReferenceID,
			TargetBindingId: ref.TargetBindingID, Generation: ref.Generation,
			ToolkitDigest: digestProto(ref.ToolkitDigest),
		})
	}
	if c.DefinitionDigest != (runtimedomain.Digest{}) {
		message.DefinitionDigest = digestProto(c.DefinitionDigest)
	}
	for _, link := range c.ActorChain {
		message.ActorChain = append(message.ActorChain, &runtimev1.ActorLinkV1{Kind: link.Kind, Ref: link.Ref})
	}
	return message
}

// Marshal validates the context and returns its deterministic bytes with the
// reference that names them for executionID.
func (c Context) Marshal(executionID string) ([]byte, Reference, error) {
	if err := c.Validate(); err != nil {
		return nil, Reference{}, err
	}
	if !validRef(executionID, true) {
		return nil, Reference{}, fmt.Errorf("%w: execution id is invalid", ErrInvalidContext)
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(c.Proto())
	if err != nil {
		return nil, Reference{}, fmt.Errorf("encode execution security context: %w", err)
	}
	if len(encoded) > MaxBytes {
		return nil, Reference{}, fmt.Errorf("%w: encoded context exceeds %d bytes", ErrInvalidContext, MaxBytes)
	}
	return encoded, Reference{
		ContentID:      ContentID(executionID),
		Digest:         runtimedomain.SHA256(encoded),
		SchemaRevision: SchemaRevisionV1,
	}, nil
}

// ReferenceProto is field 65 of WorkerCommandV1. A zero reference is nil, so
// a legacy command carries no field and its bytes are unchanged.
func (r Reference) ReferenceProto() *runtimev1.ExecutionSecurityContextReferenceV1 {
	if r == (Reference{}) {
		return nil
	}
	return &runtimev1.ExecutionSecurityContextReferenceV1{
		ContentEntryId: r.ContentID,
		Digest:         digestProto(r.Digest),
		SchemaRevision: r.SchemaRevision,
	}
}

// ReferenceFromProto reads field 65. A nil message is the zero reference.
func ReferenceFromProto(message *runtimev1.ExecutionSecurityContextReferenceV1) (Reference, error) {
	if message == nil {
		return Reference{}, nil
	}
	digest := message.GetDigest()
	if !IsContentID(message.GetContentEntryId()) || !validRef(message.GetContentEntryId(), true) ||
		message.GetSchemaRevision() != SchemaRevisionV1 || digest == nil ||
		digest.GetAlgorithm() != runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256 ||
		len(digest.GetValue()) != len(runtimedomain.Digest{}) || len(message.ProtoReflect().GetUnknown()) != 0 {
		return Reference{}, fmt.Errorf("%w: the command's security context reference is malformed", ErrInvalidContext)
	}
	var value runtimedomain.Digest
	copy(value[:], digest.GetValue())
	return Reference{ContentID: message.GetContentEntryId(), Digest: value, SchemaRevision: message.GetSchemaRevision()}, nil
}

// Verify checks stored bytes against a reference and decodes them strictly:
// an unknown field or another schema revision is refused.
func Verify(encoded []byte, reference Reference) (*runtimev1.ExecutionSecurityContextV1, error) {
	if len(encoded) == 0 || len(encoded) > MaxBytes {
		return nil, fmt.Errorf("%w: encoded context is empty or too large", ErrInvalidContext)
	}
	if runtimedomain.SHA256(encoded) != reference.Digest {
		return nil, ErrDigestMismatch
	}
	var message runtimev1.ExecutionSecurityContextV1
	if err := proto.Unmarshal(encoded, &message); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidContext, err)
	}
	if hasUnknown(message.ProtoReflect()) {
		return nil, fmt.Errorf("%w: the context carries an unknown field", ErrInvalidContext)
	}
	if message.GetSchemaRevision() != SchemaRevisionV1 || message.GetProfile() != GovernedProfile {
		return nil, fmt.Errorf("%w: unsupported schema revision or profile", ErrInvalidContext)
	}
	return &message, nil
}

func digestProto(digest runtimedomain.Digest) *runtimev1.DigestV1 {
	return &runtimev1.DigestV1{
		Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256,
		Value:     append([]byte(nil), digest[:]...),
	}
}

// hasUnknown reports an unknown field anywhere in the message tree.
func hasUnknown(message protoreflect.Message) bool {
	if len(message.GetUnknown()) != 0 {
		return true
	}
	found := false
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		switch {
		case field.IsList() && field.Message() != nil:
			list := value.List()
			for i := 0; i < list.Len() && !found; i++ {
				found = hasUnknown(list.Get(i).Message())
			}
		case field.Message() != nil && !field.IsMap():
			found = hasUnknown(value.Message())
		}
		return !found
	})
	return found
}
