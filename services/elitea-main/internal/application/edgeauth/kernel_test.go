package edgeauth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	browserapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/browserauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type credentialAuthenticatorFunc func(
	context.Context,
	Source,
	CredentialInput,
) (CredentialResult, error)

func (f credentialAuthenticatorFunc) AuthenticateCredential(
	ctx context.Context,
	source Source,
	credential CredentialInput,
) (CredentialResult, error) {
	return f(ctx, source, credential)
}

type sessionAuthorizerFunc func(context.Context, string) (browserapp.Authorization, error)

func (f sessionAuthorizerFunc) Authorize(
	ctx context.Context,
	sessionID string,
) (browserapp.Authorization, error) {
	return f(ctx, sessionID)
}

func TestCredentialPrecedenceUsesOnlyFirstPresentInput(t *testing.T) {
	var got []CredentialInput
	kernel := newTestKernel(t,
		credentialAuthenticatorFunc(func(_ context.Context, _ Source, credential CredentialInput) (CredentialResult, error) {
			got = append(got, credential)
			return CredentialResult{Resolution: CredentialAccepted, Principal: validTokenPrincipalFixture()}, nil
		}),
		panicSessionAuthorizer(),
		emptyPublicPolicy(t),
	)
	request := validRequest(DirectHTTPTraversal)
	request.Credentials = []CredentialInput{
		{},
		{Present: true, Type: "bearer", Data: "authorization-token"},
		{Present: true, Type: "bearer", Data: "configured-header-token"},
	}

	decision, err := kernel.Authorize(context.Background(), request)
	if err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if decision.Kind != DecisionAllow || decision.Reason != ReasonCredentialAccepted ||
		decision.Authentication.Type != AuthenticationToken {
		t.Fatalf("decision = %+v", decision)
	}
	if len(got) != 1 || got[0].Data != "authorization-token" {
		t.Fatalf("authenticated credentials = %+v, want only first present input", got)
	}
	if decision.Source.TargetPresent || decision.Source.Target != "" {
		t.Fatalf("target presence was not preserved: %+v", decision.Source)
	}
}

func TestMainTraversalContinuesThroughOrderedRejectedCredentials(t *testing.T) {
	var got []string
	kernel := newTestKernel(t,
		credentialAuthenticatorFunc(func(_ context.Context, _ Source, credential CredentialInput) (CredentialResult, error) {
			got = append(got, credential.Data)
			if credential.Data == "authorization-invalid" {
				return CredentialResult{Resolution: CredentialRejected}, nil
			}
			return CredentialResult{Resolution: CredentialAccepted, Principal: validTokenPrincipalFixture()}, nil
		}),
		panicSessionAuthorizer(),
		emptyPublicPolicy(t),
	)
	request := validRequest(MainTraversal)
	request.Credentials = []CredentialInput{
		{Present: true, Type: "bearer", Data: "authorization-invalid"},
		{},
		{Present: true, Type: "bearer", Data: "configured-header-valid"},
		{Present: true, Type: "bearer", Data: "must-not-run"},
	}

	decision, err := kernel.Authorize(context.Background(), request)
	if err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if decision.Kind != DecisionAllow || decision.Authentication.Type != AuthenticationToken {
		t.Fatalf("decision = %+v", decision)
	}
	if len(got) != 2 || got[0] != "authorization-invalid" || got[1] != "configured-header-valid" {
		t.Fatalf("credential order = %v", got)
	}
}

func TestRejectedCredentialIsTerminalForDirectHTTPTraversal(t *testing.T) {
	tests := []struct {
		name       string
		public     PublicPolicy
		wantKind   DecisionKind
		wantAuth   AuthenticationType
		wantReason DecisionReason
	}{
		{
			name:       "private request is denied",
			public:     emptyPublicPolicy(t),
			wantKind:   DecisionDeny,
			wantReason: ReasonCredentialRejected,
		},
		{
			name:       "matching public rule overrides rejection",
			public:     uriPublicPolicy(t, "public", `/api/public`),
			wantKind:   DecisionAllow,
			wantAuth:   AuthenticationPublic,
			wantReason: ReasonPublicRuleMatched,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			credentialCalls := 0
			sessionCalls := 0
			kernel := newTestKernel(t,
				credentialAuthenticatorFunc(func(_ context.Context, _ Source, credential CredentialInput) (CredentialResult, error) {
					credentialCalls++
					if credential.Data != "invalid" {
						t.Fatalf("credential = %+v", credential)
					}
					return CredentialResult{Resolution: CredentialRejected}, nil
				}),
				sessionAuthorizerFunc(func(context.Context, string) (browserapp.Authorization, error) {
					sessionCalls++
					return validBrowserAuthorizationFixture(), nil
				}),
				test.public,
			)
			request := validRequest(DirectHTTPTraversal)
			request.Source.URI = "/api/public"
			request.Credentials = []CredentialInput{
				{Present: true, Type: "bearer", Data: "invalid"},
				{Present: true, Type: "bearer", Data: "would-be-valid"},
			}
			request.BrowserSession = validBrowserInput()

			decision, err := kernel.Authorize(context.Background(), request)
			if err != nil {
				t.Fatalf("Authorize() error = %v", err)
			}
			if decision.Kind != test.wantKind || decision.Reason != test.wantReason ||
				decision.Authentication.Type != test.wantAuth {
				t.Fatalf("decision = %+v", decision)
			}
			if credentialCalls != 1 {
				t.Fatalf("credential calls = %d, want 1", credentialCalls)
			}
			if sessionCalls != 0 {
				t.Fatalf("browser session calls = %d, direct policy traversed invalid credential", sessionCalls)
			}
		})
	}
}

func TestBrowserSessionOutcomes(t *testing.T) {
	tests := []struct {
		name       string
		session    sessionAuthorizerFunc
		public     PublicPolicy
		wantKind   DecisionKind
		wantAuth   AuthenticationType
		wantReason DecisionReason
	}{
		{
			name: "valid session wins before public",
			session: func(context.Context, string) (browserapp.Authorization, error) {
				return validBrowserAuthorizationFixture(), nil
			},
			public:     uriPublicPolicy(t, "public", `/api/private`),
			wantKind:   DecisionAllow,
			wantAuth:   AuthenticationUser,
			wantReason: ReasonBrowserSessionAccepted,
		},
		{
			name: "expired session falls back to public",
			session: func(context.Context, string) (browserapp.Authorization, error) {
				return browserapp.Authorization{}, browserapp.ErrAuthenticationExpired
			},
			public:     uriPublicPolicy(t, "public", `/api/private`),
			wantKind:   DecisionAllow,
			wantAuth:   AuthenticationPublic,
			wantReason: ReasonPublicRuleMatched,
		},
		{
			name: "missing session requires login",
			session: func(context.Context, string) (browserapp.Authorization, error) {
				return browserapp.Authorization{}, browserapp.ErrUnauthenticated
			},
			public:     emptyPublicPolicy(t),
			wantKind:   DecisionLogin,
			wantReason: ReasonAuthenticationRequired,
		},
		{
			name: "invalid session ID requires login",
			session: func(context.Context, string) (browserapp.Authorization, error) {
				return browserapp.Authorization{}, browserapp.ErrInvalidRequest
			},
			public:     emptyPublicPolicy(t),
			wantKind:   DecisionLogin,
			wantReason: ReasonAuthenticationRequired,
		},
		{
			name: "dependency failure cannot become public",
			session: func(context.Context, string) (browserapp.Authorization, error) {
				return browserapp.Authorization{}, errors.New("dependency unavailable")
			},
			public:     uriPublicPolicy(t, "public", `/api/private`),
			wantKind:   DecisionDependencyFailure,
			wantReason: ReasonDependencyUnavailable,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			kernel := newTestKernel(t, panicCredentialAuthenticator(), test.session, test.public)
			request := validRequest(DirectHTTPTraversal)
			request.BrowserSession = validBrowserInput()

			decision, err := kernel.Authorize(context.Background(), request)
			if err != nil {
				t.Fatalf("Authorize() error = %v", err)
			}
			if decision.Kind != test.wantKind || decision.Reason != test.wantReason ||
				decision.Authentication.Type != test.wantAuth {
				t.Fatalf("decision = %+v", decision)
			}
			if test.wantAuth == AuthenticationUser && decision.Authentication.Reference != "v1.session-id" {
				t.Fatalf("browser reference = %q", decision.Authentication.Reference)
			}
		})
	}
}

func TestMainTraversalPreservesCredentialBrowserPublicFallback(t *testing.T) {
	tests := []struct {
		name       string
		hasSession bool
		session    sessionAuthorizerFunc
		public     PublicPolicy
		wantKind   DecisionKind
		wantAuth   AuthenticationType
		wantReason DecisionReason
	}{
		{
			name:       "invalid credential traverses valid browser session",
			hasSession: true,
			session: func(context.Context, string) (browserapp.Authorization, error) {
				return validBrowserAuthorizationFixture(), nil
			},
			public:     emptyPublicPolicy(t),
			wantKind:   DecisionAllow,
			wantAuth:   AuthenticationUser,
			wantReason: ReasonBrowserSessionAccepted,
		},
		{
			name:       "invalid credential falls back to public",
			hasSession: true,
			session: func(context.Context, string) (browserapp.Authorization, error) {
				return browserapp.Authorization{}, browserapp.ErrUnauthenticated
			},
			public:     uriPublicPolicy(t, "public", `/api/private`),
			wantKind:   DecisionAllow,
			wantAuth:   AuthenticationPublic,
			wantReason: ReasonPublicRuleMatched,
		},
		{
			name:       "invalid credential without fallback is denied",
			hasSession: false,
			session:    panicSessionAuthorizer(),
			public:     emptyPublicPolicy(t),
			wantKind:   DecisionDeny,
			wantReason: ReasonCredentialRejected,
		},
		{
			name:       "session outage never falls back to public",
			hasSession: true,
			session: func(context.Context, string) (browserapp.Authorization, error) {
				return browserapp.Authorization{}, errors.New("session dependency unavailable")
			},
			public:     uriPublicPolicy(t, "public", `/api/private`),
			wantKind:   DecisionDependencyFailure,
			wantReason: ReasonDependencyUnavailable,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			kernel := newTestKernel(t,
				credentialAuthenticatorFunc(func(context.Context, Source, CredentialInput) (CredentialResult, error) {
					return CredentialResult{Resolution: CredentialRejected}, nil
				}),
				test.session,
				test.public,
			)
			request := validRequest(MainTraversal)
			request.Credentials = []CredentialInput{{Present: true, Type: "bearer", Data: "invalid"}}
			if test.hasSession {
				request.BrowserSession = validBrowserInput()
			}

			decision, err := kernel.Authorize(context.Background(), request)
			if err != nil {
				t.Fatalf("Authorize() error = %v", err)
			}
			if decision.Kind != test.wantKind || decision.Reason != test.wantReason ||
				decision.Authentication.Type != test.wantAuth {
				t.Fatalf("decision = %+v", decision)
			}
		})
	}
}

func TestCredentialDependencyCannotBecomePublic(t *testing.T) {
	for _, traversal := range []TraversalPolicy{DirectHTTPTraversal, MainTraversal} {
		t.Run(traversalName(traversal), func(t *testing.T) {
			kernel := newTestKernel(t,
				credentialAuthenticatorFunc(func(context.Context, Source, CredentialInput) (CredentialResult, error) {
					return CredentialResult{}, errors.New("postgres unavailable")
				}),
				panicSessionAuthorizer(),
				uriPublicPolicy(t, "public", `/api/private`),
			)
			request := validRequest(traversal)
			request.Credentials = []CredentialInput{{Present: true, Type: "bearer", Data: "token"}}

			decision, err := kernel.Authorize(context.Background(), request)
			if err != nil {
				t.Fatalf("Authorize() error = %v", err)
			}
			if decision.Kind != DecisionDependencyFailure || decision.Reason != ReasonDependencyUnavailable {
				t.Fatalf("decision = %+v", decision)
			}
		})
	}
}

func TestPublicPolicyUsesRegistrationOrderAndFullMatch(t *testing.T) {
	policy, err := NewPublicPolicy([]PublicRule{
		{Name: "does-not-match", Conditions: []RuleCondition{{Field: SourceURI, Pattern: `/other`}}},
		{Name: "first-match", Conditions: []RuleCondition{{Field: SourceURI, Pattern: `/api/.*`}}},
		{Name: "later-match", Conditions: []RuleCondition{{Field: SourceURI, Pattern: `.*`}}},
	})
	if err != nil {
		t.Fatalf("NewPublicPolicy() error = %v", err)
	}
	kernel := newTestKernel(t, panicCredentialAuthenticator(), panicSessionAuthorizer(), policy)
	request := validRequest(DirectHTTPTraversal)

	decision, err := kernel.Authorize(context.Background(), request)
	if err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if decision.Kind != DecisionAllow || decision.Authentication.Type != AuthenticationPublic ||
		decision.PublicMatch.RuleIndex != 1 || decision.PublicMatch.RuleName != "first-match" {
		t.Fatalf("decision = %+v", decision)
	}

	fullMatch := uriPublicPolicy(t, "exact", `/api`)
	kernel = newTestKernel(t, panicCredentialAuthenticator(), panicSessionAuthorizer(), fullMatch)
	request.Source.URI = "/api/private"
	decision, err = kernel.Authorize(context.Background(), request)
	if err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if decision.Kind != DecisionLogin {
		t.Fatalf("partial regex match decision = %+v", decision)
	}
}

func TestPublicPolicyRequiresExplicitMatchAll(t *testing.T) {
	if _, err := NewPublicPolicy([]PublicRule{{Name: "implicit match all"}}); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("implicit match-all error = %v, want %v", err, ErrInvalidConfiguration)
	}
	if _, err := NewPublicPolicy([]PublicRule{{
		Name:       "ambiguous match all",
		MatchAll:   true,
		Conditions: []RuleCondition{{Field: SourceURI, Pattern: `.*`}},
	}}); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("ambiguous match-all error = %v, want %v", err, ErrInvalidConfiguration)
	}

	policy, err := NewPublicPolicy([]PublicRule{{Name: "explicit match all", MatchAll: true}})
	if err != nil {
		t.Fatalf("NewPublicPolicy() error = %v", err)
	}
	kernel := newTestKernel(t, panicCredentialAuthenticator(), panicSessionAuthorizer(), policy)
	decision, err := kernel.Authorize(context.Background(), validRequest(DirectHTTPTraversal))
	if err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if decision.Kind != DecisionAllow || decision.Authentication.Type != AuthenticationPublic ||
		decision.PublicMatch.RuleName != "explicit match all" {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestPublicPolicyAcceptsTrackedPythonHyphenEscape(t *testing.T) {
	policy := uriPublicPolicy(t, "edge-auth", `/auth/.*`)
	kernel := newTestKernel(t, panicCredentialAuthenticator(), panicSessionAuthorizer(), policy)
	request := validRequest(DirectHTTPTraversal)
	request.Source.URI = "/auth/login"

	decision, err := kernel.Authorize(context.Background(), request)
	if err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if decision.Kind != DecisionAllow || decision.Authentication.Type != AuthenticationPublic {
		t.Fatalf("decision = %+v", decision)
	}

	classPolicy := uriPublicPolicy(t, "literal-hyphen", `[a\-z]`)
	kernel = newTestKernel(t, panicCredentialAuthenticator(), panicSessionAuthorizer(), classPolicy)
	request.Source.URI = "-"
	decision, err = kernel.Authorize(context.Background(), request)
	if err != nil {
		t.Fatalf("Authorize(character class) error = %v", err)
	}
	if decision.Kind != DecisionAllow {
		t.Fatalf("literal hyphen decision = %+v", decision)
	}
	request.Source.URI = "m"
	decision, err = kernel.Authorize(context.Background(), request)
	if err != nil {
		t.Fatalf("Authorize(character range probe) error = %v", err)
	}
	if decision.Kind != DecisionLogin {
		t.Fatalf("escaped hyphen became a range: %+v", decision)
	}
}

func TestPublicPolicyPreservesTargetPresence(t *testing.T) {
	policy, err := NewPublicPolicy([]PublicRule{{
		Name:       "explicit-empty-target",
		Conditions: []RuleCondition{{Field: SourceTarget, Pattern: ``}},
	}})
	if err != nil {
		t.Fatalf("NewPublicPolicy() error = %v", err)
	}
	kernel := newTestKernel(t, panicCredentialAuthenticator(), panicSessionAuthorizer(), policy)

	absent := validRequest(DirectHTTPTraversal)
	decision, err := kernel.Authorize(context.Background(), absent)
	if err != nil {
		t.Fatalf("Authorize(absent) error = %v", err)
	}
	if decision.Kind != DecisionLogin {
		t.Fatalf("absent target decision = %+v", decision)
	}

	present := validRequest(DirectHTTPTraversal)
	present.Source.TargetPresent = true
	decision, err = kernel.Authorize(context.Background(), present)
	if err != nil {
		t.Fatalf("Authorize(present) error = %v", err)
	}
	if decision.Kind != DecisionAllow || decision.Authentication.Type != AuthenticationPublic {
		t.Fatalf("present empty target decision = %+v", decision)
	}
}

func TestAuthorizePreservesCancellation(t *testing.T) {
	t.Run("already canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		kernel := newTestKernel(t, panicCredentialAuthenticator(), panicSessionAuthorizer(), emptyPublicPolicy(t))
		_, err := kernel.Authorize(ctx, validRequest(DirectHTTPTraversal))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Authorize() error = %v, want context.Canceled", err)
		}
	})

	tests := []struct {
		name       string
		credential bool
		failure    error
	}{
		{name: "credential deadline", credential: true, failure: context.DeadlineExceeded},
		{name: "session canceled", failure: context.Canceled},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			credential := panicCredentialAuthenticator()
			session := panicSessionAuthorizer()
			request := validRequest(DirectHTTPTraversal)
			if test.credential {
				request.Credentials = []CredentialInput{{Present: true, Type: "bearer", Data: "token"}}
				credential = func(context.Context, Source, CredentialInput) (CredentialResult, error) {
					return CredentialResult{}, test.failure
				}
			} else {
				request.BrowserSession = validBrowserInput()
				session = func(context.Context, string) (browserapp.Authorization, error) {
					return browserapp.Authorization{}, test.failure
				}
			}
			kernel := newTestKernel(t, credential, session, emptyPublicPolicy(t))
			_, err := kernel.Authorize(context.Background(), request)
			if !errors.Is(err, test.failure) {
				t.Fatalf("Authorize() error = %v, want %v", err, test.failure)
			}
		})
	}
}

func TestMalformedTypedDependencyResultsFailClosed(t *testing.T) {
	credentialCases := []struct {
		name   string
		result CredentialResult
	}{
		{
			name: "accepted token missing token ID",
			result: CredentialResult{Resolution: CredentialAccepted, Principal: auth.User{
				ID: "7", UserID: "7", AuthType: "token",
			}},
		},
		{
			name: "accepted token compatibility ID is not owner",
			result: CredentialResult{Resolution: CredentialAccepted, Principal: auth.User{
				ID: "42", UserID: "7", TokenID: "42", AuthType: "token",
			}},
		},
		{
			name: "rejection unexpectedly carries a principal",
			result: CredentialResult{Resolution: CredentialRejected, Principal: auth.User{
				ID: "7",
			}},
		},
		{name: "unknown credential resolution", result: CredentialResult{}},
	}
	for _, test := range credentialCases {
		t.Run(test.name, func(t *testing.T) {
			kernel := newTestKernel(t,
				credentialAuthenticatorFunc(func(context.Context, Source, CredentialInput) (CredentialResult, error) {
					return test.result, nil
				}),
				panicSessionAuthorizer(),
				uriPublicPolicy(t, "public", `/api/private`),
			)
			request := validRequest(DirectHTTPTraversal)
			request.Credentials = []CredentialInput{{Present: true, Type: "bearer", Data: "token"}}

			decision, err := kernel.Authorize(context.Background(), request)
			if err != nil {
				t.Fatalf("Authorize() error = %v", err)
			}
			if decision.Kind != DecisionDependencyFailure || decision.Reason != ReasonMalformedDependencyResult {
				t.Fatalf("decision = %+v", decision)
			}
		})
	}

	browserCases := []struct {
		name          string
		authorization browserapp.Authorization
	}{
		{
			name: "session principal carries token ID",
			authorization: func() browserapp.Authorization {
				value := validBrowserAuthorizationFixture()
				value.Principal.TokenID = "9"
				return value
			}(),
		},
		{
			name: "session principal IDs disagree",
			authorization: func() browserapp.Authorization {
				value := validBrowserAuthorizationFixture()
				value.Principal.ID = "8"
				return value
			}(),
		},
		{
			name: "provider attributes are not an object",
			authorization: func() browserapp.Authorization {
				value := validBrowserAuthorizationFixture()
				value.ProviderAttributes = []byte(`[]`)
				return value
			}(),
		},
	}
	for _, test := range browserCases {
		t.Run(test.name, func(t *testing.T) {
			kernel := newTestKernel(t,
				panicCredentialAuthenticator(),
				sessionAuthorizerFunc(func(context.Context, string) (browserapp.Authorization, error) {
					return test.authorization, nil
				}),
				uriPublicPolicy(t, "public", `/api/private`),
			)
			request := validRequest(DirectHTTPTraversal)
			request.BrowserSession = validBrowserInput()

			decision, err := kernel.Authorize(context.Background(), request)
			if err != nil {
				t.Fatalf("Authorize() error = %v", err)
			}
			if decision.Kind != DecisionDependencyFailure || decision.Reason != ReasonMalformedDependencyResult {
				t.Fatalf("decision = %+v", decision)
			}
		})
	}
}

func TestConfigurationAndRequestBounds(t *testing.T) {
	for _, test := range []struct {
		name  string
		rules []PublicRule
	}{
		{name: "invalid regex", rules: []PublicRule{{Conditions: []RuleCondition{{Field: SourceURI, Pattern: `(`}}}}},
		{name: "unknown field", rules: []PublicRule{{Conditions: []RuleCondition{{Field: 255, Pattern: `.*`}}}}},
		{name: "duplicate field", rules: []PublicRule{{Conditions: []RuleCondition{
			{Field: SourceURI, Pattern: `.*`}, {Field: SourceURI, Pattern: `.*`},
		}}}},
		{name: "blank name", rules: []PublicRule{{Name: " ", Conditions: []RuleCondition{{Field: SourceURI, Pattern: `.*`}}}}},
		{name: "duplicate name", rules: []PublicRule{
			{Name: "same", Conditions: []RuleCondition{{Field: SourceURI, Pattern: `/first`}}},
			{Name: "same", Conditions: []RuleCondition{{Field: SourceURI, Pattern: `/second`}}},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewPublicPolicy(test.rules); !errors.Is(err, ErrInvalidConfiguration) {
				t.Fatalf("NewPublicPolicy() error = %v", err)
			}
		})
	}

	kernel := newTestKernel(t, panicCredentialAuthenticator(), panicSessionAuthorizer(), emptyPublicPolicy(t))
	requests := []Request{
		func() Request {
			request := validRequest(0)
			return request
		}(),
		func() Request {
			request := validRequest(DirectHTTPTraversal)
			request.Source.Target = "rpc"
			return request
		}(),
		func() Request {
			request := validRequest(DirectHTTPTraversal)
			request.Credentials = make([]CredentialInput, MaxCredentials+1)
			return request
		}(),
		func() Request {
			request := validRequest(DirectHTTPTraversal)
			request.Credentials = []CredentialInput{{Present: true, Type: "bearer", Data: strings.Repeat("x", MaxCredentialDataBytes+1)}}
			return request
		}(),
		func() Request {
			request := validRequest(DirectHTTPTraversal)
			request.Source.Method = ""
			return request
		}(),
		func() Request {
			request := validRequest(DirectHTTPTraversal)
			request.BrowserSession = BrowserSessionInput{Present: true, ID: "", Reference: "v1.valid"}
			return request
		}(),
		func() Request {
			request := validRequest(DirectHTTPTraversal)
			request.BrowserSession = BrowserSessionInput{Present: true, ID: "session-id", Reference: ""}
			return request
		}(),
	}
	for index, request := range requests {
		if _, err := kernel.Authorize(context.Background(), request); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("request %d error = %v, want ErrInvalidRequest", index, err)
		}
	}
}

func TestDecisionOwnsMutableIdentityData(t *testing.T) {
	principal := validTokenPrincipalFixture()
	principal.Roles = []string{"editor"}
	principal.Permissions = []string{"configuration.read"}
	kernel := newTestKernel(t,
		credentialAuthenticatorFunc(func(context.Context, Source, CredentialInput) (CredentialResult, error) {
			return CredentialResult{Resolution: CredentialAccepted, Principal: principal}, nil
		}),
		panicSessionAuthorizer(),
		emptyPublicPolicy(t),
	)
	request := validRequest(DirectHTTPTraversal)
	request.Credentials = []CredentialInput{{Present: true, Type: "bearer", Data: "token"}}

	decision, err := kernel.Authorize(context.Background(), request)
	if err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	principal.Roles[0] = "mutated"
	principal.Permissions[0] = "mutated"
	if decision.Authentication.Principal.Roles[0] != "editor" ||
		decision.Authentication.Principal.Permissions[0] != "configuration.read" {
		t.Fatalf("decision aliases resolver data: %+v", decision.Authentication.Principal)
	}
}

func TestDecisionAuthorizedBrowserRequiresCoherentKernelResultAndReturnsClone(t *testing.T) {
	authorization := validBrowserAuthorizationFixture()
	kernel := newTestKernel(t,
		panicCredentialAuthenticator(),
		sessionAuthorizerFunc(func(context.Context, string) (browserapp.Authorization, error) {
			return authorization, nil
		}),
		emptyPublicPolicy(t),
	)
	request := validRequest(DirectHTTPTraversal)
	request.BrowserSession = validBrowserInput()
	decision, err := kernel.Authorize(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}

	got, ok := decision.AuthorizedBrowser()
	if !ok || got.Provider != "form" || string(got.ProviderAttributes) != `{"nameid":"user"}` ||
		got.Principal.UserID != "7" {
		t.Fatalf("authorization = %+v, ok=%v", got, ok)
	}
	got.ProviderAttributes[2] = 'X'
	got.Principal.Email = "mutated@example.test"
	again, ok := decision.AuthorizedBrowser()
	if !ok || string(again.ProviderAttributes) != `{"nameid":"user"}` ||
		again.Principal.Email != "user@example.test" {
		t.Fatalf("authorization aliases caller data: %+v, ok=%v", again, ok)
	}

	invalid := []Decision{
		func() Decision { value := decision; value.Kind = DecisionDeny; return value }(),
		func() Decision { value := decision; value.Reason = ReasonPublicRuleMatched; return value }(),
		func() Decision { value := decision; value.Authentication.Type = AuthenticationToken; return value }(),
		func() Decision { value := decision; value.Authentication.Reference = ""; return value }(),
		func() Decision { value := decision; value.Authentication.Principal.UserID = "8"; return value }(),
		func() Decision {
			value := decision
			value.Authentication.BrowserAuthorization.ProviderAttributes = []byte(`{"nameid":"first","nameid":"second"}`)
			return value
		}(),
	}
	for index, value := range invalid {
		if got, ok := value.AuthorizedBrowser(); ok || got.Provider != "" || got.ProviderAttributes != nil ||
			got.Expiration != nil || got.Principal.ID != "" {
			t.Fatalf("invalid decision %d authorization = %+v, ok=%v", index, got, ok)
		}
	}
}

func newTestKernel(
	t *testing.T,
	credentials CredentialAuthenticator,
	sessions SessionAuthorizer,
	public PublicPolicy,
) *Kernel {
	t.Helper()
	kernel, err := NewKernel(credentials, sessions, public)
	if err != nil {
		t.Fatalf("NewKernel() error = %v", err)
	}
	return kernel
}

func validRequest(traversal TraversalPolicy) Request {
	return Request{
		Source: Source{
			Method: "GET",
			Proto:  "https",
			Host:   "elitea.example.test",
			URI:    "/api/private",
			IP:     "192.0.2.7",
		},
		Traversal: traversal,
	}
}

func validTokenPrincipalFixture() auth.User {
	return auth.User{
		ID:       "7",
		UserID:   "7",
		TokenID:  "42",
		Email:    "user@example.test",
		AuthType: "token",
	}
}

func validBrowserAuthorizationFixture() browserapp.Authorization {
	expiration := time.Now().UTC().Add(time.Hour)
	return browserapp.Authorization{
		Principal: auth.User{
			ID:       "7",
			UserID:   "7",
			Email:    "user@example.test",
			AuthType: "session",
		},
		Provider:           "form",
		ProviderAttributes: []byte(`{"nameid":"user"}`),
		Expiration:         &expiration,
	}
}

func validBrowserInput() BrowserSessionInput {
	return BrowserSessionInput{Present: true, ID: "session-id", Reference: "v1.session-id"}
}

func emptyPublicPolicy(t *testing.T) PublicPolicy {
	t.Helper()
	policy, err := NewPublicPolicy(nil)
	if err != nil {
		t.Fatalf("NewPublicPolicy() error = %v", err)
	}
	return policy
}

func uriPublicPolicy(t *testing.T, name, pattern string) PublicPolicy {
	t.Helper()
	policy, err := NewPublicPolicy([]PublicRule{{
		Name:       name,
		Conditions: []RuleCondition{{Field: SourceURI, Pattern: pattern}},
	}})
	if err != nil {
		t.Fatalf("NewPublicPolicy() error = %v", err)
	}
	return policy
}

func panicCredentialAuthenticator() credentialAuthenticatorFunc {
	return func(context.Context, Source, CredentialInput) (CredentialResult, error) {
		panic("credential authenticator must not be called")
	}
}

func panicSessionAuthorizer() sessionAuthorizerFunc {
	return func(context.Context, string) (browserapp.Authorization, error) {
		panic("session authorizer must not be called")
	}
}

func traversalName(traversal TraversalPolicy) string {
	if traversal == DirectHTTPTraversal {
		return "direct"
	}
	return "main"
}

// ADR-0025 WP3: a revoked native credential is a rejection for every rule of
// the kernel, but the deny says WHY, so the edge can answer device_revoked.
func TestRevokedCredentialDeniesWithItsOwnReason(t *testing.T) {
	revoked := credentialAuthenticatorFunc(func(context.Context, Source, CredentialInput) (CredentialResult, error) {
		return CredentialResult{Resolution: CredentialRevoked}, nil
	})
	for _, traversal := range []TraversalPolicy{MainTraversal, DirectHTTPTraversal} {
		kernel := newTestKernel(t, revoked, panicSessionAuthorizer(), emptyPublicPolicy(t))
		request := validRequest(traversal)
		request.Credentials = []CredentialInput{{Present: true, Type: "bearer", Data: "elnat_x"}}
		decision, err := kernel.Authorize(context.Background(), request)
		if err != nil || decision.Kind != DecisionDeny || decision.Reason != ReasonCredentialRevoked {
			t.Fatalf("traversal %v: decision = %+v, err = %v", traversal, decision, err)
		}
	}
	// A public route stays public even for a revoked bearer.
	kernel := newTestKernel(t, revoked, panicSessionAuthorizer(), uriPublicPolicy(t, "public", `^/api/private$`))
	request := validRequest(MainTraversal)
	request.Credentials = []CredentialInput{{Present: true, Type: "bearer", Data: "elnat_x"}}
	if decision, err := kernel.Authorize(context.Background(), request); err != nil || decision.Kind != DecisionAllow {
		t.Fatalf("public route with a revoked bearer = %+v, %v", decision, err)
	}
}

func TestCredentialAuthenticatorClassifiesDeviceRevoked(t *testing.T) {
	authenticator, err := NewTokenCredentialAuthenticator(tokenValidatorFunc(func(context.Context, string) (auth.User, error) {
		return auth.User{}, fmt.Errorf("wrapped: %w", auth.ErrDeviceRevoked)
	}))
	if err != nil {
		t.Fatal(err)
	}
	result, err := authenticator.AuthenticateCredential(context.Background(), Source{},
		CredentialInput{Present: true, Type: "bearer", Data: "elnat_x"})
	if err != nil || result.Resolution != CredentialRevoked {
		t.Fatalf("result = %+v, %v; want CredentialRevoked", result, err)
	}
}
