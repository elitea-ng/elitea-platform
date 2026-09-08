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
	// catalogueProjectID is the ONE public project — the schema that holds the
	// published catalogue (internal/publicproject). It is the single foreign
	// project a conversation may address an agent in, and it is a field rather
	// than an environment read inside the resolve so that the admission rule is
	// exercisable without setting a process-wide variable.
	catalogueProjectID int32
}

func NewCurrentAgentStartRepository(
	pool *pgxpool.Pool,
	catalogueProjectID int32,
) (*CurrentAgentStartRepository, error) {
	projects, err := newPostgresProjectStore(pool)
	if err != nil {
		return nil, err
	}
	return newCurrentAgentStartRepository(projects, catalogueProjectID)
}

func newCurrentAgentStartRepository(
	projects projectStore,
	catalogueProjectID int32,
) (*CurrentAgentStartRepository, error) {
	if projects == nil {
		return nil, errors.New("current agent project database is required")
	}
	if catalogueProjectID <= 0 {
		return nil, errors.New("current agent catalogue project id is required")
	}
	return &CurrentAgentStartRepository{
		projects:           projects,
		catalogueProjectID: catalogueProjectID,
	}, nil
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
		// The catalogue version is read in its OWN short transaction after this
		// one closes, never nested inside it: two pool connections held at once
		// per turn start is a deadlock waiting for a busy pool, and the second
		// read needs no snapshot the first one took.
		catalogue := currentCatalogueApplicationReference{}
		if err := repository.projects.WithinProjectTx(
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
						CatalogueProjectID:  repository.catalogueProjectID,
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
				if row.ApplicationID <= 0 {
					return agentexecutionapp.UnsupportedCurrentAgentStart(
						"the target participant carries no entity_meta.id")
				}
				if row.ApplicationVersionID <= 0 {
					return agentexecutionapp.UnsupportedCurrentAgentStart(
						"the target participant mapping carries no entity_settings.version_id")
				}
				if int64(row.ApplicationProjectID) != request.ProjectID {
					// A FOREIGN project. Exactly one is admitted — the public
					// (catalogue) project — and only for a version the second
					// read proves is `published`. Everything else is the
					// tenancy boundary and stays refused here.
					//
					// The SQL branch already made this the only foreign project
					// that can reach this line. The comparison is repeated
					// because a resolver that trusted the query for its own
					// boundary would silently widen the day that WHERE is
					// edited, and this is the boundary.
					if row.ApplicationProjectID != repository.catalogueProjectID {
						return agentexecutionapp.UnsupportedCurrentAgentStart(
							"the target participant's entity_meta.project_id is neither the project " +
								"the turn runs in nor the published catalogue; an agent private to " +
								"another project cannot be chatted with from this one")
					}
					if !json.Valid(variables) || !json.Valid(chatHistory) ||
						!json.Valid(internalTools) {
						return agentexecutionapp.UnsupportedCurrentAgentStart(
							"the resolved turn carries a malformed JSON projection")
					}
					// `versionDetails` from this row is a document of nulls: the
					// version is not in THIS schema, so the projection had no
					// row to build from. It is deliberately not carried over.
					catalogue = currentCatalogueApplicationReference{
						applicationID: row.ApplicationID,
						versionID:     row.ApplicationVersionID,
					}
					target = agentexecutionapp.CurrentApplicationTarget{
						ApplicationID:        int64(row.ApplicationID),
						ApplicationVersionID: int64(row.ApplicationVersionID),
						Variables:            variables,
						ChatHistory:          chatHistory,
						InternalTools:        internalTools,
					}
					return nil
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
		); err != nil {
			return err
		}
		if catalogue.versionID == 0 {
			return nil
		}
		versionDetails, err := repository.resolveCatalogueApplicationVersion(ctx, catalogue)
		if err != nil {
			return err
		}
		target.VersionDetails = versionDetails
		return nil
	}
	if err := repository.resolveAfterCurrentResponseSettles(
		ctx, request.ProjectID, conversationUUID, resolve,
	); err != nil {
		return agentexecutionapp.CurrentApplicationTarget{}, err
	}
	return target, nil
}

// currentCatalogueApplicationReference names the version a turn must read out
// of the PUBLIC (catalogue) project's schema rather than its own. A zero
// versionID means the turn is an ordinary same-project one and no second read
// is due.
type currentCatalogueApplicationReference struct {
	applicationID int32
	versionID     int32
}

// currentPublishedVersionStatus is the one `application_versions.status` value
// a conversation in another project may address.
//
// The vocabulary is the publish plane's: `draft` is an author's working copy,
// `published` is what Publish clones and what the catalogue twin is written as,
// `embedded` is the marker a sub-agent clone carries
// (internal/api/v2/eliteacore/handler.go, catalog_mirror.go). Only `published`
// was ever offered to another project, so only `published` is admitted; a
// moderator's draft sitting in the catalogue schema is refused exactly like a
// private agent in a stranger's project.
const currentPublishedVersionStatus = "published"

// resolveCatalogueApplicationVersion reads one PUBLISHED version out of the
// catalogue project's own tenant schema, for a conversation that lives
// somewhere else.
//
// WHY A SECOND READ AT ALL. `application_versions` is a per-project table.
// ResolveCurrentApplicationTurn runs inside the conversation's schema and can
// only see that project's rows, so the version of an agent published in the
// catalogue is not reachable from there at any price — the turn query's LEFT
// JOIN misses it by construction. Everything the TURN is made of (the
// conversation, its participants, its history) stays in the conversation's
// project; only the agent's definition comes from the catalogue.
//
// WHAT STAYS WITH THE CALLER. The execution, its claim, its budget and its
// `X-SECRET` are the CALLER's project's, unchanged: the turn is billed and
// authorised where it is held. The model is the exception the publish plane
// already arranged — a publishable version must name a SHARED model, whose
// `model_project_id` is the catalogue project, and both the published clone and
// the catalogue twin copy `llm_settings` verbatim
// (internal/api/v2/eliteacore/handler.go, catalog_mirror.go). The freeze then
// resolves that model through the shared-model catalogue, so a published agent
// answers with the catalogue's model while the turn is still the caller's.
//
// WHAT IS NOT CROSS-PROJECT YET, AND WHAT IT WOULD TAKE. Only the SEND path.
// The two statements a send runs — ResolveCurrentApplicationTurn and
// InsertCurrentApplicationTurn — both admit the catalogue participant. The
// eight statements behind REGENERATE, CONTINUE, the two resumes and the reset
// (ResolveCurrentRegeneration, ResolveCurrentContinuation,
// ResolveCurrentOutputLimitContinuation, ResolveCurrentAuthorizationContinuation,
// ResumeCurrentAgentOutputLimit, ResumeCurrentAgentHITL,
// ResumeCurrentAgentAuthorization, ResetCurrentAgentResponse) still compare the
// response author's `entity_meta.project_id` against the request's project
// alone, so those actions on a catalogue participant's answer are refused with
// the same 422 the send used to give. That is fail-closed and visible, not a
// wrong answer: nothing is written and nothing is billed. Closing it is the
// same two edits per statement made here — the project disjunct, and for the
// five that join `application_versions`, the LEFT JOIN with its identity
// comparisons restated — and it needs its own tests, because none of those
// paths is exercised by the send journey.
func (repository *CurrentAgentStartRepository) resolveCatalogueApplicationVersion(
	ctx context.Context,
	reference currentCatalogueApplicationReference,
) (json.RawMessage, error) {
	if repository == nil || repository.projects == nil ||
		repository.catalogueProjectID <= 0 ||
		reference.applicationID <= 0 || reference.versionID <= 0 {
		return nil, agentexecutionapp.ErrUnsupportedCurrentAgentStart
	}
	var versionDetails json.RawMessage
	err := repository.projects.WithinProjectTx(
		ctx,
		int64(repository.catalogueProjectID),
		pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly},
		func(tx sqlExecutor) error {
			queries, ok := tx.(currentApplicationVersionQuerier)
			if !ok {
				return errors.New("current application version query is unavailable")
			}
			nesting, ok := tx.(currentApplicationNestingQuerier)
			if !ok {
				return errors.New("current application nesting query is unavailable")
			}
			row, queryErr := queries.ResolveCurrentApplicationVersionDetails(
				ctx,
				sqlcgen.ResolveCurrentApplicationVersionDetailsParams{
					ApplicationVersionID: reference.versionID,
					ApplicationID:        reference.applicationID,
				},
			)
			if errors.Is(queryErr, pgx.ErrNoRows) {
				return agentexecutionapp.UnsupportedCurrentAgentStart(
					"the catalogue project holds no application version with the id pair the " +
						"participant names")
			}
			if queryErr != nil {
				return fmt.Errorf("resolve catalogue application version details: %w", queryErr)
			}
			details := json.RawMessage(row.ApplicationVersionDetailsJson)
			if row.ApplicationID != reference.applicationID ||
				row.ApplicationVersionID != reference.versionID ||
				!json.Valid(details) {
				return agentexecutionapp.UnsupportedCurrentAgentStart(
					"the catalogue version read back does not match the pair that was asked for")
			}
			if err := currentCatalogueVersionAdmissible(details); err != nil {
				return err
			}
			if validationErr := validateCurrentApplicationNesting(
				ctx,
				nesting,
				reference.versionID,
				1,
			); validationErr != nil {
				if contextErr := ctx.Err(); contextErr != nil {
					return contextErr
				}
				if errors.Is(validationErr, errInvalidCurrentApplicationNesting) {
					return agentexecutionapp.ErrUnsupportedCurrentAgentStart
				}
				return fmt.Errorf("validate catalogue application nesting: %w", validationErr)
			}
			materialized, materializeErr := materializeCurrentApplicationVersionNestedSkills(
				ctx,
				nesting,
				details,
			)
			if materializeErr != nil {
				if contextErr := ctx.Err(); contextErr != nil {
					return contextErr
				}
				if errors.Is(materializeErr, errInvalidCurrentApplicationNesting) {
					return agentexecutionapp.ErrUnsupportedCurrentAgentStart
				}
				return fmt.Errorf("materialize catalogue nested skills: %w", materializeErr)
			}
			versionDetails = materialized
			return nil
		},
	)
	if err != nil {
		return nil, err
	}
	return versionDetails, nil
}

// currentCatalogueVersionAdmissible applies the two rules that only apply to a
// version borrowed from another project.
//
// STATUS. Only `published` crosses a project line. The turn query admitted the
// participant on its project alone, because status lives on a row that schema
// cannot see; this is where the other half of the rule is enforced.
//
// TOOLS. A catalogue version must carry none, and this refuses rather than
// silently drops. Every id in a `tools` entry — the toolkit row, the credential
// reference inside its settings, a nested agent's application/version pair —
// names a row in the CATALOGUE project's schema, while the freeze that runs
// next resolves toolkit settings and credentials in the CALLER's project
// (internal/application/agentexecution/tools.go) and the nested-version route
// reads the claim's project (internal/infra/storage/runtime_application_version.go).
// Carrying such an entry across would resolve one project's id against another
// project's tables, which is a wrong answer or a leak, never a correct run.
//
// It is not a hypothetical guard on a shape that cannot occur: it is the shape
// the catalogue twin already has. `mirrorPublishedVersion` deliberately copies
// no `entity_tool_mapping` or `entity_skill_mapping` row
// (internal/api/v2/eliteacore/catalog_mirror.go), so a twin's `tools` is `[]`
// and this refusal never fires for one. It fires if that ever changes, instead
// of the change quietly reaching across schemas.
func currentCatalogueVersionAdmissible(details json.RawMessage) error {
	var version struct {
		Status string            `json:"status"`
		Tools  []json.RawMessage `json:"tools"`
		Meta   struct {
			InternalTools []any `json:"internal_tools"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(details, &version); err != nil {
		return agentexecutionapp.UnsupportedCurrentAgentStart(
			"the catalogue version details are not a readable version document")
	}
	if version.Status != currentPublishedVersionStatus {
		return agentexecutionapp.UnsupportedCurrentAgentStart(
			"the version the participant names in the catalogue project is not published; " +
				"only a published version may be chatted with from another project")
	}
	if len(version.Tools) != 0 {
		return agentexecutionapp.UnsupportedCurrentAgentStart(
			"the catalogue version carries toolkit or sub-agent references, whose ids belong to " +
				"the catalogue project; they cannot be resolved in the project this turn runs in")
	}
	for _, entry := range version.Meta.InternalTools {
		name, isString := entry.(string)
		if !isString || !currentAuthorableInternalTools[name] {
			return agentexecutionapp.UnsupportedCurrentAgentStart(
				"the catalogue version names an internal tool this platform does not serve")
		}
	}
	return nil
}

// currentAuthorableInternalTools is the platform's authorable internal-tool
// catalogue, restated for the ONE version read that SQL cannot gate.
//
// Both turn statements carry this list against
// `application_version.meta -> 'internal_tools'`
// (internal/db/queries/agent_chat.sql). Neither can apply it to a catalogue
// version: the row is in another schema, so their join misses it and the clause
// is vacuously true. Applying it here keeps a published agent held to the same
// admission as a local one, on the document actually read.
//
// It must stay equal to the list in that file. A name added there and not here
// refuses a published agent the product can run; a name added here and not
// there admits one it cannot.
var currentAuthorableInternalTools = map[string]bool{
	"ask_user": true, "attachments": true, "data_analysis": true,
	"image_generation": true, "internal_mcp": true, "lazy_tools_mode": true,
	"planner": true, "pyodide": true, "swarm": true,
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
