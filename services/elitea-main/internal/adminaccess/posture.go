package adminaccess

// The access posture checks (ADR-0059 decision 8, "Posture"; story R2.14a,
// issue 2528).
//
// Roles, tokens and emergency access drift into unsafe states quietly. The
// posture list says, in one place, what is wrong (`fail`) and what is only
// worth knowing (`warn`, `info`). Only a `fail` that does not pass counts as
// a failure: a check that is true everywhere, or that never clears, teaches
// people to ignore the list. So every `fail` and `warn` check here passes on
// a well-kept deployment, and an `info` check is a fact, never a failure:
// super_admin_holders needs attention whenever anyone holds super_admin,
// which a working deployment does, and is counted only as a notice.
//
// This file is the registry. Every check has a stable id (the wire id, the
// text key's last part and the anchor of its docs entry), a severity and one
// read: one query or one configuration read (the presence of the seed
// record is read once per list), so the list stays cheap enough for a Home
// card with no cache. Later stories add their own checks here
// (R2.14b, the break-glass checks; R2.15, Form sign-in beside single
// sign-on); the order of postureChecks is the order of the list.
//
// A finding's facts are counts, role names and, for the checks that are
// about people, the people: the route that serves them is gated on
// access.roles.view, which is what may see who holds a role (decision 6).

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// PostureSeverity is how much a check matters when it does not pass.
type PostureSeverity string

// The severities.
const (
	PostureFail PostureSeverity = "fail"
	PostureWarn PostureSeverity = "warn"
	PostureInfo PostureSeverity = "info"
)

// The states of a check.
const (
	PostureStatePass      = "pass"
	PostureStateAttention = "attention"
)

// The ids of the checks of R2.14a.
const (
	PostureBuiltInNameCollision          = "builtin_name_collision"
	PostureBuiltInSetDrift               = "builtin_set_drift"
	PostureInheritTokens                 = "inherit_tokens"
	PostureCustomRoleLegacyPermission    = "custom_role_legacy_permission"
	PostureLegacySystemRoleHeld          = "legacy_system_role_held"
	PostureInitialGlobalAdminsConfigured = "initial_global_admins_configured"
	PostureSuperAdminHolders             = "super_admin_holders"
	PostureLegacySystemRoleExists        = "legacy_system_role_exists"
)

// posturePrefix is the prefix of every text key.
const posturePrefix = "admin.access.posture."

// PosturePerson is a person a finding names.
type PosturePerson struct {
	ID    int64
	Name  string
	Email string
}

// PostureFacts are what a finding is about. Count is the number of things
// the check found (roles, tokens, people, entries).
type PostureFacts struct {
	Count  int
	Roles  []string
	People []PosturePerson
}

// PostureFinding is the answer of one check.
type PostureFinding struct {
	Pass  bool
	Text  string
	Facts PostureFacts
}

// PostureSources is what the checks read.
type PostureSources struct {
	// DB is the platform database.
	DB Querier
	// InitialGlobalAdmins is the configured identity.initial_global_admins
	// list (or ELITEA_INITIAL_GLOBAL_ADMINS where it applies).
	InitialGlobalAdmins []string
	// GlobalAdministratorExists answers whether an unsuspended standing
	// holder of admin.auth.users.super_admin exists: the condition that ends
	// the list as a bootstrap (internal/infra/identityrepo).
	GlobalAdministratorExists func(ctx context.Context) (bool, error)
	// Now is the clock; nil is time.Now.
	Now func() time.Time

	// seed memoizes the presence of the seed record for one RunPosture.
	seed *seedMemo
}

type seedMemo struct {
	read    bool
	present bool
}

func (s PostureSources) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

// PostureCheck is one check of the registry.
type PostureCheck struct {
	ID       string
	Severity PostureSeverity
	Run      func(ctx context.Context, sources PostureSources) (PostureFinding, error)
}

// TextKey is the check's translation key.
func (c PostureCheck) TextKey() string { return posturePrefix + c.ID }

// PostureResult is a check with its finding.
type PostureResult struct {
	Check PostureCheck
	PostureFinding
}

// State is pass or attention.
func (r PostureResult) State() string {
	if r.Pass {
		return PostureStatePass
	}
	return PostureStateAttention
}

// Failed reports whether the result counts as a failure: a `fail` check that
// does not pass, and nothing else.
func (r PostureResult) Failed() bool { return r.Check.Severity == PostureFail && !r.Pass }

// postureChecks is the registry, in list order: fail, then warn, then info.
var postureChecks = []PostureCheck{
	{ID: PostureBuiltInNameCollision, Severity: PostureFail, Run: checkBuiltInNameCollision},
	{ID: PostureBuiltInSetDrift, Severity: PostureFail, Run: checkBuiltInSetDrift},
	{ID: PostureInheritTokens, Severity: PostureWarn, Run: checkInheritTokens},
	{ID: PostureCustomRoleLegacyPermission, Severity: PostureWarn, Run: checkCustomRoleLegacyPermission},
	{ID: PostureLegacySystemRoleHeld, Severity: PostureWarn, Run: checkLegacySystemRoleHeld},
	{ID: PostureInitialGlobalAdminsConfigured, Severity: PostureWarn, Run: checkInitialGlobalAdmins},
	{ID: PostureSuperAdminHolders, Severity: PostureInfo, Run: checkSuperAdminHolders},
	{ID: PostureLegacySystemRoleExists, Severity: PostureInfo, Run: checkLegacySystemRoleExists},
}

// PostureChecks returns the registry, in list order.
func PostureChecks() []PostureCheck { return slices.Clone(postureChecks) }

// ValidatePostureChecks checks a registry: a non-empty id of lower-case
// letters and underscores, unique, a known severity and a read.
func ValidatePostureChecks(checks []PostureCheck) error {
	seen := map[string]bool{}
	for _, check := range checks {
		if check.ID == "" || strings.Trim(check.ID, "abcdefghijklmnopqrstuvwxyz_") != "" {
			return fmt.Errorf("adminaccess: posture check %q: the id is not lower-case letters and underscores", check.ID)
		}
		if seen[check.ID] {
			return fmt.Errorf("adminaccess: posture check %q appears twice", check.ID)
		}
		seen[check.ID] = true
		switch check.Severity {
		case PostureFail, PostureWarn, PostureInfo:
		default:
			return fmt.Errorf("adminaccess: posture check %q has the unknown severity %q", check.ID, check.Severity)
		}
		if check.Run == nil {
			return fmt.Errorf("adminaccess: posture check %q reads nothing", check.ID)
		}
	}
	return nil
}

// RunPosture runs checks, in order. A check that cannot read its source
// fails the whole list: a posture list with a silent hole reads as correct.
func RunPosture(ctx context.Context, sources PostureSources, checks []PostureCheck) ([]PostureResult, error) {
	results := make([]PostureResult, 0, len(checks))
	sources.seed = &seedMemo{}
	for _, check := range checks {
		finding, err := check.Run(ctx, sources)
		if err != nil {
			return nil, fmt.Errorf("posture check %s: %w", check.ID, err)
		}
		if finding.Facts.Roles == nil {
			finding.Facts.Roles = []string{}
		}
		if finding.Facts.People == nil {
			finding.Facts.People = []PosturePerson{}
		}
		results = append(results, PostureResult{Check: check, PostureFinding: finding})
	}
	return results, nil
}

/* ── the checks ─────────────────────────────────────────────────────────── */

// seedRecordPresent reports whether the seed record (shared 0263) exists. A
// database without it cannot tell a built-in role from a custom one, and the
// two checks that need it then fail and say so, rather than pass.
func seedRecordPresent(ctx context.Context, s PostureSources) (bool, error) {
	if s.seed != nil && s.seed.read {
		return s.seed.present, nil
	}
	var present bool
	if err := s.DB.QueryRow(ctx, `SELECT to_regclass('elitea_auth.admin_role_seeds') IS NOT NULL`).Scan(&present); err != nil {
		return false, fmt.Errorf("look for the built-in role record: %w", err)
	}
	if s.seed != nil {
		s.seed.read, s.seed.present = true, present
	}
	return present, nil
}

// counted is "1 thing" or "n things", with the verb that agrees.
func counted(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

const seedRecordMissing = "The record of which roles are built-in (elitea_auth.admin_role_seeds) is missing, so " +
	"built-in roles cannot be told from custom ones: run the database migrations."

func builtInNames() []string {
	names := make([]string, 0, len(builtInRoles))
	for _, role := range builtInRoles {
		names = append(names, role.Name)
	}
	return names
}

func collectStrings(rows pgx.Rows, err error) ([]string, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// A role whose name is a built-in name and that has no seed row is a custom
// role that took the name before the upgrade: it is not built-in, is not
// locked and cannot be bound to a group (decision 3), and the console would
// show a built-in name with a custom set. security_auditor is included.
func checkBuiltInNameCollision(ctx context.Context, s PostureSources) (PostureFinding, error) {
	present, err := seedRecordPresent(ctx, s)
	if err != nil {
		return PostureFinding{}, err
	}
	if !present {
		return PostureFinding{Text: seedRecordMissing}, nil
	}
	roles, err := collectStrings(s.DB.Query(ctx, `
SELECT role.name
FROM public.auth_core__role AS role
LEFT JOIN elitea_auth.admin_role_seeds AS seed ON seed.role_id = role.id
WHERE role.mode = $1 AND role.name = ANY($2::text[]) AND seed.role_id IS NULL
ORDER BY 1`, ModeAdministration, builtInNames()))
	if err != nil {
		return PostureFinding{}, fmt.Errorf("read the roles of a built-in name: %w", err)
	}
	finding := PostureFinding{Pass: len(roles) == 0, Facts: PostureFacts{Count: len(roles), Roles: roles}}
	if finding.Pass {
		finding.Text = "Every role with a built-in name is recorded as built-in."
	} else {
		finding.Text = "These roles have the name of a built-in role but are not recorded as built-in: " +
			strings.Join(roles, ", ") + ". They are custom roles that took the name before the upgrade: they " +
			"are not locked, cannot be bound to a group, and look built-in in the console. Rename each one."
	}
	return finding, nil
}

// A seeded role with a declared set whose permissions differ from the
// registry's set, over the names of the permission table (an administrator
// role also holds legacy names outside it, which the registry does not
// declare). Roles marked as_found (editor, viewer) and unassignable (system)
// have no declared set and are never reported.
func checkBuiltInSetDrift(ctx context.Context, s PostureSources) (PostureFinding, error) {
	present, err := seedRecordPresent(ctx, s)
	if err != nil {
		return PostureFinding{}, err
	}
	if !present {
		return PostureFinding{Text: seedRecordMissing}, nil
	}
	rows, err := s.DB.Query(ctx, `
SELECT role.name,
       COALESCE(array_agg(grant_row.permission ORDER BY grant_row.permission)
                FILTER (WHERE grant_row.permission IS NOT NULL), '{}')
FROM elitea_auth.admin_role_seeds AS seed
JOIN public.auth_core__role AS role ON role.id = seed.role_id
LEFT JOIN public.auth_core__role_permission AS grant_row ON grant_row.role_id = role.id
WHERE seed.marker = $1 AND role.mode = $2
GROUP BY role.name
ORDER BY role.name`, string(MarkerDeclared), ModeAdministration)
	if err != nil {
		return PostureFinding{}, fmt.Errorf("read the sets of the built-in roles: %w", err)
	}
	type held struct {
		name string
		set  []string
	}
	sets, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (held, error) {
		var h held
		err := row.Scan(&h.name, &h.set)
		return h, err
	})
	if err != nil {
		return PostureFinding{}, fmt.Errorf("read the sets of the built-in roles: %w", err)
	}
	table := Names()
	var drifted []string
	for _, h := range sets {
		role, ok := LookupBuiltInRole(h.name)
		if !ok || !role.DeclaresSet() || role.Marker != MarkerDeclared {
			continue
		}
		want := role.Set()
		for _, name := range table {
			if slices.Contains(h.set, name) != slices.Contains(want, name) {
				drifted = append(drifted, h.name)
				break
			}
		}
	}
	finding := PostureFinding{Pass: len(drifted) == 0, Facts: PostureFacts{Count: len(drifted), Roles: drifted}}
	if finding.Pass {
		finding.Text = "Every built-in role holds exactly the set Elitea defines for it."
		return finding, nil
	}
	commands := make([]string, 0, len(drifted))
	for _, role := range drifted {
		commands = append(commands, "`elitea-main admin-role reset "+role+"`")
	}
	finding.Text = "These built-in roles hold a set that differs from the one Elitea defines: " +
		strings.Join(drifted, ", ") + ". A built-in set is locked in the console; put it back from the main " +
		"workload with " + strings.Join(commands, ", ") + "."
	return finding, nil
}

// Personal access tokens with `inherit` administration access act with their
// owner's whole administration set and end on the sunset date (decision 7).
// An expired token is not counted.
func checkInheritTokens(ctx context.Context, s PostureSources) (PostureFinding, error) {
	var count int
	if err := s.DB.QueryRow(ctx, `
SELECT count(*) FROM public.auth_core__token
WHERE uuid IS NOT NULL AND admin_access = $1
  AND (expires IS NULL OR expires > ($2::timestamptz AT TIME ZONE 'UTC'))`,
		TokenAccessInherit, s.now().UTC()).Scan(&count); err != nil {
		return PostureFinding{}, fmt.Errorf("count the inherit tokens: %w", err)
	}
	finding := PostureFinding{Pass: count == 0, Facts: PostureFacts{Count: count}}
	if finding.Pass {
		finding.Text = "No personal access token has inherit administration access."
	} else {
		finding.Text = counted(count, "personal access token has", "personal access tokens have") +
			" inherit administration access: each acts with its owner's whole administration set. Give " +
			"each the scopes it needs, or none."
	}
	return finding, nil
}

// A custom role that holds a legacy permission (runtime.plugins or
// configuration.governance, the registry's `legacy` flag). It is about the
// permissions the role holds, not about its name.
func checkCustomRoleLegacyPermission(ctx context.Context, s PostureSources) (PostureFinding, error) {
	var legacy []string
	for _, row := range Registry().Rows() {
		if row.Legacy {
			legacy = append(legacy, row.Name)
		}
	}
	present, err := seedRecordPresent(ctx, s)
	if err != nil {
		return PostureFinding{}, err
	}
	if !present {
		return PostureFinding{Text: seedRecordMissing}, nil
	}
	roles, err := collectStrings(s.DB.Query(ctx, `
SELECT DISTINCT role.name
FROM public.auth_core__role AS role
JOIN public.auth_core__role_permission AS grant_row ON grant_row.role_id = role.id
WHERE role.mode = $1 AND grant_row.permission = ANY($2::text[])
  AND NOT EXISTS (SELECT 1 FROM elitea_auth.admin_role_seeds AS seed WHERE seed.role_id = role.id)
ORDER BY 1`, ModeAdministration, legacy))
	if err != nil {
		return PostureFinding{}, fmt.Errorf("read the custom roles with a legacy permission: %w", err)
	}
	finding := PostureFinding{Pass: len(roles) == 0, Facts: PostureFacts{Count: len(roles), Roles: roles}}
	if finding.Pass {
		finding.Text = "No custom role holds a legacy permission."
	} else {
		finding.Text = "These custom roles hold a legacy permission (" + strings.Join(legacy, " or ") +
			"), which ends on the sunset date: " + strings.Join(roles, ", ") + ". The check is about the " +
			"permissions the role holds, not its name. Replace it with the narrow permissions the role needs."
	}
	return finding, nil
}

// readPeople reads the unsuspended holders of the administration role name.
func readPeople(ctx context.Context, db Querier, role string) ([]PosturePerson, error) {
	rows, err := db.Query(ctx, `
SELECT DISTINCT account.id, COALESCE(account.name, ''), COALESCE(account.email, '')
FROM public.auth_core__user_role AS assignment
JOIN public.auth_core__role AS role ON role.id = assignment.role_id AND role.mode = $1 AND role.name = $2
JOIN public.auth_core__user AS account ON account.id = assignment.user_id AND account.suspended IS NOT TRUE
ORDER BY account.id`, ModeAdministration, role)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (PosturePerson, error) {
		var p PosturePerson
		err := row.Scan(&p.ID, &p.Name, &p.Email)
		return p, err
	})
}

func names(people []PosturePerson) string {
	out := make([]string, 0, len(people))
	for _, p := range people {
		switch {
		case p.Name != "" && p.Email != "":
			out = append(out, p.Name+" <"+p.Email+">")
		case p.Email != "":
			out = append(out, p.Email)
		default:
			out = append(out, p.Name)
		}
	}
	return strings.Join(out, ", ")
}

// A person who holds the legacy administration role `system` (a
// legacy-backed database): the role is never assigned (decision 3).
func checkLegacySystemRoleHeld(ctx context.Context, s PostureSources) (PostureFinding, error) {
	people, err := readPeople(ctx, s.DB, RoleSystem)
	if err != nil {
		return PostureFinding{}, fmt.Errorf("read the holders of %s: %w", RoleSystem, err)
	}
	finding := PostureFinding{Pass: len(people) == 0, Facts: PostureFacts{Count: len(people), People: people}}
	if finding.Pass {
		finding.Text = "Nobody holds the legacy role system."
	} else {
		finding.Text = counted(len(people), "person holds", "people hold") +
			" the legacy role system, which is never assigned: " + names(people) +
			". Give each a built-in role instead."
	}
	return finding, nil
}

// identity.initial_global_admins is a bootstrap: it applies only while no
// global administrator exists, and every entry is skipped once one does
// (story R0.12). A list left configured after that is a standing door.
func checkInitialGlobalAdmins(ctx context.Context, s PostureSources) (PostureFinding, error) {
	entries := len(s.InitialGlobalAdmins)
	finding := PostureFinding{Pass: true, Facts: PostureFacts{Count: entries}}
	if entries > 0 {
		if s.GlobalAdministratorExists == nil {
			return PostureFinding{}, errors.New("no reader for the global administrators")
		}
		exists, err := s.GlobalAdministratorExists(ctx)
		if err != nil {
			return PostureFinding{}, err
		}
		finding.Pass = !exists
	}
	switch {
	case entries == 0:
		finding.Text = "identity.initial_global_admins is not configured."
	case finding.Pass:
		finding.Text = "identity.initial_global_admins is configured and no global administrator exists yet: " +
			"the first sign-in of a listed person makes them one."
	default:
		finding.Text = "identity.initial_global_admins still lists " + counted(entries, "entry", "entries") +
			" while a global administrator exists, so every entry is ignored at sign-in. Remove the list " +
			"from the configuration."
	}
	return finding, nil
}

// The standing holders of the role named super_admin: that name passes the
// project access check of every project (infra/db/projectaccess), so each
// is cross-project access by name. Worth knowing, never a failure.
func checkSuperAdminHolders(ctx context.Context, s PostureSources) (PostureFinding, error) {
	people, err := readPeople(ctx, s.DB, RoleSuperAdmin)
	if err != nil {
		return PostureFinding{}, fmt.Errorf("read the holders of %s: %w", RoleSuperAdmin, err)
	}
	finding := PostureFinding{Pass: len(people) == 0, Facts: PostureFacts{Count: len(people), People: people}}
	finding.Text = counted(len(people), "person holds", "people hold") +
		" super_admin as a standing role. The role reaches every project by its name."
	return finding, nil
}

// The legacy administration role `system` exists (a legacy-backed
// database). Worth knowing, never a failure.
func checkLegacySystemRoleExists(ctx context.Context, s PostureSources) (PostureFinding, error) {
	var exists bool
	if err := s.DB.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM public.auth_core__role WHERE mode = $1 AND name = $2)`,
		ModeAdministration, RoleSystem).Scan(&exists); err != nil {
		return PostureFinding{}, fmt.Errorf("look for the role %s: %w", RoleSystem, err)
	}
	finding := PostureFinding{Pass: !exists}
	if exists {
		finding.Facts = PostureFacts{Count: 1, Roles: []string{RoleSystem}}
		finding.Text = "The legacy role system exists: this database was migrated from the legacy platform. " +
			"It is locked and never assigned."
	} else {
		finding.Text = "The legacy role system does not exist."
	}
	return finding, nil
}
