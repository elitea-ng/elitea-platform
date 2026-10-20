package repos

// Client contract 1.11: forkConversation, through the real handler and
// repository.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

// forkObjects is an in-memory object store: Get, Put and DeleteBatch only.
type forkObjects struct {
	storage.ObjectStore
	mu      sync.Mutex
	objects map[string][]byte
}

func (s *forkObjects) Put(_ context.Context, ref storage.ObjectRef, body io.Reader, _ storage.PutOptions) (storage.ObjectInfo, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[ref.Bucket()+"/"+ref.Key()] = data
	return storage.ObjectInfo{Key: ref.Key(), Size: int64(len(data))}, nil
}

func (s *forkObjects) Get(_ context.Context, ref storage.ObjectRef, _ *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[ref.Bucket()+"/"+ref.Key()]
	if !ok {
		return nil, storage.ObjectInfo{}, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), storage.ObjectInfo{Key: ref.Key(), Size: int64(len(data)), TotalSize: int64(len(data))}, nil
}

func (s *forkObjects) DeleteBatch(_ context.Context, refs []storage.ObjectRef) (storage.BatchResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ref := range refs {
		delete(s.objects, ref.Bucket()+"/"+ref.Key())
	}
	return storage.BatchResult{}, nil
}

// forkAttachmentRows is the attachment metadata: one bucket, rows by key.
type forkAttachmentRows struct {
	conversations.AttachmentStore
	mu   sync.Mutex
	rows map[string]conversations.AttachmentObjectInfo
}

const forkBucket = "chat-attachments"

func (f *forkAttachmentRows) AttachmentPolicy(context.Context, int64) (string, *int64, *int32, error) {
	return forkBucket, nil, nil, nil
}

func (f *forkAttachmentRows) LookupAttachmentBucket(context.Context, int64, string) (int64, error) {
	return 1, nil
}

func (f *forkAttachmentRows) RecordAttachmentObject(_ context.Context, _ int64, key string, byteLength int64, mediaType string, _ *time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[key] = conversations.AttachmentObjectInfo{MediaType: mediaType, ByteLength: byteLength}
	return nil
}

func (f *forkAttachmentRows) DeleteAttachmentObjects(_ context.Context, _ int64, keys []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, key := range keys {
		delete(f.rows, key)
	}
	return nil
}

func (f *forkAttachmentRows) AttachmentObject(_ context.Context, _ int64, bucket, key string) (conversations.AttachmentObjectInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info, ok := f.rows[key]
	if !ok || bucket != forkBucket {
		return conversations.AttachmentObjectInfo{}, storage.ErrNotFound
	}
	return info, nil
}

type forkFixture struct {
	pool        *pgxpool.Pool
	repo        *ConversationsRepo
	objects     *forkObjects
	attachments *forkAttachmentRows
	router      chi.Router
	sourceID    string
	sourceUUID  string
	groups      []string // q1, a1, q2, a2
}

// newForkFixture seeds user 7's private conversation with two turns, an
// agent participant with its settings, user 8 as a second user participant,
// conversation settings, a pin, an archive, a trace step on the first answer
// and an attachment (with its bytes) on the first question.
func newForkFixture(t *testing.T) *forkFixture {
	t.Helper()
	pool := newMigratedPostgresIntegrationPool(t)
	grantPinMembership(t, pool, 1, 7, 8, 9)
	repo := NewConversationsRepo(pool)
	f := &forkFixture{pool: pool, repo: repo,
		objects:     &forkObjects{objects: map[string][]byte{}},
		attachments: &forkAttachmentRows{rows: map[string]conversations.AttachmentObjectInfo{}}}
	f.sourceID, f.sourceUUID, f.groups = seedConversationWithParticipant(t, repo, "q1", "a1", "q2", "a2")
	linkReply(t, repo, f.groups[1], f.groups[0])
	linkReply(t, repo, f.groups[3], f.groups[2])
	ctx := context.Background()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	exec(`UPDATE p_1.chat_conversations SET instructions = 'be brief',
		meta = '{"steps_limit": 9, "attachments": [{"name": "old"}], "forked_from": {"conversation_id": 999}}'::jsonb WHERE id = $1::int`, f.sourceID)
	exec(`WITH agent AS (
		INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta) VALUES (gen_random_uuid(), 'application', '{"id": 41, "project_id": 1}'::jsonb) RETURNING id)
		INSERT INTO p_1.chat_participant_mapping (conversation_id, participant_id, entity_settings, created_at)
		SELECT $1::int, agent.id, '{"version_id": 5, "llm_settings": {"temperature": 0.2}}'::jsonb, now() AT TIME ZONE 'UTC' FROM agent`, f.sourceID)
	exec(`WITH other AS (
		INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta) VALUES (gen_random_uuid(), 'user', '{"id": 8}'::jsonb) RETURNING id)
		INSERT INTO p_1.chat_participant_mapping (conversation_id, participant_id, created_at) SELECT $1::int, other.id, now() AT TIME ZONE 'UTC' FROM other`, f.sourceID)
	exec(`INSERT INTO p_1.chat_conversation_archives (conversation_id, archived_by, archived_at) VALUES ($1::int, 7, now() AT TIME ZONE 'UTC')`, f.sourceID)
	if err := NewCurrentSocialPinsRepository(pool).Pin(asUser("7"), "1", "conversation", f.sourceID); err != nil {
		t.Fatalf("pin the source: %v", err)
	}
	exec(`INSERT INTO p_1.chat_message_trace_step (message_group_id, kind, tool_name, text)
		SELECT id, 'tool', 'search', 'found it' FROM p_1.chat_message_group WHERE uuid = $1::uuid`, f.groups[1])
	// The participants read names users from auth_core__user; the membership
	// fixture's table has no name or email.
	exec(`ALTER TABLE public.auth_core__user ADD COLUMN IF NOT EXISTS name text, ADD COLUMN IF NOT EXISTS email text`)
	// An earlier group that still carries a stale pause key.
	exec(`UPDATE p_1.chat_message_group SET meta = '{"hitl_interrupt": {"stale": true}}'::jsonb WHERE uuid = $1::uuid`, f.groups[2])
	exec(`UPDATE p_1.chat_message_group SET meta = '{"is_error": false, "thread_id": "t1"}'::jsonb WHERE uuid = $1::uuid`, f.groups[1])
	name := f.sourceUUID + "/notes.txt"
	seedAttachmentItem(t, repo, f.groups[0], forkBucket, name)
	exec(`UPDATE p_1.chat_messages_attachment SET content = json_build_array(json_build_object('type', 'text', 'text', 'filepath: /chat-attachments/' || $1::text))
		WHERE name = $1`, name)
	f.objects.objects[forkBucket+"/"+name] = []byte("hello notes")
	f.attachments.rows[name] = conversations.AttachmentObjectInfo{MediaType: "text/plain", ByteLength: 11}

	handler := conversations.NewHandler(repo).WithPool(pool).WithObjectStore(f.objects).WithAttachmentStore(f.attachments)
	f.router = chi.NewRouter()
	f.router.Post("/fork/{projectID}/{conversationID}", handler.Fork)
	f.router.Get("/conversation/{projectID}/{conversationID}", handler.Get)
	f.router.Get("/list/{projectID}", handler.List)
	f.router.Delete("/conversation/{projectID}/{conversationID}", handler.Delete)
	f.router.Get("/attachments/{projectID}/{conversationID}/{name}", handler.DownloadAttachment)
	return f
}

func (f *forkFixture) do(t *testing.T, method, caller, path, body string) (int, map[string]any, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{UserID: caller}))
	recorder := httptest.NewRecorder()
	f.router.ServeHTTP(recorder, request)
	decoded := map[string]any{}
	_ = json.Unmarshal(recorder.Body.Bytes(), &decoded)
	return recorder.Code, decoded, recorder.Body.String()
}

func (f *forkFixture) fork(t *testing.T, caller, body string) map[string]any {
	t.Helper()
	code, row, raw := f.do(t, http.MethodPost, caller, "/fork/1/"+f.sourceUUID, body)
	if code != http.StatusCreated {
		t.Fatalf("fork answered %d: %s", code, raw)
	}
	return row
}

func rowID(t *testing.T, row map[string]any) int64 {
	t.Helper()
	id, ok := row["id"].(float64)
	if !ok {
		t.Fatalf("row %v has no numeric id", row)
	}
	return int64(id)
}

// forkTranscript is a conversation's message texts in transcript order.
func forkTranscript(t *testing.T, pool *pgxpool.Pool, conversationID int64) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT COALESCE(t.content, '')
		FROM p_1.chat_message_group g
		LEFT JOIN p_1.chat_message_items i ON i.message_group_id = g.id AND i.item_type = 'text_message'
		LEFT JOIN p_1.chat_messages_text t ON t.id = i.id
		WHERE g.conversation_id = $1 ORDER BY g.created_at, g.id`, conversationID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			t.Fatal(err)
		}
		out = append(out, text)
	}
	return out
}

func TestForkFromAMessageCopiesTheHistoryUpToAndIncludingIt(t *testing.T) {
	f := newForkFixture(t)
	ctx := context.Background()
	row := f.fork(t, "7", `{"message_id": "`+f.groups[1]+`"}`)
	forkID := rowID(t, row)

	if got := strings.Join(forkTranscript(t, f.pool, forkID), ","); got != "q1,a1" {
		t.Fatalf("the fork's transcript is %q, want q1,a1", got)
	}
	if got := strings.Join(forkTranscript(t, f.pool, mustInt(t, f.sourceID)), ","); got != "q1,a1,q2,a2" {
		t.Fatalf("the source changed: %q", got)
	}

	// The row: the listConversations shape, with the fork marker.
	if row["name"] != "delete-me (fork)" || row["source"] != "agent" || row["is_pinned"] != false || row["is_archived"] != false {
		t.Fatalf("row = %v", row)
	}
	if uuid, _ := row["uuid"].(string); uuid == "" || uuid == f.sourceUUID {
		t.Fatalf("row uuid = %v", row["uuid"])
	}
	ff, _ := row["forked_from"].(map[string]any)
	var answerID int64
	if err := f.pool.QueryRow(ctx, `SELECT id FROM p_1.chat_message_group WHERE uuid = $1::uuid`, f.groups[1]).Scan(&answerID); err != nil {
		t.Fatal(err)
	}
	if ff["conversation_id"] != float64(mustInt(t, f.sourceID)) || ff["conversation_uuid"] != f.sourceUUID ||
		ff["message_id"] != float64(answerID) || ff["message_uuid"] != f.groups[1] {
		t.Fatalf("forked_from = %v", ff)
	}

	// Ordering, created_at and the reply link are kept; the copies are new rows.
	var sameCreated, linked, newIDs bool
	if err := f.pool.QueryRow(ctx, `SELECT
		(SELECT array_agg(created_at ORDER BY created_at, id) FROM p_1.chat_message_group WHERE conversation_id = $1)
		  = (SELECT array_agg(created_at ORDER BY created_at, id) FROM p_1.chat_message_group WHERE conversation_id = $2::int
		       AND uuid = ANY($3::uuid[])),
		EXISTS (SELECT 1 FROM p_1.chat_message_group a JOIN p_1.chat_message_group q ON q.id = a.reply_to_id
		        WHERE a.conversation_id = $1 AND q.conversation_id = $1),
		NOT EXISTS (SELECT 1 FROM p_1.chat_message_group WHERE conversation_id = $1 AND uuid = ANY($3::uuid[]))`,
		forkID, f.sourceID, []string{f.groups[0], f.groups[1]}).Scan(&sameCreated, &linked, &newIDs); err != nil {
		t.Fatal(err)
	}
	if !sameCreated || !linked || !newIDs {
		t.Fatalf("created_at kept %v, reply link remapped %v, new uuids %v", sameCreated, linked, newIDs)
	}

	// The trace step is copied; the stale pause key is not.
	var traces int
	var pause, thread bool
	if err := f.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM p_1.chat_message_trace_step s JOIN p_1.chat_message_group g ON g.id = s.message_group_id
		 WHERE g.conversation_id = $1 AND s.tool_name = 'search'),
		EXISTS (SELECT 1 FROM p_1.chat_message_group WHERE conversation_id = $1 AND meta ? 'hitl_interrupt'),
		EXISTS (SELECT 1 FROM p_1.chat_message_group WHERE conversation_id = $1 AND meta->>'thread_id' = 't1')`, forkID).
		Scan(&traces, &pause, &thread); err != nil {
		t.Fatal(err)
	}
	if traces != 1 || pause || !thread {
		t.Fatalf("trace steps %d, pause kept %v, other meta kept %v", traces, pause, thread)
	}

	// Participants: the agent with its settings and the caller; not user 8.
	var settings []byte
	var users []string
	if err := f.pool.QueryRow(ctx, `SELECT m.entity_settings FROM p_1.chat_participant_mapping m
		JOIN p_1.chat_participants p ON p.id = m.participant_id WHERE m.conversation_id = $1 AND p.entity_name = 'application'`, forkID).
		Scan(&settings); err != nil {
		t.Fatalf("agent participant: %v", err)
	}
	if !strings.Contains(string(settings), `"version_id": 5`) || !strings.Contains(string(settings), `"temperature": 0.2`) {
		t.Fatalf("agent settings = %s", settings)
	}
	if err := f.pool.QueryRow(ctx, `SELECT array_agg(p.entity_meta->>'id' ORDER BY 1) FROM p_1.chat_participant_mapping m
		JOIN p_1.chat_participants p ON p.id = m.participant_id WHERE m.conversation_id = $1 AND p.entity_name = 'user'`, forkID).
		Scan(&users); err != nil {
		t.Fatal(err)
	}
	if strings.Join(users, ",") != "7" {
		t.Fatalf("user participants of the fork = %v, want only the caller", users)
	}

	// Conversation settings: copied; the upload listing and the old marker are not.
	var meta []byte
	var instructions string
	var private bool
	if err := f.pool.QueryRow(ctx, `SELECT meta, instructions, is_private FROM p_1.chat_conversations WHERE id = $1`, forkID).
		Scan(&meta, &instructions, &private); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(meta), `"steps_limit": 9`) || strings.Contains(string(meta), `"attachments"`) ||
		strings.Contains(string(meta), "999") || instructions != "be brief" || !private {
		t.Fatalf("fork meta %s, instructions %q, private %v", meta, instructions, private)
	}

	// Not copied: the pin and the archive.
	var pinned, archived bool
	if err := f.pool.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM centry.social_pins WHERE entity = 'conversation' AND project_id = 1 AND entity_id = $1),
		EXISTS (SELECT 1 FROM p_1.chat_conversation_archives WHERE conversation_id = $1)`, forkID).Scan(&pinned, &archived); err != nil {
		t.Fatal(err)
	}
	if pinned || archived {
		t.Fatalf("pin copied %v, archive copied %v", pinned, archived)
	}
}

func mustInt(t *testing.T, text string) int64 {
	t.Helper()
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestForkWithoutAMessageCopiesTheWholeConversation(t *testing.T) {
	f := newForkFixture(t)
	row := f.fork(t, "7", "")
	if got := strings.Join(forkTranscript(t, f.pool, rowID(t, row)), ","); got != "q1,a1,q2,a2" {
		t.Fatalf("whole fork transcript %q", got)
	}
	ff, _ := row["forked_from"].(map[string]any)
	if _, present := ff["message_id"]; present || ff["conversation_uuid"] != f.sourceUUID {
		t.Fatalf("forked_from = %v", ff)
	}
	// The stale pause key of q2 is not copied; its other content is.
	var pause bool
	if err := f.pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM p_1.chat_message_group
		WHERE conversation_id = $1 AND meta ? 'hitl_interrupt')`, rowID(t, row)).Scan(&pause); err != nil {
		t.Fatal(err)
	}
	if pause {
		t.Fatal("the fork copied a pending pause")
	}

	// A name of the caller's, by the numeric id.
	code, named, raw := f.do(t, http.MethodPost, "7", "/fork/1/"+f.sourceID, `{"name": "  my branch  ", "message_id": "`+f.groups[0]+`"}`)
	if code != http.StatusCreated || named["name"] != "my branch" {
		t.Fatalf("named fork answered %d: %s", code, raw)
	}
	for _, body := range []string{`{"name": ""}`, `{"name": "a\u0007b"}`, `{"message_id": 5}`, `[]`} {
		if code, _, raw := f.do(t, http.MethodPost, "7", "/fork/1/"+f.sourceID, body); code != http.StatusBadRequest {
			t.Errorf("body %s answered %d, want 400: %s", body, code, raw)
		}
	}
}

func TestForkOfAnInvisibleSourceOrAForeignMessageIs404(t *testing.T) {
	f := newForkFixture(t)
	// User 9 is a member but not a participant of the private source.
	if code, _, raw := f.do(t, http.MethodPost, "9", "/fork/1/"+f.sourceUUID, ""); code != http.StatusNotFound {
		t.Fatalf("an invisible source answered %d: %s", code, raw)
	}
	// User 8 is a participant, so it sees the source and may fork it.
	if code, _, raw := f.do(t, http.MethodPost, "8", "/fork/1/"+f.sourceUUID, ""); code != http.StatusCreated {
		t.Fatalf("a participant's fork answered %d: %s", code, raw)
	}
	_, _, otherGroups := seedConversationWithParticipant(t, f.repo, "elsewhere")
	for _, messageID := range []string{otherGroups[0], "999999", "not-an-id"} {
		if code, _, raw := f.do(t, http.MethodPost, "7", "/fork/1/"+f.sourceUUID, `{"message_id": "`+messageID+`"}`); code != http.StatusNotFound {
			t.Errorf("message %s answered %d: %s", messageID, code, raw)
		}
	}
	var forks int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM p_1.chat_conversations WHERE meta ? 'forked_from' AND author_id <> 7`).Scan(&forks); err != nil {
		t.Fatal(err)
	}
	if forks != 1 {
		t.Fatalf("%d forks by other users, want only user 8's", forks)
	}
}

func TestForkPointMustBeSettled(t *testing.T) {
	f := newForkFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE p_1.chat_message_group SET is_streaming = TRUE WHERE uuid = $1::uuid`, f.groups[3]); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"message_id": "` + f.groups[3] + `"}`, ""} {
		code, decoded, raw := f.do(t, http.MethodPost, "7", "/fork/1/"+f.sourceUUID, body)
		if code != http.StatusConflict || decoded["error"] != "fork_point_unsettled" {
			t.Fatalf("a streaming fork point (%s) answered %d: %s", body, code, raw)
		}
	}
	// An earlier settled message still forks.
	if code, _, raw := f.do(t, http.MethodPost, "7", "/fork/1/"+f.sourceUUID, `{"message_id": "`+f.groups[1]+`"}`); code != http.StatusCreated {
		t.Fatalf("a settled fork point answered %d: %s", code, raw)
	}
	// A pending approval is unsettled too.
	if _, err := f.pool.Exec(ctx, `UPDATE p_1.chat_message_group SET is_streaming = FALSE,
		meta = '{"authorization_requests": []}'::jsonb WHERE uuid = $1::uuid`, f.groups[3]); err != nil {
		t.Fatal(err)
	}
	if code, decoded, _ := f.do(t, http.MethodPost, "7", "/fork/1/"+f.sourceUUID, ""); code != http.StatusConflict || decoded["error"] != "fork_point_unsettled" {
		t.Fatalf("a paused fork point answered %d %v", code, decoded)
	}
	var forks int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM p_1.chat_conversations WHERE meta ? 'forked_from' AND name LIKE '%(fork)'`).Scan(&forks); err != nil {
		t.Fatal(err)
	}
	if forks != 1 {
		t.Fatalf("%d forks written, want only the settled one", forks)
	}
}

func TestForkOfAnEditorTestConversationIsRefused(t *testing.T) {
	f := newForkFixture(t)
	if _, err := f.pool.Exec(context.Background(), `UPDATE p_1.chat_conversations SET source = 'editor_test' WHERE id = $1::int`, f.sourceID); err != nil {
		t.Fatal(err)
	}
	if code, decoded, raw := f.do(t, http.MethodPost, "7", "/fork/1/"+f.sourceUUID, ""); code != http.StatusConflict || decoded["error"] != "fork_not_supported" {
		t.Fatalf("an editor Test fork answered %d: %s", code, raw)
	}
}

func TestForkOfALocalWorkThreadStaysALocalWorkThread(t *testing.T) {
	f := newForkFixture(t)
	if _, err := f.pool.Exec(context.Background(), `UPDATE p_1.chat_conversations SET source = 'local_work',
		meta = meta || '{"local_work": {"folder_name": "api"}}'::jsonb WHERE id = $1::int`, f.sourceID); err != nil {
		t.Fatal(err)
	}
	row := f.fork(t, "7", "")
	if row["source"] != conversations.LocalWorkSource {
		t.Fatalf("source = %v", row["source"])
	}
	meta, _ := row["meta"].(map[string]any)
	if folder, _ := meta["local_work"].(map[string]any); folder["folder_name"] != "api" {
		t.Fatalf("meta = %v", meta)
	}
	// It is listed with the Local work threads, not with the chats.
	_, listing, _ := f.do(t, http.MethodGet, "7", "/list/1?source=local_work&archived=include", "")
	found := false
	for _, raw := range listing["rows"].([]any) {
		if listed := raw.(map[string]any); listed["uuid"] == row["uuid"] {
			found = listed["forked_from"] != nil
		}
	}
	if !found {
		t.Fatalf("the fork is not in the Local work list with its forked_from: %v", listing)
	}
}

func TestForkKeepsAttachmentsReadableWhenTheSourceIsDeleted(t *testing.T) {
	f := newForkFixture(t)
	row := f.fork(t, "7", "")
	forkUUID := row["uuid"].(string)
	forkID := rowID(t, row)

	// The copy names the fork's own object, in the name and in the content.
	var name, content string
	if err := f.pool.QueryRow(context.Background(), `SELECT a.name, a.content::text FROM p_1.chat_messages_attachment a
		JOIN p_1.chat_message_items i ON i.id = a.id JOIN p_1.chat_message_group g ON g.id = i.message_group_id
		WHERE g.conversation_id = $1`, forkID).Scan(&name, &content); err != nil {
		t.Fatal(err)
	}
	if name != forkUUID+"/notes.txt" || !strings.Contains(content, forkUUID) || strings.Contains(content, f.sourceUUID) {
		t.Fatalf("copied attachment name %q content %s", name, content)
	}

	if code, _, raw := f.do(t, http.MethodDelete, "7", "/conversation/1/"+f.sourceUUID, ""); code != http.StatusOK && code != http.StatusNoContent {
		t.Fatalf("delete the source answered %d: %s", code, raw)
	}
	request := httptest.NewRequest(http.MethodGet, "/attachments/1/"+forkUUID+"/notes.txt", nil)
	request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{UserID: "7"}))
	recorder := httptest.NewRecorder()
	f.router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "hello notes" {
		t.Fatalf("the fork's attachment after the source delete: %d %q", recorder.Code, recorder.Body.String())
	}
	if _, gone := f.objects.objects[forkBucket+"/"+f.sourceUUID+"/notes.txt"]; gone {
		t.Fatal("the source delete kept the source's own object")
	}

	// The fork is intact, and its back-link has lost the name only.
	if got := strings.Join(forkTranscript(t, f.pool, forkID), ","); got != "q1,a1,q2,a2" {
		t.Fatalf("the fork's transcript after the source delete: %q", got)
	}
	_, detail, _ := f.do(t, http.MethodGet, "7", "/conversation/1/"+forkUUID, "")
	ff, _ := detail["forked_from"].(map[string]any)
	if _, named := ff["name"]; named || ff["conversation_uuid"] != f.sourceUUID {
		t.Fatalf("forked_from after the source delete = %v", ff)
	}
}

func TestForkedFromCarriesTheSourceNameOnlyForAReaderWhoSeesTheSource(t *testing.T) {
	f := newForkFixture(t)
	row := f.fork(t, "7", `{"message_id": "`+f.groups[0]+`"}`)
	forkUUID := row["uuid"].(string)

	_, detail, raw := f.do(t, http.MethodGet, "7", "/conversation/1/"+forkUUID, "")
	ff, _ := detail["forked_from"].(map[string]any)
	if ff["name"] != "delete-me" || ff["message_uuid"] != f.groups[0] {
		t.Fatalf("forked_from = %v (%s)", ff, raw)
	}

	// A rename keeps the server's marker, and a create cannot forge one.
	if _, err := f.repo.Update(asUser("7"), "1", forkUUID, conversations.Conversation{Meta: map[string]any{"steps_limit": 3}}); err != nil {
		t.Fatal(err)
	}
	_, detail, _ = f.do(t, http.MethodGet, "7", "/conversation/1/"+forkUUID, "")
	if detail["forked_from"] == nil {
		t.Fatalf("a meta write dropped forked_from: %v", detail)
	}

	// Make the fork public: user 9 then sees the fork, not the private source.
	if _, err := f.pool.Exec(context.Background(), `UPDATE p_1.chat_conversations SET is_private = FALSE WHERE uuid = $1::uuid`, forkUUID); err != nil {
		t.Fatal(err)
	}
	code, detail, raw := f.do(t, http.MethodGet, "9", "/conversation/1/"+forkUUID, "")
	if code != http.StatusOK {
		t.Fatalf("user 9 reading the public fork: %d %s", code, raw)
	}
	ff, _ = detail["forked_from"].(map[string]any)
	if _, named := ff["name"]; named || ff == nil {
		t.Fatalf("user 9 sees forked_from %v, want the ids without the name", ff)
	}
}

// A name without the source prefix (an older writer) is copied as it is, so
// both conversations name one object. A delete of either side reports it
// only when no other conversation still names it.
func TestADeleteKeepsAnObjectAnotherConversationStillNames(t *testing.T) {
	f := newForkFixture(t)
	ctx := context.Background()
	seedAttachmentItem(t, f.repo, f.groups[3], forkBucket, "legacy/shared.pdf")
	row := f.fork(t, "7", "")
	forkUUID := row["uuid"].(string)

	// A message delete in the source: the fork still names the object.
	result, err := f.repo.DeleteMessage(ctx, "1", f.groups[3], conversationAuthorID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range result.Attachments {
		if ref.Name == "legacy/shared.pdf" {
			t.Fatalf("the message delete reported the shared object: %v", result.Attachments)
		}
	}
	// The fork's delete: no other row names it now, so it is reported.
	refs, err := f.repo.Delete(ctx, "1", forkUUID)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(attachmentKeys(refs), ",")
	if !strings.Contains(got, forkBucket+"/legacy/shared.pdf") || !strings.Contains(got, forkBucket+"/"+forkUUID+"/notes.txt") {
		t.Fatalf("the fork's delete reported %v", got)
	}
}
