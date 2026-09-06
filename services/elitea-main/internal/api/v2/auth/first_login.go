package auth

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/identity"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/identityrepo"
)

// FirstLoginPolicy is the operator configuration this plane applies once an
// assertion has resolved to an account. It is trusted configuration captured at
// construction; nothing an identity provider asserts can add to it.
type FirstLoginPolicy struct {
	// InitialGlobalAdmins is `identity.initial_global_admins` from the
	// authentication configuration document, or ELITEA_INITIAL_GLOBAL_ADMINS
	// when the deployment has no document. See matchesInitialGlobalAdmin for
	// what an entry is compared against.
	InitialGlobalAdmins []string
}

// InitialGlobalAdminsFromEnv reads the list for a deployment that federates
// logins WITHOUT an authentication configuration document.
//
// The document is the primary source and main.go passes it when it has one, so
// the list has one meaning on both browser planes. This exists because a
// single-sign-on-only deployment is exactly the shape that has no document, and
// that shape is the one with no other way to make its first administrator.
//
// Keep "ELITEA_INITIAL_GLOBAL_ADMINS" a literal inside this os.Getenv call.
// services/elitea-llm-gateway/scripts/env-drift-check.sh greps for a quoted
// name in an os.Getenv call; a named constant hides the read and the gate then
// reports a false green.
func InitialGlobalAdminsFromEnv() []string {
	raw := os.Getenv("ELITEA_INITIAL_GLOBAL_ADMINS")
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var admins []string
	for _, entry := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(entry); trimmed != "" {
			admins = append(admins, trimmed)
		}
	}
	return admins
}

// matchesInitialGlobalAdmin decides whether a stored provider reference names a
// configured initial global administrator.
//
// THE MATCHING RULE, stated once, because getting it wrong makes the whole
// feature silently do nothing:
//
//	A configured entry matches if it equals the reference EXACTLY, or if it
//	equals the reference with its federation namespace prefix removed.
//
// Two spellings are accepted on purpose. This plane stores PREFIXED references
// — `oidc:<sub>` and `saml:<nameid>`, see FederatedRefPrefixes in oidc.go —
// while the other provisioning plane (internal/application/identity) stores and
// matches BARE ones. One configuration key that means the bare subject on one
// plane and the prefixed one on the other is how this feature would come back
// broken. `oidc.go:577` records that the namespace mismatch has already caused
// one silent failure here, when a guard read `LIKE 'oidc:%'` and SAML wrote
// `saml:`.
//
// The comparison itself is delegated to identity.MatchesInitialGlobalAdmin so
// that "who is an initial admin" has ONE definition. Only the spelling of the
// reference is this function's business.
//
// AN `email:` ENTRY IS THE THIRD SPELLING, and it is not a reference at all. It
// matches `verifiedEmail`, which the caller fills in only when the identity
// provider STATED that the address is verified. The namespace prefix strip
// below therefore cannot reach it: identity.MatchesInitialGlobalAdmin compares
// an `email:` entry against the address and against nothing else, so a subject
// spelled `email:someone@corp.com` collects no grant.
func matchesInitialGlobalAdmin(admins []string, providerRef, verifiedEmail string) bool {
	if identity.MatchesInitialGlobalAdmin(admins, providerRef, verifiedEmail) {
		return true
	}
	for _, prefix := range []string{OIDCProviderRefPrefix, SAMLProviderRefPrefix} {
		if bare, found := strings.CutPrefix(providerRef, prefix); found {
			return identity.MatchesInitialGlobalAdmin(admins, bare, verifiedEmail)
		}
	}
	return false
}

// applyFirstLoginGrants is what a login gets beyond an account row, and it runs
// in the SAME transaction that resolved the account: an interrupted login
// leaves neither a half-granted administrator nor an orphan credential.
//
// It carries the two things a freshly installed deployment cannot otherwise
// obtain without hand-written SQL:
//
//  1. the `initial_global_admins` role grant, which internal/api/
//     production_router.go's mounted browser plane never applied, because that
//     policy lives on the browserauth plane that OIDC displaces; and
//  2. an actor personal access token, without which the person's first chat
//     turn fails inside the runtime (see identityrepo.EnsureActorPAT).
//
// Both effects are idempotent, so every subsequent login runs them as two
// cheap reads that change nothing.
//
// `verifiedEmail` carries the address ONLY when the identity provider stated
// that it is verified. Pass "" when there is no such statement. See
// identity.MatchesInitialGlobalAdmin.
func applyFirstLoginGrants(
	ctx context.Context,
	tx pgx.Tx,
	userID int,
	providerRef string,
	verifiedEmail string,
	policy FirstLoginPolicy,
) error {
	if userID <= 0 || userID > math.MaxInt32 {
		return fmt.Errorf("first login grants: user id %d is invalid", userID)
	}
	queries := sqlcgen.New(tx)

	if matchesInitialGlobalAdmin(policy.InitialGlobalAdmins, providerRef, verifiedEmail) {
		if err := identityrepo.ApplyInitialAdministrationRole(
			ctx, queries, int32(userID),
			identity.InitialAdministrationMode, identity.InitialAdministrationRole,
		); err != nil {
			return err
		}
	}

	created, err := identityrepo.EnsureActorPAT(ctx, queries, int32(userID))
	if err != nil {
		return err
	}
	if created {
		slog.Info("issued an actor personal access token at first sign-in",
			"user_id", userID, "token_name", identityrepo.ActorPATName)
	}
	return nil
}
