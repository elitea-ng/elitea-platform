package credentialadapters

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
)

// ErrInvalidDescriptor refuses a descriptor NewRegistry cannot admit.
var ErrInvalidDescriptor = errors.New("credential adapter: invalid descriptor")

// providerTypePattern is one segment of an operation id.
var providerTypePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Registry is the immutable set of adapter descriptors. Build it with
// NewRegistry at the composition root; nothing changes it afterwards, so
// it is safe for concurrent use.
type Registry struct {
	providers  map[string]AdapterDescriptor
	operations map[credentialgrant.OperationID]operationEntry
	order      []string
}

type operationEntry struct {
	provider string
	spec     OperationSpec
}

var _ credentialgrant.OperationRegistry = (*Registry)(nil)

// NewRegistry validates every descriptor and builds the registry. It
// refuses a duplicate provider, an unknown or undeclared class, an
// operation id used twice (in one provider or across providers), material
// keys or a qualification for a class the descriptor does not declare, a
// declared class with no material keys or no qualification, and a
// "qualified" value without an evidence ref.
func NewRegistry(ds ...AdapterDescriptor) (*Registry, error) {
	r := &Registry{
		providers:  make(map[string]AdapterDescriptor, len(ds)),
		operations: map[credentialgrant.OperationID]operationEntry{},
	}
	for _, raw := range ds {
		d := raw.clone()
		if err := validate(d); err != nil {
			return nil, err
		}
		if _, dup := r.providers[d.ProviderType]; dup {
			return nil, fmt.Errorf("%w: provider %q is registered twice", ErrInvalidDescriptor, d.ProviderType)
		}
		for _, op := range d.Operations {
			if owner, dup := r.operations[op.ID]; dup {
				return nil, fmt.Errorf("%w: operation %q of %q is already registered by %q",
					ErrInvalidDescriptor, op.ID, d.ProviderType, owner.provider)
			}
			r.operations[op.ID] = operationEntry{provider: d.ProviderType, spec: op}
		}
		r.providers[d.ProviderType] = d
		r.order = append(r.order, d.ProviderType)
	}
	return r, nil
}

func validate(d AdapterDescriptor) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%w: provider %q: %s", ErrInvalidDescriptor, d.ProviderType, fmt.Sprintf(format, args...))
	}
	if !providerTypePattern.MatchString(d.ProviderType) {
		return fail("the provider type must be one lowercase operation id segment")
	}
	if len(d.AuthClasses) == 0 {
		return fail("no auth class is declared")
	}
	for i, class := range d.AuthClasses {
		if !class.Valid() {
			return fail("auth class %q is unknown", class)
		}
		if slices.Contains(d.AuthClasses[:i], class) {
			return fail("auth class %q is declared twice", class)
		}
	}
	if d.TargetKind != TargetBaseURL && d.TargetKind != TargetTenantSite && d.TargetKind != TargetResourceURI {
		return fail("target kind %q is unknown", d.TargetKind)
	}
	if d.CanonicalizeTarget == nil {
		return fail("no target canonicalizer")
	}
	if len(d.Operations) == 0 {
		return fail("no operation is declared")
	}
	own := make(map[credentialgrant.OperationID]OperationSpec, len(d.Operations))
	for _, op := range d.Operations {
		switch {
		case !op.ID.WellFormed():
			return fail("operation %q is not provider.resource.verb", op.ID)
		case !strings.HasPrefix(string(op.ID), d.ProviderType+"."):
			return fail("operation %q does not start with the provider type", op.ID)
		case op.Class != OperationRead && op.Class != OperationWrite:
			return fail("operation %q has class %q, want read or write", op.ID, op.Class)
		case strings.TrimSpace(op.ResourceKind) == "":
			return fail("operation %q names no resource kind", op.ID)
		case op.CostHint < 0:
			return fail("operation %q has a negative cost hint", op.ID)
		}
		if _, dup := own[op.ID]; dup {
			return fail("operation %q is declared twice", op.ID)
		}
		own[op.ID] = op
	}
	if d.Probe != nil {
		op, ok := own[d.Probe.Operation]
		if !ok {
			return fail("probe operation %q is not one of its operations", d.Probe.Operation)
		}
		if op.Class != OperationRead {
			return fail("probe operation %q is not a read", d.Probe.Operation)
		}
	}
	for class, keys := range d.MaterialKeys {
		if !d.Accepts(class) {
			return fail("material keys for undeclared class %q", class)
		}
		if err := distinctNonBlank(keys); err != nil {
			return fail("material keys of %q: %v", class, err)
		}
	}
	for class, scopes := range d.Scopes {
		if !d.Accepts(class) {
			return fail("scopes for undeclared class %q", class)
		}
		if err := distinctNonBlank(scopes); err != nil {
			return fail("scopes of %q: %v", class, err)
		}
	}
	for class, value := range d.Qualification {
		if !d.Accepts(class) {
			return fail("qualification for undeclared class %q", class)
		}
		if err := validQualification(value); err != nil {
			return fail("qualification of %q: %v", class, err)
		}
	}
	for _, class := range d.AuthClasses {
		if len(d.MaterialKeys[class]) == 0 {
			return fail("class %q lists no material key", class)
		}
		if _, ok := d.Qualification[class]; !ok {
			return fail("class %q states no qualification", class)
		}
	}
	switch {
	case d.Accepts(credentialgrant.ClassOAuthDelegated) && !d.OAuthAuthority.Valid():
		return fail("the oauth_delegated class states no OAuth authority (entra_obo or provider)")
	case !d.Accepts(credentialgrant.ClassOAuthDelegated) && d.OAuthAuthority != "":
		return fail("an OAuth authority without the oauth_delegated class")
	case d.OAuthAuthority == credentialgrant.OAuthAuthorityEntraOBO && d.Audience == "":
		return fail("an Entra on-behalf-of class needs the resource audience")
	}
	for _, class := range d.RotationRequired {
		if !d.Accepts(class) {
			return fail("rotation requirement for undeclared class %q", class)
		}
	}
	return nil
}

func distinctNonBlank(values []string) error {
	for i, v := range values {
		if strings.TrimSpace(v) == "" || v != strings.TrimSpace(v) {
			return errors.New("a value is blank or padded")
		}
		if slices.Contains(values[:i], v) {
			return fmt.Errorf("%q repeats", v)
		}
	}
	return nil
}

// Known reports whether id is an operation of a registered provider. A nil
// registry knows nothing.
func (r *Registry) Known(id credentialgrant.OperationID) bool {
	if r == nil {
		return false
	}
	_, ok := r.operations[id]
	return ok
}

// Class is the operation's class ("read" or "write"); empty for an unknown id.
func (r *Registry) Class(id credentialgrant.OperationID) string {
	if r == nil {
		return ""
	}
	return r.operations[id].spec.Class
}

// Operation returns the spec of id.
func (r *Registry) Operation(id credentialgrant.OperationID) (OperationSpec, bool) {
	if r == nil {
		return OperationSpec{}, false
	}
	entry, ok := r.operations[id]
	return entry.spec, ok
}

// ProviderFor names the provider that serves id.
func (r *Registry) ProviderFor(id credentialgrant.OperationID) (string, bool) {
	if r == nil {
		return "", false
	}
	entry, ok := r.operations[id]
	return entry.provider, ok
}

// MaterialKeys are the envelope keys the broker may release for class of
// providerType. Nil for an unknown provider or an undeclared class: the
// broker then releases nothing. The slice is a copy.
func (r *Registry) MaterialKeys(providerType string, class credentialgrant.CredentialClass) []string {
	if r == nil {
		return nil
	}
	d, ok := r.providers[providerType]
	if !ok || !d.Accepts(class) {
		return nil
	}
	return slices.Clone(d.MaterialKeys[class])
}

// OAuthResource is the delegated token's resource and least-privilege
// scopes of providerType for class: empty when the descriptor names none.
func (r *Registry) OAuthResource(providerType string, class credentialgrant.CredentialClass) (string, []string) {
	if r == nil {
		return "", nil
	}
	d, ok := r.providers[providerType]
	if !ok || !d.Accepts(class) {
		return "", nil
	}
	return d.Audience, slices.Clone(d.Scopes[class])
}

// OAuthAuthority is who issued the consent of providerType's class:
// empty for an unknown provider or a class that is not oauth_delegated.
func (r *Registry) OAuthAuthority(providerType string, class credentialgrant.CredentialClass) credentialgrant.OAuthAuthority {
	if r == nil || class != credentialgrant.ClassOAuthDelegated {
		return ""
	}
	d, ok := r.providers[providerType]
	if !ok || !d.Accepts(class) {
		return ""
	}
	return d.OAuthAuthority
}

// Descriptor returns a copy of providerType's descriptor.
func (r *Registry) Descriptor(providerType string) (AdapterDescriptor, bool) {
	if r == nil {
		return AdapterDescriptor{}, false
	}
	d, ok := r.providers[providerType]
	if !ok {
		return AdapterDescriptor{}, false
	}
	return d.clone(), true
}

// Providers lists the registered provider types in registration order.
func (r *Registry) Providers() []string {
	if r == nil {
		return nil
	}
	return slices.Clone(r.order)
}
