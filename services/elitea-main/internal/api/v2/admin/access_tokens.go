package admin

// The central listing of tokens with administration access (ADR-0059
// decision 7, "`inherit`": "is listed on the Access page"; story R1.3a,
// issue 2507). The Access page of R3.9 reads it.
//
// GET /admin/access/tokens lists every personal access token whose
// admin_access is not `none`, with its owner, for a holder of
// `access.roles.view` (or the names it narrows). It is a list of who holds an
// administration credential, so it sits beside the role holders, behind the
// same gate (decision 6: names of administrators only here).
//
// IT CARRIES NO SECRET. No token value, no masked hint, no digest and no
// uuid: the uuid is what a token's signed value names, and nothing on the
// Access page needs it. A row is named by its id.

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// accessToken is one token with administration access.
type accessToken struct {
	ID          int64      `json:"id"`
	Name        *string    `json:"name"`
	Expires     *time.Time `json:"expires"`
	AdminAccess string     `json:"admin_access"`
	AdminScopes []string   `json:"admin_scopes"`
	// InheritEndsOn is the sunset date of an `inherit` token, absent while
	// it is not set and for a `scoped` token.
	InheritEndsOn  string `json:"inherit_ends_on,omitempty"`
	OwnerID        int    `json:"owner_id"`
	OwnerName      string `json:"owner_name"`
	OwnerEmail     string `json:"owner_email"`
	OwnerSuspended bool   `json:"owner_suspended"`
}

// AccessTokens serves `GET /admin/access/tokens`.
//
// Query: `limit` (1..200, default 50) and `offset` page, like the role
// history: the upgrade backfilled `inherit` onto every token of an
// administrator, so the list is not small on a large deployment. `total`
// counts every row.
func (h *Handler) AccessTokens(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	limit, offset, ok := accessPage(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	var total int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM public.auth_core__token AS token
JOIN public.auth_core__user AS account ON account.id = token.user_id
WHERE `+accessTokenFilter).Scan(&total); err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to list tokens with administration access")
		return
	}
	tokens, err := h.accessTokens(ctx, limit, offset)
	if err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to list tokens with administration access")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "rows": tokens})
}

// accessPage reads `limit` and `offset` with the role history's bounds, or
// writes the 400 and answers false.
func accessPage(w http.ResponseWriter, r *http.Request) (limit, offset int, ok bool) {
	query := r.URL.Query()
	limit = 50
	if raw := query.Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > accessHistoryMaxLimit {
			apierr.WriteStatus(w, http.StatusBadRequest, fmt.Sprintf("limit must be between 1 and %d", accessHistoryMaxLimit))
			return 0, 0, false
		}
		limit = value
	}
	if raw := query.Get("offset"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			apierr.WriteStatus(w, http.StatusBadRequest, "offset must be a non-negative integer")
			return 0, 0, false
		}
		offset = value
	}
	return limit, offset, true
}

// accessTokenFilter is the rows listed: a row with uuid NULL is a native
// device session anchor, not a personal access token (shared 0265), and is
// never listed whatever its column says.
const accessTokenFilter = `token.uuid IS NOT NULL AND token.admin_access <> 'none'`

// accessTokens reads one page of the tokens.
func (h *Handler) accessTokens(ctx context.Context, limit, offset int) ([]accessToken, error) {
	rows, err := h.pool.Query(ctx, `
SELECT token.id, token.name, token.expires, token.admin_access, token.admin_scopes,
       account.id, COALESCE(account.name, account.email, ''), COALESCE(account.email, ''), account.suspended
FROM public.auth_core__token AS token
JOIN public.auth_core__user AS account ON account.id = token.user_id
WHERE `+accessTokenFilter+`
ORDER BY token.admin_access, lower(COALESCE(account.name, account.email, '')), account.id, token.id
LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list tokens with administration access: %w", err)
	}
	tokens, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (accessToken, error) {
		var token accessToken
		var expires *time.Time
		err := row.Scan(&token.ID, &token.Name, &expires, &token.AdminAccess, &token.AdminScopes,
			&token.OwnerID, &token.OwnerName, &token.OwnerEmail, &token.OwnerSuspended)
		if expires != nil {
			// auth_core__token.expires is TIMESTAMP without time zone, written
			// in UTC by the token routes.
			value := expires.UTC()
			token.Expires = &value
		}
		if token.AdminScopes == nil {
			token.AdminScopes = []string{}
		}
		if token.AdminAccess == adminaccess.TokenAccessInherit {
			token.InheritEndsOn = adminaccess.InheritEndsOn()
		}
		return token, err
	})
	if err != nil {
		return nil, fmt.Errorf("list tokens with administration access: %w", err)
	}
	if tokens == nil {
		tokens = []accessToken{}
	}
	return tokens, nil
}
