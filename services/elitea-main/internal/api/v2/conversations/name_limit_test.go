package conversations

import (
	"errors"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// Issue 1968: the rename limit is 128 characters (it was 256), counted in
// runes, and the refusal names the field.
func TestConversationNameLimitIs128Runes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		ok    bool
	}{
		{"128", strings.Repeat("a", 128), true},
		{"129", strings.Repeat("a", 129), false},
		{"128 multibyte", strings.Repeat("я", 128), true},
		{"129 multibyte", strings.Repeat("я", 129), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := conversationNameFromBody(map[string]any{"name": tc.value})
			if tc.ok {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var apiErr *apierr.APIError
			if !errors.As(err, &apiErr) || apiErr.Field != "name" || apiErr.MaxLength != 128 {
				t.Fatalf("want a field-level 400, got %v", err)
			}
		})
	}
}
