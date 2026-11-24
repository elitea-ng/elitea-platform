package authsvc

import (
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// A token of a project's system identity is the kind `system`, which holds no
// administration rights (ADR-0059 decision 7); every other personal access
// token is `token`. The address is matched exactly, as shared 0265 matches it.
func TestPATCredentialKindSeparatesTheProjectSystemIdentity(t *testing.T) {
	for email, want := range map[string]auth.CredentialKind{
		"system_user_7@centry.user":      auth.CredentialKindSystem,
		"system_user_12345@centry.user":  auth.CredentialKindSystem,
		"system@centry.user":             auth.CredentialKindToken,
		"system_user_7@centry.user.evil": auth.CredentialKindToken,
		"xsystem_user_7@centry.user":     auth.CredentialKindToken,
		"system_user_@centry.user":       auth.CredentialKindToken,
		"system_user_7@centryXuser":      auth.CredentialKindToken,
		"ada@example.com":                auth.CredentialKindToken,
		"":                               auth.CredentialKindToken,
	} {
		if got := patCredentialKind(email); got != want {
			t.Errorf("patCredentialKind(%q) = %q, want %q", email, got, want)
		}
	}
}
