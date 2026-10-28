//go:build conformance

package governed_test

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestGovernedOldWorker is GOV-007 (phase oldworker): the governance flag is
// on and the worker runs WITHOUT the governed context, as a worker image that
// predates the profile would. A governed turn must be admitted, pinned to the
// governed profile and HELD: the old worker consumes only the legacy subjects,
// so it never claims the command, and nothing runs it on the legacy path.
func TestGovernedOldWorker(t *testing.T) {
	ctx := testContext(t, 10*time.Minute)
	w := newWorld(ctx, t, phaseOldWorker)
	s := w.cfg.readState(t)
	if s.OldWorker == nil {
		t.Fatal("state.json has no GOV-007 fixture; phase on (TestGovernedProfile/GOV-007/prepare) must run first")
	}
	t.Run("GOV-007/old_worker_does_not_claim_a_governed_run", func(t *testing.T) {
		marker := "GOVHELD" + shortID()
		result := w.mustAdmit(ctx, t, w.user, *s.OldWorker, "hold this run "+marker)
		if profile := w.admissionProfile(ctx, t, result.ExecutionID); profile != governedProfile {
			t.Fatalf("the turn was pinned to %s", profile)
		}
		// Long enough for an idle worker to claim and answer an echo many
		// times over (the mock answers in well under a second).
		time.Sleep(90 * time.Second)
		// Exactly one message may carry the marker: the user's own prompt.
		// A second one is an answer, which means some worker ran the run.
		var carrying []string
		for _, message := range w.messages(ctx, t, w.user, *s.OldWorker) {
			if raw := messageRaw(message); strings.Contains(raw, marker) {
				carrying = append(carrying, raw)
			}
		}
		switch {
		case len(carrying) == 0:
			t.Fatal("the transcript does not even hold the prompt; the hold cannot be judged")
		case len(carrying) > 1:
			t.Fatalf("a worker without the governed context answered a governed run: %v", carrying[1:])
		}
		if n := w.cfg.psqlInt(ctx, t, `SELECT count(*) FROM elitea_runtime.execution_security_contexts WHERE execution_id = :'id';`,
			map[string]string{"id": result.ExecutionID}); n != 1 {
			t.Fatalf("the held run has %d security contexts", n)
		}
		w.cfg.updateState(t, func(st *state) {
			st.Held = &heldRun{Turn: *s.OldWorker, ExecutionID: result.ExecutionID, MessageID: result.ResponseMessageID, Marker: marker}
		})
	})
}

// TestGovernedRestored (phase restored: flag on, governed worker back) shows
// GOV-007's held run was held, not lost: the governed worker runs it. It
// then pauses one governed run on a HITL decision for the rollback test.
func TestGovernedRestored(t *testing.T) {
	ctx := testContext(t, 15*time.Minute)
	w := newWorld(ctx, t, phaseRestored)
	s := w.cfg.readState(t)

	t.Run("GOV-007/held_run_completes_on_the_governed_worker", func(t *testing.T) {
		if s.Held == nil {
			t.Fatal("state.json has no held run; phase oldworker must run first")
		}
		last := w.settled(ctx, t, w.user, s.Held.Turn, 5*time.Minute)
		if !strings.Contains(messageRaw(last), s.Held.Marker) {
			t.Fatalf("the held run did not complete on the governed worker: %s", messageRaw(last))
		}
	})

	t.Run("ROLLBACK/prepare_paused_governed_run", func(t *testing.T) {
		agent := w.governedAgent(ctx, t, agentSpec{Label: "rollback", MandateFor: []*persona{w.user}})
		agent.turnFixture = w.converse(ctx, t, w.user, agent.turnFixture)
		result := w.mustAdmit(ctx, t, w.user, agent.turnFixture,
			callTool("add_comments", `{"issue_key":"GOV-2","comment":"rollback"}`))
		paused := w.interrupt(ctx, t, w.user, agent.turnFixture, result.ResponseMessageID)
		paused.ExecutionID = result.ExecutionID
		w.cfg.updateState(t, func(st *state) { st.Paused = &paused })
	})
}

// TestGovernedRollback (phase rollback: flag off again, elitea-main
// restarted) is the rollback test of #1124: the legacy path works again, and
// a run pinned to the governed profile does NOT downgrade: resumed after the
// flag went off, it never reaches the provider with any credential.
func TestGovernedRollback(t *testing.T) {
	ctx := testContext(t, 15*time.Minute)
	w := newWorld(ctx, t, phaseRollback)
	s := w.cfg.readState(t)
	if s.Legacy == nil || s.Paused == nil {
		t.Fatal("state.json lacks the legacy fixture or the paused run; phases off and restored must run first")
	}

	t.Run("ROLLBACK/governed_routes_are_gone", func(t *testing.T) {
		w.admin.do(ctx, t, http.MethodGet, "/api/v2/admin/agent_identities", nil, http.StatusNotFound)
	})

	t.Run("ROLLBACK/legacy_turn_works", func(t *testing.T) {
		before := len(w.cfg.journal(ctx, t, "jira.governed.test"))
		w.mustAdmit(ctx, t, w.user, *s.Legacy, callTool("list_projects", ""))
		last := w.settled(ctx, t, w.user, *s.Legacy, 4*time.Minute)
		if !strings.Contains(messageRaw(last), "GOVJIRASENTINEL") {
			t.Fatalf("the legacy turn did not reach the provider after rollback: %s", messageRaw(last))
		}
		var legacy int
		for _, e := range w.cfg.journal(ctx, t, "jira.governed.test")[before:] {
			if e.CredentialSHA256 == s.Legacy.CredentialSHA256 {
				legacy++
			}
		}
		if legacy == 0 {
			t.Fatal("the provider never saw the legacy token after rollback")
		}
	})

	t.Run("ROLLBACK/pinned_governed_run_does_not_downgrade", func(t *testing.T) {
		paused := *s.Paused
		if profile := w.admissionProfile(ctx, t, paused.ExecutionID); profile != governedProfile {
			t.Fatalf("the paused run's pin changed to %s", profile)
		}
		before := len(w.cfg.journal(ctx, t, "jira.governed.test"))
		answer := w.resume(ctx, t, w.user, paused)
		if answer.Status == http.StatusOK {
			w.settled(ctx, t, w.user, paused.Turn, 4*time.Minute)
		}
		for _, e := range w.cfg.journal(ctx, t, "jira.governed.test")[before:] {
			if e.Method == http.MethodPost && strings.Contains(e.Path, "/comment") {
				t.Fatalf("a governed run reached the provider after rollback: %+v", e)
			}
		}
		if n := w.redemptions(ctx, t, paused.ExecutionID); n != 0 {
			t.Fatalf("the pinned run redeemed %d credentials after rollback", n)
		}
		if profile := w.admissionProfile(ctx, t, paused.ExecutionID); profile != governedProfile {
			t.Fatalf("the paused run was re-pinned to %s", profile)
		}
	})
}
