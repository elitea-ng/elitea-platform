package authsvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
)

type currentUserQueriesStub struct {
	get func(context.Context, int32) (sqlcgen.AuthCoreUser, error)
}

func (s currentUserQueriesStub) GetCurrentActiveAuthUser(
	ctx context.Context,
	userID int32,
) (sqlcgen.AuthCoreUser, error) {
	if s.get == nil {
		return sqlcgen.AuthCoreUser{}, pgx.ErrNoRows
	}
	return s.get(ctx, userID)
}

func TestCurrentUserResolverUsesOwningUserAndPreservesNullableFields(t *testing.T) {
	lastLogin := time.Date(2026, time.July, 19, 12, 30, 0, 0, time.UTC)
	resolver := &CurrentUserResolver{queries: currentUserQueriesStub{
		get: func(_ context.Context, userID int32) (sqlcgen.AuthCoreUser, error) {
			if userID != 7 {
				t.Fatalf("user ID = %d, want 7", userID)
			}
			return sqlcgen.AuthCoreUser{
				ID:        7,
				Email:     stringPointer("owner@example.test"),
				Name:      nil,
				LastLogin: pgtype.Timestamp{Time: lastLogin, Valid: true},
				Suspended: false,
			}, nil
		},
	}}

	got, err := resolver.Resolve(context.Background(), auth.User{
		ID:       "42",
		TokenID:  "42",
		UserID:   "7",
		Email:    "untrusted@example.test",
		AuthType: "token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != 7 || got.Email == nil || *got.Email != "owner@example.test" || got.Name != nil {
		t.Fatalf("current user = %+v", got)
	}
	if got.LastLogin == nil || !got.LastLogin.Equal(lastLogin) {
		t.Fatalf("last login = %v, want %v", got.LastLogin, lastLogin)
	}
}

func TestCurrentUserResolverPreservesNullLastLogin(t *testing.T) {
	resolver := &CurrentUserResolver{queries: currentUserQueriesStub{
		get: func(context.Context, int32) (sqlcgen.AuthCoreUser, error) {
			return sqlcgen.AuthCoreUser{ID: 7}, nil
		},
	}}

	got, err := resolver.Resolve(
		context.Background(),
		auth.User{ID: "7", UserID: "7", AuthType: "session"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != nil || got.Name != nil || got.LastLogin != nil {
		t.Fatalf("nullable fields = email:%v name:%v last_login:%v", got.Email, got.Name, got.LastLogin)
	}
}

func TestCurrentUserResolverRejectsIncompleteTokenIdentityBeforeLookup(t *testing.T) {
	calls := 0
	resolver := &CurrentUserResolver{queries: currentUserQueriesStub{
		get: func(context.Context, int32) (sqlcgen.AuthCoreUser, error) {
			calls++
			return sqlcgen.AuthCoreUser{}, nil
		},
	}}

	_, err := resolver.Resolve(
		context.Background(),
		auth.User{ID: "42", TokenID: "42", AuthType: "token"},
	)
	if !errors.Is(err, ErrCurrentUserUnavailable) {
		t.Fatalf("error = %v, want ErrCurrentUserUnavailable", err)
	}
	if calls != 0 {
		t.Fatalf("database lookups = %d, want 0", calls)
	}
}

func TestCurrentUserResolverRejectsMissingSuspendedOrMismatchedRows(t *testing.T) {
	for _, test := range []struct {
		name string
		row  sqlcgen.AuthCoreUser
		err  error
	}{
		{name: "missing", err: pgx.ErrNoRows},
		{name: "suspended", row: sqlcgen.AuthCoreUser{ID: 7, Suspended: true}},
		{name: "mismatched row", row: sqlcgen.AuthCoreUser{ID: 8}},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolver := &CurrentUserResolver{queries: currentUserQueriesStub{
				get: func(context.Context, int32) (sqlcgen.AuthCoreUser, error) {
					return test.row, test.err
				},
			}}

			_, err := resolver.Resolve(
				context.Background(),
				auth.User{ID: "7", UserID: "7", AuthType: "session"},
			)
			if !errors.Is(err, ErrCurrentUserUnavailable) {
				t.Fatalf("error = %v, want ErrCurrentUserUnavailable", err)
			}
		})
	}
}

func TestCurrentUserResolverWrapsDatabaseFailure(t *testing.T) {
	databaseErr := errors.New("database unavailable")
	resolver := &CurrentUserResolver{queries: currentUserQueriesStub{
		get: func(context.Context, int32) (sqlcgen.AuthCoreUser, error) {
			return sqlcgen.AuthCoreUser{}, databaseErr
		},
	}}

	_, err := resolver.Resolve(
		context.Background(),
		auth.User{ID: "7", UserID: "7", AuthType: "session"},
	)
	if !errors.Is(err, databaseErr) {
		t.Fatalf("error = %v, want wrapped database error", err)
	}
}

func TestCurrentUserResolverRejectsIDsOutsideBaselineDatabaseRange(t *testing.T) {
	resolver := &CurrentUserResolver{queries: currentUserQueriesStub{
		get: func(context.Context, int32) (sqlcgen.AuthCoreUser, error) {
			t.Fatal("database lookup must not run")
			return sqlcgen.AuthCoreUser{}, nil
		},
	}}

	_, err := resolver.Resolve(
		context.Background(),
		auth.User{ID: "2147483648", UserID: "2147483648", AuthType: "session"},
	)
	if !errors.Is(err, ErrCurrentUserUnavailable) {
		t.Fatalf("error = %v, want ErrCurrentUserUnavailable", err)
	}
}

func stringPointer(value string) *string {
	return &value
}
