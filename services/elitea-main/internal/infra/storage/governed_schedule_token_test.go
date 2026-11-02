package storage

import (
	"context"
	"errors"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/runtoken"
	"github.com/stretchr/testify/require"
)

// A governed scheduled run is refused the project system token (#1122): the
// issuer is never called, and nothing falls back to the actor's token.
func TestGovernedScheduledRunNeverIssuesTheProjectToken(t *testing.T) {
	t.Parallel()

	projectIssueCalls, actorIssueCalls := 0, 0
	service, err := NewEliteaClientTokenServiceWithSchedules(
		runtimeContextAuthorizerFunc(func(context.Context, ContentClaim) (RuntimeContextAuthorization, error) {
			return RuntimeContextAuthorization{
				ResourceProjectID: 42, ActorID: "900", Initiator: runtimeContextInitiatorSchedule,
			}, nil
		}),
		actorTokenIssuerFunc(func(context.Context, int64) (string, error) {
			actorIssueCalls++
			return "", errors.New("actor issuer must not be used")
		}),
		projectSystemTokenIssuerFunc(func(context.Context, int64) (ProjectSystemToken, error) {
			projectIssueCalls++
			return ProjectSystemToken{projectID: 42, userID: 777, token: "project.system.signature"}, nil
		}),
		projectTokenValidatorFunc(func(context.Context, string) (auth.User, error) {
			return auth.User{ID: "777", UserID: "777", TokenID: "778", AuthType: "token"}, nil
		}),
		stubProjectSecretsHeader("project-header-value"),
	)
	require.NoError(t, err)

	_, err = service.ResolveAccepting(context.Background(), ContentClaim{}, RuntimeContextAccept{GovernedRun: true})
	require.Error(t, err)
	require.Zero(t, projectIssueCalls)
	require.Zero(t, actorIssueCalls)

	// The same claim ungoverned still gets the token: flag off is unchanged.
	value, err := service.ResolveAccepting(context.Background(), ContentClaim{}, RuntimeContextAccept{})
	require.NoError(t, err)
	require.Equal(t, "project.system.signature", value.Token)
	require.Equal(t, 1, projectIssueCalls)
}

// A governed scheduled run with run tokens composed gets a run token
// (#1473): it names the run's actor and that actor's token row, and the
// project system token is never issued. The actor's token itself is not
// returned.
func TestGovernedScheduledRunGetsARunTokenNotTheProjectToken(t *testing.T) {
	t.Parallel()

	projectIssueCalls, actorIssueCalls := 0, 0
	service, err := NewEliteaClientTokenServiceWithSchedules(
		runtimeContextAuthorizerFunc(func(context.Context, ContentClaim) (RuntimeContextAuthorization, error) {
			return RuntimeContextAuthorization{
				ResourceProjectID: 42, ActorID: "900", Initiator: runtimeContextInitiatorSchedule,
			}, nil
		}),
		actorTokenIssuerFunc(func(_ context.Context, actorID int64) (string, error) {
			actorIssueCalls++
			require.Equal(t, int64(900), actorID)
			return "actor.pat.signature", nil
		}),
		projectSystemTokenIssuerFunc(func(context.Context, int64) (ProjectSystemToken, error) {
			projectIssueCalls++
			return ProjectSystemToken{projectID: 42, userID: 777, token: "project.system.signature"}, nil
		}),
		projectTokenValidatorFunc(func(_ context.Context, token string) (auth.User, error) {
			if token == "project.system.signature" {
				return auth.User{ID: "777", UserID: "777", TokenID: "778", AuthType: "token"}, nil
			}
			require.Equal(t, "actor.pat.signature", token)
			return auth.User{ID: "900", UserID: "900", TokenID: "901", AuthType: "token"}, nil
		}),
		stubProjectSecretsHeader("project-header-value"),
	)
	require.NoError(t, err)
	var issued []runtoken.Binding
	service.WithRunTokens(runTokenIssuerFunc(func(binding runtoken.Binding) (string, error) {
		issued = append(issued, binding)
		return runtoken.Prefix + "scheduled", nil
	}))

	claim := ContentClaim{ExecutionID: "execution-1", Generation: 1, ClaimID: "claim-1", FenceToken: make([]byte, 32)}
	value, err := service.ResolveAccepting(context.Background(), claim, RuntimeContextAccept{GovernedRun: true})
	require.NoError(t, err)
	require.Equal(t, runtoken.Prefix+"scheduled", value.Token)
	require.Empty(t, value.SecretsHeaderValue)
	require.Zero(t, projectIssueCalls)
	require.Equal(t, 1, actorIssueCalls)
	require.Equal(t, []runtoken.Binding{{
		ExecutionID: "execution-1", Generation: 1, ClaimID: "claim-1", FenceToken: make([]byte, 32),
		ProjectID: 42, UserID: 900, TokenID: 901,
	}}, issued)

	// Flag off (no governed pin): the project system token, as before.
	value, err = service.ResolveAccepting(context.Background(), claim, RuntimeContextAccept{})
	require.NoError(t, err)
	require.Equal(t, "project.system.signature", value.Token)
	require.Equal(t, 1, projectIssueCalls)
}
