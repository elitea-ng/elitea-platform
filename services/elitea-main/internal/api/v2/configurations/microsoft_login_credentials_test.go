package configurations

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftlogin"
)

func TestMicrosoftLoginCredentialsAddressesByUUIDOnly(t *testing.T) {
	// A numeric id would match the serial column, which no client of the sign-in
	// holds; it is refused before any query, as is an id of an unreadable project.
	credentials := NewHandler(nil).MicrosoftLoginCredentials()
	for _, id := range []string{"7", "0", "-1", "00000007"} {
		_, err := credentials.Load(context.Background(), 7, id)
		require.ErrorIs(t, err, microsoftlogin.ErrCredentialNotFound, id)
	}
	_, err := credentials.Load(context.Background(), 0, "aaaaaaaa-0000-4000-8000-000000000001")
	require.ErrorIs(t, err, microsoftlogin.ErrCredentialNotFound)
}
