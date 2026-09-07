package main

import (
	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
)

// productionAuthenticationComposed reports whether the composition root built
// a credential plane that a product capability may mount behind.
//
// It reads the ALREADY-COMPOSED apimw.AuthConfig — the one
// apiGroupAuthConfig returns — instead of testing the FormGraph. The FormGraph
// was never the requirement. A capability needs two things: one reader of a
// caller-supplied credential, and a PrincipalValidator that re-checks the
// named user against the database on every request. Without the validator a
// deactivated user's unexpired credential still passes, because RBAC rows
// survive deactivation (#301, #314, #370).
//
// The FormGraph test made the two shapes disagree. ELITEA_AUTH_CONFIG_FILE is
// the ONLY thing that builds a FormGraph, so a deployment with real corporate
// single sign-on — OIDC, or a SAML/typed identity provider, which share the
// same session composition — was refused a capability it can authenticate
// perfectly well. Gap G2 records the effect: an OIDC-only Kubernetes install
// composed no Configurations plane at all, so every LLM credential and model
// had to be inserted with deploy/scripts/seed-llm-api.py or raw SQL.
//
// A deployment with NO authentication is still refused. apiGroupAuthConfig
// returns the zero AuthConfig when neither plane exists, and the zero value
// fails both halves of this test. AUTH_DEV_MODE cannot reach here at all: it
// is a startup error (ADR-0017), not a third plane.
//
// Validator is the credential reader apimw.Auth's Bearer branch uses. It used
// to be joined here by Client, the pylon Redis-RPC validator; #383 deleted
// that client, so Validator is the only token reader left. SessionSecret
// counts because the browser session cookie is a credential the deployment
// issues and apimw.Auth verifies with the same key.
func productionAuthenticationComposed(auth apimw.AuthConfig) bool {
	if auth.PrincipalValidator == nil {
		return false
	}
	return auth.ForwardedIdentityVerifier != nil ||
		auth.Validator != nil ||
		auth.SessionSecret != ""
}
