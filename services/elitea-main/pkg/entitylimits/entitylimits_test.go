package entitylimits

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

func TestBoundaries(t *testing.T) {
	cases := []struct {
		name  string
		check func(string, string) error
		value string
		ok    bool
		max   int
	}{
		{"name 128", CheckName, strings.Repeat("a", 128), true, 128},
		{"name 129", CheckName, strings.Repeat("a", 129), false, 128},
		{"name 128 multibyte", CheckName, strings.Repeat("я", 128), true, 128},
		{"name 129 multibyte", CheckName, strings.Repeat("я", 129), false, 128},
		{"name 128 emoji (4 bytes each)", CheckName, strings.Repeat("😀", 128), true, 128},
		{"name 129 emoji", CheckName, strings.Repeat("😀", 129), false, 128},
		{"name empty", CheckName, "", true, 128},
		{"description 2000", CheckDescription, strings.Repeat("a", 2000), true, 2000},
		{"description 2001", CheckDescription, strings.Repeat("a", 2001), false, 2000},
		{"description 2000 multibyte", CheckDescription, strings.Repeat("字", 2000), true, 2000},
		{"description 2001 multibyte", CheckDescription, strings.Repeat("字", 2001), false, 2000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.check("field", tc.value)
			if tc.ok {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var apiErr *apierr.APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("want *APIError, got %v", err)
			}
			if apiErr.Status != http.StatusBadRequest || apiErr.Field != "field" || apiErr.MaxLength != tc.max {
				t.Fatalf("unexpected error %+v", apiErr)
			}
		})
	}
}

func TestTruncateKeepsRunes(t *testing.T) {
	got := Truncate(strings.Repeat("😀", 200), 128)
	if n := len([]rune(got)); n != 128 {
		t.Fatalf("want 128 runes, got %d", n)
	}
	if Truncate("short", 128) != "short" {
		t.Fatal("short text must be unchanged")
	}
}
