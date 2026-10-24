package repos

// Issue 1234: rewind, fork_id replay, the handoff fence and the size bound,
// through the real handler and repository (the fixture of
// conversation_fork_postgres_integration_test.go).
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacestore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

func (f *forkFixture) forkCount(t *testing.T) int {
	t.Helper()
	var forks int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM p_1.chat_conversations WHERE meta->'forked_from'->>'conversation_uuid' = $1`,
		f.sourceUUID).Scan(&forks); err != nil {
		t.Fatal(err)
	}
	return forks
}

func TestRewindStoresABranchSummaryOfTheAbandonedTurnsAndTheCut(t *testing.T) {
	f := newForkFixture(t)
	row := f.fork(t, "7", `{"message_id": "`+f.groups[1]+`", "mode": "rewind"}`)
	if got := strings.Join(forkTranscript(t, f.pool, rowID(t, row)), ","); got != "q1,a1" {
		t.Fatalf("the rewound conversation's transcript is %q, want q1,a1", got)
	}
	if got := strings.Join(forkTranscript(t, f.pool, mustInt(t, f.sourceID)), ","); got != "q1,a1,q2,a2" {
		t.Fatalf("the source changed: %q", got)
	}
	cut := forkedMessages(t, row)[f.groups[1]]
	ff, _ := row["forked_from"].(map[string]any)
	if ff["mode"] != "rewind" || ff["cut_message_uuid"] != cut || cut == "" || ff["branch_summary"] != nil {
		t.Fatalf("the fork's row forked_from = %v (cut %q)", ff, cut)
	}
	// The detail, which a desktop seeds its session from, carries the summary.
	uuid, _ := row["uuid"].(string)
	code, detail, raw := f.do(t, http.MethodGet, "7", "/conversation/1/"+uuid, "")
	if code != http.StatusOK {
		t.Fatalf("the detail answered %d: %s", code, raw)
	}
	ff, _ = detail["forked_from"].(map[string]any)
	summary, _ := ff["branch_summary"].(map[string]any)
	facts, _ := summary["key_facts"].([]any)
	if summary["version"] != float64(1) || len(facts) != 2 || facts[0] != "Asked: q2" || facts[1] != "Answered: a2" {
		t.Fatalf("branch_summary = %v", ff["branch_summary"])
	}
}

func TestRewindKeepsTheClientsBranchSummaryAndRefusesAnInvalidOne(t *testing.T) {
	f := newForkFixture(t)
	summary := `{"version": 1, "objective": "Try a cache.", "constraints": [], "decisions": [], "key_facts": ["It was slow."],
		"completed_work": [], "open_work": [], "next_steps": [], "unresolved_issues": [], "references": []}`
	for name, body := range map[string]string{
		"another version":           `{"message_id": "` + f.groups[1] + `", "mode": "rewind", "branch_summary": {"version": 2}}`,
		"a summary on a plain fork": `{"message_id": "` + f.groups[1] + `", "branch_summary": ` + summary + `}`,
		"a rewind without a point":  `{"mode": "rewind"}`,
		"an unknown mode":           `{"message_id": "` + f.groups[1] + `", "mode": "branch"}`,
	} {
		if code, _, raw := f.do(t, http.MethodPost, "7", "/fork/1/"+f.sourceUUID, body); code != http.StatusBadRequest {
			t.Fatalf("%s answered %d: %s", name, code, raw)
		}
	}
	if forks := f.forkCount(t); forks != 0 {
		t.Fatalf("%d forks written by refused requests", forks)
	}
	row := f.fork(t, "7", `{"message_id": "`+f.groups[1]+`", "mode": "rewind", "branch_summary": `+summary+`}`)
	uuid, _ := row["uuid"].(string)
	_, detail, _ := f.do(t, http.MethodGet, "7", "/conversation/1/"+uuid, "")
	ff, _ := detail["forked_from"].(map[string]any)
	if got, _ := ff["branch_summary"].(map[string]any); got["objective"] != "Try a cache." {
		t.Fatalf("branch_summary = %v", ff["branch_summary"])
	}
}

// forkAudited is f.fork under the audit middleware's slot: the answer, and
// whether the handler annotated the request as a fork.
func (f *forkFixture) forkAudited(t *testing.T, body string) (map[string]any, bool) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/fork/1/"+f.sourceUUID, strings.NewReader(body))
	ctx, slot := audit.ContextWithAnnotationSlot(auth.ContextWithUser(request.Context(), auth.User{UserID: "7"}))
	recorder := httptest.NewRecorder()
	f.router.ServeHTTP(recorder, request.WithContext(ctx))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("fork answered %d: %s", recorder.Code, recorder.Body.String())
	}
	row := map[string]any{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &row); err != nil {
		t.Fatal(err)
	}
	annotation, present := slot.Read()
	return row, present && annotation.Action == "conversation.fork"
}

func TestTheSameForkIDAnswersTheSameFork(t *testing.T) {
	f := newForkFixture(t)
	body := `{"message_id": "` + f.groups[1] + `", "fork_id": "6f9619ff-8b86-d011-b42d-00c04fc964ff"}`
	first, firstAudited := f.forkAudited(t, body)
	again, againAudited := f.forkAudited(t, body)
	if first["uuid"] != again["uuid"] || rowID(t, first) != rowID(t, again) {
		t.Fatalf("a replay made another fork: %v then %v", first["uuid"], again["uuid"])
	}
	if forks := f.forkCount(t); forks != 1 {
		t.Fatalf("%d forks, want 1", forks)
	}
	// Issue 1420: the replay answers the same message map, and is not
	// audited as a second fork.
	if made, replayed := forkedMessages(t, first), forkedMessages(t, again); len(made) != 2 || !maps.Equal(made, replayed) {
		t.Fatalf("forked_messages = %v, then on the replay %v", made, replayed)
	}
	if !firstAudited || againAudited {
		t.Fatalf("audited as a fork: first %v, replay %v", firstAudited, againAudited)
	}
	var indexed bool
	if err := f.pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM pg_indexes
		WHERE schemaname = 'p_1' AND indexname = 'chat_conversations_fork_id_idx')`).Scan(&indexed); err != nil || !indexed {
		t.Fatalf("the fork_id index (tenant/0155) is missing (%v)", err)
	}
	// The key names that fork only: another fork point is a conflict.
	code, decoded, raw := f.do(t, http.MethodPost, "7", "/fork/1/"+f.sourceUUID,
		`{"message_id": "`+f.groups[3]+`", "fork_id": "6f9619ff-8b86-d011-b42d-00c04fc964ff"}`)
	if code != http.StatusConflict || decoded["error"] != "fork_id_conflict" {
		t.Fatalf("a reused fork_id answered %d: %s", code, raw)
	}
	if forks := f.forkCount(t); forks != 1 {
		t.Fatalf("%d forks after the conflict, want 1", forks)
	}
}

func TestAThreadTheCloudExecutorOwnsIsNotForked(t *testing.T) {
	f := newForkFixture(t)
	if _, err := f.pool.Exec(context.Background(), `UPDATE p_1.chat_conversations SET source = 'local_work' WHERE id = $1::int`, f.sourceID); err != nil {
		t.Fatal(err)
	}
	thread := workspacestore.Thread{ProjectID: 1, ConversationUUID: f.sourceUUID}
	seedHandoffRow(t, f.pool, thread, 7, workspacestore.StateCloudIdle, 1, false, "")
	code, decoded, raw := f.do(t, http.MethodPost, "7", "/fork/1/"+f.sourceUUID, `{"message_id": "`+f.groups[1]+`", "mode": "rewind"}`)
	if code != http.StatusConflict || decoded["error"] != "thread_executor_is_cloud" {
		t.Fatalf("a fork of a handed-off thread answered %d: %s", code, raw)
	}
	if forks := f.forkCount(t); forks != 0 {
		t.Fatalf("%d forks of a handed-off thread", forks)
	}
	// Taken back, it forks.
	seedHandoffRow(t, f.pool, thread, 7, workspacestore.StateLocal, 2, false, "")
	f.fork(t, "7", `{"message_id": "`+f.groups[1]+`"}`)
}

func TestAForkOfTooManyMessagesIsRefused(t *testing.T) {
	f := newForkFixture(t)
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO p_1.chat_message_group (uuid, author_participant_id, conversation_id, created_at)
		SELECT gen_random_uuid(), g.author_participant_id, g.conversation_id, g.created_at - interval '1 day'
		FROM p_1.chat_message_group g, generate_series(1, $2::int)
		WHERE g.uuid = $1::uuid`, f.groups[0], maxForkGroups); err != nil {
		t.Fatal(err)
	}
	code, decoded, raw := f.do(t, http.MethodPost, "7", "/fork/1/"+f.sourceUUID, `{"message_id": "`+f.groups[1]+`"}`)
	if code != http.StatusRequestEntityTooLarge || decoded["error"] != "fork_too_large" {
		t.Fatalf("a fork of %d+ groups answered %d: %s", maxForkGroups, code, raw)
	}
}
