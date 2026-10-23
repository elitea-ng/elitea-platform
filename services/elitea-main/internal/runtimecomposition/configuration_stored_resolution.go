package runtimecomposition

import (
	"context"
	"errors"

	configurationapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

var errCurrentStoredConfigurationResolution = errors.New(
	"current stored configuration resolution failed",
)

// CurrentStoredConfigurationResolution resolves ONE stored configuration row
// to the plaintext payload a live provider check needs
// (internal/api/v2/configurations/stored_check.go).
//
// It is the same walk currentProviderConfigurationResolution performs, over
// the same expander and the same vault unsecreter, in the same order: redeem
// the owner's own secret references first, then expand, so each nested
// configuration resolves through ITS OWN project's vault and no cross-project
// fallback can occur. The ONE difference is the disposal of the result.
//
//	currentProviderConfigurationResolution — throws the payload away. Its
//	    question is "does this row resolve at all", and the answer is the
//	    status_ok column. Returning the plaintext there would hand every
//	    lifecycle caller a live credential it has no use for.
//
//	this type — returns the payload, to ONE caller, which forwards it to the
//	    gateway's check endpoint and drops it. That caller may not log it,
//	    persist it, or return it to the browser.
//
// The two are kept as separate types rather than one with a flag, so that the
// capability to obtain plaintext is something a composition root grants
// deliberately: a handler holding this resolver can read any credential of the
// projects it serves, and that must be visible at the wiring, not hidden in a
// boolean argument.
type CurrentStoredConfigurationResolution struct {
	expander   currentConfigurationLifecycleExpander
	unsecreter currentConfigurationLifecycleUnsecreter
}

// NewCurrentStoredConfigurationResolver composes the resolver from the
// Configurations runtime, which owns the vault and the expander. It refuses
// rather than degrade: a resolver built without either collaborator could only
// hand back the stored {{secret.NAME}} reference as though it were the
// credential, and the check would report every working key as rejected.
func NewCurrentStoredConfigurationResolver(
	configurations *CurrentConfigurationsRuntime,
) (*CurrentStoredConfigurationResolution, error) {
	if configurations == nil || configurations.expander == nil || configurations.unsecreter == nil {
		return nil, errors.New("current stored configuration resolution composition is incomplete")
	}
	return &CurrentStoredConfigurationResolution{
		expander:   configurations.expander,
		unsecreter: configurations.unsecreter,
	}, nil
}

// ResolveStoredConfiguration returns the resolved payload for one stored row.
//
// Every failure is reported as one opaque sentinel, except a cancelled or
// expired context, which is returned as itself: the caller distinguishes "no
// answer was reached" from "this row does not resolve", and no dependency's
// error text — which can name another project's configuration — reaches it.
//
// The nil-receiver test is not decoration. A composition root that boxes a nil
// *CurrentStoredConfigurationResolution into the interface makes the handler's
// nil test false, and every call would then land on this method with a nil
// receiver. It answers "cannot resolve" instead of panicking the process.
func (resolution *CurrentStoredConfigurationResolution) ResolveStoredConfiguration(
	ctx context.Context,
	request configurationapi.StoredConfigurationResolution,
) (map[string]any, error) {
	if ctx == nil || resolution == nil || resolution.expander == nil || resolution.unsecreter == nil ||
		request.ProjectID <= 0 || request.Data == nil {
		return nil, errCurrentStoredConfigurationResolution
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	owned, err := resolution.unsecreter.Unsecret(ctx, request.ProjectID, request.Data)
	if err != nil {
		return nil, currentStoredConfigurationResolutionError(ctx, err)
	}
	expanded, err := resolution.expander.Expand(ctx, currentStoredExpansionRequest(request, owned))
	if err != nil {
		return nil, currentStoredConfigurationResolutionError(ctx, err)
	}
	if expanded == nil {
		return nil, errCurrentStoredConfigurationResolution
	}
	return expanded, nil
}

// currentStoredExpansionRequest is the expansion the resolution asks for.
//
// Unsecret stays TRUE so a nested configuration's own secrets are redeemed
// through that configuration's project vault — the reason the owner's secrets
// are redeemed separately first. UserID carries the row's author, which is the
// only identity a `private: true` reference may be resolved against; a row
// with no author resolves no private reference, which is a refusal rather than
// a fallback to the caller's project.
func currentStoredExpansionRequest(
	request configurationapi.StoredConfigurationResolution,
	owned map[string]any,
) configurationapp.CurrentExpansionRequest {
	return configurationapp.CurrentExpansionRequest{
		Payload:          owned,
		CurrentProjectID: request.ProjectID,
		UserID:           request.AuthorID,
		Unsecret:         true,
	}
}

func currentStoredConfigurationResolutionError(ctx context.Context, err error) error {
	if ctx != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return errCurrentStoredConfigurationResolution
}

var _ configurationapi.StoredConfigurationResolver = (*CurrentStoredConfigurationResolution)(nil)
