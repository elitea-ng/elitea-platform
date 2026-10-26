package repos

import (
	"strings"
	"sync"
	"testing"
	"time"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
)

// endSteeringTurn settles the turn's execution `ago` before now.
func endSteeringTurn(t *testing.T, repository *SteeringRepository, response, executionID string, ago time.Duration) {
	t.Helper()
	if _, err := repository.shared.Exec(t.Context(), `
UPDATE elitea_runtime.execution_jobs
SET state = 'SUCCEEDED', settled_at = clock_timestamp() - make_interval(secs => $2)
WHERE execution_id = $1`, executionID, ago.Seconds()); err != nil {
		t.Fatal(err)
	}
	err := repository.projects.WithinProjectTx(t.Context(), 1, steeringWriteTx, func(tx sqlExecutor) error {
		_, err := tx.Exec(t.Context(), `UPDATE chat_message_group SET is_streaming = FALSE WHERE uuid = $1::uuid`, response)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func queueSteeringTexts(t *testing.T, repository *SteeringRepository, response string, texts ...string) {
	t.Helper()
	for index, text := range texts {
		if _, _, err := repository.QueueSteering(t.Context(), agentexecutionapp.SteeringQueueRequest{
			SteeringTarget: steeringTarget(response), IdempotencyKey: "queue-" + string(rune('a'+index)), Content: text,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPostgresSteeringClientHandOffReplaysAndWaitsForTheEnd(t *testing.T) {
	repository, pool := newSteeringIntegrationRepository(t)
	response := seedSteeringTurn(t, pool, "21", "execution-steer-handoff-client", "RUNNING")
	queueSteeringTexts(t, repository, response, "first", "second")
	target := steeringTarget(response)

	// A running turn hands off nothing.
	handed, err := repository.HandOffSteering(t.Context(), agentexecutionapp.SteeringHandOffRequest{SteeringTarget: target, IdempotencyKey: "h-1"})
	if err != nil || len(handed) != 0 {
		t.Fatalf("running handed=%+v err=%v", handed, err)
	}
	endSteeringTurn(t, repository, response, "execution-steer-handoff-client", 0)
	handed, err = repository.HandOffSteering(t.Context(), agentexecutionapp.SteeringHandOffRequest{SteeringTarget: target, IdempotencyKey: "h-1"})
	if err != nil || len(handed) != 2 || handed[0].Content != "first" || handed[1].Content != "second" ||
		handed[0].State != agentexecutionapp.SteeringHandedOff {
		t.Fatalf("handed=%+v err=%v", handed, err)
	}
	// The same key answers the same rows; another key finds nothing left.
	replay, err := repository.HandOffSteering(t.Context(), agentexecutionapp.SteeringHandOffRequest{SteeringTarget: target, IdempotencyKey: "h-1"})
	if err != nil || len(replay) != 2 || replay[0].ID != handed[0].ID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	other, err := repository.HandOffSteering(t.Context(), agentexecutionapp.SteeringHandOffRequest{SteeringTarget: target, IdempotencyKey: "h-2"})
	if err != nil || len(other) != 0 {
		t.Fatalf("other=%+v err=%v", other, err)
	}
	// Another participant's hand-off does not take the author's rows.
	stranger := target
	stranger.ActorUserID = 12
	if taken, err := repository.HandOffSteering(t.Context(), agentexecutionapp.SteeringHandOffRequest{SteeringTarget: stranger, IdempotencyKey: "h-3"}); err != nil || len(taken) != 0 {
		t.Fatalf("stranger took %+v", taken)
	}
	listed, err := repository.ListSteering(t.Context(), target)
	if err != nil || len(listed) != 2 || listed[0].State != agentexecutionapp.SteeringHandedOff {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
	// The server finds nothing to start.
	server, _, err := repository.TakeServerHandOffs(t.Context(), 0, time.Hour, 10)
	if err != nil || len(server) != 0 {
		t.Fatalf("server=%+v err=%v", server, err)
	}
}

func TestPostgresSteeringServerHandOffStartsRetriesAndDrops(t *testing.T) {
	repository, pool := newSteeringIntegrationRepository(t)
	response := seedSteeringTurn(t, pool, "22", "execution-steer-handoff-server", "RUNNING")
	queueSteeringTexts(t, repository, response, "use the staging branch", "and skip the docs")

	// Not before the client's window has passed.
	endSteeringTurn(t, repository, response, "execution-steer-handoff-server", 2*time.Second)
	early, _, err := repository.TakeServerHandOffs(t.Context(), agentexecutionapp.ServerHandOffDelay, time.Hour, 10)
	if err != nil || len(early) != 0 {
		t.Fatalf("early=%+v err=%v", early, err)
	}
	endSteeringTurn(t, repository, response, "execution-steer-handoff-server", time.Minute)
	taken, _, err := repository.TakeServerHandOffs(t.Context(), agentexecutionapp.ServerHandOffDelay, time.Hour, 10)
	if err != nil || len(taken) != 1 {
		t.Fatalf("taken=%+v err=%v", taken, err)
	}
	handOff := taken[0]
	if handOff.ProjectID != 1 || handOff.AuthorUserID != 11 || handOff.ResponseMessageID != response ||
		handOff.ConversationUUID == "" || handOff.ParticipantID <= 0 || handOff.Adhoc || handOff.QuestionID == "" ||
		len(handOff.Texts) != 2 || handOff.Texts[0] != "use the staging branch" || len(handOff.SteeringIDs) != 2 {
		t.Fatalf("handOff=%+v", handOff)
	}
	// A client that comes late gets nothing: the server owns the rows.
	late, err := repository.HandOffSteering(t.Context(), agentexecutionapp.SteeringHandOffRequest{SteeringTarget: steeringTarget(response), IdempotencyKey: "late"})
	if err != nil || len(late) != 0 {
		t.Fatalf("late=%+v err=%v", late, err)
	}
	// Not started: a retry answers the same question id.
	retried, _, err := repository.TakeServerHandOffs(t.Context(), agentexecutionapp.ServerHandOffDelay, 0, 10)
	if err != nil || len(retried) != 1 || retried[0].QuestionID != handOff.QuestionID || len(retried[0].Texts) != 2 {
		t.Fatalf("retried=%+v err=%v", retried, err)
	}
	if err := repository.MarkServerHandOffStarted(t.Context(), handOff); err != nil {
		t.Fatal(err)
	}
	again, _, err := repository.TakeServerHandOffs(t.Context(), agentexecutionapp.ServerHandOffDelay, 0, 10)
	if err != nil || len(again) != 0 {
		t.Fatalf("started again=%+v err=%v", again, err)
	}

	// A group that can never start is removed.
	dropped := seedSteeringTurn(t, pool, "23", "execution-steer-handoff-drop", "RUNNING")
	queueSteeringTexts(t, repository, dropped, "never")
	endSteeringTurn(t, repository, dropped, "execution-steer-handoff-drop", time.Minute)
	toDrop, _, err := repository.TakeServerHandOffs(t.Context(), agentexecutionapp.ServerHandOffDelay, time.Hour, 10)
	if err != nil || len(toDrop) != 1 {
		t.Fatalf("toDrop=%+v err=%v", toDrop, err)
	}
	if err := repository.DropServerHandOff(t.Context(), toDrop[0]); err != nil {
		t.Fatal(err)
	}
	listed, err := repository.ListSteering(t.Context(), steeringTarget(dropped))
	if err != nil || len(listed) != 1 || listed[0].State != agentexecutionapp.SteeringRemoved {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
}

// TestPostgresSteeringHandOffRaceStartsOneTurn runs a client hand-off and the
// server's take at the same time, many times: exactly one of them takes the
// rows, so exactly one next turn can start.
func TestPostgresSteeringHandOffRaceStartsOneTurn(t *testing.T) {
	repository, pool := newSteeringIntegrationRepository(t)
	clientWins, serverWins := 0, 0
	for round := range 8 {
		suffix := string(rune('3')) + string(rune('0'+round))
		executionID := "execution-steer-race-" + suffix
		response := seedSteeringTurn(t, pool, suffix, executionID, "RUNNING")
		queueSteeringTexts(t, repository, response, "one", "two", "three")
		endSteeringTurn(t, repository, response, executionID, time.Minute)

		var wait sync.WaitGroup
		start := make(chan struct{})
		var client []agentexecutionapp.SteeringMessage
		var server []agentexecutionapp.ServerHandOff
		var clientErr, serverErr error
		wait.Add(2)
		go func() {
			defer wait.Done()
			<-start
			client, clientErr = repository.HandOffSteering(t.Context(), agentexecutionapp.SteeringHandOffRequest{
				SteeringTarget: steeringTarget(response), IdempotencyKey: "race-" + suffix,
			})
		}()
		go func() {
			defer wait.Done()
			<-start
			server, _, serverErr = repository.TakeServerHandOffs(t.Context(), agentexecutionapp.ServerHandOffDelay, time.Hour, 10)
		}()
		close(start)
		wait.Wait()
		if clientErr != nil || serverErr != nil {
			t.Fatalf("round %d: client err=%v server err=%v", round, clientErr, serverErr)
		}
		serverTook := 0
		for _, handOff := range server {
			if handOff.ResponseMessageID == response {
				serverTook += len(handOff.Texts)
			}
		}
		switch {
		case len(client) == 3 && serverTook == 0:
			clientWins++
		case len(client) == 0 && serverTook == 3:
			serverWins++
			// Mark it started, so the next round's take does not retry it.
			for _, handOff := range server {
				if err := repository.MarkServerHandOffStarted(t.Context(), handOff); err != nil {
					t.Fatal(err)
				}
			}
		default:
			t.Fatalf("round %d: client took %d, server took %d", round, len(client), serverTook)
		}
		var keys int
		if err := repository.shared.QueryRow(t.Context(), `
SELECT count(DISTINCT handoff_key) FROM elitea_runtime.execution_steering_messages
WHERE response_message_id = $1::uuid AND state = 'handed_off'`, response).Scan(&keys); err != nil || keys != 1 {
			t.Fatalf("round %d: keys=%d err=%v", round, keys, err)
		}
	}
	t.Logf("client won %d, server won %d", clientWins, serverWins)
}

// TestPostgresSteeringServerHandOffSkipsANewerMessage: the author already sent
// a newer message in the conversation, so the server starts no turn. The
// leftover messages are removed, and steer.dropped `superseded` goes on the
// newer turn's stream, which the client watches.
func TestPostgresSteeringServerHandOffSkipsANewerMessage(t *testing.T) {
	repository, pool := newSteeringIntegrationRepository(t)
	response := seedSteeringTurn(t, pool, "41", "execution-steer-superseded-old", "RUNNING")
	queueSteeringTexts(t, repository, response, "old instruction", "another one")
	endSteeringTurn(t, repository, response, "execution-steer-superseded-old", time.Minute)
	// The author's newer turn in the same conversation.
	seedSteeringTurn(t, pool, "42", "execution-steer-superseded-new", "RUNNING")

	taken, effects, err := repository.TakeServerHandOffs(t.Context(), agentexecutionapp.ServerHandOffDelay, time.Hour, 10)
	if err != nil || len(taken) != 0 || len(effects) != 2 {
		t.Fatalf("taken=%+v effects=%+v err=%v", taken, effects, err)
	}
	for _, effect := range effects {
		if effect.ExecutionID != "execution-steer-superseded-new" || effect.Cursor == 0 {
			t.Fatalf("effect=%+v", effect)
		}
	}
	listed, err := repository.ListSteering(t.Context(), steeringTarget(response))
	if err != nil || len(listed) != 2 || listed[0].State != agentexecutionapp.SteeringRemoved || listed[1].State != agentexecutionapp.SteeringRemoved {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
	rows, err := repository.shared.Query(t.Context(), `
SELECT event_type, convert_from(event_bytes, 'UTF8') FROM elitea_runtime.execution_replay_events
WHERE execution_id = 'execution-steer-superseded-new' AND event_type = 'steer.dropped'
ORDER BY cursor`)
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	for rows.Next() {
		var eventType, data string
		if err := rows.Scan(&eventType, &data); err != nil {
			t.Fatal(err)
		}
		events = append(events, data)
	}
	rows.Close()
	if len(events) != 2 || !strings.Contains(events[0], `"reason":"superseded"`) ||
		!strings.Contains(events[0], `"content":"old instruction"`) || !strings.Contains(events[0], response) {
		t.Fatalf("events=%v", events)
	}
	// Nothing is left to start, and the client's hand-off finds nothing.
	again, _, err := repository.TakeServerHandOffs(t.Context(), 0, 0, 10)
	if err != nil || len(again) != 0 {
		t.Fatalf("again=%+v err=%v", again, err)
	}
	if handed, err := repository.HandOffSteering(t.Context(), agentexecutionapp.SteeringHandOffRequest{
		SteeringTarget: steeringTarget(response), IdempotencyKey: "after",
	}); err != nil || len(handed) != 0 {
		t.Fatalf("handed=%+v err=%v", handed, err)
	}
}
