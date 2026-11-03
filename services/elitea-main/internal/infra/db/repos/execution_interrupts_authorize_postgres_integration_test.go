package repos

import (
	"bytes"

	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	domain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/executioninterrupt"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/mcpoauth"
)

// FU-250: a fan-out `authorize` against the real delegated grant store. The
// harness's run is executed by interruptAsker (execution_jobs.actor_id '8');
// interruptOwner owns the conversation and may decide other cards.

const (
	authorizeCard     = "fanout-interrupt-card-v1.delegated-auth-toolkit.json"
	authorizeToolkit  = int64(17)
	authorizeResource = "https://mcp.atlassian.com/v1/sse"
)

type authorizeHarness struct {
	*interruptHarness
	tokens *mcpoauth.Tokens
}

func newAuthorizeHarness(t *testing.T) *authorizeHarness {
	t.Helper()
	h := newInterruptHarness(t)
	tokens, err := mcpoauth.NewTokens(h.pool, bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	h.repo.SetDelegatedTokens(tokens)
	return &authorizeHarness{interruptHarness: h, tokens: tokens}
}

// grant saves a delegated grant and returns its opaque reference.
func (h *authorizeHarness) grant(projectID, actorID int32, toolkitID int64, resource string) string {
	h.t.Helper()
	reference, err := h.tokens.Save(h.t.Context(),
		mcpoauth.TokenBinding{ProjectID: projectID, ActorID: actorID, ToolkitID: toolkitID, Resource: resource},
		mcpoauth.AccessToken{AccessToken: "test-only-fu250-marker", TokenType: "Bearer"}, time.Now().Add(time.Hour))
	if err != nil {
		h.t.Fatalf("save grant: %v", err)
	}
	return reference.Reference
}

// wakeReferences reads the references a wake of response would carry, in
// the response's project transaction as wake admission does.
func (h *authorizeHarness) wakeReferences(response string) ([]*runtimev1.McpTokenReferenceV1, error) {
	var references []*runtimev1.McpTokenReferenceV1
	err := h.repo.projects.WithinProjectTx(h.t.Context(), 1, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx sqlExecutor) error {
		var err error
		references, err = wakeDelegatedCredentials(h.t.Context(), tx, response, 1)
		return err
	})
	return references, err
}

func (h *authorizeHarness) authorize(actor int64, response, key, reference string) error {
	h.t.Helper()
	_, canonical := decisionBody(h.t, "authorize-"+key+reference, "authorize", "", reference)
	_, err := h.repo.Decide(h.t.Context(), domain.DecideInput{
		Selector: domain.Selector{ProjectID: 1, ActorUserID: actor, ResponseMessageID: response}, InterruptKey: key, Canonical: canonical,
	})
	return err
}

// cardRow is everything a decision would write on the card and its response.
func (h *authorizeHarness) cardRow(response, key string) string {
	h.t.Helper()
	var row string
	if err := h.pool.QueryRow(h.t.Context(), `
SELECT card.state || ':' || card.revision || ':' || COALESCE(card.request_id, '-') || ':' ||
       COALESCE(encode(card.decision_json, 'hex'), '-') || ':' || COALESCE(card.decided_by::text, '-') || ':' ||
       response.decision_revision || ':' ||
       (SELECT count(*) FROM elitea_runtime.execution_interrupt_audit audit
        WHERE audit.root_response_id = card.root_response_id AND audit.transition = 'DECIDED')
FROM elitea_runtime.execution_interrupts card
JOIN elitea_runtime.execution_interrupt_responses response ON response.root_response_id = card.root_response_id
WHERE card.root_response_id = $1::uuid AND card.interrupt_key = $2`, response, key).Scan(&row); err != nil {
		h.t.Fatal(err)
	}
	return row
}

// The authorization matrix of `authorize` (gate §3a, E4): only the run's
// actor with a grant of their own for exactly this toolkit and resource is
// accepted; every other credential is the same typed refusal and writes
// nothing (the card stays PENDING at its revision, no audit, no bump).
func TestAuthorizeAcceptsOnlyTheRunActorsOwnGrantForThisCard(t *testing.T) {
	h := newAuthorizeHarness(t)
	foreignActor := h.grant(1, int32(interruptOwner), authorizeToolkit, authorizeResource)
	otherToolkit := h.grant(1, int32(interruptAsker), authorizeToolkit+1, authorizeResource)
	otherResource := h.grant(1, int32(interruptAsker), authorizeToolkit, "https://mcp.other.example/v1/sse")
	otherProject := h.grant(2, int32(interruptAsker), authorizeToolkit, authorizeResource)
	revoked := h.grant(1, int32(interruptAsker), authorizeToolkit, authorizeResource)
	if err := h.tokens.Revoke(t.Context(), revoked, mcpoauth.TokenBinding{
		ProjectID: 1, ActorID: int32(interruptAsker), ToolkitID: authorizeToolkit, Resource: authorizeResource,
	}); err != nil {
		t.Fatal(err)
	}
	expired := h.grant(1, int32(interruptAsker), authorizeToolkit, authorizeResource)
	h.exec(`UPDATE elitea_auth.mcp_oauth_tokens SET expires_at = now() - interval '1 second' WHERE id = $1`, expired)
	unknown := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

	refusals := []struct {
		name      string
		actor     int64
		reference string
	}{
		{"another user's grant", interruptAsker, foreignActor},
		{"another toolkit's grant", interruptAsker, otherToolkit},
		{"another resource's grant", interruptAsker, otherResource},
		{"another project's grant", interruptAsker, otherProject},
		{"a revoked grant", interruptAsker, revoked},
		{"an expired grant", interruptAsker, expired},
		{"an unknown reference", interruptAsker, unknown},
		{"a member who is not the run's actor, with their own grant", interruptOwner, foreignActor},
	}
	for _, refusal := range refusals {
		response := h.newResponse()
		key := h.raise(response, authorizeCard)
		before := h.cardRow(response, key)
		err := h.authorize(refusal.actor, response, key, refusal.reference)
		if !errors.Is(err, domain.ErrCredentialRefused) {
			t.Fatalf("%s: %v, want the one typed refusal", refusal.name, err)
		}
		if after := h.cardRow(response, key); after != before {
			t.Fatalf("%s: a refusal wrote %s (was %s)", refusal.name, after, before)
		}
	}

	response := h.newResponse()
	key := h.raise(response, authorizeCard)
	own := h.grant(1, int32(interruptAsker), authorizeToolkit, authorizeResource)
	if err := h.authorize(interruptAsker, response, key, own); err != nil {
		t.Fatalf("the run's actor with their own grant: %v", err)
	}
}

// A refusal leaves the card decidable at the same revision (E4).
func TestARefusedAuthorizeLeavesTheCardDecidable(t *testing.T) {
	h := newAuthorizeHarness(t)
	response := h.newResponse()
	key := h.raise(response, authorizeCard)
	if err := h.authorize(interruptAsker, response, key, h.grant(1, int32(interruptOwner), authorizeToolkit, authorizeResource)); !errors.Is(err, domain.ErrCredentialRefused) {
		t.Fatal(err)
	}
	if err := h.authorize(interruptAsker, response, key, h.grant(1, int32(interruptAsker), authorizeToolkit, authorizeResource)); err != nil {
		t.Fatalf("a valid authorize at the same expected revision: %v", err)
	}
}

// A card raised before cards named their toolkit stays skip-only (M2), and
// without the grant store every authorize is refused.
func TestAnOlderCardOrNoGrantStoreRefusesAuthorize(t *testing.T) {
	h := newAuthorizeHarness(t)
	response := h.newResponse()
	key := h.raise(response, "fanout-interrupt-card-v1.delegated-auth.json")
	if err := h.authorize(interruptAsker, response, key, h.grant(1, int32(interruptAsker), authorizeToolkit, authorizeResource)); !errors.Is(err, domain.ErrAuthorizeUnavailable) {
		t.Fatalf("older card: %v", err)
	}
	if _, err := h.decide(interruptAsker, response, key, "older-skip", "skip", ""); err != nil {
		t.Fatalf("skip on an older card: %v", err)
	}

	bare := newInterruptHarness(t)
	response = bare.newResponse()
	key = bare.raise(response, authorizeCard)
	_, canonical := decisionBody(t, "no-store", "authorize", "", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	_, err := bare.repo.Decide(t.Context(), domain.DecideInput{
		Selector: domain.Selector{ProjectID: 1, ActorUserID: interruptAsker, ResponseMessageID: response}, InterruptKey: key, Canonical: canonical,
	})
	if !errors.Is(err, domain.ErrCredentialRefused) {
		t.Fatalf("no grant store: %v", err)
	}
}

// D2: List offers authorize only to the run's actor, and only on a card
// that names its toolkit; no viewer is shown any grant.
func TestListOffersAuthorizeOnlyToTheRunActor(t *testing.T) {
	h := newAuthorizeHarness(t)
	response := h.newResponse()
	h.raise(response, authorizeCard)
	h.raise(response, "fanout-interrupt-card-v1.delegated-auth.json")
	offered := func(viewer int64) []bool {
		listed, err := h.repo.List(t.Context(), domain.Selector{ProjectID: 1, ActorUserID: viewer, ResponseMessageID: response})
		if err != nil {
			t.Fatal(err)
		}
		var result []bool
		for _, item := range listed.Interrupts {
			var card struct {
				Actions []string `json:"available_actions"`
				Display struct {
					ToolkitID int64 `json:"toolkit_id"`
				} `json:"display"`
			}
			if err := json.Unmarshal(item.Card, &card); err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(item.Card, []byte("test-only-fu250-marker")) {
				t.Fatal("a listed card carries grant material")
			}
			result = append(result, slices.Contains(card.Actions, "authorize"))
		}
		return result
	}
	if got := offered(interruptAsker); !slices.Equal(got, []bool{true, false}) {
		t.Fatalf("the run's actor: %v, want authorize on the bound card only", got)
	}
	if got := offered(interruptOwner); !slices.Equal(got, []bool{false, false}) {
		t.Fatalf("another member: %v, want skip only", got)
	}
}

// E1: a wake carries references only for DECIDED (not consumed) authorize
// cards, bound to each card's toolkit and canonical resource.
func TestAWakeCarriesOnlyDecidedAuthorizeReferences(t *testing.T) {
	h := newAuthorizeHarness(t)
	response := h.newResponse()
	fence := h.bindLiveClaim(response)
	decided := h.raise(response, authorizeCard)
	own := h.grant(1, int32(interruptAsker), authorizeToolkit, authorizeResource)
	if err := h.authorize(interruptAsker, response, decided, own); err != nil {
		t.Fatal(err)
	}
	skipped := h.raise(response, "fanout-interrupt-card-v1.delegated-auth.json")
	if _, err := h.decide(interruptAsker, response, skipped, "wake-skip", "skip", ""); err != nil {
		t.Fatal(err)
	}
	references, err := h.wakeReferences(response)
	if err != nil {
		t.Fatal(err)
	}
	if len(references) != 1 || references[0].GetReference() != own || references[0].GetToolkitId() != uint64(authorizeToolkit) ||
		references[0].GetResource() != authorizeResource || references[0].GetRevision() != 1 {
		t.Fatalf("wake references: %v", references)
	}
	// The decided card is consumed through the claim-fenced ACK.
	fetch, err := h.repo.FetchDecided(t.Context(), fence)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := "ckpt-fu250"
	for _, entry := range fetch.Decisions {
		if entry.InterruptKey != decided {
			continue
		}
		if _, err := h.ack(fence, domain.Ack{InterruptKey: decided, RequestID: entry.RequestID, Revision: entry.Revision,
			DecisionSHA256: entry.DecisionSHA256, Outcome: domain.AckApplied, ChildCheckpointID: &checkpoint}); err != nil {
			t.Fatal(err)
		}
	}
	if references, err = h.wakeReferences(response); err != nil || len(references) != 0 {
		t.Fatalf("a consumed card still adds a reference: %v %v", references, err)
	}
}

// M1: the claim gate's predicate is true exactly for a wake whose response
// holds an authorize decision.
func TestTheWakeCredentialPredicateSeesAnAuthorizeDecision(t *testing.T) {
	h := newAuthorizeHarness(t)
	response := h.newResponse()
	key := h.raise(response, authorizeCard)
	wakeExecution := h.executions[response]
	h.exec(`INSERT INTO elitea_runtime.execution_interrupt_wakes
  (root_response_id, decision_revision, project_id, parked_execution_id, parked_generation, idempotency_key, state, wake_execution_id)
VALUES ($1::uuid, 1, 1, $2, 1, 'continue-interrupt/' || $1 || '/1', 'ADMITTED', $2)`, response, wakeExecution)
	carries := func() bool {
		var value bool
		if err := h.pool.QueryRow(t.Context(), `SELECT `+wakeCredentialsSQL+`
FROM elitea_runtime.execution_jobs j WHERE j.execution_id = $1 AND j.generation = 1`, wakeExecution).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	if carries() {
		t.Fatal("a wake with no decision is gated")
	}
	if err := h.authorize(interruptAsker, response, key, h.grant(1, int32(interruptAsker), authorizeToolkit, authorizeResource)); err != nil {
		t.Fatal(err)
	}
	if !carries() {
		t.Fatal("a wake with an authorize decision is not gated")
	}
}

// FU-250 (A2): a card naming a toolkit of another project, or a project
// toolkit whose grant is for another server than the card's server_url,
// gets the same refusal: the grant must be bound to this project, this
// toolkit and the canonical resource of the card's own server.
func TestAuthorizeRefusesAnotherProjectsToolkitAndAnotherServer(t *testing.T) {
	h := newAuthorizeHarness(t)
	const foreignToolkit = int64(99)
	foreignGrant := h.grant(2, int32(interruptAsker), foreignToolkit, authorizeResource)
	response := h.newResponse()
	key, raw := h.card(authorizeCard, func(card map[string]any) {
		card["display"].(map[string]any)["toolkit_id"] = foreignToolkit
	})
	if _, err := h.repo.Raise(t.Context(), h.raiseInput(response, raw)); err != nil {
		t.Fatal(err)
	}
	before := h.cardRow(response, key)
	if err := h.authorize(interruptAsker, response, key, foreignGrant); !errors.Is(err, domain.ErrCredentialRefused) {
		t.Fatalf("another project's toolkit: %v", err)
	}
	if after := h.cardRow(response, key); after != before {
		t.Fatalf("a refusal wrote %s", after)
	}

	otherServer := h.grant(1, int32(interruptAsker), authorizeToolkit, "https://mcp.other.example/v1/sse")
	response = h.newResponse()
	key = h.raise(response, authorizeCard)
	if err := h.authorize(interruptAsker, response, key, otherServer); !errors.Is(err, domain.ErrCredentialRefused) {
		t.Fatalf("a grant for another server than the card's: %v", err)
	}
}
