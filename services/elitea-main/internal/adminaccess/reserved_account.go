package adminaccess

// The reserved break-glass account (ADR-0059 decision 5, "The reserved
// break-glass account"; story R2.6b, issue 2599).
//
// An envelope in a safe cannot name the person who will open it, so an
// UNBOUND sealed credential signs in as one reserved account,
// ReservedBreakGlassEmail. The account exists only for that:
//
//   - it is created by the first `elitea-main break-glass issue --unbound`
//     (EnsureReservedBreakGlassAccount), never by a migration: a deployment
//     that issues no unbound credential has no such row;
//   - it holds no role: every assignment path refuses it (rolegrants.Grant,
//     the admin Users and modes routes, the project member routes);
//   - it cannot create a token, and no sign-in plane but the sealed one
//     reaches it: single sign-on, SCIM and the Form plane refuse the reserved
//     domain `@centry.user`, and the single sign-on plane of internal/api/v2/auth
//     refuses this address by name;
//   - the Users listing never returns it; the Access page (R2.17, R3.9) is
//     the one place that shows it.
//
// The refusals are by ACCOUNT, not by session kind, so they hold without an
// elevation too.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ReservedBreakGlassEmail is the address of the reserved account. It is in the
// reserved system-identity domain `@centry.user`, which no person's sign-in
// may carry. rolegrants holds the same literal (it cannot import this
// package); reserved_account_test.go keeps the two equal.
const ReservedBreakGlassEmail = "break-glass@centry.user"

// reservedBreakGlassName is the display name the account is created with.
const reservedBreakGlassName = "Break-glass account"

// CodeReservedAccount is the stable code of a refusal that names the reserved
// account as the target of a role, a membership or a token.
const CodeReservedAccount = "reserved_account"

// ReservedAccountAuditAction is the admin audit row's action of the refusal.
const ReservedAccountAuditAction = "admin.reserved_account_refused"

// ErrReservedAccountUnusable: the reserved account exists but cannot carry an
// unbound credential (it is suspended, or it holds a role a write outside
// every guard gave it). Nothing is issued.
var ErrReservedAccountUnusable = errors.New("the reserved break-glass account is suspended or holds a role")

// IsReservedBreakGlassEmail reports whether an address is the reserved
// account's, in any case and with surrounding space.
func IsReservedBreakGlassEmail(email string) bool {
	return strings.EqualFold(strings.TrimSpace(email), ReservedBreakGlassEmail)
}

// ReservedAccountRefusal is the refusal of a role, a membership or a token for
// the reserved account.
func ReservedAccountRefusal() *Refusal {
	return &Refusal{
		Code: CodeReservedAccount,
		Message: ReservedBreakGlassEmail + " is the reserved break-glass account: it holds no role, " +
			"belongs to no project and creates no token",
	}
}

// IsReservedAccount reports whether the user id names the reserved account.
// An unknown id is not the reserved account.
func IsReservedAccount(ctx context.Context, q Querier, userID int64) (bool, error) {
	return AnyReservedAccount(ctx, q, []int64{userID})
}

// AnyReservedAccount reports whether one of the user ids names the reserved
// account.
func AnyReservedAccount(ctx context.Context, q Querier, userIDs []int64) (bool, error) {
	if len(userIDs) == 0 {
		return false, nil
	}
	var reserved bool
	if err := q.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM public.auth_core__user
    WHERE id = ANY($1::bigint[]) AND lower(email) = $2)`, userIDs, ReservedBreakGlassEmail).Scan(&reserved); err != nil {
		return false, fmt.Errorf("read the reserved account: %w", err)
	}
	return reserved, nil
}

// EnsureReservedBreakGlassAccount answers the id of the reserved account in
// the caller's transaction, creating it when it is absent, and whether this
// call created it. A second call reuses the row.
//
// The row it creates has no role, no provider link and no membership. An
// existing row that is suspended, or that holds any role (central or
// project), is refused with ErrReservedAccountUnusable: an unbound credential
// is never issued for an account something else has changed.
func EnsureReservedBreakGlassAccount(ctx context.Context, tx pgx.Tx) (int64, bool, error) {
	if tx == nil {
		return 0, false, errors.New("reserved break-glass account: no transaction")
	}
	var id int64
	created := true
	err := tx.QueryRow(ctx, `
INSERT INTO public.auth_core__user (email, name)
VALUES ($1, $2)
ON CONFLICT (email) DO NOTHING
RETURNING id`, ReservedBreakGlassEmail, reservedBreakGlassName).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		created = false
		err = tx.QueryRow(ctx, `
SELECT id FROM public.auth_core__user WHERE lower(email) = $1
ORDER BY id LIMIT 1
FOR UPDATE`, ReservedBreakGlassEmail).Scan(&id)
	}
	if err != nil {
		return 0, false, fmt.Errorf("reserved break-glass account: %w", err)
	}
	var suspended, holdsRole bool
	if err := tx.QueryRow(ctx, `
SELECT owner.suspended,
       EXISTS (SELECT 1 FROM public.auth_core__user_role WHERE user_id = owner.id)
       OR EXISTS (SELECT 1 FROM public.auth_core__project_user_role WHERE user_id = owner.id)
FROM public.auth_core__user AS owner WHERE owner.id = $1`, id).Scan(&suspended, &holdsRole); err != nil {
		return 0, false, fmt.Errorf("reserved break-glass account: read its state: %w", err)
	}
	if suspended || holdsRole {
		return 0, false, ErrReservedAccountUnusable
	}
	return id, created, nil
}
