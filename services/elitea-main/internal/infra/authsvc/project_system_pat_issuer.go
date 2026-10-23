package authsvc

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
)

type activeProjectSystemPATQueries interface {
	GetActiveProjectSystemPAT(context.Context, int32) (sqlcgen.GetActiveProjectSystemPATRow, error)
}

// ProjectSystemToken is the current project-system identity materialized from
// an already provisioned active "api" PAT. Its default string representations
// are redacted so logging the value does not expose the bearer token.
type ProjectSystemToken struct {
	projectID int64
	userID    int64
	token     string
}

func (t ProjectSystemToken) ProjectID() int64 { return t.projectID }
func (t ProjectSystemToken) UserID() int64    { return t.userID }
func (t ProjectSystemToken) Token() string    { return t.token }
func (t ProjectSystemToken) String() string   { return "authsvc.ProjectSystemToken{redacted}" }
func (t ProjectSystemToken) GoString() string { return t.String() }

// ProjectSystemIssuer preserves the current project-system-user naming and
// active "api" PAT selection contract. It never creates a user or PAT, and it
// is intentionally not a fallback from a failed actor-PAT resolution.
type ProjectSystemIssuer struct {
	queries   activeProjectSystemPATQueries
	secretKey []byte
}

func NewProjectSystemIssuerBytes(pool *pgxpool.Pool, secretKey []byte) *ProjectSystemIssuer {
	issuer := &ProjectSystemIssuer{secretKey: append([]byte(nil), secretKey...)}
	if pool != nil {
		issuer.queries = sqlcgen.New(pool)
	}
	return issuer
}

func (i *ProjectSystemIssuer) IssueProjectToken(
	ctx context.Context,
	projectID int64,
) (ProjectSystemToken, error) {
	if i == nil || len(i.secretKey) == 0 || i.queries == nil {
		return ProjectSystemToken{}, fmt.Errorf(
			"%w: project-system PAT issuer is not configured",
			ErrTokenValidationUnavailable,
		)
	}
	if projectID <= 0 || projectID > math.MaxInt32 {
		return ProjectSystemToken{}, fmt.Errorf(
			"%w: project identity is invalid",
			ErrTokenValidationUnavailable,
		)
	}
	row, err := i.queries.GetActiveProjectSystemPAT(ctx, int32(projectID))
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return ProjectSystemToken{}, contextErr
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return ProjectSystemToken{}, fmt.Errorf("%w: active project-system PAT not found", ErrTokenRejected)
		}
		return ProjectSystemToken{}, fmt.Errorf(
			"%w: active project-system PAT lookup failed: %w",
			ErrTokenValidationUnavailable,
			err,
		)
	}
	expectedEmail := fmt.Sprintf("system_user_%d@centry.user", projectID)
	if int64(row.ProjectID) != projectID ||
		row.UserID <= 0 ||
		row.TokenID <= 0 ||
		row.Email != expectedEmail ||
		row.Uuid == nil ||
		*row.Uuid == "" {
		return ProjectSystemToken{}, fmt.Errorf(
			"%w: active project-system PAT contains invalid identity data",
			ErrTokenValidationUnavailable,
		)
	}
	var expires *time.Time
	if row.Expires.Valid {
		value := row.Expires.Time
		expires = &value
	}
	encoded, err := SignBaselinePAT(i.secretKey, row.Uuid, expires)
	if err != nil {
		return ProjectSystemToken{}, fmt.Errorf(
			"%w: sign project-system PAT: %w",
			ErrTokenValidationUnavailable,
			err,
		)
	}
	return ProjectSystemToken{
		projectID: projectID,
		userID:    int64(row.UserID),
		token:     encoded,
	}, nil
}
