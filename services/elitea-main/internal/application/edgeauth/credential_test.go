package edgeauth

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type tokenValidatorFunc func(context.Context, string) (auth.User, error)

func (f tokenValidatorFunc) ValidateToken(ctx context.Context, token string) (auth.User, error) {
	return f(ctx, token)
}

func TestTokenCredentialAuthenticatorParsesCurrentHandlers(t *testing.T) {
	validBasic := base64.StdEncoding.EncodeToString([]byte("basic-token:ignored-password"))
	invalidUTF8 := base64.StdEncoding.EncodeToString([]byte{0xff, ':', 'x'})
	tests := []struct {
		name      string
		input     CredentialInput
		wantToken string
		want      CredentialResolution
	}{
		{
			name:      "mixed-case bearer",
			input:     CredentialInput{Present: true, Type: "BeArEr", Data: "bearer-token"},
			wantToken: "bearer-token",
			want:      CredentialAccepted,
		},
		{
			name:      "mixed-case basic",
			input:     CredentialInput{Present: true, Type: "BaSiC", Data: validBasic},
			wantToken: "basic-token",
			want:      CredentialAccepted,
		},
		{name: "empty bearer", input: CredentialInput{Present: true, Type: "bearer"}, want: CredentialRejected},
		{name: "invalid base64", input: CredentialInput{Present: true, Type: "basic", Data: "!!!"}, want: CredentialRejected},
		{
			name:  "basic missing colon",
			input: CredentialInput{Present: true, Type: "basic", Data: base64.StdEncoding.EncodeToString([]byte("token"))},
			want:  CredentialRejected,
		},
		{
			name:  "basic empty token",
			input: CredentialInput{Present: true, Type: "basic", Data: base64.StdEncoding.EncodeToString([]byte(":password"))},
			want:  CredentialRejected,
		},
		{name: "basic invalid UTF-8", input: CredentialInput{Present: true, Type: "basic", Data: invalidUTF8}, want: CredentialRejected},
		{name: "unsupported type", input: CredentialInput{Present: true, Type: "digest", Data: "value"}, want: CredentialRejected},
		{
			name: "oversized bearer",
			input: CredentialInput{
				Present: true,
				Type:    "bearer",
				Data:    string(make([]byte, MaxCredentialDataBytes+1)),
			},
			want: CredentialRejected,
		},
		{name: "absent input", input: CredentialInput{}, want: CredentialRejected},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			authenticator, err := NewTokenCredentialAuthenticator(tokenValidatorFunc(
				func(_ context.Context, token string) (auth.User, error) {
					calls++
					if token != test.wantToken {
						t.Fatalf("token = %q, want %q", token, test.wantToken)
					}
					return validTokenPrincipalFixture(), nil
				},
			))
			if err != nil {
				t.Fatalf("NewTokenCredentialAuthenticator() error = %v", err)
			}

			result, err := authenticator.AuthenticateCredential(context.Background(), Source{}, test.input)
			if err != nil {
				t.Fatalf("AuthenticateCredential() error = %v", err)
			}
			if result.Resolution != test.want {
				t.Fatalf("resolution = %v, want %v", result.Resolution, test.want)
			}
			wantCalls := 0
			if test.want == CredentialAccepted {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("validator calls = %d, want %d", calls, wantCalls)
			}
		})
	}
}

func TestTokenCredentialAuthenticatorClassifiesValidatorFailures(t *testing.T) {
	tests := []struct {
		name           string
		validatorError error
		wantResolution CredentialResolution
		wantError      error
	}{
		{
			name:           "credential rejected",
			validatorError: errors.Join(auth.ErrCredentialRejected, errors.New("test detail")),
			wantResolution: CredentialRejected,
		},
		{
			name:           "dependency unavailable",
			validatorError: errors.Join(auth.ErrCredentialValidationUnavailable, errors.New("test detail")),
			wantError:      auth.ErrCredentialValidationUnavailable,
		},
		{name: "unclassified error is dependency", validatorError: errors.New("unknown validator error"), wantError: errors.New("non-nil")},
		{name: "cancellation", validatorError: context.Canceled, wantError: context.Canceled},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authenticator, err := NewTokenCredentialAuthenticator(tokenValidatorFunc(
				func(context.Context, string) (auth.User, error) {
					return auth.User{}, test.validatorError
				},
			))
			if err != nil {
				t.Fatalf("NewTokenCredentialAuthenticator() error = %v", err)
			}

			result, err := authenticator.AuthenticateCredential(context.Background(), Source{}, CredentialInput{
				Present: true,
				Type:    "bearer",
				Data:    "token",
			})
			if result.Resolution != test.wantResolution {
				t.Fatalf("resolution = %v, want %v", result.Resolution, test.wantResolution)
			}
			switch {
			case test.wantError == nil && err != nil:
				t.Fatalf("AuthenticateCredential() error = %v", err)
			case test.wantError != nil && err == nil:
				t.Fatal("AuthenticateCredential() error = nil")
			case test.wantError != nil && test.wantError.Error() != "non-nil" && !errors.Is(err, test.wantError):
				t.Fatalf("AuthenticateCredential() error = %v, want %v", err, test.wantError)
			}
		})
	}
}

func TestTokenCredentialAuthenticatorRequiresValidatorAndPreservesCanceledContext(t *testing.T) {
	if _, err := NewTokenCredentialAuthenticator(nil); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("NewTokenCredentialAuthenticator(nil) error = %v", err)
	}

	called := false
	authenticator, err := NewTokenCredentialAuthenticator(tokenValidatorFunc(
		func(context.Context, string) (auth.User, error) {
			called = true
			return validTokenPrincipalFixture(), nil
		},
	))
	if err != nil {
		t.Fatalf("NewTokenCredentialAuthenticator() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = authenticator.AuthenticateCredential(ctx, Source{}, CredentialInput{
		Present: true,
		Type:    "bearer",
		Data:    "token",
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("AuthenticateCredential() error = %v, want context.Canceled", err)
	}
	if called {
		t.Fatal("validator was called after cancellation")
	}
}
