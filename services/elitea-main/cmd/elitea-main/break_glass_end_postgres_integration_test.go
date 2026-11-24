package main

// `elitea-main break-glass end ID` against the full corpus (ADR-0059 R2.13,
// issue 2527): the elevated session's next request resolves no elevated
// permission, and the end is one chained `ended` event with the source
// `operator`. CI-only: skipped without ELITEA_TEST_DATABASE_URL.

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/browsersession"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

func TestBreakGlassEndStopsTheElevatedSession(t *testing.T) {
	pool, dsn := newMigrateTestDatabase(t)
	elevation := startTestElevation(t, pool)
	id := strconv.FormatInt(elevation.UserID, 10)
	principal := auth.User{ID: id, UserID: id, AuthType: "session", CredentialKind: auth.CredentialKindSession,
		BrowserSessionRef: elevation.BrowserSessionRef}
	resolver := legacyrbac.NewPostgresResolver(pool)
	resolve := func() auth.PermissionResolution {
		t.Helper()
		resolution, err := resolver.ResolvePermissions(t.Context(), principal, auth.PermissionModeAdministration, "")
		if err != nil {
			t.Fatalf("resolve the elevated session: %v", err)
		}
		return resolution
	}
	if before := resolve(); len(before.ElevatedPermissions) == 0 ||
		!slices.Contains(before.ElevationIDs, elevation.ID.String()) {
		t.Fatalf("premise: the session resolves no elevation: %+v", before)
	}

	run := runBreakGlassAgainst(t, dsn, "end", elevation.ID.String())
	if run.code != breakGlassExitOK || !strings.Contains(run.stdout, "ended elevation "+elevation.ID.String()) {
		t.Fatalf("end: exit %d\nstdout: %s\nstderr: %s", run.code, run.stdout, run.stderr)
	}
	if after := resolve(); after.Elevated() || slices.Contains(after.Permissions, adminaccess.AdminAuthUsers) {
		t.Fatalf("the session still resolves the elevation after the end: %+v", after)
	}

	current, err := adminaccess.ReadElevation(t.Context(), pool, elevation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.EndReason != adminaccess.ElevationEndOperator || current.ReviewState != adminaccess.ElevationReviewPending {
		t.Fatalf("the ended elevation: %+v", current)
	}
	events := elevationOutboxActions(t, pool, elevation.ID)
	if !slices.Equal(events, [][2]string{{"started", "console"}, {"ended", "operator"}}) {
		t.Fatalf("events = %v, want started, then one ended with the source operator", events)
	}
}

// A sealed session (R2.7a) through `end --all` (R2.13's deferred criterion):
// the person holds super_admin as a standing role, the sealed elevation is at
// the admin level, and the sealed session resolves the admin set only, never
// the standing super_admin; `end --all` ends the elevation and signs the
// session out, so its next request is unauthenticated.
func TestBreakGlassEndAllSignsOutASealedSession(t *testing.T) {
	pool, dsn := newMigrateTestDatabase(t)
	ctx := t.Context()
	var userID int64
	if err := pool.QueryRow(ctx, `INSERT INTO public.auth_core__user (email, name) VALUES ($1, 'Sealed') RETURNING id`,
		"sealed-"+uuid.NewString()+"@autotest.local").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__user_role (user_id, role_id)
SELECT $1, id FROM public.auth_core__role WHERE name = $2 AND mode = 'administration'`, userID, adminaccess.RoleSuperAdmin); err != nil {
		t.Fatal(err)
	}
	appender, err := securityaudit.NewPostgresAppender("elitea-main/break-glass-test")
	if err != nil {
		t.Fatal(err)
	}
	var started browsersession.SealedSession
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		started, err = browsersession.StartSealedSession(ctx, tx, appender, browsersession.SealedStart{
			UserID: userID, Level: adminaccess.ElevationLevelAdmin, ReasonCode: adminaccess.ElevationReasonIdentityOutage,
			Reason: "the identity provider is down", At: time.Now().UTC().Add(-time.Minute), Duration: time.Hour,
		})
		return err
	}); err != nil {
		t.Fatalf("start a sealed session: %v", err)
	}
	store, err := browsersession.NewPostgresStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := browsersession.NewManager(store, browsersession.Policy{AbsoluteLifetime: 8 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	session, err := manager.ValidateSealed(ctx, started.CookieValue)
	if err != nil {
		t.Fatalf("premise: the sealed session does not authenticate: %v", err)
	}
	principal := session.User()
	resolver := legacyrbac.NewPostgresResolver(pool)
	resolution, err := resolver.ResolvePermissions(ctx, principal, auth.PermissionModeAdministration, "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(resolution.Permissions, adminaccess.AdminAuthUsers) ||
		!slices.Contains(resolution.ElevationIDs, started.Elevation.ID.String()) {
		t.Fatalf("the sealed session does not resolve the admin set of its elevation: %+v", resolution)
	}
	if slices.Contains(resolution.Permissions, adminaccess.SuperAdminPermission) {
		t.Fatalf("an admin-level sealed session resolves %s from the standing role: %+v",
			adminaccess.SuperAdminPermission, resolution)
	}
	if len(resolution.Roles) != 0 {
		t.Fatalf("a sealed session acts with the standing roles %v", resolution.Roles)
	}
	standing, err := resolver.ResolveStandingAdministration(ctx, principal)
	if err != nil {
		t.Fatal(err)
	}
	if len(standing.Permissions) != 0 {
		t.Fatalf("the standing set of a sealed session is %v, want empty", standing.Permissions)
	}

	run := runBreakGlassAgainst(t, dsn, "end", "--all")
	if run.code != breakGlassExitOK || !strings.Contains(run.stdout, started.Elevation.ID.String()) {
		t.Fatalf("end --all: exit %d\nstdout: %s\nstderr: %s", run.code, run.stdout, run.stderr)
	}
	if _, err := manager.ValidateSealed(ctx, started.CookieValue); err == nil {
		t.Fatal("the sealed session still authenticates after end --all")
	}
	var revoked bool
	if err := pool.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM elitea_auth.browser_sessions WHERE ref = $1`,
		started.Session.Ref).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if !revoked {
		t.Fatal("end --all left the sealed session row live")
	}
	after, err := resolver.ResolvePermissions(ctx, principal, auth.PermissionModeAdministration, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Permissions) != 0 {
		t.Fatalf("the sealed principal resolves %v after end --all", after.Permissions)
	}
}
