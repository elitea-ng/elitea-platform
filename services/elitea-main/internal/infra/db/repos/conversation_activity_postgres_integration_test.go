package repos

// Client contract 1.12: the conversation activity status of the list rows,
// the per-user read marker (markConversationRead), the turn-only sync bumps
// of tenant/0153 and the activity frames' audience, on the fully migrated
// tenant.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/conversationactivity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/chatactivity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// activityChat is a conversation of user 7 with user 8 and an agent as
// participants.
type activityChat struct {
	pool                *pgxpool.Pool
	id                  int64
	uuid                string
	user7, user8, agent int64
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func newActivityChat(t *testing.T, pool *pgxpool.Pool, private bool) *activityChat {
	t.Helper()
	ctx := context.Background()
	c := &activityChat{pool: pool}
	if err := pool.QueryRow(ctx, `INSERT INTO p_1.chat_conversations (uuid, name, author_id, is_private, source, created_at)
		VALUES (gen_random_uuid(), 'activity', 7, $1, 'elitea', now() AT TIME ZONE 'UTC') RETURNING id, uuid::text`, private).
		Scan(&c.id, &c.uuid); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	participant := func(name, meta string) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx, `WITH p AS (
			INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta) VALUES (gen_random_uuid(), $1, $2::jsonb) RETURNING id)
			INSERT INTO p_1.chat_participant_mapping (conversation_id, participant_id, created_at)
			SELECT $3, p.id, now() AT TIME ZONE 'UTC' FROM p RETURNING participant_id`, name, meta, c.id).Scan(&id); err != nil {
			t.Fatalf("seed participant %s: %v", meta, err)
		}
		return id
	}
	c.user7 = participant("user", `{"id": 7}`)
	c.user8 = participant("user", `{"id": 8}`)
	c.agent = participant("application", `{"id": 41, "project_id": 1}`)
	return c
}

// message adds a message group by author and answers its id.
func (c *activityChat) message(t *testing.T, author int64, streaming bool) int64 {
	t.Helper()
	var id int64
	if err := c.pool.QueryRow(context.Background(), `INSERT INTO p_1.chat_message_group
		(uuid, author_participant_id, conversation_id, is_streaming, created_at)
		VALUES (gen_random_uuid(), $1, $2, $3, clock_timestamp() AT TIME ZONE 'UTC') RETURNING id`,
		author, c.id, streaming).Scan(&id); err != nil {
		t.Fatalf("seed message group: %v", err)
	}
	return id
}

func (c *activityChat) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := c.pool.Exec(context.Background(), query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func (c *activityChat) activity(t *testing.T, reader int64) chatactivity.Activity {
	t.Helper()
	got, err := chatactivity.Load(context.Background(), c.pool, `"p_1"`, reader, []int64{c.id})
	if err != nil {
		t.Fatal(err)
	}
	a, ok := got[c.id]
	if !ok {
		t.Fatalf("conversation %d has no activity", c.id)
	}
	return a
}

func (c *activityChat) syncAt(t *testing.T) time.Time {
	t.Helper()
	var at time.Time
	if err := c.pool.QueryRow(context.Background(), `SELECT sync_at FROM p_1.chat_conversations WHERE id = $1`, c.id).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

func TestConversationActivityFollowsTheTurn(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	grantPinMembership(t, pool, 1, 7, 8)
	c := newActivityChat(t, pool, false)

	if a := c.activity(t, 7); a.State != chatactivity.StateIdle || a.Unread {
		t.Fatalf("a new conversation = %+v, want idle and read", a)
	}

	// A question of user 7 is not an answer: it is unread for nobody.
	c.message(t, c.user7, false)
	for _, reader := range []int64{7, 8} {
		if a := c.activity(t, reader); a.State != chatactivity.StateIdle || a.Unread {
			t.Fatalf("after the question, reader %d = %+v, want idle and read", reader, a)
		}
	}

	// The answer streams: running, and not unread while it streams.
	answer := c.message(t, c.agent, true)
	running := c.activity(t, 8)
	if running.State != chatactivity.StateRunning || running.Unread {
		t.Fatalf("a streaming answer = %+v, want running and read", running)
	}

	// The answer pauses for a tool approval: needs_you, and unread.
	c.exec(t, `UPDATE p_1.chat_message_group SET is_streaming = FALSE,
		meta = '{"hitl_interrupt": {"tool": "x"}}'::jsonb, updated_at = clock_timestamp() AT TIME ZONE 'UTC' WHERE id = $1`, answer)
	paused := c.activity(t, 8)
	if paused.State != chatactivity.StateNeedsYou || !paused.Unread || !paused.Since.After(running.Since) {
		t.Fatalf("a paused answer = %+v (running since %v), want needs_you, unread, a later since", paused, running.Since)
	}
	pausedSet, err := chatactivity.Paused(context.Background(), pool, `"p_1"`, []int64{c.id})
	if err != nil || !pausedSet[c.id] {
		t.Fatalf("Paused = %v, %v; want the conversation", pausedSet, err)
	}

	// The user answers and the turn finishes: idle, and unread for both
	// users (the author of the turn did not write the answer).
	c.exec(t, `UPDATE p_1.chat_message_group SET meta = '{}'::jsonb, is_streaming = TRUE,
		updated_at = clock_timestamp() AT TIME ZONE 'UTC' WHERE id = $1`, answer)
	if a := c.activity(t, 7); a.State != chatactivity.StateRunning {
		t.Fatalf("a resumed answer = %+v, want running", a)
	}
	c.exec(t, `UPDATE p_1.chat_message_group SET is_streaming = FALSE,
		updated_at = clock_timestamp() AT TIME ZONE 'UTC' WHERE id = $1`, answer)
	for _, reader := range []int64{7, 8} {
		a := c.activity(t, reader)
		if a.State != chatactivity.StateIdle || !a.Unread || a.LastActivityAt.Before(paused.Since) {
			t.Fatalf("a finished turn, reader %d = %+v, want idle and unread", reader, a)
		}
	}
	pausedSet, err = chatactivity.Paused(context.Background(), pool, `"p_1"`, []int64{c.id})
	if err != nil || pausedSet[c.id] {
		t.Fatalf("Paused after the turn = %v, %v; want empty", pausedSet, err)
	}

	// Reader 0 (the frame snapshot) never reads unread.
	if a := c.activity(t, 0); a.Unread {
		t.Fatalf("reader 0 = %+v, want read", a)
	}
}

func TestConversationReadMarkerIsPerUserMonotonicAndIdempotent(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	grantPinMembership(t, pool, 1, 7, 8)
	repo := NewConversationsRepo(pool)
	c := newActivityChat(t, pool, false)
	question := c.message(t, c.user7, false)
	answer := c.message(t, c.agent, false)
	id := itoa(c.id)

	// Read up to the question: the answer is still unread.
	state, err := repo.MarkConversationRead(asUser("8"), "1", id, itoa(question))
	if err != nil || !state.Unread || state.LastReadMessageID == nil || *state.LastReadMessageID != question {
		t.Fatalf("read up to the question = %+v, %v; want unread, marker at %d", state, err, question)
	}
	// Read up to now, by the UUID of the conversation.
	state, err = repo.MarkConversationRead(asUser("8"), "1", c.uuid, "")
	if err != nil || state.Unread || state.LastReadMessageID == nil || *state.LastReadMessageID != answer {
		t.Fatalf("read up to now = %+v, %v; want read, marker at %d", state, err, answer)
	}
	first := state.ReadAt
	// Idempotent, and never back: a repeat and an older message keep it.
	again, err := repo.MarkConversationRead(asUser("8"), "1", id, itoa(question))
	if err != nil || again.Unread || again.ReadAt != first || *again.LastReadMessageID != answer {
		t.Fatalf("an older read = %+v, %v; want the marker of %s at %d", again, err, first, answer)
	}
	var rows int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM p_1.chat_conversation_reads WHERE conversation_id = $1`, c.id).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("marker rows = %d, %v; want 1", rows, err)
	}

	// The marker is per user: user 7 still has the answer unread.
	if a := c.activity(t, 7); !a.Unread {
		t.Fatalf("user 7 = %+v, want unread", a)
	}
	if a := c.activity(t, 8); a.Unread {
		t.Fatalf("user 8 = %+v, want read", a)
	}

	// A read does not stamp sync_at: it is no change for the other members.
	before := c.syncAt(t)
	if _, err := repo.MarkConversationRead(asUser("7"), "1", id, ""); err != nil {
		t.Fatal(err)
	}
	if after := c.syncAt(t); !after.Equal(before) {
		t.Fatalf("a read moved sync_at from %v to %v", before, after)
	}

	// A new answer after the marker is unread again.
	c.message(t, c.agent, false)
	if a := c.activity(t, 8); !a.Unread {
		t.Fatalf("a new answer after the read = %+v, want unread", a)
	}
}

func TestConversationReadMarkerAnswers404(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	grantPinMembership(t, pool, 1, 7, 8, 9)
	repo := NewConversationsRepo(pool)
	private := newActivityChat(t, pool, true)
	other := newActivityChat(t, pool, false)
	foreign := other.message(t, other.agent, false)

	notFound := func(name string, err error) {
		t.Helper()
		var apiErr *apierr.APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
			t.Fatalf("%s: err = %v, want 404", name, err)
		}
	}
	// User 9 is a member, but not a participant of the private chat.
	_, err := repo.MarkConversationRead(asUser("9"), "1", itoa(private.id), "")
	notFound("an invisible conversation", err)
	_, err = repo.MarkConversationRead(asUser("8"), "1", "999999", "")
	notFound("an absent conversation", err)
	_, err = repo.MarkConversationRead(asUser("8"), "1", itoa(private.id), itoa(foreign))
	notFound("a message of another conversation", err)
	_, err = repo.MarkConversationRead(asUser("8"), "1", itoa(private.id), "not-a-message")
	notFound("a message id that does not parse", err)

	var rows int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM p_1.chat_conversation_reads`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("a refused read wrote %d marker rows (%v)", rows, err)
	}
}

func TestConversationSyncBumpsOnlyOnTurnTransitions(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	c := newActivityChat(t, pool, false)
	c.message(t, c.user7, false)

	// Each step starts a minute after the last bump, past 0144's one-second
	// throttle, so only the trigger's own rule decides whether it bumps.
	past := func() { backdate(t, pool, "p_1.chat_conversations", c.id, time.Minute) }

	// A turn starts: the INSERT bumps.
	past()
	before := c.syncAt(t)
	answer := c.message(t, c.agent, true)
	started := c.syncAt(t)
	if !started.After(before) {
		t.Fatalf("a turn start did not bump sync_at (%v, then %v)", before, started)
	}

	// Tokens: text items and group writes while the group streams never
	// bump the conversation.
	var item int64
	if err := pool.QueryRow(context.Background(), `INSERT INTO p_1.chat_message_items (uuid, item_type, order_index, message_group_id, created_at)
		VALUES (gen_random_uuid(), 'text_message', 0, $1, now() AT TIME ZONE 'UTC') RETURNING id`, answer).Scan(&item); err != nil {
		t.Fatal(err)
	}
	c.exec(t, `INSERT INTO p_1.chat_messages_text (id, content) VALUES ($1, '')`, item)
	past()
	afterItem := c.syncAt(t)
	for token := range 5 {
		c.exec(t, `UPDATE p_1.chat_messages_text SET content = content || $2 WHERE id = $1`, item, "tok"+itoa(int64(token)))
		c.exec(t, `UPDATE p_1.chat_message_group SET updated_at = clock_timestamp() AT TIME ZONE 'UTC',
			meta = jsonb_set(COALESCE(meta, '{}'), '{tokens}', to_jsonb($2::int)) WHERE id = $1`, answer, token)
	}
	if streamed := c.syncAt(t); !streamed.Equal(afterItem) {
		t.Fatalf("streamed tokens bumped sync_at from %v to %v", afterItem, streamed)
	}

	// A pause bumps, and so does the end of the pause.
	c.exec(t, `UPDATE p_1.chat_message_group SET is_streaming = FALSE, meta = '{"hitl_interrupt": {}}'::jsonb WHERE id = $1`, answer)
	pausedAt := c.syncAt(t)
	if !pausedAt.After(afterItem) {
		t.Fatalf("a pause did not bump sync_at")
	}
	past()
	pausedAt = c.syncAt(t)
	c.exec(t, `UPDATE p_1.chat_message_group SET meta = '{}'::jsonb WHERE id = $1`, answer)
	if resolved := c.syncAt(t); !resolved.After(pausedAt) {
		t.Fatalf("the end of a pause did not bump sync_at")
	}
}

// A group moved to another conversation bumps both, even when the same
// UPDATE also settles the turn (the transition branch must not shadow the
// move).
func TestConversationSyncBumpsBothSidesOfAMove(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	from := newActivityChat(t, pool, false)
	to := newActivityChat(t, pool, false)
	answer := from.message(t, from.agent, true)
	backdate(t, pool, "p_1.chat_conversations", from.id, time.Minute)
	backdate(t, pool, "p_1.chat_conversations", to.id, time.Minute)
	fromBefore, toBefore := from.syncAt(t), to.syncAt(t)

	from.exec(t, `UPDATE p_1.chat_message_group SET conversation_id = $2, is_streaming = FALSE WHERE id = $1`, answer, to.id)
	if got := from.syncAt(t); !got.After(fromBefore) {
		t.Fatalf("the conversation the group left was not bumped (%v, then %v)", fromBefore, got)
	}
	if got := to.syncAt(t); !got.After(toBefore) {
		t.Fatalf("the conversation the group joined was not bumped (%v, then %v)", toBefore, got)
	}
}

func TestConversationReadMarkersCascadeAndForksStartRead(t *testing.T) {
	f := newForkFixture(t)
	ctx := context.Background()
	// The source's newest group (a2) is an answer of a user participant in
	// this fixture; make it an agent answer so the source is unread.
	if _, err := f.pool.Exec(ctx, `WITH agent AS (SELECT p.id FROM p_1.chat_participants p JOIN p_1.chat_participant_mapping m ON m.participant_id = p.id
		WHERE m.conversation_id = $1::int AND p.entity_name = 'application')
		UPDATE p_1.chat_message_group SET author_participant_id = (SELECT id FROM agent) WHERE uuid = $2::uuid`, f.sourceID, f.groups[3]); err != nil {
		t.Fatal(err)
	}
	source, err := strconv.ParseInt(f.sourceID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	before, err := chatactivity.Load(ctx, f.pool, `"p_1"`, 7, []int64{source})
	if err != nil || !before[source].Unread {
		t.Fatalf("the source = %+v, %v; want unread for user 7", before[source], err)
	}

	row := f.fork(t, "7", "")
	forkID := rowID(t, row)
	if row["unread"] != false || row["activity"] == nil {
		t.Fatalf("the fork row = %v, want read with an activity", row)
	}
	got, err := chatactivity.Load(ctx, f.pool, `"p_1"`, 7, []int64{forkID})
	if err != nil || got[forkID].Unread {
		t.Fatalf("the fork for its forker = %+v, %v; want read", got[forkID], err)
	}

	if _, err := f.repo.MarkConversationRead(asUser("7"), "1", f.sourceID, ""); err != nil {
		t.Fatal(err)
	}
	code, _, raw := f.do(t, "DELETE", "7", "/conversation/1/"+f.sourceID, "")
	if code != http.StatusOK && code != http.StatusNoContent {
		t.Fatalf("delete answered %d: %s", code, raw)
	}
	var left int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM p_1.chat_conversation_reads WHERE conversation_id = $1`, source).Scan(&left); err != nil || left != 0 {
		t.Fatalf("the delete left %d marker rows (%v)", left, err)
	}
}

// activityBus records the published frames.
type activityBus struct {
	mu     sync.Mutex
	frames []publishedFrame
}

type publishedFrame struct {
	project  int64
	audience conversationactivity.Audience
	frame    conversationactivity.Frame
}

func (b *activityBus) Publish(_ context.Context, project int64, audience conversationactivity.Audience, frame conversationactivity.Frame) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.frames = append(b.frames, publishedFrame{project, audience, frame})
	return nil
}

func (b *activityBus) take(t *testing.T) publishedFrame {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.frames) != 1 {
		t.Fatalf("published %d frames, want 1: %+v", len(b.frames), b.frames)
	}
	frame := b.frames[0]
	b.frames = nil
	return frame
}

func TestConversationActivityFramesReachTheMembersWhoCanSeeIt(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	grantPinMembership(t, pool, 1, 7, 8, 9, 10)
	// User 10 is a project admin: a private conversation with a user
	// participant is visible to admins in the listing.
	if _, err := pool.Exec(context.Background(), `WITH role AS (
		INSERT INTO public.auth_core__project_role (project_id, name) VALUES (1, 'admin')
		ON CONFLICT (project_id, name) DO UPDATE SET name = EXCLUDED.name RETURNING id)
		INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id) SELECT 1, 10, id FROM role`); err != nil {
		t.Fatal(err)
	}
	bus := &activityBus{}
	notifier := conversationactivity.New(NewConversationActivityRepo(pool), bus, nil, conversationactivity.Synchronous())

	public := newActivityChat(t, pool, false)
	public.message(t, public.user7, false)
	answer := public.message(t, public.agent, true)
	notifier.ConversationChanged(context.Background(), 1, public.uuid)
	got := bus.take(t)
	if got.project != 1 || !got.audience.Project || len(got.audience.Users) != 0 ||
		got.frame.State != chatactivity.StateRunning || got.frame.UnreadHint != nil || got.frame.ConversationID != public.id {
		t.Fatalf("the running frame = %+v, want the whole project, running, no unread hint", got)
	}

	var answerUUID string
	if err := pool.QueryRow(context.Background(), `UPDATE p_1.chat_message_group SET is_streaming = FALSE WHERE id = $1 RETURNING uuid::text`, answer).Scan(&answerUUID); err != nil {
		t.Fatal(err)
	}
	notifier.MessageChanged(context.Background(), 1, answerUUID)
	got = bus.take(t)
	if got.frame.State != chatactivity.StateIdle || got.frame.UnreadHint == nil || !*got.frame.UnreadHint || got.frame.LastActivityAt == "" {
		t.Fatalf("the finished frame = %+v, want idle with unread_hint", got)
	}

	// A private conversation reaches its user participants and the admins
	// only, never the whole project.
	private := newActivityChat(t, pool, true)
	private.message(t, private.agent, true)
	notifier.ConversationChanged(context.Background(), 1, private.uuid)
	got = bus.take(t)
	users := slices.Clone(got.audience.Users)
	slices.Sort(users)
	if got.audience.Project || !slices.Equal(users, []int64{7, 8, 10}) {
		t.Fatalf("the private frame's audience = %+v, want users 7, 8 and 10", got.audience)
	}

	// A hidden conversation sends nothing.
	private.exec(t, `UPDATE p_1.chat_conversations SET meta = '{"is_hidden": true}'::jsonb WHERE id = $1`, private.id)
	notifier.ConversationChanged(context.Background(), 1, private.uuid)
	bus.mu.Lock()
	sent := len(bus.frames)
	bus.mu.Unlock()
	if sent != 0 {
		t.Fatalf("a hidden conversation published %d frames", sent)
	}

	// A read reaches the reader's own devices only.
	notifier.Read(context.Background(), 1, 8, public.id, public.uuid, "2026-10-20T00:00:00Z")
	got = bus.take(t)
	if got.audience.Project || !slices.Equal(got.audience.Users, []int64{8}) || got.frame.UnreadHint == nil || *got.frame.UnreadHint {
		t.Fatalf("the read frame = %+v, want user 8 only with unread_hint false", got)
	}
}

// readEventsSpy records the read frames of the read route.
type readEventsSpy struct {
	mu    sync.Mutex
	reads []string
}

func (s *readEventsSpy) Read(_ context.Context, projectID, readerID, conversationID int64, _ string, readAt string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads = append(s.reads, itoa(projectID)+"/"+itoa(readerID)+"/"+itoa(conversationID)+"@"+readAt)
}

func TestMarkConversationReadRoute(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	grantPinMembership(t, pool, 1, 7, 8, 9)
	c := newActivityChat(t, pool, true)
	answer := c.message(t, c.agent, false)
	spy := &readEventsSpy{}
	handler := conversations.NewHandler(NewConversationsRepo(pool)).WithPool(pool).WithReadEvents(spy)
	router := chi.NewRouter()
	router.Put("/{projectID}/{conversationID}", handler.MarkRead)
	call := func(caller, conversation, body string) (int, map[string]any) {
		t.Helper()
		request := httptest.NewRequest(http.MethodPut, "/1/"+conversation, strings.NewReader(body))
		request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{UserID: caller}))
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		decoded := map[string]any{}
		_ = json.Unmarshal(recorder.Body.Bytes(), &decoded)
		return recorder.Code, decoded
	}

	code, body := call("8", c.uuid, `{"message_id": "`+itoa(answer)+`"}`)
	if code != http.StatusOK || body["unread"] != false || body["last_read_message_id"] != float64(answer) ||
		body["conversation_id"] != float64(c.id) || body["read_at"] == "" {
		t.Fatalf("PUT answered %d %v", code, body)
	}
	// No body: up to now. A numeric message id is accepted too.
	if code, body = call("8", itoa(c.id), ""); code != http.StatusOK {
		t.Fatalf("PUT without a body answered %d %v", code, body)
	}
	if code, _ = call("8", itoa(c.id), `{"message_id": `+itoa(answer)+`}`); code != http.StatusOK {
		t.Fatalf("PUT with a numeric message_id answered %d", code)
	}
	if code, _ = call("8", itoa(c.id), `{"message_id": true}`); code != http.StatusBadRequest {
		t.Fatalf("PUT with a boolean message_id answered %d, want 400", code)
	}
	if code, _ = call("9", itoa(c.id), ""); code != http.StatusNotFound {
		t.Fatalf("PUT by a member who cannot see it answered %d, want 404", code)
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.reads) != 3 {
		t.Fatalf("read frames = %v, want one per accepted read", spy.reads)
	}
	for _, read := range spy.reads {
		if !strings.HasPrefix(read, "1/8/"+itoa(c.id)+"@") {
			t.Fatalf("read frame %q, want project 1, reader 8", read)
		}
	}
}
