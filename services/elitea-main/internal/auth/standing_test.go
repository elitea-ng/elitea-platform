package auth_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// plainResolver has no standing-set call: it answers one resolution for every
// mode, elevated or not.
type plainResolver struct{ resolution auth.PermissionResolution }

func (p plainResolver) ResolvePermissions(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
	return p.resolution, nil
}

type standingResolver struct {
	plainResolver
	standing auth.PermissionResolution
}

func (s standingResolver) ResolveStandingAdministration(context.Context, auth.User) (auth.PermissionResolution, error) {
	return s.standing, nil
}

func elevatedResolution() auth.PermissionResolution {
	return auth.PermissionResolution{
		UserID:              7,
		Permissions:         []string{"a", "b", "super"},
		ElevatedPermissions: []string{"super"},
		ElevationIDs:        []string{"e1"},
		ElevationID:         "e1",
		Roles:               []string{"admin"},
	}
}

func TestTheStandingPartLeavesOutWhatOnlyTheElevationGives(t *testing.T) {
	resolution := elevatedResolution()
	if got := resolution.StandingPermissions(); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("the standing part is %v", got)
	}
	if !resolution.Elevated() {
		t.Fatal("an elevated resolution says it is not")
	}
	if !(auth.PermissionResolution{ElevationID: "e1"}).Elevated() {
		t.Fatal("the elevation indicator alone does not say elevated")
	}
	if got := (auth.PermissionResolution{}).StandingPermissions(); got == nil || len(got) != 0 {
		t.Fatalf("an empty resolution's standing part is %#v", got)
	}
}

// A resolver with no standing-set call answers its resolution minus the
// elevated part; one with the call is asked through it. Neither answer
// reports an elevation.
func TestResolveStandingAdministrationNeverAnswersAnElevation(t *testing.T) {
	fallback, err := auth.ResolveStandingAdministration(context.Background(),
		plainResolver{resolution: elevatedResolution()}, auth.User{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fallback.Permissions, []string{"a", "b"}) || fallback.Elevated() || fallback.UserID != 7 ||
		fallback.ElevationID != "" {
		t.Fatalf("the fallback answers %+v", fallback)
	}

	// Even a standing call that (wrongly) reported an elevation is cleaned.
	direct, err := auth.ResolveStandingAdministration(context.Background(), standingResolver{
		plainResolver: plainResolver{resolution: auth.PermissionResolution{UserID: 7, Permissions: []string{"wrong"}}},
		standing:      elevatedResolution(),
	}, auth.User{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(direct.Permissions, []string{"a", "b"}) || direct.Elevated() {
		t.Fatalf("the standing call answers %+v", direct)
	}

	if _, err := auth.ResolveStandingAdministration(context.Background(), nil, auth.User{}); !errors.Is(err, auth.ErrPermissionDenied) {
		t.Fatalf("no resolver: %v, want a refusal", err)
	}
}
