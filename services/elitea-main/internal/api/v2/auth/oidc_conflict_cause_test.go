package auth

// The 409 of an OIDC login names the cause of the conflict and only that
// cause's remedy (#1460 item 12, review of #1548). These drive the
// resolution through the scripted transaction of oidc_test.go.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// A refused email fallback reports WHY the account may not be adopted, and
// the 409 names the remedy for that cause only: oidc-relink for an account
// another OIDC subject holds, adopt_scim_users for a SCIM account, and
// neither for the others.
func TestARefusedFallbackReportsItsCause(t *testing.T) {
	for name, tc := range map[string]struct {
		row     scriptedRow // suspended, OIDC-held, SAML-held, SCIM
		adopt   bool
		want    identityConflictCause
		says    string
		mustNot []string
	}{
		"re-created OIDC subject": {scriptedRow{values: []any{false, true, false, false}}, false,
			conflictOIDCSubjectHeld, "elitea-main oidc-relink", []string{"adopt_scim_users", "SAML"}},
		"SAML holds the account": {scriptedRow{values: []any{false, false, true, false}}, false,
			conflictSAMLSubjectHeld, "SAML", []string{"oidc-relink", "adopt_scim_users"}},
		"SCIM adoption refused": {scriptedRow{values: []any{false, false, false, true}}, false,
			conflictSCIMAdoptionRefused, "adopt_scim_users", []string{"oidc-relink", "already linked"}},
		"SCIM row adoptable now: a concurrent change": {scriptedRow{values: []any{false, false, false, true}}, true,
			conflictConcurrentChange, "Sign in again", []string{"oidc-relink", "adopt_scim_users"}},
		"row deleted concurrently": {scriptedRow{err: pgx.ErrNoRows}, false,
			conflictConcurrentChange, "Sign in again", []string{"oidc-relink", "adopt_scim_users"}},
	} {
		t.Run(name, func(t *testing.T) {
			tx := &scriptedTx{rows: []scriptedRow{
				{err: pgx.ErrNoRows}, // no link for this subject
				{err: pgx.ErrNoRows}, // the email upsert refused the row
				tc.row,
			}}
			_, err := resolveProvisionedUserFor(
				context.Background(), tx, "oidc:sub-N", "nina@corp.com", "Nina", nil, false, tc.adopt,
			)
			if !errors.Is(err, errIdentityConflict) || identityConflictCauseOf(err) != tc.want {
				t.Fatalf("err = %v, want the conflict cause %s", err, tc.want)
			}
			message := oidcIdentityConflictMessage(identityConflictCauseOf(err))
			if !strings.Contains(message, tc.says) {
				t.Fatalf("409 %q does not say %q", message, tc.says)
			}
			for _, wrong := range tc.mustNot {
				if strings.Contains(message, wrong) {
					t.Fatalf("409 %q says %q, which is not this cause's remedy", message, wrong)
				}
			}
		})
	}
}

// The two causes outside the fallback: a linked subject whose new address
// another account holds, and a link that a concurrent login won.
func TestTheOtherConflictsCarryTheirCause(t *testing.T) {
	tx := &scriptedTx{
		rows:       []scriptedRow{{values: []any{7, false}}},
		execErrors: map[string]error{"SET last_login": &pgconn.PgError{Code: "23505"}},
	}
	_, err := resolveProvisionedUser(context.Background(), tx, "oidc:sub-A", "taken@corp.com", "Alice", nil, false)
	if identityConflictCauseOf(err) != conflictEmailTaken ||
		strings.Contains(oidcIdentityConflictMessage(conflictEmailTaken), "oidc-relink") {
		t.Fatalf("err = %v, want %s without the relink remedy", err, conflictEmailTaken)
	}

	tx = &scriptedTx{rows: []scriptedRow{{err: pgx.ErrNoRows}, {values: []any{11}}, {values: []any{7}}}}
	_, err = resolveProvisionedUser(context.Background(), tx, "oidc:sub-A", "carol@corp.com", "Carol", nil, false)
	if identityConflictCauseOf(err) != conflictConcurrentChange {
		t.Fatalf("err = %v, want %s", err, conflictConcurrentChange)
	}
	if oidcIdentityConflictMessage(conflictUnknown) == "" || identityConflictCauseOf(errIdentityConflict) != conflictUnknown {
		t.Fatal("a bare errIdentityConflict has no answer")
	}
}
