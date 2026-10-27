package storage

import (
	"context"
	"errors"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
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
