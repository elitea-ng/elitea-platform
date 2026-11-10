package repos

// Several agents in a desktop thread (ADR-0050 decision 7f, issue 1806)
// against a REAL database (ELITEA_TEST_DATABASE_URL): the local turn start
// routes a person's message by the conversation's floor, a later turn is
// admitted only at its place in the routed order and while its message is
// the newest, the later agent's answer replies to the person's question, and
// a cloud agent's answer is marked agent_executor cloud and stays executed_by desktop.

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/localturn"
)

func TestPostgresLocalTurnFloorRoutesAndSequencesAgents(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const user int64 = 4531

	var conversationID int
	var conversationUUID string
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_conversations (uuid, name, author_id, source, created_at)
VALUES (gen_random_uuid(), 'two agents', $1, 'local_work', now() AT TIME ZONE 'UTC')
RETURNING id, uuid::text`, user).Scan(&conversationID, &conversationUUID); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	ids := map[string]int64{}
	for _, participant := range []struct {
		key, entity, meta, settings string
	}{
		{"user", "user", `{"id": 4531}`, `{}`},
		{"coder", "application", `{"id": 5, "project_id": 1, "name": "Coder"}`, `{"version_id": 6}`},
		{"reviewer", "application", `{"id": 7, "project_id": 1, "name": "Reviewer"}`, `{"version_id": 8, "executor": "cloud"}`},
		{"dummy", "dummy", `{}`, `{}`},
	} {
		var id int64
		if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta, meta)
VALUES (gen_random_uuid(), $1, $2::jsonb, '{}'::json) RETURNING id`, participant.entity, participant.meta).Scan(&id); err != nil {
			t.Fatalf("seed participant: %v", err)
		}
		if _, err := pool.Exec(ctx, `
INSERT INTO p_1.chat_participant_mapping (conversation_id, participant_id, entity_settings, created_at)
VALUES ($1, $2, $3::jsonb, now() AT TIME ZONE 'UTC')`, conversationID, id, participant.settings); err != nil {
			t.Fatalf("map participant: %v", err)
		}
		ids[participant.key] = id
	}
	coder, reviewer := ids["coder"], ids["reviewer"]

	repo := NewLocalTurnsRepo(pool)
	floor, err := repo.LocalTurnFloor(ctx, 1, user, conversationUUID)
	if err != nil {
		t.Fatal(err)
	}
	if len(floor.Agents) != 2 || floor.Agents[0] != (localturn.FloorAgent{ParticipantID: coder, Name: "Coder", Executor: "local"}) ||
		floor.Agents[1] != (localturn.FloorAgent{ParticipantID: reviewer, Name: "Reviewer", Executor: "cloud"}) {
		t.Fatalf("floor agents = %+v", floor.Agents)
	}
	if _, err := repo.LocalTurnFloor(ctx, 1, user+1, conversationUUID); !errors.Is(err, localturn.ErrNotFound) {
		t.Fatalf("a stranger's floor read = %v, want ErrNotFound", err)
	}

	next := 0
	service, err := localturn.NewService(repo, &localTurnPolicy{allowed: true}, NewMemoriesRepo(pool), &localTurnAudit{},
		func() (string, error) {
			next++
			return strings.Repeat("0", 30) + strconv.Itoa(10+next), nil
		}, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.WithFloor(repo)
	credential := localturn.Credential{TokenID: "77", NativeClientID: "ai.elitea.desktop"}
	request := func(question string, participant int64) localturn.StartRequest {
		return localturn.StartRequest{
			ProjectID: 1, ActorUserID: user, TokenID: credential.TokenID, NativeClientID: credential.NativeClientID,
			ConversationUUID: conversationUUID, QuestionID: question, UserInput: "Review the change", ParticipantID: participant,
		}
	}
	commit := func(executionID, answer string) localturn.CommittedTurn {
		t.Helper()
		committed, err := service.Commit(ctx, localturn.CommitRequest{
			ProjectID: 1, ActorUserID: user, Credential: credential, ExecutionID: executionID,
			UserMessage: "Review the change", AssistantMessage: answer,
		})
		if err != nil {
			t.Fatalf("commit %s: %v", executionID, err)
		}
		return committed
	}

	// Two agents and no address, no lead, no target: the person picks.
	const first = "a1a1a1a1-0000-4000-8000-000000000001"
	if _, err := service.Start(ctx, request(first, 0)); !errors.Is(err, localturn.ErrAgentTargetRequired) {
		t.Fatalf("unaddressed start = %v, want ErrAgentTargetRequired", err)
	}

	// Addressed to both: the composer's target (Coder) first, then Reviewer.
	addressed := request(first, coder)
	addressed.AddressedTo = []int64{reviewer, coder}
	started, err := service.Start(ctx, addressed)
	if err != nil {
		t.Fatal(err)
	}
	if started.ParticipantID != coder || started.Executor != "local" || started.Floor == nil ||
		!slices.Equal(started.Floor.Targets, []int64{coder, reviewer}) || started.Floor.Position != 0 ||
		started.Floor.QuestionID != first {
		t.Fatalf("first start = %+v floor %+v", started, started.Floor)
	}
	// A replay answers the same routed order.
	if replay, err := service.Start(ctx, addressed); err != nil || replay.Created || replay.Floor == nil ||
		!slices.Equal(replay.Floor.Targets, started.Floor.Targets) {
		t.Fatalf("replayed start = %+v, %v", replay, err)
	}

	// The later turn waits for the first turn's commit.
	const later = "a1a1a1a1-0000-4000-8000-000000000002"
	laterRequest := request(later, reviewer)
	laterRequest.FloorTurn = &localturn.FloorTurnRef{QuestionID: first, Position: 1}
	if _, err := service.Start(ctx, laterRequest); !errors.Is(err, localturn.ErrFloorTurn) {
		t.Fatalf("later turn before the first commit = %v, want ErrFloorTurn", err)
	}
	firstCommit := commit(started.ExecutionID, "Coder's answer")

	// The wrong agent at the position, and a position past the order, are refused.
	wrong := request("a1a1a1a1-0000-4000-8000-000000000003", coder)
	wrong.FloorTurn = &localturn.FloorTurnRef{QuestionID: first, Position: 1}
	if _, err := service.Start(ctx, wrong); !errors.Is(err, localturn.ErrFloorTurn) {
		t.Fatalf("wrong agent at the position = %v, want ErrFloorTurn", err)
	}
	past := request("a1a1a1a1-0000-4000-8000-000000000004", reviewer)
	past.FloorTurn = &localturn.FloorTurnRef{QuestionID: first, Position: 2}
	if _, err := service.Start(ctx, past); !errors.Is(err, localturn.ErrFloorTurn) {
		t.Fatalf("past the order = %v, want ErrFloorTurn", err)
	}

	laterStarted, err := service.Start(ctx, laterRequest)
	if err != nil {
		t.Fatal(err)
	}
	if laterStarted.ParticipantID != reviewer || laterStarted.Executor != "cloud" || laterStarted.Floor == nil ||
		laterStarted.Floor.Position != 1 || laterStarted.Floor.QuestionID != first {
		t.Fatalf("later start = %+v floor %+v", laterStarted, laterStarted.Floor)
	}
	// The cloud agent's turn may relay its own agent to the cloud.
	live, err := localturn.NewLiveTurns(repo, &localTurnPolicy{allowed: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if turn, err := live.Live(ctx, 1, user, credential, laterStarted.ExecutionID); err != nil || !turn.Cloud ||
		turn.ApplicationID != 7 || turn.VersionID != 8 {
		t.Fatalf("live cloud turn = %+v, %v", turn, err)
	}
	laterCommit := commit(laterStarted.ExecutionID, "Reviewer's answer")
	if laterCommit.QuestionMessageID != first {
		t.Fatalf("later commit question = %q, want the person's %q", laterCommit.QuestionMessageID, first)
	}

	// One question, addressed to both agents; two answers replying to it,
	// the reviewer's after the coder's and marked as run in the cloud.
	var questions int
	var addressees []int32
	if err := pool.QueryRow(ctx, `
SELECT count(*) OVER (), addressed_to
FROM p_1.chat_message_group WHERE conversation_id = $1 AND author_participant_id = $2
LIMIT 1`,
		conversationID, ids["user"]).Scan(&questions, &addressees); err != nil {
		t.Fatal(err)
	}
	if questions != 1 || !slices.Equal(addressees, []int32{int32(coder), int32(reviewer)}) {
		t.Fatalf("questions = %d addressed_to %v", questions, addressees)
	}
	rows, err := pool.Query(ctx, `
SELECT answer.author_participant_id, answer.meta ->> 'executed_by', COALESCE(answer.meta ->> 'agent_executor', ''), question.uuid::text
FROM p_1.chat_message_group AS answer
JOIN p_1.chat_message_group AS question ON question.id = answer.reply_to_id
WHERE answer.conversation_id = $1 AND answer.author_participant_id <> $2
ORDER BY answer.created_at, answer.id`, conversationID, ids["user"])
	if err != nil {
		t.Fatal(err)
	}
	type answer struct {
		author     int64
		executedBy string
		executor   string
		question   string
	}
	var answers []answer
	for rows.Next() {
		var a answer
		if err := rows.Scan(&a.author, &a.executedBy, &a.executor, &a.question); err != nil {
			t.Fatal(err)
		}
		answers = append(answers, a)
	}
	rows.Close()
	want := []answer{{coder, "desktop", "", first}, {reviewer, "desktop", "cloud", first}}
	if !slices.Equal(answers, want) {
		t.Fatalf("answers = %+v, want %+v (first commit %s)", answers, want, firstCommit.ResponseMessageID)
	}

	// A newer person's message supersedes the older one's remaining agents.
	const second = "b2b2b2b2-0000-4000-8000-000000000001"
	fresh := request(second, reviewer)
	fresh.AddressedTo = []int64{reviewer, coder}
	freshStarted, err := service.Start(ctx, fresh)
	if err != nil {
		t.Fatal(err)
	}
	if freshStarted.ParticipantID != reviewer || !slices.Equal(freshStarted.Floor.Targets, []int64{reviewer, coder}) {
		t.Fatalf("second message = %+v", freshStarted.Floor)
	}
	stale := request("a1a1a1a1-0000-4000-8000-000000000005", reviewer)
	stale.FloorTurn = &localturn.FloorTurnRef{QuestionID: first, Position: 1}
	if _, err := service.Start(ctx, stale); !errors.Is(err, localturn.ErrFloorSuperseded) {
		t.Fatalf("a superseded message's agent = %v, want ErrFloorSuperseded", err)
	}

	// A cloud agent whose participant is another project's agent cannot be
	// relayed: its start is refused.
	if _, err := pool.Exec(ctx, `
UPDATE p_1.chat_participants SET entity_meta = '{"id": 7, "project_id": 2, "name": "Reviewer"}'::jsonb WHERE id = $1`,
		reviewer); err != nil {
		t.Fatal(err)
	}
	foreign := request("c3c3c3c3-0000-4000-8000-000000000001", reviewer)
	foreign.AddressedTo = []int64{reviewer}
	if _, err := service.Start(ctx, foreign); !errors.Is(err, localturn.ErrParticipant) {
		t.Fatalf("a catalogue cloud agent = %v, want ErrParticipant", err)
	}
}
