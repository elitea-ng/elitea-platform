package eliteacore_test

// #130 acceptance.
//
// The defect these tests exist for is NOT "the endpoint answers the wrong
// status" — the broken build answered 200 to POST, PUT and DELETE alike, which
// is exactly what a working one answers. Asserting on the status therefore
// proves nothing at all. Every case below performs the write and then RE-READS
// the membership through the product's own GET handler (and, where the read
// path could conceivably paper over a miss, through SQL as well). Against the
// pre-fix router — all four verbs mounted on Handler.Users — every one of these
// re-reads returns the untouched member list and the test fails.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	dbschema "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/schema"
	infradb "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db"
)

const usersWriteProjectID = 1

type usersListBody struct {
	Rows []struct {
		ID    string   `json:"id"`
		Email string   `json:"email"`
		Name  string   `json:"name"`
		Roles []string `json:"roles"`
	} `json:"rows"`
	Total int `json:"total"`
}

// usersWriteRouter mounts the four verbs exactly as internal/api/router.go
// does, minus the permission middleware (which needs the whole auth stack; the
// E2E journey covers it through a real logged-in session instead).
func usersWriteRouter(handler *eliteacore.Handler) chi.Router {
	router := chi.NewRouter()
	router.Get("/admin/users/{mode}/{projectID}", handler.Users)
	router.Post("/admin/users/{mode}/{projectID}", handler.UsersCreate)
	router.Put("/admin/users/{mode}/{projectID}", handler.UsersUpdate)
	router.Delete("/admin/users/{mode}/{projectID}", handler.UsersDelete)
	return router
}

func usersWriteDo(t *testing.T, router chi.Router, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, target, reader)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// readMembers re-reads the project membership through the SAME GET handler the
// Users page calls. This is the assertion that the pre-fix code cannot pass.
func readMembers(t *testing.T, router chi.Router) usersListBody {
	t.Helper()
	recorder := usersWriteDo(t, router, http.MethodGet,
		fmt.Sprintf("/admin/users/default/%d?limit=100&offset=0", usersWriteProjectID), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("re-read GET status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var listing usersListBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &listing); err != nil {
		t.Fatalf("decode re-read body %q: %v", recorder.Body.String(), err)
	}
	return listing
}

func memberRoles(listing usersListBody, email string) ([]string, bool) {
	for _, row := range listing.Rows {
		if row.Email == email {
			roles := append([]string(nil), row.Roles...)
			sort.Strings(roles)
			return roles, true
		}
	}
	return nil, false
}

func TestUsersWriteVerbsPersistProjectMembership(t *testing.T) {
	pool := newUsersWritePostgresPool(t)
	prepareUsersWriteFixture(t, pool)
	router := usersWriteRouter(eliteacore.NewHandler(pool))

	const invited = "e2e-invited@autotest.local"
	const existing = "e2e-member@autotest.local"

	t.Run("POST invites an unknown address and the member list shows it", func(t *testing.T) {
		before := readMembers(t, router)
		if _, found := memberRoles(before, invited); found {
			t.Fatalf("fixture already contains %s", invited)
		}

		recorder := usersWriteDo(t, router, http.MethodPost,
			fmt.Sprintf("/admin/users/default/%d", usersWriteProjectID),
			map[string]any{"emails": []string{invited}, "roles": []string{"editor"}})
		if recorder.Code != http.StatusOK {
			t.Fatalf("POST status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
		}

		// THE assertion: read it back the way the page does.
		after := readMembers(t, router)
		roles, found := memberRoles(after, invited)
		if !found {
			t.Fatalf("invited address %s absent from the re-read member list %+v", invited, after.Rows)
		}
		if len(roles) != 1 || roles[0] != "editor" {
			t.Fatalf("invited member roles = %v, want [editor]", roles)
		}
		if after.Total != before.Total+1 {
			t.Fatalf("total = %d after inviting one member, want %d", after.Total, before.Total+1)
		}

		// The invite must have created the auth_core__user row too, not just a
		// dangling role assignment.
		var userCount int
		if err := pool.QueryRow(context.Background(),
			`SELECT COUNT(*) FROM auth_core__user WHERE email = $1`, invited).Scan(&userCount); err != nil {
			t.Fatal(err)
		}
		if userCount != 1 {
			t.Fatalf("auth_core__user rows for %s = %d, want 1", invited, userCount)
		}
	})

	t.Run("POST reports an existing member as an error and adds no duplicate", func(t *testing.T) {
		before := readMembers(t, router)

		recorder := usersWriteDo(t, router, http.MethodPost,
			fmt.Sprintf("/admin/users/default/%d", usersWriteProjectID),
			map[string]any{"emails": []string{existing}, "roles": []string{"viewer"}})
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("POST for an existing member status = %d, want 400 (body %s)",
				recorder.Code, recorder.Body.String())
		}

		after := readMembers(t, router)
		if after.Total != before.Total {
			t.Fatalf("total changed from %d to %d on a rejected invite", before.Total, after.Total)
		}
		roles, _ := memberRoles(after, existing)
		if len(roles) != 1 || roles[0] != "editor" {
			t.Fatalf("existing member roles = %v after a rejected invite, want [editor]", roles)
		}
	})

	t.Run("POST rejects a role the project does not define and writes nothing", func(t *testing.T) {
		before := readMembers(t, router)

		recorder := usersWriteDo(t, router, http.MethodPost,
			fmt.Sprintf("/admin/users/default/%d", usersWriteProjectID),
			map[string]any{"emails": []string{"e2e-nobody@autotest.local"}, "roles": []string{"wizard"}})
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("POST with an unknown role status = %d, want 400", recorder.Code)
		}

		after := readMembers(t, router)
		if _, found := memberRoles(after, "e2e-nobody@autotest.local"); found {
			t.Fatal("a rejected invite still added the member")
		}
		if after.Total != before.Total {
			t.Fatalf("total changed from %d to %d on a rejected invite", before.Total, after.Total)
		}
	})

	t.Run("PUT replaces the role set and the member list shows the new role", func(t *testing.T) {
		before := readMembers(t, router)
		roles, found := memberRoles(before, existing)
		if !found || len(roles) != 1 || roles[0] != "editor" {
			t.Fatalf("precondition: %s roles = %v, want [editor]", existing, roles)
		}
		userID := ""
		for _, row := range before.Rows {
			if row.Email == existing {
				userID = row.ID
			}
		}

		// The exact body the client sends: entities/user/model/useEditUser.ts:30.
		recorder := usersWriteDo(t, router, http.MethodPut,
			fmt.Sprintf("/admin/users/default/%d", usersWriteProjectID),
			map[string]any{"userId": userID, "roles": []string{"admin"}})
		if recorder.Code != http.StatusOK {
			t.Fatalf("PUT status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
		}

		after := readMembers(t, router)
		roles, found = memberRoles(after, existing)
		if !found {
			t.Fatalf("%s disappeared from the member list after an edit", existing)
		}
		// REPLACEMENT, not merge: `editor` must be gone. A handler that only
		// inserted the new role would still show [admin editor] here.
		if len(roles) != 1 || roles[0] != "admin" {
			t.Fatalf("roles after edit = %v, want [admin]", roles)
		}
	})

	t.Run("PUT accepts the comma-joined batch id form", func(t *testing.T) {
		before := readMembers(t, router)
		ids := make([]string, 0, 2)
		for _, row := range before.Rows {
			if row.Email == existing || row.Email == invited {
				ids = append(ids, row.ID)
			}
		}
		if len(ids) != 2 {
			t.Fatalf("precondition: expected both members present, got %+v", before.Rows)
		}

		// useEditUser.ts:61 joins the selected ids with a comma into ONE string.
		recorder := usersWriteDo(t, router, http.MethodPut,
			fmt.Sprintf("/admin/users/default/%d", usersWriteProjectID),
			map[string]any{"userId": ids[0] + "," + ids[1], "roles": []string{"viewer"}})
		if recorder.Code != http.StatusOK {
			t.Fatalf("batch PUT status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
		}

		after := readMembers(t, router)
		for _, email := range []string{existing, invited} {
			roles, found := memberRoles(after, email)
			if !found {
				t.Fatalf("%s missing after a batch edit", email)
			}
			if len(roles) != 1 || roles[0] != "viewer" {
				t.Fatalf("%s roles after batch edit = %v, want [viewer]", email, roles)
			}
		}
	})

	t.Run("DELETE removes the member from the project but keeps the account", func(t *testing.T) {
		before := readMembers(t, router)
		userID := ""
		for _, row := range before.Rows {
			if row.Email == invited {
				userID = row.ID
			}
		}
		if userID == "" {
			t.Fatalf("precondition: %s not in the member list", invited)
		}

		// shared/api/generated/admin/admin.ts's getUserDeleteUrl stringifies the
		// id array into ONE `id[]` occurrence.
		recorder := usersWriteDo(t, router, http.MethodDelete,
			fmt.Sprintf("/admin/users/default/%d?id%%5B%%5D=%s", usersWriteProjectID, userID), nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("DELETE status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
		}

		after := readMembers(t, router)
		if _, found := memberRoles(after, invited); found {
			t.Fatalf("%s is still a member after DELETE: %+v", invited, after.Rows)
		}
		if after.Total != before.Total-1 {
			t.Fatalf("total = %d after removing one member, want %d", after.Total, before.Total-1)
		}

		// "Remove from project" is not "delete the account".
		var userCount int
		if err := pool.QueryRow(context.Background(),
			`SELECT COUNT(*) FROM auth_core__user WHERE email = $1`, invited).Scan(&userCount); err != nil {
			t.Fatal(err)
		}
		if userCount != 1 {
			t.Fatalf("auth_core__user rows for %s = %d after removal from the project, want 1", invited, userCount)
		}
	})

	// Administration mode used to answer 501 here, and this case used to assert
	// it. That was a hole, not a policy: `/admin/users/{mode}/{projectID}`
	// carries the project id in its own path, pylon's users.py maps BOTH modes
	// to the same body, and the admin Projects page's member dialog is exactly
	// this call — so it reached a Not Implemented (unit A14, #200). It writes
	// for real now; what differs between the modes is the route-level GATE
	// (router.go resolves `configuration.users.users.*` centrally for
	// administration mode), not the query.
	t.Run("administration mode writes the membership", func(t *testing.T) {
		const invited = "e2e-admin-mode@autotest.local"
		before := readMembers(t, router)
		recorder := usersWriteDo(t, router, http.MethodPost,
			fmt.Sprintf("/admin/users/administration/%d", usersWriteProjectID),
			map[string]any{"emails": []string{invited}, "roles": []string{"admin"}})
		if recorder.Code != http.StatusOK {
			t.Fatalf("administration-mode POST status = %d, want 200 (body %s)",
				recorder.Code, recorder.Body.String())
		}

		// RE-READ through the product's own GET.
		after := readMembers(t, router)
		roles, found := memberRoles(after, invited)
		if !found {
			t.Fatalf("%s is not a member after an administration-mode invite", invited)
		}
		if len(roles) != 1 || roles[0] != "admin" {
			t.Fatalf("%s holds %v after the invite, want [admin]", invited, roles)
		}
		if after.Total != before.Total+1 {
			t.Fatalf("total = %d after one administration-mode invite, want %d", after.Total, before.Total+1)
		}
	})

	// Neither mode may write project 0's membership — the original reason the
	// administration branch was closed off. The project id is still required to
	// be a concrete positive integer.
	t.Run("administration mode still requires a real project id", func(t *testing.T) {
		for _, badID := range []string{"0", "-1", "abc"} {
			recorder := usersWriteDo(t, router, http.MethodPost,
				"/admin/users/administration/"+badID,
				map[string]any{"emails": []string{"e2e-nowhere@autotest.local"}, "roles": []string{"admin"}})
			if recorder.Code != http.StatusBadRequest {
				t.Errorf("administration-mode POST to project %q status = %d, want 400", badID, recorder.Code)
			}
		}
	})
}

/* ── token project binding revocation (spec-llm-project-scope §7.3) ───────── */

// A binding MUST NOT outlive membership. The binding decides which project pays
// for a /llm call and whose provider credentials the call spends
// (elitea-llm-gateway/internal/account/account.go), so a binding that survives
// a removal is a former member spending a project's keys. The cases below
// bracket the sweep from both sides: it must remove the right rows, and it must
// leave every other row alone.
const otherProjectID = 4242

func TestUsersDeleteRevokesTokenProjectBindingsForThatProjectOnly(t *testing.T) {
	pool := newUsersWritePostgresPool(t)
	prepareUsersWriteFixture(t, pool)
	router := usersWriteRouter(eliteacore.NewHandler(pool))

	const leaving = "e2e-member@autotest.local"
	const staying = "e2e-admin@autotest.local"

	// The key that must be revoked, plus the two that must not: the same
	// person's key for a DIFFERENT project, and a DIFFERENT person's key for the
	// same project.
	revoked := seedBoundPAT(t, pool, "leaving-here", leaving, usersWriteProjectID)
	keptOtherProject := seedBoundPAT(t, pool, "leaving-elsewhere", leaving, otherProjectID)
	keptOtherUser := seedBoundPAT(t, pool, "staying-here", staying, usersWriteProjectID)

	before := readMembers(t, router)
	userID := memberUserID(t, before, leaving)

	recorder := usersWriteDo(t, router, http.MethodDelete,
		fmt.Sprintf("/admin/users/default/%d?id%%5B%%5D=%s", usersWriteProjectID, userID), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	// Membership really went — otherwise the binding assertions prove nothing.
	after := readMembers(t, router)
	if _, found := memberRoles(after, leaving); found {
		t.Fatalf("%s is still a member after DELETE: %+v", leaving, after.Rows)
	}

	if projectID, found := boundProjectID(t, pool, revoked); found {
		t.Errorf("token %d of the removed member still binds project %d — the key keeps "+
			"spending a project its owner has left (spec §7 invariant 3)", revoked, projectID)
	}

	// Precision, the half a broad `DELETE ... WHERE project_id = $1` would fail.
	if projectID, found := boundProjectID(t, pool, keptOtherProject); !found || projectID != otherProjectID {
		t.Errorf("the removed member's binding for project %d was swept too (found=%v project=%d); "+
			"leaving one project must not unbind the keys of another",
			otherProjectID, found, projectID)
	}
	// And the half a broad `DELETE ... WHERE token_id IN (...)` would fail.
	if projectID, found := boundProjectID(t, pool, keptOtherUser); !found || projectID != usersWriteProjectID {
		t.Errorf("a still-member's binding for project %d was swept (found=%v project=%d); "+
			"removing one user must not touch another user's keys",
			usersWriteProjectID, found, projectID)
	}
}

// The regression that would otherwise ship unnoticed. UsersUpdate DELETEs the
// same auth_core__project_user_role rows UsersDelete does, and re-inserts them
// in the same transaction. Reading that DELETE as a removal — and revoking
// bindings beside it — would unbind every key of every edited user on an
// ordinary role change, reported as "roles updated". Nothing in the response,
// the member list or the UI would say so.
func TestUsersUpdateRoleChangeKeepsTokenProjectBindings(t *testing.T) {
	pool := newUsersWritePostgresPool(t)
	prepareUsersWriteFixture(t, pool)
	router := usersWriteRouter(eliteacore.NewHandler(pool))

	const edited = "e2e-member@autotest.local"
	tokenID := seedBoundPAT(t, pool, "edited-member", edited, usersWriteProjectID)

	before := readMembers(t, router)
	userID := memberUserID(t, before, edited)
	if roles, _ := memberRoles(before, edited); len(roles) != 1 || roles[0] != "editor" {
		t.Fatalf("precondition: %s roles = %v, want [editor]", edited, roles)
	}

	recorder := usersWriteDo(t, router, http.MethodPut,
		fmt.Sprintf("/admin/users/default/%d", usersWriteProjectID),
		map[string]any{"userId": userID, "roles": []string{"admin"}})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	// The edit did happen, and the user is still a member.
	after := readMembers(t, router)
	roles, found := memberRoles(after, edited)
	if !found {
		t.Fatalf("%s disappeared from the member list after a role change", edited)
	}
	if len(roles) != 1 || roles[0] != "admin" {
		t.Fatalf("roles after edit = %v, want [admin]", roles)
	}

	projectID, bound := boundProjectID(t, pool, tokenID)
	if !bound || projectID != usersWriteProjectID {
		t.Fatalf("a role change unbound the member's access token (found=%v project=%d); "+
			"the user never lost membership, so the binding must survive", bound, projectID)
	}
}

// Atomicity. The binding revocation and the assignment delete are one
// transaction: a failure in either must leave the database exactly as it was.
// The trigger below makes the assignment delete fail deterministically, which
// no ordering of two bare pool.Exec calls could survive — the binding would
// already be gone, and the member would still be a member holding an unbound
// key.
func TestUsersDeleteBindingRevocationRollsBackWithTheAssignment(t *testing.T) {
	pool := newUsersWritePostgresPool(t)
	prepareUsersWriteFixture(t, pool)
	router := usersWriteRouter(eliteacore.NewHandler(pool))

	const member = "e2e-member@autotest.local"
	tokenID := seedBoundPAT(t, pool, "rollback-member", member, usersWriteProjectID)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, statement := range []string{
		`CREATE FUNCTION reject_assignment_delete() RETURNS trigger AS $$
		 BEGIN RAISE EXCEPTION 'assignment delete refused by test'; END $$ LANGUAGE plpgsql`,
		`CREATE TRIGGER reject_assignment_delete
		 BEFORE DELETE ON public.auth_core__project_user_role
		 FOR EACH ROW EXECUTE FUNCTION reject_assignment_delete()`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("install failure trigger: %v", err)
		}
	}

	before := readMembers(t, router)
	userID := memberUserID(t, before, member)

	recorder := usersWriteDo(t, router, http.MethodDelete,
		fmt.Sprintf("/admin/users/default/%d?id%%5B%%5D=%s", usersWriteProjectID, userID), nil)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("DELETE status = %d with the assignment delete refused, want 500 (body %s)",
			recorder.Code, recorder.Body.String())
	}

	if _, err := pool.Exec(ctx,
		`DROP TRIGGER reject_assignment_delete ON public.auth_core__project_user_role`); err != nil {
		t.Fatalf("remove failure trigger: %v", err)
	}

	// Nothing moved. The member is still a member…
	after := readMembers(t, router)
	if _, stillMember := memberRoles(after, member); !stillMember {
		t.Errorf("%s was removed even though the assignment delete failed", member)
	}
	// …so the binding must still be there. A committed revocation beside a
	// failed removal is the split this transaction exists to prevent.
	projectID, bound := boundProjectID(t, pool, tokenID)
	if !bound || projectID != usersWriteProjectID {
		t.Errorf("the binding was revoked (found=%v project=%d) while the member kept membership; "+
			"the two writes are not atomic", bound, projectID)
	}
}

/* ── fixture ─────────────────────────────────────────────────────────────── */

// TestUsersCreateBulkBatchIsPerAddress is the bulk-invite acceptance.
//
// The transport has taken `{emails: [...], roles: [...]}` since the port
// landed, but nothing proved what a MIXED batch does, and that is the whole
// contract: the reference writes one transaction per address, so one bad
// address must not roll back the good ones. A handler that stopped at the
// first failure — or that wrapped the loop in one transaction — passes every
// single-address test in this file and fails this one.
//
// The four rows below are the four states an address can end in, in one
// request: a new account, an existing project member, a malformed address, and
// (in the sub-test after it) a role the project does not define.
func TestUsersCreateBulkBatchIsPerAddress(t *testing.T) {
	pool := newUsersWritePostgresPool(t)
	prepareUsersWriteFixture(t, pool)
	router := usersWriteRouter(eliteacore.NewHandler(pool))

	const fresh = "bulk-new@autotest.local"
	const alsoFresh = "bulk-second@autotest.local"
	const alreadyMember = "e2e-member@autotest.local"
	const malformed = "bulk-not-an-email"

	t.Run("one bad address does not roll back the good ones", func(t *testing.T) {
		before := readMembers(t, router)

		recorder := usersWriteDo(t, router, http.MethodPost,
			fmt.Sprintf("/admin/users/default/%d", usersWriteProjectID),
			map[string]any{
				"emails": []string{fresh, alreadyMember, malformed, alsoFresh},
				"roles":  []string{"viewer"},
			})

		// 400 because SOME row failed. It is not a rejected request.
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("mixed batch status = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
		}

		var rows []struct {
			Msg     string `json:"msg"`
			Status  string `json:"status"`
			Email   string `json:"email"`
			ID      string `json:"id"`
			Outcome string `json:"outcome"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &rows); err != nil {
			t.Fatalf("decode mixed-batch body %q: %v", recorder.Body.String(), err)
		}
		if len(rows) != 4 {
			t.Fatalf("mixed batch returned %d rows, want one per requested address (4): %+v", len(rows), rows)
		}

		// ORDER IS THE REQUEST ORDER. A client renders these beside the
		// addresses the operator typed, so a reordered array mislabels them.
		want := []struct {
			email   string
			status  string
			outcome string
		}{
			{fresh, "ok", "invited"},
			{alreadyMember, "error", "already_member"},
			{malformed, "error", "invalid_email"},
			{alsoFresh, "ok", "invited"},
		}
		for index, expected := range want {
			if rows[index].Email != expected.email {
				t.Fatalf("row %d email = %q, want %q", index, rows[index].Email, expected.email)
			}
			if rows[index].Status != expected.status {
				t.Fatalf("row %d (%s) status = %q, want %q", index, expected.email, rows[index].Status, expected.status)
			}
			if rows[index].Outcome != expected.outcome {
				t.Fatalf("row %d (%s) outcome = %q, want %q", index, expected.email, rows[index].Outcome, expected.outcome)
			}
		}
		if rows[0].ID == "" || rows[3].ID == "" {
			t.Fatalf("an invited row carries no id: %+v", rows)
		}

		// THE assertion the per-address transaction exists for: both good
		// addresses are members, read back through the page's own GET.
		after := readMembers(t, router)
		for _, email := range []string{fresh, alsoFresh} {
			roles, found := memberRoles(after, email)
			if !found {
				t.Fatalf("%s absent from the member list after a 400 batch: %+v", email, after.Rows)
			}
			if len(roles) != 1 || roles[0] != "viewer" {
				t.Fatalf("%s roles = %v, want [viewer]", email, roles)
			}
		}
		if _, found := memberRoles(after, malformed); found {
			t.Fatalf("the malformed address %s was added as a member", malformed)
		}
		// The existing member keeps the role set it already had — an
		// already_member row must not silently re-grant.
		roles, _ := memberRoles(after, alreadyMember)
		if len(roles) != 1 || roles[0] != "editor" {
			t.Fatalf("existing member roles = %v after the batch, want [editor]", roles)
		}
		if after.Total != before.Total+2 {
			t.Fatalf("total = %d after a batch that added two members, want %d", after.Total, before.Total+2)
		}
	})

	t.Run("an unknown role rejects the whole batch before any address is written", func(t *testing.T) {
		// The role names are resolved ONCE, up front. This is the one
		// all-or-nothing check in the request, and it is deliberate: a role
		// nobody defined is a mistake in the form, not in an address.
		before := readMembers(t, router)

		recorder := usersWriteDo(t, router, http.MethodPost,
			fmt.Sprintf("/admin/users/default/%d", usersWriteProjectID),
			map[string]any{
				"emails": []string{"bulk-blocked-a@autotest.local", "bulk-blocked-b@autotest.local"},
				"roles":  []string{"viewer", "wizard"},
			})
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("unknown-role batch status = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
		}

		// Not the per-address array: a single error object, because no address
		// was tried at all.
		var refusal struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
			t.Fatalf("decode unknown-role body %q: %v", recorder.Body.String(), err)
		}
		if refusal.Error == "" {
			t.Fatalf("unknown-role refusal carries no reason: %s", recorder.Body.String())
		}

		after := readMembers(t, router)
		if after.Total != before.Total {
			t.Fatalf("total changed from %d to %d on a batch refused for its roles", before.Total, after.Total)
		}
		for _, email := range []string{"bulk-blocked-a@autotest.local", "bulk-blocked-b@autotest.local"} {
			var userCount int
			if err := pool.QueryRow(context.Background(),
				`SELECT COUNT(*) FROM auth_core__user WHERE email = $1`, email).Scan(&userCount); err != nil {
				t.Fatal(err)
			}
			if userCount != 0 {
				t.Fatalf("a batch refused for its roles still created the account %s", email)
			}
		}
	})
}

func prepareUsersWriteFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	statements := []string{
		`INSERT INTO public.auth_core__group (id, name) VALUES (1, 'Root')`,
		`INSERT INTO public.auth_core__user (email, name) VALUES
			('e2e-admin@autotest.local', 'E2E Admin'),
			('e2e-member@autotest.local', 'E2E Member')`,
		fmt.Sprintf(`INSERT INTO public.auth_core__project_role (project_id, name) VALUES
			(%d, 'admin'), (%d, 'editor'), (%d, 'viewer')`,
			usersWriteProjectID, usersWriteProjectID, usersWriteProjectID),
		fmt.Sprintf(`INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
			SELECT %d, u.id, r.id
			FROM public.auth_core__user u
			JOIN public.auth_core__project_role r ON r.project_id = %d AND r.name = 'admin'
			WHERE u.email = 'e2e-admin@autotest.local'`, usersWriteProjectID, usersWriteProjectID),
		fmt.Sprintf(`INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
			SELECT %d, u.id, r.id
			FROM public.auth_core__user u
			JOIN public.auth_core__project_role r ON r.project_id = %d AND r.name = 'editor'
			WHERE u.email = 'e2e-member@autotest.local'`, usersWriteProjectID, usersWriteProjectID),
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
}

func newUsersWritePostgresPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := os.Getenv(environment)
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", environment, err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open PostgreSQL admin pool: %v", err)
	}
	if err := adminPool.Ping(ctx); err != nil {
		adminPool.Close()
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	databaseName := fmt.Sprintf("elitea_users_write_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quotedDatabase); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated PostgreSQL integration database: %v", err)
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		if _, dropErr := adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); dropErr != nil {
			t.Errorf("drop database after pool open failure: %v", dropErr)
		}
		adminPool.Close()
		t.Fatalf("open isolated PostgreSQL integration database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		// 120 s, not the old 20 s to 30 s. This DROP queues behind the
		// CREATE DATABASE calls of every package that `go test ./...` runs at
		// the same time, so the wait is server load and not a hang. Two full
		// runs failed here with "drop isolated ... database: timeout" (#409).
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated PostgreSQL integration database: %v", err)
		}
		adminPool.Close()
	})

	if _, err := pool.Exec(ctx, dbschema.AuthCoreBaselineSQLCProjection); err != nil {
		t.Fatalf("apply auth_core baseline projection: %v", err)
	}

	// The REAL migration, not a hand-written CREATE TABLE. UsersDelete revokes
	// token bindings through this table on every removal, so a fixture shape
	// that drifted from migrations/shared/0071 would let the revocation pass
	// here and fail 42P01 in production. Reading the file means it cannot.
	// The foreign key inside it is guarded by to_regclass and resolves, because
	// the projection above created public.auth_core__token.
	bindingSQL, err := infradb.TokenBindingMigrationSQL()
	if err != nil {
		t.Fatalf("read token binding migration: %v", err)
	}
	if _, err := pool.Exec(ctx, bindingSQL); err != nil {
		t.Fatalf("apply token binding migration: %v", err)
	}
	return pool
}

/* ── token binding helpers (spec-llm-project-scope §7 invariant 3) ────────── */

// seedBoundPAT creates one access token for the named member and binds it to
// projectID, the way internal/api/v2/auth/tokens.go does after its membership
// check. It returns the token id.
func seedBoundPAT(t *testing.T, pool *pgxpool.Pool, label, email string, projectID int) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var tokenID int
	if err := pool.QueryRow(ctx, `
INSERT INTO public.auth_core__token (uuid, user_id, name)
SELECT $1, owner.id, $2
FROM public.auth_core__user AS owner
WHERE owner.email = $3
RETURNING id`, "pat-"+label, label, email).Scan(&tokenID); err != nil {
		t.Fatalf("seed token %s for %s: %v", label, email, err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_identity.token_project_binding (token_id, project_id) VALUES ($1, $2)`,
		tokenID, projectID); err != nil {
		t.Fatalf("bind token %s to project %d: %v", label, projectID, err)
	}
	return tokenID
}

// boundProjectID answers what the /llm edge would resolve for this token: the
// bound project, or nothing when the binding is gone.
func boundProjectID(t *testing.T, pool *pgxpool.Pool, tokenID int) (int, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var projectID int
	err := pool.QueryRow(ctx,
		`SELECT project_id FROM elitea_identity.token_project_binding WHERE token_id = $1`,
		tokenID).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false
	}
	if err != nil {
		t.Fatalf("read binding for token %d: %v", tokenID, err)
	}
	return projectID, true
}

func memberUserID(t *testing.T, listing usersListBody, email string) string {
	t.Helper()
	for _, row := range listing.Rows {
		if row.Email == email {
			return row.ID
		}
	}
	t.Fatalf("precondition: %s is not in the member list %+v", email, listing.Rows)
	return ""
}
