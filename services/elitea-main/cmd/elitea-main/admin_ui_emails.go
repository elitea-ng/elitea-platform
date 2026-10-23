package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// adminUIEmails resolves the admin console operator's address for the nav
// footer.
//
// The admin SPA is served to a browser whose Form session carries a user ID
// and never an address. The footer read
// the injected `user_name`, found it empty, and fell back to the generic word
// "Admin" on every load. This read fills it.
//
// It is display material only. Nothing downstream authorises on it, and it runs
// after the permission resolver has already confirmed the user exists and is
// not suspended.
type adminUIEmails struct {
	pool *pgxpool.Pool
}

// UserEmail returns the address, or "" with no error when the user has none.
//
// A nil pool reports an error rather than an empty address so the caller logs
// a composition problem instead of silently showing the fallback name. The
// value is a nil *pgxpool.Pool inside a struct field, not inside an interface,
// so this test is a real one.
func (e adminUIEmails) UserEmail(ctx context.Context, userID int64) (string, error) {
	if e.pool == nil {
		return "", errors.New("admin ui email lookup has no database pool")
	}
	var email *string
	if err := e.pool.QueryRow(ctx,
		`SELECT email FROM public.auth_core__user WHERE id = $1 AND suspended = false`,
		userID,
	).Scan(&email); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("read admin ui user email: %w", err)
	}
	if email == nil {
		return "", nil
	}
	return *email, nil
}
