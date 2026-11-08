package agentexecution

// Idle compaction per (conversation, agent) in the cloud (ADR-0050 decision
// 9b2, issue 1610). When an agent's turn settles with an answer on a model
// with a paid prompt cache, Main arms one check (shared 0230,
// agent_idle_compactions). When it is due and still worth running, the
// admitter below admits a quiet `idle_compaction` continuation of the settled
// execution: the source's exact frozen input, an empty user input and the
// meta.idle_compaction_v1 marker. The worker summarises the session while its
// prompt cache is warm; Main projects nothing of it into the transcript and
// keeps only the measurement it produced (result_context).

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/idlecompaction"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/noderecovery"
	"google.golang.org/protobuf/proto"
)

// IdleCompactionSchema names the payload.meta.idle_compaction_v1 marker.
const IdleCompactionSchema = "elitea.agent.idle-compaction.v1"

// Skip codes the store and the admitter write besides the domain's
// (idlecompaction.Skip*). All match the row's skip_code shape.
const (
	IdleCompactionSkipAdmission       = "admission_refused"
	IdleCompactionSkipExhausted       = "admission_exhausted"
	IdleCompactionSkipSourceInvalid   = "source_input_invalid"
	IdleCompactionSkipActorNotAllowed = "actor_not_allowed"
	IdleCompactionSkipHandedOff       = "thread_handed_off"
	IdleCompactionSkipRunFailed       = "idle_run_failed"
	IdleCompactionSkipRunCancelled    = "idle_run_cancelled"
)

var (
	errInvalidIdleCompaction = errors.New("idle compaction is invalid")
	// ErrIdleCompactionNotQueued: the row is no longer QUEUED (admitted by
	// another admitter, or cancelled by the agent's next turn). Nothing is
	// admitted and nothing is left to do.
	ErrIdleCompactionNotQueued = errors.New("idle compaction is no longer queued")
	// ErrIdleCompactionSuperseded: the admission transaction found a newer
	// execution of the participant than the source. The row ends SKIPPED.
	ErrIdleCompactionSuperseded = errors.New("idle compaction source is no longer the agent's latest turn")
	// ErrIdleCompactionAgentBusy: a turn of the participant runs or waits.
	ErrIdleCompactionAgentBusy = errors.New("idle compaction agent has a running turn")
	// ErrIdleCompactionActorNotAllowed: the source's actor may no longer
	// write to the conversation.
	ErrIdleCompactionActorNotAllowed = errors.New("idle compaction actor may no longer continue the conversation")
)

// IdleCompactionIdempotencyKey is idle-compaction/{source execution id}, the
// row's derived key and the admission idempotency key of its job.
func IdleCompactionIdempotencyKey(sourceExecutionID string) string {
	return "idle-compaction/" + sourceExecutionID
}

// IdleCompactionMeta writes the idle input's meta member
// {"cache_ttl_seconds","request_started_unix_ms","schema",
// "source_execution_id","source_generation"} in canonical form (keys sorted,
// compact). The Rust worker parses exactly this document.
func IdleCompactionMeta(sourceExecutionID string, sourceGeneration int64, requestStarted time.Time, cacheTTL time.Duration) (json.RawMessage, error) {
	if !validParkedExecutionID(sourceExecutionID) || sourceGeneration < 1 || requestStarted.IsZero() ||
		requestStarted.UnixMilli() <= 0 || cacheTTL < time.Minute || cacheTTL > 24*time.Hour {
		return nil, errInvalidIdleCompaction
	}
	// Only a lowercase hex id and integers: the text is already canonical.
	return json.RawMessage(`{"cache_ttl_seconds":` + strconv.FormatInt(int64(cacheTTL/time.Second), 10) +
		`,"request_started_unix_ms":` + strconv.FormatInt(requestStarted.UnixMilli(), 10) +
		`,"schema":"` + IdleCompactionSchema +
		`","source_execution_id":"` + sourceExecutionID +
		`","source_generation":` + strconv.FormatInt(sourceGeneration, 10) + `}`), nil
}

// CurrentIdleCompactionTurn is the server-owned side of one idle compaction
// admission. Every field comes from the durable row and the source
// execution's job and binding; nothing comes from a browser. The admission
// transaction rechecks the row, the participant's latest execution and the
// actor.
type CurrentIdleCompactionTurn struct {
	ProjectID        int64
	ActorUserID      int64
	ConversationUUID string
	// ResponseMessageID is the source's answer (the binding's client
	// message id): the idle job binds to it, writes nothing on it.
	ResponseMessageID   string
	ParticipantID       int64
	SourceExecutionID   string
	SourceGeneration    int64
	ExecutionGeneration string
	ThreadID            string
	// ApplicationID is the source's agent (0 for an ad hoc chat), so the
	// kill switch, the gateway throttle and spend see the agent.
	ApplicationID int64
}

func (turn CurrentIdleCompactionTurn) Validate() error {
	if turn.ProjectID <= 0 || turn.ActorUserID <= 0 || !validStoredUUID(turn.ConversationUUID) ||
		!noderecovery.ValidResponseMessageID(turn.ResponseMessageID) ||
		turn.ParticipantID <= 0 || turn.ParticipantID > math.MaxInt32 ||
		!validParkedExecutionID(turn.SourceExecutionID) || turn.SourceGeneration < 1 ||
		turn.ExecutionGeneration == "" || len(turn.ExecutionGeneration) > 256 ||
		strings.ContainsAny(turn.ExecutionGeneration, "\x00\r\n") ||
		turn.ThreadID == "" || len(turn.ThreadID) > 256 || strings.ContainsRune(turn.ThreadID, '\x00') ||
		turn.ApplicationID < 0 || turn.ApplicationID > math.MaxInt32 {
		return ErrInvalidCurrentAgentStart
	}
	return nil
}

func (turn *CurrentIdleCompactionTurn) Clone() *CurrentIdleCompactionTurn {
	if turn == nil {
		return nil
	}
	clone := *turn
	return &clone
}

// IdleCompactionTarget is one claimed, due row with what its decision and its
// admission need.
type IdleCompactionTarget struct {
	Turn                CurrentIdleCompactionTurn
	TenantID            string
	ResourceProjectID   string
	ProjectionProjectID string
	ActorID             string
	CapabilityID        string
	TriggerOrigin       executiondomain.TriggerOrigin
	FrozenInput         []byte
	FrozenInputDigest   []byte
	// RequestStarted is the source's last model request start; CacheTTL the
	// row's cache lifetime.
	RequestStarted time.Time
	CacheTTL       time.Duration
	// LatestExecution is the participant's newest execution in the
	// conversation (idle runs excluded); AgentBusy a non-terminal one exists.
	LatestExecution string
	AgentBusy       bool
	// Now is the database clock at the claim.
	Now time.Time
}

// Check is the domain decision input of the target.
func (target IdleCompactionTarget) Check() idlecompaction.Check {
	return idlecompaction.Check{
		RequestStarted:  target.RequestStarted,
		SourceExecution: target.Turn.SourceExecutionID,
		LatestExecution: target.LatestExecution,
		AgentBusy:       target.AgentBusy,
	}
}

// idleCompactionResumeMarkers are the continuation discriminators a source's
// frozen meta may carry; the idle input drops them all.
var idleCompactionResumeMarkers = []string{
	"pipeline_static_resume_v1", "pipeline_static_tool_resume_v1", "fanout_wake_v1", "idle_compaction_v1",
}

// IdleCompactionInput derives the idle input from the source's frozen
// request: every frozen field is the source's (so the provider prompt cache
// of its last request is read), user_input is the empty string, no resume,
// continue or regenerate, no HITL decision, no checkpoint and no delegated
// sign-in, and meta.idle_compaction_v1 names the source. The frozen bytes
// must match their stored digest, so the input is deterministic and a
// retried admission collapses on its idempotency key.
func IdleCompactionInput(target IdleCompactionTarget) (*runtimev1.AgentExecutionInputV1, error) {
	turn := target.Turn
	if turn.Validate() != nil || len(target.FrozenInput) == 0 ||
		len(target.FrozenInput) > executiondomain.MaxAgentExecutionInputBytes {
		return nil, errInvalidIdleCompaction
	}
	digest := sha256.Sum256(target.FrozenInput)
	if !bytes.Equal(digest[:], target.FrozenInputDigest) {
		return nil, errInvalidIdleCompaction
	}
	var input runtimev1.AgentExecutionInputV1
	if proto.Unmarshal(target.FrozenInput, &input) != nil || input.GetSchemaRevision() != agentInputSchemaRevision ||
		input.GetExecutionGeneration() != turn.ExecutionGeneration ||
		(input.ConversationId != nil && input.GetConversationId() != turn.ConversationUUID) ||
		(input.ThreadId != nil && input.GetThreadId() != turn.ThreadID) {
		return nil, errInvalidIdleCompaction
	}
	var meta map[string]json.RawMessage
	if json.Unmarshal(input.GetMeta(), &meta) != nil || meta == nil {
		return nil, errInvalidIdleCompaction
	}
	for _, marker := range idleCompactionResumeMarkers {
		delete(meta, marker)
	}
	ttl := target.CacheTTL
	if ttl == 0 {
		ttl = idlecompaction.CacheTTL
	}
	marker, err := IdleCompactionMeta(turn.SourceExecutionID, turn.SourceGeneration, target.RequestStarted, ttl)
	if err != nil {
		return nil, err
	}
	meta["idle_compaction_v1"] = marker
	encodedMeta, err := json.Marshal(meta)
	if err != nil {
		return nil, errInvalidIdleCompaction
	}
	input.Meta = encodedMeta
	input.ThreadId = stringPointer(turn.ThreadID)
	input.UserInput = []byte(`""`)
	input.ShouldContinue = false
	input.HitlResume = false
	input.HitlAction = nil
	input.HitlValue = nil
	input.HitlDecisions = []byte(`[]`)
	input.CheckpointId = nil
	input.TruncatedContent = nil
	input.IsRegenerate = false
	input.McpTokenReferences = nil
	return &input, nil
}

// IdleCompactionPromptCache reports a source whose frozen model is native
// Anthropic, the one family the cloud breakpoints cache for (the rule of
// tools.go's max_tokens freeze): not OpenAI-compatible, and a model name that
// names anthropic or claude. The model comes from llm.kwargs.model (an ad
// hoc chat) or application.version_details.llm_settings.model_name (an
// agent); openai_compatible from llm.kwargs, else llm_settings.
func IdleCompactionPromptCache(input *runtimev1.AgentExecutionInputV1) bool {
	if input == nil {
		return false
	}
	var llm struct {
		Kwargs struct {
			Model            string `json:"model"`
			OpenAICompatible *bool  `json:"openai_compatible"`
		} `json:"kwargs"`
	}
	_ = json.Unmarshal(input.GetLlm(), &llm)
	var application struct {
		VersionDetails struct {
			LLMSettings struct {
				ModelName        string `json:"model_name"`
				OpenAICompatible *bool  `json:"openai_compatible"`
			} `json:"llm_settings"`
		} `json:"version_details"`
	}
	_ = json.Unmarshal(input.GetApplication(), &application)
	model := llm.Kwargs.Model
	if model == "" {
		model = application.VersionDetails.LLMSettings.ModelName
	}
	compatible := llm.Kwargs.OpenAICompatible
	if compatible == nil {
		compatible = application.VersionDetails.LLMSettings.OpenAICompatible
	}
	if compatible == nil || *compatible {
		return false
	}
	lower := strings.ToLower(model)
	return strings.Contains(lower, "anthropic") || strings.Contains(lower, "claude")
}
