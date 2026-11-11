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

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentmemory"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	scope "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/executionchildscope"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/pipelinelimits"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/turntaking"
	"github.com/google/uuid"
)

const maxCurrentAgentUserInputBytes = 256 * 1024

var (
	ErrInvalidCurrentAgentStart                = errors.New("invalid current agent start")
	ErrUnsupportedCurrentAgentStart            = errors.New("current agent start is not supported by the admitted parity slice")
	ErrCurrentAgentRegenerationStillFinalizing = errors.New("current agent response is still being finalized")
	// ErrLocalWorkThread refuses a server-side send, regenerate or continue in
	// a conversation the desktop's Local work owns (source `local_work`): its
	// turns run against a folder on the person's computer, which a cloud run
	// cannot see. The route answers 409 `local_work_thread`.
	//
	// The fence opens only when the cloud executor is wired and the thread
	// is handed off to it and idle (ADR-0034 decision 7): a handoff row in
	// CLOUD_IDLE with no take back pending.
	ErrLocalWorkThread = errors.New("conversation is a desktop Local work thread")
	// ErrAgentTargetRequired refuses an ad-hoc start that names no
	// participant in a conversation holding more than one agent (ADR-0050
	// phase 0, issue 1602). Without it the start resolved to the
	// conversation's plain model and the agents never saw the message. The
	// route answers 422 `agent_target_required`.
	ErrAgentTargetRequired = errors.New("conversation holds several agents and the start names none")
	// ErrCloudTurnRunning refuses a send in a handed-off thread while a
	// cloud turn runs in it (CLOUD_RUNNING): one turn at a time owns the
	// workspace. The route answers 409 `cloud_turn_running` (retryable).
	ErrCloudTurnRunning = errors.New("a cloud turn is running in this thread")
	// ErrLocalWorkTurn refuses regenerating a turn the desktop ran in a
	// thread the cloud executor now owns (ADR-0034 decision 7). The route
	// answers 409 `local_work_turn`.
	ErrLocalWorkTurn = errors.New("the response was executed on the desktop")
	// ErrCloudTurnNotNewest refuses regenerating a cloud turn other than the
	// newest one of a handed-off thread: only the newest turn's commit can be
	// rewound. The route answers 409 `cloud_turn_not_newest`.
	ErrCloudTurnNotNewest = errors.New("only the newest cloud turn can be regenerated")
	// ErrConversationLegalHold refuses regenerating (and editing) a turn of a
	// conversation an active legal hold covers (ADR-0042 decision 4): the
	// regeneration deletes the stored answer and an edit overwrites the
	// question, both content the hold keeps. The route answers 409
	// `legal_hold`.
	ErrConversationLegalHold = errors.New("conversation is under legal hold")
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
	// AllowEmptyUserInput carries CurrentApplicationStartRequest's flag onto
	// the turn, so the repository's re-validation admits the same empty input
	// the use case admitted.
	AllowEmptyUserInput bool
	// Attachments are the `attachment_message` items written onto the QUESTION
	// group in the same transaction (#606). Empty for a turn with no files.
	Attachments []CurrentTurnAttachment
	// The turn-taking question fields (ADR-0050 decisions 7c and 7d,
	// floor.go). Zero values write the question as before.
	//
	// QuestionAuthorParticipantID: an agent-to-agent turn's question is
	// authored by the handing agent.
	QuestionAuthorParticipantID int64
	// ReplyToQuestionID: a later addressed agent answers the person's
	// existing question; no question is written.
	ReplyToQuestionID string
	// QuestionAddressedTo are the question's addressees; empty derives them
	// from the target.
	QuestionAddressedTo []int64
	// QuestionVisibility is all (empty) or side.
	QuestionVisibility string
	// QuestionCause is empty (derived from the author) or a turntaking cause.
	QuestionCause string
	// ContextView is the view the turn's input carries (issue 1669); the
	// admission records it on the response. nil records nothing.
	ContextView *ContextViewRecord
}

func (turn CurrentApplicationTurn) Validate() error {
	if turn.ProjectID <= 0 || turn.ActorUserID <= 0 || turn.TargetParticipantID <= 0 ||
		turn.ApplicationID <= 0 || turn.ApplicationVersionID <= 0 ||
		!validUUID(turn.ConversationUUID) || !validUUID(turn.QuestionID) ||
		!validUUID(turn.QuestionItemID) || !validUUID(turn.ResponseMessageID) ||
		!validCurrentAgentInput(turn.UserInput, turn.AllowEmptyUserInput) ||
		!validJSONObject(turn.QuestionMeta) ||
		!validCurrentTurnAttachments(turn.Attachments) ||
		turn.QuestionAuthorParticipantID < 0 ||
		(turn.ReplyToQuestionID != "" && !validUUID(turn.ReplyToQuestionID)) ||
		!validAddressedTo(turn.QuestionAddressedTo) ||
		(turn.QuestionVisibility != "" && turn.QuestionVisibility != "all" && turn.QuestionVisibility != "side") {
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
	clone.QuestionAddressedTo = append([]int64(nil), turn.QuestionAddressedTo...)
	clone.ContextView = turn.ContextView.Clone()
	return &clone
}

type CurrentApplicationTarget struct {
	ApplicationID        int64
	ApplicationVersionID int64
	Variables            json.RawMessage
	VersionDetails       json.RawMessage
	// Main resolver-only carrier; never serialized into execution input or the command bus.
	SourceVersionDetails json.RawMessage
	ChatHistory          json.RawMessage
	// ChatHistoryRefs names each ChatHistory entry, in order (ADR-0050
	// decision 9c): the worker reconciles the reader's durable session with
	// them on every turn.
	ChatHistoryRefs []ChatHistoryRef
	// ParticipantContextHistory is the participant's own eligibility mode
	// (entity_settings.context_history: "share history from: now"), or "".
	ParticipantContextHistory string
	InternalTools             json.RawMessage
	OriginalSource            *scope.SourceReference
	// Release is the agent release the run executes (release.go); nil for
	// a run of a draft, of a version published before releases existed, or
	// of the catalogue. When set, VersionDetails and SourceVersionDetails
	// are the release's definition.
	Release *ReleaseRun
	// CloudWorkspace: the conversation is a desktop Local work thread
	// handed off to the cloud executor (CLOUD_IDLE). Its turn has the
	// workspace tools, so an unattended grant may name workspace/git_push
	// and workspace/open_pull_request (ADR-0045).
	CloudWorkspace bool
}

type CurrentApplicationResolver interface {
	ResolveCurrentApplication(
		context.Context,
		CurrentApplicationStartRequest,
	) (CurrentApplicationTarget, error)
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
	// AllowEmptyUserInput admits an EMPTY UserInput. Only the unattended
	// entry points set it — an inbound pipeline trigger and a pipeline
	// schedule (internal/api/v2/pipelinetriggers). A webhook that only says
	// "something happened" carries no text, and a schedule may have none
	// configured. Both used to fail this Validate and surface as an opaque
	// 503 "the pipeline run could not be started" (regression finding
	// UI-PD-3). A typed chat turn never sets it: an empty composer message is
	// still refused. Skill projection sends "continue" to the model for an
	// empty input, which is what a pipeline started from its entry node needs.
	AllowEmptyUserInput bool
	InteractionUUID     string
	MCPTokens           json.RawMessage
	// Attachments carries `payload.attachments` from the start body: the
	// files the composer uploaded before sending, already split into
	// (bucket, name) by the route. #606.
	Attachments []CurrentTurnAttachmentRef
	// MentionedUserIDs are the people this message TAGS (#977) — already
	// deduplicated and bounded by the route. Empty for an ordinary message.
	//
	// They do NOT reach the model: a mention is a notification, not context,
	// and the runtime input is unchanged by one. What they reach is
	// `notifyMentionedUsers`, after admission.
	MentionedUserIDs []int64
	// MentionsEveryone is the composer's `@everyone`. The ids it sends
	// alongside are the CLIENT's view of who that is; membership is
	// re-resolved server-side, so a stale or tampered client cannot notify
	// somebody who is not in the project.
	MentionsEveryone bool
	// TriggerOrigin is how this run started (shared 0140): empty for the chat
	// composer, `api` for an MCP client, `schedule` or `webhook` for the
	// unattended pipeline entry points. The analytics active-user reads use
	// it to stop counting a person as active because a cron job ran under
	// their name (legacy issues 6802 and 6881). It never changes what the
	// run does, who it runs as, or who pays for it.
	TriggerOrigin executiondomain.TriggerOrigin
	// ProjectSkillIDs are the project skills the composer's `~` menu picked,
	// by id and nothing else (at most five, deduplicated by the route). The
	// service reads each skill's base version itself under the caller's own
	// visibility (CurrentProjectSkillResolver) and never takes skill text
	// from the request. Empty for every turn that picked none.
	ProjectSkillIDs []int64
	// ReleaseID names the agent release to execute (ADR-0040 decision 2):
	// a release of the target participant's application, published or
	// withdrawn. Zero runs the participant's version, which is the
	// application's published release when that version is published.
	ReleaseID int64
	// AssertedScope is the scope of the caller's verified delegation
	// assertion (ADR-0044 decision 4); nil without one. See
	// SubmitRequest.AssertedScope.
	AssertedScope *AssertedScope
	// Unattended is the grant the person gives at the start (ADR-0045);
	// nil for an attended run.
	Unattended *unattended.Request
	// UnattendedContinuation is set only when Main starts a step-limit
	// continuation of a chain (unattendedchain); never from a request body.
	UnattendedContinuation *UnattendedContinuation
	// HandoffCompletion is set only when Main starts the turn a send
	// continues with (handoff_continue.go); never from a request body.
	HandoffCompletion *HandoffCompletion
	// ThreadRun is set only for a schedule's or a trigger's turn on a
	// handed-off thread (thread_run.go); never from a request body.
	ThreadRun *ThreadRun
	// AddressedTo are the agents this message is for (ADR-0050 decision
	// 7c): at most 16 positive unique participant ids. Empty: the floor
	// decides.
	AddressedTo []int64
	// FloorTurn is set only when Main's floor hook starts a later agent
	// turn of a human message (floor_hook.go); never from a request body.
	FloorTurn *FloorTurn
	// Heartbeat is set only when a proactive agent's heartbeat starts an
	// act (ADR-0050 decision 2, internal/application/heartbeat); never from
	// a request body. The turn binds only its unattended tools and its
	// question carries cause heartbeat. It needs the schedule origin.
	Heartbeat bool
	// projectSkills is ProjectSkillIDs, read and frozen by the service for
	// this one turn. Unexported so no caller can hand the service skill
	// content of its own.
	projectSkills []map[string]any
}

func (request CurrentApplicationStartRequest) Validate() error {
	if request.ProjectID <= 0 || request.ActorUserID <= 0 || request.TargetParticipantID <= 0 ||
		!request.TriggerOrigin.Valid() ||
		!validUUID(request.ConversationUUID) || !validUUID(request.QuestionID) ||
		!validCurrentAgentInput(request.UserInput, request.AllowEmptyUserInput) ||
		(request.InteractionUUID != "" && !validUUID(request.InteractionUUID)) ||
		!validCurrentMCPTokens(request.MCPTokens) ||
		!validProjectSkillIDs(request.ProjectSkillIDs) || request.ReleaseID < 0 ||
		!request.ThreadRun.valid(request.TriggerOrigin) ||
		!validAddressedTo(request.AddressedTo) || !request.FloorTurn.valid() ||
		(request.FloorTurn != nil && (len(request.AddressedTo) > 0 || request.Unattended != nil)) ||
		(request.Heartbeat && (request.TriggerOrigin != executiondomain.TriggerOriginSchedule ||
			request.FloorTurn != nil || request.ThreadRun != nil || len(request.AddressedTo) > 0)) {
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
	originalSources      OriginalRootSourceOwner
	resolver             CurrentApplicationResolver
	adhocResolver        CurrentAdhocResolver
	regenerationResolver CurrentRegenerationResolver
	continuationResolver CurrentContinuationResolver
	guardrails           CurrentAgentGuardrailResolver
	freezer              CurrentApplicationVersionFreezer
	admissions           admissionSubmitter
	// memories is optional — attached after construction via WithMemories
	// (#870, memories.go). A service nobody attaches it to injects no
	// long-term memory, exactly its pre-#870 behavior.
	contextPolicy CurrentContextPolicySource
	memories      CurrentMemoryRecallResolver
	// agentMemory is the agent's own memory (#1607, agent_memory.go).
	agentMemory AgentMemoryRecallResolver
	// projectContext is optional — attached after construction via
	// WithProjectContext (#946, projectcontext.go). A service nobody attaches
	// it to injects no project context, which is what every test that
	// predates that file expects.
	projectContext CurrentProjectContextTextResolver
	// mentions is optional — attached after construction via
	// WithMentionNotifications (#977, mentions.go). A service nobody attaches
	// it to writes no mention rows, which is the behaviour every test that
	// predates that file expects.
	mentions MentionNotificationWriter
	// attachmentImages is optional — attached after construction via
	// WithAttachmentImages (#979, attachments.go). A service nobody attaches
	// it to embeds no image, which is the behaviour every test that predates
	// it expects and the honest answer on a deployment with no object store.
	attachmentImages CurrentAttachmentImageReader
	// projectSkills is optional — attached via WithProjectSkills
	// (project_skills.go). A service without it refuses a request that picks
	// a project skill.
	projectSkills CurrentProjectSkillResolver
	// governedRefusals is optional — attached via
	// WithGovernedRefusalSettlement (governed_refusal.go, #1485). A service
	// without it answers a refused governed continuation and leaves the
	// pause open.
	governedRefusals GovernedRefusalSettler
	// pipelineApproval is optional — attached via WithPipelineApproval
	// (pipeline_approval.go, ADR-0041 decision 5). A service without it
	// does not check the pipeline hitl rule.
	pipelineApproval PipelineApprovalAdmission
	// pipelineChildren reads the saved child versions the check covers.
	pipelineChildren PipelineChildVersionSource
	// unattended admits unattended grants (WithUnattendedGrants, ADR-0045).
	unattended *UnattendedGrantPolicy
	// runBriefs writes the run brief of a new chain (WithRunBriefs,
	// ADR-0045 decision 11).
	runBriefs RunBriefStarter
	// floor reads a conversation's floor (WithFloor, floor.go); nil turns
	// the turn-taking engine off.
	floor FloorStore
}

// WithAttachmentImages attaches the reader that lets an attached IMAGE reach
// the model as bytes (#979), in the same after-construction idiom WithMemories
// and WithProjectContext use and for the same reason: every existing
// constructor call site keeps working, and a service without it behaves
// exactly as this package did before.
func (service *CurrentApplicationStartService) WithAttachmentImages(
	images CurrentAttachmentImageReader,
) *CurrentApplicationStartService {
	service.attachmentImages = images
	return service
}

func NewCurrentApplicationStartService(
	resolver CurrentApplicationResolver,
	adhocResolver CurrentAdhocResolver,
	regenerationResolver CurrentRegenerationResolver,
	continuationResolver CurrentContinuationResolver,
	guardrailPolicies CurrentAgentGuardrailResolver,
	freezer CurrentApplicationVersionFreezer,
	admissions admissionSubmitter,
) (*CurrentApplicationStartService, error) {
	if resolver == nil || adhocResolver == nil || regenerationResolver == nil ||
		continuationResolver == nil || guardrailPolicies == nil ||
		freezer == nil || admissions == nil {
		return nil, errors.New("current application start dependencies are required")
	}
	return &CurrentApplicationStartService{
		resolver: resolver, adhocResolver: adhocResolver,
		regenerationResolver: regenerationResolver,
		continuationResolver: continuationResolver,
		guardrails:           guardrailPolicies,
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
	// ADR-0050 decision 7c: the floor routes a human message before the
	// target is resolved; the first agent it names answers now.
	floor, err := service.planFloor(ctx, &request)
	if err != nil {
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
	if err = currentPipelineStartBound(target.VersionDetails); err != nil {
		return CurrentApplicationStartOutcome{}, unsupportedStartBecause("pipeline exceeds a size bound", err)
	}
	if err = service.captureApplicationSource(ctx, request.ProjectID, request.ActorUserID, &target); err != nil {
		return CurrentApplicationStartOutcome{}, err
	}
	frozenVersion, contextSettings, err := service.freezeVersionWithContext(
		ctx,
		CurrentApplicationVersionFreezeRequest{
			ProjectID:      int32(request.ProjectID),
			ActorUserID:    int32(request.ActorUserID),
			VersionDetails: target.VersionDetails,
			InternalTools:  target.InternalTools,
		},
		request.ConversationUUID,
		target.ApplicationID,
	)
	if err != nil {
		return CurrentApplicationStartOutcome{}, err
	}
	target.VersionDetails = frozenVersion
	questionItemID := currentTurnUUID(request.QuestionID, "question-item")
	responseMessageID := currentTurnUUID(request.QuestionID, "response-message")
	questionMeta := request.ThreadRun.questionMeta(
		currentQuestionMeta(request.InteractionUUID, request.MentionedUserIDs, request.MentionsEveryone))
	attachments, err := currentTurnAttachmentsWithImages(
		ctx,
		int64(request.ProjectID),
		service.attachmentImages,
		request.QuestionID,
		request.ConversationUUID,
		request.Attachments,
	)
	if err != nil {
		return CurrentApplicationStartOutcome{}, err
	}
	toolkitGuardrails, err := service.resolveToolkitGuardrails(ctx)
	if err != nil {
		return CurrentApplicationStartOutcome{}, err
	}
	// Project skills the composer picked: read here, by id, under the caller's
	// visibility. A pick the caller cannot read refuses the turn.
	request.projectSkills, err = service.resolveProjectSkills(ctx, request)
	if err != nil {
		return CurrentApplicationStartOutcome{}, err
	}
	// #870: recalled BEFORE building the input so its text can ride the same
	// `instructions` field the worker already decodes — see memories.go's
	// CurrentMemoryRecallResolver comment for why this is not a new wire
	// field. Fails open: a memory-store hiccup costs this turn its recalled
	// context, never the turn itself.
	memoryRecall := service.resolveCurrentMemoryRecall(
		ctx, request.ProjectID, request.ActorUserID,
		currentMemoryRecallUserInputText(request.UserInput),
	)
	// #946: the project's own standing context, read the same way and for the
	// same reason — it rides the turn's `instructions`, and a failed read
	// costs this turn its project context, never the turn. This REPLACED an
	// admission gate that refused every turn in a context-bearing project;
	// see projectcontext.go's header.
	projectContextText := service.resolveCurrentProjectContextText(ctx, request.ProjectID)
	// #1607: the agent's own notes, after the person's memories.
	agentMemory := service.resolveAgentMemory(
		ctx, request.ProjectID, target.ApplicationID, request.ConversationUUID,
		currentMemoryRecallUserInputText(request.UserInput), target.VersionDetails,
	)
	if agentMemory.Text != "" {
		// #1826: the other injected blocks share the instructions with the
		// agent's notes, so none of them may show a `[memory N, ...]` mark.
		memoryRecall.Text = agentmemory.Neutralize(memoryRecall.Text)
		projectContextText = agentmemory.Neutralize(projectContextText)
	}
	modelRequest := request
	modelRequest.UserInput = floorModelInput(request)
	input, view, err := currentApplicationInputWithView(
		modelRequest, target, toolkitGuardrails, attachments,
		joinRecallText(memoryRecall.Text, agentMemory.Text), projectContextText,
	)
	if err != nil {
		return CurrentApplicationStartOutcome{}, err
	}
	input.ContextSettings = contextSettings
	input.Floor = heartbeatFloor(floor.input(request.TargetParticipantID), request.Heartbeat)
	input.CurrentMessageId = currentMessageID(request)
	// #1826: the entries the instructions show, so a brief can name them.
	input.RecalledMemory = recalledMemoryInput(agentMemory, input.GetApplication())
	grant, chain, err := service.admitUnattended(ctx, unattendedTarget{
		request: request.Unattended, continuation: request.UnattendedContinuation,
		capabilityID: executiondomain.AgentApplicationCapability, input: input,
		projectID: request.ProjectID, actorUserID: request.ActorUserID, applicationID: target.ApplicationID,
		conversationUUID: request.ConversationUUID, grantKey: request.QuestionID,
		responseMessageID: responseMessageID, questionID: request.QuestionID,
		triggerOrigin: request.TriggerOrigin, cloudWorkspace: target.CloudWorkspace,
		start: &unattended.CloudStart{
			Contract: CloudStartContractApplication, ParticipantID: request.TargetParticipantID,
			Heartbeat: request.Heartbeat,
		},
	})
	if err != nil {
		return CurrentApplicationStartOutcome{}, err
	}
	input.UnattendedGrant = grant
	if request.UnattendedContinuation != nil {
		questionMeta = markContinuedAutomatically(questionMeta)
	}
	currentTurn := &CurrentApplicationTurn{
		ProjectID: request.ProjectID, ActorUserID: request.ActorUserID,
		ConversationUUID:     request.ConversationUUID,
		TargetParticipantID:  request.TargetParticipantID,
		ApplicationID:        target.ApplicationID,
		ApplicationVersionID: target.ApplicationVersionID,
		QuestionID:           request.QuestionID, QuestionItemID: questionItemID,
		ResponseMessageID: responseMessageID, QuestionMeta: questionMeta,
		UserInput: request.UserInput, AllowEmptyUserInput: request.AllowEmptyUserInput,
		Attachments: attachments,
		ContextView: newContextViewRecord(request.TargetParticipantID, target.ParticipantContextHistory, view, input),
	}
	applyFloorTurn(currentTurn, floor, request)
	if request.Heartbeat {
		currentTurn.QuestionCause = turntaking.CauseHeartbeat
	}
	projectID := strconv.FormatInt(request.ProjectID, 10)
	actorID := strconv.FormatInt(request.ActorUserID, 10)
	outcome, err := service.admissions.Submit(ctx, SubmitRequest{
		Floor:      floor.admission(request),
		Unattended: chain,
		Identity: executionapp.AdmissionIdentity{
			TenantID: projectID, ResourceProjectID: projectID,
			ProjectionProjectID: projectID, ActorID: actorID,
		},
		IdempotencyKey:    request.QuestionID,
		CapabilityID:      executiondomain.AgentApplicationCapability,
		ClientStreamID:    request.ConversationUUID,
		ClientMessageID:   responseMessageID,
		SIOEvent:          "chat_predict",
		Input:             input,
		TriggerOrigin:     request.TriggerOrigin,
		AssertedScope:     request.AssertedScope,
		Release:           target.Release.Clone(),
		HandoffCompletion: request.HandoffCompletion.Clone(),
		CloudThread:       target.CloudWorkspace,
		RunPolicy:         request.ThreadRun.snapshot(int64(request.ProjectID), request.ConversationUUID, request.TriggerOrigin),
		CurrentTurn:       currentTurn,
	})
	if err != nil {
		return CurrentApplicationStartOutcome{}, err
	}
	// Best-effort, AFTER admission — see recordCurrentMemoryUsage's own
	// comment for why this never affects the turn's outcome.
	service.recordCurrentMemoryUsage(ctx, request.ProjectID, responseMessageID, memoryRecall)
	service.beginRunBrief(ctx, chain, outcome.ExecutionID, request.UserInput)
	// #977, and best-effort for the same reason: a notification that could not
	// be written must cost the mention, never the turn. The message is already
	// sent and stored by the time this runs, so a failure here leaves a
	// conversation that is correct and a colleague who was not told — which is
	// the pre-#977 behaviour, not a new failure mode.
	//
	// Only a NEW admission notifies. A replay of the same question_id is a
	// client retrying a response it lost (a mobile outbox does exactly this),
	// and it is the same message: notifying again would ring the same bell
	// once per retry.
	if outcome.Created {
		service.notifyMentionedUsers(ctx, request)
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
	toolkitGuardrails json.RawMessage,
	attachments []CurrentTurnAttachment,
	memoryText string,
	projectContextText string,
) (*runtimev1.AgentExecutionInputV1, error) {
	input, _, err := currentApplicationInputWithView(request, target, toolkitGuardrails, attachments, memoryText, projectContextText)
	return input, err
}

// currentApplicationInputWithView also returns the reader's view, which the turn
// records (issue 1669).
func currentApplicationInputWithView(
	request CurrentApplicationStartRequest,
	target CurrentApplicationTarget,
	toolkitGuardrails json.RawMessage,
	attachments []CurrentTurnAttachment,
	memoryText string,
	projectContextText string,
) (*runtimev1.AgentExecutionInputV1, ReaderView, error) {
	skills, err := projectCurrentApplicationSkills(request.UserInput, target.VersionDetails, request.projectSkills)
	if err != nil {
		return nil, ReaderView{}, err
	}
	userInput, err := json.Marshal(skills.userInput)
	if err != nil {
		return nil, ReaderView{}, ErrInvalidCurrentAgentStart
	}
	// #870: appended AFTER skill processing, onto version_details as it
	// stands, so recalled memory text is never itself scanned for
	// `[[skill:...]]` markers — see appendCurrentApplicationMemories's own
	// comment.
	versionDetails := appendCurrentApplicationMemories(skills.versionDetails, memoryText)
	// #946: after memories, and after skill processing for the same reason —
	// injected text is never itself scanned for `[[skill:...]]` markers. The
	// per-agent "Ignore Project Context" toggle is consulted inside
	// appendCurrentApplicationProjectContext, off the frozen version's own
	// meta (ELITEA-0945).
	versionDetails = appendCurrentApplicationProjectContext(versionDetails, projectContextText)
	versionDetails, err = freezeCurrentHTTPActionRequests(target.ApplicationID, target.ApplicationVersionID, versionDetails, target.SourceVersionDetails)
	if err != nil {
		// A pipeline the shared admission refuses (a size bound, a node type
		// this deployment does not run, an unattached toolkit) keeps the
		// unsupported-start identity and also carries the typed refusal, so
		// the route can name it.
		if pipelinelimits.Refusal(err) != nil {
			return nil, ReaderView{}, unsupportedStartBecause("pipeline refused by start admission", err)
		}
		return nil, ReaderView{}, ErrUnsupportedCurrentAgentStart
	}
	applicationFields := map[string]any{
		"id":              target.ApplicationID,
		"version_id":      target.ApplicationVersionID,
		"variables":       json.RawMessage(target.Variables),
		"version_details": json.RawMessage(versionDetails),
	}
	if target.OriginalSource != nil {
		if target.OriginalSource.Validate() != nil {
			return nil, ReaderView{}, ErrUnsupportedCurrentAgentStart
		}
		applicationFields["source_definition"] = target.OriginalSource
	}
	application, err := json.Marshal(applicationFields)
	if err != nil {
		return nil, ReaderView{}, ErrInvalidCurrentAgentStart
	}
	// One decode serves both readers below. The projection already decoded
	// this document once to build it; decoding it again per reader put three
	// full UseNumber passes over the same bytes on every turn start.
	version, err := decodeCurrentApplicationVersion(skills.versionDetails)
	if err != nil {
		return nil, ReaderView{}, ErrUnsupportedCurrentAgentStart
	}
	llm, err := currentApplicationRuntimeLLM(version)
	if err != nil {
		return nil, ReaderView{}, err
	}
	internalTools, err := currentRuntimeInternalTools(target.InternalTools)
	if err != nil {
		return nil, ReaderView{}, err
	}
	stepsLimit, err := currentApplicationStepsLimit(version)
	if err != nil {
		return nil, ReaderView{}, err
	}
	projectContext, err := currentFrozenProjectContext(version)
	if err != nil {
		return nil, ReaderView{}, err
	}
	modelContextLimits, err := currentFrozenModelContextLimits(version)
	if err != nil {
		return nil, ReaderView{}, err
	}
	modelReasoning, err := currentFrozenModelReasoning(version)
	if err != nil {
		return nil, ReaderView{}, err
	}
	summaryModel, err := currentFrozenSummaryModel(version)
	if err != nil {
		return nil, ReaderView{}, err
	}
	subagentPolicy, err := currentFrozenSubagentPolicy(version)
	if err != nil {
		return nil, ReaderView{}, err
	}
	// ADR-0050 decision 9 (issue 1609): the history is this reader's view.
	view, err := BuildReaderView(
		readerViewSettings(version, target.ParticipantContextHistory),
		target.ChatHistory, target.ChatHistoryRefs,
	)
	if err != nil {
		return nil, ReaderView{}, ErrUnsupportedCurrentAgentStart
	}
	threadID := request.ConversationUUID
	conversationID := request.ConversationUUID
	executionGeneration := request.QuestionID
	input := &runtimev1.AgentExecutionInputV1{
		SchemaRevision:     "elitea.runtime.agent-execution-input.v1",
		ProjectContext:     projectContext,
		ModelContextLimits: modelContextLimits,
		ModelReasoning:     modelReasoning,
		SummaryModel:       summaryModel,
		SubagentPolicy:     subagentPolicy,
		// Current chat history remains authoritative for ordinary turns. The
		// shared LangGraph checkpoint stores resumable graph state for this stable
		// thread; it does not replace the current chat-history projection.
		Llm: llm, ChatHistory: view.History,
		ChatHistoryRefs: chatHistoryRefsInput(view.Refs),
		UserInput:       userInput, ThreadId: &threadID, Tools: []byte(`[]`),
		Application: application, InternalTools: internalTools,
		McpTokens: currentMCPTokens(request.MCPTokens), IgnoredMcpServers: []byte(`[]`),
		UserDeclinedMcpServers: []byte(`[]`), HitlDecisions: []byte(`[]`),
		ExecutionGeneration: &executionGeneration, Meta: []byte(`{}`),
		ConversationId: &conversationID, ContextSettings: []byte(`{}`),
		InvokedSkills: skills.invoked, AppliedSkills: skills.applied,
		AttachedSkills:    skills.attached,
		InputAttachments:  currentTurnInputAttachments(attachments),
		ParallelReconcile: []byte(`null`), ParallelTerminalErrors: []byte(`[]`),
		NextInputSuggestion: noNextInputSuggestion(),
		ToolkitGuardrails:   bytes.Clone(toolkitGuardrails),
	}
	if stepsLimit != nil {
		input.StepsLimit = stepsLimit
	}
	input.ContextView = contextViewInput(view.Settings, modelContextLimits, input.ChatHistory, input.ChatHistoryRefs)
	return input, view, nil
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
// `agent_internal_tool_skipped` — the native one for the five it has not
// implemented (data_analysis, image_generation, internal_mcp, pyodide, swarm;
// services/elitea-worker-rust/src/agents/internal_tools.rs). It serves
// planner, lazy_tools_mode (Smart Tools Selection), attachments and subagents itself. The Python worker's image now ships everything in this
// map (#872 gave it a pinned Deno runtime plus the SDK's own sandbox
// entrypoint, closing the one gap it had —
// services/elitea-worker-python/src/elitea_worker/agents/internal_tools.py).
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
	// The two chat-authored builder modules (#940 A8). Unlike every other
	// name here they are served by the NATIVE runtime and skipped by the
	// Python worker — the reverse of the rest of this map — which is why
	// this layer still forwards rather than judges: the same reason the doc
	// comment above gives, read the other way round.
	"project_context_builder": true,
	"pyodide":                 true,
	"skills_builder":          true,
	// The runtime's read-only sub-agents (#1226), served natively.
	"subagents": true,
	"swarm":     true,
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
		case currentBuilderFlag(name):
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
// resolveCurrentMemoryRecall (memories.go) does with its own dependency, and
// the difference is not a style choice:
//
//   - recalled memories are optional context. Losing them costs the user that
//     context for one turn, so an unavailable store degrades to "no memories"
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

// noNextInputSuggestion is the next_input_suggestion every turn carries. The
// worker reads the field as an optional policy and does nothing for `null`;
// the platform owns no suggestion policy (#334), so there is nothing to send.
func noNextInputSuggestion() []byte {
	return []byte(`null`)
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

// validUUID admits exactly the canonical lowercase spelling. question_id is
// the turn's idempotency key and is compared as a string, while the database
// stores it as a uuid: an upper-case spelling of the same id would be a
// second key for one stored question. API_CONTRACT.md promises clients a 400
// for it (ADR-0025 coordinator decision 14).
func validUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.String() == value
}

// validStoredUUID is the rule for an id read back from storage rather than
// sent by a client: any case spelling of a canonical UUID. Turns admitted
// before validUUID became lowercase-only stored their question_id verbatim as
// the execution generation, and a paused one must still continue. The value is
// not normalised: downstream compares it as a string with what was stored.
func validStoredUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.String() == strings.ToLower(value)
}

var currentTurnNamespace = uuid.MustParse("71581f1e-fb1b-4d50-a9db-8ebd4b47db76")

func currentTurnUUID(questionID, role string) string {
	return uuid.NewSHA1(currentTurnNamespace, []byte(questionID+"\x00"+role)).String()
}

// validCurrentAgentInput is the user-input rule for an application start. An
// empty value passes only when the caller opted in (AllowEmptyUserInput);
// every other rule — size, UTF-8, no NUL — applies either way.
func validCurrentAgentInput(value string, allowEmpty bool) bool {
	if value == "" {
		return allowEmpty
	}
	return validCurrentAgentText(value, maxCurrentAgentUserInputBytes)
}

func validCurrentAgentText(value string, limit int) bool {
	return value != "" && len(value) <= limit && utf8.ValidString(value) &&
		!strings.ContainsRune(value, '\x00')
}

// currentPipelineStartBound names the shared pipeline size bound before any
// start work. The root-source capture that runs next has its own, larger bound
// (executionchildscope.MaxDefinitionBytes) and refuses with the generic
// unsupported-start text, so an over-size pipeline must be refused here to be
// told which limit it exceeds. Only the byte length is tested: the node count
// cannot fail the capture, and the HTTP freeze pre-scan already names it, so a
// second YAML parse per start would buy nothing.
func currentPipelineStartBound(versionDetails json.RawMessage) error {
	var version struct {
		AgentType    string          `json:"agent_type"`
		Instructions json.RawMessage `json:"instructions"`
	}
	if json.Unmarshal(versionDetails, &version) != nil || version.AgentType != "pipeline" {
		return nil
	}
	// A decoded string is never longer than its quoted JSON form, so only an
	// over-bound raw value is unquoted to learn its exact length.
	if len(version.Instructions) <= pipelinelimits.MaxInstructionsBytes {
		return nil
	}
	var instructions string
	if json.Unmarshal(version.Instructions, &instructions) == nil && len(instructions) > pipelinelimits.MaxInstructionsBytes {
		return pipelinelimits.ErrInstructionsTooLarge
	}
	return nil
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
