package personalproject

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// rowQuerier is the one pool method the ownership check needs.
type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// OwnershipCheck reports whether a project is the personal project of a user.
type OwnershipCheck struct {
	db rowQuerier
}

// NewOwnershipCheck builds the check over a pool. A nil pool gives a check
// that answers false for every project. The parameter is the concrete pool
// type on purpose: a nil *pgxpool.Pool stored in the interface field would be
// a non-nil interface, and the first query would panic.
func NewOwnershipCheck(pool *pgxpool.Pool) *OwnershipCheck {
	if pool == nil {
		return &OwnershipCheck{}
	}
	return &OwnershipCheck{db: pool}
}

// IsOwnPersonalProject answers true only when the project row carries the
// personal name of userID (`project_user_<userID>`) AND names that user as its
// owner. The name alone is not enough: a team project can be renamed to look
// personal. The owner alone is not enough: the maker of a team project is its
// owner too.
//
// An unknown project answers false with no error. A malformed id answers
// false with no error, because the route gate has already refused it.
func (c *OwnershipCheck) IsOwnPersonalProject(ctx context.Context, projectID string, userID int64) (bool, error) {
	if c == nil || c.db == nil || userID <= 0 {
		return false, nil
	}
	id, err := strconv.ParseInt(projectID, 10, 64)
	if err != nil || id <= 0 {
		return false, nil
	}
	var personal bool
	err = c.db.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM centry.project
    WHERE id = $1
      AND owner_id = $2
      AND name = $3
)`, id, userID, Name(userID)).Scan(&personal)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("check personal project ownership: %w", err)
	}
	return personal, nil
}
