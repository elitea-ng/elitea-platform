package llmproxy

import (
	"context"
	"testing"
)

type fakeGoogleTokens struct{}

func (fakeGoogleTokens) IDToken(context.Context, string) (string, error) { return "id", nil }
func (fakeGoogleTokens) AccessToken(context.Context) (string, error)     { return "access", nil }

// TestGuardrailEnvGoogleTokenOnlyWhenArmed: a keyless model_armor detector
// gets the gateway's Google identity only when workload identity is armed.
func TestGuardrailEnvGoogleTokenOnlyWhenArmed(t *testing.T) {
	h := &Handler{}
	if h.guardrailEnv().GoogleAccessToken != nil {
		t.Fatal("an unarmed gateway handed out its Google identity")
	}
	WithWorkloadIdentity(false, "1", fakeGoogleTokens{})(h)
	if h.guardrailEnv().GoogleAccessToken != nil {
		t.Fatal("a disabled workload identity handed out its Google identity")
	}
	WithWorkloadIdentity(true, "1", fakeGoogleTokens{})(h)
	mint := h.guardrailEnv().GoogleAccessToken
	if mint == nil {
		t.Fatal("an armed gateway gave no Google token minter")
	}
	if tok, err := mint(context.Background()); err != nil || tok != "access" {
		t.Fatalf("token = %q, %v", tok, err)
	}
}
