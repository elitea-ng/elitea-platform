package admin_test

// The admin.access.changed security audit events of the role write paths
// (ADR-0059 R1.5, issue 2509): one chained event per change kind, with the
// actor, the subject and the before and after sets, appended in the change's
// transaction. The identity trust kind is in
// identity_trust_access_event_postgres_integration_test.go (it needs the full
// corpus), the global provider kind in internal/api/v2/configurations, and the
// token scopes kind is wired by R1.3a (issue 2507).
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/admin"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// accessEventRow is one admin.access.changed event of the outbox.
type accessEventRow struct {
	Partition string
	Subject   string
	Action    string
	Principal string
	Access    struct {
		Kind       string   `json:"kind"`
		Mode       string   `json:"mode"`
		Source     string   `json:"source"`
		Before     []string `json:"before"`
		After      []string `json:"after"`
		Fields     []string `json:"fields"`
		Membership *struct {
			ActorIsMember        bool `json:"actor_is_member"`
			MemberHoldsAdminRole bool `json:"member_holds_admin_role"`
			Bulk                 bool `json:"bulk"`
		} `json:"membership"`
	}
}

func accessEvents(t *testing.T, pool *pgxpool.Pool) []accessEventRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT partition_key, event FROM elitea_runtime.security_audit_outbox
WHERE event_type = $1 ORDER BY seq`, string(securityaudit.TypeAdminAccessChanged))
	if err != nil {
		t.Fatalf("read the outbox: %v", err)
	}
	defer rows.Close()
	var events []accessEventRow
	for rows.Next() {
		var partition string
		var raw []byte
		if err := rows.Scan(&partition, &raw); err != nil {
			t.Fatalf("scan the outbox: %v", err)
		}
		var envelope struct {
			Subject string `json:"subject"`
			Data    struct {
				Action       string          `json:"action"`
				PrincipalRef string          `json:"principal_ref"`
				Access       json.RawMessage `json:"access"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		event := accessEventRow{
			Partition: partition, Subject: envelope.Subject,
			Action: envelope.Data.Action, Principal: envelope.Data.PrincipalRef,
		}
		if err := json.Unmarshal(envelope.Data.Access, &event.Access); err != nil {
			t.Fatalf("decode the access change %s: %v", envelope.Data.Access, err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the outbox: %v", err)
	}
	return events
}

// requireOneAccessEvent asserts exactly one event and returns it.
func requireOneAccessEvent(t *testing.T, pool *pgxpool.Pool) accessEventRow {
	t.Helper()
	events := accessEvents(t, pool)
	if len(events) != 1 {
		t.Fatalf("admin.access.changed events = %+v, want exactly one", events)
	}
	return events[0]
}

// requireChainsVerify runs the security audit verifier over each partition.
func requireChainsVerify(t *testing.T, pool *pgxpool.Pool, partitions ...int64) {
	t.Helper()
	for _, partition := range partitions {
		report, err := securityaudit.Verify(context.Background(), pool, securityaudit.SourceOutbox, partition, 1)
		if err != nil {
			t.Fatalf("verify partition %d: %v", partition, err)
		}
		if report.Break != nil || report.Checked == 0 {
			t.Fatalf("partition %d: report %+v, want an unbroken non-empty chain", partition, report)
		}
	}
}

func setOf(values ...string) string {
	sorted := append([]string{}, values...)
	slices.Sort(sorted)
	return strings.Join(sorted, ",")
}

func TestARoleAssignmentAppendsOneAccessChangedEvent(t *testing.T) {
	env := newEscalationEnvironment(t)
	plain, actor := env.ids[escalationPlainTarget], env.ids[escalationSuper]

	expectStatus(t, adminDo(t, env.as(escalationSuper), http.MethodPost, "/u/admin/auth_users/administration",
		map[string]any{"action": "set_admin_role", "user_id": plain, "role_name": "editor"}), http.StatusOK)

	event := requireOneAccessEvent(t, env.pool)
	if event.Action != "admin.access.role_assignment" || event.Partition != "0" ||
		event.Principal != fmt.Sprintf("user:%d", actor) || event.Subject != fmt.Sprintf("user/%d", plain) {
		t.Fatalf("event = %+v", event)
	}
	if event.Access.Kind != "role_assignment" || event.Access.Mode != "administration" ||
		event.Access.Source != "console" || setOf(event.Access.Before...) != "" ||
		setOf(event.Access.After...) != "editor" {
		t.Fatalf("access = %+v", event.Access)
	}

	// The removal through the modes route is the second event, with the sets
	// reversed.
	expectStatus(t, adminDo(t, env.as(escalationSuper), http.MethodDelete,
		fmt.Sprintf("/o/admin/modes/administration?id=%d:administration:editor", plain), nil), http.StatusOK)
	events := accessEvents(t, env.pool)
	if len(events) != 2 || setOf(events[1].Access.Before...) != "editor" || len(events[1].Access.After) != 0 {
		t.Fatalf("events = %+v, want the removal second", events)
	}
	requireChainsVerify(t, env.pool, 0)
}

// Assigning a role the person already holds changes nothing and appends
// nothing.
func TestAnUnchangedRoleSetAppendsNoEvent(t *testing.T) {
	env := newEscalationEnvironment(t)
	target := env.ids[escalationTarget] // holds admin
	expectStatus(t, adminDo(t, env.as(escalationSuper), http.MethodPost, "/o/admin/modes/administration",
		map[string]any{"user_id": target, "mode": "administration", "role": "admin"}), http.StatusOK)
	if events := accessEvents(t, env.pool); len(events) != 0 {
		t.Fatalf("events = %+v, want none", events)
	}
}

func TestARolePermissionEditAppendsOneAccessChangedEvent(t *testing.T) {
	env := newEscalationEnvironment(t)
	before := grantsSQL(t, env.pool, "administration", "admin")

	expectStatus(t, saveCell(t, env.as(escalationSuper), centralMatrix, "models.alpha.view", "admin", true),
		http.StatusOK)

	event := requireOneAccessEvent(t, env.pool)
	if event.Action != "admin.access.role_permissions" || event.Subject != "role/administration:admin" ||
		event.Principal != fmt.Sprintf("user:%d", env.ids[escalationSuper]) {
		t.Fatalf("event = %+v", event)
	}
	if setOf(event.Access.Before...) != setOf(before...) ||
		setOf(event.Access.After...) != setOf(append(before, "models.alpha.view")...) {
		t.Fatalf("access = %+v, before %v", event.Access, before)
	}
	requireChainsVerify(t, env.pool, 0)
}

func TestARoleCreateAndDeleteAppendOneEventEach(t *testing.T) {
	env := newEscalationEnvironment(t)
	router := env.as(escalationSuper)

	expectStatus(t, adminDo(t, router, http.MethodPost, "/r/admin/roles/administration/administration",
		map[string]any{"name": "auditors"}), http.StatusCreated)
	created := requireOneAccessEvent(t, env.pool)
	if created.Action != "admin.access.role_created" || created.Subject != "role/administration:auditors" ||
		len(created.Access.Before) != 0 || len(created.Access.After) != 0 {
		t.Fatalf("created = %+v", created)
	}

	// A role with a permission, so the deletion names the set it took.
	if _, err := env.pool.Exec(context.Background(), `
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT id, 'models.alpha.view' FROM public.auth_core__role WHERE mode = 'administration' AND name = 'auditors'`); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, adminDo(t, router, http.MethodDelete, "/r/admin/roles/administration/administration",
		map[string]any{"name": "auditors"}), http.StatusOK)
	events := accessEvents(t, env.pool)
	if len(events) != 2 {
		t.Fatalf("events = %+v, want the create and the delete", events)
	}
	deleted := events[1]
	if deleted.Action != "admin.access.role_deleted" || deleted.Subject != "role/administration:auditors" ||
		setOf(deleted.Access.Before...) != "models.alpha.view" || len(deleted.Access.After) != 0 {
		t.Fatalf("deleted = %+v", deleted)
	}
	requireChainsVerify(t, env.pool, 0)
}

func TestACentralMembershipWriteAppendsOneEventWithItsFacts(t *testing.T) {
	env := newEscalationEnvironment(t)
	plain := env.ids[escalationPlainTarget]
	target := env.ids[escalationTarget] // holds the administration role admin

	expectStatus(t, adminDo(t, env.as(escalationSuper), http.MethodPost, "/b/admin/invites_bulk/administration",
		map[string]any{"users": []int{plain}, "projects": []int{2}, "role": "viewer"}), http.StatusOK)
	event := requireOneAccessEvent(t, env.pool)
	if event.Action != "admin.access.project_membership" || event.Partition != "2" ||
		event.Subject != fmt.Sprintf("user/%d", plain) || len(event.Access.Before) != 0 ||
		setOf(event.Access.After...) != "viewer" || event.Access.Membership == nil {
		t.Fatalf("event = %+v", event)
	}
	if facts := *event.Access.Membership; facts.ActorIsMember || facts.MemberHoldsAdminRole || !facts.Bulk {
		t.Fatalf("facts = %+v, want a bulk write by a non-member of a member without an administration role", facts)
	}

	// A new member who holds an administration role is the fact R1.6 rates.
	expectStatus(t, adminDo(t, env.as(escalationSuper), http.MethodPost, "/b/admin/invites_bulk/administration",
		map[string]any{"users": []int{target}, "projects": []int{2}, "role": "viewer"}), http.StatusOK)
	events := accessEvents(t, env.pool)
	if len(events) != 2 || events[1].Access.Membership == nil || !events[1].Access.Membership.MemberHoldsAdminRole {
		t.Fatalf("events = %+v, want the second to name an administration role holder", events)
	}
	requireChainsVerify(t, env.pool, 2)
}

// A failed append rolls the change back: the role is not assigned, no grant
// history row is written and no event is in the outbox.
func TestAFailedAppendRollsTheRoleChangeBack(t *testing.T) {
	env := newEscalationEnvironment(t)
	plain, super := env.ids[escalationPlainTarget], env.ids[escalationSuper]
	id := fmt.Sprint(super)
	principal := &auth.User{ID: id, UserID: id, CredentialKind: auth.CredentialKindSession}
	handler := admin.NewHandler(env.pool,
		admin.WithPermissionResolver(legacyrbac.NewPostgresResolver(env.pool)),
		admin.WithAccessAuditAppender(securityaudit.FailingAppender{Err: errors.New("injected append failure")}))
	router := chi.NewRouter()
	router.Mount("/u", adminUsersRouter(handler, principal))
	router.Mount("/o", modesRouter(handler, principal))
	router.Mount("/r", roleWriteRouter(handler, nil, principal))
	router.Mount("/m", rolesRouter(handler, nil, principal))

	expectStatus(t, adminDo(t, router, http.MethodPost, "/u/admin/auth_users/administration",
		map[string]any{"action": "set_admin_role", "user_id": plain, "role_name": "editor"}),
		http.StatusInternalServerError)
	expectStatus(t, adminDo(t, router, http.MethodPost, "/o/admin/modes/administration",
		map[string]any{"user_id": plain, "mode": "administration", "role": "editor"}),
		http.StatusInternalServerError)
	if history := grantHistory(t, env.pool, plain); len(history) != 0 {
		t.Fatalf("a rolled-back change wrote history: %+v", history)
	}
	var held int
	if err := env.pool.QueryRow(context.Background(), `
SELECT count(*) FROM public.auth_core__user_role WHERE user_id = $1`, plain).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if held != 0 {
		t.Fatalf("the user holds %d roles after a failed append, want 0", held)
	}

	expectStatus(t, adminDo(t, router, http.MethodPost, "/r/admin/roles/administration/administration",
		map[string]any{"name": "auditors"}), http.StatusInternalServerError)
	if centralRoleExists(t, env.pool, "administration", "auditors") {
		t.Fatal("a role create survived a failed append")
	}
	before := grantsSQL(t, env.pool, "administration", "admin")
	if recorder := saveCell(t, router, centralMatrix, "models.alpha.view", "admin", true); recorder.Code == http.StatusOK {
		t.Fatalf("a matrix save answered 200 with a failed append: %s", recorder.Body.String())
	}
	if after := grantsSQL(t, env.pool, "administration", "admin"); setOf(after...) != setOf(before...) {
		t.Fatalf("the matrix moved after a failed append: %v became %v", before, after)
	}
	if events := accessEvents(t, env.pool); len(events) != 0 {
		t.Fatalf("events = %+v, want none", events)
	}
}

// Deleting a central `default` role also removes its same-named project
// copies; the one role_deleted event says so.
func TestADefaultRoleDeleteRecordsTheProjectCopiesItRemoved(t *testing.T) {
	env := newEscalationEnvironment(t)
	router := env.as(escalationSuper)
	expectStatus(t, adminDo(t, router, http.MethodPost, "/r/admin/roles/administration/default",
		map[string]any{"name": "reviewers"}), http.StatusCreated)
	if _, err := env.pool.Exec(context.Background(),
		`INSERT INTO public.auth_core__project_role (project_id, name) VALUES (7, 'reviewers')`); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, adminDo(t, router, http.MethodDelete, "/r/admin/roles/administration/default",
		map[string]any{"name": "reviewers"}), http.StatusOK)
	events := accessEvents(t, env.pool)
	if len(events) != 2 {
		t.Fatalf("events = %+v, want the create and the delete", events)
	}
	if deleted := events[1]; deleted.Action != "admin.access.role_deleted" ||
		strings.Join(deleted.Access.Fields, ",") != "project_role_copies" {
		t.Fatalf("deleted = %+v, want fields [project_role_copies]", deleted)
	}
	requireChainsVerify(t, env.pool, 0)
}
