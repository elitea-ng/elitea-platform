package storage

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentmemory"
)

// The agent's own memory tools (ADR-0050 decision 1b, issue 1607):
// remember, recall and forget, served on the private claim-bound listener
// for the reason the builder writes are (runtime_entity_builder.go).
//
// WHAT AUTHORIZES A CALL. The claim, through the agent-scoped authorizer.
// The project, the agent (agent_execution_jobs.application_id), the actor,
// the conversation, the response message and the version's memory block all
// come from the claimed execution; the request carries only the content. A
// run with no agent (an ad hoc chat) has no memory and is refused.
//
// WHAT MAIN DECIDES, not the worker:
//
//   - the status a write lands in (agentmemory.DecideStatus): Main knows
//     whether a person started the run (trigger_origin manual and not a link
//     of an unattended chain) and whether the project is personal. The worker
//     says whether it read tool output first; that flag can only make a
//     write pending, never active.
//   - the secret scan (agentmemory.CheckContent), whatever the worker did.
//   - the taint rule: after personal memory was read in the conversation
//     (memory_conversation_taints), a write must be about the asking person.
//   - who an entry may be about: only the asking person, and only if that
//     person has not opted out.
//   - presence: an entry about a person is recalled, and can be forgotten,
//     only while that person is in the conversation.
const (
	RuntimeAgentMemoryRememberSchemaVersion = "elitea.runtime.agent-memory-remember.v1"
	RuntimeAgentMemoryRecallSchemaVersion   = "elitea.runtime.agent-memory-recall.v1"
	RuntimeAgentMemoryForgetSchemaVersion   = "elitea.runtime.agent-memory-forget.v1"

	maxRuntimeAgentMemoryRequestBytes  = 8 * 1024
	maxRuntimeAgentMemoryResponseBytes = 64 * 1024
	maxRuntimeAgentMemoryQueryBytes    = 1024

	runtimeContextStageAgentMemoryRun    = "agent_memory_run"
	runtimeContextStageAgentMemoryWrite  = "agent_memory_write"
	runtimeContextStageAgentMemoryRecall = "agent_memory_recall"
)

// RuntimeAgentMemoryRememberRequest is the remember tool's body.
type RuntimeAgentMemoryRememberRequest struct {
	Kind    string `json:"kind"`
	Content string `json:"content"`
	// AboutUser makes the entry about the person who asked this turn.
	AboutUser bool `json:"about_user"`
	// FromToolOutput: the run read tool or document output before this
	// write. It can only make the write pending.
	FromToolOutput bool `json:"from_tool_output"`
	// TargetApplicationID and TargetVersionID optionally address a nested
	// saved agent reached from the claimed execution (FU-353). Both must be
	// omitted (0) for a root call, or both positive for a nested call. Main
	// proves composition reachability and principal visibility server-side.
	TargetApplicationID int64 `json:"target_application_id,omitempty"`
	TargetVersionID     int64 `json:"target_version_id,omitempty"`
}

// RuntimeAgentMemoryRecallRequest is the recall tool's body.
type RuntimeAgentMemoryRecallRequest struct {
	Query               string `json:"query"`
	Limit               int    `json:"limit"`
	TargetApplicationID int64  `json:"target_application_id,omitempty"`
	TargetVersionID     int64  `json:"target_version_id,omitempty"`
}

// RuntimeAgentMemoryForgetRequest is the forget tool's body.
type RuntimeAgentMemoryForgetRequest struct {
	EntryID             string `json:"entry_id"`
	TargetApplicationID int64  `json:"target_application_id,omitempty"`
	TargetVersionID     int64  `json:"target_version_id,omitempty"`
}

// AgentMemoryTarget names a nested saved agent reached from the claimed run.
// Zero values target the claimed run's own root application.
type AgentMemoryTarget struct {
	ApplicationID int64
	VersionID     int64
}

// parseAgentMemoryTarget enforces that either both target ids are omitted
// (root call) or both are positive 32-bit application/version ids (nested).
func parseAgentMemoryTarget(applicationID, versionID int64) (AgentMemoryTarget, error) {
	if applicationID == 0 && versionID == 0 {
		return AgentMemoryTarget{}, nil
	}
	if applicationID <= 0 || applicationID > math.MaxInt32 ||
		versionID <= 0 || versionID > math.MaxInt32 {
		return AgentMemoryTarget{}, ErrContentRejected
	}
	return AgentMemoryTarget{
		ApplicationID: applicationID,
		VersionID:     versionID,
	}, nil
}

// RuntimeAgentMemoryRemembered is the remember response. Its fields are the
// complete set the worker accepts.
type RuntimeAgentMemoryRemembered struct {
	SchemaVersion string `json:"schema_version"`
	ProjectID     int64  `json:"project_id"`
	EntryID       string `json:"entry_id"`
	Status        string `json:"status"`
}

// RuntimeAgentMemoryEntry is one recalled entry.
type RuntimeAgentMemoryEntry struct {
	EntryID   string `json:"entry_id"`
	Kind      string `json:"kind"`
	Content   string `json:"content"`
	AboutUser string `json:"about_user,omitempty"`
	Pinned    bool   `json:"pinned"`
}

// RuntimeAgentMemoryRecalled is the recall response.
type RuntimeAgentMemoryRecalled struct {
	SchemaVersion string                    `json:"schema_version"`
	ProjectID     int64                     `json:"project_id"`
	Entries       []RuntimeAgentMemoryEntry `json:"entries"`
}

// RuntimeAgentMemoryForgotten is the forget response.
type RuntimeAgentMemoryForgotten struct {
	SchemaVersion string `json:"schema_version"`
	ProjectID     int64  `json:"project_id"`
	Forgotten     bool   `json:"forgotten"`
}

// AgentMemoryRun is what the claimed execution says about a memory call.
type AgentMemoryRun struct {
	ProjectID     int64
	ApplicationID int64
	// DelegatedFromApplicationID is non-zero when this call targets a nested
	// saved agent reached from a root application (FU-353); recorded in the
	// security audit event's reasons for provenance.
	DelegatedFromApplicationID int64
	ActorID                    int64
	// ConversationUUID and MessageID are the provenance of a write: the
	// conversation and the response message of the run.
	ConversationUUID string
	MessageID        string
	ExecutionID      string
	Settings         agentmemory.Settings
	// Attended: a person started the run in the product.
	Attended        bool
	PersonalProject bool
	// Tainted: personal memory was read in the conversation.
	Tainted bool
	// Present are the people in the conversation, by user id, with the
	// name a recalled entry shows.
	Present map[int64]string
}

// AgentMemoryWrite is one entry to store.
type AgentMemoryWrite struct {
	Kind          agentmemory.Kind
	Content       string
	SubjectUserID int64
	Status        agentmemory.Status
}

// ErrAgentMemorySubjectOptedOut refuses an entry about a person who turned
// off "remember things about me" for the agent.
var ErrAgentMemorySubjectOptedOut = errors.New("the person opted out of this agent's memory")

// AgentMemorySink is the tenant side (repos.AgentMemory).
type AgentMemorySink interface {
	ResolveAgentMemoryRun(ctx context.Context, projectID int64, executionID string, generation uint64, target AgentMemoryTarget) (AgentMemoryRun, error)
	InsertAgentMemory(ctx context.Context, run AgentMemoryRun, write AgentMemoryWrite) (int64, error)
	ActiveAgentMemory(ctx context.Context, projectID, applicationID int64) ([]agentmemory.Candidate, error)
	// ForgetAgentMemory deletes one entry of the run's agent, unless it is
	// about a person not in present. It answers false for no such entry.
	ForgetAgentMemory(ctx context.Context, run AgentMemoryRun, entryID int64) (bool, error)
}

// RuntimeAgentMemoryService serves the three memory routes.
type RuntimeAgentMemoryService struct {
	authorizer AgentRuntimeContextAuthorizer
	sink       AgentMemorySink
}

// NewRuntimeAgentMemoryService builds the service.
func NewRuntimeAgentMemoryService(authorizer AgentRuntimeContextAuthorizer, sink AgentMemorySink) (*RuntimeAgentMemoryService, error) {
	if authorizer == nil || sink == nil {
		return nil, errors.New("runtime agent memory dependencies are required")
	}
	return &RuntimeAgentMemoryService{authorizer: authorizer, sink: sink}, nil
}

func (service *RuntimeAgentMemoryService) run(ctx context.Context, claim ContentClaim, target AgentMemoryTarget) (AgentMemoryRun, error) {
	if service == nil || service.authorizer == nil || service.sink == nil {
		return AgentMemoryRun{}, runtimeContextUnavailable(runtimeContextStageAgentMemoryRun)
	}
	builders := RuntimeEntityBuilderService{authorizer: service.authorizer}
	authorization, err := builders.authorizeBuilder(ctx, claim)
	if err != nil {
		return AgentMemoryRun{}, err
	}
	run, err := service.sink.ResolveAgentMemoryRun(ctx, authorization.ResourceProjectID, claim.ExecutionID, claim.Generation, target)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return AgentMemoryRun{}, contextErr
		}
		if errors.Is(err, ErrContentUnauthorized) || errors.Is(err, ErrContentRejected) {
			return AgentMemoryRun{}, err
		}
		return AgentMemoryRun{}, runtimeContextUnavailable(runtimeContextStageAgentMemoryRun)
	}
	// The run the sink read must be the one the claim authorized, and for a
	// nested target must resolve strictly to the requested application.
	if run.ProjectID != authorization.ResourceProjectID ||
		(authorization.ConversationID != "" && run.ConversationUUID != authorization.ConversationID) ||
		(target.ApplicationID > 0 && run.ApplicationID != target.ApplicationID) {
		return AgentMemoryRun{}, ErrContentUnauthorized
	}
	// No agent (an ad hoc chat) or memory off: there is no memory to reach.
	if run.ApplicationID <= 0 || !run.Settings.Enabled {
		return AgentMemoryRun{}, ErrContentRejected
	}
	return run, nil
}

// Remember stores one entry for the claimed run's agent (or a proven nested agent).
func (service *RuntimeAgentMemoryService) Remember(ctx context.Context, claim ContentClaim, request RuntimeAgentMemoryRememberRequest) (RuntimeAgentMemoryRemembered, error) {
	target, err := parseAgentMemoryTarget(request.TargetApplicationID, request.TargetVersionID)
	if err != nil {
		return RuntimeAgentMemoryRemembered{}, err
	}
	kind := agentmemory.Kind(request.Kind)
	if !kind.AgentWritable() {
		return RuntimeAgentMemoryRemembered{}, ErrContentRejected
	}
	content, err := agentmemory.CheckContent(request.Content)
	if err != nil {
		return RuntimeAgentMemoryRemembered{}, ErrContentRejected
	}
	run, err := service.run(ctx, claim, target)
	if err != nil {
		return RuntimeAgentMemoryRemembered{}, err
	}
	status, err := agentmemory.DecideStatus(run.Settings, agentmemory.WriteContext{
		Attended: run.Attended, FromToolOutput: request.FromToolOutput, PersonalProject: run.PersonalProject,
	})
	if err != nil {
		return RuntimeAgentMemoryRemembered{}, ErrContentRejected
	}
	var subject int64
	if request.AboutUser {
		if _, present := run.Present[run.ActorID]; run.ActorID <= 0 || !present {
			return RuntimeAgentMemoryRemembered{}, ErrContentRejected
		}
		subject = run.ActorID
	}
	// The taint rule: what a personal read brought into this conversation
	// may only go into an entry about that person.
	if run.Tainted && subject == 0 {
		return RuntimeAgentMemoryRemembered{}, ErrContentRejected
	}
	id, err := service.sink.InsertAgentMemory(ctx, run, AgentMemoryWrite{
		Kind: kind, Content: content, SubjectUserID: subject, Status: status,
	})
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return RuntimeAgentMemoryRemembered{}, contextErr
		}
		if errors.Is(err, ErrAgentMemorySubjectOptedOut) || errors.Is(err, ErrContentRejected) {
			return RuntimeAgentMemoryRemembered{}, ErrContentRejected
		}
		return RuntimeAgentMemoryRemembered{}, runtimeContextUnavailable(runtimeContextStageAgentMemoryWrite)
	}
	return RuntimeAgentMemoryRemembered{
		SchemaVersion: RuntimeAgentMemoryRememberSchemaVersion,
		ProjectID:     run.ProjectID,
		EntryID:       strconv.FormatInt(id, 10),
		Status:        string(status),
	}, nil
}

// Recall answers the run's agent's active entries that match the query,
// under the same presence rule and budgets as the recall at turn start.
func (service *RuntimeAgentMemoryService) Recall(ctx context.Context, claim ContentClaim, request RuntimeAgentMemoryRecallRequest) (RuntimeAgentMemoryRecalled, error) {
	target, err := parseAgentMemoryTarget(request.TargetApplicationID, request.TargetVersionID)
	if err != nil {
		return RuntimeAgentMemoryRecalled{}, err
	}
	if len(request.Query) > maxRuntimeAgentMemoryQueryBytes || request.Limit < 0 || request.Limit > agentmemory.MaxRecallEntries {
		return RuntimeAgentMemoryRecalled{}, ErrContentRejected
	}
	run, err := service.run(ctx, claim, target)
	if err != nil {
		return RuntimeAgentMemoryRecalled{}, err
	}
	candidates, err := service.sink.ActiveAgentMemory(ctx, run.ProjectID, run.ApplicationID)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return RuntimeAgentMemoryRecalled{}, contextErr
		}
		return RuntimeAgentMemoryRecalled{}, runtimeContextUnavailable(runtimeContextStageAgentMemoryRecall)
	}
	settings := run.Settings
	if request.Limit > 0 {
		settings.MaxEntries = request.Limit
	}
	present := make(map[int64]bool, len(run.Present))
	for id := range run.Present {
		present[id] = true
	}
	selected := agentmemory.Select(candidates, present, strings.TrimSpace(request.Query), settings)
	entries := make([]RuntimeAgentMemoryEntry, 0, len(selected))
	for _, entry := range selected {
		about := ""
		if entry.SubjectUserID > 0 {
			about = run.Present[entry.SubjectUserID]
			if about == "" {
				about = "a person in this conversation"
			}
		}
		entries = append(entries, RuntimeAgentMemoryEntry{
			EntryID: strconv.FormatInt(entry.ID, 10), Kind: string(entry.Kind),
			Content: entry.Content, AboutUser: about, Pinned: entry.Pinned,
		})
	}
	return RuntimeAgentMemoryRecalled{
		SchemaVersion: RuntimeAgentMemoryRecallSchemaVersion,
		ProjectID:     run.ProjectID,
		Entries:       entries,
	}, nil
}

// Forget deletes one entry of the run's agent. Off mode never writes, and
// forgetting is a write.
func (service *RuntimeAgentMemoryService) Forget(ctx context.Context, claim ContentClaim, request RuntimeAgentMemoryForgetRequest) (RuntimeAgentMemoryForgotten, error) {
	target, err := parseAgentMemoryTarget(request.TargetApplicationID, request.TargetVersionID)
	if err != nil {
		return RuntimeAgentMemoryForgotten{}, err
	}
	entryID, err := strconv.ParseInt(request.EntryID, 10, 64)
	if err != nil || entryID <= 0 {
		return RuntimeAgentMemoryForgotten{}, ErrContentRejected
	}
	run, err := service.run(ctx, claim, target)
	if err != nil {
		return RuntimeAgentMemoryForgotten{}, err
	}
	if !run.Settings.Writes() {
		return RuntimeAgentMemoryForgotten{}, ErrContentRejected
	}
	forgotten, err := service.sink.ForgetAgentMemory(ctx, run, entryID)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return RuntimeAgentMemoryForgotten{}, contextErr
		}
		return RuntimeAgentMemoryForgotten{}, runtimeContextUnavailable(runtimeContextStageAgentMemoryWrite)
	}
	return RuntimeAgentMemoryForgotten{
		SchemaVersion: RuntimeAgentMemoryForgetSchemaVersion,
		ProjectID:     run.ProjectID,
		Forgotten:     forgotten,
	}, nil
}
