package profile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
)

// PolicySchemaV1 is the only policy document schema this release reads.
// Later issues add fields with a schema bump; unknown fields are refused.
const PolicySchemaV1 = "elitea.credential-policy.v1"

const (
	maxPolicyDocumentBytes  = 64 * 1024
	maxPolicyListItems      = 64
	maxPolicyStringBytes    = 128
	maxMandateTTLUpperBound = 366 * 24 * time.Hour
)

var (
	// ErrInvalidPolicy refuses a document that does not match the schema.
	ErrInvalidPolicy = errors.New("credential policy document is invalid")
	// ErrRevisionNotFound is a revision id with no stored row.
	ErrRevisionNotFound = errors.New("credential policy revision not found")
	// ErrRevisionExists refuses a second write of one revision id.
	ErrRevisionExists = errors.New("credential policy revision already exists")
	// ErrDigestMismatch is a stored revision whose bytes no longer hash to
	// its recorded digest.
	ErrDigestMismatch = errors.New("credential policy revision digest mismatch")
	// ErrPinNotFound means the execution has no pin row: it is legacy.
	ErrPinNotFound = errors.New("execution admission profile pin not found")
)

// PolicyDocument holds parameters only. It never holds a secret, a token or
// a principal: every authority decision reads its own records, and this
// document only bounds them.
type PolicyDocument struct {
	Schema string `json:"schema"`
	// AllowedIdentityModes lists the IdentityMode values bindings may use
	// (gate D2). Boot refuses an active revision whose list is empty.
	AllowedIdentityModes []agentidentity.IdentityMode `json:"allowed_identity_modes"`
	// VaultRequiredCredentialClasses lists the credential classes that must
	// live in an external Vault/OpenBao store (gate D1). Empty: none.
	VaultRequiredCredentialClasses []string `json:"vault_required_credential_classes"`
	// MaxMandateTTLSeconds bounds a Mandate's lifetime.
	MaxMandateTTLSeconds int64 `json:"max_mandate_ttl_seconds"`
	// AuditBacklogAlertThreshold is the unexported security audit row count
	// that raises the backlog alert (gate D7). Rows are never dropped.
	AuditBacklogAlertThreshold int64 `json:"audit_backlog_alert_threshold"`
}

// MaxMandateTTL is MaxMandateTTLSeconds as a duration.
func (d PolicyDocument) MaxMandateTTL() time.Duration {
	return time.Duration(d.MaxMandateTTLSeconds) * time.Second
}

// AllowsIdentityMode reports whether mode is in the allow-list.
func (d PolicyDocument) AllowsIdentityMode(mode agentidentity.IdentityMode) bool {
	return slices.Contains(d.AllowedIdentityModes, mode)
}

// RequiresVault reports whether credentialClass must live in Vault/OpenBao.
func (d PolicyDocument) RequiresVault(credentialClass string) bool {
	return slices.Contains(d.VaultRequiredCredentialClasses, credentialClass)
}

// Validate checks the document against PolicySchemaV1.
func (d PolicyDocument) Validate() error {
	if d.Schema != PolicySchemaV1 {
		return fmt.Errorf("%w: schema must be %q", ErrInvalidPolicy, PolicySchemaV1)
	}
	if len(d.AllowedIdentityModes) > maxPolicyListItems || len(d.VaultRequiredCredentialClasses) > maxPolicyListItems {
		return fmt.Errorf("%w: a list exceeds %d items", ErrInvalidPolicy, maxPolicyListItems)
	}
	for _, mode := range d.AllowedIdentityModes {
		if !mode.Valid() {
			return fmt.Errorf("%w: unknown identity mode %q", ErrInvalidPolicy, mode)
		}
	}
	if hasDuplicates(d.AllowedIdentityModes) || hasDuplicates(d.VaultRequiredCredentialClasses) {
		return fmt.Errorf("%w: a list repeats a value", ErrInvalidPolicy)
	}
	for _, class := range d.VaultRequiredCredentialClasses {
		if class == "" || len(class) > maxPolicyStringBytes {
			return fmt.Errorf("%w: credential class must be 1..%d bytes", ErrInvalidPolicy, maxPolicyStringBytes)
		}
	}
	if d.MaxMandateTTLSeconds <= 0 || d.MaxMandateTTL() > maxMandateTTLUpperBound {
		return fmt.Errorf("%w: max_mandate_ttl_seconds must be in 1..%d", ErrInvalidPolicy, int64(maxMandateTTLUpperBound/time.Second))
	}
	if d.AuditBacklogAlertThreshold <= 0 {
		return fmt.Errorf("%w: audit_backlog_alert_threshold must be positive", ErrInvalidPolicy)
	}
	return nil
}

func hasDuplicates[T comparable](values []T) bool {
	seen := make(map[T]struct{}, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			return true
		}
		seen[value] = struct{}{}
	}
	return false
}

// ParsePolicyDocument decodes raw strictly (unknown fields refused), validates
// it and returns the document with its canonical bytes.
func ParsePolicyDocument(raw []byte) (PolicyDocument, []byte, error) {
	if len(raw) == 0 || len(raw) > maxPolicyDocumentBytes {
		return PolicyDocument{}, nil, fmt.Errorf("%w: document must be 1..%d bytes", ErrInvalidPolicy, maxPolicyDocumentBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var doc PolicyDocument
	if err := decoder.Decode(&doc); err != nil {
		return PolicyDocument{}, nil, fmt.Errorf("%w: %s", ErrInvalidPolicy, safeDecodeReason(err))
	}
	if decoder.More() {
		return PolicyDocument{}, nil, fmt.Errorf("%w: trailing data after the document", ErrInvalidPolicy)
	}
	if err := doc.Validate(); err != nil {
		return PolicyDocument{}, nil, err
	}
	canonical, err := doc.Canonical()
	if err != nil {
		return PolicyDocument{}, nil, err
	}
	return doc, canonical, nil
}

// safeDecodeReason keeps the decoder's structural reason without echoing
// document content back into logs.
func safeDecodeReason(err error) string {
	var syntax *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntax):
		return fmt.Sprintf("malformed JSON at offset %d", syntax.Offset)
	case errors.As(err, &typeErr):
		return fmt.Sprintf("field %q has the wrong type", typeErr.Field)
	}
	return "unknown field or malformed document"
}

// Canonical returns the document's canonical JSON: struct field order, lists
// sorted, nil lists as []. Equal parameters give equal bytes and digests.
func (d PolicyDocument) Canonical() ([]byte, error) {
	modes := slices.Clone(d.AllowedIdentityModes)
	if modes == nil {
		modes = []agentidentity.IdentityMode{}
	}
	slices.Sort(modes)
	classes := slices.Clone(d.VaultRequiredCredentialClasses)
	if classes == nil {
		classes = []string{}
	}
	slices.Sort(classes)
	d.AllowedIdentityModes, d.VaultRequiredCredentialClasses = modes, classes
	encoded, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("encode credential policy: %w", err)
	}
	return encoded, nil
}

// Digest is sha256 over canonical bytes.
func Digest(canonical []byte) [32]byte {
	return sha256.Sum256(canonical)
}

// PolicyRevision is one immutable stored revision.
type PolicyRevision struct {
	ID        string
	Digest    [32]byte
	Document  PolicyDocument
	Canonical []byte
	CreatedAt time.Time
	// CreatedBy is the operator identity that ran the CLI, not a user id.
	CreatedBy string
}

// VerifyDigest re-hashes the stored canonical bytes.
func (r PolicyRevision) VerifyDigest() error {
	if Digest(r.Canonical) != r.Digest {
		return fmt.Errorf("%w: revision %q", ErrDigestMismatch, r.ID)
	}
	return nil
}

// Pin is the profile and revision one execution was admitted under.
type Pin struct {
	ExecutionID      string
	Profile          Profile
	PolicyRevisionID string
	PolicyDigest     [32]byte
	PinnedAt         time.Time
}

// RevisionStore reads and appends immutable revisions.
type RevisionStore interface {
	Revision(ctx context.Context, id string) (PolicyRevision, error)
	// PutRevision appends a new revision. An existing id is ErrRevisionExists;
	// a revision is never edited.
	PutRevision(ctx context.Context, revision PolicyRevision) error
}

// PinReader answers "which profile and revision admitted this execution?".
// An execution with no pin row is legacy: ReadPin returns a legacy Pin.
type PinReader interface {
	ReadPin(ctx context.Context, executionID string) (Pin, error)
}

// Prerequisite is a governed dependency (secret store, audit outbox, signing
// key) that boot and revision publishing validate against a revision. It is
// built at the composition root, only with the flag on.
type Prerequisite interface {
	Name() string
	Check(ctx context.Context, revision PolicyRevision) error
}
