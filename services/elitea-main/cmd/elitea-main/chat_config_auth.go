package main

import (
	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
)

// chatConfigAuthConfig returns the credential set for the ungated chat_config
// / project-context pair, UNCHANGED from apiGroupAuth — the credential set
// the whole /api/v2 group authenticates with.
//
// It used to re-compose its own AuthConfig from the same raw inputs
// (formGraph, principalValidator, forwardedIdentityVerifier,
// sessionSecret) that build apiGroupAuth, one call earlier in main.go. The
// gate in front of the only call site,
// `formGraph != nil || oidcSessionHandler != nil`, makes the two
// compositions IDENTICAL in both branches: when formGraph is nil the gate
// proves oidcSessionHandler is not, which is exactly apiGroupAuthConfig's
// `oidcSessionEnabled` condition. Two functions computing the same value
// from the same inputs can still drift, and did: apiGroupAuthConfig's
// OIDC-only branch carries `Validator: sessionTokens` (the pool-backed
// personal-access-token validator — see its comment for the deliberate
// widening this fixed on the whole group), and chatConfigAuthConfig's
// OIDC-only branch never gained it. A caller presenting the same Bearer
// token or X-API-Key that every other /api/v2 route accepted got 401 on
// GET /api/v2/elitea_core/chat_config/prompt_lib/{projectID} in an
// OIDC-only deployment — the one deployment shape that issues no other
// credential.
//
// Reusing apiGroupAuth outright — rather than adding the missing field here
// too — makes that drift impossible: there is only one composition left to
// keep in sync with itself. This is the same reasoning main.go gives for
// `currentAuth := apiGroupAuth` ahead of the current-Configurations reads,
// which shadow the compatibility handler at the same route patterns for the
// same reason.
//
// The route still resolves permissions through its own per-project gate
// (legacyrbac.NewPostgresResolver) after authentication; this widening only
// decides which credential PROVES the caller, same as apiGroupAuthConfig.
func chatConfigAuthConfig(apiGroupAuth apimw.AuthConfig) apimw.AuthConfig {
	return apiGroupAuth
}
