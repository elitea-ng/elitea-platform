package applications

import (
	"errors"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// Issue 1968: every versions[] entry's name is held to 128 characters, not only
// the first.
func TestCheckVersionNamesChecksEveryEntry(t *testing.T) {
	ok := map[string]any{"name": "base"}
	long := map[string]any{"name": strings.Repeat("v", 129)}
	if err := checkVersionNames([]any{ok, map[string]any{"name": strings.Repeat("v", 128)}}); err != nil {
		t.Fatalf("names at the limit must pass: %v", err)
	}
	err := checkVersionNames([]any{ok, long})
	var apiErr *apierr.APIError
	if !errors.As(err, &apiErr) || apiErr.Field != "versions[1].name" || apiErr.MaxLength != 128 {
		t.Fatalf("want a field-level 400 naming versions[1].name, got %v", err)
	}
}
