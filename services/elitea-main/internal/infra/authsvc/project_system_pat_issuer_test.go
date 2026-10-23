package authsvc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
)

type activeProjectSystemPATQueriesFunc func(
	context.Context,
	int32,
) (sqlcgen.GetActiveProjectSystemPATRow, error)

func (f activeProjectSystemPATQueriesFunc) GetActiveProjectSystemPAT(
	ctx context.Context,
	projectID int32,
) (sqlcgen.GetActiveProjectSystemPATRow, error) {
	return f(ctx, projectID)
}

func TestProjectSystemIssuerPreservesExactCurrentIdentity(t *testing.T) {
	const (
		projectID = int64(42)
		userID    = int32(74)
		uuid      = "8ce4be49-0d10-4f05-a63f-d6d46f99a3f0"
	)
	issuer := &ProjectSystemIssuer{
		secretKey: []byte("secret"),
		queries: activeProjectSystemPATQueriesFunc(func(
			_ context.Context,
			gotProjectID int32,
		) (sqlcgen.GetActiveProjectSystemPATRow, error) {
			if int64(gotProjectID) != projectID {
				t.Fatalf("project ID = %d, want %d", gotProjectID, projectID)
			}
			return sqlcgen.GetActiveProjectSystemPATRow{
				ProjectID: int32(projectID),
				UserID:    userID,
				TokenID:   9,
				Uuid:      pointer(uuid),
				Email:     "system_user_42@centry.user",
			}, nil
		}),
	}

	got, err := issuer.IssueProjectToken(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProjectID() != projectID || got.UserID() != int64(userID) || got.Token() == "" {
		t.Fatalf("identity = project %d, user %d, token empty=%t", got.ProjectID(), got.UserID(), got.Token() == "")
	}
	if fmt.Sprint(got) != "authsvc.ProjectSystemToken{redacted}" ||
		fmt.Sprintf("%#v", got) != "authsvc.ProjectSystemToken{redacted}" {
		t.Fatalf("project-system token string representation is not redacted")
	}
}

func TestProjectSystemIssuerFailsClosedWithoutExactProjectIdentity(t *testing.T) {
	valid := sqlcgen.GetActiveProjectSystemPATRow{
		ProjectID: 42,
		UserID:    74,
		TokenID:   9,
		Uuid:      pointer("8ce4be49-0d10-4f05-a63f-d6d46f99a3f0"),
		Email:     "system_user_42@centry.user",
	}
	for name, test := range map[string]struct {
		mutate   func(*sqlcgen.GetActiveProjectSystemPATRow)
		queryErr error
		want     error
		contains string
	}{
		"missing": {
			queryErr: pgx.ErrNoRows,
			want:     ErrTokenRejected,
			contains: "active project-system PAT not found",
		},
		"database unavailable": {
			queryErr: errors.New("database-canary"),
			want:     ErrTokenValidationUnavailable,
			contains: "database-canary",
		},
		"cross project": {
			mutate:   func(row *sqlcgen.GetActiveProjectSystemPATRow) { row.ProjectID = 43 },
			want:     ErrTokenValidationUnavailable,
			contains: "invalid identity data",
		},
		"wrong system email": {
			mutate:   func(row *sqlcgen.GetActiveProjectSystemPATRow) { row.Email = "system_user_43@centry.user" },
			want:     ErrTokenValidationUnavailable,
			contains: "invalid identity data",
		},
		"missing system user": {
			mutate:   func(row *sqlcgen.GetActiveProjectSystemPATRow) { row.UserID = 0 },
			want:     ErrTokenValidationUnavailable,
			contains: "invalid identity data",
		},
		"missing PAT uuid": {
			mutate:   func(row *sqlcgen.GetActiveProjectSystemPATRow) { row.Uuid = nil },
			want:     ErrTokenValidationUnavailable,
			contains: "invalid identity data",
		},
	} {
		t.Run(name, func(t *testing.T) {
			row := valid
			if test.mutate != nil {
				test.mutate(&row)
			}
			issuer := &ProjectSystemIssuer{
				secretKey: []byte("secret"),
				queries: activeProjectSystemPATQueriesFunc(func(
					context.Context,
					int32,
				) (sqlcgen.GetActiveProjectSystemPATRow, error) {
					return row, test.queryErr
				}),
			}
			_, err := issuer.IssueProjectToken(context.Background(), 42)
			if !errors.Is(err, test.want) || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("error = %v, want %v containing %q", err, test.want, test.contains)
			}
		})
	}
}

func TestProjectSystemIssuerDoesNotFallbackOrCreate(t *testing.T) {
	calls := 0
	issuer := &ProjectSystemIssuer{
		secretKey: []byte("secret"),
		queries: activeProjectSystemPATQueriesFunc(func(
			context.Context,
			int32,
		) (sqlcgen.GetActiveProjectSystemPATRow, error) {
			calls++
			return sqlcgen.GetActiveProjectSystemPATRow{}, pgx.ErrNoRows
		}),
	}
	if _, err := issuer.IssueProjectToken(context.Background(), 42); !errors.Is(err, ErrTokenRejected) {
		t.Fatalf("error = %v, want ErrTokenRejected", err)
	}
	if calls != 1 {
		t.Fatalf("lookup calls = %d, want one exact project lookup", calls)
	}
}

func TestProjectSystemIssuerSnapshotsSigningKey(t *testing.T) {
	key := []byte("secret")
	issuer := NewProjectSystemIssuerBytes(nil, key)
	clear(key)
	if string(issuer.secretKey) != "secret" {
		t.Fatalf("issuer key did not survive source clearing")
	}
}
