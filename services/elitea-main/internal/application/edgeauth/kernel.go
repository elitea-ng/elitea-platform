package edgeauth

import (
	"context"
	"errors"
	"slices"
	"strconv"

	browserapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/browserauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browserflow"
	sessionstate "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/session"
)

// AuthorizedBrowser returns a detached authorization only for a coherent
// browser-session allow decision produced by this kernel. Presentation-layer
// mappers use this accessor instead of trusting the exported diagnostic fields
// independently or re-reading a session by bearer reference.
func (decision Decision) AuthorizedBrowser() (browserapp.Authorization, bool) {
	if decision.Kind != DecisionAllow || decision.Reason != ReasonBrowserSessionAccepted ||
		decision.Authentication.Type != AuthenticationUser ||
		!requiredBoundedText(decision.Authentication.Reference, MaxBrowserReferenceBytes) ||
		!validBrowserAuthorization(decision.Authentication.BrowserAuthorization) ||
		!sameUser(decision.Authentication.Principal, decision.Authentication.BrowserAuthorization.Principal) {
		return browserapp.Authorization{}, false
	}
	return cloneBrowserAuthorization(decision.Authentication.BrowserAuthorization), true
}

type Kernel struct {
	credentials CredentialAuthenticator
	sessions    SessionAuthorizer
	public      PublicPolicy
}

func NewKernel(
	credentials CredentialAuthenticator,
	sessions SessionAuthorizer,
	public PublicPolicy,
) (*Kernel, error) {
	if credentials == nil || sessions == nil {
		return nil, ErrInvalidConfiguration
	}
	return &Kernel{credentials: credentials, sessions: sessions, public: public}, nil
}

func (k *Kernel) Authorize(ctx context.Context, request Request) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	if k == nil || k.credentials == nil || k.sessions == nil {
		return Decision{}, ErrInvalidConfiguration
	}
	if !request.validate() {
		return Decision{}, ErrInvalidRequest
	}

	credentialPresent := false
	denyReason := ReasonCredentialRejected
	for _, credential := range request.Credentials {
		if !credential.Present {
			continue
		}
		credentialPresent = true
		result, err := k.credentials.AuthenticateCredential(ctx, request.Source, credential)
		if err != nil {
			if contextErr := requestContextError(ctx, err); contextErr != nil {
				return Decision{}, contextErr
			}
			return dependencyDecision(request.Source, ReasonDependencyUnavailable), nil
		}
		if err := ctx.Err(); err != nil {
			return Decision{}, err
		}

		switch result.Resolution {
		case CredentialAccepted:
			if !validTokenPrincipal(result.Principal) {
				return dependencyDecision(request.Source, ReasonMalformedDependencyResult), nil
			}
			return tokenDecision(request.Source, result.Principal), nil
		case CredentialRejected, CredentialRevoked:
			if !emptyUser(result.Principal) {
				return dependencyDecision(request.Source, ReasonMalformedDependencyResult), nil
			}
			if result.Resolution == CredentialRevoked {
				denyReason = ReasonCredentialRevoked
			}
			if request.Traversal == DirectHTTPTraversal {
				return k.publicOrDeny(request.Source, denyReason), nil
			}
			continue
		default:
			return dependencyDecision(request.Source, ReasonMalformedDependencyResult), nil
		}
	}
	if credentialPresent {
		return k.sessionThenFallback(ctx, request, true, denyReason)
	}

	return k.sessionThenFallback(ctx, request, false, denyReason)
}

func (k *Kernel) sessionThenFallback(
	ctx context.Context,
	request Request,
	credentialRejected bool,
	denyReason DecisionReason,
) (Decision, error) {
	if request.BrowserSession.Present {
		authorization, err := k.sessions.Authorize(ctx, request.BrowserSession.ID)
		if err == nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return Decision{}, contextErr
			}
			if !validBrowserAuthorization(authorization) {
				return dependencyDecision(request.Source, ReasonMalformedDependencyResult), nil
			}
			return browserDecision(request.Source, request.BrowserSession.Reference, authorization), nil
		}
		if contextErr := requestContextError(ctx, err); contextErr != nil {
			return Decision{}, contextErr
		}
		if !browserSessionUnavailable(err) {
			return dependencyDecision(request.Source, ReasonDependencyUnavailable), nil
		}
	}

	if publicMatch, ok := k.public.match(request.Source); ok {
		return publicDecision(request.Source, publicMatch), nil
	}
	if credentialRejected {
		return denyDecision(request.Source, denyReason), nil
	}
	return loginDecision(request.Source), nil
}

func (k *Kernel) publicOrDeny(source Source, reason DecisionReason) Decision {
	if publicMatch, ok := k.public.match(source); ok {
		return publicDecision(source, publicMatch)
	}
	return denyDecision(source, reason)
}

func validTokenPrincipal(principal auth.User) bool {
	userID, userOK := positiveID(principal.UserID)
	tokenID, tokenOK := positiveID(principal.TokenID)
	compatibilityID, compatibilityOK := positiveID(principal.ID)
	return principal.AuthType == "token" && userOK && tokenOK && compatibilityOK &&
		userID == compatibilityID && tokenID > 0
}

func validBrowserAuthorization(authorization browserapp.Authorization) bool {
	principal := authorization.Principal
	userID, userOK := positiveID(principal.UserID)
	compatibilityID, compatibilityOK := positiveID(principal.ID)
	if principal.AuthType != "session" || !userOK || !compatibilityOK ||
		userID != compatibilityID || principal.TokenID != "" ||
		browserflow.ValidateProvider(authorization.Provider) != nil {
		return false
	}

	provider := authorization.Provider
	stateUserID := userID
	state := sessionstate.State{
		SchemaVersion:      sessionstate.CurrentSchemaVersion,
		Done:               true,
		Provider:           &provider,
		ProviderAttributes: authorization.ProviderAttributes,
		Expiration:         authorization.Expiration,
		UserID:             &stateUserID,
	}
	return state.Validate() == nil
}

func positiveID(value string) (int64, bool) {
	id, err := strconv.ParseInt(value, 10, 64)
	return id, err == nil && id > 0
}

func emptyUser(user auth.User) bool {
	return user.ID == "" && user.UserID == "" && user.TokenID == "" && user.Email == "" &&
		len(user.Roles) == 0 && len(user.Permissions) == 0 && user.ProjectID == "" && user.AuthType == ""
}

func sameUser(left, right auth.User) bool {
	return left.ID == right.ID && left.UserID == right.UserID && left.TokenID == right.TokenID &&
		left.Email == right.Email && left.ProjectID == right.ProjectID && left.AuthType == right.AuthType &&
		slices.Equal(left.Roles, right.Roles) && slices.Equal(left.Permissions, right.Permissions)
}

func browserSessionUnavailable(err error) bool {
	return errors.Is(err, browserapp.ErrUnauthenticated) ||
		errors.Is(err, browserapp.ErrAuthenticationExpired) ||
		errors.Is(err, browserapp.ErrInvalidRequest)
}

func requestContextError(ctx context.Context, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return nil
}

func tokenDecision(source Source, principal auth.User) Decision {
	return Decision{
		Kind:   DecisionAllow,
		Reason: ReasonCredentialAccepted,
		Source: source,
		Authentication: Authentication{
			Type:      AuthenticationToken,
			Principal: cloneUser(principal),
			Reference: "-",
		},
	}
}

func browserDecision(source Source, reference string, authorization browserapp.Authorization) Decision {
	authorization = cloneBrowserAuthorization(authorization)
	return Decision{
		Kind:   DecisionAllow,
		Reason: ReasonBrowserSessionAccepted,
		Source: source,
		Authentication: Authentication{
			Type:                 AuthenticationUser,
			Principal:            authorization.Principal,
			Reference:            reference,
			BrowserAuthorization: authorization,
		},
	}
}

func publicDecision(source Source, match PublicMatch) Decision {
	return Decision{
		Kind:        DecisionAllow,
		Reason:      ReasonPublicRuleMatched,
		Source:      source,
		PublicMatch: match,
		Authentication: Authentication{
			Type:      AuthenticationPublic,
			Reference: "-",
		},
	}
}

func denyDecision(source Source, reason DecisionReason) Decision {
	return Decision{Kind: DecisionDeny, Reason: reason, Source: source}
}

func loginDecision(source Source) Decision {
	return Decision{Kind: DecisionLogin, Reason: ReasonAuthenticationRequired, Source: source}
}

func dependencyDecision(source Source, reason DecisionReason) Decision {
	return Decision{Kind: DecisionDependencyFailure, Reason: reason, Source: source}
}

func cloneUser(user auth.User) auth.User {
	user.Roles = append([]string(nil), user.Roles...)
	user.Permissions = append([]string(nil), user.Permissions...)
	return user
}

func cloneBrowserAuthorization(authorization browserapp.Authorization) browserapp.Authorization {
	authorization.Principal = cloneUser(authorization.Principal)
	authorization.ProviderAttributes = append([]byte(nil), authorization.ProviderAttributes...)
	if authorization.Expiration != nil {
		expiration := authorization.Expiration.UTC()
		authorization.Expiration = &expiration
	}
	return authorization
}
