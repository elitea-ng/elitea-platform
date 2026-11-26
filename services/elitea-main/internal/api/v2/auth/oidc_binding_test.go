package auth

// A federated link resolves only for the provider and issuer it is bound to
// (issue 2558, ADR-0059 R3B-1). Before this rule a link was `oidc:<sub>` or
// `saml:<NameID>` and nothing else, so whoever repointed the enabled provider
// at an identity provider they control could assert any subject and sign in as
// that account. These tests drive the resolution through the scripted
// transaction; the same rules against PostgreSQL are in
// identity_binding_postgres_integration_test.go.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identityrepo"
)

// A link bound only under another issuer (the provider's issuer was edited)
// is refused, and the account is not touched: no address repair, no binding.
func TestALinkBoundUnderAnotherIssuerIsRefused(t *testing.T) {
	tx := &scriptedTx{rows: []scriptedRow{{values: []any{7, false, false, true}}}}

	_, err := resolveProvisionedUser(context.Background(), tx,
		identityrepo.ProviderBinding{Key: "corporate", Issuer: "https://attacker.example"},
		"oidc:admin-sub", "admin@corp.com", "Admin", nil, false)
	if identityConflictCauseOf(err) != conflictProviderNotBound {
		t.Fatalf("err = %v, want the provider_not_bound conflict", err)
	}
	for _, fragment := range []string{"UPDATE auth_core__user", "identity_bindings (provider_ref", "ON CONFLICT (email)"} {
		if statementIndex(t, tx, fragment) >= 0 {
			t.Fatalf("a refused sign-in wrote %q: %v", fragment, tx.statements)
		}
	}
}

// A link with no binding at all predates shared migration 0258 on a protocol
// that had no enabled stored provider, or was written by a replica of the
// previous release during the upgrade. A STORED provider claims it only
// through the guarded upgrade-window statement (which binds nothing here: the
// scripted statement affects no row, as for a provider saved since 0258), and
// it is otherwise refused until a super_admin re-binds the links.
func TestAnUnboundLinkIsRefusedToAStoredProvider(t *testing.T) {
	tx := &scriptedTx{rows: []scriptedRow{{values: []any{7, false, false, false}}}}

	_, err := resolveProvisionedUser(context.Background(), tx, testBinding,
		"saml:alice", "alice@corp.com", "Alice", nil, false)
	if identityConflictCauseOf(err) != conflictProviderNotBound {
		t.Fatalf("err = %v, want the provider_not_bound conflict", err)
	}
	if statementIndex(t, tx, "VALUES ($1, $2, $3, $4)") >= 0 {
		t.Fatalf("a stored provider bound an unbound link unconditionally: %v", tx.statements)
	}
	if statementIndex(t, tx, "provider.updated_at <= ledger.applied_at") < 0 {
		t.Fatalf("the upgrade-window guard did not run: %v", tx.statements)
	}
}

// The ENVIRONMENT provider (OIDC_ISSUER_URL, configured only by an operator)
// binds an unbound link on its next sign-in. This is how a deployment with no
// stored provider keeps signing in across the upgrade.
func TestTheEnvironmentProviderBindsAnUnboundLink(t *testing.T) {
	tx := &scriptedTx{rows: []scriptedRow{{values: []any{7, false, false, false}}}}
	environment := identityrepo.ProviderBinding{
		Key: identityrepo.EnvironmentProviderKey, Issuer: "https://dex.internal",
	}

	userID, err := resolveProvisionedUser(context.Background(), tx, environment,
		"oidc:alice", "alice@corp.com", "Alice", nil, false)
	if err != nil || userID != 7 {
		t.Fatalf("resolved %d, %v; want account 7", userID, err)
	}
	if statementIndex(t, tx, "INSERT INTO elitea_auth.identity_bindings") < 0 {
		t.Fatalf("the environment provider did not bind the link: %v", tx.statements)
	}
}

// The environment provider does NOT take a link another provider holds: a
// deployment that falls back to the environment is a different provider.
func TestTheEnvironmentProviderDoesNotTakeALinkBoundElsewhere(t *testing.T) {
	tx := &scriptedTx{rows: []scriptedRow{{values: []any{7, false, false, true}}}}

	_, err := resolveProvisionedUser(context.Background(), tx,
		identityrepo.ProviderBinding{Key: identityrepo.EnvironmentProviderKey, Issuer: "https://dex.internal"},
		"oidc:alice", "alice@corp.com", "Alice", nil, false)
	if identityConflictCauseOf(err) != conflictProviderNotBound {
		t.Fatalf("err = %v, want the provider_not_bound conflict", err)
	}
}

// A first sign-in binds the new link to the provider that asserted it.
func TestAFirstSignInBindsTheNewLink(t *testing.T) {
	tx := &scriptedTx{rows: []scriptedRow{
		{err: pgx.ErrNoRows}, // no link for this subject
		{values: []any{11}},  // the account the address created
		{values: []any{11}},  // the link names that row
	}}

	if _, err := resolveProvisionedUser(context.Background(), tx, testBinding,
		"oidc:new-sub", "new@corp.com", "New", nil, false); err != nil {
		t.Fatalf("resolveProvisionedUser: %v", err)
	}
	link := statementIndex(t, tx, "INSERT INTO auth_core__user_provider")
	binding := statementIndex(t, tx, "INSERT INTO elitea_auth.identity_bindings")
	if link < 0 || binding < link {
		t.Fatalf("the new link was not bound after it was written: %v", tx.statements)
	}
}

// A resolution with no provider key or no issuer is a composition defect and
// fails before it reads anything.
func TestAResolutionWithoutABindingFailsClosed(t *testing.T) {
	for _, binding := range []identityrepo.ProviderBinding{
		{}, {Key: "corporate"}, {Issuer: "https://idp.corp.example"},
	} {
		tx := &scriptedTx{}
		_, err := resolveProvisionedUser(context.Background(), tx, binding,
			"oidc:alice", "alice@corp.com", "Alice", nil, false)
		if !errors.Is(err, identityrepo.ErrInvalidBinding) {
			t.Fatalf("binding %+v: err = %v, want ErrInvalidBinding", binding, err)
		}
		if len(tx.statements) != 0 {
			t.Fatalf("binding %+v: statements ran: %v", binding, tx.statements)
		}
	}
}

// The 409 says what to do, in both protocols, and names no account.
func TestTheNotBoundRefusalNamesTheRemedy(t *testing.T) {
	message := oidcIdentityConflictMessage(conflictProviderNotBound)
	if !strings.Contains(message, "re-bind") || strings.Contains(message, "oidc-relink") {
		t.Fatalf("OIDC message = %q", message)
	}
	recorder := httptest.NewRecorder()
	(&SAMLHandler{}).writeProvisioningFailure(recorder, newIdentityConflict(conflictProviderNotBound), "alice@corp.com")
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "re-bind") {
		t.Fatalf("SAML answer = %d %q", recorder.Code, recorder.Body.String())
	}
}

// Each runtime carries the binding its links are checked against: a stored
// provider's key and document issuer, or the environment key and
// OIDC_ISSUER_URL.
func TestTheEnvironmentRuntimeCarriesTheEnvironmentBinding(t *testing.T) {
	issuer := newFakeIdentityProvider(t).server.URL
	runtime, err := newOIDCRuntimeFromEnvironment(context.Background(), loopbackIdentityClient(t), &OIDCConfig{
		IssuerURL: issuer, ClientID: "elitea", RedirectURI: "https://elitea.example/auth/oidc/callback",
	})
	if err != nil {
		t.Fatalf("newOIDCRuntimeFromEnvironment: %v", err)
	}
	want := identityrepo.ProviderBinding{Key: identityrepo.EnvironmentProviderKey, Issuer: issuer}
	if runtime.binding != want {
		t.Fatalf("binding = %+v, want %+v", runtime.binding, want)
	}
}
