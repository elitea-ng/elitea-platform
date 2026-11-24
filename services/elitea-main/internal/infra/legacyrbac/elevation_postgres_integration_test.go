package legacyrbac_test

// The elevation union of the resolver, bound to one browser session (ADR-0059
// decision 5, "Bound to one session" and "Permissions, not a name"; story
// R2.1b, issue 2595), on the real schema: 001_initial.sql and the shared
// corpus, so the ledger, its trigger and the seeded admin and super_admin
// roles are the ones a deployment has. The elevations are written by the
// ledger itself (adminaccess.StartElevation) wherever the ledger can write
// them.
//
//   - the session the elevation is bound to resolves the standing set plus
//     the level's role set, and says which permissions came from the
//     elevation; the role names stay the standing ones;
//   - past its expiry it grants nothing, with no sweeper running (test clock);
//   - a second browser session of the same person resolves nothing from it;
//   - a token, a native session and a run token of the same person resolve
//     nothing from it, even when they claim the session's reference;
//   - a session without an elevation resolves exactly its role permissions,
//     and the developer mode is unchanged by an elevation;
//   - the standing-set call answers the same with and without an elevation.

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

// elevationTestStart is the start of every elevation of this file; the
// resolver's test clock is set relative to it.
var elevationTestStart = time.Date(2026, time.November, 2, 9, 0, 0, 0, time.UTC)

const (
	elevationTestDuration   = time.Hour
	elevationTestDeveloper  = "elevation.fixture.developer"
	elevationTestDevRole    = "elevation_fixture_developer"
	elevationTestFixtureTag = "r21b"
)

type elevationFixture struct {
	pool     *pgxpool.Pool
	resolver *legacyrbac.PostgresResolver
	// holder holds the standing role admin and a developer role.
	holder   int64
	approver int64
	// session is the holder's browser session the elevation is bound to,
	// other a second browser session of the holder.
	session, other int64
	// adminSet and superAdminSet are the administration grants of the two
	// seeded roles.
	adminSet, superAdminSet []string
}

func newElevationFixture(t *testing.T) elevationFixture {
	t.Helper()
	pool := newElevationLedgerDatabase(t)
	f := elevationFixture{pool: pool, resolver: legacyrbac.NewPostgresResolver(pool)}
	legacyrbac.SetClockForTest(f.resolver, func() time.Time { return elevationTestStart.Add(time.Minute) })
	f.holder = seedElevationPerson(t, pool, adminaccess.RoleAdmin)
	f.approver = seedElevationPerson(t, pool, adminaccess.RoleSuperAdmin)
	f.session = seedElevationSession(t, pool, f.holder)
	f.other = seedElevationSession(t, pool, f.holder)
	for _, statement := range []string{
		`INSERT INTO public.auth_core__role (name, mode) VALUES ('` + elevationTestDevRole + `', 'developer')`,
		`INSERT INTO public.auth_core__role_permission (role_id, permission)
			SELECT id, '` + elevationTestDeveloper + `' FROM public.auth_core__role
			WHERE name = '` + elevationTestDevRole + `' AND mode = 'developer'`,
		fmt.Sprintf(`INSERT INTO public.auth_core__user_role (user_id, role_id)
			SELECT %d, id FROM public.auth_core__role WHERE name = '%s' AND mode = 'developer'`,
			f.holder, elevationTestDevRole),
	} {
		if _, err := pool.Exec(t.Context(), statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	f.adminSet = elevationRoleSet(t, pool, adminaccess.RoleAdmin)
	f.superAdminSet = elevationRoleSet(t, pool, adminaccess.RoleSuperAdmin)
	if len(f.adminSet) == 0 || !slices.Contains(f.superAdminSet, adminaccess.SuperAdminPermission) ||
		slices.Contains(f.adminSet, adminaccess.SuperAdminPermission) {
		t.Fatalf("premise: admin holds %d grants, super_admin %v", len(f.adminSet), f.superAdminSet)
	}
	return f
}

// elevate starts a super_admin-level elevation of the holder bound to the
// session, approved by the approver, through the ledger.
func (f elevationFixture) elevate(t *testing.T) adminaccess.ElevationRecord {
	t.Helper()
	appender, err := securityaudit.NewPostgresAppender("elitea-main/r21b-test")
	if err != nil {
		t.Fatalf("appender: %v", err)
	}
	var elevation adminaccess.ElevationRecord
	if err := pgx.BeginFunc(t.Context(), f.pool, func(tx pgx.Tx) error {
		elevation, err = adminaccess.StartElevation(t.Context(), tx, appender, adminaccess.ElevationStart{
			UserID:            f.holder,
			BrowserSessionRef: f.session,
			Level:             adminaccess.ElevationLevelSuperAdmin,
			Path:              adminaccess.ElevationPathApproved,
			ReasonCode:        adminaccess.ElevationReasonIncident,
			Reason:            "R2.1b: the resolver union",
			ApproverUserID:    f.approver,
			At:                elevationTestStart,
			Duration:          elevationTestDuration,
		})
		return err
	}); err != nil {
		t.Fatalf("start the elevation: %v", err)
	}
	return elevation
}

func (f elevationFixture) browser(ref int64) auth.User {
	id := strconv.FormatInt(f.holder, 10)
	return auth.User{ID: id, UserID: id, AuthType: "session", CredentialKind: auth.CredentialKindSession,
		BrowserSessionRef: ref}
}

func (f elevationFixture) administration(t *testing.T, principal auth.User) auth.PermissionResolution {
	t.Helper()
	resolution, err := f.resolver.ResolvePermissions(t.Context(), principal, auth.PermissionModeAdministration, "")
	if err != nil {
		t.Fatalf("resolve the administration set of %+v: %v", principal, err)
	}
	return resolution
}

func (f elevationFixture) standing(t *testing.T, principal auth.User) auth.PermissionResolution {
	t.Helper()
	resolution, err := f.resolver.ResolveStandingAdministration(t.Context(), principal)
	if err != nil {
		t.Fatalf("resolve the standing set of %+v: %v", principal, err)
	}
	return resolution
}

// token seeds a token row of the holder and returns its id.
func (f elevationFixture) token(t *testing.T, access string, withUUID bool) string {
	t.Helper()
	var id int
	query := `INSERT INTO public.auth_core__token (uuid, user_id, name, admin_access)
		VALUES (gen_random_uuid()::text, $1, 'r21b', $2) RETURNING id`
	if !withUUID {
		query = `INSERT INTO public.auth_core__token (user_id, name, admin_access)
		VALUES ($1, 'r21b-device', $2) RETURNING id`
	}
	if err := f.pool.QueryRow(t.Context(), query, f.holder, access).Scan(&id); err != nil {
		t.Fatalf("seed a token of the holder: %v", err)
	}
	return strconv.Itoa(id)
}

// noElevation fails when a resolution carries anything from an elevation.
func noElevation(t *testing.T, what string, resolution auth.PermissionResolution, want []string) {
	t.Helper()
	if resolution.Elevated() || resolution.ElevationIDs != nil || resolution.ElevatedPermissions != nil ||
		resolution.ElevationID != "" {
		t.Fatalf("%s carries an elevation: ids %v, elevated %v", what, resolution.ElevationIDs, resolution.ElevatedPermissions)
	}
	if !slices.Equal(resolution.Permissions, want) {
		t.Fatalf("%s resolves %v, want %v", what, resolution.Permissions, want)
	}
}

func TestTheBoundSessionResolvesTheLevelsSetAndNamesNoRole(t *testing.T) {
	f := newElevationFixture(t)
	elevation := f.elevate(t)

	got := f.administration(t, f.browser(f.session))
	want, elevatedOnly := legacyrbac.UnionElevation(f.adminSet, f.superAdminSet)
	if !slices.Equal(got.Permissions, want) {
		t.Fatalf("the bound session resolves %v, want %v", got.Permissions, want)
	}
	if !slices.Equal(got.ElevatedPermissions, elevatedOnly) || !slices.Contains(got.ElevatedPermissions, adminaccess.SuperAdminPermission) {
		t.Fatalf("the elevated part is %v, want %v (with %s)", got.ElevatedPermissions, elevatedOnly, adminaccess.SuperAdminPermission)
	}
	if !slices.Equal(got.ElevationIDs, []string{elevation.ID.String()}) {
		t.Fatalf("the elevation ids are %v, want [%s]", got.ElevationIDs, elevation.ID)
	}
	// The resolver's elevation indicator, which the token-scopes rule
	// (R1.3a, CodeScopesElevated) refuses on, is set by the union.
	if got.ElevationID != elevation.ID.String() {
		t.Fatalf("the elevation indicator is %q, want %s", got.ElevationID, elevation.ID)
	}
	if !slices.Equal(got.StandingPermissions(), f.adminSet) {
		t.Fatalf("the standing part is %v, want the admin set", got.StandingPermissions())
	}
	// Permissions, not a name: the role names are the standing ones.
	if !slices.Equal(got.Roles, []string{adminaccess.RoleAdmin}) {
		t.Fatalf("the bound session acts with the roles %v, want [admin]", got.Roles)
	}
	// The admin audit row of the request records the elevation (shared
	// 0268); the standing-set call records none.
	audited, slot := audit.ContextWithAnnotationSlot(t.Context())
	if _, err := f.resolver.ResolvePermissions(audited, f.browser(f.session), auth.PermissionModeAdministration, ""); err != nil {
		t.Fatal(err)
	}
	if slot.ElevationID() != elevation.ID.String() {
		t.Fatalf("the audit slot records the elevation %q, want %s", slot.ElevationID(), elevation.ID)
	}
	standingAudited, standingSlot := audit.ContextWithAnnotationSlot(t.Context())
	if _, err := f.resolver.ResolveStandingAdministration(standingAudited, f.browser(f.session)); err != nil {
		t.Fatal(err)
	}
	if standingSlot.ElevationID() != "" {
		t.Fatalf("the standing-set call recorded the elevation %q", standingSlot.ElevationID())
	}
	// The membership resolver answers the central mode the same way.
	membership, err := f.resolver.ResolveMembershipPermissions(t.Context(), f.browser(f.session), auth.PermissionModeAdministration)
	if err != nil || !slices.Equal(membership.Permissions, want) {
		t.Fatalf("the membership resolution of the bound session is %v (%v), want %v", membership.Permissions, err, want)
	}
}

// Access is the predicate at the resolver's clock: no job runs here.
func TestAnElevationPastItsExpiryGrantsNothing(t *testing.T) {
	f := newElevationFixture(t)
	f.elevate(t)
	legacyrbac.SetClockForTest(f.resolver, func() time.Time { return elevationTestStart.Add(elevationTestDuration) })
	noElevation(t, "the bound session at its expiry", f.administration(t, f.browser(f.session)), f.adminSet)
	legacyrbac.SetClockForTest(f.resolver, func() time.Time { return elevationTestStart.Add(-time.Second) })
	noElevation(t, "the bound session before its start", f.administration(t, f.browser(f.session)), f.adminSet)
	var ended *time.Time
	if err := f.pool.QueryRow(t.Context(),
		`SELECT ended_at FROM elitea_auth.admin_elevations WHERE user_id = $1`, f.holder).Scan(&ended); err != nil {
		t.Fatal(err)
	}
	if ended != nil {
		t.Fatalf("premise: the elevation was ended (%v); the test is of the expiry alone", ended)
	}
}

func TestAnEndedElevationGrantsNothing(t *testing.T) {
	f := newElevationFixture(t)
	elevation := f.elevate(t)
	appender, err := securityaudit.NewPostgresAppender("elitea-main/r21b-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(t.Context(), f.pool, func(tx pgx.Tx) error {
		_, _, err := adminaccess.EndElevation(t.Context(), tx, appender, adminaccess.ElevationEnd{
			ID: elevation.ID, Reason: adminaccess.ElevationEndHolder, At: elevationTestStart.Add(30 * time.Second)})
		return err
	}); err != nil {
		t.Fatalf("end the elevation: %v", err)
	}
	noElevation(t, "the bound session after the end", f.administration(t, f.browser(f.session)), f.adminSet)
}

func TestASecondBrowserSessionOfTheSamePersonResolvesNothingFromTheElevation(t *testing.T) {
	f := newElevationFixture(t)
	f.elevate(t)
	noElevation(t, "the holder's second browser session", f.administration(t, f.browser(f.other)), f.adminSet)
	// A Form session of the holder, with its own reference from the same
	// sequence, is another session too.
	form := f.browser(seedElevationFormSession(t, f.pool, f.holder))
	form.CredentialKind = auth.CredentialKindForm
	noElevation(t, "the holder's Form session", f.administration(t, form), f.adminSet)
	// The legacy signed cookie has no reference at all.
	noElevation(t, "the holder's session without a row", f.administration(t, f.browser(0)), f.adminSet)
}

// Both the person and the reference must match: another person's principal
// that claims the bound session's reference resolves nothing from it.
func TestAnotherPersonClaimingTheBoundReferenceResolvesNothing(t *testing.T) {
	f := newElevationFixture(t)
	f.elevate(t)
	id := strconv.FormatInt(f.approver, 10)
	impostor := auth.User{ID: id, UserID: id, AuthType: "session", CredentialKind: auth.CredentialKindSession,
		BrowserSessionRef: f.session}
	noElevation(t, "another person on the bound reference", f.administration(t, impostor), f.superAdminSet)
}

func TestATokenOfThePersonResolvesNothingFromTheElevation(t *testing.T) {
	f := newElevationFixture(t)
	f.elevate(t)
	id := strconv.FormatInt(f.holder, 10)
	token := auth.User{ID: id, UserID: id, AuthType: "token", CredentialKind: auth.CredentialKindToken,
		TokenID: f.token(t, legacyrbac.TokenAdminAccessInherit, true), BrowserSessionRef: f.session}
	noElevation(t, "an inherit token claiming the bound reference", f.administration(t, token), f.adminSet)
}

func TestANativeSessionOfThePersonResolvesNothingFromTheElevation(t *testing.T) {
	f := newElevationFixture(t)
	f.elevate(t)
	id := strconv.FormatInt(f.holder, 10)
	native := auth.User{ID: id, UserID: id, AuthType: "token", CredentialKind: auth.CredentialKindNative,
		TokenID: f.token(t, legacyrbac.TokenAdminAccessNone, false), BrowserSessionRef: f.session,
		NativeClientID: "desktop"}
	got := f.administration(t, native)
	want := legacyrbac.AdministrationSet(auth.CredentialKindNative, legacyrbac.TokenAdminAccess{}, f.adminSet)
	noElevation(t, "a native session claiming the bound reference", got, want)
}

func TestARunTokenOfThePersonResolvesNothingFromTheElevation(t *testing.T) {
	f := newElevationFixture(t)
	f.elevate(t)
	id := strconv.FormatInt(f.holder, 10)
	run := auth.User{ID: id, UserID: id, AuthType: "token", CredentialKind: auth.CredentialKindRun,
		TokenID: f.token(t, legacyrbac.TokenAdminAccessInherit, true), BrowserSessionRef: f.session}
	noElevation(t, "a run token claiming the bound reference", f.administration(t, run), []string{})
}

// A session without an elevation resolves exactly its role permissions, and
// the developer mode answers the same golden set with and without one.
func TestWithoutAnElevationASessionResolvesItsRolesAndTheDeveloperModeIsUnchanged(t *testing.T) {
	f := newElevationFixture(t)
	noElevation(t, "the session before any elevation", f.administration(t, f.browser(f.session)), f.adminSet)
	developerGolden := []string{elevationTestDeveloper}
	developer := func(what string) {
		t.Helper()
		resolution, err := f.resolver.ResolvePermissions(t.Context(), f.browser(f.session), auth.PermissionModeDeveloper, "")
		if err != nil {
			t.Fatal(err)
		}
		noElevation(t, what, resolution, developerGolden)
		if resolution.ElevationSessionRef != 0 || resolution.Roles != nil {
			t.Fatalf("%s carries administration fields: %+v", what, resolution)
		}
	}
	developer("the developer mode before the elevation")
	f.elevate(t)
	developer("the developer mode under the elevation")
}

// The standing-set call never includes an elevation: the same answer with and
// without one, on the bound session itself.
func TestTheStandingSetCallIgnoresTheElevation(t *testing.T) {
	f := newElevationFixture(t)
	before := f.standing(t, f.browser(f.session))
	noElevation(t, "the standing set before the elevation", before, f.adminSet)
	f.elevate(t)
	after := f.standing(t, f.browser(f.session))
	noElevation(t, "the standing set under the elevation", after, f.adminSet)
	if !slices.Equal(before.Roles, after.Roles) || before.ElevationSessionRef != after.ElevationSessionRef {
		t.Fatalf("the standing call answered %+v before and %+v under the elevation", before, after)
	}
	// auth.ResolveStandingAdministration reaches the same call.
	via, err := auth.ResolveStandingAdministration(t.Context(), f.resolver, f.browser(f.session))
	if err != nil {
		t.Fatal(err)
	}
	noElevation(t, "auth.ResolveStandingAdministration under the elevation", via, f.adminSet)
}

// The resolver never picks one row of several: with two open elevations on
// one session (the ledger allows one; a writer outside its lock could leave
// two), the answer is the union of both levels' sets.
func TestTwoOpenElevationsOfOneSessionResolveTheUnion(t *testing.T) {
	f := newElevationFixture(t)
	plain := seedElevationPerson(t, f.pool)
	session := seedElevationSession(t, f.pool, plain)
	ctx := t.Context()
	if _, err := f.pool.Exec(ctx, `ALTER TABLE elitea_auth.admin_elevations DISABLE TRIGGER admin_elevations_append_only`); err != nil {
		t.Fatalf("disable the one-open-elevation check: %v", err)
	}
	var ids []string
	for _, level := range []string{adminaccess.RoleAdmin, adminaccess.RoleSuperAdmin} {
		var id string
		if err := f.pool.QueryRow(ctx, `
INSERT INTO elitea_auth.admin_elevations
    (user_id, browser_session_ref, level, path, reason_code, reason, requester_user_id, approver_user_id,
     requested_at, decision, decided_at, started_at, expires_at)
VALUES ($1, $2, $3, 'approved', 'incident', 'R2.1b: two open rows', $1, $4, $5, 'approved', $5, $5, $6)
RETURNING id::text`, plain, session, level, f.approver, elevationTestStart,
			elevationTestStart.Add(elevationTestDuration)).Scan(&id); err != nil {
			t.Fatalf("insert an open %s elevation: %v", level, err)
		}
		ids = append(ids, id)
	}
	if _, err := f.pool.Exec(ctx, `ALTER TABLE elitea_auth.admin_elevations ENABLE TRIGGER admin_elevations_append_only`); err != nil {
		t.Fatalf("enable the trigger again: %v", err)
	}
	slices.Sort(ids)
	id := strconv.FormatInt(plain, 10)
	got := f.administration(t, auth.User{ID: id, UserID: id, AuthType: "session",
		CredentialKind: auth.CredentialKindSession, BrowserSessionRef: session})
	want, _ := legacyrbac.UnionElevation(f.adminSet, f.superAdminSet)
	if !slices.Equal(got.Permissions, want) || !slices.Equal(got.ElevatedPermissions, want) {
		t.Fatalf("a person with no standing role resolves %v (elevated %v), want the union %v",
			got.Permissions, got.ElevatedPermissions, want)
	}
	if !slices.Equal(got.ElevationIDs, ids) {
		t.Fatalf("the elevation ids are %v, want both %v", got.ElevationIDs, ids)
	}
	if len(got.StandingPermissions()) != 0 || len(got.Roles) != 0 {
		t.Fatalf("a person with no standing role has the standing set %v and the roles %v", got.StandingPermissions(), got.Roles)
	}
}

func elevationRoleSet(t *testing.T, pool *pgxpool.Pool, role string) []string {
	t.Helper()
	set, err := adminaccess.CentralRolePermissions(t.Context(), pool, adminaccess.ModeAdministration, []string{role})
	if err != nil {
		t.Fatalf("read the set of %s: %v", role, err)
	}
	return set
}

func seedElevationPerson(t *testing.T, pool *pgxpool.Pool, roles ...string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(t.Context(), `
INSERT INTO public.auth_core__user (email, name) VALUES ($1, 'R2.1b') RETURNING id`,
		elevationTestFixtureTag+"-"+uuid.NewString()+"@autotest.local").Scan(&id); err != nil {
		t.Fatalf("seed a person: %v", err)
	}
	for _, role := range roles {
		tag, err := pool.Exec(t.Context(), `
INSERT INTO public.auth_core__user_role (user_id, role_id)
SELECT $1, id FROM public.auth_core__role WHERE name = $2 AND mode = 'administration'`, id, role)
		if err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("give %s: %v", role, err)
		}
	}
	return id
}

func seedElevationSession(t *testing.T, pool *pgxpool.Pool, userID int64) int64 {
	t.Helper()
	var ref int64
	if err := pool.QueryRow(t.Context(), `
INSERT INTO elitea_auth.browser_sessions (id, user_id, provider, created_at, last_seen_at, expires_at, idle_timeout_seconds)
VALUES ($1, $2, 'oidc', $3::timestamptz, $3::timestamptz, $3::timestamptz + interval '1 day', 0)
RETURNING ref`, elevationSessionID(), userID, elevationTestStart.Add(-time.Hour)).Scan(&ref); err != nil {
		t.Fatalf("seed a browser session: %v", err)
	}
	return ref
}

func seedElevationFormSession(t *testing.T, pool *pgxpool.Pool, userID int64) int64 {
	t.Helper()
	var ref int64
	if err := pool.QueryRow(t.Context(), `
INSERT INTO elitea_auth.form_sessions (id_hash, record, expires_at)
VALUES (encode(sha256(convert_to($1, 'UTF8')), 'hex'),
        convert_to(json_build_object('schema_version', 1, 'done', true, 'user_id', $2::bigint)::text, 'UTF8'),
        $3::timestamptz + interval '1 day')
RETURNING ref`, elevationSessionID(), userID, elevationTestStart).Scan(&ref); err != nil {
		t.Fatalf("seed a Form session: %v", err)
	}
	return ref
}

func elevationSessionID() string {
	return strings.ReplaceAll(uuid.NewString()+uuid.NewString(), "-", "")
}

// newElevationLedgerDatabase is an isolated database with 001_initial.sql and
// the shared corpus applied.
func newElevationLedgerDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := testpg.DSN(os.Getenv(environment))
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer adminPool.Close()
	databaseName := fmt.Sprintf("elitea_rbac_elevation_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", environment, err)
	}
	config.ConnConfig.Database = databaseName
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open isolated pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		// 120 s: the DROP queues behind the CREATE DATABASE calls of every
		// package `go test ./...` runs at the same time (#409).
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		dropPool, err := pgxpool.New(dropCtx, databaseURL)
		if err != nil {
			return
		}
		defer dropPool.Close()
		_, _ = dropPool.Exec(dropCtx, "DROP DATABASE IF EXISTS "+quoted+" WITH (FORCE)")
	})
	bootstrap, err := os.ReadFile("../db/migrations/001_initial.sql")
	if err != nil {
		t.Fatalf("read the bootstrap schema: %v", err)
	}
	if _, err := pool.Exec(ctx, string(bootstrap)); err != nil {
		t.Fatalf("apply the bootstrap schema: %v", err)
	}
	if err := migrate.New(pool, migrations.Files).ApplyShared(ctx); err != nil {
		t.Fatalf("apply the shared corpus: %v", err)
	}
	return pool
}
