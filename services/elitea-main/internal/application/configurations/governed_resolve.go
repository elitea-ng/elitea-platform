package configurations

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
)

// CredentialReferenceIDField is the optional key of a stored configuration
// reference ({"elitea_title", "private", "credential_reference_id"}) that
// names its governed CredentialReference exactly (#1119). Legacy readers
// ignore it: the toolkit settings walker strips it before the legacy
// expansion, whose output is therefore unchanged. Only the governed admin
// API writes it.
const CredentialReferenceIDField = "credential_reference_id"

// Typed governed resolution errors (#1119). Each message is safe to return
// to a caller: none names a title, a project or a credential.
var (
	// ErrGovernedReferenceRequired refuses a title-only reference: a title
	// never selects a governed credential.
	ErrGovernedReferenceRequired = errors.New("governed credential resolution: an exact credential reference is required")
	// ErrGovernedReferenceInvalid refuses a reference whose id is not a uuid.
	ErrGovernedReferenceInvalid = errors.New("governed credential resolution: the credential reference id is malformed")
	// ErrGovernedReferenceAmbiguous refuses a toolkit that names more than
	// one credential reference: the worker redeems one per toolkit.
	ErrGovernedReferenceAmbiguous = errors.New("governed credential resolution: the toolkit names more than one credential reference")
	// ErrGovernedReferenceNotFound is a reference with no row in the run's
	// tenant project. Nothing is looked up anywhere else.
	ErrGovernedReferenceNotFound = errors.New("governed credential resolution: the credential reference is not in the run's tenant project")
	// ErrGovernedReferenceNotActive is a reference that is not `active`.
	ErrGovernedReferenceNotActive = errors.New("governed credential resolution: the credential reference is not active")
	// ErrGovernedToolkitNotBound is a toolkit with no AgentToolkitBinding
	// for the run's agent binding.
	ErrGovernedToolkitNotBound = errors.New("governed credential resolution: the toolkit is not bound to the agent")
	// ErrGovernedToolkitDigestMismatch is a bound toolkit whose digest is not
	// the digest of the toolkit the run froze.
	ErrGovernedToolkitDigestMismatch = errors.New("governed credential resolution: the toolkit digest differs from the bound digest")
	// ErrGovernedGrantMissing is no active, unexpired `use` grant for the
	// agent binding, toolkit, reference and target.
	ErrGovernedGrantMissing = errors.New("governed credential resolution: no active use grant for the credential")
	// ErrPrivateCredentialNotGoverned refuses a `private` reference in the
	// governed profile (customer gate D1, default): an isolated personal
	// credential is not a governed credential.
	ErrPrivateCredentialNotGoverned = errors.New("governed credential resolution: a private credential is not governed")
	// ErrLegacyResolverInGovernedRun is the legacy title expansion called
	// for a governed run (defence in depth; see WithGovernedRun).
	ErrLegacyResolverInGovernedRun = errors.New("legacy configuration expansion called for a governed run")
)

// GovernedResolutionCode is the safe admission code of a typed error.
func GovernedResolutionCode(err error) (string, bool) {
	for _, known := range []struct {
		err  error
		code string
	}{
		{ErrGovernedReferenceRequired, "credential_reference_required"},
		{ErrGovernedReferenceInvalid, "credential_reference_invalid"},
		{ErrGovernedReferenceAmbiguous, "credential_reference_ambiguous"},
		{ErrGovernedReferenceNotFound, "credential_reference_not_found"},
		{ErrGovernedReferenceNotActive, "credential_reference_not_active"},
		{ErrGovernedToolkitNotBound, "toolkit_not_bound"},
		{ErrGovernedToolkitDigestMismatch, "toolkit_digest_mismatch"},
		{ErrGovernedGrantMissing, "credential_grant_missing"},
		{ErrPrivateCredentialNotGoverned, "private_credential_not_governed"},
		{ErrLegacyResolverInGovernedRun, "legacy_resolver_in_governed_run"},
	} {
		if errors.Is(err, known.err) {
			return known.code, true
		}
	}
	return "", false
}

// ToolkitCredentialRef is one stored configuration reference of a toolkit.
type ToolkitCredentialRef struct {
	// CredentialReferenceID is the exact governed reference; empty for a
	// title-only (legacy) reference.
	CredentialReferenceID string
	// Title and Private are the legacy fields. The governed path never
	// resolves by Title; Private is refused (D1).
	Title   string
	Private bool
}

// ToolkitCredentialRefs reads the configuration references of a toolkit's
// STORED settings: every top-level object that carries `elitea_title` and
// `private`, in key order. A field holding anything else is not a
// reference.
func ToolkitCredentialRefs(settings map[string]any) []ToolkitCredentialRef {
	fields := ToolkitCredentialRefFields(settings)
	if len(fields) == 0 {
		return nil
	}
	refs := make([]ToolkitCredentialRef, 0, len(fields))
	for _, name := range fields {
		object := settings[name].(map[string]any)
		ref := ToolkitCredentialRef{}
		ref.Title, _ = object["elitea_title"].(string)
		ref.Private, _ = object["private"].(bool)
		if id, ok := object[CredentialReferenceIDField].(string); ok {
			ref.CredentialReferenceID = id
		}
		refs = append(refs, ref)
	}
	return refs
}

// ToolkitCredentialRefFields names the settings fields ToolkitCredentialRefs
// reads, in the same order: the one definition of the shape, so a writer of
// a configuration reference (the admin attach) and the governed resolver can
// never disagree on which field it is.
func ToolkitCredentialRefFields(settings map[string]any) []string {
	names := make([]string, 0, len(settings))
	for name := range settings {
		names = append(names, name)
	}
	sort.Strings(names)
	var fields []string
	for _, name := range names {
		object, ok := settings[name].(map[string]any)
		if !ok {
			continue
		}
		_, hasTitle := object["elitea_title"]
		_, hasPrivate := object["private"]
		if !hasTitle || !hasPrivate {
			continue
		}
		fields = append(fields, name)
	}
	return fields
}

// GovernedResolution is a toolkit's resolved credential reference.
type GovernedResolution struct {
	ToolkitID             int64
	CredentialReferenceID string
	TargetBindingID       string
	Generation            int64
}

// GovernedResolver resolves a governed run's toolkit credential by its exact
// reference id, in the run's tenant project only. toolkitDigest is the
// digest of the toolkit the run froze (credentialgrant.ToolkitDigest).
type GovernedResolver interface {
	Resolve(ctx context.Context, tenantProjectID int64, agentBindingID string, toolkitID int64,
		toolkitDigest [32]byte, ref ToolkitCredentialRef) (GovernedResolution, error)
	// BindToolkit checks a toolkit whose stored settings name no
	// configuration reference: bound=false when the agent binding has no
	// AgentToolkitBinding for it (it is not governed), and
	// ErrGovernedToolkitDigestMismatch when the bound digest differs.
	BindToolkit(ctx context.Context, tenantProjectID int64, agentBindingID string, toolkitID int64,
		toolkitDigest [32]byte) (bound bool, err error)
}

// GovernedReferenceResolver is the GovernedResolver over the credential
// grant readers. It is built only with credential governance on, and reads
// no configuration row: no title, personal-project or public-project lookup
// exists on this path.
type GovernedReferenceResolver struct {
	references credentialgrant.ReferenceReader
	toolkits   credentialgrant.ToolkitBindingReader
	grants     credentialgrant.GrantReader
	now        func() time.Time
}

// NewGovernedReferenceResolver builds the resolver. now may be nil.
func NewGovernedReferenceResolver(references credentialgrant.ReferenceReader, toolkits credentialgrant.ToolkitBindingReader,
	grants credentialgrant.GrantReader, now func() time.Time) (*GovernedReferenceResolver, error) {
	if references == nil || toolkits == nil || grants == nil {
		return nil, errors.New("governed credential resolver: reference, toolkit binding and grant readers are required")
	}
	if now == nil {
		now = time.Now
	}
	return &GovernedReferenceResolver{references: references, toolkits: toolkits, grants: grants, now: now}, nil
}

// Resolve applies the rules of #1119 in order; the first that fails is the
// typed error.
//
//  1. The reference names an exact id (else ErrGovernedReferenceRequired)
//     and is not `private` (D1).
//  2. The reference exists in the run's tenant project and is `active`.
//  3. The toolkit is bound to the agent binding at the frozen digest.
//  4. An active, unexpired grant for (agent, toolkit, reference, target)
//     lists `use`.
func (r *GovernedReferenceResolver) Resolve(ctx context.Context, tenantProjectID int64, agentBindingID string,
	toolkitID int64, toolkitDigest [32]byte, ref ToolkitCredentialRef) (GovernedResolution, error) {
	if tenantProjectID <= 0 || agentBindingID == "" || toolkitID <= 0 {
		return GovernedResolution{}, errors.New("governed credential resolver: tenant, agent binding and toolkit are required")
	}
	if ref.Private {
		return GovernedResolution{}, ErrPrivateCredentialNotGoverned
	}
	if ref.CredentialReferenceID == "" {
		return GovernedResolution{}, ErrGovernedReferenceRequired
	}
	if _, err := uuid.Parse(ref.CredentialReferenceID); err != nil || len(ref.CredentialReferenceID) != 36 {
		return GovernedResolution{}, ErrGovernedReferenceInvalid
	}
	reference, err := r.references.CredentialReference(ctx, tenantProjectID, ref.CredentialReferenceID)
	switch {
	case errors.Is(err, credentialgrant.ErrReferenceNotFound):
		return GovernedResolution{}, ErrGovernedReferenceNotFound
	case err != nil:
		return GovernedResolution{}, fmt.Errorf("governed credential resolver: read the credential reference: %w", err)
	case reference.TenantProjectID != tenantProjectID || reference.ID != ref.CredentialReferenceID:
		// The reader filters on both; a row that does not repeat them is
		// refused rather than trusted.
		return GovernedResolution{}, ErrGovernedReferenceNotFound
	case reference.State != credentialgrant.ReferenceActive:
		return GovernedResolution{}, ErrGovernedReferenceNotActive
	}
	if err := r.boundAt(ctx, tenantProjectID, agentBindingID, toolkitID, toolkitDigest); err != nil {
		return GovernedResolution{}, err
	}
	key := credentialgrant.GrantKey{
		TenantProjectID: tenantProjectID, AgentBindingID: agentBindingID, ToolkitID: toolkitID,
		CredentialReferenceID: reference.ID, TargetBindingID: reference.TargetBindingID,
	}
	grant, err := r.grants.GrantForKey(ctx, key)
	switch {
	case errors.Is(err, credentialgrant.ErrGrantNotFound):
		return GovernedResolution{}, ErrGovernedGrantMissing
	case err != nil:
		return GovernedResolution{}, fmt.Errorf("governed credential resolver: read the grant: %w", err)
	case grant.TenantProjectID != key.TenantProjectID || grant.AgentBindingID != key.AgentBindingID ||
		grant.ToolkitID != key.ToolkitID || grant.CredentialReferenceID != key.CredentialReferenceID ||
		grant.TargetBindingID != key.TargetBindingID ||
		!grant.ActiveAt(r.now()) || !grant.Allows(credentialgrant.CapUse):
		return GovernedResolution{}, ErrGovernedGrantMissing
	}
	return GovernedResolution{
		ToolkitID: toolkitID, CredentialReferenceID: reference.ID,
		TargetBindingID: reference.TargetBindingID, Generation: reference.Generation,
	}, nil
}

// BindToolkit implements GovernedResolver.
func (r *GovernedReferenceResolver) BindToolkit(ctx context.Context, tenantProjectID int64, agentBindingID string,
	toolkitID int64, toolkitDigest [32]byte) (bool, error) {
	if tenantProjectID <= 0 || agentBindingID == "" || toolkitID <= 0 {
		return false, errors.New("governed credential resolver: tenant, agent binding and toolkit are required")
	}
	err := r.boundAt(ctx, tenantProjectID, agentBindingID, toolkitID, toolkitDigest)
	if errors.Is(err, ErrGovernedToolkitNotBound) {
		return false, nil
	}
	return err == nil, err
}

// boundAt is rule 3: the toolkit is bound to the agent binding, in the
// tenant project, at the frozen digest.
func (r *GovernedReferenceResolver) boundAt(ctx context.Context, tenantProjectID int64, agentBindingID string,
	toolkitID int64, toolkitDigest [32]byte) error {
	binding, err := r.toolkits.AgentToolkitBinding(ctx, agentBindingID, toolkitID)
	switch {
	case errors.Is(err, credentialgrant.ErrToolkitNotBound):
		return ErrGovernedToolkitNotBound
	case err != nil:
		return fmt.Errorf("governed credential resolver: read the toolkit binding: %w", err)
	case binding.TenantProjectID != tenantProjectID || binding.AgentBindingID != agentBindingID || binding.ToolkitID != toolkitID:
		return ErrGovernedToolkitNotBound
	case toolkitDigest == [32]byte{} || binding.ToolkitDigest != toolkitDigest:
		return ErrGovernedToolkitDigestMismatch
	}
	return nil
}

type governedRunKey struct{}

// WithGovernedRun marks ctx as a governed run's resolution. The legacy title
// expansion (CurrentExpansionService.Expand) refuses a marked context with
// ErrLegacyResolverInGovernedRun, so no title or cross-project lookup can
// run under a governed resolution even if a future caller reaches for it.
func WithGovernedRun(ctx context.Context) context.Context {
	return context.WithValue(ctx, governedRunKey{}, true)
}

// IsGovernedRun reports whether ctx carries WithGovernedRun.
func IsGovernedRun(ctx context.Context) bool {
	marked, _ := ctx.Value(governedRunKey{}).(bool)
	return marked
}
