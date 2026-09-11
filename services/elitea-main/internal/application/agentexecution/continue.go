package agentexecution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

const (
	maxCurrentHITLValueBytes          = 256 * 1024
	maxCurrentHITLDecisions           = 16
	maxCurrentOutputContinuationBytes = 64 * 1024
)

var (
	ErrCurrentAgentHITLAlreadyResolved          = errors.New("current agent HITL interrupt is already resolved")
	ErrCurrentAgentAuthorizationAlreadyResolved = errors.New("current agent authorization request is already resolved")
	ErrCurrentAgentOutputLimitAlreadyResolved   = errors.New("current agent output-limit continuation is already resolved")
)

type CurrentContinuationKind string

const (
	CurrentContinuationHITL          CurrentContinuationKind = "hitl"
	CurrentContinuationAuthorization CurrentContinuationKind = "authorization"
	CurrentContinuationOutputLimit   CurrentContinuationKind = "output_limit"
)

type CurrentContinuationResolveRequest struct {
	ProjectID         int64
	ActorUserID       int64
	ConversationUUID  string
	ResponseMessageID string
	Kind              CurrentContinuationKind
	AuthorizationID   string
}

func (request CurrentContinuationResolveRequest) Validate() error {
	if request.ProjectID <= 0 || request.ActorUserID <= 0 ||
		!validUUID(request.ConversationUUID) || !validUUID(request.ResponseMessageID) {
		return ErrInvalidCurrentAgentStart
	}
	if request.normalizedKind() != CurrentContinuationHITL &&
		request.normalizedKind() != CurrentContinuationAuthorization &&
		request.normalizedKind() != CurrentContinuationOutputLimit {
		return ErrInvalidCurrentAgentStart
	}
	if request.normalizedKind() == CurrentContinuationAuthorization &&
		request.AuthorizationID != "" &&
		(len(request.AuthorizationID) > 512 || strings.ContainsRune(request.AuthorizationID, '\x00')) {
		return ErrInvalidCurrentAgentStart
	}
	if request.normalizedKind() != CurrentContinuationAuthorization && request.AuthorizationID != "" {
		return ErrInvalidCurrentAgentStart
	}
	return nil
}

func (request CurrentContinuationResolveRequest) normalizedKind() CurrentContinuationKind {
	if request.Kind == "" {
		return CurrentContinuationHITL
	}
	return request.Kind
}

// CurrentContinuationTarget is reconstructed from the paused response in the
// current project schema. Browser input never selects the graph checkpoint,
// execution generation, participant, question, or interrupt identity.
type CurrentContinuationTarget struct {
	ContinuationKind      CurrentContinuationKind
	Kind                  CurrentRegenerationKind
	TargetParticipantID   int64
	QuestionID            string
	UserInput             string
	ThreadID              string
	ExecutionGeneration   string
	InterruptID           string
	ToolCallID            string
	AvailableActions      []string
	HITLInterrupts        []CurrentHITLInterrupt
	AuthorizationRequests []CurrentAuthorizationRequest
	TruncatedContent      string
	OutputLimitSequence   int64
}

type CurrentHITLInterrupt struct {
	InterruptID      string
	AvailableActions []string
}

type CurrentAuthorizationRequest struct {
	InterruptID      string
	ToolCallID       string
	AvailableActions []string
}

type CurrentHITLDecision struct {
	InterruptID   string `json:"interrupt_id"`
	ToolCallID    string `json:"tool_call_id,omitempty"`
	GuardrailType string `json:"guardrail_type,omitempty"`
	Action        string `json:"action"`
	Value         string `json:"value,omitempty"`
}

func (target CurrentContinuationTarget) Validate() error {
	kind := target.ContinuationKind
	if kind == "" {
		kind = CurrentContinuationHITL
	}
	if (target.Kind != CurrentRegenerationApplication && target.Kind != CurrentRegenerationAdhoc) ||
		target.TargetParticipantID <= 0 || !validUUID(target.QuestionID) ||
		!validCurrentAgentText(target.UserInput, maxCurrentAgentUserInputBytes) ||
		target.ThreadID == "" || len(target.ThreadID) > 256 || strings.ContainsRune(target.ThreadID, '\x00') ||
		!validUUID(target.ExecutionGeneration) ||
		(kind != CurrentContinuationHITL && kind != CurrentContinuationAuthorization &&
			kind != CurrentContinuationOutputLimit) {
		return ErrUnsupportedCurrentAgentStart
	}
	if kind == CurrentContinuationOutputLimit {
		if len(target.TruncatedContent) > maxCurrentOutputContinuationBytes ||
			strings.ContainsRune(target.TruncatedContent, '\x00') || target.OutputLimitSequence <= 0 ||
			len(target.HITLInterrupts) != 0 || len(target.AuthorizationRequests) != 0 ||
			target.InterruptID != "" || target.ToolCallID != "" || len(target.AvailableActions) != 0 {
			return ErrUnsupportedCurrentAgentStart
		}
		return nil
	}
	if kind == CurrentContinuationHITL {
		if len(target.HITLInterrupts) == 0 || len(target.HITLInterrupts) > maxCurrentHITLDecisions ||
			len(target.AuthorizationRequests) != 0 {
			return ErrUnsupportedCurrentAgentStart
		}
		seen := make(map[string]struct{}, len(target.HITLInterrupts))
		for _, interrupt := range target.HITLInterrupts {
			if interrupt.InterruptID == "" || len(interrupt.InterruptID) > 512 ||
				strings.ContainsRune(interrupt.InterruptID, '\x00') ||
				len(interrupt.AvailableActions) == 0 || len(interrupt.AvailableActions) > 8 {
				return ErrUnsupportedCurrentAgentStart
			}
			if _, duplicate := seen[interrupt.InterruptID]; duplicate {
				return ErrUnsupportedCurrentAgentStart
			}
			seen[interrupt.InterruptID] = struct{}{}
			for _, action := range interrupt.AvailableActions {
				if !currentRootHITLAction(action) {
					return ErrUnsupportedCurrentAgentStart
				}
			}
		}
		return nil
	}
	if len(target.AuthorizationRequests) == 0 ||
		len(target.AuthorizationRequests) > maxCurrentHITLDecisions || len(target.HITLInterrupts) != 0 {
		return ErrUnsupportedCurrentAgentStart
	}
	seen := make(map[string]struct{}, len(target.AuthorizationRequests))
	for _, request := range target.AuthorizationRequests {
		if request.InterruptID == "" || len(request.InterruptID) > 512 ||
			strings.ContainsRune(request.InterruptID, '\x00') ||
			(request.ToolCallID != "" && (len(request.ToolCallID) > 512 || strings.ContainsRune(request.ToolCallID, '\x00'))) ||
			len(request.AvailableActions) == 0 || len(request.AvailableActions) > 8 {
			return ErrUnsupportedCurrentAgentStart
		}
		if _, duplicate := seen[request.InterruptID]; duplicate {
			return ErrUnsupportedCurrentAgentStart
		}
		seen[request.InterruptID] = struct{}{}
		for _, action := range request.AvailableActions {
			if !currentAuthorizationAction(action) {
				return ErrUnsupportedCurrentAgentStart
			}
		}
	}
	if target.InterruptID != "" &&
		(len(target.AuthorizationRequests) != 1 ||
			target.AuthorizationRequests[0].InterruptID != target.InterruptID ||
			target.AuthorizationRequests[0].ToolCallID != target.ToolCallID ||
			!slices.Equal(target.AuthorizationRequests[0].AvailableActions, target.AvailableActions)) {
		return ErrUnsupportedCurrentAgentStart
	}
	if target.InterruptID == "" && (target.ToolCallID != "" || len(target.AvailableActions) != 0) {
		return ErrUnsupportedCurrentAgentStart
	}
	return nil
}

type CurrentContinuationResolver interface {
	ResolveCurrentContinuation(
		context.Context,
		CurrentContinuationResolveRequest,
	) (CurrentContinuationTarget, error)
}

type CurrentContinuationRequest struct {
	ProjectID          int64
	ActorUserID        int64
	ConversationUUID   string
	ResponseMessageID  string
	ThreadID           string
	Action             string
	Value              string
	Kind               CurrentContinuationKind
	AuthorizationID    string
	MCPTokens          json.RawMessage
	IgnoredMCPServers  json.RawMessage
	DeclinedMCPServers json.RawMessage
	HITLDecisions      []CurrentHITLDecision
}

func (request CurrentContinuationRequest) Validate() error {
	kind := request.normalizedKind()
	if request.ProjectID <= 0 || request.ActorUserID <= 0 ||
		!validUUID(request.ConversationUUID) || !validUUID(request.ResponseMessageID) ||
		len(request.Value) > maxCurrentHITLValueBytes ||
		strings.ContainsRune(request.Value, '\x00') ||
		(request.ThreadID != "" && (len(request.ThreadID) > 256 || strings.ContainsRune(request.ThreadID, '\x00'))) {
		return ErrInvalidCurrentAgentStart
	}
	if kind == CurrentContinuationHITL {
		if _, err := request.normalizedHITLDecisions(); err != nil ||
			request.AuthorizationID != "" || len(request.MCPTokens) != 0 ||
			len(request.IgnoredMCPServers) != 0 || len(request.DeclinedMCPServers) != 0 {
			return ErrInvalidCurrentAgentStart
		}
		return nil
	}
	if kind == CurrentContinuationOutputLimit {
		if request.ThreadID != "" || request.Action != "" || request.Value != "" ||
			request.AuthorizationID != "" || len(request.MCPTokens) != 0 ||
			len(request.IgnoredMCPServers) != 0 || len(request.DeclinedMCPServers) != 0 ||
			len(request.HITLDecisions) != 0 {
			return ErrInvalidCurrentAgentStart
		}
		return nil
	}
	if kind != CurrentContinuationAuthorization || request.Value != "" ||
		!validJSONObject(request.MCPTokens) ||
		!validJSONArray(request.IgnoredMCPServers) || !validJSONArray(request.DeclinedMCPServers) {
		return ErrInvalidCurrentAgentStart
	}
	_, err := request.normalizedAuthorizationDecisions()
	return err
}

func (request CurrentContinuationRequest) normalizedHITLDecisions() ([]CurrentHITLDecision, error) {
	if len(request.HITLDecisions) == 0 {
		decision := CurrentHITLDecision{Action: request.Action, Value: request.Value}
		if !validCurrentHITLDecision(decision, false) {
			return nil, ErrInvalidCurrentAgentStart
		}
		return []CurrentHITLDecision{decision}, nil
	}
	if request.Action != "" || request.Value != "" || len(request.HITLDecisions) > maxCurrentHITLDecisions {
		return nil, ErrInvalidCurrentAgentStart
	}
	seen := make(map[string]struct{}, len(request.HITLDecisions))
	decisions := append([]CurrentHITLDecision(nil), request.HITLDecisions...)
	for _, decision := range decisions {
		if !validCurrentHITLDecision(decision, true) {
			return nil, ErrInvalidCurrentAgentStart
		}
		if _, duplicate := seen[decision.InterruptID]; duplicate {
			return nil, ErrInvalidCurrentAgentStart
		}
		seen[decision.InterruptID] = struct{}{}
	}
	return decisions, nil
}

func validCurrentHITLDecision(decision CurrentHITLDecision, requireIdentity bool) bool {
	if decision.GuardrailType != "" || !currentRootHITLAction(decision.Action) || len(decision.Value) > maxCurrentHITLValueBytes ||
		strings.ContainsRune(decision.Value, '\x00') ||
		((decision.Action == "edit" || decision.Action == "block_with_comment" || decision.Action == "answer") && decision.Value == "") ||
		(decision.Action != "edit" && decision.Action != "block_with_comment" && decision.Action != "answer" && decision.Value != "") {
		return false
	}
	if !requireIdentity {
		return decision.InterruptID == "" && decision.ToolCallID == ""
	}
	return decision.InterruptID != "" && len(decision.InterruptID) <= 512 &&
		!strings.ContainsRune(decision.InterruptID, '\x00') &&
		(decision.ToolCallID == "" || (len(decision.ToolCallID) <= 512 && !strings.ContainsRune(decision.ToolCallID, '\x00')))
}

func validCurrentAuthorizationDecision(decision CurrentHITLDecision) bool {
	return decision.GuardrailType == "mcp_auth" &&
		currentAuthorizationAction(decision.Action) && decision.Value == "" &&
		decision.InterruptID != "" && len(decision.InterruptID) <= 512 &&
		!strings.ContainsRune(decision.InterruptID, '\x00') &&
		(decision.ToolCallID == "" ||
			(len(decision.ToolCallID) <= 512 && !strings.ContainsRune(decision.ToolCallID, '\x00')))
}

func (request CurrentContinuationRequest) normalizedAuthorizationDecisions() ([]CurrentHITLDecision, error) {
	if len(request.HITLDecisions) == 0 {
		decision := CurrentHITLDecision{
			InterruptID:   request.AuthorizationID,
			GuardrailType: "mcp_auth",
			Action:        request.Action,
		}
		if !validCurrentAuthorizationDecision(decision) {
			return nil, ErrInvalidCurrentAgentStart
		}
		return []CurrentHITLDecision{decision}, nil
	}
	if request.AuthorizationID != "" || request.Action != "" ||
		len(request.HITLDecisions) > maxCurrentHITLDecisions {
		return nil, ErrInvalidCurrentAgentStart
	}
	seen := make(map[string]struct{}, len(request.HITLDecisions))
	decisions := append([]CurrentHITLDecision(nil), request.HITLDecisions...)
	for _, decision := range decisions {
		if !validCurrentAuthorizationDecision(decision) {
			return nil, ErrInvalidCurrentAgentStart
		}
		if _, duplicate := seen[decision.InterruptID]; duplicate {
			return nil, ErrInvalidCurrentAgentStart
		}
		seen[decision.InterruptID] = struct{}{}
	}
	return decisions, nil
}

func decisionsMatchCurrentAuthorizationRequests(
	decisions []CurrentHITLDecision,
	requests []CurrentAuthorizationRequest,
) bool {
	if len(decisions) != len(requests) || len(decisions) == 0 {
		return false
	}
	remaining := make(map[string]CurrentAuthorizationRequest, len(requests))
	for _, request := range requests {
		remaining[request.InterruptID] = request
	}
	for index := range decisions {
		request, exists := remaining[decisions[index].InterruptID]
		if !exists || !slices.Contains(request.AvailableActions, decisions[index].Action) ||
			(decisions[index].ToolCallID != "" && decisions[index].ToolCallID != request.ToolCallID) {
			return false
		}
		decisions[index].ToolCallID = request.ToolCallID
		delete(remaining, decisions[index].InterruptID)
	}
	return len(remaining) == 0
}

func decisionsMatchCurrentHITLInterrupts(
	decisions []CurrentHITLDecision,
	interrupts []CurrentHITLInterrupt,
) bool {
	if len(decisions) != len(interrupts) || len(decisions) == 0 {
		return false
	}
	remaining := make(map[string]CurrentHITLInterrupt, len(interrupts))
	for _, interrupt := range interrupts {
		remaining[interrupt.InterruptID] = interrupt
	}
	for _, decision := range decisions {
		interruptID := decision.InterruptID
		if interruptID == "" && len(decisions) == 1 {
			interruptID = interrupts[0].InterruptID
		}
		interrupt, exists := remaining[interruptID]
		if !exists || !slices.Contains(interrupt.AvailableActions, decision.Action) {
			return false
		}
		delete(remaining, interruptID)
	}
	return len(remaining) == 0
}

func (request CurrentContinuationRequest) normalizedKind() CurrentContinuationKind {
	if request.Kind == "" {
		return CurrentContinuationHITL
	}
	return request.Kind
}

// CurrentContinueTurn is the immutable current-schema side of one bounded
// in-process HITL resume. The admission transaction rechecks every field while
// atomically consuming the exact pending card set and resuming the response.
type CurrentContinueTurn struct {
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
	ThreadID             string
	InterruptID          string
	Action               string
	ContinuationKind     CurrentContinuationKind
	HITLDecisions        json.RawMessage
	OutputLimitSequence  int64
}

func (turn CurrentContinueTurn) Validate() error {
	if turn.ProjectID <= 0 || turn.ActorUserID <= 0 || turn.TargetParticipantID <= 0 ||
		!validUUID(turn.ConversationUUID) || !validUUID(turn.QuestionID) ||
		!validUUID(turn.ResponseMessageID) || !validUUID(turn.ExecutionGeneration) ||
		turn.ThreadID == "" || len(turn.ThreadID) > 256 || strings.ContainsRune(turn.ThreadID, '\x00') {
		return ErrInvalidCurrentAgentStart
	}
	kind := turn.ContinuationKind
	if kind == "" {
		kind = CurrentContinuationHITL
	}
	if kind != CurrentContinuationHITL && kind != CurrentContinuationAuthorization &&
		kind != CurrentContinuationOutputLimit {
		return ErrInvalidCurrentAgentStart
	}
	if kind == CurrentContinuationOutputLimit {
		if len(turn.HITLDecisions) != 0 || turn.InterruptID != "" || turn.Action != "" ||
			turn.OutputLimitSequence <= 0 {
			return ErrInvalidCurrentAgentStart
		}
	}
	if kind == CurrentContinuationHITL && !validCurrentHITLDecisionsJSON(turn.HITLDecisions) {
		return ErrInvalidCurrentAgentStart
	}
	if kind == CurrentContinuationAuthorization {
		var decisions []CurrentHITLDecision
		if json.Unmarshal(turn.HITLDecisions, &decisions) != nil ||
			!validCurrentAuthorizationDecisions(decisions) ||
			(len(decisions) == 1 &&
				(decisions[0].InterruptID != turn.InterruptID || decisions[0].Action != turn.Action)) ||
			(len(decisions) > 1 && (turn.InterruptID != "" || turn.Action != "")) {
			return ErrInvalidCurrentAgentStart
		}
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

func validCurrentHITLDecisionsJSON(raw json.RawMessage) bool {
	var decisions []CurrentHITLDecision
	if json.Unmarshal(raw, &decisions) != nil || len(decisions) == 0 || len(decisions) > maxCurrentHITLDecisions {
		return false
	}
	seen := make(map[string]struct{}, len(decisions))
	for _, decision := range decisions {
		if !validCurrentHITLDecision(decision, true) {
			return false
		}
		if _, duplicate := seen[decision.InterruptID]; duplicate {
			return false
		}
		seen[decision.InterruptID] = struct{}{}
	}
	return true
}

func validCurrentAuthorizationDecisions(decisions []CurrentHITLDecision) bool {
	if len(decisions) == 0 || len(decisions) > maxCurrentHITLDecisions {
		return false
	}
	seen := make(map[string]struct{}, len(decisions))
	for _, decision := range decisions {
		if !validCurrentAuthorizationDecision(decision) {
			return false
		}
		if _, duplicate := seen[decision.InterruptID]; duplicate {
			return false
		}
		seen[decision.InterruptID] = struct{}{}
	}
	return true
}

func (turn *CurrentContinueTurn) Clone() *CurrentContinueTurn {
	if turn == nil {
		return nil
	}
	clone := *turn
	clone.HITLDecisions = bytes.Clone(turn.HITLDecisions)
	return &clone
}

func (service *CurrentApplicationStartService) ContinueCurrentAgent(
	ctx context.Context,
	request CurrentContinuationRequest,
) (CurrentApplicationStartOutcome, error) {
	if err := request.Validate(); err != nil {
		return CurrentApplicationStartOutcome{}, err
	}
	target, err := service.continuationResolver.ResolveCurrentContinuation(
		ctx,
		CurrentContinuationResolveRequest{
			ProjectID: request.ProjectID, ActorUserID: request.ActorUserID,
			ConversationUUID:  request.ConversationUUID,
			ResponseMessageID: request.ResponseMessageID,
			Kind:              request.normalizedKind(),
			AuthorizationID:   request.AuthorizationID,
		},
	)
	if err != nil {
		return CurrentApplicationStartOutcome{}, err
	}
	targetKind := target.ContinuationKind
	if targetKind == "" {
		targetKind = CurrentContinuationHITL
	}
	if err := target.Validate(); err != nil || targetKind != request.normalizedKind() ||
		(request.ThreadID != "" && request.ThreadID != target.ThreadID) ||
		request.ProjectID > math.MaxInt32 || request.ActorUserID > math.MaxInt32 ||
		target.TargetParticipantID > math.MaxInt32 {
		return CurrentApplicationStartOutcome{}, ErrUnsupportedCurrentAgentStart
	}
	var decisions []CurrentHITLDecision
	if request.normalizedKind() == CurrentContinuationHITL {
		decisions, err = request.normalizedHITLDecisions()
		if err != nil || !decisionsMatchCurrentHITLInterrupts(decisions, target.HITLInterrupts) {
			return CurrentApplicationStartOutcome{}, ErrUnsupportedCurrentAgentStart
		}
	} else if request.normalizedKind() == CurrentContinuationAuthorization {
		decisions, err = request.normalizedAuthorizationDecisions()
		if err != nil || !decisionsMatchCurrentAuthorizationRequests(decisions, target.AuthorizationRequests) {
			return CurrentApplicationStartOutcome{}, ErrUnsupportedCurrentAgentStart
		}
	}

	input, turn, capabilityID, err := service.currentContinuationInput(ctx, request, target, decisions)
	if err != nil {
		return CurrentApplicationStartOutcome{}, err
	}
	projectID := strconv.FormatInt(request.ProjectID, 10)
	actorID := strconv.FormatInt(request.ActorUserID, 10)
	idempotencyKey := currentContinuationIdempotencyKey(
		request.ResponseMessageID,
		input.HitlDecisions,
		request.AuthorizationID,
		request.Action,
	)
	if request.normalizedKind() == CurrentContinuationOutputLimit {
		idempotencyKey = currentOutputContinuationIdempotencyKey(
			request.ResponseMessageID,
			target.OutputLimitSequence,
		)
	}
	outcome, err := service.admissions.Submit(ctx, SubmitRequest{
		Identity: executionapp.AdmissionIdentity{
			TenantID: projectID, ResourceProjectID: projectID,
			ProjectionProjectID: projectID, ActorID: actorID,
		},
		IdempotencyKey: idempotencyKey,
		CapabilityID:   capabilityID, ClientStreamID: request.ConversationUUID,
		ClientMessageID: request.ResponseMessageID, SIOEvent: "chat_continue_predict",
		Input: input, CurrentContinueTurn: turn,
	})
	if err != nil {
		return CurrentApplicationStartOutcome{}, err
	}
	return CurrentApplicationStartOutcome{
		ExecutionID: outcome.ExecutionID, CommandID: outcome.CommandID,
		ResponseMessageID: request.ResponseMessageID, Created: outcome.Created,
	}, nil
}

func (service *CurrentApplicationStartService) currentContinuationInput(
	ctx context.Context,
	request CurrentContinuationRequest,
	target CurrentContinuationTarget,
	decisions []CurrentHITLDecision,
) (*runtimev1.AgentExecutionInputV1, *CurrentContinueTurn, string, error) {
	projectID, projectIDValid := currentContinuationDatabaseID(request.ProjectID)
	actorUserID, actorUserIDValid := currentContinuationDatabaseID(request.ActorUserID)
	if !projectIDValid || !actorUserIDValid {
		return nil, nil, "", ErrUnsupportedCurrentAgentStart
	}
	turn := &CurrentContinueTurn{
		ProjectID: request.ProjectID, ActorUserID: request.ActorUserID,
		ConversationUUID:    request.ConversationUUID,
		TargetParticipantID: target.TargetParticipantID, Kind: target.Kind,
		QuestionID: target.QuestionID, ResponseMessageID: request.ResponseMessageID,
		ExecutionGeneration: target.ExecutionGeneration, ThreadID: target.ThreadID,
		ContinuationKind:    request.normalizedKind(),
		OutputLimitSequence: target.OutputLimitSequence,
	}
	var input *runtimev1.AgentExecutionInputV1
	var capabilityID string
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
			ProjectID: request.ProjectID, ActorUserID: request.ActorUserID,
			ConversationUUID:    request.ConversationUUID,
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
				ProjectID: projectID, ActorUserID: actorUserID,
				VersionDetails: resolved.VersionDetails,
				InternalTools:  resolved.InternalTools,
			},
		)
		if err != nil {
			return nil, nil, "", err
		}
		resolved.VersionDetails = frozen
		// #870: memory recall is wired on turn START only
		// (StartCurrentApplication, StartCurrentAdhoc). ContinueCurrentAgent
		// resumes an interrupted (e.g. HITL-paused) turn that already began
		// with whatever recall its own start performed; re-running recall
		// here would answer a DIFFERENT question ("what should the model
		// have known") after the model has already partly answered, which
		// is out of this issue's scope. "" is the same no-recall behavior
		// this call site had before #870.
		input, err = currentApplicationInput(start, resolved, suggestionPolicy, toolkitGuardrails, nil, "")
		if err != nil {
			return nil, nil, "", err
		}
		turn.ApplicationID = resolved.ApplicationID
		turn.ApplicationVersionID = resolved.ApplicationVersionID
		capabilityID = executiondomain.AgentApplicationCapability
	case CurrentRegenerationAdhoc:
		start := CurrentAdhocStartRequest{
			ProjectID: request.ProjectID, ActorUserID: request.ActorUserID,
			ConversationUUID:    request.ConversationUUID,
			TargetParticipantID: target.TargetParticipantID,
			QuestionID:          target.QuestionID, UserInput: target.UserInput,
			LLMSettings: json.RawMessage(`{}`),
		}
		resolved, err := service.adhocResolver.ResolveCurrentAdhoc(ctx, start)
		if err != nil {
			return nil, nil, "", err
		}
		snapshot, err := currentAdhocSnapshot(start.LLMSettings, resolved)
		if err != nil {
			return nil, nil, "", err
		}
		frozen, err := service.freezer.FreezeCurrentApplicationVersion(
			ctx,
			CurrentApplicationVersionFreezeRequest{
				ProjectID: projectID, ActorUserID: actorUserID,
				VersionDetails: snapshot,
			},
		)
		if err != nil {
			return nil, nil, "", err
		}
		input, err = currentAdhocInput(start, resolved, frozen, suggestionPolicy, toolkitGuardrails, nil, "") // #870: see currentApplicationInput's continuation call site
		if err != nil {
			return nil, nil, "", err
		}
		capabilityID = executiondomain.AgentAdhocCapability
	default:
		return nil, nil, "", ErrUnsupportedCurrentAgentStart
	}

	input.ThreadId = stringPointer(target.ThreadID)
	input.ExecutionGeneration = stringPointer(target.ExecutionGeneration)
	input.ShouldContinue = true
	if request.normalizedKind() == CurrentContinuationOutputLimit {
		encodedTruncatedContent, err := json.Marshal(target.TruncatedContent)
		if err != nil {
			return nil, nil, "", ErrInvalidCurrentAgentStart
		}
		input.TruncatedContent = encodedTruncatedContent
	} else if request.normalizedKind() == CurrentContinuationAuthorization {
		slices.SortFunc(decisions, func(left, right CurrentHITLDecision) int {
			return strings.Compare(left.InterruptID, right.InterruptID)
		})
		encodedDecisions, err := json.Marshal(decisions)
		if err != nil {
			return nil, nil, "", ErrInvalidCurrentAgentStart
		}
		if len(decisions) == 1 {
			turn.InterruptID = decisions[0].InterruptID
			turn.Action = decisions[0].Action
			input.HitlAction = stringPointer(decisions[0].Action)
			input.HitlValue = stringPointer("")
		}
		turn.HITLDecisions = bytes.Clone(encodedDecisions)
		input.HitlResume = true
		input.HitlDecisions = encodedDecisions
		input.McpTokens = bytes.Clone(request.MCPTokens)
		input.IgnoredMcpServers = bytes.Clone(request.IgnoredMCPServers)
		input.UserDeclinedMcpServers = bytes.Clone(request.DeclinedMCPServers)
	} else {
		for index := range decisions {
			if decisions[index].InterruptID == "" && len(target.HITLInterrupts) == 1 {
				decisions[index].InterruptID = target.HITLInterrupts[0].InterruptID
			}
		}
		// One logical decision set must produce one durable idempotency input even
		// when a browser reconstructs the visible cards in a different order.
		slices.SortFunc(decisions, func(left, right CurrentHITLDecision) int {
			return strings.Compare(left.InterruptID, right.InterruptID)
		})
		encodedDecisions, err := json.Marshal(decisions)
		if err != nil {
			return nil, nil, "", ErrInvalidCurrentAgentStart
		}
		input.HitlResume = true
		if len(decisions) == 1 {
			input.HitlAction = stringPointer(decisions[0].Action)
			input.HitlValue = stringPointer(decisions[0].Value)
			turn.Action = decisions[0].Action
			turn.InterruptID = decisions[0].InterruptID
		}
		input.HitlDecisions = encodedDecisions
		turn.HITLDecisions = bytes.Clone(encodedDecisions)
	}
	return input, turn, capabilityID, nil
}

func currentContinuationDatabaseID(value int64) (int32, bool) {
	if value <= 0 || value > math.MaxInt32 {
		return 0, false
	}
	return int32(value), true
}

func currentRootHITLAction(action string) bool {
	switch action {
	case "approve", "reject", "edit", "block_with_comment", "answer":
		return true
	default:
		return false
	}
}

func currentAuthorizationAction(action string) bool {
	return action == "authorize" || action == "skip"
}

func currentContinuationIdempotencyKey(
	responseID string,
	decisions json.RawMessage,
	authorizationID string,
	authorizationAction string,
) string {
	digest := sha256.New()
	_, _ = digest.Write(decisions)
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(authorizationID))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(authorizationAction))
	sum := digest.Sum(nil)
	return "continue/" + responseID + "/" + hex.EncodeToString(sum[:16])
}

func currentOutputContinuationIdempotencyKey(responseID string, sequence int64) string {
	return "continue-output/" + responseID + "/" + strconv.FormatInt(sequence, 10)
}

func stringPointer(value string) *string {
	return &value
}
