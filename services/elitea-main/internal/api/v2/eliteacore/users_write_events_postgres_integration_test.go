package eliteacore_test

// The project_membership events of the central member routes (ADR-0059 R1.5
// follow-up, issue 2994): an invite, a role change and a removal through
// /admin/users/administration/{projectID} each append one chained event per
// membership they change, in the write's transaction, with the facts R1.6
// rates it by. The project-gated `{mode}` routes append none, and a failed
// append fails the write.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

type membershipEvent struct {
	Partition, Subject, Action string
	Before, After              []string
	ActorIsMember, AdminRole   bool
	Bulk                       bool
}

func membershipEvents(t *testing.T, pool *pgxpool.Pool) []membershipEvent {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT partition_key, event FROM elitea_runtime.security_audit_outbox
WHERE event_type = $1 ORDER BY seq`, string(securityaudit.TypeAdminAccessChanged))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var events []membershipEvent
	for rows.Next() {
		var partition string
		var raw []byte
		if err := rows.Scan(&partition, &raw); err != nil {
			t.Fatal(err)
		}
		var envelope struct {
			Subject string `json:"subject"`
			Data    struct {
				Action string `json:"action"`
				Access struct {
					Before     []string `json:"before"`
					After      []string `json:"after"`
					Membership struct {
						ActorIsMember        bool `json:"actor_is_member"`
						MemberHoldsAdminRole bool `json:"member_holds_admin_role"`
						Bulk                 bool `json:"bulk"`
					} `json:"membership"`
				} `json:"access"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatal(err)
		}
		access := envelope.Data.Access
		events = append(events, membershipEvent{
			Partition: partition, Subject: envelope.Subject, Action: envelope.Data.Action,
			Before: access.Before, After: access.After,
			ActorIsMember: access.Membership.ActorIsMember, AdminRole: access.Membership.MemberHoldsAdminRole,
			Bulk: access.Membership.Bulk,
		})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

func accountID(t *testing.T, pool *pgxpool.Pool, email string) int {
	t.Helper()
	var id int
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM public.auth_core__user WHERE email = $1`, email).Scan(&id); err != nil {
		t.Fatalf("read the id of %s: %v", email, err)
	}
	return id
}

func TestTheCentralMemberRoutesAppendOneMembershipEventPerChange(t *testing.T) {
	pool := newUsersWritePostgresPool(t)
	prepareUsersWriteFixture(t, pool)
	router := usersWriteRouter(eliteacore.NewHandler(pool, eliteacore.WithPermissionResolver(superAdminCaller{})))
	central := fmt.Sprintf("/admin/users/administration/%d", usersWriteProjectID)
	const invited = "e2e-central-invited@autotest.local"
	const member = "e2e-member@autotest.local"

	// The project-gated route is a member's own administration: no event.
	if recorder := usersWriteDo(t, router, http.MethodPost, fmt.Sprintf("/admin/users/default/%d", usersWriteProjectID),
		map[string]any{"emails": []string{"e2e-default-route@autotest.local"}, "roles": []string{"viewer"}}); recorder.Code != http.StatusOK {
		t.Fatalf("default-mode invite = %d: %s", recorder.Code, recorder.Body.String())
	}
	if events := membershipEvents(t, pool); len(events) != 0 {
		t.Fatalf("the project-gated route appended %+v", events)
	}

	// Invite: one event, nothing before.
	if recorder := usersWriteDo(t, router, http.MethodPost, central,
		map[string]any{"emails": []string{invited}, "roles": []string{"editor"}}); recorder.Code != http.StatusOK {
		t.Fatalf("central invite = %d: %s", recorder.Code, recorder.Body.String())
	}
	events := membershipEvents(t, pool)
	invitedID := accountID(t, pool, invited)
	if len(events) != 1 {
		t.Fatalf("events after the invite = %+v, want one", events)
	}
	if e := events[0]; e.Action != "admin.access.project_membership" || e.Partition != strconv.Itoa(usersWriteProjectID) ||
		e.Subject != fmt.Sprintf("user/%d", invitedID) || len(e.Before) != 0 || strings.Join(e.After, ",") != "editor" ||
		!e.ActorIsMember || e.AdminRole || e.Bulk {
		t.Fatalf("invite event = %+v", e)
	}

	// Role change: one event, the old set before, the new after.
	memberID := accountID(t, pool, member)
	if recorder := usersWriteDo(t, router, http.MethodPut, central,
		map[string]any{"userId": strconv.Itoa(memberID), "roles": []string{"viewer"}}); recorder.Code != http.StatusOK {
		t.Fatalf("central role change = %d: %s", recorder.Code, recorder.Body.String())
	}
	// The same roles again change nothing and append nothing.
	if recorder := usersWriteDo(t, router, http.MethodPut, central,
		map[string]any{"userId": strconv.Itoa(memberID), "roles": []string{"viewer"}}); recorder.Code != http.StatusOK {
		t.Fatalf("central unchanged role change = %d: %s", recorder.Code, recorder.Body.String())
	}
	events = membershipEvents(t, pool)
	if len(events) != 2 {
		t.Fatalf("events after the role change = %+v, want two", events)
	}
	if e := events[1]; e.Subject != fmt.Sprintf("user/%d", memberID) || strings.Join(e.Before, ",") != "editor" ||
		strings.Join(e.After, ",") != "viewer" || e.Bulk {
		t.Fatalf("role change event = %+v", e)
	}

	// Removal of two members in one request: one event each, after empty,
	// bulk.
	if recorder := usersWriteDo(t, router, http.MethodDelete,
		fmt.Sprintf("%s?id[]=%d,%d", central, memberID, invitedID), nil); recorder.Code != http.StatusOK {
		t.Fatalf("central removal = %d: %s", recorder.Code, recorder.Body.String())
	}
	events = membershipEvents(t, pool)
	if len(events) != 4 {
		t.Fatalf("events after the removal = %+v, want four", events)
	}
	for _, e := range events[2:] {
		if len(e.After) != 0 || len(e.Before) != 1 || !e.Bulk {
			t.Fatalf("removal event = %+v, want one role before, none after, bulk", e)
		}
	}

	report, err := securityaudit.Verify(context.Background(), pool, securityaudit.SourceOutbox, int64(usersWriteProjectID), 1)
	if err != nil || report.Break != nil || report.Checked != 4 {
		t.Fatalf("verify = %+v, %v", report, err)
	}
}

// A failed append fails the central write: nothing is written.
func TestAFailedAppendFailsTheCentralMemberWrite(t *testing.T) {
	pool := newUsersWritePostgresPool(t)
	prepareUsersWriteFixture(t, pool)
	router := usersWriteRouter(eliteacore.NewHandler(pool, eliteacore.WithPermissionResolver(superAdminCaller{}),
		eliteacore.WithAccessAuditAppender(securityaudit.FailingAppender{Err: errors.New("injected append failure")})))
	central := fmt.Sprintf("/admin/users/administration/%d", usersWriteProjectID)
	const member = "e2e-member@autotest.local"
	memberID := accountID(t, pool, member)

	if recorder := usersWriteDo(t, router, http.MethodPost, central,
		map[string]any{"emails": []string{"e2e-failed@autotest.local"}, "roles": []string{"editor"}}); recorder.Code == http.StatusOK {
		t.Fatalf("an invite answered 200 with a failed append: %s", recorder.Body.String())
	}
	if recorder := usersWriteDo(t, router, http.MethodPut, central,
		map[string]any{"userId": strconv.Itoa(memberID), "roles": []string{"viewer"}}); recorder.Code != http.StatusInternalServerError {
		t.Fatalf("a role change with a failed append = %d, want 500", recorder.Code)
	}
	if recorder := usersWriteDo(t, router, http.MethodDelete,
		fmt.Sprintf("%s?id[]=%d", central, memberID), nil); recorder.Code != http.StatusInternalServerError {
		t.Fatalf("a removal with a failed append = %d, want 500", recorder.Code)
	}
	roles, found := memberRoles(readMembers(t, router), member)
	if !found || strings.Join(roles, ",") != "editor" {
		t.Fatalf("%s holds %v (member %v) after the failed writes, want [editor]", member, roles, found)
	}
	if roles, found := memberRoles(readMembers(t, router), "e2e-failed@autotest.local"); found {
		t.Fatalf("the failed invite left a membership %v", roles)
	}
}
