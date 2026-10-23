package auth_test

import (
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

func TestOwningUserIDDoesNotConfuseLegacyTokenIDWithUserID(t *testing.T) {
	for _, test := range []struct {
		name string
		user auth.User
		want int64
		ok   bool
	}{
		{
			name: "direct user",
			user: auth.User{ID: "42", AuthType: "user"},
			want: 42,
			ok:   true,
		},
		{
			name: "resolved token owner",
			user: auth.User{ID: "900", TokenID: "900", UserID: "42", AuthType: "token"},
			want: 42,
			ok:   true,
		},
		{
			name: "unresolved token",
			user: auth.User{ID: "900", TokenID: "900", AuthType: "token"},
			ok:   false,
		},
		{
			name: "invalid direct user",
			user: auth.User{ID: "not-an-id", AuthType: "user"},
			ok:   false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := test.user.OwningUserID()
			if ok != test.ok || got != test.want {
				t.Fatalf("OwningUserID() = (%d, %v), want (%d, %v)", got, ok, test.want, test.ok)
			}
		})
	}
}
