package repos

// Local hand-offs and consults (ADR-0061 decisions 3c and 3e, issues 3063
// and 3065) against a REAL database (ELITEA_TEST_DATABASE_URL): a live
// local turn's hand-off queues the agent, the handed turn is admitted only
// for it, a newer message supersedes it, a committed, foreign or cloud turn
// cannot hand off, a guard's stop writes its system message once, the skip
// route skips, a handed turn's commit stores the hand-off as side talk
// whatever its body says, and a consult is stored as two delegation side
// messages that count toward the next hand-off's hops. The shared guard
// fixture runs through both the cloud listener's repository and the local
// route here.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/localturn"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/turntaking"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

const localHandsUser int64 = 4532

// localHands is one seeded desktop thread with a user and agents.
type localHands struct {
	t            *testing.T
	pool         *pgxpool.Pool
	service      *localturn.Service
	hands        *localturn.Hands
	credential   localturn.Credential
	conversation int64
	uuid         string
	ids          map[string]int64
	next         int
}

func newLocalHands(t *testing.T, pool *pgxpool.Pool, agents ...[3]string) *localHands {
	t.Helper()
	ctx := context.Background()
	h := &localHands{t: t, pool: pool, ids: map[string]int64{},
		credential: localturn.Credential{TokenID: "78", NativeClientID: "ai.elitea.desktop"}}
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_conversations (uuid, name, author_id, source, created_at)
VALUES (gen_random_uuid(), 'team', $1, 'local_work', now() AT TIME ZONE 'UTC')
RETURNING id, uuid::text`, localHandsUser).Scan(&h.conversation, &h.uuid); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	h.seed("user", "user", fmt.Sprintf(`{"id": %d}`, localHandsUser), `{}`)
	for _, agent := range agents {
		h.seedAgent(agent[0], agent[1], agent[2])
	}
	repo := NewLocalTurnsRepo(pool)
	service, err := localturn.NewService(repo, &localTurnPolicy{allowed: true}, NewMemoriesRepo(pool), &localTurnAudit{},
		func() (string, error) {
			h.next++
			return fmt.Sprintf("%016x%016x", localHandsUser, uint64(h.next)+uint64(h.conversation)<<20), nil
		}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.service = service.WithFloor()
	if h.hands, err = localturn.NewHands(repo, &localTurnPolicy{allowed: true}); err != nil {
		t.Fatal(err)
	}
	return h
}

// seed maps one participant into the thread.
func (h *localHands) seed(key, entity, meta, settings string) int64 {
	h.t.Helper()
	ctx := context.Background()
	var id int64
	if err := h.pool.QueryRow(ctx, `
INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta, meta)
VALUES (gen_random_uuid(), $1, $2::jsonb, '{}'::json) RETURNING id`, entity, meta).Scan(&id); err != nil {
		h.t.Fatalf("seed participant: %v", err)
	}
	if _, err := h.pool.Exec(ctx, `
INSERT INTO p_1.chat_participant_mapping (conversation_id, participant_id, entity_settings, created_at)
VALUES ($1, $2, $3::jsonb, now() AT TIME ZONE 'UTC')`, h.conversation, id, settings); err != nil {
		h.t.Fatalf("map participant: %v", err)
	}
	h.ids[key] = id
	return id
}

// seedAgent maps an application agent, local or cloud, into the thread.
func (h *localHands) seedAgent(key, name, executor string) int64 {
	index := len(h.ids)
	settings := fmt.Sprintf(`{"version_id": %d}`, 60+index)
	if executor == "cloud" {
		settings = fmt.Sprintf(`{"version_id": %d, "executor": "cloud"}`, 60+index)
	}
	return h.seed(key, "application", fmt.Sprintf(`{"id": %d, "project_id": 1, "name": %q}`, 49+index, name), settings)
}

func (h *localHands) start(participant int64, addressed []int64, floor *localturn.FloorTurnRef) (localturn.StartOutcome, error) {
	return h.service.Start(context.Background(), localturn.StartRequest{
		ProjectID: 1, ActorUserID: localHandsUser, TokenID: h.credential.TokenID, NativeClientID: h.credential.NativeClientID,
		ConversationUUID: h.uuid, QuestionID: uuid.NewString(), UserInput: "Fix the build",
		ParticipantID: participant, AddressedTo: addressed, FloorTurn: floor,
	})
}

func (h *localHands) mustStart(participant int64, addressed []int64, floor *localturn.FloorTurnRef) localturn.StartOutcome {
	h.t.Helper()
	started, err := h.start(participant, addressed, floor)
	if err != nil {
		h.t.Fatalf("start %d: %v", participant, err)
	}
	return started
}

func (h *localHands) hand(execution string, to []int64, consults ...turntaking.Turn) (localturn.HandResult, error) {
	return h.hands.Hand(context.Background(), localturn.HandRequest{
		ProjectID: 1, ActorUserID: localHandsUser, Credential: h.credential, ExecutionID: execution,
		ParticipantIDs: to, Message: "over to you", Consults: consults,
	})
}

func (h *localHands) commit(execution, user string, consults ...localturn.Consult) error {
	_, err := h.service.Commit(context.Background(), localturn.CommitRequest{
		ProjectID: 1, ActorUserID: localHandsUser, Credential: h.credential, ExecutionID: execution,
		UserMessage: user, AssistantMessage: "done", Consults: consults,
	})
	return err
}

func (h *localHands) mustCommit(execution string, consults ...localturn.Consult) {
	h.t.Helper()
	if err := h.commit(execution, "Fix the build", consults...); err != nil {
		h.t.Fatalf("commit %s: %v", execution, err)
	}
}

func (h *localHands) queued(question string, to int64) string {
	h.t.Helper()
	var id string
	if err := h.pool.QueryRow(context.Background(), `
SELECT id::text FROM elitea_runtime.local_floor_hands
WHERE question_id = $1::uuid AND to_participant_id = $2 AND status = 'QUEUED' ORDER BY ord LIMIT 1`,
		question, to).Scan(&id); err != nil {
		h.t.Fatalf("queued hand-off to %d: %v", to, err)
	}
	return id
}

func (h *localHands) statuses(question string) []string {
	h.t.Helper()
	rows, err := h.pool.Query(context.Background(), `
SELECT status || COALESCE(':' || skip_code, '') FROM elitea_runtime.local_floor_hands
WHERE question_id = $1::uuid ORDER BY ord`, question)
	if err != nil {
		h.t.Fatal(err)
	}
	states, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		h.t.Fatal(err)
	}
	return states
}

func (h *localHands) systemMessages() int {
	h.t.Helper()
	var count int
	if err := h.pool.QueryRow(context.Background(), `
SELECT count(*) FROM p_1.chat_message_group message
JOIN p_1.chat_participants author ON author.id = message.author_participant_id
WHERE message.conversation_id = $1 AND author.entity_name = 'system' AND author.entity_meta ->> 'kind' = 'turn_taking'`,
		h.conversation).Scan(&count); err != nil {
		h.t.Fatal(err)
	}
	return count
}

func TestPostgresLocalFloorHandsQueueAdmitAndRefuse(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	h := newLocalHands(t, pool, [3]string{"mira", "Mira", "local"}, [3]string{"doctor", "CI Doctor", "local"},
		[3]string{"cloudy", "Cloudy", "cloud"})
	mira, doctor, cloudy := h.ids["mira"], h.ids["doctor"], h.ids["cloudy"]

	first := h.mustStart(mira, []int64{mira}, nil)
	if first.Floor == nil || len(first.Floor.Agents) != 3 || first.Floor.Agents[0].ApplicationID != 50 {
		t.Fatalf("first floor = %+v", first.Floor)
	}
	question := first.QuestionID
	handed, err := h.hand(first.ExecutionID, []int64{doctor})
	if err != nil || len(handed.HandedTo) != 1 || handed.HandedTo[0].ParticipantID != doctor ||
		handed.HandedTo[0].Name != "CI Doctor" || !localturn.ValidUUID(handed.HandedTo[0].HandID) || handed.Stopped != "" {
		t.Fatalf("hand = %+v, %v", handed, err)
	}
	handID := handed.HandedTo[0].HandID
	ref := &localturn.FloorTurnRef{QuestionID: question, HandID: handID}

	// Not before the handing turn is committed.
	if _, err := h.start(doctor, nil, ref); !errors.Is(err, localturn.ErrFloorTurn) {
		t.Fatalf("start before the commit = %v, want ErrFloorTurn", err)
	}
	h.mustCommit(first.ExecutionID)
	// A committed turn cannot hand off any more.
	if _, err := h.hand(first.ExecutionID, []int64{doctor}); !errors.Is(err, localturn.ErrNotLive) {
		t.Fatalf("a committed turn's hand = %v, want ErrNotLive", err)
	}
	// Another user's or another credential's execution is not found.
	if _, err := h.hands.Hand(ctx, localturn.HandRequest{ProjectID: 1, ActorUserID: localHandsUser + 1, Credential: h.credential,
		ExecutionID: first.ExecutionID, ParticipantIDs: []int64{doctor}, Message: "x"}); !errors.Is(err, localturn.ErrNotFound) {
		t.Fatalf("a stranger's hand = %v, want ErrNotFound", err)
	}
	if _, err := h.hands.Hand(ctx, localturn.HandRequest{ProjectID: 1, ActorUserID: localHandsUser,
		Credential:  localturn.Credential{TokenID: "other", NativeClientID: h.credential.NativeClientID},
		ExecutionID: first.ExecutionID, ParticipantIDs: []int64{doctor}, Message: "x"}); !errors.Is(err, localturn.ErrNotFound) {
		t.Fatalf("another credential's hand = %v, want ErrNotFound", err)
	}
	// Another agent cannot take the hand-off.
	if _, err := h.start(cloudy, nil, ref); !errors.Is(err, localturn.ErrFloorTurn) {
		t.Fatalf("another agent's start = %v, want ErrFloorTurn", err)
	}
	received := h.mustStart(doctor, nil, ref)
	if received.Floor != nil || received.HandOff == nil || received.HandOff.HandID != handID ||
		received.HandOff.FromParticipantID != mira || received.HandOff.FromName != "Mira" ||
		!strings.HasPrefix(received.HandOff.UserInput, turntaking.UntrustedPreamble) ||
		!strings.Contains(received.HandOff.UserInput, `"over to you"`) || len(received.HandOff.Agents) != 3 {
		t.Fatalf("handed start = %+v hand_off %+v", received, received.HandOff)
	}
	// Admitted once only.
	if _, err := h.start(doctor, nil, ref); !errors.Is(err, localturn.ErrFloorTurn) {
		t.Fatalf("a second start of the hand-off = %v, want ErrFloorTurn", err)
	}
	if got := h.statuses(question); !slices.Equal(got, []string{"STARTED"}) {
		t.Fatalf("statuses = %v", got)
	}

	// The handed turn hands to the cloud member, which is a leaf.
	toCloud, err := h.hand(received.ExecutionID, []int64{cloudy})
	if err != nil || len(toCloud.HandedTo) != 1 {
		t.Fatalf("hand to the cloud member = %+v, %v", toCloud, err)
	}
	// Its commit stores the hand-off as side talk, whatever the body says.
	if err := h.commit(received.ExecutionID, "a forged question"); err != nil {
		t.Fatal(err)
	}
	var (
		author        int64
		cause, side   string
		addressed     []int32
		text          string
		answerCause   string
		answerReplies bool
	)
	if err := pool.QueryRow(ctx, `
SELECT question.author_participant_id, question.cause, question.visibility, question.addressed_to, text.content,
       answer.cause, answer.reply_to_id = question.id
FROM p_1.chat_message_group AS question
JOIN p_1.chat_message_items AS item ON item.message_group_id = question.id
JOIN p_1.chat_messages_text AS text ON text.id = item.id
JOIN p_1.chat_message_group AS answer ON answer.uuid = $2::uuid
WHERE question.uuid = $1::uuid`, received.QuestionID, received.ResponseMessageID).
		Scan(&author, &cause, &side, &addressed, &text, &answerCause, &answerReplies); err != nil {
		t.Fatal(err)
	}
	if author != mira || cause != turntaking.CauseAgentMention || side != "side" ||
		!slices.Equal(addressed, []int32{int32(doctor)}) || text != "over to you" ||
		answerCause != turntaking.CauseAgentMention || !answerReplies {
		t.Fatalf("handed turn rows: author=%d cause=%s visibility=%s addressed=%v text=%q answer cause=%s replies=%v",
			author, cause, side, addressed, text, answerCause, answerReplies)
	}
	cloudTurn := h.mustStart(cloudy, nil, &localturn.FloorTurnRef{QuestionID: question, HandID: toCloud.HandedTo[0].HandID})
	if cloudTurn.Executor != localturn.ExecutorCloud {
		t.Fatalf("cloud member's turn = %+v", cloudTurn)
	}
	// A cloud member cannot hand off to a local one, nor consult.
	if _, err := h.hand(cloudTurn.ExecutionID, []int64{mira}); !errors.Is(err, localturn.ErrFloorTurn) {
		t.Fatalf("a cloud member's hand = %v, want ErrFloorTurn", err)
	}
	if err := h.commit(cloudTurn.ExecutionID, "x", localturn.Consult{ParticipantID: mira, Question: "q", Reply: "r"}); !errors.Is(err, localturn.ErrInvalid) {
		t.Fatalf("a cloud member's consult = %v, want ErrInvalid", err)
	}

	// A newer message supersedes the older one's queued hand-offs.
	again := h.mustStart(mira, []int64{mira}, nil)
	queuedAgain, err := h.hand(again.ExecutionID, []int64{doctor})
	if err != nil || len(queuedAgain.HandedTo) != 1 {
		t.Fatal(queuedAgain, err)
	}
	h.mustCommit(again.ExecutionID)
	newer := h.mustStart(doctor, []int64{doctor}, nil)
	if _, err := h.start(doctor, nil, &localturn.FloorTurnRef{QuestionID: again.QuestionID, HandID: queuedAgain.HandedTo[0].HandID}); !errors.Is(err, localturn.ErrFloorTurn) && !errors.Is(err, localturn.ErrFloorSuperseded) {
		t.Fatalf("a superseded hand-off's start = %v", err)
	}
	if got := h.statuses(again.QuestionID); !slices.Equal(got, []string{"SKIPPED:superseded"}) {
		t.Fatalf("superseded statuses = %v", got)
	}
	// And a live turn of the older message hands off no more.
	older := h.mustStart(mira, []int64{mira}, nil)
	_ = newer
	evenNewer := h.mustStart(doctor, []int64{doctor}, nil)
	_ = evenNewer
	if _, err := h.hand(older.ExecutionID, []int64{doctor}); !errors.Is(err, localturn.ErrFloorSuperseded) {
		t.Fatalf("an older message's hand = %v, want ErrFloorSuperseded", err)
	}
}

func TestPostgresLocalFloorHandsStopSkipAndConsults(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	h := newLocalHands(t, pool, [3]string{"mira", "Mira", "local"}, [3]string{"doctor", "CI Doctor", "local"},
		[3]string{"writer", "Writer", "local"})
	mira, doctor, writer := h.ids["mira"], h.ids["doctor"], h.ids["writer"]

	// Skip: the person skips one queued hand-off; the other stays.
	first := h.mustStart(mira, []int64{mira, doctor}, nil)
	handed, err := h.hand(first.ExecutionID, []int64{writer, doctor})
	if err != nil || len(handed.HandedTo) != 2 {
		t.Fatal(handed, err)
	}
	skipped, err := h.hands.Skip(ctx, 1, localHandsUser, first.QuestionID, []string{handed.HandedTo[0].HandID},
		localturn.SkipStoppedByPerson)
	if err != nil || !slices.Equal(skipped, []string{handed.HandedTo[0].HandID}) {
		t.Fatalf("skip = %v, %v", skipped, err)
	}
	if _, err := h.hands.Skip(ctx, 1, localHandsUser+1, first.QuestionID, []string{handed.HandedTo[1].HandID},
		localturn.SkipSkippedByPerson); !errors.Is(err, localturn.ErrNotFound) {
		t.Fatalf("a stranger's skip = %v", err)
	}
	if got := h.statuses(first.QuestionID); !slices.Equal(got, []string{"SKIPPED:stopped_by_person", "QUEUED"}) {
		t.Fatalf("statuses = %v", got)
	}
	// Doctor already had the routed turn: a second hand-off to it is its
	// second turn, a third passes the guard: the stop is written once and
	// the queued hand-off is skipped with the guard's code.
	stopped, err := h.hand(first.ExecutionID, []int64{doctor})
	if err != nil || stopped.Stopped != turntaking.StopAgentTurns || len(stopped.HandedTo) != 0 ||
		!strings.HasSuffix(stopped.Message, "Answer the person yourself.") {
		t.Fatalf("stop = %+v, %v", stopped, err)
	}
	if again, err := h.hand(first.ExecutionID, []int64{writer}); err != nil || again.Stopped != turntaking.StopAgentTurns ||
		len(again.HandedTo) != 0 {
		t.Fatalf("after the stop = %+v, %v", again, err)
	}
	if got := h.statuses(first.QuestionID); !slices.Equal(got, []string{"SKIPPED:stopped_by_person", "SKIPPED:consecutive_turns"}) {
		t.Fatalf("statuses after the stop = %v", got)
	}
	if n := h.systemMessages(); n != 1 {
		t.Fatalf("system messages = %d, want 1", n)
	}

	// Consults: stored as delegation side messages before the answer.
	// A consult of no local agent of the thread (outside it, the agent
	// itself, a cloud member) is left out; the answer is still stored.
	cloudy := h.seedAgent("cloudy", "Cloudy", "cloud")
	stray := h.mustStart(mira, []int64{mira}, nil)
	h.mustCommit(stray.ExecutionID,
		localturn.Consult{ParticipantID: 999999, Question: "q", Reply: "r"},
		localturn.Consult{ParticipantID: mira, Question: "q", Reply: "r"},
		localturn.Consult{ParticipantID: cloudy, Question: "q", Reply: "r"})
	var strays int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM p_1.chat_message_group WHERE conversation_id = $1 AND cause = 'delegation'`,
		h.conversation).Scan(&strays); err != nil || strays != 0 {
		t.Fatalf("stray consult rows = %d, %v", strays, err)
	}
	second := h.mustStart(mira, []int64{mira}, nil)
	h.mustCommit(second.ExecutionID,
		localturn.Consult{ParticipantID: doctor, Question: "Is lint-web green?", Reply: "Yes."},
		localturn.Consult{ParticipantID: writer, Question: "Draft the note", Reply: "Done."})
	rows, err := pool.Query(ctx, `
SELECT author_participant_id, cause, visibility, addressed_to[1], reply_to_id IS NOT NULL
FROM p_1.chat_message_group WHERE conversation_id = $1 AND cause = 'delegation' ORDER BY created_at, id`, h.conversation)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		author    int64
		cause     string
		side      string
		addressed int64
		reply     bool
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.author, &x.cause, &x.side, &x.addressed, &x.reply)
		return x, err
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []row{
		{mira, "delegation", "side", doctor, false}, {doctor, "delegation", "side", mira, true},
		{mira, "delegation", "side", writer, false}, {writer, "delegation", "side", mira, true},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("consult rows = %+v, want %+v", got, want)
	}
	var beforeAnswer bool
	if err := pool.QueryRow(ctx, `
SELECT max(consult.created_at) < (SELECT created_at FROM p_1.chat_message_group WHERE uuid = $2::uuid)
FROM p_1.chat_message_group AS consult WHERE consult.conversation_id = $1 AND consult.cause = 'delegation'`,
		h.conversation, second.ResponseMessageID).Scan(&beforeAnswer); err != nil || !beforeAnswer {
		t.Fatalf("consults before the answer = %v, %v", beforeAnswer, err)
	}
}

// TestPostgresHandCasesFixtureCloudAndLocal runs the shared fixture through
// the cloud listener's repository (HandFloorInExchange with the
// listener's own decision) and through handLocalFloor: every case gives the
// same handed agents and stop.
func TestPostgresHandCasesFixtureCloudAndLocal(t *testing.T) {
	raw, err := os.ReadFile("../../../domain/turntaking/testdata/hand_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name      string                   `json:"name"`
			First     int                      `json:"first"`
			Routed    []int                    `json:"routed"`
			LocalOnly bool                     `json:"local_only"`
			Consults  []struct{ From, To int } `json:"consults"`
			Steps     []struct {
				From int
				To   []int
			} `json:"steps"`
			Want struct {
				HandedTo []int  `json:"handed_to"`
				Stopped  string `json:"stopped"`
				Limit    int    `json:"limit"`
			} `json:"want"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	type answer struct {
		handed  []int
		stopped string
		message string
	}
	check := func(t *testing.T, leg string, got answer, handed []int, stopped string, limit int) {
		t.Helper()
		if got.handed == nil {
			got.handed = []int{}
		}
		if !slices.Equal(got.handed, handed) || got.stopped != stopped ||
			(limit > 0 && !strings.Contains(got.message, strconv.Itoa(limit))) {
			t.Fatalf("%s: %+v, want handed %v stopped %q limit %d", leg, got, handed, stopped, limit)
		}
	}
	pool := newMigratedPostgresIntegrationPool(t)
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			// The local route.
			h := newLocalHands(t, pool, [3]string{"1", "Ada", "local"}, [3]string{"2", "Bo", "local"}, [3]string{"3", "Cy", "local"})
			id := func(n int) int64 {
				if v, ok := h.ids[strconv.Itoa(n)]; ok {
					return v
				}
				return 999999
			}
			index := map[int64]int{}
			for n := 1; n <= 3; n++ {
				index[id(n)] = n
			}
			addressed := []int64{id(c.First)}
			for _, r := range c.Routed {
				addressed = append(addressed, id(r))
			}
			live := h.mustStart(id(c.First), addressed, nil)
			question, liveAgent := live.QuestionID, c.First
			var consults []turntaking.Turn
			for _, consult := range c.Consults {
				consults = append(consults, turntaking.Turn{From: id(consult.From), To: id(consult.To)})
			}
			var local answer
			for index2, step := range c.Steps {
				if step.From != liveAgent {
					h.mustCommit(live.ExecutionID)
					live = h.mustStart(id(step.From), nil, &localturn.FloorTurnRef{QuestionID: question, HandID: h.queued(question, id(step.From))})
					liveAgent = step.From
				}
				targets := make([]int64, 0, len(step.To))
				for _, to := range step.To {
					targets = append(targets, id(to))
				}
				var stepConsults []turntaking.Turn
				if index2 == len(c.Steps)-1 {
					stepConsults = consults
				}
				result, err := h.hand(live.ExecutionID, targets, stepConsults...)
				if err != nil {
					t.Fatalf("local step %d: %v", index2, err)
				}
				local = answer{stopped: result.Stopped, message: result.Message}
				for _, agent := range result.HandedTo {
					local.handed = append(local.handed, index[agent.ParticipantID])
				}
			}
			check(t, "local", local, c.Want.HandedTo, c.Want.Stopped, c.Want.Limit)
			if c.LocalOnly {
				return
			}

			// The cloud listener's repository.
			f := newFloorHarness(t)
			// The harness's first agent has no name; the fixture's is Ada.
			f.exec(`UPDATE p_1.chat_participants SET meta = json_build_object('name', 'Ada') WHERE id = $1`, f.agents[0])
			cloudID := func(n int) int64 {
				if n >= 1 && n <= 3 {
					return f.agents[n-1]
				}
				return 999999
			}
			cloudIndex := map[int64]int{f.agents[0]: 1, f.agents[1]: 2, f.agents[2]: 3}
			cloudQuestion := uuid.NewString()
			var routed []int64
			for _, r := range c.Routed {
				routed = append(routed, cloudID(r))
			}
			f.admitExchange(cloudQuestion, cloudID(c.First), routed...)
			run := storage.HandFloorRun{ProjectID: 1, ConversationUUID: f.conversation, ParticipantID: cloudID(c.First),
				ExecutionID: f.runOf(cloudID(c.First), cloudQuestion)}
			var cloud answer
			for index2, step := range c.Steps {
				if cloudID(step.From) != run.ParticipantID {
					var position int
					var turnQuestion string
					if err := f.pool.QueryRow(t.Context(), `
SELECT position, turn_question_id::text FROM elitea_runtime.agent_floor_turns
WHERE question_id = $1::uuid AND to_participant_id = $2 AND state = 'QUEUED' ORDER BY position LIMIT 1`,
						cloudQuestion, cloudID(step.From)).Scan(&position, &turnQuestion); err != nil {
						t.Fatalf("cloud step %d: the queued turn: %v", index2, err)
					}
					run = storage.HandFloorRun{ProjectID: 1, ConversationUUID: f.conversation, ParticipantID: cloudID(step.From),
						ExecutionID: f.runOf(cloudID(step.From), turnQuestion)}
					f.exec(`UPDATE elitea_runtime.agent_floor_turns SET state = 'STARTED', execution_id = $1
WHERE question_id = $2::uuid AND position = $3`, run.ExecutionID, cloudQuestion, position)
				}
				targets := make([]int64, 0, len(step.To))
				for _, to := range step.To {
					targets = append(targets, cloudID(to))
				}
				var result storage.RuntimeHandFloorResult
				if err := f.floors.HandFloorInExchange(t.Context(), run, func(view storage.FloorExchangeView) (storage.FloorHandWrite, error) {
					var write storage.FloorHandWrite
					result, write = storage.DecideHandFloor(view, run.ParticipantID, targets, "over to you")
					return write, nil
				}); err != nil {
					t.Fatalf("cloud step %d: %v", index2, err)
				}
				cloud = answer{stopped: result.Stopped, message: result.Message}
				for _, agent := range result.HandedTo {
					cloud.handed = append(cloud.handed, cloudIndex[agent.ParticipantID])
				}
			}
			check(t, "cloud", cloud, c.Want.HandedTo, c.Want.Stopped, c.Want.Limit)
			if cloud.message != local.message {
				t.Fatalf("the model reads %q in the cloud and %q locally", cloud.message, local.message)
			}
		})
	}
}

// Parallel hand_floor calls of one turn (a model's parallel tool calls)
// serialise on the calling turn's row instead of deadlocking.
func TestPostgresLocalFloorHandsParallelCallsDoNotDeadlock(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	h := newLocalHands(t, pool, [3]string{"mira", "Mira", "local"}, [3]string{"doctor", "CI Doctor", "local"},
		[3]string{"writer", "Writer", "local"})
	first := h.mustStart(h.ids["mira"], []int64{h.ids["mira"]}, nil)
	const calls = 6
	errs := make(chan error, calls)
	for index := range calls {
		to := h.ids["doctor"]
		if index%2 == 1 {
			to = h.ids["writer"]
		}
		go func() {
			_, err := h.hand(first.ExecutionID, []int64{to})
			errs <- err
		}()
	}
	for range calls {
		if err := <-errs; err != nil {
			t.Fatalf("a parallel hand: %v", err)
		}
	}
	// Whatever order they ran in, some agent's third turn came before the
	// sixth call (each takes at most 2): the guard stopped the exchange
	// once and skipped everything it had queued.
	states := h.statuses(first.QuestionID)
	if len(states) < 2 || len(states) > 4 || h.systemMessages() != 1 {
		t.Fatalf("statuses = %v, system messages %d", states, h.systemMessages())
	}
	for _, state := range states {
		if state != "SKIPPED:consecutive_turns" {
			t.Fatalf("statuses = %v", states)
		}
	}
}
