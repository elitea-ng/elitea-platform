package authsvc

// The principal store gives two different answers, and they must stay
// separable (#537).
//
// pgx.ErrNoRows is the store ANSWERING: there is no active row of this kind.
// Every other error is the store NOT answering, and the principal was never
// read. Both used to leave this package as an error the caller could not
// classify, so the caller answered 401 to both.

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
)

var errPoolTimeout = errors.New("timeout: context deadline exceeded (pool)")

func TestValidatePrincipalMarksAStoreFaultAsUnavailable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		principal auth.User
		queries   principalQueriesStub
	}{
		{
			name:      "user",
			principal: auth.User{ID: "7", UserID: "7", AuthType: "user"},
			queries: principalQueriesStub{
				activeUser: func(context.Context, int32) (sqlcgen.GetActiveUserPrincipalByIDRow, error) {
					return sqlcgen.GetActiveUserPrincipalByIDRow{}, errPoolTimeout
				},
			},
		},
		{
			name:      "token",
			principal: auth.User{ID: "42", TokenID: "42", UserID: "7", AuthType: "token"},
			queries: principalQueriesStub{
				activePAT: func(context.Context, int32) (sqlcgen.GetActivePATPrincipalByIDRow, error) {
					return sqlcgen.GetActivePATPrincipalByIDRow{}, errPoolTimeout
				},
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			validator := &PrincipalValidator{queries: testCase.queries}

			_, err := validator.ValidatePrincipal(context.Background(), testCase.principal)

			if !errors.Is(err, ErrPrincipalUnavailable) {
				t.Fatalf("error = %v, want it to carry ErrPrincipalUnavailable", err)
			}
			if errors.Is(err, ErrPrincipalInactive) {
				t.Fatalf("error = %v: a store fault must not read as an inactive principal", err)
			}
			if !errors.Is(err, errPoolTimeout) {
				t.Fatalf("error = %v: the cause must stay in the chain for the log line", err)
			}
		})
	}
}

func TestValidatePrincipalKeepsNoRowsInactive(t *testing.T) {
	t.Parallel()

	validator := &PrincipalValidator{queries: principalQueriesStub{
		activeUser: func(context.Context, int32) (sqlcgen.GetActiveUserPrincipalByIDRow, error) {
			return sqlcgen.GetActiveUserPrincipalByIDRow{}, pgx.ErrNoRows
		},
	}}

	_, err := validator.ValidatePrincipal(context.Background(),
		auth.User{ID: "7", UserID: "7", AuthType: "user"})

	if !errors.Is(err, ErrPrincipalInactive) {
		t.Fatalf("error = %v, want ErrPrincipalInactive", err)
	}
	if errors.Is(err, ErrPrincipalUnavailable) {
		t.Fatalf("error = %v: a suspended principal is not a store fault", err)
	}
}

func TestUnavailableErrorUsesStorageNeutralAuthContract(t *testing.T) {
	t.Parallel()

	if ErrPrincipalUnavailable != auth.ErrPrincipalUnavailable {
		t.Fatalf("ErrPrincipalUnavailable = %v, want storage-neutral %v",
			ErrPrincipalUnavailable, auth.ErrPrincipalUnavailable)
	}
	if errors.Is(auth.ErrPrincipalUnavailable, auth.ErrPrincipalInactive) {
		t.Fatal("the two sentinels match each other, so no caller can tell them apart")
	}
}
