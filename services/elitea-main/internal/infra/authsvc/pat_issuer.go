package authsvc

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
)

type activePATIssuerQueries interface {
	GetActivePATForUser(context.Context, int32) (sqlcgen.GetActivePATForUserRow, error)
}

// LocalIssuer recreates the current-baseline bearer representation for an
// already persisted active PAT. It never creates, rotates, or stores a PAT.
type LocalIssuer struct {
	queries   activePATIssuerQueries
	secretKey []byte
}

func NewLocalIssuerBytes(pool *pgxpool.Pool, secretKey []byte) *LocalIssuer {
	issuer := &LocalIssuer{secretKey: append([]byte(nil), secretKey...)}
	if pool != nil {
		issuer.queries = sqlcgen.New(pool)
	}
	return issuer
}

func (i *LocalIssuer) IssueToken(ctx context.Context, userID int64) (string, error) {
	if i == nil || len(i.secretKey) == 0 || i.queries == nil {
		return "", fmt.Errorf("%w: PAT issuer is not configured", ErrTokenValidationUnavailable)
	}
	if userID <= 0 || userID > math.MaxInt32 {
		return "", fmt.Errorf("%w: PAT owner is invalid", ErrTokenValidationUnavailable)
	}
	row, err := i.queries.GetActivePATForUser(ctx, int32(userID))
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return "", contextErr
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("%w: active PAT not found", ErrTokenRejected)
		}
		return "", fmt.Errorf("%w: active PAT lookup failed: %w", ErrTokenValidationUnavailable, err)
	}
	if row.TokenID <= 0 || int64(row.UserID) != userID || row.Uuid == nil || *row.Uuid == "" {
		return "", fmt.Errorf("%w: active PAT contains invalid identity data", ErrTokenValidationUnavailable)
	}
	var expires *time.Time
	if row.Expires.Valid {
		value := row.Expires.Time
		expires = &value
	}
	return SignBaselinePAT(i.secretKey, row.Uuid, expires)
}

// SignBaselinePAT preserves the current Python HS512 payload contract.
func SignBaselinePAT(secret []byte, tokenUUID *string, expiresAt *time.Time) (string, error) {
	if len(secret) == 0 {
		return "", errors.New("PAT signing key is required")
	}
	var expires *string
	if expiresAt != nil {
		value := expiresAt.Format("2006-01-02T15:04")
		expires = &value
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS512, issuedTokenClaims{
		UUID:    tokenUUID,
		Expires: expires,
	})
	return token.SignedString(secret)
}

type issuedTokenClaims struct {
	UUID    *string `json:"uuid"`
	Expires *string `json:"expires"`
	jwt.RegisteredClaims
}
