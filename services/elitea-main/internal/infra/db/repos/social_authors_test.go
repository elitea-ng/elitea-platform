package repos

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5/pgtype"
)

type currentSocialAuthorsQueriesStub struct {
	rows      []sqlcgen.ListCurrentProjectAuthorsRow
	err       error
	calls     int
	projectID int32
	context   context.Context
}

func (stub *currentSocialAuthorsQueriesStub) ListCurrentProjectAuthors(
	ctx context.Context,
	projectID int32,
) ([]sqlcgen.ListCurrentProjectAuthorsRow, error) {
	stub.calls++
	stub.projectID = projectID
	stub.context = ctx
	return stub.rows, stub.err
}

func TestCurrentSocialAuthorsRepositoryMapsOneGeneratedQuery(t *testing.T) {
	t.Parallel()

	email := "member@example.test"
	name := "Member"
	avatar := "avatar-data"
	stored := time.Date(2026, time.July, 27, 11, 12, 13, 987654321, time.FixedZone("source", 3*60*60))
	queries := &currentSocialAuthorsQueriesStub{
		rows: []sqlcgen.ListCurrentProjectAuthorsRow{{
			ID:        41,
			Email:     &email,
			Name:      &name,
			LastLogin: pgtype.Timestamp{Time: stored, Valid: true},
			Suspended: true,
			Avatar:    &avatar,
		}, {
			ID: 42,
		}},
	}
	repository, err := newCurrentSocialAuthorsRepository(queries)
	if err != nil {
		t.Fatal(err)
	}

	authors, err := repository.ListCurrentProjectAuthors(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if queries.calls != 1 || queries.projectID != 7 {
		t.Fatalf("query calls=%d project=%d", queries.calls, queries.projectID)
	}
	deadline, ok := queries.context.Deadline()
	remaining := time.Until(deadline)
	if !ok || remaining <= 0 || remaining > CurrentSocialAuthorsQueryTimeout {
		t.Fatalf("query deadline present=%t remaining=%v", ok, remaining)
	}
	if len(authors) != 2 || authors[0].ID != 41 || authors[0].Email == nil ||
		*authors[0].Email != email || authors[0].Name == nil || *authors[0].Name != name ||
		authors[0].Avatar == nil || *authors[0].Avatar != avatar || !authors[0].Suspended {
		t.Fatalf("authors=%+v", authors)
	}
	wantUTC := time.Date(2026, time.July, 27, 11, 12, 13, 987654321, time.UTC)
	if authors[0].LastLogin == nil || !authors[0].LastLogin.Equal(wantUTC) ||
		authors[0].LastLogin.Location() != time.UTC {
		t.Fatalf("last_login=%v want=%v", authors[0].LastLogin, wantUTC)
	}
	if authors[1].LastLogin != nil {
		t.Fatalf("null last_login=%v", authors[1].LastLogin)
	}
}

func TestCurrentSocialAuthorsRepositoryRejectsBeforeQueryAndPreservesErrors(t *testing.T) {
	t.Parallel()

	privateFailure := errors.New("private database detail")
	queries := &currentSocialAuthorsQueriesStub{err: privateFailure}
	repository, err := newCurrentSocialAuthorsRepository(queries)
	if err != nil {
		t.Fatal(err)
	}

	if _, gotErr := repository.ListCurrentProjectAuthors(context.Background(), 7); !errors.Is(gotErr, privateFailure) {
		t.Fatalf("database error=%v", gotErr)
	}
	if queries.calls != 1 {
		t.Fatalf("query calls=%d want=1", queries.calls)
	}

	queries.calls = 0
	for _, test := range []struct {
		name      string
		ctx       context.Context
		projectID int32
		want      error
	}{
		{name: "nil context", projectID: 7, want: ErrInvalidCurrentSocialAuthorsRequest},
		{name: "invalid project", ctx: context.Background(), want: ErrInvalidCurrentSocialAuthorsRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, gotErr := repository.ListCurrentProjectAuthors(test.ctx, test.projectID); !errors.Is(gotErr, test.want) {
				t.Fatalf("error=%v want=%v", gotErr, test.want)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, gotErr := repository.ListCurrentProjectAuthors(ctx, 7); !errors.Is(gotErr, context.Canceled) {
		t.Fatalf("cancelled error=%v", gotErr)
	}
	if queries.calls != 0 {
		t.Fatalf("invalid requests issued %d queries", queries.calls)
	}
}

func TestNewCurrentSocialAuthorsRepositoryRejectsMissingDatabase(t *testing.T) {
	t.Parallel()

	if _, err := NewCurrentSocialAuthorsRepository(nil); err == nil {
		t.Fatal("expected missing-pool error")
	}
	if _, err := newCurrentSocialAuthorsRepository(nil); err == nil {
		t.Fatal("expected missing-query error")
	}
}
