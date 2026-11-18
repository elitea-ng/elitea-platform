package repos

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/delegationnotice"
	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

// TestPostgresDelegationNoticeFoldsOntoTheAnswer (#2161): each accepted
// `delegation_refused` or `delegation_narrowed` frame is folded onto the
// answer's meta.delegation_notices_v1 in the frame's own transaction, so a
// reload shows the notice; the answer's other meta survives, a replayed frame
// does not duplicate its notice, and what is not a notice is ignored.
func TestPostgresDelegationNoticeFoldsOntoTheAnswer(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentActivitySchemas(t, pool)

	const (
		conversationID   = "10000000-0000-4000-8000-000000002161"
		responseID       = "20000000-0000-4000-8000-000000002161"
		clientGeneration = "30000000-0000-4000-8000-000000002161"
	)
	admitted := admitPostgresAgentExecution(t, pool, conversationID, responseID, clientGeneration)
	seedCurrentAgentResponseGroup(t, pool, conversationID, responseID, clientGeneration, admitted.ExecutionID)
	groupID := currentResponseGroupID(t, pool, responseID)
	if _, err := pool.Exec(t.Context(), `
UPDATE p_1.chat_message_group SET meta = COALESCE(meta, '{}'::jsonb) || '{"grounding":{"version":1}}'::jsonb WHERE id = $1`, groupID); err != nil {
		t.Fatal(err)
	}
	store, err := newPostgresSharedStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	project := func(frameType, bindingMessage, notice string) error {
		frame := outputapp.NodeEventFrame{
			Fence: runtimedomain.Fence{ExecutionID: admitted.ExecutionID, Generation: 1},
			BrowserData: []byte(fmt.Sprintf(`{"type":%q,"stream_id":%q,"message_id":%q,
"execution_generation":%q,"sio_event":"chat_predict","content":null,"response_metadata":{"delegation_v1":%s}}`,
				frameType, conversationID, bindingMessage, clientGeneration, notice)),
		}
		return store.WithinTx(t.Context(), pgx.TxOptions{}, func(tx sqlExecutor) error {
			return projectDelegationNotice(t.Context(), tx, 1, frame)
		})
	}
	refused := `{"id":"call-1","kind":"refused","reason":"no_grant","code":"DELEGATION_GRANT_MISSING","secret":"hunter2",
"edge":{"caller_id":2,"caller_name":"Agent2","callee_id":1,"callee_name":"Agent1","kind":"agent_tool"},
"missing":[{"kind":"toolkit","name":"Git"}]}`
	narrowed := `{"id":"call-2","kind":"narrowed","reason":"stale","grant_id":5,"missing_total":1,
"edge":{"caller_id":2,"callee_id":3,"callee_name":"Agent3","kind":"pipeline_child"},
"missing":[{"kind":"toolkit","name":"Database"}]}`
	for _, step := range []struct{ frameType, notice string }{
		{delegationnotice.EventRefused, refused},
		{delegationnotice.EventNarrowed, narrowed},
		// A replay of the same tool call's notice replaces, never duplicates.
		{delegationnotice.EventRefused, refused},
		// What is not a notice is ignored, not refused.
		{delegationnotice.EventRefused, `{"id":"call-3"}`},
		{"agent_tool_end", refused},
	} {
		if err := project(step.frameType, responseID, step.notice); err != nil {
			t.Fatalf("%s: %v", step.notice, err)
		}
	}

	var meta []byte
	if err := pool.QueryRow(t.Context(), `
SELECT COALESCE(meta -> 'delegation_notices_v1', 'null'::jsonb) FROM p_1.chat_message_group WHERE id = $1`, groupID).Scan(&meta); err != nil {
		t.Fatal(err)
	}
	notices := delegationnotice.Decode(meta)
	if len(notices) != 2 || notices[0].ID != "call-1" || notices[1].ID != "call-2" {
		t.Fatalf("notices = %+v (%s)", notices, meta)
	}
	if notices[0].Reason != "no_grant" || notices[0].Missing[0].Name != "Git" || notices[1].Reason != "stale" || notices[1].GrantID != 5 {
		t.Fatalf("notices = %+v", notices)
	}
	if strings.Contains(string(meta), "hunter2") {
		t.Fatalf("an unknown key was stored: %s", meta)
	}
	var grounding bool
	if err := pool.QueryRow(t.Context(), `SELECT meta ? 'grounding' FROM p_1.chat_message_group WHERE id = $1`, groupID).Scan(&grounding); err != nil {
		t.Fatal(err)
	}
	if !grounding {
		t.Fatal("the notice write touched the answer's other meta")
	}

	// A frame bound to another answer is refused.
	if err := project(delegationnotice.EventRefused, "20000000-0000-4000-8000-00000000ffff", refused); err == nil {
		t.Fatal("a notice frame of another answer was accepted")
	}
}

// TestPostgresDelegationEditorsNameTheAgentsOwners (#2161): the people a viewer
// who may not review is told to ask are the agent's registry owners, else its
// owner, by display name (never an email), bounded.
func TestPostgresDelegationEditorsNameTheAgentsOwners(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedAuthCoreTables(t, pool)
	ctx := t.Context()
	// The application tables belong to the agent editor, not the ledgered schema.
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS p_1.applications (
    id SERIAL PRIMARY KEY, name VARCHAR(128) NOT NULL, description VARCHAR(2304), owner_id INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	var ada, grace, nameless int64
	for _, user := range []struct {
		email, name string
		id          *int64
	}{
		{"ada-2161@autotest.local", "Ada", &ada},
		{"grace-2161@autotest.local", "Grace", &grace},
		{"nameless-2161@autotest.local", "", &nameless},
	} {
		if err := pool.QueryRow(ctx, `INSERT INTO public.auth_core__user (email, name) VALUES ($1, $2) RETURNING id`,
			user.email, user.name).Scan(user.id); err != nil {
			t.Fatal(err)
		}
	}
	for _, app := range []struct {
		id    int64
		owner int64
	}{{216101, ada}, {216102, ada}, {216103, nameless}} {
		if _, err := pool.Exec(ctx, `INSERT INTO p_1.applications (id, name, description, owner_id) VALUES ($1, 'Fixture', 'fixture', $2)`,
			app.id, app.owner); err != nil {
			t.Fatal(err)
		}
	}
	// 216102 has registry owners: they win over the application's owner.
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_lifecycle.agent_registry (tenant_project_id, application_id, risk_tier, business_owner_id, technical_owner_id)
VALUES (1, 216102, 'medium', $1, $2)`, grace, ada); err != nil {
		t.Fatal(err)
	}
	editors, err := NewDelegationEditors(pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name  string
		app   int64
		limit int
		want  []string
	}{
		{"the application's owner when there is no registry row", 216101, 3, []string{"Ada"}},
		{"registry owners first, in order", 216102, 3, []string{"Grace", "Ada"}},
		{"bounded", 216102, 1, []string{"Grace"}},
		{"an owner with no name is not offered", 216103, 3, nil},
		{"an unknown agent has no one to ask", 999999, 3, nil},
	} {
		got, err := editors.EditorNames(ctx, 1, c.app, c.limit)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
