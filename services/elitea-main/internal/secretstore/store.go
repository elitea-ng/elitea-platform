// Package secretstore is the one seam governed credential reads and writes go
// through (ADR-0011 amendment "governed credential use", ADR-0039, #1113).
//
// A SecretStore is storage only. It never decides whether an execution may use
// a credential: the Elitea broker decides, and hands the store a StoreContext
// that names the decision. Two adapters exist:
//
//   - pgvault: the encrypted PostgreSQL vault in elitea-main, in its
//     legacy-schema-compatible representation (centry.secrets_key /
//     centry.secrets_data). It stays the default backend.
//   - vault: HashiCorp Vault or OpenBao KV v2.
//
// There is no fallback between backends. A failed read from one backend is
// never retried on another, and an adapter refuses a SecretRef that names a
// different backend.
//
// Only the broker receives SecretStore values, as a map keyed by backend id
// built once at the composition root. There is no global registry.
package secretstore

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Backend ids. A SecretRef names exactly one of them.
const (
	BackendPGVault = "pgvault"
	BackendVault   = "vault"
)

// Typed failures. Every adapter error wraps exactly one of these. The wrapped
// text names the operation and a reason class, never a path, a mount or a
// value.
var (
	ErrNotFound        = errors.New("secretstore: not found")
	ErrVersionConflict = errors.New("secretstore: version conflict")
	ErrUnavailable     = errors.New("secretstore: backend unavailable")
	ErrDenied          = errors.New("secretstore: backend denied")
	ErrMisconfigured   = errors.New("secretstore: misconfigured")
	// ErrInvalidRequest is a call the store refuses before contacting the
	// backend: a missing StoreContext field, version 0 on a read, a malformed
	// SecretRef.
	ErrInvalidRequest = errors.New("secretstore: invalid request")
	// ErrUnexpectedVersion is a write the backend accepted but that created
	// a version other than casVersion+1. It is not a refusal: a version
	// exists. Write returns that version alongside the error when the
	// backend reported one, so the ledger can retire it.
	ErrUnexpectedVersion = errors.New("secretstore: backend created an unexpected version")
)

// StoreContext is what the broker passes with every call. Every field comes
// from authoritative server state; none is user-supplied.
type StoreContext struct {
	// TenantProjectID is the project that owns the credential reference.
	TenantProjectID int64
	// CredentialRefID is the CredentialReference the call serves.
	CredentialRefID string
	// DecisionID is the authorization decision that allowed this use. A read
	// without one is refused.
	DecisionID string
	// WorkloadScope selects the backend role (Vault: one role per scope,
	// arrangement C). Never user-supplied.
	WorkloadScope string
	// PolicyRevisionID is the credential policy revision the calling run is
	// pinned to.
	PolicyRevisionID string
}

// Op names a SecretStore operation for validation and error text.
type Op string

const (
	OpRead     Op = "read"
	OpWrite    Op = "write"
	OpMetadata Op = "metadata"
	OpRetire   Op = "retire"
)

var (
	workloadScopePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	boundedIDPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
)

// ValidWorkloadScope reports whether scope is an acceptable workload scope.
// The pattern keeps a scope usable as a Vault role-name suffix.
func ValidWorkloadScope(scope string) bool { return workloadScopePattern.MatchString(scope) }

// Validate refuses a StoreContext that is missing a field op needs. Every
// governed call carries a policy revision; a read also carries the decision
// that allowed it. Retire is reachable from the ledger sweeper, which has no
// decision, so it needs none.
func (sc StoreContext) Validate(op Op) error {
	switch {
	case sc.TenantProjectID <= 0:
		return Errorf(ErrInvalidRequest, op, "tenant project is required")
	case !boundedIDPattern.MatchString(sc.CredentialRefID):
		return Errorf(ErrInvalidRequest, op, "credential reference id is required")
	case !ValidWorkloadScope(sc.WorkloadScope):
		return Errorf(ErrInvalidRequest, op, "workload scope is required")
	case !boundedIDPattern.MatchString(sc.PolicyRevisionID):
		return Errorf(ErrInvalidRequest, op, "policy revision is required")
	case op == OpRead && !boundedIDPattern.MatchString(sc.DecisionID):
		return Errorf(ErrInvalidRequest, op, "a read requires the decision id that allowed it")
	}
	return nil
}

// SecretRef is the backend-specific locator of one secret. It stays inside
// elitea-main: worker messages carry a CredentialReference id, never a
// SecretRef.
//
// pgvault: Mount is empty and Path is the secret name in the project vault
// that StoreContext.TenantProjectID selects. vault: Mount is the KV v2 mount
// and Path the secret path under it.
type SecretRef struct {
	Backend string
	Mount   string
	Path    string
}

// SecretVersion is one explicit version read from a store.
type SecretVersion struct {
	Version   int64
	Value     RuntimeSecret // caller must Destroy after use
	CreatedAt time.Time
}

// Metadata describes the versions a store holds for one SecretRef. It never
// carries custom metadata: nothing a backend operator can annotate is read
// into any decision.
type Metadata struct {
	CurrentVersion int64
	// Versions lists the readable versions, ascending.
	Versions []int64
	// CASRequired reports whether the backend enforces check-and-set writes
	// for this SecretRef.
	CASRequired bool
}

// SecretStore is the ADR-0011 amendment contract.
//
// Versions are secret versions, never the execution claim generation. Read
// takes an explicit version; there is no "latest" read. Write is
// check-and-set: casVersion is the version the caller expects to be current
// (0: the secret must not exist yet), and the returned version is the one the
// write created, casVersion+1. A Write that fails with ErrUnexpectedVersion
// returns the version it did create (0 when the backend did not say).
type SecretStore interface {
	Read(ctx context.Context, sc StoreContext, ref SecretRef, version int64) (SecretVersion, error)
	Write(ctx context.Context, sc StoreContext, ref SecretRef, value RuntimeSecret, casVersion int64) (int64, error)
	Metadata(ctx context.Context, sc StoreContext, ref SecretRef) (Metadata, error)
	Retire(ctx context.Context, sc StoreContext, ref SecretRef, version int64) error
}

// VersionScopedRetirer is implemented by a store whose Retire removes
// exactly one version and whose versions are each created by exactly one
// check-and-set write, so a retire of the version a staged write intended
// can only remove what that write (or another uncommitted write of the same
// version) created. The ledger sweeper retires only through such a store.
// A store that does not implement it (pgvault, whose Retire removes a name
// whoever wrote it) is never retired by the sweeper.
type VersionScopedRetirer interface {
	RetiresOnlyTheWrittenVersion()
}

// ValidateReadVersion refuses a read without an explicit version.
func ValidateReadVersion(version int64) error {
	if version <= 0 {
		return Errorf(ErrInvalidRequest, OpRead, "an explicit secret version is required")
	}
	return nil
}

// ValidateBackend refuses a SecretRef addressed to another backend. There is
// no fallback: the caller chose a backend and gets exactly that one.
func ValidateBackend(op Op, ref SecretRef, backend string) error {
	if ref.Backend != backend {
		return Errorf(ErrMisconfigured, op, "the reference names another backend")
	}
	return nil
}

// Errorf wraps kind with an operation and a fixed reason. reason must be a
// constant phrase: it is never built from a path, a mount or a value.
func Errorf(kind error, op Op, reason string) error {
	return fmt.Errorf("%w: %s: %s", kind, op, reason)
}
