package edgeauth

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type TokenValidator interface {
	ValidateToken(ctx context.Context, token string) (auth.User, error)
}

// TokenCredentialAuthenticator implements the two current Auth Core
// credential handlers without depending on an API package. Validator errors
// must use auth.ErrCredentialRejected for ordinary bad credentials; every
// other error remains a dependency failure for Kernel to fail closed.
type TokenCredentialAuthenticator struct {
	validator TokenValidator
}

func NewTokenCredentialAuthenticator(validator TokenValidator) (*TokenCredentialAuthenticator, error) {
	if validator == nil {
		return nil, ErrInvalidConfiguration
	}
	return &TokenCredentialAuthenticator{validator: validator}, nil
}

func (a *TokenCredentialAuthenticator) AuthenticateCredential(
	ctx context.Context,
	_ Source,
	credential CredentialInput,
) (CredentialResult, error) {
	if err := ctx.Err(); err != nil {
		return CredentialResult{}, err
	}
	if a == nil || a.validator == nil {
		return CredentialResult{}, ErrInvalidConfiguration
	}
	if !credential.validate() {
		return CredentialResult{Resolution: CredentialRejected}, nil
	}

	token, ok := credentialToken(credential)
	if !ok {
		return CredentialResult{Resolution: CredentialRejected}, nil
	}
	principal, err := a.validator.ValidateToken(ctx, token)
	if err != nil {
		if contextErr := requestContextError(ctx, err); contextErr != nil {
			return CredentialResult{}, contextErr
		}
		if errors.Is(err, auth.ErrCredentialRejected) {
			return CredentialResult{Resolution: CredentialRejected}, nil
		}
		return CredentialResult{}, err
	}
	return CredentialResult{Resolution: CredentialAccepted, Principal: principal}, nil
}

func credentialToken(credential CredentialInput) (string, bool) {
	if !credential.Present {
		return "", false
	}
	switch strings.ToLower(credential.Type) {
	case "bearer":
		return credential.Data, credential.Data != ""
	case "basic":
		decoded, err := base64.StdEncoding.DecodeString(credential.Data)
		if err != nil || !utf8.Valid(decoded) {
			return "", false
		}
		parts := strings.SplitN(string(decoded), ":", 2)
		if len(parts) != 2 || parts[0] == "" {
			return "", false
		}
		return parts[0], true
	default:
		return "", false
	}
}
