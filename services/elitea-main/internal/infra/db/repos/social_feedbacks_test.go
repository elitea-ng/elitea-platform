package repos

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestCurrentSocialFeedbacksRepositoryInsertsIntoSharedCentryTable(t *testing.T) {
	referrer := "https://elitea.example/app/chat"
	executor := &scriptedExecutor{
		rowResults: []scriptedRow{{values: []any{int64(73)}}},
	}
	repository, err := newCurrentSocialFeedbacksRepository(executor)
	if err != nil {
		t.Fatal(err)
	}

	id, err := repository.CreateCurrentFeedback(
		context.Background(),
		41,
		7,
		"current feedback",
		5,
		&referrer,
		"EliteaUI/current",
	)
	if err != nil {
		t.Fatal(err)
	}
	if id != 73 || len(executor.rowCalls) != 1 {
		t.Fatalf("id=%d row_calls=%d", id, len(executor.rowCalls))
	}
	call := executor.rowCalls[0]
	normalizedSQL := strings.Join(strings.Fields(call.sql), " ")
	if !strings.HasPrefix(normalizedSQL, "INSERT INTO centry.social_feedbacks ( user_id, referrer, description, rating, user_agent, project_id ) SELECT $1::int, $2::varchar, $3::text, $4::int, $5::varchar, $6::int WHERE EXISTS (") ||
		!strings.Contains(normalizedSQL, "project_id = $6 AND user_id = $1") ||
		!strings.HasSuffix(normalizedSQL, "RETURNING id") ||
		strings.Contains(normalizedSQL, "p_") {
		t.Fatalf("unexpected feedback SQL: %s", normalizedSQL)
	}
	if len(call.args) != 6 || call.args[5] != int64(7) ||
		call.args[0] != int64(41) ||
		call.args[1] != &referrer ||
		call.args[2] != "current feedback" ||
		call.args[3] != 5 ||
		call.args[4] != "EliteaUI/current" {
		t.Fatalf("feedback args=%#v", call.args)
	}
}

func TestCurrentSocialFeedbacksRepositoryPreservesOptionalReferrerAndErrors(t *testing.T) {
	databaseFailure := errors.New("database unavailable")
	executor := &scriptedExecutor{
		rowResults: []scriptedRow{
			{err: databaseFailure},
			{values: []any{int64(0)}},
		},
	}
	repository, err := newCurrentSocialFeedbacksRepository(executor)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := repository.CreateCurrentFeedback(
		context.Background(),
		41,
		7,
		"",
		0,
		nil,
		"",
	); !errors.Is(err, databaseFailure) {
		t.Fatalf("database error=%v", err)
	}
	if len(executor.rowCalls) != 1 || executor.rowCalls[0].args[1] != (*string)(nil) {
		t.Fatalf("optional referrer args=%#v", executor.rowCalls)
	}

	if _, err := repository.CreateCurrentFeedback(
		context.Background(),
		41,
		7,
		"",
		0,
		nil,
		"",
	); err == nil {
		t.Fatal("non-positive returned ID was accepted")
	}
}

func TestCurrentSocialFeedbacksRepositoryRejectsInvalidInputsBeforeSQL(t *testing.T) {
	for name, test := range map[string]struct {
		ctx       context.Context
		userID    int64
		projectID int64
		rating    int
	}{
		"nil context":       {userID: 41, projectID: 7, rating: 5},
		"invalid user":      {ctx: context.Background(), projectID: 7, rating: 5},
		"invalid project":   {ctx: context.Background(), userID: 41, rating: 5},
		"rating below zero": {ctx: context.Background(), userID: 41, projectID: 7, rating: -1},
		"rating above five": {ctx: context.Background(), userID: 41, projectID: 7, rating: 6},
		"canceled context":  {ctx: canceledCurrentFeedbackContext(), userID: 41, projectID: 7, rating: 5},
	} {
		t.Run(name, func(t *testing.T) {
			executor := &scriptedExecutor{}
			repository, err := newCurrentSocialFeedbacksRepository(executor)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := repository.CreateCurrentFeedback(
				test.ctx,
				test.userID,
				test.projectID,
				"feedback",
				test.rating,
				nil,
				"",
			); err == nil {
				t.Fatal("invalid feedback was accepted")
			}
			if len(executor.rowCalls) != 0 {
				t.Fatalf("invalid feedback issued %d SQL calls", len(executor.rowCalls))
			}
		})
	}

	if _, err := newCurrentSocialFeedbacksRepository(nil); err == nil {
		t.Fatal("nil SQL store was accepted")
	}
	if _, err := NewCurrentSocialFeedbacksRepository(nil); err == nil {
		t.Fatal("nil PostgreSQL pool was accepted")
	}
}

func TestCurrentSocialFeedbacksRepositoryMapsNoRowToForbidden(t *testing.T) {
	executor := &scriptedExecutor{rowResults: []scriptedRow{{err: pgx.ErrNoRows}}}
	repository, err := newCurrentSocialFeedbacksRepository(executor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CreateCurrentFeedback(
		context.Background(), 41, 7, "feedback", 5, nil, "",
	); !errors.Is(err, ErrSocialFeedbackForbidden) {
		t.Fatalf("non-member insert error=%v", err)
	}
}

func TestCurrentSocialFeedbacksRepositoryListUsesOneConstantOrderedStatement(t *testing.T) {
	for _, test := range []struct {
		page      FeedbackPage
		wantOrder string
	}{
		{FeedbackPage{Limit: 10, SortBy: "id"}, "id ASC"},
		{FeedbackPage{Limit: 10, SortBy: "id", Desc: true}, "id DESC"},
		{FeedbackPage{Limit: 10, SortBy: "created_at"}, "created_at ASC, id ASC"},
		{FeedbackPage{Limit: 10, SortBy: "created_at", Desc: true}, "created_at DESC, id DESC"},
	} {
		executor := &scriptedExecutor{rowResults: []scriptedRow{{values: []any{
			true, int64(1), []byte(`[{"id":3,"user_id":41,"project_id":null,"referrer":null,` +
				`"description":"d","rating":4,"user_agent":null,"created_at":"2026-10-09T10:00:00.5"}]`),
		}}}}
		repository, err := newCurrentSocialFeedbacksRepository(executor)
		if err != nil {
			t.Fatal(err)
		}
		list, err := repository.ListCurrentFeedback(context.Background(), 41, 7, true, test.page)
		if err != nil {
			t.Fatal(err)
		}
		if list.Total != 1 || len(list.Rows) != 1 || list.Rows[0].ID != 3 || list.Rows[0].ProjectID != nil {
			t.Fatalf("list=%#v", list)
		}
		if len(executor.rowCalls) != 1 {
			t.Fatalf("row_calls=%d", len(executor.rowCalls))
		}
		call := executor.rowCalls[0]
		normalized := strings.Join(strings.Fields(call.sql), " ")
		if !strings.Contains(normalized, "ORDER BY "+test.wantOrder+" LIMIT $4 OFFSET $5") ||
			!strings.Contains(normalized, "project_id = $1 AND user_id = $2") {
			t.Fatalf("unexpected list SQL: %s", normalized)
		}
		if len(call.args) != 5 || call.args[0] != int64(7) || call.args[1] != int64(41) ||
			call.args[2] != true || call.args[3] != 10 || call.args[4] != 0 {
			t.Fatalf("list args=%#v", call.args)
		}
	}
}

func TestCurrentSocialFeedbacksRepositoryListRefusesNonMemberAndBadInput(t *testing.T) {
	executor := &scriptedExecutor{rowResults: []scriptedRow{{values: []any{false, int64(0), []byte(`[]`)}}}}
	repository, err := newCurrentSocialFeedbacksRepository(executor)
	if err != nil {
		t.Fatal(err)
	}
	page := FeedbackPage{Limit: 10, SortBy: "id"}
	if _, err := repository.ListCurrentFeedback(context.Background(), 41, 7, false, page); !errors.Is(err, ErrSocialFeedbackForbidden) {
		t.Fatalf("non-member list error=%v", err)
	}

	executor = &scriptedExecutor{}
	repository, _ = newCurrentSocialFeedbacksRepository(executor)
	for name, bad := range map[string]FeedbackPage{
		"limit zero":      {Limit: 0, SortBy: "id"},
		"limit too big":   {Limit: MaxFeedbackPageLimit + 1, SortBy: "id"},
		"offset negative": {Limit: 1, Offset: -1, SortBy: "id"},
		"offset too big":  {Limit: 1, Offset: MaxFeedbackPageOffset + 1, SortBy: "id"},
		"sort from text":  {Limit: 1, SortBy: "id; DROP TABLE x"},
	} {
		if _, err := repository.ListCurrentFeedback(context.Background(), 41, 7, false, bad); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if _, err := repository.ListCurrentFeedback(context.Background(), 0, 7, false, page); err == nil {
		t.Fatal("invalid caller accepted")
	}
	if _, err := repository.ListCurrentFeedback(context.Background(), 41, 0, false, page); err == nil {
		t.Fatal("invalid project accepted")
	}
	if len(executor.rowCalls) != 0 {
		t.Fatalf("invalid list issued %d SQL calls", len(executor.rowCalls))
	}
}

func canceledCurrentFeedbackContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
