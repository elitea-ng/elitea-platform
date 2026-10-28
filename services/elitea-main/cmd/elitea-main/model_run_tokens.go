package main

import (
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/modelruntoken"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/authcomposition"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

// modelRunTokenComposition is the issuer GetEliteaClientToken mints a
// governed run's client token with, and the validator the /llm edge reads
// it back with (#1461). Both are nil, never typed nils, when not composed.
type modelRunTokenComposition struct {
	Issuer    storage.ModelRunTokenIssuer
	Validator apimw.ModelRunTokenValidator
}

// modelRunTokensAtBoot composes the model run tokens wherever a governed pin
// can be read: credential governance on, or its continuation guard. That is
// the condition under which the content server reads pins, so it is the only
// condition under which a run can be served one; a flag-off deployment
// composes nothing and every run keeps its PAT.
//
// The issuer needs the runtime's actor tokens, which only the Form graph
// provides: without one no run is served a client token at all, so there is
// nothing to scope. The key is derived from the graph's PAT signing key and
// the lease is read from the platform database, the pool the /llm
// execution-id verifier reads execution_jobs from.
func modelRunTokensAtBoot(admission profile.Admission, formGraph *authcomposition.FormGraph, pool *pgxpool.Pool) (modelRunTokenComposition, error) {
	if !admission.Governed() && !admission.ContinuationGuard() {
		return modelRunTokenComposition{}, nil
	}
	if formGraph == nil || pool == nil {
		return modelRunTokenComposition{}, nil
	}
	signer, err := modelruntoken.NewSigner(formGraph.ModelRunTokenKey())
	if err != nil {
		return modelRunTokenComposition{}, fmt.Errorf("compose model run tokens: %w", err)
	}
	leases, err := modelruntoken.NewPostgresLeases(pool)
	if err != nil {
		return modelRunTokenComposition{}, fmt.Errorf("compose model run tokens: %w", err)
	}
	validator, err := modelruntoken.NewValidator(signer, leases)
	if err != nil {
		return modelRunTokenComposition{}, fmt.Errorf("compose model run tokens: %w", err)
	}
	return modelRunTokenComposition{Issuer: signer, Validator: validator}, nil
}
