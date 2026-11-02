package agentexecution

import (
	"context"
	"encoding/json"
	"errors"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

// FreezeToolkit resolves the governed credential of ONE toolkit for an
// unattended run that runs no agent version itself: a governed scheduled
// index run (#1473). The run's authority is its mandate, so the agent
// binding is the mandate's (agentBindingID), not one inferred from a
// version.
//
// The rules are the agent admission's (freezeToolkit, #1119):
//
//   - a toolkit whose stored settings name a configuration reference must
//     resolve it exactly, or the run is refused with a typed
//     GovernedCredentialAdmissionError;
//   - a toolkit with no configuration reference that the binding binds is
//     frozen at its digest alone, and the broker takes the one usable grant;
//   - a toolkit the binding does not bind is not frozen. A credentialed
//     toolkit of that kind then gets no credential from the broker.
//
// The freeze names the binding and no application version: the admission
// transaction compares the binding with the one the mandate binds.
func (g *GovernedCredentials) FreezeToolkit(ctx context.Context, tenantProjectID int64, agentBindingID string, toolkitID int64) (*GovernedCredentialFreeze, error) {
	if g == nil {
		return nil, errors.New("governed credentials: not composed")
	}
	if tenantProjectID <= 0 || agentBindingID == "" || toolkitID <= 0 {
		return nil, ErrInvalidAgentAdmission
	}
	ctx = configurationapp.WithGovernedRun(ctx)
	entry, err := json.Marshal(map[string]int64{"id": toolkitID})
	if err != nil {
		return nil, err
	}
	freeze := &GovernedCredentialFreeze{AgentBindingID: agentBindingID}
	_, ref, err := g.freezeToolkit(ctx, tenantProjectID, agentBindingID, entry)
	if err != nil {
		return nil, err
	}
	if ref != nil {
		freeze.References = []GovernedCredentialReference{*ref}
	}
	return freeze, nil
}

type unattendedFreezeKey struct{}

// WithUnattendedCredentialFreeze carries the governed credential freeze of
// an unattended run that has no agent admission step (an index run, #1473)
// to the admission transaction that ctx reaches. The transaction writes it
// into the run's security context.
func WithUnattendedCredentialFreeze(ctx context.Context, freeze *GovernedCredentialFreeze) context.Context {
	if freeze == nil {
		return ctx
	}
	return context.WithValue(ctx, unattendedFreezeKey{}, freeze)
}

// UnattendedCredentialFreezeFrom reads the freeze
// WithUnattendedCredentialFreeze carried; nil when there is none.
func UnattendedCredentialFreezeFrom(ctx context.Context) *GovernedCredentialFreeze {
	freeze, _ := ctx.Value(unattendedFreezeKey{}).(*GovernedCredentialFreeze)
	return freeze
}
