package admin_test

// The central listing of tokens with administration access (ADR-0059 R1.3a,
// issue 2507): GET /admin/access/tokens, behind access.roles.view like the
// other Access reads, with no secret in the answer.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

type listedAccessToken struct {
	ID            int64    `json:"id"`
	Name          *string  `json:"name"`
	AdminAccess   string   `json:"admin_access"`
	AdminScopes   []string `json:"admin_scopes"`
	InheritEndsOn *string  `json:"inherit_ends_on"`
	OwnerID       int      `json:"owner_id"`
	OwnerEmail    string   `json:"owner_email"`
}

// Acceptance: a holder of only access.roles.view reads the listing; a caller
// without it gets 403; the answer carries no secret, and lists only the
// tokens whose access is not none.
func TestTheTokenListingIsForTheAccessReaderAndCarriesNoSecret(t *testing.T) {
	// The auth_core baseline projection carries admin_access and admin_scopes
	// (shared 0265).
	env := newAccessEnvironment(t)
	owner := env.ids[escalationSuper]
	for _, statement := range []string{
		`INSERT INTO public.auth_core__token (uuid, user_id, name, admin_access, admin_scopes) VALUES
			('11111111-1111-4111-8111-111111111111', $1, 'ci-scoped', 'scoped', ARRAY['admin.auth.users']),
			('22222222-2222-4222-8222-222222222222', $1, 'ci-inherit', 'inherit', '{}'),
			('33333333-3333-4333-8333-333333333333', $1, 'ci-none', 'none', '{}')`,
		// A native device session anchor (uuid NULL) is never listed.
		`INSERT INTO public.auth_core__token (uuid, user_id, name, admin_access) VALUES (NULL, $1, 'device', 'inherit')`,
	} {
		if _, err := env.pool.Exec(t.Context(), statement, owner); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}

	recorder := adminDo(t, env.gatedAccessRouter(accessReader), http.MethodGet, accessTokens, nil)
	expectStatus(t, recorder, http.StatusOK)
	raw := recorder.Body.String()
	for _, forbidden := range []string{`"token"`, `"uuid"`, "digest", "11111111-1111", "22222222-2222"} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("the listing carries %s: %s", forbidden, raw)
		}
	}
	total, rows := decodeRows[listedAccessToken](t, recorder.Body.Bytes())
	if total != 2 || len(rows) != 2 {
		t.Fatalf("listing = %d %s, want the scoped and the inherit token", total, raw)
	}
	byName := map[string]listedAccessToken{}
	for _, row := range rows {
		if row.Name == nil {
			t.Fatalf("row without a name: %+v", row)
		}
		byName[*row.Name] = row
	}
	scoped, inherit := byName["ci-scoped"], byName["ci-inherit"]
	if scoped.AdminAccess != "scoped" || strings.Join(scoped.AdminScopes, ",") != adminaccess.AdminAuthUsers ||
		scoped.OwnerID != owner || scoped.OwnerEmail != escalationSuper || scoped.InheritEndsOn != nil {
		t.Fatalf("scoped row = %+v", scoped)
	}
	// The sunset date is not set yet, so an inherit token states no end.
	if inherit.AdminAccess != "inherit" || len(inherit.AdminScopes) != 0 || inherit.InheritEndsOn != nil {
		t.Fatalf("inherit row = %+v", inherit)
	}
	var shape []map[string]json.RawMessage
	var page struct {
		Rows *[]map[string]json.RawMessage `json:"rows"`
	}
	page.Rows = &shape
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	for _, row := range shape {
		for key := range row {
			switch key {
			case "id", "name", "expires", "admin_access", "admin_scopes", "inherit_ends_on",
				"owner_id", "owner_name", "owner_email", "owner_suspended":
			default:
				t.Fatalf("the listing has the unexpected field %q", key)
			}
		}
	}

	// Paged like the role history: total counts every row.
	paged := adminDo(t, env.gatedAccessRouter(accessReader), http.MethodGet, accessTokens+"?limit=1&offset=1", nil)
	expectStatus(t, paged, http.StatusOK)
	if total, rows := decodeRows[listedAccessToken](t, paged.Body.Bytes()); total != 2 || len(rows) != 1 ||
		rows[0].ID != inherit.ID && rows[0].ID != scoped.ID {
		t.Fatalf("page = %d %+v, want total 2 and one row", total, rows)
	}
	for _, bad := range []string{"?limit=0", "?limit=201", "?offset=-1", "?limit=x"} {
		expectStatus(t, adminDo(t, env.gatedAccessRouter(accessReader), http.MethodGet, accessTokens+bad, nil),
			http.StatusBadRequest)
	}

	expectStatus(t, adminDo(t, env.gatedAccessRouter(accessNobody), http.MethodGet, accessTokens, nil),
		http.StatusForbidden)
}
