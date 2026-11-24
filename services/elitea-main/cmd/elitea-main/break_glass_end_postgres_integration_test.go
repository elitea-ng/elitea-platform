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

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
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
