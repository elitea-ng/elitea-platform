package main

// `elitea-main admin-role`: administrative roles from the command line
// (ADR-0059 decision 3 and decision 7; story R0.12a, issue 2501).
//
// Three situations have no path through the API by design: the first
// administrator of a deployment outside identity.initial_global_admins, a role
// for a person when no second administrator can assign it (nobody changes
// their own roles), and overriding the last-administrator rule. A built-in set
// that was edited before the upgrade, or damaged, needs a repair the API
// refuses too (locked sets). The control for all of them is access to the
// workload, as for `oidc-relink`: the command ships in the elitea-main image
// and runs where the service runs (kubectl exec into the main workload, or a
// one-off Job from the same image and Secret). It reads DATABASE_URL and talks
// to the database directly; it is not reachable over HTTP or gRPC.
//
// Usage:
//
//	elitea-main admin-role list
//	elitea-main admin-role grant  USER ROLE [--reason TEXT]
//	elitea-main admin-role revoke USER ROLE [--reason TEXT] [--override-last-administrator]
//	elitea-main admin-role reset  ROLE [--override-last-administrator]
//
// USER is a numeric user id or an e-mail address. ROLE is an administration
// role name. Every write takes adminaccess.LockRoleWrites first, like every
// other role write. grant and revoke write one grant history row (source
// `operator`) and one security audit event per change; reset writes one
// `critical` event with the sets before and after. Output is one line per
// change, with the history row id and the event id, and never a secret.
//
// Exit codes: 0 done (or nothing to change), 1 failed and nothing changed
// (database unreachable, a write failed), 2 invalid invocation (bad
// arguments, unknown or ambiguous user, unknown role), 3 refused by a rule
// (the last administrator, a role that is never assigned, a reset of a role
// with no declared set, a binary older than the database).

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess/rolegrants"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

const (
	adminRoleCommand = "admin-role"
	adminRoleTimeout = 2 * time.Minute
	// adminRoleAuditSource is the CloudEvents source of the command's events.
	adminRoleAuditSource = "elitea-main/admin-role"
	// adminRolePrincipal is the actor of the command's events: a person with
	// access to the workload, whom the platform cannot name.
	adminRolePrincipal = "operator"
	maxAdminRoleReason = 500
)

// The exit codes.
const (
	adminRoleExitOK      = 0
	adminRoleExitFailed  = 1
	adminRoleExitUsage   = 2
	adminRoleExitRefused = 3
)

// The sub-commands.
const (
	adminRoleList   = "list"
	adminRoleGrant  = "grant"
	adminRoleRevoke = "revoke"
	adminRoleReset  = "reset"
)

type adminRoleRequest struct {
	sub    string
	user   string
	role   string
	reason string
	// override is --override-last-administrator (revoke and reset).
	override bool
}

// adminRoleError carries the exit code of a refusal or an invalid argument
// found against the database. Every such error rolls the transaction back.
type adminRoleError struct {
	code    int
	message string
}

func (e *adminRoleError) Error() string { return e.message }

func usageError(format string, args ...any) error {
	return &adminRoleError{code: adminRoleExitUsage, message: fmt.Sprintf(format, args...)}
}

func refusedError(format string, args ...any) error {
	return &adminRoleError{code: adminRoleExitRefused, message: fmt.Sprintf(format, args...)}
}

func runAdminRole(ctx context.Context, args []string, lookup func(string) (string, bool), stdout, stderr io.Writer) int {
	return runAdminRoleWith(ctx, args, lookup, stdout, stderr, pgxpool.New)
}

func adminRoleUsage(w io.Writer) {
	_, _ = fmt.Fprintf(w, `usage:
  elitea-main %[1]s list
  elitea-main %[1]s grant  USER ROLE [--reason TEXT]
  elitea-main %[1]s revoke USER ROLE [--reason TEXT] [--override-last-administrator]
  elitea-main %[1]s reset  ROLE [--override-last-administrator]
USER is a numeric user id or an e-mail address; ROLE an administration role.
Reads DATABASE_URL. Exit codes: 0 done, 1 failed (nothing changed),
2 invalid invocation, 3 refused by a rule.
`, adminRoleCommand)
}

// parseAdminRoleArgs validates the invocation. Flags may come before, between
// or after the positional arguments. It returns ok=false after it has printed
// the reason.
func parseAdminRoleArgs(args []string, stderr io.Writer) (adminRoleRequest, bool) {
	if len(args) == 0 {
		adminRoleUsage(stderr)
		return adminRoleRequest{}, false
	}
	req := adminRoleRequest{sub: args[0]}
	want := map[string]int{adminRoleList: 0, adminRoleGrant: 2, adminRoleRevoke: 2, adminRoleReset: 1}
	positional, known := want[req.sub]
	if !known {
		_, _ = fmt.Fprintf(stderr, "unknown sub-command %q\n", req.sub)
		adminRoleUsage(stderr)
		return adminRoleRequest{}, false
	}

	flags := flag.NewFlagSet(adminRoleCommand+" "+req.sub, flag.ContinueOnError)
	flags.SetOutput(stderr)
	reason := flags.String("reason", "", "grant and revoke: why, recorded in the grant history")
	override := flags.Bool("override-last-administrator", false,
		"revoke and reset: go ahead even when the change removes the last administrator")
	flags.Usage = func() { adminRoleUsage(stderr) }

	var values []string
	rest := args[1:]
	for {
		before := len(rest)
		if err := flags.Parse(rest); err != nil {
			return adminRoleRequest{}, false
		}
		if flags.NArg() == 0 {
			break
		}
		// `--` ends the flags: what follows is positional, as with flag.Parse.
		consumed := rest[:before-flags.NArg()]
		if len(consumed) > 0 && consumed[len(consumed)-1] == "--" {
			values = append(values, flags.Args()...)
			break
		}
		values = append(values, flags.Arg(0))
		rest = flags.Args()[1:]
	}
	if len(values) != positional {
		_, _ = fmt.Fprintf(stderr, "%s takes %d argument(s), got %d\n", req.sub, positional, len(values))
		adminRoleUsage(stderr)
		return adminRoleRequest{}, false
	}
	req.reason, req.override = strings.TrimSpace(*reason), *override
	switch req.sub {
	case adminRoleGrant, adminRoleRevoke:
		req.user, req.role = strings.TrimSpace(values[0]), strings.TrimSpace(values[1])
	case adminRoleReset:
		req.role = strings.TrimSpace(values[0])
	}
	switch {
	case req.override && req.sub != adminRoleRevoke && req.sub != adminRoleReset:
		_, _ = fmt.Fprintln(stderr, "--override-last-administrator only applies to revoke and reset")
	case req.reason != "" && req.sub != adminRoleGrant && req.sub != adminRoleRevoke:
		_, _ = fmt.Fprintln(stderr, "--reason only applies to grant and revoke")
	case utf8.RuneCountInString(req.reason) > maxAdminRoleReason || strings.IndexFunc(req.reason, unicode.IsControl) >= 0:
		_, _ = fmt.Fprintf(stderr, "--reason is at most %d characters, on one line\n", maxAdminRoleReason)
	case (req.sub == adminRoleGrant || req.sub == adminRoleRevoke) && req.user == "":
		_, _ = fmt.Fprintln(stderr, "USER is required")
	case req.sub != adminRoleList && req.role == "":
		_, _ = fmt.Fprintln(stderr, "ROLE is required")
	default:
		return req, true
	}
	return adminRoleRequest{}, false
}

func runAdminRoleWith(
	ctx context.Context, args []string, lookup func(string) (string, bool), stdout, stderr io.Writer,
	connect oidcRelinkConnect,
) int {
	req, ok := parseAdminRoleArgs(args, stderr)
	if !ok {
		return adminRoleExitUsage
	}
	dsn, _ := lookup("DATABASE_URL")
	if strings.TrimSpace(dsn) == "" {
		_, _ = fmt.Fprintln(stderr, "DATABASE_URL is required")
		return adminRoleExitUsage
	}
	ctx, cancel := context.WithTimeout(ctx, adminRoleTimeout)
	defer cancel()
	pool, err := connect(ctx, dsn)
	if err != nil {
		// pgx's error can quote the DSN; say what failed, not with what.
		_, _ = fmt.Fprintln(stderr, "connect to the platform database (DATABASE_URL) failed")
		return adminRoleExitFailed
	}
	defer pool.Close()
	// pgxpool.New does not connect; an unreachable database would otherwise
	// fail on the first query, with an error that names the host and user.
	if err := pool.Ping(ctx); err != nil {
		_, _ = fmt.Fprintln(stderr, "connect to the platform database (DATABASE_URL) failed")
		return adminRoleExitFailed
	}

	var lines []string
	switch req.sub {
	case adminRoleList:
		lines, err = adminRoleListHolders(ctx, pool)
	default:
		err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			// The role-writes lock is the transaction's first statement, as
			// every role write takes it (adminaccess.HoldRoleWrites).
			if err := adminaccess.LockRoleWrites(ctx, tx); err != nil {
				return err
			}
			var err error
			switch req.sub {
			case adminRoleGrant:
				lines, err = adminRoleGrantTx(ctx, tx, req)
			case adminRoleRevoke:
				lines, err = adminRoleRevokeTx(ctx, tx, req)
			case adminRoleReset:
				lines, err = adminRoleResetTx(ctx, tx, req)
			}
			return err
		})
	}
	var typed *adminRoleError
	switch {
	case err == nil:
	case errors.As(err, &typed):
		_, _ = fmt.Fprintf(stderr, "%s %s: %s; nothing was changed\n", adminRoleCommand, req.sub, typed.message)
		return typed.code
	default:
		_, _ = fmt.Fprintf(stderr, "%s %s failed; nothing was changed: %v\n", adminRoleCommand, req.sub, err)
		return adminRoleExitFailed
	}
	for _, line := range lines {
		_, _ = fmt.Fprintln(stdout, line)
	}
	return adminRoleExitOK
}

/* ── the account and the role ──────────────────────────────────────────── */

type adminRoleUser struct {
	id        int64
	email     string
	suspended bool
}

func (u adminRoleUser) label() string {
	label := fmt.Sprintf("user %d <%s>", u.id, u.email)
	if u.suspended {
		label += " (suspended)"
	}
	return label
}

// resolveAdminRoleUser finds the one account `user` names: a numeric id, or
// an e-mail address compared case-insensitively. Unknown and ambiguous are
// refused.
func resolveAdminRoleUser(ctx context.Context, q pgx.Tx, user string) (adminRoleUser, error) {
	query := `SELECT id, COALESCE(email, ''), suspended FROM public.auth_core__user WHERE lower(email) = lower($1) ORDER BY id LIMIT 2`
	var argument any = user
	if id, err := strconv.ParseInt(user, 10, 64); err == nil {
		if id <= 0 {
			return adminRoleUser{}, usageError("user id %d is not valid", id)
		}
		query, argument = `SELECT id, COALESCE(email, ''), suspended FROM public.auth_core__user WHERE id = $1`, id
	}
	rows, err := q.Query(ctx, query, argument)
	if err != nil {
		return adminRoleUser{}, fmt.Errorf("read the user: %w", err)
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (adminRoleUser, error) {
		var u adminRoleUser
		err := row.Scan(&u.id, &u.email, &u.suspended)
		return u, err
	})
	if err != nil {
		return adminRoleUser{}, fmt.Errorf("read the user: %w", err)
	}
	switch len(found) {
	case 0:
		return adminRoleUser{}, usageError("no user %q", user)
	case 1:
		return found[0], nil
	default:
		return adminRoleUser{}, usageError("%q matches more than one account; give the numeric user id", user)
	}
}

// requireAdministrationRole refuses a role the deployment does not define in
// the administration mode.
func requireAdministrationRole(ctx context.Context, q pgx.Tx, role string) error {
	var exists bool
	if err := q.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM public.auth_core__role WHERE mode = $1 AND name = $2)`,
		adminaccess.ModeAdministration, role).Scan(&exists); err != nil {
		return fmt.Errorf("read the role: %w", err)
	}
	if !exists {
		return usageError("no administration role %q", role)
	}
	return nil
}

// seedMarker is the role's built-in record marker, "" for a role with none.
func seedMarker(ctx context.Context, q pgx.Tx, role string) (adminaccess.SeedMarker, error) {
	var marker string
	err := q.QueryRow(ctx, `
SELECT seed.marker
FROM elitea_auth.admin_role_seeds AS seed
JOIN public.auth_core__role AS role ON role.id = seed.role_id
WHERE role.mode = $1 AND role.name = $2`, adminaccess.ModeAdministration, role).Scan(&marker)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the built-in record of the role: %w", err)
	}
	return adminaccess.SeedMarker(marker), nil
}

// historyRowID is the id of the history row the change just wrote, in this
// transaction (under the role-writes lock, so it is the newest of its kind).
func historyRowID(ctx context.Context, q pgx.Tx, userID int64, role, action string) (int64, error) {
	var id int64
	err := q.QueryRow(ctx, `
SELECT id FROM elitea_auth.admin_role_grants
WHERE user_id = $1 AND role_mode = $2 AND role_name = $3 AND action = $4 AND source = $5
ORDER BY id DESC LIMIT 1`,
		userID, adminaccess.ModeAdministration, role, action, rolegrants.SourceOperator).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("read the history row: %w", err)
	}
	return id, nil
}

func operatorOrigin(req adminRoleRequest) rolegrants.Origin {
	return rolegrants.Origin{Source: rolegrants.SourceOperator, Reason: req.reason}
}

/* ── grant and revoke ──────────────────────────────────────────────────── */

func adminRoleGrantTx(ctx context.Context, tx pgx.Tx, req adminRoleRequest) ([]string, error) {
	user, err := resolveAdminRoleUser(ctx, tx, req.user)
	if err != nil {
		return nil, err
	}
	if err := requireAdministrationRole(ctx, tx, req.role); err != nil {
		return nil, err
	}
	marker, err := seedMarker(ctx, tx, req.role)
	if err != nil {
		return nil, err
	}
	if marker == adminaccess.MarkerUnassignable || (marker == "" && req.role == adminaccess.RoleSystem) {
		return nil, refusedError("%q is a locked built-in role that is never assigned", req.role)
	}
	granted, err := rolegrants.Grant(ctx, tx, user.id, adminaccess.ModeAdministration, req.role, operatorOrigin(req))
	if errors.Is(err, rolegrants.ErrReservedAccount) {
		return nil, refusedError("%s is the reserved break-glass account, which holds no role", user.label())
	}
	if err != nil {
		return nil, err
	}
	if !granted {
		return []string{fmt.Sprintf("%s already holds %s; nothing to change", user.label(), req.role)}, nil
	}
	historyID, err := historyRowID(ctx, tx, user.id, req.role, rolegrants.ActionGranted)
	if err != nil {
		return nil, err
	}
	// R1.5 (issue 2509) lists this command as a producer of
	// admin.access.changed; AppendAccessEvent is the one call site it switches.
	record, err := adminaccess.AppendAccessEvent(ctx, tx, adminRoleAuditSource, adminaccess.AccessEvent{
		Action: adminaccess.AccessActionRoleAssigned, Source: rolegrants.SourceOperator,
		PrincipalRef: adminRolePrincipal, SubjectUserID: user.id, Role: req.role,
	})
	if err != nil {
		return nil, err
	}
	lines := []string{fmt.Sprintf("granted %s to %s; history row %d; security audit event %s",
		req.role, user.label(), historyID, record.EventID)}
	if user.suspended {
		lines = append(lines, "note: the account is suspended; the role takes effect when it is unsuspended")
	}
	return lines, nil
}

func adminRoleRevokeTx(ctx context.Context, tx pgx.Tx, req adminRoleRequest) ([]string, error) {
	user, err := resolveAdminRoleUser(ctx, tx, req.user)
	if err != nil {
		return nil, err
	}
	if err := requireAdministrationRole(ctx, tx, req.role); err != nil {
		return nil, err
	}
	guard, err := adminaccess.BeginLastAdministratorGuard(ctx, tx)
	if err != nil {
		return nil, err
	}
	removed, err := rolegrants.Revoke(ctx, tx, user.id, adminaccess.ModeAdministration, req.role, operatorOrigin(req))
	if err != nil {
		return nil, err
	}
	if !removed {
		return []string{fmt.Sprintf("%s does not hold %s; nothing to change", user.label(), req.role)}, nil
	}
	refusal, err := guard.Check(ctx, tx)
	if err != nil {
		return nil, err
	}
	severity, reasons := adminaccess.SeverityNotice, []string(nil)
	if refusal != nil {
		if !req.override {
			return nil, refusedError("%s is the last %s; grant a successor first "+
				"(%s USER ROLE), or repeat with --override-last-administrator",
				user.label(), lostLevel(ctx, tx), adminaccess.SuccessorCommand)
		}
		severity, reasons = adminaccess.SeverityCritical, []string{"override:last_administrator"}
	}
	historyID, err := historyRowID(ctx, tx, user.id, req.role, rolegrants.ActionRevoked)
	if err != nil {
		return nil, err
	}
	record, err := adminaccess.AppendAccessEvent(ctx, tx, adminRoleAuditSource, adminaccess.AccessEvent{
		Action: adminaccess.AccessActionRoleRemoved, Source: rolegrants.SourceOperator,
		PrincipalRef: adminRolePrincipal, SubjectUserID: user.id, Role: req.role,
		Severity: severity, Reasons: reasons,
	})
	if err != nil {
		return nil, err
	}
	lines := []string{fmt.Sprintf("revoked %s from %s; history row %d; security audit event %s",
		req.role, user.label(), historyID, record.EventID)}
	if refusal != nil {
		lines = append(lines, "warning: the deployment now has no "+lostLevel(ctx, tx)+" (--override-last-administrator)")
	}
	return lines, nil
}

// lostLevel names what a change the rule refused removes, measured in its own
// transaction after the change: the last administrator at all, or the last
// holder of the permission while an `admin` remains.
func lostLevel(ctx context.Context, tx pgx.Tx) string {
	after, err := adminaccess.CountAdministrators(ctx, tx)
	if err != nil || !after.Protected() {
		return "administrator"
	}
	return "holder of " + adminaccess.SuperAdminPermission + " (global administrator; an " +
		adminaccess.RoleAdmin + " remains)"
}

/* ── reset ─────────────────────────────────────────────────────────────── */

// requireCurrentBinary refuses a reset by a binary older than the database: its
// registry could lack a permission a newer release granted, and the reset
// would strip it.
func requireCurrentBinary(ctx context.Context, q pgx.Tx) error {
	manifest, err := migrate.LoadManifest(platformmigrations.Files, migrate.ScopeShared)
	if err != nil {
		return fmt.Errorf("read the embedded migrations: %w", err)
	}
	embedded := migrate.Head(manifest)
	var applied int64
	if err := q.QueryRow(ctx, `
SELECT COALESCE(MAX(version), 0) FROM elitea_runtime.schema_migrations WHERE target_kind = 'shared'`).Scan(&applied); err != nil {
		return fmt.Errorf("read the migration ledger: %w", err)
	}
	if applied > embedded {
		return refusedError("the database is at shared migration %d and this binary knows %d; "+
			"run the command from the image of the release that migrated the database", applied, embedded)
	}
	return nil
}

func adminRoleResetTx(ctx context.Context, tx pgx.Tx, req adminRoleRequest) ([]string, error) {
	var roleID int
	err := tx.QueryRow(ctx, `SELECT id FROM public.auth_core__role WHERE mode = $1 AND name = $2`,
		adminaccess.ModeAdministration, req.role).Scan(&roleID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, usageError("no administration role %q", req.role)
	}
	if err != nil {
		return nil, fmt.Errorf("read the role: %w", err)
	}
	marker, err := seedMarker(ctx, tx, req.role)
	if err != nil {
		return nil, err
	}
	builtIn, known := adminaccess.LookupBuiltInRole(req.role)
	switch {
	case marker == "":
		return nil, refusedError("%q is not a built-in role on this deployment (it has no built-in record)", req.role)
	case marker != adminaccess.MarkerDeclared || !known || !builtIn.DeclaresSet():
		return nil, refusedError("%q is marked %s: Elitea declares no set for it, so there is nothing to reset to",
			req.role, marker)
	}
	if err := requireCurrentBinary(ctx, tx); err != nil {
		return nil, err
	}
	// A damaged set can hold admin.auth.users.super_admin (a legacy edit of
	// `admin`): taking it away can remove the last administrator, so the
	// rule applies to a reset too.
	guard, err := adminaccess.BeginLastAdministratorGuard(ctx, tx)
	if err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
SELECT permission FROM public.auth_core__role_permission
WHERE role_id = $1 AND permission IS NOT NULL ORDER BY 1
FOR UPDATE`, roleID)
	if err != nil {
		return nil, fmt.Errorf("read the role's set: %w", err)
	}
	before, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the role's set: %w", err)
	}
	after := builtIn.Set()
	var added, removed []string
	for _, name := range after {
		if !slices.Contains(before, name) {
			added = append(added, name)
		}
	}
	for _, name := range before {
		if !slices.Contains(after, name) {
			removed = append(removed, name)
		}
	}
	if len(added) == 0 && len(removed) == 0 {
		return []string{fmt.Sprintf("%s already holds exactly its declared set (%d permissions); nothing to change",
			req.role, len(after))}, nil
	}
	if len(added) > 0 {
		if _, err := tx.Exec(ctx, `
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT $1, granted.permission FROM unnest($2::text[]) AS granted(permission)
ON CONFLICT (role_id, permission) DO NOTHING`, roleID, added); err != nil {
			return nil, fmt.Errorf("add the missing permissions: %w", err)
		}
	}
	if len(removed) > 0 {
		if _, err := tx.Exec(ctx, `
DELETE FROM public.auth_core__role_permission WHERE role_id = $1 AND permission = ANY($2::text[])`,
			roleID, removed); err != nil {
			return nil, fmt.Errorf("remove the extra permissions: %w", err)
		}
	}
	refusal, err := guard.Check(ctx, tx)
	if err != nil {
		return nil, err
	}
	var reasons []string
	if refusal != nil {
		if !req.override {
			return nil, refusedError("the reset takes a permission that makes the last %s; grant a successor first, "+
				"or repeat with --override-last-administrator", lostLevel(ctx, tx))
		}
		reasons = []string{"override:last_administrator"}
	}
	record, err := adminaccess.AppendAccessEvent(ctx, tx, adminRoleAuditSource, adminaccess.AccessEvent{
		Action: adminaccess.AccessActionRoleSetReset, Source: rolegrants.SourceOperator,
		PrincipalRef: adminRolePrincipal, Role: req.role,
		Before: before, After: after, Severity: adminaccess.SeverityCritical, Reasons: reasons,
	})
	if err != nil {
		return nil, err
	}
	lines := make([]string, 0, len(added)+len(removed)+1)
	for _, name := range added {
		lines = append(lines, fmt.Sprintf("%s: added %s", req.role, name))
	}
	for _, name := range removed {
		lines = append(lines, fmt.Sprintf("%s: removed %s", req.role, name))
	}
	lines = append(lines, fmt.Sprintf("reset %s to its declared set (%d permissions); security audit event %s (critical)",
		req.role, len(after), record.EventID))
	return lines, nil
}

/* ── list ──────────────────────────────────────────────────────────────── */

// adminRoleListHolders lists every administration role with its built-in
// marker and its holders, and the administrators the last-administrator rule
// counts.
func adminRoleListHolders(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, `
SELECT role.name,
       COALESCE(seed.marker, 'custom'),
       COALESCE(array_agg(account.id ORDER BY account.id) FILTER (WHERE account.id IS NOT NULL), '{}'),
       COALESCE(array_agg(COALESCE(account.email, '') ORDER BY account.id) FILTER (WHERE account.id IS NOT NULL), '{}'),
       COALESCE(array_agg(account.suspended ORDER BY account.id) FILTER (WHERE account.id IS NOT NULL), '{}')
FROM public.auth_core__role role
LEFT JOIN elitea_auth.admin_role_seeds seed ON seed.role_id = role.id
LEFT JOIN public.auth_core__user_role assignment ON assignment.role_id = role.id
LEFT JOIN public.auth_core__user account ON account.id = assignment.user_id
WHERE role.mode = $1
GROUP BY role.name, seed.marker
ORDER BY role.name`, adminaccess.ModeAdministration)
	if err != nil {
		return nil, fmt.Errorf("read the roles: %w", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var (
			name, marker string
			ids          []int64
			emails       []string
			suspended    []bool
		)
		if err := rows.Scan(&name, &marker, &ids, &emails, &suspended); err != nil {
			return nil, fmt.Errorf("read the roles: %w", err)
		}
		lines = append(lines, fmt.Sprintf("%s [%s]: %d holder(s)", name, marker, len(ids)))
		for i, id := range ids {
			holder := adminRoleUser{id: id, email: emails[i], suspended: suspended[i]}
			lines = append(lines, "  "+holder.label())
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the roles: %w", err)
	}
	counts, err := adminaccess.CountAdministrators(ctx, pool)
	if err != nil {
		return nil, err
	}
	summary := fmt.Sprintf("administrators: %d active holder(s) of %s, %d active holder(s) of %s",
		counts.SuperAdmins, adminaccess.SuperAdminPermission, counts.Admins, adminaccess.RoleAdmin)
	if !counts.Protected() {
		summary += "; the deployment has no administrator (grant one with " + adminaccess.SuccessorCommand + ")"
	}
	return append(lines, summary), nil
}
