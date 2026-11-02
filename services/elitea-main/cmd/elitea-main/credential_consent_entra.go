package main

import (
	"log/slog"

	v2credentialconsent "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/credentialconsent"
	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
)

// withEntraReconsent adds the Entra re-consent of a paused governed run
// (#1477) to the consent routes: POST /credential_consent/entra:start with
// a `car_` authorization_reference, a new sign-in, and the callback that
// creates the new delegation and resumes the run through
// CredentialAuthorizations.Resume.
//
// It needs Entra governance (custody) and the chat continuation use case.
// Without either it answers consent unchanged. With no provider OAuth
// application, it builds the consent routes for Entra alone.
func withEntraReconsent(
	consent *v2credentialconsent.Handler,
	entra *entraGovernance,
	continuer agentexecutionapp.CurrentAgentContinuer,
	secureCookies bool,
	logger *slog.Logger,
) (*v2credentialconsent.Handler, error) {
	if entra == nil || entra.Delegations == nil || continuer == nil {
		return consent, nil
	}
	authorizations, err := agentexecutionapp.NewCredentialAuthorizations(entra.Delegations, continuer)
	if err != nil {
		return nil, err
	}
	if consent != nil {
		return consent.WithEntra(entra.Delegations, authorizations, secureCookies), nil
	}
	handler, err := v2credentialconsent.New(nil, logger,
		v2credentialconsent.WithEntraConsent(entra.Delegations, authorizations, secureCookies))
	if err != nil {
		return nil, err
	}
	logger.Info("Entra credential re-consent enabled")
	return handler, nil
}
