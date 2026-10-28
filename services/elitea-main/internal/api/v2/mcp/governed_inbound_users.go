package mcp

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// PostgresInboundUsers resolves an inbound MCP token's user claim (email by
// default) to the one active Elitea user with that address. No user, more
// than one, or a suspended one is auth.ErrPrincipalInactive: an inbound
// token never creates a user.
type PostgresInboundUsers struct{ pool *pgxpool.Pool }

// NewPostgresInboundUsers builds the resolver.
func NewPostgresInboundUsers(pool *pgxpool.Pool) (*PostgresInboundUsers, error) {
	if pool == nil {
		return nil, errors.New("mcp: the inbound user resolver needs the platform database")
	}
	return &PostgresInboundUsers{pool: pool}, nil
}

// ResolveInboundUser implements InboundUserResolver.
func (p *PostgresInboundUsers) ResolveInboundUser(r *http.Request, token InboundToken) (auth.User, error) {
	email := strings.ToLower(strings.TrimSpace(token.UserClaim))
	if email == "" || len(email) > 320 {
		return auth.User{}, auth.ErrPrincipalInactive
	}
	rows, err := p.pool.Query(r.Context(), `
SELECT id, COALESCE(email, ''), COALESCE(name, '')
  FROM auth_core__user
 WHERE lower(email) = $1 AND NOT COALESCE(suspended, false)
 ORDER BY id LIMIT 2`, email)
	if err != nil {
		return auth.User{}, fmt.Errorf("mcp: resolve the inbound user: %w", err)
	}
	defer rows.Close()
	var users []auth.User
	for rows.Next() {
		var id int64
		var u auth.User
		if err := rows.Scan(&id, &u.Email, &u.Name); err != nil {
			return auth.User{}, fmt.Errorf("mcp: resolve the inbound user: %w", err)
		}
		u.ID, u.UserID = strconv.FormatInt(id, 10), strconv.FormatInt(id, 10)
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return auth.User{}, fmt.Errorf("mcp: resolve the inbound user: %w", err)
	}
	if len(users) != 1 {
		return auth.User{}, auth.ErrPrincipalInactive
	}
	return users[0], nil
}
