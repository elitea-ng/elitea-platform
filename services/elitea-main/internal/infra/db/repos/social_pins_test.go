package repos

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

func TestSocialPinsValidateIdentityBeforeStorage(t *testing.T) {
	for _, test := range []struct {
		name    string
		user    auth.User
		project string
		entity  string
		id      string
		status  int
	}{
		{"missing actor", auth.User{}, "1", "conversation", "1", http.StatusForbidden},
		{"invalid actor", auth.User{ID: "not-an-id"}, "1", "conversation", "1", http.StatusForbidden},
		{"actor overflow", auth.User{ID: "2147483648"}, "1", "conversation", "1", http.StatusForbidden},
		{"unowned token", auth.User{ID: "7", TokenID: "1"}, "1", "conversation", "1", http.StatusForbidden},
		{"zero project", auth.User{ID: "7"}, "0", "conversation", "1", http.StatusBadRequest},
		{"project alias", auth.User{ID: "7"}, "01", "conversation", "1", http.StatusBadRequest},
		{"project overflow", auth.User{ID: "7"}, "2147483648", "conversation", "1", http.StatusBadRequest},
		{"negative entity", auth.User{ID: "7"}, "1", "conversation", "-1", http.StatusBadRequest},
		{"entity alias", auth.User{ID: "7"}, "1", "conversation", "01", http.StatusBadRequest},
		{"entity overflow", auth.User{ID: "7"}, "1", "conversation", "2147483648", http.StatusBadRequest},
		{"unknown entity", auth.User{ID: "7"}, "1", "unknown", "1", http.StatusBadRequest},
		{"valid unavailable storage", auth.User{ID: "7"}, "1", "conversation", "1", http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := auth.ContextWithUser(context.Background(), test.user)
			repository := NewCurrentSocialPinsRepository(nil)
			for _, operation := range []func(context.Context, string, string, string) error{repository.Pin, repository.Unpin} {
				err := operation(ctx, test.project, test.entity, test.id)
				var apiError *apierr.APIError
				if !errors.As(err, &apiError) || apiError.Status != test.status {
					t.Fatalf("error=%v, want HTTP %d before storage", err, test.status)
				}
			}
		})
	}
	for _, operation := range []func(context.Context, string, string, string) error{
		NewCurrentSocialPinsRepository(nil).Pin, NewCurrentSocialPinsRepository(nil).Unpin,
	} {
		var apiError *apierr.APIError
		if err := operation(context.Background(), "1", "conversation", "1"); !errors.As(err, &apiError) || apiError.Status != http.StatusUnauthorized {
			t.Fatalf("unauthenticated error=%v, want HTTP 401", err)
		}
	}
}

func TestSocialPinUsesTokenOwnerAndCurrentEntityTypes(t *testing.T) {
	ctx := auth.ContextWithUser(context.Background(), auth.User{ID: "7", UserID: "8", TokenID: "1"})
	for _, entity := range []string{"prompt", "collection", "datasource", "application", "toolkit", "configuration", "conversation", "skill", "agent", "pipeline", "mcp"} {
		project, id, actor, err := validateSocialPin(ctx, "1", entity, "2")
		if err != nil || project != 1 || id != 2 || actor != 8 {
			t.Fatalf("entity %s: project=%d id=%d actor=%d error=%v", entity, project, id, actor, err)
		}
	}
}
