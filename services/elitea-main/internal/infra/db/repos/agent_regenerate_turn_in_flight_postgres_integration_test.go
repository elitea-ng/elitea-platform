package repos

// Regenerating an answer while ANOTHER turn of the conversation is still
// being written (#3137).
//
// Run to completion (ADR-0045) is on by default. When the goal check of the
// first turn finds the goal not met, the chain appends an automatic
// continuation: a new question and a new answer that streams. A regeneration
// of the FIRST answer sent in that window was refused 422
// `unsupported_agent_execution`, with no reason. ResolveCurrentRegeneration
// admitted it, and the turn resolve after it (ResolveCurrentAdhocTurn /
// ResolveCurrentApplicationTurn) matched no row because of its overlap gate.
// The window is a wait, not a refusal: the regeneration resolve now names it
// with ErrConversationTurnInFlight, which the route answers as the retryable
// 409 `agent_regeneration_pending`.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenant"
	"github.com/jackc/pgx/v5"
)

const (
	inFlightContinuationQuestion     = "20000000-0000-4000-8000-000000003137"
	inFlightContinuationQuestionItem = "30000000-0000-4000-8000-000000003137"
	inFlightContinuationResponse     = "40000000-0000-4000-8000-000000003137"
)

func TestPostgresRegenerationWhileAnotherTurnStreamsIsRetryable(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// 1. The first turn, answered and settled.
	commitAnsweredTurnForRegeneration(t, pool, "The first answer.")

	// 2. The automatic continuation: a later question and an answer that is
	//    still being written.
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.BindProject(ctx, tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	continuation := agentexecutionapp.CurrentApplicationTurn{
		ProjectID: 1, ActorUserID: 11, TargetParticipantID: 21,
		ApplicationID: 31, ApplicationVersionID: 41,
		ConversationUUID: regenCanvasConversation, QuestionID: inFlightContinuationQuestion,
		QuestionItemID: inFlightContinuationQuestionItem, ResponseMessageID: inFlightContinuationResponse,
		QuestionMeta: json.RawMessage(`{"continued_automatically":true}`),
		UserInput:    "Continue from where you stopped.",
	}
	if err := insertCurrentApplicationTurn(ctx, sqlcgen.New(tx), "execution-continuation-3137", continuation, 1); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	startRepo, err := NewCurrentAgentStartRepository(pool, 1)
	if err != nil {
		t.Fatal(err)
	}
	regenerateFirst := agentexecutionapp.CurrentRegenerationResolveRequest{
		ProjectID: 1, ActorUserID: 11, ResponseMessageID: regenCanvasResponse,
	}

	// 3. While the continuation streams, the regeneration of the first answer
	//    is the named, retryable wait.
	_, err = startRepo.ResolveCurrentRegeneration(ctx, regenerateFirst)
	if !errors.Is(err, agentexecutionapp.ErrConversationTurnInFlight) {
		t.Fatalf("regeneration during a streaming later turn: error=%v, want ErrConversationTurnInFlight", err)
	}

	// 4. The streaming answer itself is still the finalizing case, not this one.
	_, err = startRepo.ResolveCurrentRegeneration(ctx, agentexecutionapp.CurrentRegenerationResolveRequest{
		ProjectID: 1, ActorUserID: 11, ResponseMessageID: inFlightContinuationResponse,
	})
	if !errors.Is(err, agentexecutionapp.ErrCurrentAgentRegenerationStillFinalizing) {
		t.Fatalf("regeneration of the streaming answer: error=%v, want ErrCurrentAgentRegenerationStillFinalizing", err)
	}

	// 5. When the continuation settles, the same regeneration resolves.
	settle, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = settle.Rollback(context.Background()) }()
	if err := tenant.BindProject(ctx, settle, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	completePostgresCurrentApplicationTurn(t, settle, mustCurrentPGUUID(t, inFlightContinuationResponse), "Done.")
	if err := settle.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	target, err := startRepo.ResolveCurrentRegeneration(ctx, regenerateFirst)
	if err != nil {
		t.Fatalf("regeneration after the continuation settled: %v", err)
	}
	if target.QuestionID != regenCanvasQuestion {
		t.Fatalf("target=%+v, want the first question", target)
	}
}
