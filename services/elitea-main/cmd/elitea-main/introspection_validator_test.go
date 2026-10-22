package main

import (
	"context"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/authcomposition"
)

type stubValidator struct{}

func (stubValidator) ValidateToken(context.Context, string) (auth.User, error) {
	return auth.User{}, nil
}

// A deployment without ELITEA_AUTH_CONFIG_FILE (the dev compose, OIDC-only)
// has no form graph: its callback bearers are signed with
// APPLICATION_SECRET_KEY, so introspection must read them back with that
// validator instead of refusing to compose (found on the live dev stack).
func TestIntrospectionTokenValidatorFallsBackToTheSecretKeyValidator(t *testing.T) {
	session := stubValidator{}
	if got := introspectionTokenValidator(nil, session); got != session {
		t.Fatalf("without a form graph: got %#v, want the session validator", got)
	}
	if got := introspectionTokenValidator(nil, nil); got != nil {
		t.Fatalf("with neither: got %#v, want a nil interface", got)
	}
	graph := new(authcomposition.FormGraph)
	if got := introspectionTokenValidator(graph, session); got != graph {
		t.Fatalf("with a form graph: got %#v, want the graph", got)
	}
}
