package repos

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/google/uuid"
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
	handed, err := repository.HandOffSteering(t.Context(), agentexecutionapp.SteeringHandOffRequest{SteeringTarget: target, IdempotencyKey: "h-1", QuestionID: uuid.NewString()})
	if err != nil || len(handed) != 0 {
		t.Fatalf("running handed=%+v err=%v", handed, err)
	}
	endSteeringTurn(t, repository, response, "execution-steer-handoff-client", 0)
	handed, err = repository.HandOffSteering(t.Context(), agentexecutionapp.SteeringHandOffRequest{SteeringTarget: target, IdempotencyKey: "h-1", QuestionID: uuid.NewString()})
	if err != nil || len(handed) != 2 || handed[0].Content != "first" || handed[1].Content != "second" ||
		handed[0].State != agentexecutionapp.SteeringHandedOff {
		t.Fatalf("handed=%+v err=%v", handed, err)
	}
	// The same key answers the same rows; another key finds nothing left.
	replay, err := repository.HandOffSteering(t.Context(), agentexecutionapp.SteeringHandOffRequest{SteeringTarget: target, IdempotencyKey: "h-1", QuestionID: uuid.NewString()})
	if err != nil || len(replay) != 2 || replay[0].ID != handed[0].ID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	other, err := repository.HandOffSteering(t.Context(), agentexecutionapp.SteeringHandOffRequest{SteeringTarget: target, IdempotencyKey: "h-2", QuestionID: uuid.NewString()})
	if err != nil || len(other) != 0 {
		t.Fatalf("other=%+v err=%v", other, err)
	}
	// Another participant's hand-off does not take the author's rows.
	stranger := target
	stranger.ActorUserID = 12
	if taken, err := repository.HandOffSteering(t.Context(), agentexecutionapp.SteeringHandOffRequest{SteeringTarget: stranger, IdempotencyKey: "h-3", QuestionID: uuid.NewString()}); err != nil || len(taken) != 0 {
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
	late, err := repository.HandOffSteering(t.Context(), agentexecutionapp.SteeringHandOffRequest{SteeringTarget: steeringTarget(response), IdempotencyKey: "late", QuestionID: uuid.NewString()})
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
				SteeringTarget: steeringTarget(response), IdempotencyKey: "race-" + suffix, QuestionID: uuid.NewString(),
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

// TestPostgresSteeringServerHandOffCarriesToTheRunningNewerTurn: the author
// already sent a newer message, and its turn runs. The leftovers move to that
// turn as pending, before its own steering, with steer.queued on its stream.
func TestPostgresSteeringServerHandOffCarriesToTheRunningNewerTurn(t *testing.T) {
	repository, pool := newSteeringIntegrationRepository(t)
	old := seedSteeringTurn(t, pool, "41", "execution-steer-carry-old", "RUNNING")
	queueSteeringTexts(t, repository, old, "old instruction", "another one")
	endSteeringTurn(t, repository, old, "execution-steer-carry-old", time.Minute)
	newer := seedSteeringTurn(t, pool, "42", "execution-steer-carry-new", "RUNNING")
	if _, _, err := repository.QueueSteering(t.Context(), agentexecutionapp.SteeringQueueRequest{
		SteeringTarget: steeringTarget(newer), IdempotencyKey: "newer-own", Content: "newer own",
	}); err != nil {
		t.Fatal(err)
	}

	taken, effects, err := repository.TakeServerHandOffs(t.Context(), agentexecutionapp.ServerHandOffDelay, time.Hour, 10)
	if err != nil || len(taken) != 0 || len(effects) != 2 {
		t.Fatalf("taken=%+v effects=%+v err=%v", taken, effects, err)
	}
	for _, effect := range effects {
		if effect.ExecutionID != "execution-steer-carry-new" || effect.Cursor == 0 {
			t.Fatalf("effect=%+v", effect)
		}
	}
	listed, err := repository.ListSteering(t.Context(), steeringTarget(newer))
	if err != nil || len(listed) != 3 ||
		listed[0].Content != "old instruction" || listed[1].Content != "another one" || listed[2].Content != "newer own" ||
		listed[0].State != agentexecutionapp.SteeringPending || listed[0].Revision != 2 || listed[0].ResponseMessageID != newer {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
	if left, err := repository.ListSteering(t.Context(), steeringTarget(old)); err != nil || len(left) != 0 {
		t.Fatalf("left=%+v err=%v", left, err)
	}
	var events []string
	rows, err := repository.shared.Query(t.Context(), `
SELECT convert_from(event_bytes, 'UTF8') FROM elitea_runtime.execution_replay_events
WHERE execution_id = 'execution-steer-carry-new' AND event_type = 'steer.queued'
ORDER BY cursor`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			t.Fatal(err)
		}
		events = append(events, data)
	}
	rows.Close()
	// The newer turn's own message, then the two carried ones.
	if len(events) != 3 || !strings.Contains(events[1], `"content":"old instruction"`) ||
		!strings.Contains(events[1], `"revision":2`) || !strings.Contains(events[1], newer) {
		t.Fatalf("events=%v", events)
	}
	// Idempotent: nothing is left to carry or start, and the old answer's
	// client hand-off finds nothing.
	again, more, err := repository.TakeServerHandOffs(t.Context(), 0, 0, 10)
	if err != nil || len(again) != 0 || len(more) != 0 {
		t.Fatalf("again=%+v more=%+v err=%v", again, more, err)
	}
	if handed, err := repository.HandOffSteering(t.Context(), agentexecutionapp.SteeringHandOffRequest{
		SteeringTarget: steeringTarget(old), IdempotencyKey: "after", QuestionID: uuid.NewString(),
	}); err != nil || len(handed) != 0 {
		t.Fatalf("handed=%+v err=%v", handed, err)
	}
}

// TestPostgresSteeringServerHandOffCarriesToAnEndedNewerTurn: the author's
// newer turn already ended. The leftovers move to it as turn_ended, and the
// next pass starts ONE next turn with them, after the newer turn.
func TestPostgresSteeringServerHandOffCarriesToAnEndedNewerTurn(t *testing.T) {
	repository, pool := newSteeringIntegrationRepository(t)
	old := seedSteeringTurn(t, pool, "43", "execution-steer-carry-old2", "RUNNING")
	queueSteeringTexts(t, repository, old, "first", "second")
	endSteeringTurn(t, repository, old, "execution-steer-carry-old2", 2*time.Minute)
	newer := seedSteeringTurn(t, pool, "44", "execution-steer-carry-new2", "RUNNING")
	endSteeringTurn(t, repository, newer, "execution-steer-carry-new2", time.Minute)

	taken, effects, err := repository.TakeServerHandOffs(t.Context(), agentexecutionapp.ServerHandOffDelay, time.Hour, 10)
	if err != nil || len(taken) != 0 || len(effects) != 2 || effects[0].ExecutionID != "execution-steer-carry-new2" {
		t.Fatalf("taken=%+v effects=%+v err=%v", taken, effects, err)
	}
	listed, err := repository.ListSteering(t.Context(), steeringTarget(newer))
	if err != nil || len(listed) != 2 || listed[0].State != agentexecutionapp.SteeringTurnEnded {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
	next, _, err := repository.TakeServerHandOffs(t.Context(), agentexecutionapp.ServerHandOffDelay, time.Hour, 10)
	if err != nil || len(next) != 1 || next[0].ResponseMessageID != newer ||
		len(next[0].Texts) != 2 || next[0].Texts[0] != "first" || next[0].Texts[1] != "second" || next[0].ParticipantID <= 0 {
		t.Fatalf("next=%+v err=%v", next, err)
	}
}

// TestPostgresSteeringCarryRacesTheClientHandOff runs the old answer's client
// hand-off and the server's carry-over at the same time: exactly one of them
// takes every message.
func TestPostgresSteeringCarryRacesTheClientHandOff(t *testing.T) {
	repository, pool := newSteeringIntegrationRepository(t)
	for round := range 4 {
		oldSuffix := "5" + string(rune('0'+round))
		newSuffix := "6" + string(rune('0'+round))
		old := seedSteeringTurn(t, pool, oldSuffix, "execution-steer-carry-race-old-"+oldSuffix, "RUNNING")
		queueSteeringTexts(t, repository, old, "one", "two")
		endSteeringTurn(t, repository, old, "execution-steer-carry-race-old-"+oldSuffix, time.Minute)
		newer := seedSteeringTurn(t, pool, newSuffix, "execution-steer-carry-race-new-"+newSuffix, "RUNNING")

		var wait sync.WaitGroup
		start := make(chan struct{})
		var client []agentexecutionapp.SteeringMessage
		var carried []agentexecutionapp.SteeringEffect
		var clientErr, serverErr error
		wait.Add(2)
		go func() {
			defer wait.Done()
			<-start
			client, clientErr = repository.HandOffSteering(t.Context(), agentexecutionapp.SteeringHandOffRequest{
				SteeringTarget: steeringTarget(old), IdempotencyKey: "carry-race-" + oldSuffix, QuestionID: uuid.NewString(),
			})
		}()
		go func() {
			defer wait.Done()
			<-start
			_, carried, serverErr = repository.TakeServerHandOffs(t.Context(), agentexecutionapp.ServerHandOffDelay, time.Hour, 10)
		}()
		close(start)
		wait.Wait()
		if clientErr != nil || serverErr != nil {
			t.Fatalf("round %d: client err=%v server err=%v", round, clientErr, serverErr)
		}
		onNewer, err := repository.ListSteering(t.Context(), steeringTarget(newer))
		if err != nil {
			t.Fatal(err)
		}
		clientWon := len(client) == 2 && len(carried) == 0 && len(onNewer) == 0
		serverWon := len(client) == 0 && len(carried) == 2 && len(onNewer) == 2
		if clientWon == serverWon {
			t.Fatalf("round %d: client took %d, carried %d, newer lists %d", round, len(client), len(carried), len(onNewer))
		}
		// End the newer turn, so the next round's newer message is the
		// newest and its carried rows wait for their own hand-off.
		endSteeringTurn(t, repository, newer, "execution-steer-carry-race-new-"+newSuffix, 0)
	}
}

// endSteeringTurnAs settles the turn's execution in a terminal state.
func endSteeringTurnAs(t *testing.T, repository *SteeringRepository, response, executionID, state string, ago time.Duration) {
	t.Helper()
	endSteeringTurn(t, repository, response, executionID, ago)
	if _, err := repository.shared.Exec(t.Context(), `
UPDATE elitea_runtime.execution_jobs SET state = $2, desired_state = CASE WHEN $2 = 'CANCELLED' THEN 'CANCELLED' ELSE desired_state END
WHERE execution_id = $1`, executionID, state); err != nil {
		t.Fatal(err)
	}
}

func countSteeringEvents(t *testing.T, repository *SteeringRepository, executionID string) int {
	t.Helper()
	var count int
	if err := repository.shared.QueryRow(t.Context(), `
SELECT count(*) FROM elitea_runtime.execution_replay_events WHERE execution_id = $1 AND event_type LIKE 'steer.%'`,
		executionID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// TestPostgresSteeringRemoveRefusesAHandedOffMessage: a handed_off message is
// final; a remove changes nothing and writes no event.
func TestPostgresSteeringRemoveRefusesAHandedOffMessage(t *testing.T) {
	repository, pool := newSteeringIntegrationRepository(t)
	response := seedSteeringTurn(t, pool, "70", "execution-steer-remove-handed", "RUNNING")
	queueSteeringTexts(t, repository, response, "keep")
	endSteeringTurn(t, repository, response, "execution-steer-remove-handed", 0)
	handed, err := repository.HandOffSteering(t.Context(), agentexecutionapp.SteeringHandOffRequest{
		SteeringTarget: steeringTarget(response), IdempotencyKey: "h-remove", QuestionID: uuid.NewString(),
	})
	if err != nil || len(handed) != 1 {
		t.Fatalf("handed=%+v err=%v", handed, err)
	}
	before := countSteeringEvents(t, repository, "execution-steer-remove-handed")
	if _, err := repository.RemoveSteering(t.Context(), agentexecutionapp.SteeringRemoveRequest{
		SteeringTarget: steeringTarget(response), SteeringID: handed[0].ID,
	}); !errors.Is(err, agentexecutionapp.ErrSteeringNotPending) {
		t.Fatalf("remove err=%v", err)
	}
	if after := countSteeringEvents(t, repository, "execution-steer-remove-handed"); after != before {
		t.Fatalf("events %d -> %d", before, after)
	}
	listed, err := repository.ListSteering(t.Context(), steeringTarget(response))
	if err != nil || len(listed) != 1 || listed[0].State != agentexecutionapp.SteeringHandedOff {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
}

// TestPostgresSteeringServerNeverContinuesAStoppedOrFailedTurn: the server
// leaves the leftovers of a stopped or failed turn for the author; the
// client can still hand them off; a newer message still carries them over.
func TestPostgresSteeringServerNeverContinuesAStoppedOrFailedTurn(t *testing.T) {
	repository, pool := newSteeringIntegrationRepository(t)
	// Each turn is the newest of the conversation when the pass runs (the
	// seeds share one conversation), so no newer message carries it over.
	stopped := seedSteeringTurn(t, pool, "71", "execution-steer-stopped", "RUNNING")
	queueSteeringTexts(t, repository, stopped, "after the stop")
	endSteeringTurnAs(t, repository, stopped, "execution-steer-stopped", "CANCELLED", time.Minute)
	taken, effects, err := repository.TakeServerHandOffs(t.Context(), agentexecutionapp.ServerHandOffDelay, time.Hour, 10)
	if err != nil || len(taken) != 0 || len(effects) != 0 {
		t.Fatalf("stopped taken=%+v effects=%+v err=%v", taken, effects, err)
	}
	failed := seedSteeringTurn(t, pool, "72", "execution-steer-failed", "RUNNING")
	queueSteeringTexts(t, repository, failed, "after the failure")
	endSteeringTurnAs(t, repository, failed, "execution-steer-failed", "FAILED", time.Minute)
	taken, effects, err = repository.TakeServerHandOffs(t.Context(), agentexecutionapp.ServerHandOffDelay, time.Hour, 10)
	if err != nil || len(taken) != 0 || len(effects) != 0 {
		t.Fatalf("failed taken=%+v effects=%+v err=%v", taken, effects, err)
	}
	for _, response := range []string{stopped, failed} {
		listed, err := repository.ListSteering(t.Context(), steeringTarget(response))
		if err != nil || len(listed) != 1 || listed[0].State != agentexecutionapp.SteeringTurnEnded {
			t.Fatalf("listed=%+v err=%v", listed, err)
		}
	}
	// Held: later passes do not look at them again.
	if again, _, err := repository.TakeServerHandOffs(t.Context(), 0, time.Hour, 10); err != nil || len(again) != 0 {
		t.Fatalf("again=%+v err=%v", again, err)
	}
	// The author accepts the client's offer: the hand-off still works.
	handed, err := repository.HandOffSteering(t.Context(), agentexecutionapp.SteeringHandOffRequest{
		SteeringTarget: steeringTarget(failed), IdempotencyKey: "offer", QuestionID: uuid.NewString(),
	})
	if err != nil || len(handed) != 1 || handed[0].Content != "after the failure" {
		t.Fatalf("handed=%+v err=%v", handed, err)
	}

	// A stopped turn whose author already sent a newer message: carried over.
	stoppedAgain := seedSteeringTurn(t, pool, "73", "execution-steer-stopped-2", "RUNNING")
	queueSteeringTexts(t, repository, stoppedAgain, "carry me")
	endSteeringTurnAs(t, repository, stoppedAgain, "execution-steer-stopped-2", "CANCELLED", time.Minute)
	newer := seedSteeringTurn(t, pool, "74", "execution-steer-stopped-newer", "RUNNING")
	_, carried, err := repository.TakeServerHandOffs(t.Context(), agentexecutionapp.ServerHandOffDelay, time.Hour, 10)
	if err != nil || len(carried) != 1 || carried[0].ExecutionID != "execution-steer-stopped-newer" {
		t.Fatalf("carried=%+v err=%v", carried, err)
	}
	if listed, err := repository.ListSteering(t.Context(), steeringTarget(newer)); err != nil || len(listed) != 1 || listed[0].Content != "carry me" {
		t.Fatalf("newer listed=%+v err=%v", listed, err)
	}
}

// TestPostgresSteeringServerStartsAClientHandOffThatNeverSent: the client
// handed off and closed before its send. After the grace, the server starts
// that turn with the client's question id; a send that did happen is left.
func TestPostgresSteeringServerStartsAClientHandOffThatNeverSent(t *testing.T) {
	repository, pool := newSteeringIntegrationRepository(t)
	lost := seedSteeringTurn(t, pool, "75", "execution-steer-client-lost", "RUNNING")
	queueSteeringTexts(t, repository, lost, "lost one", "lost two")
	endSteeringTurn(t, repository, lost, "execution-steer-client-lost", 0)
	questionID := uuid.NewString()
	if handed, err := repository.HandOffSteering(t.Context(), agentexecutionapp.SteeringHandOffRequest{
		SteeringTarget: steeringTarget(lost), IdempotencyKey: "h-lost", QuestionID: questionID,
	}); err != nil || len(handed) != 2 {
		t.Fatalf("handed=%+v err=%v", handed, err)
	}
	// Inside the grace: the client may still send.
	if early, _, err := repository.TakeServerHandOffs(t.Context(), agentexecutionapp.ServerHandOffDelay, time.Hour, 10); err != nil || len(early) != 0 {
		t.Fatalf("early=%+v err=%v", early, err)
	}
	if _, err := repository.shared.Exec(t.Context(), `
UPDATE elitea_runtime.execution_steering_messages SET handed_off_at = clock_timestamp() - interval '2 minutes'
WHERE handoff_key = 'h-lost'`); err != nil {
		t.Fatal(err)
	}
	taken, _, err := repository.TakeServerHandOffs(t.Context(), agentexecutionapp.ServerHandOffDelay, time.Hour, 10)
	if err != nil || len(taken) != 1 || taken[0].QuestionID != questionID || len(taken[0].Texts) != 2 || taken[0].Texts[0] != "lost one" {
		t.Fatalf("taken=%+v err=%v", taken, err)
	}

	// A client that did send: its question exists, so the hand-off is only
	// marked started.
	sent := seedSteeringTurn(t, pool, "76", "execution-steer-client-sent", "RUNNING")
	queueSteeringTexts(t, repository, sent, "sent")
	endSteeringTurn(t, repository, sent, "execution-steer-client-sent", 0)
	// The question of a later turn stands for the client's send.
	next := seedSteeringTurn(t, pool, "77", "execution-steer-client-next", "RUNNING")
	_ = next
	if _, err := repository.HandOffSteering(t.Context(), agentexecutionapp.SteeringHandOffRequest{
		SteeringTarget: steeringTarget(sent), IdempotencyKey: "h-sent", QuestionID: "20000000-0000-4000-8000-000000000277",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.shared.Exec(t.Context(), `
UPDATE elitea_runtime.execution_steering_messages SET handed_off_at = clock_timestamp() - interval '2 minutes'
WHERE handoff_key = 'h-sent'`); err != nil {
		t.Fatal(err)
	}
	if again, _, err := repository.TakeServerHandOffs(t.Context(), agentexecutionapp.ServerHandOffDelay, time.Hour, 10); err != nil ||
		len(again) != 1 || again[0].QuestionID != questionID {
		t.Fatalf("again=%+v err=%v", again, err)
	}
	var started bool
	if err := repository.shared.QueryRow(t.Context(), `
SELECT handoff_started_at IS NOT NULL FROM elitea_runtime.execution_steering_messages WHERE handoff_key = 'h-sent'`).Scan(&started); err != nil || !started {
		t.Fatalf("started=%v err=%v", started, err)
	}
}

// TestPostgresSteeringQueueCapHoldsUnderConcurrency: twenty concurrent queues
// on one running turn admit exactly the cap.
func TestPostgresSteeringQueueCapHoldsUnderConcurrency(t *testing.T) {
	repository, pool := newSteeringIntegrationRepository(t)
	response := seedSteeringTurn(t, pool, "78", "execution-steer-cap", "RUNNING")
	var wait sync.WaitGroup
	start := make(chan struct{})
	results := make([]error, 20)
	for index := range results {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, _, results[index] = repository.QueueSteering(t.Context(), agentexecutionapp.SteeringQueueRequest{
				SteeringTarget: steeringTarget(response), IdempotencyKey: "cap-" + uuid.NewString(), Content: "message",
			})
		}()
	}
	close(start)
	wait.Wait()
	admitted, full := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			admitted++
		case errors.Is(err, agentexecutionapp.ErrSteeringQueueFull):
			full++
		default:
			t.Fatalf("err=%v", err)
		}
	}
	if admitted != agentexecutionapp.MaxPendingSteeringMessages || full != 20-admitted {
		t.Fatalf("admitted=%d full=%d", admitted, full)
	}
}

// TestPostgresSteeringCarryKeepsTheCap: a running newer turn with 9 waiting
// messages takes one carried message; the other waits for a free place.
func TestPostgresSteeringCarryKeepsTheCap(t *testing.T) {
	repository, pool := newSteeringIntegrationRepository(t)
	old := seedSteeringTurn(t, pool, "79", "execution-steer-carry-cap-old", "RUNNING")
	queueSteeringTexts(t, repository, old, "first", "second")
	endSteeringTurn(t, repository, old, "execution-steer-carry-cap-old", time.Minute)
	newer := seedSteeringTurn(t, pool, "80", "execution-steer-carry-cap-new", "RUNNING")
	for index := range agentexecutionapp.MaxPendingSteeringMessages - 1 {
		if _, _, err := repository.QueueSteering(t.Context(), agentexecutionapp.SteeringQueueRequest{
			SteeringTarget: steeringTarget(newer), IdempotencyKey: "own-" + string(rune('a'+index)), Content: "own",
		}); err != nil {
			t.Fatal(err)
		}
	}
	_, carried, err := repository.TakeServerHandOffs(t.Context(), agentexecutionapp.ServerHandOffDelay, time.Hour, 10)
	if err != nil || len(carried) != 1 {
		t.Fatalf("carried=%+v err=%v", carried, err)
	}
	listed, err := repository.ListSteering(t.Context(), steeringTarget(newer))
	if err != nil || len(listed) != agentexecutionapp.MaxPendingSteeringMessages || listed[0].Content != "first" {
		t.Fatalf("newer listed=%d err=%v", len(listed), err)
	}
	left, err := repository.ListSteering(t.Context(), steeringTarget(old))
	if err != nil || len(left) != 1 || left[0].Content != "second" || left[0].State != agentexecutionapp.SteeringTurnEnded {
		t.Fatalf("left=%+v err=%v", left, err)
	}
	if _, more, err := repository.TakeServerHandOffs(t.Context(), agentexecutionapp.ServerHandOffDelay, time.Hour, 10); err != nil || len(more) != 0 {
		t.Fatalf("more=%+v err=%v", more, err)
	}
}

// TestPostgresSteeringListNeverHidesAWaitingMessage: a long history of final
// messages does not push a waiting one out of the capped list.
func TestPostgresSteeringListNeverHidesAWaitingMessage(t *testing.T) {
	repository, pool := newSteeringIntegrationRepository(t)
	response := seedSteeringTurn(t, pool, "81", "execution-steer-list-cap", "RUNNING")
	queueSteeringTexts(t, repository, response, "waiting")
	if _, err := repository.shared.Exec(t.Context(), `
INSERT INTO elitea_runtime.execution_steering_messages (
    steering_id, execution_id, generation, project_id, response_message_id, author_user_id, idempotency_key, content, state
)
SELECT gen_random_uuid(), 'execution-steer-list-cap', 1, 1, $1::uuid, 11, 'old-' || n, 'old', 'removed'
FROM generate_series(1, 80) AS n`, response); err != nil {
		t.Fatal(err)
	}
	listed, err := repository.ListSteering(t.Context(), steeringTarget(response))
	if err != nil || len(listed) != 64 {
		t.Fatalf("listed=%d err=%v", len(listed), err)
	}
	found := false
	for index, message := range listed {
		if index > 0 && message.QueueOrder <= listed[index-1].QueueOrder {
			t.Fatalf("not in queue order at %d", index)
		}
		found = found || message.Content == "waiting"
	}
	if !found {
		t.Fatal("the waiting message is hidden")
	}
}
