package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"time"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type CurrentAgentStartRepository struct {
	projects projectStore
}

func NewCurrentAgentStartRepository(pool *pgxpool.Pool) (*CurrentAgentStartRepository, error) {
	projects, err := newPostgresProjectStore(pool)
	if err != nil {
		return nil, err
	}
	return newCurrentAgentStartRepository(projects)
}

func newCurrentAgentStartRepository(projects projectStore) (*CurrentAgentStartRepository, error) {
	if projects == nil {
		return nil, errors.New("current agent project database is required")
	}
	return &CurrentAgentStartRepository{projects: projects}, nil
}

type currentApplicationStartQuerier interface {
	ResolveCurrentApplicationTurn(
		context.Context,
		sqlcgen.ResolveCurrentApplicationTurnParams,
	) (sqlcgen.ResolveCurrentApplicationTurnRow, error)
}

type currentAdhocStartQuerier interface {
	ResolveCurrentAdhocTurn(
		context.Context,
		sqlcgen.ResolveCurrentAdhocTurnParams,
	) (sqlcgen.ResolveCurrentAdhocTurnRow, error)
}

type currentRegenerationQuerier interface {
	ResolveCurrentRegeneration(
		context.Context,
		sqlcgen.ResolveCurrentRegenerationParams,
	) (sqlcgen.ResolveCurrentRegenerationRow, error)
}

type currentContinuationQuerier interface {
	ResolveCurrentOutputLimitContinuation(
		context.Context,
		sqlcgen.ResolveCurrentOutputLimitContinuationParams,
	) (sqlcgen.ResolveCurrentOutputLimitContinuationRow, error)
	ResolveCurrentContinuation(
		context.Context,
		sqlcgen.ResolveCurrentContinuationParams,
	) (sqlcgen.ResolveCurrentContinuationRow, error)
	ResolveCurrentAuthorizationContinuation(
		context.Context,
		sqlcgen.ResolveCurrentAuthorizationContinuationParams,
	) (sqlcgen.ResolveCurrentAuthorizationContinuationRow, error)
}

type currentConversationSettlingQuerier interface {
	CurrentConversationResponseSettling(context.Context, pgtype.UUID) (bool, error)
}

// How long a start may wait for the PREVIOUS response in the same conversation
// to stop being marked as streaming, and how often it looks.
//
// THE WINDOW THIS CLOSES. A turn ends for the BROWSER on the `pipeline_finish`
// node event — `isTurnTerminalFrame` in
// apps/elitea-web/src/features/chat-messages/lib/chatStreamTurnEnd.ts — and
// `ChatBox` re-enables the composer there, because elitea-main never closes the
// event stream (internal/api/v2/executions/events.go loops on heartbeats), so
// the client has nothing later to wait for. The turn ends for the SERVER only
// when the worker's separate terminal output frame is projected and
// FinalizeCurrentAgentFullMessage clears `is_streaming`. Between the two, the
// overlap gate in ResolveCurrentAdhocTurn / ResolveCurrentApplicationTurn still
// matches, the resolve returns no rows, and the route answers 422
// `unsupported_agent_execution` — to a send the product had just invited.
//
// Measured on the standalone stack (chat.multiturn, 2026-08-29): durable
// `pipeline_finish` at 21:53:47.319, composer released ~21:53:47.55, second
// start POST at 21:53:47.621 refused 422, `is_streaming` cleared at
// 21:53:47.824. The window is structural and present on EVERY turn; only how
// fast the next send arrives decides whether anyone lands in it.
//
// THREE SECONDS, not "until it clears": a response that is genuinely still
// being written must still be refused, and this must not become an unbounded
// hold on a request goroutine. Six times the measured gap, then the original
// classification stands unchanged.
const (
	currentAgentSettleBudget   = 3 * time.Second
	currentAgentSettleInterval = 100 * time.Millisecond
)

// currentResponseSettling reports whether the conversation still carries a
// response row marked as being written. Its own short read-only transaction:
// the resolve's transaction has already ended, and holding one open across a
// sleep is exactly what this must not do.
func (repository *CurrentAgentStartRepository) currentResponseSettling(
	ctx context.Context,
	projectID int64,
	conversationUUID pgtype.UUID,
) (bool, error) {
	settling := false
	err := repository.projects.WithinProjectTx(
		ctx,
		projectID,
		pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly},
		func(tx sqlExecutor) error {
			queries, ok := tx.(currentConversationSettlingQuerier)
			if !ok {
				return errors.New("current conversation settling query is unavailable")
			}
			value, queryErr := queries.CurrentConversationResponseSettling(ctx, conversationUUID)
			if queryErr != nil {
				return queryErr
			}
			settling = value
			return nil
		},
	)
	return settling, err
}

// resolveAfterCurrentResponseSettles runs `resolve` and, ONLY when it answered
// the unsupported classification AND the conversation still holds a streaming
// response, retries it within `currentAgentSettleBudget`.
//
// The narrowing matters. `resolve` collapses about twenty-five different
// refusals into one error, and all but this one are static conversation or
// participant configuration that no amount of waiting changes: a start that is
// genuinely unsupported never SLEEPS here — it is refused at the first probe,
// which finds nothing settling, and answered from the confirming read that
// probe demands (below). A probe that itself fails is treated as "do not wait"
// — it is advisory, and the original refusal is the answer.
//
// ── A SETTLE REPORT IS A REASON TO RE-READ, NOT A REASON TO GIVE UP ──────────
//
// The loop below never returns a refusal it read BEFORE the probe told it the
// response had settled. That is not defensive tidying; it is the whole
// correctness of this wait.
//
// A resolve answers from the snapshot its statement took. The probe that
// follows it is a separate, later transaction. When the worker's terminal
// projection commits BETWEEN the two, the probe truthfully reports "settled"
// while the refusal in hand was produced by a read that could not yet see it —
// and returning that refusal answers 422 to a turn the very next read admits.
//
// That window is not theoretical and it is not rare where it matters. After a
// HITL resume the runtime's `pipeline_finish` and its terminal frame are
// milliseconds apart (an ordinary turn leaves ~500ms between them), so the
// composer is released essentially AT the terminal write and the next send
// lands squarely on it. Measured on the standalone stack before this change
// (conversation 329, 2026-08-29): terminal write stamped 23:27:44.316, the 422
// logged at 23:27:44.328 — 12ms later, with the same resolve returning a row
// when replayed by hand. Every conversation that had answered an `ask_user`
// question or decided a sensitive-tool pause refused its NEXT send, which is
// how a paused conversation became a dead one.
//
// So `!settling` re-resolves once and answers with THAT. There is nothing left
// to wait for at that point — the state the gate objected to is gone — which is
// why this branch returns rather than looping. The extra query is paid only on
// a start that was already being refused.
func (repository *CurrentAgentStartRepository) resolveAfterCurrentResponseSettles(
	ctx context.Context,
	projectID int64,
	conversationUUID pgtype.UUID,
	resolve func() error,
) error {
	err := resolve()
	if !errors.Is(err, agentexecutionapp.ErrUnsupportedCurrentAgentStart) {
		return err
	}
	deadline := time.Now().Add(currentAgentSettleBudget)
	for time.Now().Before(deadline) {
		settling, probeErr := repository.currentResponseSettling(ctx, projectID, conversationUUID)
		if probeErr != nil {
			return err
		}
		if !settling {
			// Whether the conversation settled just now or was never streaming
			// at all, only a read taken AFTER this probe can say — and the
			// refusal in hand was taken before it.
			return resolve()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(currentAgentSettleInterval):
		}
		if retried := resolve(); !errors.Is(retried, agentexecutionapp.ErrUnsupportedCurrentAgentStart) {
			return retried
		}
	}
	return err
}

func (repository *CurrentAgentStartRepository) ResolveCurrentApplication(
	ctx context.Context,
	request agentexecutionapp.CurrentApplicationStartRequest,
) (agentexecutionapp.CurrentApplicationTarget, error) {
	if err := request.Validate(); err != nil {
		return agentexecutionapp.CurrentApplicationTarget{}, agentexecutionapp.ErrInvalidCurrentAgentStart
	}
	projectID, projectIDValid := currentAgentDatabaseID(request.ProjectID)
	targetParticipantID, targetParticipantIDValid := currentAgentDatabaseID(request.TargetParticipantID)
	if !projectIDValid || !targetParticipantIDValid {
		return agentexecutionapp.CurrentApplicationTarget{}, agentexecutionapp.ErrInvalidCurrentAgentStart
	}
	conversationUUID, err := currentPGUUID(request.ConversationUUID)
	if err != nil {
		return agentexecutionapp.CurrentApplicationTarget{}, agentexecutionapp.ErrInvalidCurrentAgentStart
	}
	questionID, err := currentPGUUID(request.QuestionID)
	if err != nil {
		return agentexecutionapp.CurrentApplicationTarget{}, agentexecutionapp.ErrInvalidCurrentAgentStart
	}
	var target agentexecutionapp.CurrentApplicationTarget
	resolve := func() error {
		target = agentexecutionapp.CurrentApplicationTarget{}
		return repository.projects.WithinProjectTx(
			ctx,
			request.ProjectID,
			pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly},
			func(tx sqlExecutor) error {
				queries, ok := tx.(currentApplicationStartQuerier)
				if !ok {
					return errors.New("current agent start query is unavailable")
				}
				nesting, ok := tx.(currentApplicationNestingQuerier)
				if !ok {
					return errors.New("current application nesting query is unavailable")
				}
				row, queryErr := queries.ResolveCurrentApplicationTurn(
					ctx,
					sqlcgen.ResolveCurrentApplicationTurnParams{
						ActorUserID:         request.ActorUserID,
						TargetParticipantID: targetParticipantID,
						QuestionID:          questionID,
						ConversationUuid:    conversationUUID,
						ProjectID:           projectID,
					},
				)
				if errors.Is(queryErr, pgx.ErrNoRows) {
					// EVERY join in ResolveCurrentApplicationTurn can produce
					// this one empty result, and they fail for very different
					// operator-visible reasons. The support assistant spent a
					// release answering 502 here because its conversations
					// carried no `user` participant and its agent mapping
					// carried no `entity_settings.version_id`; the log said
					// only "not supported by the admitted parity slice", so
					// neither cause was visible. The reason below names the
					// joins so the next such failure is readable.
					return agentexecutionapp.UnsupportedCurrentAgentStart(
						"application turn row not found: the conversation, its " +
							"entity_name='user' participant for the caller, the target " +
							"application participant, or the application_versions row named " +
							"by that participant's entity_settings.version_id is missing")
				}
				if queryErr != nil {
					return fmt.Errorf("resolve current application turn: %w", queryErr)
				}
				variables := json.RawMessage(row.ApplicationVariablesJson)
				versionDetails := json.RawMessage(row.ApplicationVersionDetailsJson)
				chatHistory := json.RawMessage(row.ChatHistoryJson)
				internalTools := json.RawMessage(row.InternalToolsJson)
				if int64(row.ApplicationProjectID) != request.ProjectID {
					return agentexecutionapp.UnsupportedCurrentAgentStart(
						"the target participant's entity_meta.project_id is not the project " +
							"the turn runs in; a turn reads application_versions from its own " +
							"tenant schema, so the agent must live in that project")
				}
				if row.ApplicationID <= 0 {
					return agentexecutionapp.UnsupportedCurrentAgentStart(
						"the target participant carries no entity_meta.id")
				}
				if row.ApplicationVersionID <= 0 {
					return agentexecutionapp.UnsupportedCurrentAgentStart(
						"the target participant mapping carries no entity_settings.version_id")
				}
				if !json.Valid(variables) || !json.Valid(versionDetails) ||
					!json.Valid(chatHistory) || !json.Valid(internalTools) {
					return agentexecutionapp.UnsupportedCurrentAgentStart(
						"the resolved turn carries a malformed JSON projection")
				}
				if validationErr := validateCurrentApplicationNesting(
					ctx,
					nesting,
					row.ApplicationVersionID,
					1,
				); validationErr != nil {
					if contextErr := ctx.Err(); contextErr != nil {
						return contextErr
					}
					if errors.Is(validationErr, errInvalidCurrentApplicationNesting) {
						return agentexecutionapp.ErrUnsupportedCurrentAgentStart
					}
					return fmt.Errorf("validate current application nesting: %w", validationErr)
				}
				versionDetails, queryErr = materializeCurrentApplicationVersionNestedSkills(
					ctx,
					nesting,
					versionDetails,
				)
				if queryErr != nil {
					if contextErr := ctx.Err(); contextErr != nil {
						return contextErr
					}
					if errors.Is(queryErr, errInvalidCurrentApplicationNesting) {
						return agentexecutionapp.ErrUnsupportedCurrentAgentStart
					}
					return fmt.Errorf("materialize current nested skills: %w", queryErr)
				}
				target = agentexecutionapp.CurrentApplicationTarget{
					ApplicationID:        int64(row.ApplicationID),
					ApplicationVersionID: int64(row.ApplicationVersionID),
					Variables:            variables,
					VersionDetails:       versionDetails,
					ChatHistory:          chatHistory,
					InternalTools:        internalTools,
				}
				return nil
			},
		)
	}
	if err := repository.resolveAfterCurrentResponseSettles(
		ctx, request.ProjectID, conversationUUID, resolve,
	); err != nil {
		return agentexecutionapp.CurrentApplicationTarget{}, err
	}
	return target, nil
}

func (repository *CurrentAgentStartRepository) ResolveCurrentAdhoc(
	ctx context.Context,
	request agentexecutionapp.CurrentAdhocStartRequest,
) (agentexecutionapp.CurrentAdhocTarget, error) {
	if err := request.Validate(); err != nil {
		return agentexecutionapp.CurrentAdhocTarget{}, agentexecutionapp.ErrInvalidCurrentAgentStart
	}
	projectID, projectIDValid := currentAgentDatabaseID(request.ProjectID)
	if !projectIDValid || request.TargetParticipantID > math.MaxInt32 {
		return agentexecutionapp.CurrentAdhocTarget{}, agentexecutionapp.ErrInvalidCurrentAgentStart
	}
	conversationUUID, err := currentPGUUID(request.ConversationUUID)
	if err != nil {
		return agentexecutionapp.CurrentAdhocTarget{}, agentexecutionapp.ErrInvalidCurrentAgentStart
	}
	questionID, err := currentPGUUID(request.QuestionID)
	if err != nil {
		return agentexecutionapp.CurrentAdhocTarget{}, agentexecutionapp.ErrInvalidCurrentAgentStart
	}
	var target agentexecutionapp.CurrentAdhocTarget
	resolve := func() error {
		target = agentexecutionapp.CurrentAdhocTarget{}
		return repository.projects.WithinProjectTx(
			ctx,
			request.ProjectID,
			pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly},
			func(tx sqlExecutor) error {
				queries, ok := tx.(currentAdhocStartQuerier)
				if !ok {
					return errors.New("current agent start query is unavailable")
				}
				nesting, ok := tx.(currentApplicationNestingQuerier)
				if !ok {
					return errors.New("current application nesting query is unavailable")
				}
				row, queryErr := queries.ResolveCurrentAdhocTurn(
					ctx,
					sqlcgen.ResolveCurrentAdhocTurnParams{
						ActorUserID: request.ActorUserID, TargetParticipantID: int32(request.TargetParticipantID),
						ProjectID: projectID, QuestionID: questionID, ConversationUuid: conversationUUID,
					},
				)
				if errors.Is(queryErr, pgx.ErrNoRows) {
					return agentexecutionapp.ErrUnsupportedCurrentAgentStart
				}
				if queryErr != nil {
					return fmt.Errorf("resolve current ad-hoc turn: %w", queryErr)
				}
				llmSettings := json.RawMessage(row.LlmSettingsJson)
				tools := json.RawMessage(row.ToolsJson)
				chatHistory := json.RawMessage(row.ChatHistoryJson)
				conversationMeta := json.RawMessage(row.ConversationMetaJson)
				if row.TargetParticipantID <= 0 ||
					(request.TargetParticipantID > 0 && int64(row.TargetParticipantID) != request.TargetParticipantID) ||
					!json.Valid(llmSettings) || !json.Valid(tools) || !json.Valid(chatHistory) ||
					!json.Valid(conversationMeta) {
					return agentexecutionapp.ErrUnsupportedCurrentAgentStart
				}
				tools, queryErr = filterCurrentAdhocApplicationNesting(ctx, nesting, tools)
				if queryErr != nil {
					if contextErr := ctx.Err(); contextErr != nil {
						return contextErr
					}
					if errors.Is(queryErr, errInvalidCurrentApplicationNesting) {
						return agentexecutionapp.ErrUnsupportedCurrentAgentStart
					}
					return fmt.Errorf("validate current ad-hoc application nesting: %w", queryErr)
				}
				target = agentexecutionapp.CurrentAdhocTarget{
					TargetParticipantID: int64(row.TargetParticipantID),
					LLMSettings:         llmSettings, Instructions: row.Instructions,
					Tools: tools, ChatHistory: chatHistory, ConversationMeta: conversationMeta,
				}
				return nil
			},
		)
	}
	if err := repository.resolveAfterCurrentResponseSettles(
		ctx, request.ProjectID, conversationUUID, resolve,
	); err != nil {
		return agentexecutionapp.CurrentAdhocTarget{}, err
	}
	return target, nil
}

func (repository *CurrentAgentStartRepository) ResolveCurrentRegeneration(
	ctx context.Context,
	request agentexecutionapp.CurrentRegenerationResolveRequest,
) (agentexecutionapp.CurrentRegenerationTarget, error) {
	if err := request.Validate(); err != nil {
		return agentexecutionapp.CurrentRegenerationTarget{}, agentexecutionapp.ErrInvalidCurrentAgentStart
	}
	projectID, projectIDValid := currentAgentDatabaseID(request.ProjectID)
	if !projectIDValid {
		return agentexecutionapp.CurrentRegenerationTarget{}, agentexecutionapp.ErrInvalidCurrentAgentStart
	}
	responseMessageID, err := currentPGUUID(request.ResponseMessageID)
	if err != nil {
		return agentexecutionapp.CurrentRegenerationTarget{}, agentexecutionapp.ErrInvalidCurrentAgentStart
	}
	var target agentexecutionapp.CurrentRegenerationTarget
	err = repository.projects.WithinProjectTx(
		ctx,
		request.ProjectID,
		pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly},
		func(tx sqlExecutor) error {
			queries, ok := tx.(currentRegenerationQuerier)
			if !ok {
				return errors.New("current agent regeneration query is unavailable")
			}
			row, queryErr := queries.ResolveCurrentRegeneration(
				ctx,
				sqlcgen.ResolveCurrentRegenerationParams{
					ActorUserID: request.ActorUserID, ProjectID: projectID,
					ResponseMessageID: responseMessageID,
				},
			)
			if errors.Is(queryErr, pgx.ErrNoRows) {
				return agentexecutionapp.ErrUnsupportedCurrentAgentStart
			}
			if queryErr != nil {
				return fmt.Errorf("resolve current agent regeneration: %w", queryErr)
			}
			if row.ResponseIsStreaming {
				return agentexecutionapp.ErrCurrentAgentRegenerationStillFinalizing
			}
			target = agentexecutionapp.CurrentRegenerationTarget{
				Kind:                agentexecutionapp.CurrentRegenerationKind(row.RegenerationKind),
				ConversationUUID:    uuid.UUID(row.ConversationUuid.Bytes).String(),
				TargetParticipantID: int64(row.TargetParticipantID),
				QuestionID:          uuid.UUID(row.QuestionID.Bytes).String(), UserInput: row.UserInput,
			}
			if err := target.Validate(); err != nil {
				return agentexecutionapp.ErrUnsupportedCurrentAgentStart
			}
			return nil
		},
	)
	if err != nil {
		return agentexecutionapp.CurrentRegenerationTarget{}, err
	}
	return target, nil
}

func (repository *CurrentAgentStartRepository) ResolveCurrentContinuation(
	ctx context.Context,
	request agentexecutionapp.CurrentContinuationResolveRequest,
) (agentexecutionapp.CurrentContinuationTarget, error) {
	if err := request.Validate(); err != nil {
		return agentexecutionapp.CurrentContinuationTarget{}, agentexecutionapp.ErrInvalidCurrentAgentStart
	}
	projectID, projectIDValid := currentAgentDatabaseID(request.ProjectID)
	if !projectIDValid {
		return agentexecutionapp.CurrentContinuationTarget{}, agentexecutionapp.ErrInvalidCurrentAgentStart
	}
	conversationUUID, err := currentPGUUID(request.ConversationUUID)
	if err != nil {
		return agentexecutionapp.CurrentContinuationTarget{}, agentexecutionapp.ErrInvalidCurrentAgentStart
	}
	responseMessageID, err := currentPGUUID(request.ResponseMessageID)
	if err != nil {
		return agentexecutionapp.CurrentContinuationTarget{}, agentexecutionapp.ErrInvalidCurrentAgentStart
	}
	var target agentexecutionapp.CurrentContinuationTarget
	err = repository.projects.WithinProjectTx(
		ctx,
		request.ProjectID,
		pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly},
		func(tx sqlExecutor) error {
			queries, ok := tx.(currentContinuationQuerier)
			if !ok {
				return errors.New("current agent continuation query is unavailable")
			}
			if request.Kind == agentexecutionapp.CurrentContinuationOutputLimit {
				row, queryErr := queries.ResolveCurrentOutputLimitContinuation(
					ctx,
					sqlcgen.ResolveCurrentOutputLimitContinuationParams{
						ActorUserID: request.ActorUserID, ProjectID: projectID,
						ConversationUuid: conversationUUID, ResponseMessageID: responseMessageID,
					},
				)
				if errors.Is(queryErr, pgx.ErrNoRows) {
					return agentexecutionapp.ErrCurrentAgentOutputLimitAlreadyResolved
				}
				if queryErr != nil {
					return fmt.Errorf("resolve current agent output-limit continuation: %w", queryErr)
				}
				target = agentexecutionapp.CurrentContinuationTarget{
					ContinuationKind:    agentexecutionapp.CurrentContinuationOutputLimit,
					Kind:                agentexecutionapp.CurrentRegenerationKind(row.ContinuationKind),
					TargetParticipantID: int64(row.TargetParticipantID),
					QuestionID:          uuid.UUID(row.QuestionID.Bytes).String(),
					UserInput:           row.UserInput,
					ThreadID:            row.ThreadID,
					ExecutionGeneration: row.ExecutionGeneration,
					TruncatedContent:    row.TruncatedContent,
					OutputLimitSequence: int64(row.OutputLimitSequence),
				}
				if uuid.UUID(row.ConversationUuid.Bytes).String() != request.ConversationUUID ||
					target.Validate() != nil {
					return agentexecutionapp.ErrUnsupportedCurrentAgentStart
				}
				return nil
			}
			if request.Kind == agentexecutionapp.CurrentContinuationAuthorization {
				row, queryErr := queries.ResolveCurrentAuthorizationContinuation(
					ctx,
					sqlcgen.ResolveCurrentAuthorizationContinuationParams{
						ActorUserID: request.ActorUserID, ProjectID: projectID,
						ConversationUuid: conversationUUID, ResponseMessageID: responseMessageID,
						AuthorizationRequestID: request.AuthorizationID,
					},
				)
				if errors.Is(queryErr, pgx.ErrNoRows) {
					return agentexecutionapp.ErrCurrentAgentAuthorizationAlreadyResolved
				}
				if queryErr != nil {
					return fmt.Errorf("resolve current agent authorization continuation: %w", queryErr)
				}
				var authorizationRequests []struct {
					InterruptID string `json:"interrupt_id"`
					ToolRunID   string `json:"tool_run_id"`
					ToolCallID  string `json:"tool_call_id"`
				}
				if json.Unmarshal([]byte(row.AuthorizationRequestsJson), &authorizationRequests) != nil ||
					len(authorizationRequests) == 0 || len(authorizationRequests) > 16 {
					return agentexecutionapp.ErrUnsupportedCurrentAgentStart
				}
				targetRequests := make([]agentexecutionapp.CurrentAuthorizationRequest, 0, len(authorizationRequests))
				seen := make(map[string]struct{}, len(authorizationRequests))
				for _, authorizationRequest := range authorizationRequests {
					requestID := authorizationRequest.InterruptID
					if requestID == "" {
						requestID = authorizationRequest.ToolRunID
					}
					if requestID == "" {
						requestID = authorizationRequest.ToolCallID
					}
					if requestID == "" {
						return agentexecutionapp.ErrUnsupportedCurrentAgentStart
					}
					if _, duplicate := seen[requestID]; duplicate {
						return agentexecutionapp.ErrUnsupportedCurrentAgentStart
					}
					seen[requestID] = struct{}{}
					targetRequests = append(targetRequests, agentexecutionapp.CurrentAuthorizationRequest{
						InterruptID: requestID, ToolCallID: authorizationRequest.ToolCallID,
						AvailableActions: []string{"authorize", "skip"},
					})
				}
				if request.AuthorizationID != "" &&
					(len(targetRequests) != 1 || targetRequests[0].InterruptID != request.AuthorizationID) {
					return agentexecutionapp.ErrUnsupportedCurrentAgentStart
				}
				target = agentexecutionapp.CurrentContinuationTarget{
					ContinuationKind:      agentexecutionapp.CurrentContinuationAuthorization,
					Kind:                  agentexecutionapp.CurrentRegenerationKind(row.ContinuationKind),
					TargetParticipantID:   int64(row.TargetParticipantID),
					QuestionID:            uuid.UUID(row.QuestionID.Bytes).String(),
					UserInput:             row.UserInput,
					ThreadID:              row.ThreadID,
					ExecutionGeneration:   row.ExecutionGeneration,
					AuthorizationRequests: targetRequests,
				}
				if request.AuthorizationID != "" {
					target.InterruptID = targetRequests[0].InterruptID
					target.ToolCallID = targetRequests[0].ToolCallID
					target.AvailableActions = append([]string(nil), targetRequests[0].AvailableActions...)
				}
				if uuid.UUID(row.ConversationUuid.Bytes).String() != request.ConversationUUID ||
					target.Validate() != nil {
					return agentexecutionapp.ErrUnsupportedCurrentAgentStart
				}
				return nil
			}
			row, queryErr := queries.ResolveCurrentContinuation(
				ctx,
				sqlcgen.ResolveCurrentContinuationParams{
					ActorUserID: request.ActorUserID, ProjectID: projectID,
					ConversationUuid: conversationUUID, ResponseMessageID: responseMessageID,
				},
			)
			if errors.Is(queryErr, pgx.ErrNoRows) {
				return agentexecutionapp.ErrCurrentAgentHITLAlreadyResolved
			}
			if queryErr != nil {
				return fmt.Errorf("resolve current agent continuation: %w", queryErr)
			}
			type persistedHITLInterrupt struct {
				InterruptID      string   `json:"interrupt_id"`
				AvailableActions []string `json:"available_actions"`
			}
			var interrupt persistedHITLInterrupt
			var interrupts []persistedHITLInterrupt
			var rawInterrupt map[string]any
			var rawInterrupts []map[string]any
			if json.Unmarshal([]byte(row.HitlInterruptJson), &interrupt) != nil ||
				json.Unmarshal([]byte(row.HitlInterruptsJson), &interrupts) != nil ||
				json.Unmarshal([]byte(row.HitlInterruptJson), &rawInterrupt) != nil ||
				json.Unmarshal([]byte(row.HitlInterruptsJson), &rawInterrupts) != nil ||
				len(interrupts) == 0 || len(interrupts) > 16 || len(rawInterrupts) != len(interrupts) ||
				!reflect.DeepEqual(rawInterrupt, rawInterrupts[0]) {
				return agentexecutionapp.ErrUnsupportedCurrentAgentStart
			}
			targetInterrupts := make([]agentexecutionapp.CurrentHITLInterrupt, 0, len(interrupts))
			seen := make(map[string]struct{}, len(interrupts))
			for index, pending := range interrupts {
				if pending.InterruptID == "" || !validInProcessHITLInterrupt(rawInterrupts[index]) {
					return agentexecutionapp.ErrUnsupportedCurrentAgentStart
				}
				if _, duplicate := seen[pending.InterruptID]; duplicate {
					return agentexecutionapp.ErrUnsupportedCurrentAgentStart
				}
				seen[pending.InterruptID] = struct{}{}
				targetInterrupts = append(targetInterrupts, agentexecutionapp.CurrentHITLInterrupt{
					InterruptID:      pending.InterruptID,
					AvailableActions: append([]string(nil), pending.AvailableActions...),
				})
			}
			target = agentexecutionapp.CurrentContinuationTarget{
				ContinuationKind:    agentexecutionapp.CurrentContinuationHITL,
				Kind:                agentexecutionapp.CurrentRegenerationKind(row.ContinuationKind),
				TargetParticipantID: int64(row.TargetParticipantID),
				QuestionID:          uuid.UUID(row.QuestionID.Bytes).String(), UserInput: row.UserInput,
				ThreadID: row.ThreadID, ExecutionGeneration: row.ExecutionGeneration,
				InterruptID:      interrupt.InterruptID,
				AvailableActions: append([]string(nil), interrupt.AvailableActions...),
				HITLInterrupts:   targetInterrupts,
			}
			if uuid.UUID(row.ConversationUuid.Bytes).String() != request.ConversationUUID ||
				target.Validate() != nil {
				return agentexecutionapp.ErrUnsupportedCurrentAgentStart
			}
			return nil
		},
	)
	if err != nil {
		return agentexecutionapp.CurrentContinuationTarget{}, err
	}
	return target, nil
}

func currentPGUUID(value string) (pgtype.UUID, error) {
	var result pgtype.UUID
	if err := result.Scan(value); err != nil || !result.Valid {
		return pgtype.UUID{}, errors.New("invalid UUID")
	}
	return result, nil
}

var _ currentApplicationStartQuerier = pgxExecutor{}
var _ currentAdhocStartQuerier = pgxExecutor{}
var _ currentRegenerationQuerier = pgxExecutor{}
var _ currentContinuationQuerier = pgxExecutor{}
