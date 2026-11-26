package eliteacore_test

// A project membership given under an elevation is provisional (ADR-0059
// decision 5; story R2.12b, issue 2606): the central member routes write a
// row of elitea_auth.admin_provisional_grants, never
// auth_core__project_user_role, say so in the response, and the member
// listing shows the person marked provisional.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/provisionalgrants"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

func decodeUsersWrite(t *testing.T, payload []byte, target any) {
	t.Helper()
	if err := json.Unmarshal(payload, target); err != nil {
		t.Fatalf("decode %s: %v", payload, err)
	}
}

func provisionalMemberships(t *testing.T, pool *pgxpool.Pool, userID int) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM elitea_auth.admin_provisional_grants
WHERE kind = 'membership' AND user_id = $1 AND project_id = $2`, userID, usersWriteProjectID).Scan(&n); err != nil {
		t.Fatalf("read the provisional grants: %v", err)
	}
	return n
}

func TestAnElevatedMemberWriteIsProvisional(t *testing.T) {
	provisionalgrants.EnableForTest(t)
	pool, ids := elevatedMemberEnvironment(t)
	file, err := migrations.Files.ReadFile("shared/0284_admin_provisional_grants.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), string(file)); err != nil {
		t.Fatalf("apply shared 0284: %v", err)
	}
	router, _ := sessionMemberRouter(pool, ids[elevatedAdminEmail], elevatedMemberSession)

	// The invite of a newcomer.
	const newcomer = "e2e-provisional-newcomer@autotest.local"
	recorder := usersWriteDo(t, router, http.MethodPost, centralMembersPath,
		map[string]any{"emails": []string{newcomer}, "roles": []string{"editor"}})
	if recorder.Code != http.StatusOK {
		t.Fatalf("invite: status = %d body %s, want 200", recorder.Code, recorder.Body.String())
	}
	var invite []struct {
		Provisional bool `json:"provisional"`
	}
	decodeUsersWrite(t, recorder.Body.Bytes(), &invite)
	if len(invite) != 1 || !invite[0].Provisional {
		t.Fatalf("invite response = %s, want the membership marked provisional", recorder.Body.String())
	}
	id := accountID(t, pool, newcomer)
	if roles := projectRolesOf(t, pool, id); len(roles) != 0 {
		t.Fatalf("the newcomer holds %v standing, want none", roles)
	}
	if n := provisionalMemberships(t, pool, id); n != 1 {
		t.Fatalf("%d provisional membership rows, want 1", n)
	}

	// The update of a standing member's roles: the role it gains is
	// provisional, the role it keeps stays standing.
	super, _ := sessionMemberRouter(pool, ids[standingSuperEmail], standingMemberSession)
	const member = "e2e-provisional-member@autotest.local"
	if recorder := usersWriteDo(t, super, http.MethodPost, centralMembersPath,
		map[string]any{"emails": []string{member}, "roles": []string{"viewer"}}); recorder.Code != http.StatusOK {
		t.Fatalf("seed the member: %d %s", recorder.Code, recorder.Body.String())
	}
	memberID := accountID(t, pool, member)
	recorder = usersWriteDo(t, router, http.MethodPut, centralMembersPath,
		map[string]any{"userId": fmt.Sprint(memberID), "roles": []string{"viewer", "admin"}})
	if recorder.Code != http.StatusOK {
		t.Fatalf("update: status = %d body %s, want 200", recorder.Code, recorder.Body.String())
	}
	var update struct {
		Provisional bool `json:"provisional"`
	}
	decodeUsersWrite(t, recorder.Body.Bytes(), &update)
	if !update.Provisional {
		t.Fatalf("update response = %s, want provisional", recorder.Body.String())
	}
	if roles := projectRolesOf(t, pool, memberID); len(roles) != 1 || roles[0] != "viewer" {
		t.Fatalf("the member holds %v standing, want only viewer", roles)
	}
	if n := provisionalMemberships(t, pool, memberID); n != 1 {
		t.Fatalf("%d provisional membership rows of the member, want 1", n)
	}

	// The listing shows the provisional newcomer apart from the members.
	recorder = usersWriteDo(t, super, http.MethodGet,
		fmt.Sprintf("/admin/users/administration/%d", usersWriteProjectID), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("list: status = %d body %s", recorder.Code, recorder.Body.String())
	}
	var listing struct {
		Rows []struct {
			ID any `json:"id"`
		} `json:"rows"`
		Provisional []struct {
			ID          string   `json:"id"`
			Email       string   `json:"email"`
			Roles       []string `json:"roles"`
			Provisional bool     `json:"provisional"`
		} `json:"provisional"`
	}
	decodeUsersWrite(t, recorder.Body.Bytes(), &listing)
	found := map[string]bool{}
	for _, row := range listing.Provisional {
		found[row.Email] = row.Provisional
	}
	if !found[newcomer] || !found[member] {
		t.Fatalf("provisional listing = %+v, want the newcomer and the member", listing.Provisional)
	}
}
