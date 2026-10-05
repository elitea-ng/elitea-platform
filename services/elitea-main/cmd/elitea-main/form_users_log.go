package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

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
