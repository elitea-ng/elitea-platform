package agentexecution

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

type CurrentRegenerationKind string

const (
	CurrentRegenerationApplication CurrentRegenerationKind = "application"
	CurrentRegenerationAdhoc       CurrentRegenerationKind = "adhoc"
)

type CurrentRegenerationResolveRequest struct {
	ProjectID         int64
	ActorUserID       int64
	ResponseMessageID string
}

func (request CurrentRegenerationResolveRequest) Validate() error {
	if request.ProjectID <= 0 || request.ActorUserID <= 0 ||
		!validUUID(request.ResponseMessageID) {
		return ErrInvalidCurrentAgentStart
	}
	return nil
}

type CurrentRegenerationTarget struct {
	Kind                CurrentRegenerationKind
	ConversationUUID    string
	TargetParticipantID int64
	QuestionID          string
	UserInput           string
}

func (target CurrentRegenerationTarget) Validate() error {
	if (target.Kind != CurrentRegenerationApplication && target.Kind != CurrentRegenerationAdhoc) ||
		target.TargetParticipantID <= 0 || !validUUID(target.ConversationUUID) ||
		!validUUID(target.QuestionID) ||
		!validCurrentAgentText(target.UserInput, maxCurrentAgentUserInputBytes) {
		return ErrUnsupportedCurrentAgentStart
	}
	return nil
}

type CurrentRegenerationResolver interface {
	ResolveCurrentRegeneration(
		context.Context,
		CurrentRegenerationResolveRequest,
	) (CurrentRegenerationTarget, error)
}

type CurrentRegenerationRequest struct {
	ProjectID              int64
	ActorUserID            int64
	ConversationUUID       string
	QuestionID             string
	ResponseMessageID      string
	RegenerationID         string
	RequestedParticipantID int64
	LLMSettings            json.RawMessage
	MCPTokens              json.RawMessage
}

func (request CurrentRegenerationRequest) Validate() error {
	if request.ProjectID <= 0 || request.ActorUserID <= 0 ||
		request.RequestedParticipantID < 0 ||
		!validUUID(request.ConversationUUID) || !validUUID(request.QuestionID) ||
		!validUUID(request.ResponseMessageID) || !validUUID(request.RegenerationID) ||
		!validJSONObject(request.LLMSettings) || !validCurrentMCPTokens(request.MCPTokens) {
		return ErrInvalidCurrentAgentStart
	}
	return nil
}

// CurrentRegenerateTurn is the immutable current-schema replacement side of a
// regeneration admission. The response UUID is reused; the question and
// conversation identities never come from the browser without a database
// ownership and reply-edge recheck in the admission transaction.
type CurrentRegenerateTurn struct {
	ProjectID            int64
	ActorUserID          int64
	ConversationUUID     string
	TargetParticipantID  int64
	Kind                 CurrentRegenerationKind
	ApplicationID        int64
	ApplicationVersionID int64
	QuestionID           string
	ResponseMessageID    string
	ExecutionGeneration  string
}

func (turn CurrentRegenerateTurn) Validate() error {
	if turn.ProjectID <= 0 || turn.ActorUserID <= 0 || turn.TargetParticipantID <= 0 ||
		!validUUID(turn.ConversationUUID) || !validUUID(turn.QuestionID) ||
		!validUUID(turn.ResponseMessageID) || !validUUID(turn.ExecutionGeneration) {
		return ErrInvalidCurrentAgentStart
	}
	switch turn.Kind {
	case CurrentRegenerationApplication:
		if turn.ApplicationID <= 0 || turn.ApplicationVersionID <= 0 {
			return ErrInvalidCurrentAgentStart
		}
	case CurrentRegenerationAdhoc:
		if turn.ApplicationID != 0 || turn.ApplicationVersionID != 0 {
			return ErrInvalidCurrentAgentStart
		}
	default:
		return ErrInvalidCurrentAgentStart
	}
	return nil
}

func (turn *CurrentRegenerateTurn) Clone() *CurrentRegenerateTurn {
	if turn == nil {
		return nil
	}
	clone := *turn
	return &clone
}

func (service *CurrentApplicationStartService) RegenerateCurrentAgent(
	ctx context.Context,
	request CurrentRegenerationRequest,
) (CurrentApplicationStartOutcome, error) {
	if err := request.Validate(); err != nil {
		return CurrentApplicationStartOutcome{}, err
	}
	target, err := service.regenerationResolver.ResolveCurrentRegeneration(
		ctx,
		CurrentRegenerationResolveRequest{
			ProjectID: request.ProjectID, ActorUserID: request.ActorUserID,
			ResponseMessageID: request.ResponseMessageID,
		},
	)
	if err != nil {
		return CurrentApplicationStartOutcome{}, err
	}
	if err := target.Validate(); err != nil ||
		target.ConversationUUID != request.ConversationUUID ||
		target.QuestionID != request.QuestionID ||
		(request.RequestedParticipantID > 0 && target.TargetParticipantID != request.RequestedParticipantID) ||
		request.ProjectID > math.MaxInt32 || request.ActorUserID > math.MaxInt32 ||
		target.TargetParticipantID > math.MaxInt32 {
		return CurrentApplicationStartOutcome{}, ErrUnsupportedCurrentAgentStart
	}

	input, turn, capabilityID, err := service.currentRegenerationInput(ctx, request, target)
	if err != nil {
		return CurrentApplicationStartOutcome{}, err
	}
	projectID := strconv.FormatInt(request.ProjectID, 10)
	actorID := strconv.FormatInt(request.ActorUserID, 10)
	outcome, err := service.admissions.Submit(ctx, SubmitRequest{
		Identity: executionapp.AdmissionIdentity{
			TenantID: projectID, ResourceProjectID: projectID,
			ProjectionProjectID: projectID, ActorID: actorID,
		},
		IdempotencyKey: "regenerate/" + request.ResponseMessageID + "/" + request.RegenerationID,
		CapabilityID:   capabilityID, ClientStreamID: target.ConversationUUID,
		ClientMessageID: request.ResponseMessageID, SIOEvent: "chat_predict",
		Input: input, CurrentRegenerateTurn: turn,
	})
	if err != nil {
		return CurrentApplicationStartOutcome{}, err
	}
	return CurrentApplicationStartOutcome{
		ExecutionID: outcome.ExecutionID, CommandID: outcome.CommandID,
		ResponseMessageID: request.ResponseMessageID, Created: outcome.Created,
	}, nil
}

func (service *CurrentApplicationStartService) currentRegenerationInput(
	ctx context.Context,
	request CurrentRegenerationRequest,
	target CurrentRegenerationTarget,
) (*runtimev1.AgentExecutionInputV1, *CurrentRegenerateTurn, string, error) {
	// Narrow the ids once, before the branch, because BOTH arms freeze a version
	// with them. int64->int32 truncates silently and these address rows, so an
	// out-of-range value would freeze against a DIFFERENT, valid project rather
	// than fail. Refusing is the only correct answer: the columns are Postgres
	// `integer`, so such an id cannot denote a real row.
	projectID32, ok := narrowRowID(request.ProjectID)
	if !ok {
		return nil, nil, "", ErrInvalidCurrentAgentStart
	}
	actorUserID32, ok := narrowRowID(request.ActorUserID)
	if !ok {
		return nil, nil, "", ErrInvalidCurrentAgentStart
	}
	turn := &CurrentRegenerateTurn{
		ProjectID: request.ProjectID, ActorUserID: request.ActorUserID,
		ConversationUUID:    target.ConversationUUID,
		TargetParticipantID: target.TargetParticipantID, Kind: target.Kind,
		QuestionID: target.QuestionID, ResponseMessageID: request.ResponseMessageID,
		ExecutionGeneration: request.RegenerationID,
	}
	suggestionPolicy := service.resolveNextInputSuggestionPolicy(
		ctx,
		request.ProjectID,
		request.ActorUserID,
	)
	// Resolved once for the whole branch below. A resume and a regenerate are
	// executions like any other: the sensitive-tool policy governs what the
	// agent may do WITHOUT asking, and a resumed turn that arrived without one
	// would run the rest of its tool calls unguarded — on the very path a user
	// reached by answering an authorization prompt.
	toolkitGuardrails, err := service.resolveToolkitGuardrails(ctx)
	if err != nil {
		return nil, nil, "", err
	}
	switch target.Kind {
	case CurrentRegenerationApplication:
		start := CurrentApplicationStartRequest{
			MCPTokens: request.MCPTokens,
			ProjectID: request.ProjectID, ActorUserID: request.ActorUserID,
			ConversationUUID:    target.ConversationUUID,
			TargetParticipantID: target.TargetParticipantID,
			QuestionID:          target.QuestionID, UserInput: target.UserInput,
		}
		resolved, err := service.resolver.ResolveCurrentApplication(ctx, start)
		if err != nil {
			return nil, nil, "", err
		}
		frozen, err := service.freezer.FreezeCurrentApplicationVersion(
			ctx,
			CurrentApplicationVersionFreezeRequest{
				ProjectID: projectID32, ActorUserID: actorUserID32,
				VersionDetails: resolved.VersionDetails,
			},
		)
		if err != nil {
			return nil, nil, "", err
		}
		resolved.VersionDetails = frozen
		start.QuestionID = request.RegenerationID
		input, err := currentApplicationInput(start, resolved, suggestionPolicy, toolkitGuardrails, nil)
		if err != nil {
			return nil, nil, "", err
		}
		input.IsRegenerate = true
		turn.ApplicationID = resolved.ApplicationID
		turn.ApplicationVersionID = resolved.ApplicationVersionID
		return input, turn, executiondomain.AgentApplicationCapability, nil
	case CurrentRegenerationAdhoc:
		start := CurrentAdhocStartRequest{
			MCPTokens: request.MCPTokens,
			ProjectID: request.ProjectID, ActorUserID: request.ActorUserID,
			ConversationUUID:    target.ConversationUUID,
			TargetParticipantID: target.TargetParticipantID,
			QuestionID:          target.QuestionID, UserInput: target.UserInput,
			LLMSettings: request.LLMSettings,
		}
		resolved, err := service.adhocResolver.ResolveCurrentAdhoc(ctx, start)
		if err != nil {
			return nil, nil, "", err
		}
		snapshot, err := currentAdhocSnapshot(request.LLMSettings, resolved)
		if err != nil {
			return nil, nil, "", err
		}
		frozen, err := service.freezer.FreezeCurrentApplicationVersion(
			ctx,
			CurrentApplicationVersionFreezeRequest{
				ProjectID: projectID32, ActorUserID: actorUserID32,
				VersionDetails: snapshot,
			},
		)
		if err != nil {
			return nil, nil, "", err
		}
		start.QuestionID = request.RegenerationID
		input, err := currentAdhocInput(start, resolved, frozen, suggestionPolicy, toolkitGuardrails, nil)
		if err != nil {
			return nil, nil, "", err
		}
		input.IsRegenerate = true
		return input, turn, executiondomain.AgentAdhocCapability, nil
	default:
		return nil, nil, "", errors.New("unsupported current regeneration kind")
	}
}

// narrowRowID converts a domain id (int64) to the int32 the freeze request and
// the sqlc params use, refusing anything the target type cannot hold.
//
// Go's int64->int32 conversion is a SILENT TRUNCATION and these ids address
// rows: 4294967300 becomes 4, so an out-of-range id would not fail — it would
// freeze a version against a DIFFERENT, valid project. The columns are Postgres
// `integer`, so a value above MaxInt32 cannot denote a real row and refusing is
// the only correct answer.
//
// The API boundary (positiveCanonicalID) already bounds ids on the way in; this
// is defence in depth at the point of narrowing, and unlike the boundary check
// it is local enough for CodeQL's dataflow to see
// (go/incorrect-integer-conversion).
func narrowRowID(value int64) (int32, bool) {
	if value < 0 || value > math.MaxInt32 {
		return 0, false
	}
	return int32(value), true
}
