package configurations

// The configuration of a governed run (ADR-0039, review of #1556).
//
// A governed run marks its context with WithGovernedRun. The legacy title
// expansion then refuses to run (ErrLegacyResolverInGovernedRun), so the
// toolkit settings walker resolves a configuration reference of a governed
// run here instead. It reads no configuration row and no vault: the stored
// reference must name its exact credential reference
// (CredentialReferenceIDField), and the walker freezes only that
// reference's target, the one non-secret value the worker needs to call the
// provider. The worker redeems the credential itself from the broker.

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
)

// GovernedConfigurationTarget is the target of one credential reference.
type GovernedConfigurationTarget struct {
	// ProviderType is the reference's adapter id (jira, confluence, ...).
	ProviderType string
	// TargetIdentity is the canonical target (credentialgrant.TargetBinding).
	TargetIdentity string
}

// GovernedConfigurationSource reads the target of an active credential
// reference in the run's tenant project. It answers
// ErrGovernedReferenceNotFound for a reference that is not there and
// ErrGovernedReferenceNotActive for one that is not active.
type GovernedConfigurationSource interface {
	GovernedConfigurationTarget(ctx context.Context, tenantProjectID int64, credentialReferenceID string) (GovernedConfigurationTarget, error)
}

// GovernedConfigurationTargets is the GovernedConfigurationSource over the
// credential grant readers.
type GovernedConfigurationTargets struct {
	references credentialgrant.ReferenceReader
	targets    credentialgrant.TargetReader
}

// NewGovernedConfigurationTargets builds the source.
func NewGovernedConfigurationTargets(references credentialgrant.ReferenceReader, targets credentialgrant.TargetReader) (*GovernedConfigurationTargets, error) {
	if references == nil || targets == nil {
		return nil, errors.New("governed configuration targets: reference and target readers are required")
	}
	return &GovernedConfigurationTargets{references: references, targets: targets}, nil
}

// GovernedConfigurationTarget implements GovernedConfigurationSource.
func (s *GovernedConfigurationTargets) GovernedConfigurationTarget(ctx context.Context, tenantProjectID int64, credentialReferenceID string) (GovernedConfigurationTarget, error) {
	reference, err := s.references.CredentialReference(ctx, tenantProjectID, credentialReferenceID)
	switch {
	case errors.Is(err, credentialgrant.ErrReferenceNotFound):
		return GovernedConfigurationTarget{}, ErrGovernedReferenceNotFound
	case err != nil:
		return GovernedConfigurationTarget{}, fmt.Errorf("governed configuration: read the credential reference: %w", err)
	case reference.TenantProjectID != tenantProjectID || reference.ID != credentialReferenceID:
		return GovernedConfigurationTarget{}, ErrGovernedReferenceNotFound
	case reference.State != credentialgrant.ReferenceActive:
		return GovernedConfigurationTarget{}, ErrGovernedReferenceNotActive
	}
	target, err := s.targets.TargetBinding(ctx, tenantProjectID, reference.TargetBindingID)
	switch {
	case errors.Is(err, credentialgrant.ErrTargetNotFound):
		return GovernedConfigurationTarget{}, ErrGovernedReferenceNotFound
	case err != nil:
		return GovernedConfigurationTarget{}, fmt.Errorf("governed configuration: read the target binding: %w", err)
	case target.TenantProjectID != tenantProjectID || target.ProviderType != reference.ProviderType:
		return GovernedConfigurationTarget{}, ErrGovernedReferenceNotFound
	}
	return GovernedConfigurationTarget{ProviderType: reference.ProviderType, TargetIdentity: target.TargetIdentity}, nil
}

// WithGovernedConfigurations makes the resolver serve the configuration
// references of a governed run (WithGovernedRun) from source. Without it a
// governed run's configuration reference is refused.
func (r *CurrentToolkitSettingsResolver) WithGovernedConfigurations(source GovernedConfigurationSource) *CurrentToolkitSettingsResolver {
	if r != nil {
		r.governed = source
	}
	return r
}

// governedTargetKeys names the configuration key a provider's worker
// adapter reads its target from, where it is not base_url.
var governedTargetKeys = map[string]string{"sharepoint": "site_url"}

// GovernedConfigurationTargetKey is the configuration key that carries the
// target of a governed toolkitType configuration.
func GovernedConfigurationTargetKey(toolkitType string) string {
	if key, ok := governedTargetKeys[toolkitType]; ok {
		return key
	}
	return "base_url"
}

// resolveGovernedConfiguration freezes a governed run's configuration
// reference as {<target key>: <target identity>, credential_reference_id}.
// A title-only, private, malformed, absent or inactive reference, or one of
// another provider, is a violation: the run fails closed.
func (w *currentToolkitSettingsWalker) resolveGovernedConfiguration(
	ctx context.Context,
	settings map[string]any,
	plan currentToolkitFieldPlan,
	fieldPath string,
	toolkitType string,
	reference map[string]any,
) error {
	if private, _ := reference["private"].(bool); private {
		w.addViolation(fieldPath, CurrentToolkitConfigurationForbiddenCode)
		return nil
	}
	id, _ := reference[CredentialReferenceIDField].(string)
	if _, err := uuid.Parse(id); err != nil || len(id) != 36 {
		// A title never selects a governed credential.
		w.addViolation(fieldPath, CurrentToolkitConfigurationReferenceInvalidCode)
		return nil
	}
	source := w.resolver.governed
	if source == nil {
		return ErrLegacyResolverInGovernedRun
	}
	target, err := source.GovernedConfigurationTarget(ctx, int64(w.request.ProjectID), id)
	switch {
	case errors.Is(err, ErrGovernedReferenceNotFound) || errors.Is(err, ErrGovernedReferenceNotActive):
		w.addViolation(fieldPath, CurrentToolkitConfigurationNotFoundCode)
		return nil
	case err != nil:
		return currentToolkitSettingsDependencyError(ctx, err)
	case target.ProviderType != toolkitType || target.TargetIdentity == "":
		w.addViolation(fieldPath, CurrentToolkitConfigurationTypeMismatchCode)
		return nil
	}
	key := GovernedConfigurationTargetKey(toolkitType)
	if err := w.budget.addContainer(2); err != nil {
		return err
	}
	for _, text := range []string{key, target.TargetIdentity, CredentialReferenceIDField, id} {
		if err := w.budget.addStringBytes(len(text)); err != nil {
			return err
		}
	}
	settings[plan.name] = map[string]any{key: target.TargetIdentity, CredentialReferenceIDField: id}
	return nil
}
