package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	v2auth "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/authcomposition"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/identityrepo"
)

// logFormUserConfiguration reports, at load, the Form users whose sign-in
// will be refused because their configuration names no usable address.
//
// No address is synthesized for them any more (they used to sign in as
// `<login>@centry.user`, the system-identity domain). A refused sign-in logs
// the cause too, but that can be days after the boot that loaded the file;
// this line is where an operator sees the problem first.
func logFormUserConfiguration(logger *slog.Logger, report authcomposition.FormUserReport) {
	if logger == nil || len(report.MisconfiguredLogins) == 0 {
		return
	}
	logger.Warn("Form users have no usable email address and will be refused at sign-in",
		"logins", report.MisconfiguredLogins,
		"misconfigured", len(report.MisconfiguredLogins),
		"configured", report.Configured,
		"fix", "set each user's top-level \"email\" (or attributes.email) in the Form users JSON; "+
			"addresses in the reserved @centry.user domain are refused")
}

// reservedDomainAccountsTimeout bounds the one boot-time read below; it must
// never hold the boot.
const reservedDomainAccountsTimeout = 5 * time.Second

// warnReservedDomainSignInAccounts reports person accounts that an earlier
// release created with a synthesized `<login>@centry.user` address. It changes
// nothing: which real address each person should have is the operator's call
// (docs/UPGRADING.md). A read failure — a database without the identity
// tables, for one — is logged at debug and ignored.
func warnReservedDomainSignInAccounts(ctx context.Context, logger *slog.Logger, pool *pgxpool.Pool) {
	if logger == nil || pool == nil {
		return
	}
	readCtx, cancel := context.WithTimeout(ctx, reservedDomainAccountsTimeout)
	defer cancel()
	found, err := identityrepo.ListReservedDomainSignInAccounts(readCtx, pool)
	if err != nil {
		logger.Debug("could not check for sign-in accounts in the reserved @centry.user domain", "error", err)
		return
	}
	logReservedDomainSignInAccounts(logger, found)
}

func logReservedDomainSignInAccounts(logger *slog.Logger, found identityrepo.ReservedDomainAccounts) {
	if logger == nil || found.Count == 0 {
		return
	}
	logger.Warn("sign-in accounts have a synthesized address in the reserved @centry.user domain",
		"accounts", found.Count,
		"provider_references", found.ProviderReferences,
		"listed", len(found.ProviderReferences),
		"effect", "the Users page, analytics, budgets and SCIM treat these people as platform accounts",
		"fix", "see docs/UPGRADING.md: give each person a real address; nothing is changed automatically")
}

// formSignInEnabledFromEnv reads ELITEA_FORM_LOGIN_ENABLED. Unset or empty is
// false: the local username/password sign-in is OFF unless an operator turns
// it on. Any other value must parse as a boolean, and a value that does not
// stops the boot — a typo must not silently decide whether passwords work.
//
// Keep "ELITEA_FORM_LOGIN_ENABLED" a literal inside the lookup call: the
// env-drift gate (services/elitea-llm-gateway/scripts/env-drift-check.sh)
// finds the name by that pattern.
func formSignInEnabledFromEnv(lookup func(string) (string, bool)) (bool, error) {
	if lookup == nil {
		return false, errors.New("environment lookup for ELITEA_FORM_LOGIN_ENABLED is required")
	}
	raw, _ := lookup("ELITEA_FORM_LOGIN_ENABLED")
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false, nil
	}
	enabled, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("ELITEA_FORM_LOGIN_ENABLED must be true or false, got %q", raw)
	}
	return enabled, nil
}

// logFormSignInDisabled says, once, that the configured Form users are not a
// sign-in path on this deployment.
func logFormSignInDisabled(logger *slog.Logger, report authcomposition.FormUserReport) {
	if logger == nil {
		return
	}
	if report.Configured == 0 {
		logger.Info("Form sign-in is disabled (ELITEA_FORM_LOGIN_ENABLED is not true)")
		return
	}
	logger.Warn("Form sign-in is disabled (ELITEA_FORM_LOGIN_ENABLED is not true); the configured Form users are ignored",
		"ignored_users", report.Configured)
}

// singleSignOnFirstLoginPolicy is the `initial_global_admins` list the OIDC
// and SAML planes apply. The authentication document's list wins; the
// environment (ELITEA_INITIAL_GLOBAL_ADMINS) fills it only when the document
// names none or there is no document.
//
// It deliberately takes nothing about Form sign-in. With Form sign-in off by
// default, this list is HOW a fresh install makes its first administrator:
// the first OIDC or SAML login whose subject (`oidc:<sub>`, `saml:<nameid>`)
// or verified address (`email:<address>`, OIDC only) is listed receives the
// administration role.
func singleSignOnFirstLoginPolicy(document v2auth.FirstLoginPolicy, environment func() []string) v2auth.FirstLoginPolicy {
	policy := v2auth.FirstLoginPolicy{InitialGlobalAdmins: append([]string(nil), document.InitialGlobalAdmins...)}
	if len(policy.InitialGlobalAdmins) == 0 && environment != nil {
		policy.InitialGlobalAdmins = environment()
	}
	return policy
}
