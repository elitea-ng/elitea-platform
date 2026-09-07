package agentexecution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/google/uuid"
)

const maxCurrentAgentUserInputBytes = 256 * 1024

var (
	ErrInvalidCurrentAgentStart                = errors.New("invalid current agent start")
	ErrUnsupportedCurrentAgentStart            = errors.New("current agent start is not supported by the admitted parity slice")
	ErrCurrentAgentRegenerationStillFinalizing = errors.New("current agent response is still being finalized")
)

// CurrentApplicationTurn is the immutable current-chat side of one durable
// application execution admission. The repository rechecks these identities
// and writes the user and streaming response groups in the same transaction as
// elitea_runtime execution/outbox state.
type CurrentApplicationTurn struct {
	ProjectID            int64
	ActorUserID          int64
	ConversationUUID     string
	TargetParticipantID  int64
	ApplicationID        int64
	ApplicationVersionID int64
	QuestionID           string
	QuestionItemID       string
	ResponseMessageID    string
	QuestionMeta         json.RawMessage
	UserInput            string
	// Attachments are the `attachment_message` items written onto the QUESTION
	// group in the same transaction (#606). Empty for a turn with no files.
	Attachments []CurrentTurnAttachment
}

func (turn CurrentApplicationTurn) Validate() error {
	if turn.ProjectID <= 0 || turn.ActorUserID <= 0 || turn.TargetParticipantID <= 0 ||
		turn.ApplicationID <= 0 || turn.ApplicationVersionID <= 0 ||
		!validUUID(turn.ConversationUUID) || !validUUID(turn.QuestionID) ||
		!validUUID(turn.QuestionItemID) || !validUUID(turn.ResponseMessageID) ||
		!validCurrentAgentText(turn.UserInput, maxCurrentAgentUserInputBytes) ||
		!validJSONObject(turn.QuestionMeta) ||
		!validCurrentTurnAttachments(turn.Attachments) {
		return ErrInvalidCurrentAgentStart
	}
	return nil
}

func (turn *CurrentApplicationTurn) Clone() *CurrentApplicationTurn {
	if turn == nil {
		return nil
	}
	clone := *turn
	clone.QuestionMeta = bytes.Clone(turn.QuestionMeta)
	clone.Attachments = cloneCurrentTurnAttachments(turn.Attachments)
	return &clone
}

type CurrentApplicationTarget struct {
	ApplicationID        int64
	ApplicationVersionID int64
	Variables            json.RawMessage
	VersionDetails       json.RawMessage
	ChatHistory          json.RawMessage
	InternalTools        json.RawMessage
}

type CurrentApplicationResolver interface {
	ResolveCurrentApplication(
		context.Context,
		CurrentApplicationStartRequest,
	) (CurrentApplicationTarget, error)
}

// NextInputSuggestionPolicyResolver resolves the current platform's effective,
// project-scoped suggestion policy for one authenticated execution actor. The
// policy is optional execution metadata: an unavailable current dependency must
// never prevent the primary agent turn from being admitted.
type NextInputSuggestionPolicyResolver interface {
	ResolveNextInputSuggestionPolicy(
		context.Context,
		int64,
		int64,
	) (json.RawMessage, error)
}

type admissionSubmitter interface {
	Submit(context.Context, SubmitRequest) (executionapp.AdmissionOutcome, error)
}

type CurrentApplicationStartRequest struct {
	ProjectID           int64
	ActorUserID         int64
	ConversationUUID    string
	TargetParticipantID int64
	QuestionID          string
	UserInput           string
	InteractionUUID     string
	// Attachments carries `payload.attachments` from the start body: the
	// files the composer uploaded before sending, already split into
	// (bucket, name) by the route. #606.
	Attachments []CurrentTurnAttachmentRef
}

func (request CurrentApplicationStartRequest) Validate() error {
	if request.ProjectID <= 0 || request.ActorUserID <= 0 || request.TargetParticipantID <= 0 ||
		!validUUID(request.ConversationUUID) || !validUUID(request.QuestionID) ||
		!validCurrentAgentText(request.UserInput, maxCurrentAgentUserInputBytes) ||
		(request.InteractionUUID != "" && !validUUID(request.InteractionUUID)) {
		return ErrInvalidCurrentAgentStart
	}
	return nil
}

type CurrentApplicationStartOutcome struct {
	ExecutionID       string
	CommandID         string
	ResponseMessageID string
	Created           bool
}

type CurrentApplicationStartService struct {
	resolver             CurrentApplicationResolver
	adhocResolver        CurrentAdhocResolver
	regenerationResolver CurrentRegenerationResolver
	continuationResolver CurrentContinuationResolver
	suggestionResolver   NextInputSuggestionPolicyResolver
	guardrails           CurrentAgentGuardrailResolver
	freezer              CurrentApplicationVersionFreezer
	admissions           admissionSubmitter
}

func NewCurrentApplicationStartService(
	resolver CurrentApplicationResolver,
	adhocResolver CurrentAdhocResolver,
	regenerationResolver CurrentRegenerationResolver,
	continuationResolver CurrentContinuationResolver,
	suggestionResolver NextInputSuggestionPolicyResolver,
	guardrailPolicies CurrentAgentGuardrailResolver,
	freezer CurrentApplicationVersionFreezer,
	admissions admissionSubmitter,
) (*CurrentApplicationStartService, error) {
	if resolver == nil || adhocResolver == nil || regenerationResolver == nil ||
		continuationResolver == nil || suggestionResolver == nil || guardrailPolicies == nil ||
		freezer == nil || admissions == nil {
		return nil, errors.New("current application start dependencies are required")
	}
	return &CurrentApplicationStartService{
		resolver: resolver, adhocResolver: adhocResolver,
		regenerationResolver: regenerationResolver,
		continuationResolver: continuationResolver,
		guardrails:           guardrailPolicies,
		suggestionResolver:   suggestionResolver,
		freezer:              freezer, admissions: admissions,
	}, nil
}

func (service *CurrentApplicationStartService) StartCurrentApplication(
	ctx context.Context,
	request CurrentApplicationStartRequest,
) (CurrentApplicationStartOutcome, error) {
	if err := request.Validate(); err != nil {
		return CurrentApplicationStartOutcome{}, err
	}
	target, err := service.resolver.ResolveCurrentApplication(ctx, request)
	if err != nil {
		return CurrentApplicationStartOutcome{}, err
	}
	if request.ProjectID > math.MaxInt32 || request.ActorUserID > math.MaxInt32 ||
		target.ApplicationID <= 0 || target.ApplicationVersionID <= 0 ||
		!validJSONArray(target.Variables) || !validJSONObject(target.VersionDetails) ||
		!validJSONArray(target.ChatHistory) ||
		(len(target.InternalTools) != 0 && !validJSONArray(target.InternalTools)) {
		return CurrentApplicationStartOutcome{}, ErrUnsupportedCurrentAgentStart
	}
	frozenVersion, err := service.freezer.FreezeCurrentApplicationVersion(
		ctx,
		CurrentApplicationVersionFreezeRequest{
			ProjectID:      int32(request.ProjectID),
			ActorUserID:    int32(request.ActorUserID),
			VersionDetails: target.VersionDetails,
		},
	)
	if err != nil {
		return CurrentApplicationStartOutcome{}, err
	}
	target.VersionDetails = frozenVersion
	questionItemID := currentTurnUUID(request.QuestionID, "question-item")
	responseMessageID := currentTurnUUID(request.QuestionID, "response-message")
	questionMeta := json.RawMessage(`{}`)
	if request.InteractionUUID != "" {
		questionMeta, _ = json.Marshal(map[string]string{"interaction_uuid": request.InteractionUUID})
	}
	attachments, err := currentTurnAttachments(request.QuestionID, request.ConversationUUID, request.Attachments)
	if err != nil {
		return CurrentApplicationStartOutcome{}, err
	}
	suggestionPolicy := service.resolveNextInputSuggestionPolicy(
		ctx,
		request.ProjectID,
		request.ActorUserID,
	)
	toolkitGuardrails, err := service.resolveToolkitGuardrails(ctx)
	if err != nil {
		return CurrentApplicationStartOutcome{}, err
	}
	input, err := currentApplicationInput(
		request, target, suggestionPolicy, toolkitGuardrails, attachments,
	)
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
		IdempotencyKey:  request.QuestionID,
		CapabilityID:    executiondomain.AgentApplicationCapability,
		ClientStreamID:  request.ConversationUUID,
		ClientMessageID: responseMessageID,
		SIOEvent:        "chat_predict",
		Input:           input,
		CurrentTurn: &CurrentApplicationTurn{
			ProjectID: request.ProjectID, ActorUserID: request.ActorUserID,
			ConversationUUID:     request.ConversationUUID,
			TargetParticipantID:  request.TargetParticipantID,
			ApplicationID:        target.ApplicationID,
			ApplicationVersionID: target.ApplicationVersionID,
			QuestionID:           request.QuestionID, QuestionItemID: questionItemID,
			ResponseMessageID: responseMessageID, QuestionMeta: questionMeta,
			UserInput: request.UserInput, Attachments: attachments,
		},
	})
	if err != nil {
		return CurrentApplicationStartOutcome{}, err
	}
	return CurrentApplicationStartOutcome{
		ExecutionID: outcome.ExecutionID, CommandID: outcome.CommandID,
		ResponseMessageID: responseMessageID, Created: outcome.Created,
	}, nil
}

// attachments are the turn's own uploaded files, whose content chunks become
// `input_attachments`. Callers that are RE-running an already-admitted question
// (regenerate.go, continue.go) pass nil on purpose: that question's attachment
// items are already rows on its message group, so they reach the model through
// the chat-history projection (internal/db/queries/agent_chat.sql), and sending
// them here as well would put every chunk in the request twice.
func currentApplicationInput(
	request CurrentApplicationStartRequest,
	target CurrentApplicationTarget,
	nextInputSuggestion json.RawMessage,
	toolkitGuardrails json.RawMessage,
	attachments []CurrentTurnAttachment,
) (*runtimev1.AgentExecutionInputV1, error) {
	skills, err := projectCurrentApplicationSkills(request.UserInput, target.VersionDetails)
	if err != nil {
		return nil, err
	}
	userInput, err := json.Marshal(skills.userInput)
	if err != nil {
		return nil, ErrInvalidCurrentAgentStart
	}
	application, err := json.Marshal(map[string]any{
		"id":              target.ApplicationID,
		"version_id":      target.ApplicationVersionID,
		"variables":       json.RawMessage(target.Variables),
		"version_details": json.RawMessage(skills.versionDetails),
	})
	if err != nil {
		return nil, ErrInvalidCurrentAgentStart
	}
	// One decode serves both readers below. The projection already decoded
	// this document once to build it; decoding it again per reader put three
	// full UseNumber passes over the same bytes on every turn start.
	version, err := decodeCurrentApplicationVersion(skills.versionDetails)
	if err != nil {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	llm, err := currentApplicationRuntimeLLM(version)
	if err != nil {
		return nil, err
	}
	internalTools, err := currentRuntimeInternalTools(target.InternalTools)
	if err != nil {
		return nil, err
	}
	stepsLimit, err := currentApplicationStepsLimit(version)
	if err != nil {
		return nil, err
	}
	threadID := request.ConversationUUID
	conversationID := request.ConversationUUID
	executionGeneration := request.QuestionID
	input := &runtimev1.AgentExecutionInputV1{
		SchemaRevision: "elitea.runtime.agent-execution-input.v1",
		// Current chat history remains authoritative for ordinary turns. The
		// shared LangGraph checkpoint stores resumable graph state for this stable
		// thread; it does not replace the current chat-history projection.
		Llm: llm, ChatHistory: bytes.Clone(target.ChatHistory),
		UserInput: userInput, ThreadId: &threadID, Tools: []byte(`[]`),
		Application: application, InternalTools: internalTools,
		McpTokens: []byte(`{}`), IgnoredMcpServers: []byte(`[]`),
		UserDeclinedMcpServers: []byte(`[]`), HitlDecisions: []byte(`[]`),
		ExecutionGeneration: &executionGeneration, Meta: []byte(`{}`),
		ConversationId: &conversationID, ContextSettings: []byte(`{}`),
		InvokedSkills: skills.invoked, AppliedSkills: skills.applied,
		AttachedSkills:    skills.attached,
		InputAttachments:  currentTurnInputAttachments(attachments),
		ParallelReconcile: []byte(`null`), ParallelTerminalErrors: []byte(`[]`),
		NextInputSuggestion: bytes.Clone(nextInputSuggestion),
		ToolkitGuardrails:   bytes.Clone(toolkitGuardrails),
	}
	if stepsLimit != nil {
		input.StepsLimit = stepsLimit
	}
	return input, nil
}

// currentApplicationStepsLimit lifts the authored step limit out of the frozen
// version's meta and onto the execution input, which is where the runtime reads
// it (services/elitea-worker-rust/src/agents/assembly.rs:152 —
// `request.payload.steps_limit`). The adhoc path has always done this from the
// conversation meta (adhoc.go:308,335-337); the application path did not, so a
// stored agent ran on the runtime's default no matter what its author set.
//
// The key stays in `meta` as well, because the Python worker reads it from
// exactly there to set the LangGraph recursion limit
// (services/elitea-worker-python/src/elitea_worker/agents/sdk_adapter.py:910-912)
// and both workers must keep honouring the same authored number.
//
// An unusable value is refused rather than dropped: a step limit that silently
// became the default is how an agent that was deliberately given room to work
// stops halfway through with no explanation.
func currentApplicationStepsLimit(version map[string]any) (*int32, error) {
	meta, ok := version["meta"].(map[string]any)
	if !ok {
		return nil, nil
	}
	value, exists := meta["step_limit"]
	if !exists || value == nil {
		return nil, nil
	}
	parsed, ok := positiveCurrentAgentJSONInteger(value)
	if !ok || parsed > maxCurrentAgentStepLimit {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	bounded := int32(parsed)
	return &bounded, nil
}

// maxCurrentAgentStepLimit mirrors MAX_AGENT_STEP_LIMIT
// (services/elitea-worker-rust/src/agents/assembly.rs:22). Refusing here rather
// than forwarding turns a runtime-side invalid_profile — which the browser sees
// as a turn that starts and then stops — into a start that fails with a stated
// reason.
const maxCurrentAgentStepLimit = 1024

// currentPlatformInternalTools is the agent form's own authorable catalogue
// (apps/elitea-web/src/features/agents/lib/internalTools.ts) plus `ask_user`.
// Membership here means "the product can do this", not "every worker can":
// BOTH runtimes skip what they cannot serve, with a logged
// `agent_internal_tool_skipped` — the native one for what it has not
// implemented (services/elitea-worker-rust/src/agents/internal_tools.rs), and
// the Python one for what its image cannot build, which today is `pyodide`,
// whose sandbox needs a Deno runtime that image does not ship
// (services/elitea-worker-python/src/elitea_worker/agents/internal_tools.py).
// This layer FORWARDS rather than judges, because refusing here turned every
// form toggle into an agent that stopped answering on both workers at once.
//
// Do not read the skip as "either worker serves everything". It was written
// down here, and in two other files, that the Python worker served the whole
// set; it did not, and the turn died in the SDK with an assistant row flagged
// `is_error` and EMPTY content.
var currentPlatformInternalTools = map[string]bool{
	"ask_user":         true,
	"attachments":      true,
	"data_analysis":    true,
	"image_generation": true,
	"lazy_tools_mode":  true,
	"planner":          true,
	"pyodide":          true,
	"swarm":            true,
}

func currentRuntimeInternalTools(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 {
		return []byte(`[]`), nil
	}
	var configured []string
	if !validJSONArray(raw) || json.Unmarshal(raw, &configured) != nil {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	selected := make([]string, 0, len(configured))
	seen := make(map[string]bool, len(configured))
	for _, name := range configured {
		switch {
		case name == "internal_mcp":
			// Internal MCP is materialized through the frozen tools projection.
		case currentPlatformInternalTools[name]:
			if !seen[name] {
				seen[name] = true
				selected = append(selected, name)
			}
		default:
			// Off the platform catalogue entirely: this names nothing the
			// product can do, so forwarding it would launder malformed
			// configuration into a per-worker decision.
			return nil, ErrUnsupportedCurrentAgentStart
		}
	}
	encoded, err := json.Marshal(selected)
	if err != nil {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	return encoded, nil
}

// resolveToolkitGuardrails marshals the live guardrails policy for the worker.
//
// It FAILS the turn on a read error, which is the opposite of what
// resolveNextInputSuggestionPolicy below does with its own dependency, and the
// difference is not a style choice:
//
//   - a suggestion policy is optional execution metadata. Losing it costs the
//     user a follow-up prompt, so an unavailable dependency degrades to `null`
//     rather than refusing the turn.
//   - a guardrails policy decides which tool calls stop and ask the user for
//     authorization. Its degraded value is "no tool is sensitive", which means
//     the run proceeds and executes, unprompted, exactly the actions an operator
//     marked as requiring approval. There is no safe default to fall back to, so
//     the turn does not start.
//
// The freeze has already read the same policy a few lines earlier and failed the
// turn if it could not, so reaching a failure here means the store went away in
// between. Failing twice for the same reason is correct; silently succeeding the
// second time would make the freeze's guarantee conditional on timing.
func (service *CurrentApplicationStartService) resolveToolkitGuardrails(
	ctx context.Context,
) (json.RawMessage, error) {
	policy, err := service.guardrails.ResolveCurrentAgentGuardrails(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve current toolkit guardrails: %w", err)
	}
	encoded, err := json.Marshal(policy.Runtime())
	if err != nil {
		return nil, fmt.Errorf("encode current toolkit guardrails: %w", err)
	}
	return encoded, nil
}

func (service *CurrentApplicationStartService) resolveNextInputSuggestionPolicy(
	ctx context.Context,
	projectID int64,
	actorUserID int64,
) json.RawMessage {
	policy, err := service.suggestionResolver.ResolveNextInputSuggestionPolicy(
		ctx,
		projectID,
		actorUserID,
	)
	if err != nil || !validJSONObject(policy) {
		return json.RawMessage(`null`)
	}
	return bytes.Clone(policy)
}

func currentApplicationRuntimeLLM(version map[string]any) ([]byte, error) {
	settings, ok := version["llm_settings"].(map[string]any)
	if !ok || settings == nil {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	compatible, ok := settings["openai_compatible"].(bool)
	if !ok {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	result, err := json.Marshal(map[string]any{
		"kwargs": map[string]any{"openai_compatible": compatible},
	})
	if err != nil {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	return result, nil
}

func validUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.String() == strings.ToLower(value)
}

var currentTurnNamespace = uuid.MustParse("71581f1e-fb1b-4d50-a9db-8ebd4b47db76")

func currentTurnUUID(questionID, role string) string {
	return uuid.NewSHA1(currentTurnNamespace, []byte(questionID+"\x00"+role)).String()
}

func validCurrentAgentText(value string, limit int) bool {
	return value != "" && len(value) <= limit && utf8.ValidString(value) &&
		!strings.ContainsRune(value, '\x00')
}

func validJSONObject(value []byte) bool {
	trimmed := bytes.TrimSpace(value)
	return json.Valid(trimmed) && len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}'
}

func validJSONArray(value []byte) bool {
	trimmed := bytes.TrimSpace(value)
	return json.Valid(trimmed) && len(trimmed) >= 2 && trimmed[0] == '[' && trimmed[len(trimmed)-1] == ']'
}

// unsupportedCurrentAgentStart attributes ONE refusal without changing what
// callers match on.
//
// The admission gate in tools.go used to return the bare
// ErrUnsupportedCurrentAgentStart sentinel from every one of its refusal sites,
// so an operator watching a deployment fail saw the same sentence — "current
// agent start is not supported by the admitted parity slice" — whichever
// precondition had actually objected, with nothing to distinguish a malformed
// tool entry from an unresolvable model. The HTTP body is deliberately generic
// and stays that way; the log line must not be (#288).
//
// Unwrap returns the sentinel first so errors.Is(err,
// ErrUnsupportedCurrentAgentStart) — which is how the route maps this to 422 —
// keeps matching, and the cause second so a dependency's own error survives
// into the log instead of being swallowed.
type unsupportedCurrentAgentStart struct {
	reason string
	cause  error
}

func (e *unsupportedCurrentAgentStart) Error() string {
	message := ErrUnsupportedCurrentAgentStart.Error() + ": " + e.reason
	if e.cause != nil {
		message += ": " + e.cause.Error()
	}
	return message
}

func (e *unsupportedCurrentAgentStart) Unwrap() []error {
	if e.cause == nil {
		return []error{ErrUnsupportedCurrentAgentStart}
	}
	return []error{ErrUnsupportedCurrentAgentStart, e.cause}
}

// unsupportedStart names a refusal. The reason must be a fixed string: it
// reaches deployment logs, so it carries no request data.
func unsupportedStart(reason string) error {
	return &unsupportedCurrentAgentStart{reason: reason}
}

// unsupportedStartBecause names a refusal a dependency caused, keeping that
// dependency's error attached.
func unsupportedStartBecause(reason string, cause error) error {
	return &unsupportedCurrentAgentStart{reason: reason, cause: cause}
}

// UnsupportedCurrentAgentStart names a refusal raised OUTSIDE this package.
//
// The repository layer resolves the turn's row and holds several separate
// reasons to refuse it. Every one of them used to answer the bare
// ErrUnsupportedCurrentAgentStart sentinel. An operator then read one sentence
// — "current agent start is not supported by the admitted parity slice" — for
// a missing user participant, a missing version id, a foreign project and six
// other causes, with nothing to tell them apart. The support assistant shipped
// broken for exactly that reason: the log named the sentinel, not the cause.
//
// The reason must be a FIXED string. It reaches deployment logs, so it carries
// no request data. Callers keep matching with
// errors.Is(err, ErrUnsupportedCurrentAgentStart), which the wrapper's Unwrap
// preserves.
func UnsupportedCurrentAgentStart(reason string) error {
	return &unsupportedCurrentAgentStart{reason: reason}
}
