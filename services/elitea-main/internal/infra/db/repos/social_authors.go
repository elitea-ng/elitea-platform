package repos

import (
	"context"
	"errors"
	"fmt"
	"time"

	socialapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/social"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrInvalidCurrentSocialAuthorsRequest = errors.New("current social authors request is invalid")

const CurrentSocialAuthorsQueryTimeout = 5 * time.Second

type currentSocialAuthorsQueries interface {
	ListCurrentProjectAuthors(
		context.Context,
		int32,
	) ([]sqlcgen.ListCurrentProjectAuthorsRow, error)
}

// CurrentSocialAuthorsRepository reads the current shared auth and social
// tables in one query. Project tenancy is expressed by the trusted numeric
// project_id predicate, not by a caller-provided schema identifier.
type CurrentSocialAuthorsRepository struct {
	queries currentSocialAuthorsQueries
}

func NewCurrentSocialAuthorsRepository(
	pool *pgxpool.Pool,
) (*CurrentSocialAuthorsRepository, error) {
	if pool == nil {
		return nil, errors.New("current social authors database is required")
	}
	return newCurrentSocialAuthorsRepository(sqlcgen.New(pool))
}

func newCurrentSocialAuthorsRepository(
	queries currentSocialAuthorsQueries,
) (*CurrentSocialAuthorsRepository, error) {
	if queries == nil {
		return nil, errors.New("current social authors queries are required")
	}
	return &CurrentSocialAuthorsRepository{queries: queries}, nil
}

func (repository *CurrentSocialAuthorsRepository) ListCurrentProjectAuthors(
	ctx context.Context,
	projectID int32,
) ([]socialapp.CurrentAuthor, error) {
	if ctx == nil || projectID <= 0 {
		return nil, ErrInvalidCurrentSocialAuthorsRequest
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	queryContext, cancel := context.WithTimeout(ctx, CurrentSocialAuthorsQueryTimeout)
	defer cancel()
	rows, err := repository.queries.ListCurrentProjectAuthors(queryContext, projectID)
	if err != nil {
		return nil, fmt.Errorf("query current social authors: %w", err)
	}
	authors := make([]socialapp.CurrentAuthor, 0, len(rows))
	for _, row := range rows {
		authors = append(authors, socialapp.CurrentAuthor{
			ID:        row.ID,
			Email:     row.Email,
			Name:      row.Name,
			LastLogin: currentSocialAuthorTimestamp(row.LastLogin),
			Suspended: row.Suspended,
			Avatar:    row.Avatar,
		})
	}
	return authors, nil
}

func currentSocialAuthorTimestamp(value pgtype.Timestamp) *time.Time {
	if !value.Valid {
		return nil
	}
	// The current auth schema stores a timezone-free UTC timestamp. Rebuilding
	// the value in UTC avoids host-local timezone drift at the HTTP boundary.
	timestamp := time.Date(
		value.Time.Year(),
		value.Time.Month(),
		value.Time.Day(),
		value.Time.Hour(),
		value.Time.Minute(),
		value.Time.Second(),
		value.Time.Nanosecond(),
		time.UTC,
	)
	return &timestamp
}
