package repos

import "testing"

// T-2A3 M1 (real PostgreSQL): a Map item's delegated sign-in card is decided
// and gated exactly as a Parallel member's. Raising it as a Map item (kind
// map, member = the item's decimal index), the wake that would apply an
// `authorize` decision is claimable only by a worker that opts in to
// fan-out delegated credentials (the claim folds this predicate into its
// first query, claims.go wakeCredentialsSQL), and that wake carries exactly
// the reference of the actor's own grant for this toolkit and resource.
func TestAMapItemAuthorizeIsGatedToAnOptedInWorkerAndCarriesOnlyItsGrant(t *testing.T) {
	h := newAuthorizeHarness(t)
	response := h.newResponse()
	key, raw := h.card(authorizeCard, func(card map[string]any) {
		fanout := card["fanout_v1"].(map[string]any)
		fanout["kind"], fanout["member"], fanout["ordinal"], fanout["total"] = "map", "0", 1, 3
		for _, tier := range card["parent_agent_path"].([]any) {
			tier.(map[string]any)["sibling_ordinal"] = 1
		}
	})
	if _, err := h.repo.Raise(t.Context(), h.raiseInput(response, raw)); err != nil {
		t.Fatalf("raise a Map item card: %v", err)
	}
	wakeExecution := h.executions[response]
	h.exec(`INSERT INTO elitea_runtime.execution_interrupt_wakes
  (root_response_id, decision_revision, project_id, parked_execution_id, parked_generation, idempotency_key, state, wake_execution_id)
VALUES ($1::uuid, 1, 1, $2, 1, 'continue-interrupt/' || $1 || '/1', 'ADMITTED', $2)`, response, wakeExecution)
	gated := func() bool {
		var value bool
		if err := h.pool.QueryRow(t.Context(), `SELECT `+wakeCredentialsSQL+`
FROM elitea_runtime.execution_jobs j WHERE j.execution_id = $1 AND j.generation = 1`, wakeExecution).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	if gated() {
		t.Fatal("a Map item wake with no decision is gated")
	}
	reference := h.grant(1, int32(interruptAsker), authorizeToolkit, authorizeResource)
	h.grant(1, int32(interruptAsker), authorizeToolkit, "https://mcp.example.invalid/other")
	if err := h.authorize(interruptAsker, response, key, reference); err != nil {
		t.Fatalf("authorize the Map item card: %v", err)
	}
	if !gated() {
		t.Fatal("a Map item wake with an authorize decision is claimable without the opt-in")
	}
	references, err := h.wakeReferences(response)
	if err != nil {
		t.Fatal(err)
	}
	if len(references) != 1 {
		t.Fatalf("the Map item wake carries %d references, want exactly the decided grant", len(references))
	}
}
