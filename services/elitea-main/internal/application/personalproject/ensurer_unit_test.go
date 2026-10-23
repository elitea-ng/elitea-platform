package personalproject_test

// The parts that need no database: the name both readers agree on, and the two
// constructor/receiver states a composition can legitimately be in.

import (
	"context"
	"errors"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/personalproject"
)

// The name is a CONTRACT with two readers: internal/api/v2/social/handler.go's
// resolvePersonalProjectID and internal/api/middleware/project_resolver.go.
// Both now build it from `NamePrefix` rather than from a literal of their own,
// so this pins the value that shape produces — a change to it that the readers
// do not follow would make provisioning succeed and resolution fail silently,
// for every account.
func TestNameMatchesThePersonalProjectTemplateBothResolversUse(t *testing.T) {
	for userID, want := range map[int64]string{
		1:       "project_user_1",
		9:       "project_user_9",
		123_456: "project_user_123456",
	} {
		if got := personalproject.Name(userID); got != want {
			t.Errorf("Name(%d) = %q, want %q", userID, got, want)
		}
	}
}

// A composition that could not build an ensurer must be able to hold a nil one
// and call it — that is what keeps the branch out of the request handler. Both
// entry points therefore have to tolerate a nil receiver, EnsureAsync by doing
// nothing and Ensure by saying why.
func TestANilEnsurerIsInertRatherThanAPanic(t *testing.T) {
	var ensurer *personalproject.Ensurer
	ensurer.EnsureAsync(9) // must not panic

	if _, err := ensurer.Ensure(context.Background(), 9); !errors.Is(err, personalproject.ErrNotConfigured) {
		t.Fatalf("Ensure on a nil ensurer = %v, want ErrNotConfigured", err)
	}
}

// NewEnsurer refuses a missing dependency instead of returning something that
// fails later: without the pool it cannot tell whether a project already
// exists, and without the provisioner it cannot create one.
func TestNewEnsurerRefusesAMissingDependency(t *testing.T) {
	if _, err := personalproject.NewEnsurer(nil, nil); !errors.Is(err, personalproject.ErrNotConfigured) {
		t.Fatalf("NewEnsurer(nil, nil) = %v, want ErrNotConfigured", err)
	}
}
