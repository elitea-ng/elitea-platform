package admin_test

// #255 acceptance for the two admin surfaces this issue ports into the admin
// package: `/admin/modes/administration` and `/admin/user_invite/administration`.
//
// A status code proves nothing about either of them. The reference DELETE on
// `modes` answers `{"ok": true}` from a handler whose body is a `# TODO` — it
// parses the composite id and returns success having deleted nothing — so a
// test that asserted 200 would pass against a stub that removes no role at all.
// Every case below therefore RE-READS through the product's own GET, and the
// state-changing ones read the underlying table too.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/admin"
	appmailer "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/mailer"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

/* ── harness ───────────────────────────────────────────────────────────── */

func decodeJSONBody(t *testing.T, payload []byte, target any) {
	t.Helper()
	if err := json.Unmarshal(payload, target); err != nil {
		t.Fatalf("decode %q: %v", string(payload), err)
	}
}

type modeAssignmentRow struct {
	ID        string `json:"id"`
	UserID    int    `json:"user_id"`
	UserEmail string `json:"user_email"`
	UserName  string `json:"user_name"`
	Mode      string `json:"mode"`
	Role      string `json:"role"`
}

type modeListing struct {
	Total int                 `json:"total"`
	Rows  []modeAssignmentRow `json:"rows"`
}

func (l modeListing) rolesFor(userID int, mode string) []string {
	roles := []string{}
	for _, row := range l.Rows {
		if row.UserID == userID && row.Mode == mode {
			roles = append(roles, row.Role)
		}
	}
	return roles
}

// modesRouter mounts the four routes exactly as internal/api/router.go does,
// minus the route-level permission middleware (covered by
// TestRequireCentralPermissions* in internal/api/middleware). The handler's own
// super_admin guard is exercised here, through the resolver.
func modesRouter(handler *admin.Handler, principal *auth.User) chi.Router {
	router := chi.NewRouter()
	if principal != nil {
		router.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), *principal)))
			})
		})
	}
	router.Get("/admin/modes/administration", handler.Modes)
	router.Post("/admin/modes/administration", handler.ModesAssign)
	router.Delete("/admin/modes/administration", handler.ModesRemove)
	router.Post("/admin/user_invite/administration", handler.UserInvite)
	return router
}

func readModes(t *testing.T, router chi.Router) modeListing {
	t.Helper()
	recorder := adminDo(t, router, http.MethodGet, "/admin/modes/administration", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET modes status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var listing modeListing
	decodeJSONBody(t, recorder.Body.Bytes(), &listing)
	if listing.Total != len(listing.Rows) {
		t.Fatalf("modes listing total = %d but carries %d rows", listing.Total, len(listing.Rows))
	}
	return listing
}

// storedModeRoles reads the assignment table directly. The GET is the product's
// own view; this is the check that the view is not the only thing that moved.
func storedModeRoles(t *testing.T, pool *pgxpool.Pool, userID int, mode string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT role.name
FROM public.auth_core__user_role assignment
JOIN public.auth_core__role role ON role.id = assignment.role_id
WHERE assignment.user_id = $1 AND role.mode = $2
ORDER BY role.name`, userID, mode)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	roles := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		roles = append(roles, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return roles
}

// newModesEnvironment reuses the roles-fixture database — it applies the same
// two baseline projections and seeds the central role vocabulary these
// endpoints assign from — and adds the two users this file acts on.
func newModesEnvironment(
	t *testing.T, resolver auth.PermissionResolver, extra ...admin.Option,
) (*pgxpool.Pool, chi.Router, int, int) {
	t.Helper()
	pool := newRolesPool(t)
	prepareRolesFixture(t, pool)

	ctx := context.Background()
	var operatorID, memberID int
	if err := pool.QueryRow(ctx, `
INSERT INTO public.auth_core__user (email, name)
VALUES ('modes-operator@autotest.local', 'Operator') RETURNING id`).Scan(&operatorID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO public.auth_core__user (email, name)
VALUES ('modes-member@autotest.local', 'Member') RETURNING id`).Scan(&memberID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__user_role (user_id, role_id)
SELECT $1, role.id FROM public.auth_core__role role
WHERE role.mode = 'administration' AND role.name = 'admin'`, operatorID); err != nil {
		t.Fatal(err)
	}

	options := []admin.Option{}
	if resolver != nil {
		options = append(options, admin.WithPermissionResolver(resolver))
	}
	options = append(options, extra...)
	router := modesRouter(admin.NewHandler(pool, options...), &auth.User{ID: "1", UserID: "1"})
	return pool, router, operatorID, memberID
}

/* ── modes ─────────────────────────────────────────────────────────────── */

func TestModesListingReportsEveryCentralAssignment(t *testing.T) {
	_, router, operatorID, _ := newModesEnvironment(t, nil)

	listing := readModes(t, router)

	roles := listing.rolesFor(operatorID, "administration")
	if len(roles) != 1 || roles[0] != "admin" {
		t.Fatalf("operator holds %v in administration mode, want [admin]", roles)
	}
	// The composite id is what the DELETE takes back apart; a listing that
	// synthesised it differently would make every removal a 400.
	want := fmt.Sprintf("%d:administration:admin", operatorID)
	found := false
	for _, row := range listing.Rows {
		if row.ID == want {
			found = true
			if row.UserEmail != "modes-operator@autotest.local" {
				t.Fatalf("row %s carries email %q", row.ID, row.UserEmail)
			}
		}
	}
	if !found {
		t.Fatalf("listing has no row with id %q; rows = %+v", want, listing.Rows)
	}
}

func TestModesAssignWritesAndIsSingleRolePerMode(t *testing.T) {
	pool, router, _, memberID := newModesEnvironment(t, nil)

	recorder := adminDo(t, router, http.MethodPost, "/admin/modes/administration",
		map[string]any{"user_id": memberID, "mode": "administration", "role": "editor"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	// RE-READ through the product's own GET…
	if roles := readModes(t, router).rolesFor(memberID, "administration"); len(roles) != 1 || roles[0] != "editor" {
		t.Fatalf("member holds %v after the assignment, want [editor]", roles)
	}
	// …and through the table the listing is derived from.
	if roles := storedModeRoles(t, pool, memberID, "administration"); len(roles) != 1 || roles[0] != "editor" {
		t.Fatalf("stored administration roles = %v, want [editor]", roles)
	}

	// Assigning again REPLACES: auth_core's assign_user_to_role drops every
	// assignment the user holds in that mode first.
	recorder = adminDo(t, router, http.MethodPost, "/admin/modes/administration",
		map[string]any{"user_id": memberID, "mode": "administration", "role": "viewer"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("second POST status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if roles := storedModeRoles(t, pool, memberID, "administration"); len(roles) != 1 || roles[0] != "viewer" {
		t.Fatalf("stored administration roles = %v after re-assignment, want [viewer]", roles)
	}
}

func TestModesAssignResolvesAnEmailAddress(t *testing.T) {
	pool, router, _, memberID := newModesEnvironment(t, nil)

	recorder := adminDo(t, router, http.MethodPost, "/admin/modes/administration",
		map[string]any{"user_id": "modes-member@autotest.local", "mode": "administration", "role": "editor"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST by email status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if roles := storedModeRoles(t, pool, memberID, "administration"); len(roles) != 1 || roles[0] != "editor" {
		t.Fatalf("stored administration roles = %v, want [editor]", roles)
	}
}

func TestModesAssignRejectsAnUndefinedRoleAndKeepsTheCurrentOne(t *testing.T) {
	pool, router, _, memberID := newModesEnvironment(t, nil)

	if recorder := adminDo(t, router, http.MethodPost, "/admin/modes/administration",
		map[string]any{"user_id": memberID, "mode": "administration", "role": "editor"}); recorder.Code != http.StatusOK {
		t.Fatalf("setup POST status = %d, want 200", recorder.Code)
	}

	recorder := adminDo(t, router, http.MethodPost, "/admin/modes/administration",
		map[string]any{"user_id": memberID, "mode": "administration", "role": "wizard"})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("POST with an undefined role status = %d, want 400 (body %s)",
			recorder.Code, recorder.Body.String())
	}
	// THE assertion: the assignment is delete-then-insert, so a non-atomic
	// implementation would have revoked `editor` and granted nothing.
	if roles := storedModeRoles(t, pool, memberID, "administration"); len(roles) != 1 || roles[0] != "editor" {
		t.Fatalf("stored administration roles = %v after a rejected assignment, want [editor]", roles)
	}
}

func TestModesAssignRefusesDefaultMode(t *testing.T) {
	pool, router, _, memberID := newModesEnvironment(t, nil)

	recorder := adminDo(t, router, http.MethodPost, "/admin/modes/administration",
		map[string]any{"user_id": memberID, "mode": "default", "role": "admin"})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("POST for default mode status = %d, want 400 (body %s)",
			recorder.Code, recorder.Body.String())
	}
	// Default-mode roles are project membership. Writing a central row for one
	// would grant a permission set no project scopes.
	if roles := storedModeRoles(t, pool, memberID, "default"); len(roles) != 0 {
		t.Fatalf("stored default-mode roles = %v, want none", roles)
	}
}

// TestModesRemoveActuallyRemoves is the case the reference cannot pass: its
// DELETE returns `{"ok": True}` and deletes nothing.
func TestModesRemoveActuallyRemoves(t *testing.T) {
	pool, router, _, memberID := newModesEnvironment(t, nil)

	if recorder := adminDo(t, router, http.MethodPost, "/admin/modes/administration",
		map[string]any{"user_id": memberID, "mode": "administration", "role": "editor"}); recorder.Code != http.StatusOK {
		t.Fatalf("setup POST status = %d, want 200", recorder.Code)
	}

	target := fmt.Sprintf("/admin/modes/administration?id=%d:administration:editor", memberID)
	recorder := adminDo(t, router, http.MethodDelete, target, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	if roles := readModes(t, router).rolesFor(memberID, "administration"); len(roles) != 0 {
		t.Fatalf("member still holds %v after removal", roles)
	}
	if roles := storedModeRoles(t, pool, memberID, "administration"); len(roles) != 0 {
		t.Fatalf("stored administration roles = %v after removal, want none", roles)
	}

	// A second removal removed nothing and says so, rather than reporting a
	// success that describes no change.
	if again := adminDo(t, router, http.MethodDelete, target, nil); again.Code != http.StatusNotFound {
		t.Fatalf("repeat DELETE status = %d, want 404 (body %s)", again.Code, again.Body.String())
	}
}

// TestModesRemoveRefusesDefaultMode pins the symmetry with ModesAssign. The
// listing reports central default-mode assignments (a fresh database seeds
// user 1's, 001_initial.sql), and removing one through this endpoint could not
// be undone through it — POST refuses `default` outright — so the row would be
// gone from the only surface that showed it.
func TestModesRemoveRefusesDefaultMode(t *testing.T) {
	pool, router, _, memberID := newModesEnvironment(t, nil)

	if _, err := pool.Exec(context.Background(), `
INSERT INTO public.auth_core__user_role (user_id, role_id)
SELECT $1, role.id FROM public.auth_core__role role
WHERE role.mode = 'default' AND role.name = 'admin'`, memberID); err != nil {
		t.Fatal(err)
	}
	if roles := storedModeRoles(t, pool, memberID, "default"); len(roles) != 1 {
		t.Fatalf("fixture holds %v in default mode, want [admin]", roles)
	}

	recorder := adminDo(t, router, http.MethodDelete,
		fmt.Sprintf("/admin/modes/administration?id=%d:default:admin", memberID), nil)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("DELETE of a default-mode assignment = %d, want 400 (body %s)",
			recorder.Code, recorder.Body.String())
	}
	if roles := storedModeRoles(t, pool, memberID, "default"); len(roles) != 1 || roles[0] != "admin" {
		t.Fatalf("default-mode roles = %v after a refused removal, want [admin]", roles)
	}
}

func TestModesRemoveRejectsAMalformedID(t *testing.T) {
	_, router, operatorID, _ := newModesEnvironment(t, nil)

	for _, id := range []string{"", "3", "3:administration", "abc:administration:admin"} {
		recorder := adminDo(t, router, http.MethodDelete, "/admin/modes/administration?id="+id, nil)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("DELETE ?id=%q status = %d, want 400", id, recorder.Code)
		}
	}
	// Nothing moved.
	if roles := readModes(t, router).rolesFor(operatorID, "administration"); len(roles) != 1 {
		t.Fatalf("operator holds %v after four rejected removals, want [admin]", roles)
	}
}

// TestModesRefusesSuperAdminEscalation pins the guard that makes this endpoint
// safe to expose at all: it writes the same table `/admin/auth_users`'s
// set_admin_role writes, so without the guard it would be a second, unguarded
// route to the platform's highest role.
func TestModesRefusesSuperAdminEscalation(t *testing.T) {
	pool, router, _, memberID := newModesEnvironment(t, grantingResolver("modes.users"))

	recorder := adminDo(t, router, http.MethodPost, "/admin/modes/administration",
		map[string]any{"user_id": memberID, "mode": "administration", "role": "super_admin"})
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("POST super_admin status = %d, want 403 (body %s)", recorder.Code, recorder.Body.String())
	}
	if roles := storedModeRoles(t, pool, memberID, "administration"); len(roles) != 0 {
		t.Fatalf("stored administration roles = %v after a refused escalation, want none", roles)
	}

	// With the permission, the same call succeeds — otherwise the guard would
	// be indistinguishable from "super_admin is never assignable".
	_, allowed, _, allowedMemberID := newModesEnvironment(t,
		grantingResolver("modes.users", "admin.auth.users.super_admin"))
	if recorder := adminDo(t, allowed, http.MethodPost, "/admin/modes/administration",
		map[string]any{"user_id": allowedMemberID, "mode": "administration", "role": "super_admin"},
	); recorder.Code != http.StatusOK {
		t.Fatalf("POST super_admin as a super_admin status = %d, want 200 (body %s)",
			recorder.Code, recorder.Body.String())
	}
}

/* ── user_invite ───────────────────────────────────────────────────────── */

type inviteResponse struct {
	OK                  bool   `json:"ok"`
	ID                  int    `json:"id"`
	Email               string `json:"email"`
	Created             bool   `json:"created"`
	InvitationDelivered bool   `json:"invitation_delivered"`
	InvitationDelivery  string `json:"invitation_delivery"`
	Error               string `json:"error"`
}

func TestUserInviteCreatesThePlatformUserRecord(t *testing.T) {
	pool, router, _, _ := newModesEnvironment(t, nil)
	const address = "invited@autotest.local"

	recorder := adminDo(t, router, http.MethodPost, "/admin/user_invite/administration",
		map[string]any{"user_name": "Invited Person", "user_email": " Invited@Autotest.Local "})
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var body inviteResponse
	decodeJSONBody(t, recorder.Body.Bytes(), &body)
	if !body.OK || !body.Created || body.ID <= 0 {
		t.Fatalf("invite response = %+v, want ok/created with a real id", body)
	}
	if body.Email != address {
		t.Fatalf("invite normalised the address to %q, want %q", body.Email, address)
	}
	// With no relay configured the endpoint sends nothing and SAYS so, rather
	// than answering a bare {"ok": true} a console would render as "invitation
	// sent". Gap G7 made the relay configurable; it did not make an
	// unconfigured deployment start claiming deliveries.
	if body.InvitationDelivered {
		t.Fatal("invitation_delivered is true on a deployment with no relay")
	}
	if body.InvitationDelivery == "" {
		t.Fatal("no invitation_delivery reason: the console cannot tell the operator what to configure")
	}

	// THE assertion: the row exists, with the submitted name.
	var name string
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(name, '') FROM public.auth_core__user WHERE lower(email) = $1`,
		address).Scan(&name); err != nil {
		t.Fatalf("the invited address has no auth_core__user row: %v", err)
	}
	if name != "Invited Person" {
		t.Fatalf("invited user name = %q, want %q", name, "Invited Person")
	}
}

func TestUserInviteIsIdempotentAndNeverRenames(t *testing.T) {
	pool, router, _, _ := newModesEnvironment(t, nil)
	const address = "modes-member@autotest.local"

	recorder := adminDo(t, router, http.MethodPost, "/admin/user_invite/administration",
		map[string]any{"user_name": "Renamed", "user_email": address})
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var body inviteResponse
	decodeJSONBody(t, recorder.Body.Bytes(), &body)
	if body.Created {
		t.Fatal("inviting an existing address reported created=true")
	}

	var rows int
	var name string
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*), COALESCE(MIN(name), '') FROM public.auth_core__user WHERE lower(email) = $1`,
		address).Scan(&rows, &name); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("auth_core__user rows for %s = %d, want 1", address, rows)
	}
	// "Invite" must not be a rename primitive for an existing account.
	if name != "Member" {
		t.Fatalf("existing user name = %q after a second invite, want %q", name, "Member")
	}
}

func TestUserInviteRejectsIncompleteInput(t *testing.T) {
	pool, router, _, _ := newModesEnvironment(t, nil)

	var before int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM public.auth_core__user`).Scan(&before); err != nil {
		t.Fatal(err)
	}

	for name, body := range map[string]map[string]any{
		"no name":       {"user_email": "someone@autotest.local"},
		"no email":      {"user_name": "Someone"},
		"blank name":    {"user_name": "   ", "user_email": "someone@autotest.local"},
		"invalid email": {"user_name": "Someone", "user_email": "someone-at-autotest"},
		"display form":  {"user_name": "Someone", "user_email": "Someone <someone@autotest.local>"},
	} {
		t.Run(name, func(t *testing.T) {
			recorder := adminDo(t, router, http.MethodPost, "/admin/user_invite/administration", body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
			}
		})
	}

	var after int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM public.auth_core__user`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("user count moved from %d to %d across five rejected invites", before, after)
	}
}

// TestModesRefusesSuperAdminEscalationInEveryMode closes the second route to
// the platform's highest role.
//
// DEFECT: the escalation guard in ModesAssign ran only when the request body
// named the `administration` mode. The matching guard in ModesRemove ran only
// when the body named `administration` too. `public.auth_core__role` is keyed UNIQUE
// (name, mode), so a database restored from a legacy dump also carries a
// `super_admin` role in the `developer` mode
// (testdata/postgres/legacy-rbac-matrix.json). A caller who held `modes.users`
// but not `admin.auth.users.super_admin` could therefore POST
// {"mode":"developer","role":"super_admin"}: the INSERT found the role row and
// wrote the assignment, and the guard never ran.
func TestModesRefusesSuperAdminEscalationInEveryMode(t *testing.T) {
	pool, router, _, memberID := newModesEnvironment(t, grantingResolver("modes.users"))

	// The role row a legacy dump carries beside the administration-mode one.
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO public.auth_core__role (name, mode) VALUES ('super_admin', 'developer')`); err != nil {
		t.Fatalf("seed developer-mode super_admin role: %v", err)
	}

	recorder := adminDo(t, router, http.MethodPost, "/admin/modes/administration",
		map[string]any{"user_id": memberID, "mode": "developer", "role": "super_admin"})
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("POST developer-mode super_admin status = %d, want 403 (body %s)",
			recorder.Code, recorder.Body.String())
	}
	if roles := storedModeRoles(t, pool, memberID, "developer"); len(roles) != 0 {
		t.Fatalf("stored developer roles = %v after a refused escalation, want none", roles)
	}
}

// TestModesRemoveRefusesADeveloperModeSuperAdmin covers the DELETE half of the
// same widening. The composite id names the mode, so the guard has to key on
// the role name alone.
func TestModesRemoveRefusesADeveloperModeSuperAdmin(t *testing.T) {
	pool, router, _, memberID := newModesEnvironment(t, grantingResolver("modes.users"))
	ctx := context.Background()

	if _, err := pool.Exec(ctx,
		`INSERT INTO public.auth_core__role (name, mode) VALUES ('super_admin', 'developer')`); err != nil {
		t.Fatalf("seed developer-mode super_admin role: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__user_role (user_id, role_id)
SELECT $1, role.id FROM public.auth_core__role role
WHERE role.mode = 'developer' AND role.name = 'super_admin'`, memberID); err != nil {
		t.Fatalf("seed developer-mode assignment: %v", err)
	}

	target := fmt.Sprintf("/admin/modes/administration?id=%d:developer:super_admin", memberID)
	recorder := adminDo(t, router, http.MethodDelete, target, nil)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("DELETE developer-mode super_admin status = %d, want 403 (body %s)",
			recorder.Code, recorder.Body.String())
	}
	if roles := storedModeRoles(t, pool, memberID, "developer"); len(roles) != 1 {
		t.Fatalf("stored developer roles = %v after a refused removal, want the assignment intact", roles)
	}
}

// TestModesAssignScopesTheDemotionGuardToAdministration pins the OTHER half of
// the guard, the arm that protects an existing super_admin from a demotion.
//
// currentAdminRole reads the administration-mode role only, and the DELETE in
// ModesAssign removes assignments in the REQUESTED mode only. A developer-mode
// write therefore demotes nobody. An unscoped demotion arm refused that write
// and gave nothing back: fail-closed, but wrong.
func TestModesAssignScopesTheDemotionGuardToAdministration(t *testing.T) {
	pool, router, _, memberID := newModesEnvironment(t, grantingResolver("modes.users"))
	ctx := context.Background()

	if _, err := pool.Exec(ctx,
		`INSERT INTO public.auth_core__role (name, mode) VALUES ('editor', 'developer')`); err != nil {
		t.Fatalf("seed developer-mode editor role: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__user_role (user_id, role_id)
SELECT $1, role.id FROM public.auth_core__role role
WHERE role.mode = 'administration' AND role.name = 'super_admin'`, memberID); err != nil {
		t.Fatalf("seed administration super_admin: %v", err)
	}

	// The caller holds `modes.users` but NOT `admin.auth.users.super_admin`.
	recorder := adminDo(t, router, http.MethodPost, "/admin/modes/administration",
		map[string]any{"user_id": memberID, "mode": "developer", "role": "editor"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST developer-mode editor status = %d, want 200 (body %s)",
			recorder.Code, recorder.Body.String())
	}
	if roles := storedModeRoles(t, pool, memberID, "developer"); len(roles) != 1 || roles[0] != "editor" {
		t.Fatalf("stored developer roles = %v, want [editor]", roles)
	}
	// The administration assignment the guard protects never moved.
	if roles := storedModeRoles(t, pool, memberID, "administration"); len(roles) != 1 ||
		roles[0] != "super_admin" {
		t.Fatalf("stored administration roles = %v, want [super_admin]", roles)
	}

	// The same caller still cannot demote that user INSIDE administration.
	recorder = adminDo(t, router, http.MethodPost, "/admin/modes/administration",
		map[string]any{"user_id": memberID, "mode": "administration", "role": "admin"})
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("POST administration demotion status = %d, want 403 (body %s)",
			recorder.Code, recorder.Body.String())
	}
	if roles := storedModeRoles(t, pool, memberID, "administration"); len(roles) != 1 ||
		roles[0] != "super_admin" {
		t.Fatalf("stored administration roles = %v after a refused demotion, want [super_admin]", roles)
	}
}

/* ── invitation DELIVERY (gap G7) ──────────────────────────────────────── */

// invitationSink is a mailer that records what it was asked to send. It stands
// where an SMTP relay would, so the assertions below are about the HANDLER's
// decision — "was an invitation delivered" — rather than about a network.
type invitationSink struct {
	configured bool
	fail       error
	invited    []appmailer.Invitation
}

func (s *invitationSink) Configured(context.Context) bool { return s.configured }

func (s *invitationSink) SendTest(context.Context, string) error { return nil }

func (s *invitationSink) SendInvitation(_ context.Context, invitation appmailer.Invitation) error {
	if s.fail != nil {
		return s.fail
	}
	s.invited = append(s.invited, invitation)
	return nil
}

// The gap G7 acceptance: with a relay resolved, the admin invite DELIVERS, and
// the response says so.
//
// This is the field a console renders as "invitation sent". Before this change
// it was false on every deployment whose environment did not carry SMTP_HOST
// before the pods started, which is what made invitations impossible to turn
// on from the admin console.
func TestUserInviteDeliversWhenARelayIsConfigured(t *testing.T) {
	sink := &invitationSink{configured: true}
	_, router, _, _ := newModesEnvironment(t, nil, admin.WithMailer(sink))

	recorder := adminDo(t, router, http.MethodPost, "/admin/user_invite/administration",
		map[string]any{"user_name": "Delivered Person", "user_email": "delivered@autotest.local"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var body inviteResponse
	decodeJSONBody(t, recorder.Body.Bytes(), &body)
	if !body.InvitationDelivered {
		t.Fatalf("invitation_delivered = false with a configured relay: %+v", body)
	}
	if len(sink.invited) != 1 || sink.invited[0].Email != "delivered@autotest.local" {
		t.Fatalf("the relay received %+v", sink.invited)
	}
	if sink.invited[0].Name != "Delivered Person" {
		t.Errorf("the invitation lost the invited person's name: %+v", sink.invited[0])
	}
}

// A relay that REFUSES the message must not be reported as a delivery, and the
// record must still exist: the operator can grant roles now and resend later.
func TestUserInviteReportsARefusedRelay(t *testing.T) {
	sink := &invitationSink{configured: true, fail: errors.New("relay refused")}
	pool, router, _, _ := newModesEnvironment(t, nil, admin.WithMailer(sink))

	recorder := adminDo(t, router, http.MethodPost, "/admin/user_invite/administration",
		map[string]any{"user_name": "Refused Person", "user_email": "refused@autotest.local"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var body inviteResponse
	decodeJSONBody(t, recorder.Body.Bytes(), &body)
	if body.InvitationDelivered {
		t.Fatal("a refused message was reported as delivered")
	}
	if body.InvitationDelivery == "" {
		t.Fatal("a refused message reported no reason")
	}
	var exists bool
	if err := pool.QueryRow(context.Background(),
		`SELECT true FROM public.auth_core__user WHERE lower(email) = $1`,
		"refused@autotest.local").Scan(&exists); err != nil || !exists {
		t.Fatalf("the record was rolled back by a relay failure: %v", err)
	}
}
