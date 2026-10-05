package authsvc

import (
	"context"
	"fmt"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// TokenValidator is the one credential seam the Auth middleware, the /auth
// edge endpoint and the edge credential authenticator share.
type TokenValidator interface {
	ValidateToken(ctx context.Context, token string) (auth.User, error)
}

// The native credential prefixes (internal/nativeauth). Restated here so this
// package does not import the authorization server.
const (
	nativeAccessPrefix  = "elnat_"
	nativeRefreshPrefix = "elnrt_"
	nativeCodePrefix    = "elnac_"
)

// NewNativeAwareValidator dispatches a bearer credential by its prefix
// (ADR-0025 WP2):
//
//   - `elnat_` — a native access token → native;
//   - `elnrt_` or `elnac_` — a refresh token or an authorization code is never
//     a bearer credential → rejected without a store read;
//   - anything else (a personal access token JWT) → pat.
//
// ADR-0017's four credential sources stay four: a native token travels as
// `Authorization: Bearer`, the same source a personal access token uses.
//
// A nil native returns pat ITSELF, unchanged. It never boxes a nil pointer into
// the interface (#86): the caller passes a nil INTERFACE when it has no store.
func NewNativeAwareValidator(pat, native TokenValidator) TokenValidator {
	if native == nil {
		return pat
	}
	return nativeAwareValidator{pat: pat, native: native}
}

type nativeAwareValidator struct {
	pat    TokenValidator
	native TokenValidator
}

func (v nativeAwareValidator) ValidateToken(ctx context.Context, token string) (auth.User, error) {
	switch {
	case strings.HasPrefix(token, nativeAccessPrefix):
		return v.native.ValidateToken(ctx, token)
	case strings.HasPrefix(token, nativeRefreshPrefix), strings.HasPrefix(token, nativeCodePrefix):
		return auth.User{}, fmt.Errorf("%w: a refresh token or authorization code is not a bearer credential",
			auth.ErrCredentialRejected)
	case v.pat == nil:
		// No PAT plane composed: the same 401 a nil Validator always gave.
		return auth.User{}, fmt.Errorf("%w: personal access token validator is not configured",
			auth.ErrCredentialRejected)
	default:
		return v.pat.ValidateToken(ctx, token)
	}
}
