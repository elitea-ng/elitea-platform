package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/projectaccess"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrSocialFeedbackForbidden is returned when the caller does not pass the
// project-membership check that the statement itself repeats.
var ErrSocialFeedbackForbidden = errors.New("social feedback: caller is not a member of the project")

// Bounds of one listing page. The HTTP layer enforces the same numbers; the
// repository refuses anything outside them so a second caller cannot widen them.
const (
	MaxFeedbackPageLimit  = 200
	MaxFeedbackPageOffset = 100000
)

// The membership decision is repeated inside the statement ($6 project, $1 user),
// so a membership revoked after the HTTP gate cannot be used to write. An
// administrator is a member of every id, so the project must also exist.
var insertCurrentSocialFeedbackSQL = `
	INSERT INTO centry.social_feedbacks (
		user_id,
		referrer,
		description,
		rating,
		user_agent,
		project_id
	)
	SELECT $1::int, $2::varchar, $3::text, $4::int, $5::varchar, $6::int
	WHERE ` + projectaccess.Membership(6, 1) + `
		AND ` + projectaccess.ProjectExists(6) + `
	RETURNING id`

// FeedbackPage selects one page of a listing. SortBy is "id" or "created_at".
type FeedbackPage struct {
	Limit  int
	Offset int
	SortBy string
	Desc   bool
}

// FeedbackRow is one listed feedback. The json tags are both the keys the
// listing statement aggregates and the keys of the HTTP response.
type FeedbackRow struct {
	ID          int64   `json:"id"`
	UserID      int64   `json:"user_id"`
	ProjectID   *int64  `json:"project_id"`
	Referrer    *string `json:"referrer"`
	Description string  `json:"description"`
	Rating      int     `json:"rating"`
	UserAgent   *string `json:"user_agent"`
	CreatedAt   string  `json:"created_at"`
}

// FeedbackList is a page of visible rows and the count of ALL visible rows.
type FeedbackList struct {
	Total int64         `json:"total"`
	Rows  []FeedbackRow `json:"rows"`
}

// listCurrentSocialFeedbackSQL is one statement over one snapshot. $1 project,
// $2 caller, $3 own-rows-only, $4 limit, $5 offset. %s is a constant ORDER BY
// clause chosen by feedbackOrderBy, never user text.
//
// A row is visible when the caller is a member AND (it belongs to this project
// and is not hidden by own-only, OR it is a legacy row without a project that
// the caller wrote).
const listCurrentSocialFeedbackSQL = `
	WITH access AS (
		SELECT %s AS allowed
	), visible AS (
		SELECT f.id, f.user_id, f.project_id, f.referrer, f.description,
			f.rating, f.user_agent, f.created_at
		FROM centry.social_feedbacks f, access
		WHERE access.allowed AND (
			(f.project_id = $1 AND (NOT $3::boolean OR f.user_id = $2))
			OR (f.project_id IS NULL AND f.user_id = $2)
		)
	)
	SELECT
		(SELECT allowed FROM access),
		(SELECT count(*) FROM visible),
		COALESCE((
			SELECT json_agg(p ORDER BY %[2]s)
			FROM (SELECT * FROM visible ORDER BY %[2]s LIMIT $4 OFFSET $5) p
		), '[]'::json)::text`

var listCurrentSocialFeedbackQueries = func() map[string]string {
	queries := make(map[string]string, 4)
	for key, order := range map[string]string{
		"id":              "id ASC",
		"id desc":         "id DESC",
		"created_at":      "created_at ASC, id ASC",
		"created_at desc": "created_at DESC, id DESC",
	} {
		queries[key] = fmt.Sprintf(listCurrentSocialFeedbackSQL, projectaccess.Membership(1, 2), order)
	}
	return queries
}()

func feedbackListQuery(page FeedbackPage) (string, bool) {
	key := page.SortBy
	if page.Desc {
		key += " desc"
	}
	query, ok := listCurrentSocialFeedbackQueries[key]
	return query, ok
}

// CurrentSocialFeedbacksRepository preserves the current shared-table storage
// contract: rows live in centry.social_feedbacks, never in p_N. Each statement
// repeats the project-membership decision, so it stands on its own.
type CurrentSocialFeedbacksRepository struct {
	store sqlExecutor
}

func NewCurrentSocialFeedbacksRepository(
	pool *pgxpool.Pool,
) (*CurrentSocialFeedbacksRepository, error) {
	store, err := newPostgresSharedStore(pool)
	if err != nil {
		return nil, err
	}
	return newCurrentSocialFeedbacksRepository(store)
}

func newCurrentSocialFeedbacksRepository(
	store sqlExecutor,
) (*CurrentSocialFeedbacksRepository, error) {
	if store == nil {
		return nil, errors.New("current social feedback database is required")
	}
	return &CurrentSocialFeedbacksRepository{store: store}, nil
}

func (repository *CurrentSocialFeedbacksRepository) CreateCurrentFeedback(
	ctx context.Context,
	userID int64,
	projectID int64,
	description string,
	rating int,
	referrer *string,
	userAgent string,
) (int64, error) {
	if ctx == nil || userID <= 0 || projectID <= 0 || rating < 0 || rating > 5 {
		return 0, errors.New("invalid current social feedback")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	var id int64
	if err := repository.store.QueryRow(
		ctx,
		insertCurrentSocialFeedbackSQL,
		userID,
		referrer,
		description,
		rating,
		userAgent,
		projectID,
	).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrSocialFeedbackForbidden
		}
		return 0, fmt.Errorf("insert current social feedback: %w", err)
	}
	if id <= 0 {
		return 0, errors.New("insert current social feedback returned an invalid ID")
	}
	return id, nil
}

// ListCurrentFeedback returns one page of the feedback visible to callerID in
// projectID, with the total of visible rows. ownOnly restricts the project's
// rows to the caller's own (the public project). A caller who is not a member
// gets ErrSocialFeedbackForbidden.
func (repository *CurrentSocialFeedbacksRepository) ListCurrentFeedback(
	ctx context.Context,
	callerID int64,
	projectID int64,
	ownOnly bool,
	page FeedbackPage,
) (FeedbackList, error) {
	query, sortOK := feedbackListQuery(page)
	if ctx == nil || callerID <= 0 || projectID <= 0 || !sortOK ||
		page.Limit < 1 || page.Limit > MaxFeedbackPageLimit ||
		page.Offset < 0 || page.Offset > MaxFeedbackPageOffset {
		return FeedbackList{}, errors.New("invalid current social feedback list")
	}
	if err := ctx.Err(); err != nil {
		return FeedbackList{}, err
	}

	var allowed bool
	var total int64
	var rowsJSON []byte
	if err := repository.store.QueryRow(
		ctx, query, projectID, callerID, ownOnly, page.Limit, page.Offset,
	).Scan(&allowed, &total, &rowsJSON); err != nil {
		return FeedbackList{}, fmt.Errorf("list current social feedback: %w", err)
	}
	if !allowed {
		return FeedbackList{}, ErrSocialFeedbackForbidden
	}
	list := FeedbackList{Total: total, Rows: []FeedbackRow{}}
	if err := json.Unmarshal(rowsJSON, &list.Rows); err != nil {
		return FeedbackList{}, fmt.Errorf("decode current social feedback page: %w", err)
	}
	return list, nil
}
