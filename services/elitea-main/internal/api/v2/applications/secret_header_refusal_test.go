package applications

// The `X-SECRET` refusal table (#408 step 3).
//
// Every branch of secretHeaderRefusal is decided by ONE vault read, so the
// table drives that read directly. The route's PostgreSQL suite
// (version_expanded_postgres_integration_test.go) proves the same rules through
// the served response; this file proves the rules themselves, and it is the
// test that fails first if the pylon fallback returns.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// stubSecretsReader answers the one vault read the check makes.
type stubSecretsReader struct {
	value string
	err   error

	// seenProject and seenRef record what the check asked for. A refusal that
	// read a different secret, or a different project, would still pass every
	// status assertion below.
	seenProject string
	seenRef     string
}

func (s *stubSecretsReader) ResolveSecretValue(_ context.Context, projectID, secretRef string) (string, error) {
	s.seenProject = projectID
	s.seenRef = secretRef
	return s.value, s.err
}

func TestSecretHeaderRefusalTable(t *testing.T) {
	t.Parallel()

	const stored = "0Fq9mQ7Zr3kk_projects_own_value"

	cases := []struct {
		name     string
		value    string
		err      error
		received string
		// wantStatus is 0 when the request must pass.
		wantStatus  int
		wantMessage string
	}{
		{
			name:       "the project value passes",
			value:      stored,
			received:   stored,
			wantStatus: 0,
		},
		{
			name:        "another value is refused",
			value:       stored,
			received:    "not-the-vault-value",
			wantStatus:  http.StatusBadRequest,
			wantMessage: "Invalid secret header",
		},
		{
			name:        "an absent header is refused",
			value:       stored,
			received:    "",
			wantStatus:  http.StatusBadRequest,
			wantMessage: "Invalid secret header",
		},
		{
			// The defect this issue closes. Pylon answers 200 here.
			name:        "the pylon literal is refused when the project has no value",
			err:         fmt.Errorf("look up p_1 secret: %w: %q", secrets.ErrSecretNotFound, "secrets_header_value"),
			received:    "secret",
			wantStatus:  http.StatusForbidden,
			wantMessage: "This project has no secrets_header_value secret. The version details route cannot authenticate a caller until the project has one.",
		},
		{
			// The same fault reached through the other typed error.
			name:        "the pylon literal is refused when the project has no vault",
			err:         fmt.Errorf("look up p_1 secret: %w", secrets.ErrVaultAbsent),
			received:    "secret",
			wantStatus:  http.StatusForbidden,
			wantMessage: "This project has no secrets_header_value secret. The version details route cannot authenticate a caller until the project has one.",
		},
		{
			// A project with no value refuses EVERY caller, not only the one
			// that guessed the literal. Without this case, a fallback that
			// moved to a different constant would still pass the case above.
			name:        "any value is refused when the project has no value",
			err:         fmt.Errorf("look up p_1 secret: %w", secrets.ErrSecretNotFound),
			received:    stored,
			wantStatus:  http.StatusForbidden,
			wantMessage: "This project has no secrets_header_value secret. The version details route cannot authenticate a caller until the project has one.",
		},
		{
			// A stored empty string authenticates an empty header under a
			// plain comparison, and an empty header is what a caller that sets
			// none sends.
			name:        "a stored empty value refuses an empty header",
			value:       "",
			received:    "",
			wantStatus:  http.StatusForbidden,
			wantMessage: "This project has no secrets_header_value secret. The version details route cannot authenticate a caller until the project has one.",
		},
		{
			// Pre-#408 behaviour, kept: a vault that will not open is not an
			// open door.
			name:        "an unreadable vault is refused",
			err:         errors.New("decrypt the p_1 vault: cipher: message authentication failed"),
			received:    stored,
			wantStatus:  http.StatusForbidden,
			wantMessage: "This project vault cannot be read.",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			reader := &stubSecretsReader{value: testCase.value, err: testCase.err}
			handler := &Handler{}

			refusal := handler.secretHeaderRefusal(context.Background(), reader, "1", testCase.received)

			if reader.seenProject != "1" || reader.seenRef != currentSecretsHeaderName {
				t.Fatalf("the check read project %q secret %q, want project \"1\" secret %q",
					reader.seenProject, reader.seenRef, currentSecretsHeaderName)
			}
			if testCase.wantStatus == 0 {
				if refusal != nil {
					t.Fatalf("the project value must pass, got %v", refusal)
				}
				return
			}
			if refusal == nil {
				t.Fatalf("expected a refusal with status %d, got none", testCase.wantStatus)
			}
			var apiError *apierr.APIError
			if !errors.As(refusal, &apiError) {
				t.Fatalf("expected an apierr.APIError, got %T: %v", refusal, refusal)
			}
			if apiError.Status != testCase.wantStatus {
				t.Errorf("expected status %d, got %d", testCase.wantStatus, apiError.Status)
			}
			if apiError.Message != testCase.wantMessage {
				t.Errorf("expected message %q, got %q", testCase.wantMessage, apiError.Message)
			}
		})
	}
}

// The literal must not survive anywhere in the package. A constant deleted from
// the comparison but kept for "documentation" is one edit away from returning.
func TestThePylonLiteralIsNotAConstantHere(t *testing.T) {
	t.Parallel()

	reader := &stubSecretsReader{err: secrets.ErrSecretNotFound}
	handler := &Handler{}
	for _, guess := range []string{"secret", "SECRET", "Secret", ""} {
		if refusal := handler.secretHeaderRefusal(context.Background(), reader, "1", guess); refusal == nil {
			t.Errorf("the guess %q must not authenticate a project with no value", guess)
		}
	}
}
