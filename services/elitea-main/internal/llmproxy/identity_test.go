package llmproxy

import (
	"context"
	"net/http"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenant"
)

func fullCtx() context.Context {
	ctx := context.Background()
	ctx = middleware.ContextWithProject(ctx, middleware.ProjectContext{ProjectID: 42, PublicProjectID: 1})
	ctx = auth.ContextWithUser(ctx, auth.User{ID: "user-7"})
	ctx = tenant.WithTenant(ctx, "acme")
	return ctx
}

func TestIdentityFromContext(t *testing.T) {
	id := identityFromContext(fullCtx())
	if id.projectID != "42" {
		t.Errorf("projectID = %q, want 42", id.projectID)
	}
	if id.userID != "user-7" {
		t.Errorf("userID = %q, want user-7", id.userID)
	}
	if id.tenantID != "acme" {
		t.Errorf("tenantID = %q, want acme", id.tenantID)
	}
}

func TestIdentityFromContext_Empty(t *testing.T) {
	id := identityFromContext(context.Background())
	if id.projectID != "" || id.userID != "" || id.tenantID != "" {
		t.Errorf("expected empty identity, got %+v", id)
	}
}

func TestIdentityFromContext_ZeroProjectIgnored(t *testing.T) {
	ctx := middleware.ContextWithProject(context.Background(), middleware.ProjectContext{ProjectID: 0})
	id := identityFromContext(ctx)
	if id.projectID != "" {
		t.Errorf("projectID = %q, want empty for zero project", id.projectID)
	}
}

func TestInjectIdentity_SetsAndSigns(t *testing.T) {
	secret := []byte("shared-secret")
	h := http.Header{}
	injectIdentity(fullCtx(), h, secret, nil)

	if got := h.Get(HeaderProjectID); got != "42" {
		t.Errorf("%s = %q, want 42", HeaderProjectID, got)
	}
	if got := h.Get(HeaderUserID); got != "user-7" {
		t.Errorf("%s = %q, want user-7", HeaderUserID, got)
	}
	if got := h.Get(HeaderTenantID); got != "acme" {
		t.Errorf("%s = %q, want acme", HeaderTenantID, got)
	}
	if !verifyIdentitySignature(h, secret) {
		t.Errorf("signature did not verify; header=%q", h.Get(HeaderSignature))
	}
}

func TestInjectIdentity_StripsSpoofedHeaders(t *testing.T) {
	secret := []byte("shared-secret")
	// Caller tries to spoof a different project with a bogus signature.
	h := http.Header{}
	h.Set(HeaderProjectID, "999")
	h.Set(HeaderUserID, "attacker")
	h.Set(HeaderTenantID, "evil")
	h.Set(HeaderSignature, "sha256=deadbeef")

	injectIdentity(fullCtx(), h, secret, nil)

	if got := h.Get(HeaderProjectID); got != "42" {
		t.Errorf("spoofed project not overwritten: %s = %q", HeaderProjectID, got)
	}
	if got := h.Get(HeaderUserID); got != "user-7" {
		t.Errorf("spoofed user not overwritten: %s = %q", HeaderUserID, got)
	}
	// The re-signed signature must match the resolved (not spoofed) identity.
	if !verifyIdentitySignature(h, secret) {
		t.Errorf("re-signed signature did not verify")
	}
	// And the attacker's project must NOT verify.
	spoofed := http.Header{}
	spoofed.Set(HeaderProjectID, "999")
	spoofed.Set(HeaderSignature, h.Get(HeaderSignature))
	if verifyIdentitySignature(spoofed, secret) {
		t.Errorf("signature validated against spoofed project 999")
	}
}

func TestInjectIdentity_NoSecretNoSignature(t *testing.T) {
	h := http.Header{}
	injectIdentity(fullCtx(), h, nil, nil)
	if got := h.Get(HeaderSignature); got != "" {
		t.Errorf("expected no signature without a secret, got %q", got)
	}
	// Identity headers are still set.
	if h.Get(HeaderProjectID) != "42" {
		t.Errorf("project header not set without secret")
	}
}

func TestInjectIdentity_NoProjectNoSignature(t *testing.T) {
	secret := []byte("s")
	ctx := auth.ContextWithUser(context.Background(), auth.User{ID: "u"})
	h := http.Header{}
	injectIdentity(ctx, h, secret, nil)
	if h.Get(HeaderProjectID) != "" {
		t.Errorf("unexpected project header")
	}
	if h.Get(HeaderSignature) != "" {
		t.Errorf("expected no signature without a project, got %q", h.Get(HeaderSignature))
	}
	if h.Get(HeaderUserID) != "u" {
		t.Errorf("user header not set")
	}
}

func TestCanonical_NoFieldAmbiguity(t *testing.T) {
	// project "1" + user "2" must not collide with project "12" + empty user.
	a := identity{projectID: "1", userID: "2"}
	b := identity{projectID: "12", userID: ""}
	if a.canonical(signatureVersionV1) == b.canonical(signatureVersionV1) {
		t.Errorf("canonical strings collide: %q == %q", a.canonical(signatureVersionV1), b.canonical(signatureVersionV1))
	}
}

func TestVerifyIdentitySignature_TamperedField(t *testing.T) {
	secret := []byte("k")
	h := http.Header{}
	injectIdentity(fullCtx(), h, secret, nil)
	// Tamper with the tenant after signing.
	h.Set(HeaderTenantID, "other")
	if verifyIdentitySignature(h, secret) {
		t.Errorf("tampered tenant still verified")
	}
}

func TestVerifyIdentitySignature_WrongSecret(t *testing.T) {
	h := http.Header{}
	injectIdentity(fullCtx(), h, []byte("right"), nil)
	if verifyIdentitySignature(h, []byte("wrong")) {
		t.Errorf("verified under the wrong secret")
	}
}

func TestVerifyIdentitySignature_Missing(t *testing.T) {
	if verifyIdentitySignature(http.Header{}, []byte("k")) {
		t.Errorf("verified with no signature header present")
	}
}

func TestStripIdentityHeaders(t *testing.T) {
	h := http.Header{}
	h.Set(HeaderProjectID, "1")
	h.Set(HeaderUserID, "2")
	h.Set(HeaderTenantID, "3")
	h.Set(HeaderSignature, "x")
	stripIdentityHeaders(h)
	for _, k := range []string{HeaderProjectID, HeaderUserID, HeaderTenantID, HeaderSignature} {
		if h.Get(k) != "" {
			t.Errorf("%s not stripped", k)
		}
	}
}
