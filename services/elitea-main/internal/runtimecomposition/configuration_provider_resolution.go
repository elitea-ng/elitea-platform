package runtimecomposition

import (
	"context"
	"errors"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

var errCurrentProviderConfigurationResolution = errors.New(
	"current provider configuration resolution failed",
)

type currentConfigurationLifecycleExpander interface {
	Expand(context.Context, configurationapp.CurrentExpansionRequest) (map[string]any, error)
}

type currentConfigurationLifecycleUnsecreter interface {
	Unsecret(context.Context, int32, map[string]any) (map[string]any, error)
}

// currentProviderConfigurationResolution proves one sanitized lifecycle
// snapshot can actually be resolved before the row is marked status_ok.
//
// This used to be the materializer that built the LiteLLM administration
// payload. Nothing is pushed any more — the Bifrost gateway reads the same
// p_{projectID}.configuration rows this lifecycle writes, decrypting through
// the same Fernet vault — but the resolution itself was never LiteLLM's: it is
// the only place that discovers a dangling configuration reference or a secret
// the project's vault cannot redeem. Dropping it would advertise unusable rows
// to the gateway and defer the failure to a user's first request, so the walk
// is kept and its result is deliberately thrown away.
//
// Generic schemas and validation remain owned by Configurations and elitea-sdk;
// this adapter only resolves the already-declared references and hidden values.
// It must not persist, cache, log, or return the resolved payload.
type currentProviderConfigurationResolution struct {
	expander   currentConfigurationLifecycleExpander
	unsecreter currentConfigurationLifecycleUnsecreter
}

func newCurrentProviderConfigurationResolution(
	expander currentConfigurationLifecycleExpander,
	unsecreter currentConfigurationLifecycleUnsecreter,
) (*currentProviderConfigurationResolution, error) {
	if expander == nil || unsecreter == nil {
		return nil, errCurrentProviderConfigurationResolution
	}
	return &currentProviderConfigurationResolution{
		expander: expander, unsecreter: unsecreter,
	}, nil
}

func (resolution *currentProviderConfigurationResolution) ResolveCurrentProviderConfiguration(
	ctx context.Context,
	request configurationapp.CurrentProviderConfigurationResolution,
) error {
	snapshot := request.Configuration
	if ctx == nil || resolution == nil || resolution.expander == nil || resolution.unsecreter == nil ||
		snapshot.ProjectID <= 0 || snapshot.UUID == "" || snapshot.Type == "" || snapshot.Data == nil {
		return errCurrentProviderConfigurationResolution
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// Resolve the snapshot owner's own secret references first. Expansion then
	// resolves each nested configuration through that nested configuration's
	// owning project, avoiding cross-project vault fallback.
	owned, err := resolution.unsecreter.Unsecret(ctx, snapshot.ProjectID, snapshot.Data)
	if err != nil {
		return currentProviderConfigurationResolutionError(ctx, err)
	}
	expanded, err := resolution.expander.Expand(ctx, configurationapp.CurrentExpansionRequest{
		Payload:          owned,
		CurrentProjectID: snapshot.ProjectID,
		UserID:           snapshot.AuthorID,
		Unsecret:         true,
	})
	if err != nil {
		return currentProviderConfigurationResolutionError(ctx, err)
	}
	if expanded == nil {
		return errCurrentProviderConfigurationResolution
	}
	// The resolved payload holds live provider credentials. It is dropped here
	// on purpose rather than returned: the gateway reads and decrypts the
	// stored row itself, so nothing outside this call may see the plaintext.
	// It is deliberately not cleared in place — the expander may hand back a
	// map that still aliases the caller's snapshot.
	return nil
}

func currentProviderConfigurationResolutionError(ctx context.Context, err error) error {
	if ctx != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return errCurrentProviderConfigurationResolution
}

var _ configurationapp.CurrentProviderConfigurationResolver = (*currentProviderConfigurationResolution)(nil)
