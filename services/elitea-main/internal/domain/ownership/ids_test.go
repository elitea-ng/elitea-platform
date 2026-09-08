package ownership_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/ownership"
)

// The point of the package: the two meanings are two TYPES, not two ints.
//
// Issue #533 measured what one shared type costs. `owner_id` holds a project in
// one tenant table and a user in the next, both are INTEGER, and the compiler
// accepted either number in either place. A wrong value reached a row and was
// visible only to a reader who joined the column.
func TestProjectIDAndUserIDAreDifferentTypes(t *testing.T) {
	t.Parallel()

	project := reflect.TypeOf(ownership.ProjectID(0))
	user := reflect.TypeOf(ownership.UserID(0))

	if project == user {
		t.Fatal("ProjectID and UserID are the same type, so one can be passed where the other belongs")
	}
	if !project.ConvertibleTo(user) {
		t.Fatal("the two types are not convertible, so this test is measuring something else")
	}
	// Assignment without a conversion is what must fail, and it does: the two
	// named types are not assignable to each other. reflect states the same
	// rule the compiler applies at every call site.
	if project.AssignableTo(user) || user.AssignableTo(project) {
		t.Error("one id type is assignable to the other, so a swapped argument still compiles")
	}
}

func TestNewProjectIDRefusesWhatCannotNameAProject(t *testing.T) {
	t.Parallel()

	for _, value := range []int64{0, -1, -4200} {
		if _, err := ownership.NewProjectID(value); !errors.Is(err, ownership.ErrNotAProjectID) {
			t.Errorf("NewProjectID(%d) = %v, want ErrNotAProjectID", value, err)
		}
	}
	id, err := ownership.NewProjectID(42)
	if err != nil {
		t.Fatalf("NewProjectID(42): %v", err)
	}
	if id.Int64() != 42 || id.String() != "42" {
		t.Errorf("NewProjectID(42) = %d / %q, want 42 / \"42\"", id.Int64(), id.String())
	}
}

func TestNewUserIDRefusesWhatCannotNameAUser(t *testing.T) {
	t.Parallel()

	for _, value := range []int64{0, -1, -7} {
		if _, err := ownership.NewUserID(value); !errors.Is(err, ownership.ErrNotAUserID) {
			t.Errorf("NewUserID(%d) = %v, want ErrNotAUserID", value, err)
		}
	}
	id, err := ownership.NewUserID(7)
	if err != nil {
		t.Fatalf("NewUserID(7): %v", err)
	}
	if id.Int64() != 7 || id.String() != "7" {
		t.Errorf("NewUserID(7) = %d / %q, want 7 / \"7\"", id.Int64(), id.String())
	}
}
