package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/stretchr/testify/require"
)

type runtimeContextAuthorizerFunc func(context.Context, ContentClaim) (RuntimeContextAuthorization, error)

func (f runtimeContextAuthorizerFunc) AuthorizeRuntimeContext(ctx context.Context, claim ContentClaim) (RuntimeContextAuthorization, error) {
	return f(ctx, claim)
}

type projectTokenValidatorFunc func(context.Context, string) (auth.User, error)

func (f projectTokenValidatorFunc) ValidateToken(ctx context.Context, token string) (auth.User, error) {
	return f(ctx, token)
}

type actorTokenIssuerFunc func(context.Context, int64) (string, error)

func (f actorTokenIssuerFunc) IssueToken(ctx context.Context, userID int64) (string, error) {
	return f(ctx, userID)
}

type projectSystemTokenIssuerFunc func(context.Context, int64) (ProjectSystemToken, error)

func (f projectSystemTokenIssuerFunc) IssueProjectToken(
	ctx context.Context,
	projectID int64,
) (ProjectSystemToken, error) {
	return f(ctx, projectID)
}

type projectSecretsHeaderReaderFunc func(context.Context, int64) (string, error)

func (f projectSecretsHeaderReaderFunc) ResolveProjectSecretsHeaderValue(
	ctx context.Context,
	projectID int64,
) (string, error) {
	return f(ctx, projectID)
}

// stubProjectSecretsHeader answers every project with one value (#408).
func stubProjectSecretsHeader(value string) projectSecretsHeaderReaderFunc {
	return func(context.Context, int64) (string, error) { return value, nil }
}

func TestIndexRuntimeContextReturnsOnlyClaimActorToken(t *testing.T) {
	t.Parallel()

	const token = "header.payload.signature"
	fence := bytes.Repeat([]byte{4}, sha256.Size)
	certificate := &x509.Certificate{}
	issueCalls := 0
	service, err := NewEliteaClientTokenService(
		runtimeContextAuthorizerFunc(func(_ context.Context, claim ContentClaim) (RuntimeContextAuthorization, error) {
			require.Equal(t, "execution-1", claim.ExecutionID)
			require.EqualValues(t, 7, claim.Generation)
			require.Equal(t, "claim-1", claim.ClaimID)
			require.Equal(t, fence, claim.FenceToken)
			require.Same(t, certificate, claim.PeerCertificate)
			require.Empty(t, claim.ContentID)
			require.Empty(t, claim.ImmutableVersion)
			return RuntimeContextAuthorization{
				ResourceProjectID: 42,
				ActorID:           "900",
				Initiator:         runtimeContextInitiatorUser,
			}, nil
		}),
		actorTokenIssuerFunc(func(_ context.Context, actorID int64) (string, error) {
			issueCalls++
			require.EqualValues(t, 900, actorID)
			return token, nil
		}),
		projectTokenValidatorFunc(func(_ context.Context, got string) (auth.User, error) {
			require.Equal(t, token, got)
			return auth.User{
				ID: "900", UserID: "900", TokenID: "901",
				Email: "actor@example.test", AuthType: "token",
			}, nil
		}),
		stubProjectSecretsHeader("project-header-value"),
	)
	require.NoError(t, err)
	server := newIndexRuntimeContextTestServer(t, service)

	request := httptest.NewRequest(http.MethodPost, "/executions/execution-1/generations/7/runtime-context/elitea-client-token", nil)
	request.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{certificate}}}
	request.Header.Set(claimIDHeader, "claim-1")
	request.Header.Set(fenceHeader, base64.RawURLEncoding.EncodeToString(fence))
	response := httptest.NewRecorder()
	server.Routes().ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "application/json", response.Header().Get("Content-Type"))
	require.Equal(t, "no-store, no-cache, must-revalidate", response.Header().Get("Cache-Control"))
	require.Equal(t, "no-cache", response.Header().Get("Pragma"))
	require.Equal(t, "0", response.Header().Get("Expires"))
	require.Equal(t, "nosniff", response.Header().Get("X-Content-Type-Options"))
	require.NotEmpty(t, response.Header().Get("Content-Length"))
	digest := sha256.Sum256(response.Body.Bytes())
	require.Equal(t, formatSHA256Digest(digest), response.Header().Get("Content-Digest"))

	var value EliteaClientTokenContext
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &value))
	require.Equal(t, EliteaClientTokenSchemaVersion, value.SchemaVersion)
	require.EqualValues(t, 42, value.ProjectID)
	require.Equal(t, token, value.Token)
	require.Equal(t, 1, issueCalls)
}

func TestIndexRuntimeContextUsesProjectSystemPATForScheduledExecution(t *testing.T) {
	t.Parallel()

	const token = "project.system.signature"
	actorIssueCalls := 0
	projectIssueCalls := 0
	service, err := NewEliteaClientTokenServiceWithSchedules(
		runtimeContextAuthorizerFunc(func(context.Context, ContentClaim) (RuntimeContextAuthorization, error) {
			return RuntimeContextAuthorization{
				ResourceProjectID: 42,
				ActorID:           "900",
				Initiator:         runtimeContextInitiatorSchedule,
			}, nil
		}),
		actorTokenIssuerFunc(func(context.Context, int64) (string, error) {
			actorIssueCalls++
			return "", errors.New("actor issuer must not be used")
		}),
		projectSystemTokenIssuerFunc(func(_ context.Context, projectID int64) (ProjectSystemToken, error) {
			projectIssueCalls++
			require.EqualValues(t, 42, projectID)
			return ProjectSystemToken{projectID: 42, userID: 777, token: token}, nil
		}),
		projectTokenValidatorFunc(func(_ context.Context, got string) (auth.User, error) {
			require.Equal(t, token, got)
			return auth.User{
				ID: "777", UserID: "777", TokenID: "778",
				Email: "system_user_42@centry.user", AuthType: "token",
			}, nil
		}),
		stubProjectSecretsHeader("project-header-value"),
	)
	require.NoError(t, err)

	value, err := service.Resolve(context.Background(), ContentClaim{})
	require.NoError(t, err)
	require.EqualValues(t, 42, value.ProjectID)
	require.Equal(t, token, value.Token)
	require.Zero(t, actorIssueCalls)
	require.Equal(t, 1, projectIssueCalls)
}

func TestProjectSystemIdentityPreflightReturnsNoBearerAndUsesExactPrincipal(t *testing.T) {
	t.Parallel()

	issuer := projectSystemTokenIssuerFunc(func(
		_ context.Context,
		projectID int64,
	) (ProjectSystemToken, error) {
		require.EqualValues(t, 42, projectID)
		return ProjectSystemToken{
			projectID: 42,
			userID:    777,
			token:     "project-system-token",
		}, nil
	})
	validator := projectTokenValidatorFunc(func(
		_ context.Context,
		token string,
	) (auth.User, error) {
		require.Equal(t, "project-system-token", token)
		return auth.User{
			ID: "777", UserID: "777", TokenID: "778",
			Email: "system_user_42@centry.user", AuthType: "token",
		}, nil
	})
	service, err := NewProjectSystemIdentityService(issuer, validator)
	require.NoError(t, err)
	require.NoError(t, service.CheckProjectSystemIdentity(
		context.Background(),
		42,
	))

	wrong, err := NewProjectSystemIdentityService(
		issuer,
		projectTokenValidatorFunc(func(
			context.Context,
			string,
		) (auth.User, error) {
			return validActorTokenPrincipal(), nil
		}),
	)
	require.NoError(t, err)
	err = wrong.CheckProjectSystemIdentity(context.Background(), 42)
	require.ErrorIs(t, err, ErrContentUnavailable)
	require.Equal(t, runtimeContextStagePrincipalBinding, runtimeContextUnavailableStage(err))
	require.NotContains(t, err.Error(), "project-system-token")
}

func TestIndexRuntimeContextScheduledExecutionFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		issued        ProjectSystemToken
		issue         error
		principal     auth.User
		validate      error
		expectedStage string
	}{
		{
			name:          "issuer unavailable",
			issue:         errors.New("system issuer secret canary"),
			expectedStage: runtimeContextStageSystemPATIssuance,
		},
		{
			name:          "wrong project",
			issued:        ProjectSystemToken{projectID: 41, userID: 777, token: "system-token"},
			expectedStage: runtimeContextStageSystemPATIssuance,
		},
		{
			name:          "missing system user",
			issued:        ProjectSystemToken{projectID: 42, token: "system-token"},
			expectedStage: runtimeContextStageSystemPATIssuance,
		},
		{
			name:          "inactive system PAT",
			issued:        ProjectSystemToken{projectID: 42, userID: 777, token: "system-token"},
			validate:      errors.New("inactive token canary"),
			expectedStage: runtimeContextStagePATValidation,
		},
		{
			name:          "different principal",
			issued:        ProjectSystemToken{projectID: 42, userID: 777, token: "system-token"},
			principal:     validActorTokenPrincipal(),
			expectedStage: runtimeContextStagePrincipalBinding,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			principal := test.principal
			if principal.ID == "" && test.validate == nil &&
				test.expectedStage != runtimeContextStageSystemPATIssuance {
				principal = auth.User{
					ID: "777", UserID: "777", TokenID: "778",
					Email: "system_user_42@centry.user", AuthType: "token",
				}
			}
			service, err := NewEliteaClientTokenServiceWithSchedules(
				runtimeContextAuthorizerFunc(func(context.Context, ContentClaim) (RuntimeContextAuthorization, error) {
					return RuntimeContextAuthorization{
						ResourceProjectID: 42,
						ActorID:           "900",
						Initiator:         runtimeContextInitiatorSchedule,
					}, nil
				}),
				actorTokenIssuerFunc(func(context.Context, int64) (string, error) {
					t.Fatal("actor issuer must not be called")
					return "", nil
				}),
				projectSystemTokenIssuerFunc(func(context.Context, int64) (ProjectSystemToken, error) {
					return test.issued, test.issue
				}),
				projectTokenValidatorFunc(func(context.Context, string) (auth.User, error) {
					return principal, test.validate
				}),
				stubProjectSecretsHeader("project-header-value"),
			)
			require.NoError(t, err)

			_, err = service.Resolve(context.Background(), ContentClaim{})
			require.ErrorIs(t, err, ErrContentUnavailable)
			require.Equal(t, test.expectedStage, runtimeContextUnavailableStage(err))
			require.NotContains(t, err.Error(), "canary")
		})
	}
}

func TestIndexRuntimeContextFailsClosedWithoutPrincipalFallback(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		authorization RuntimeContextAuthorization
		token         string
		issue         error
		principal     auth.User
		validate      error
		expectedStage string
	}{
		{
			name:          "missing project",
			authorization: validRuntimeContextAuthorization(),
			token:         "actor-token",
			principal:     validActorTokenPrincipal(),
			expectedStage: runtimeContextStageProjectIdentity,
		},
		{
			name: "noncanonical actor",
			authorization: RuntimeContextAuthorization{
				ResourceProjectID: 42, ActorID: "0900", Initiator: runtimeContextInitiatorUser,
			},
			token:         "actor-token",
			principal:     validActorTokenPrincipal(),
			expectedStage: runtimeContextStageExecutionActor,
		},
		{
			name: "scheduled execution",
			authorization: RuntimeContextAuthorization{
				ResourceProjectID: 42, ActorID: "900", Initiator: "schedule",
			},
			token:         "actor-token",
			principal:     validActorTokenPrincipal(),
			expectedStage: runtimeContextStageExecutionMode,
		},
		{
			name:          "missing actor PAT",
			authorization: validRuntimeContextAuthorization(),
			issue:         errors.New("PAT details must not escape"),
			principal:     validActorTokenPrincipal(),
			expectedStage: runtimeContextStageActorPATIssuance,
		},
		{
			name:          "inactive PAT",
			authorization: validRuntimeContextAuthorization(),
			token:         "actor-token",
			principal:     validActorTokenPrincipal(),
			validate:      errors.New("inactive token details must not escape"),
			expectedStage: runtimeContextStagePATValidation,
		},
		{
			name:          "different principal",
			authorization: validRuntimeContextAuthorization(),
			token:         "actor-token",
			principal: auth.User{
				ID: "901", UserID: "901", TokenID: "902", Email: "other@example.test", AuthType: "token",
			},
			expectedStage: runtimeContextStagePrincipalBinding,
		},
	}
	tests[0].authorization.ResourceProjectID = 0
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			issueCalls := 0
			service, err := NewEliteaClientTokenService(
				runtimeContextAuthorizerFunc(func(context.Context, ContentClaim) (RuntimeContextAuthorization, error) {
					return test.authorization, nil
				}),
				actorTokenIssuerFunc(func(context.Context, int64) (string, error) {
					issueCalls++
					return test.token, test.issue
				}),
				projectTokenValidatorFunc(func(context.Context, string) (auth.User, error) {
					return test.principal, test.validate
				}),
				stubProjectSecretsHeader("project-header-value"),
			)
			require.NoError(t, err)

			_, err = service.Resolve(context.Background(), ContentClaim{})
			require.ErrorIs(t, err, ErrContentUnavailable)
			require.Equal(t, test.expectedStage, runtimeContextUnavailableStage(err))
			require.NotContains(t, err.Error(), "PAT details")
			if test.expectedStage == runtimeContextStageProjectIdentity ||
				test.expectedStage == runtimeContextStageExecutionActor ||
				test.expectedStage == runtimeContextStageExecutionMode {
				require.Zero(t, issueCalls)
			}
		})
	}
}

func TestIndexRuntimeContextDistinguishesClaimDenialFromDependencyFailure(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		authorize error
		want      error
		stage     string
	}{
		"claim mismatch": {
			authorize: ErrContentUnauthorized,
			want:      ErrContentUnauthorized,
			stage:     "unknown",
		},
		"claim store unavailable": {
			authorize: errors.New("claim-store-canary"),
			want:      ErrContentUnavailable,
			stage:     runtimeContextStageClaimAuthorize,
		},
	} {
		t.Run(name, func(t *testing.T) {
			service, err := NewEliteaClientTokenService(
				runtimeContextAuthorizerFunc(func(context.Context, ContentClaim) (RuntimeContextAuthorization, error) {
					return RuntimeContextAuthorization{}, test.authorize
				}),
				actorTokenIssuerFunc(func(context.Context, int64) (string, error) {
					t.Fatal("issuer must not be called")
					return "", nil
				}),
				projectTokenValidatorFunc(func(context.Context, string) (auth.User, error) {
					t.Fatal("validator must not be called")
					return auth.User{}, nil
				}),
				stubProjectSecretsHeader("project-header-value"),
			)
			require.NoError(t, err)
			_, err = service.Resolve(context.Background(), ContentClaim{})
			require.ErrorIs(t, err, test.want)
			require.Equal(t, test.stage, runtimeContextUnavailableStage(err))
			require.NotContains(t, err.Error(), "canary")
		})
	}
}

func TestIndexRuntimeContextRouteRejectsUntrustedOrNonEmptyRequests(t *testing.T) {
	t.Parallel()

	calls := 0
	service, err := NewEliteaClientTokenService(
		runtimeContextAuthorizerFunc(func(context.Context, ContentClaim) (RuntimeContextAuthorization, error) {
			calls++
			return RuntimeContextAuthorization{}, ErrContentUnauthorized
		}),
		actorTokenIssuerFunc(func(context.Context, int64) (string, error) {
			t.Fatal("issuer must not be called")
			return "", nil
		}),
		projectTokenValidatorFunc(func(context.Context, string) (auth.User, error) {
			t.Fatal("validator must not be called")
			return auth.User{}, nil
		}),
		stubProjectSecretsHeader("project-header-value"),
	)
	require.NoError(t, err)
	server := newIndexRuntimeContextTestServer(t, service)

	missingTLS := httptest.NewRequest(http.MethodPost, "/executions/e/generations/1/runtime-context/elitea-client-token", nil)
	missingTLS.Header.Set(claimIDHeader, "claim")
	missingTLS.Header.Set(fenceHeader, base64.RawURLEncoding.EncodeToString(make([]byte, sha256.Size)))
	response := httptest.NewRecorder()
	server.Routes().ServeHTTP(response, missingTLS)
	require.Equal(t, http.StatusUnauthorized, response.Code)
	require.Zero(t, calls)
	require.Contains(t, response.Header().Get("Cache-Control"), "no-store")

	nonEmpty := validRuntimeContextRequest(t, bytes.NewBufferString("not-allowed"))
	response = httptest.NewRecorder()
	server.Routes().ServeHTTP(response, nonEmpty)
	require.Equal(t, http.StatusBadRequest, response.Code)
	require.Zero(t, calls)

	nonCanonical := validRuntimeContextRequest(t, nil)
	nonCanonical.URL.Path = "/executions/execution-1/generations/01/runtime-context/elitea-client-token"
	response = httptest.NewRecorder()
	server.Routes().ServeHTTP(response, nonCanonical)
	require.Equal(t, http.StatusUnauthorized, response.Code)
	require.Zero(t, calls)

	duplicateClaim := validRuntimeContextRequest(t, nil)
	duplicateClaim.Header.Add(claimIDHeader, "claim-2")
	response = httptest.NewRecorder()
	server.Routes().ServeHTTP(response, duplicateClaim)
	require.Equal(t, http.StatusUnauthorized, response.Code)
	require.Zero(t, calls)

	overflowGeneration := validRuntimeContextRequest(t, nil)
	overflowGeneration.URL.Path = "/executions/execution-1/generations/9223372036854775808/runtime-context/elitea-client-token"
	response = httptest.NewRecorder()
	server.Routes().ServeHTTP(response, overflowGeneration)
	require.Equal(t, http.StatusUnauthorized, response.Code)
	require.Zero(t, calls)
}

func TestIndexRuntimeContextRouteSharesContentCapacityAndIsNotMountedByDefault(t *testing.T) {
	t.Parallel()

	service, err := NewEliteaClientTokenService(
		runtimeContextAuthorizerFunc(func(context.Context, ContentClaim) (RuntimeContextAuthorization, error) {
			t.Fatal("saturated request must not authorize")
			return RuntimeContextAuthorization{}, nil
		}),
		actorTokenIssuerFunc(func(context.Context, int64) (string, error) {
			t.Fatal("saturated request must not issue")
			return "", nil
		}),
		projectTokenValidatorFunc(func(context.Context, string) (auth.User, error) {
			t.Fatal("saturated request must not validate")
			return auth.User{}, nil
		}),
		stubProjectSecretsHeader("project-header-value"),
	)
	require.NoError(t, err)
	server := newIndexRuntimeContextTestServer(t, service)
	server.requests <- struct{}{}
	response := httptest.NewRecorder()
	server.Routes().ServeHTTP(response, validRuntimeContextRequest(t, nil))
	<-server.requests
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	require.Equal(t, "1", response.Header().Get("Retry-After"))

	ordinary, err := NewContentServer(
		contentAuthorizerFunc(func(context.Context, ContentClaim) (ContentAuthorization, error) {
			return ContentAuthorization{}, nil
		}),
		contentStoreFunc(func(context.Context, string, string, string, string) (io.ReadCloser, error) {
			return nil, nil
		}),
		1024,
	)
	require.NoError(t, err)
	response = httptest.NewRecorder()
	ordinary.Routes().ServeHTTP(response, validRuntimeContextRequest(t, nil))
	require.Equal(t, http.StatusNotFound, response.Code)
}

func TestIndexRuntimeContextFailureLogContainsOnlyOneBoundedStage(t *testing.T) {
	t.Parallel()

	service, err := NewEliteaClientTokenService(
		runtimeContextAuthorizerFunc(func(context.Context, ContentClaim) (RuntimeContextAuthorization, error) {
			return validRuntimeContextAuthorization(), nil
		}),
		actorTokenIssuerFunc(func(context.Context, int64) (string, error) {
			return "", errors.New("issuer-secret-canary")
		}),
		projectTokenValidatorFunc(func(context.Context, string) (auth.User, error) {
			t.Fatal("validator must not be called")
			return auth.User{}, nil
		}),
		stubProjectSecretsHeader("project-header-value"),
	)
	require.NoError(t, err)
	server := newIndexRuntimeContextTestServer(t, service)
	var logs bytes.Buffer
	server.logger = slog.New(slog.NewJSONHandler(&logs, nil))

	response := httptest.NewRecorder()
	server.Routes().ServeHTTP(response, validRuntimeContextRequest(t, nil))

	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	require.NotContains(t, response.Body.String(), "canary")
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	require.Len(t, lines, 1)
	require.Contains(t, lines[0], `"msg":"runtime context unavailable"`)
	require.Contains(t, lines[0], `"stage":"actor_pat_issuance"`)
	require.NotContains(t, lines[0], "issuer-secret-canary")
	require.NotContains(t, lines[0], "execution-1")

	unknown := runtimeContextUnavailable("untrusted-stage-canary")
	require.Equal(t, "unknown", runtimeContextUnavailableStage(unknown))
}

func newIndexRuntimeContextTestServer(t *testing.T, service *EliteaClientTokenService) *ContentServer {
	t.Helper()
	server, err := NewRuntimeContentServerWithLimits(
		contentAuthorizerFunc(func(context.Context, ContentClaim) (ContentAuthorization, error) {
			t.Fatal("runtime-context route must not call content-entry authorization")
			return ContentAuthorization{}, nil
		}),
		contentStoreFunc(func(context.Context, string, string, string, string) (io.ReadCloser, error) {
			t.Fatal("runtime-context route must not open input content")
			return nil, nil
		}),
		service,
		1024,
		1,
	)
	require.NoError(t, err)
	return server
}

func validRuntimeContextRequest(t *testing.T, body io.Reader) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/executions/execution-1/generations/1/runtime-context/elitea-client-token", body)
	request.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{{}}}}
	request.Header.Set(claimIDHeader, "claim-1")
	request.Header.Set(fenceHeader, base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, sha256.Size)))
	return request
}

func validRuntimeContextAuthorization() RuntimeContextAuthorization {
	return RuntimeContextAuthorization{
		ResourceProjectID: 42,
		ActorID:           "900",
		Initiator:         runtimeContextInitiatorUser,
	}
}

func validActorTokenPrincipal() auth.User {
	return auth.User{
		ID: "900", UserID: "900", TokenID: "901",
		Email: "actor@example.test", AuthType: "token",
	}
}
