package agentexecution

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/noderecovery"
	"google.golang.org/protobuf/proto"
)

// CurrentContinuationFanoutWake is the continuation Main admits by itself
// when a decision lands on a parked fan-out response
// (libs/proto/contracts/fanout-interrupt-decisions-v1.md §8, §8.1). A browser
// never requests it: CurrentContinuationResolveRequest.Validate refuses the
// kind, and the wake carries no decision and no interrupt kind.
const CurrentContinuationFanoutWake CurrentContinuationKind = "fanout_wake"

// FanoutWakeSchema names the payload.meta.fanout_wake_v1 marker.
const FanoutWakeSchema = "elitea.pipeline.fanout-wake.v1"

var errInvalidFanoutWake = errors.New("fanout wake marker is invalid")

// FanoutWakeIdempotencyKey is continue-interrupt/{responseMessageID}/{decision_revision}.
// It is the admission idempotency key of the wake job and the derived key of
// its durable wake row, so a retried admission collapses onto one job.
func FanoutWakeIdempotencyKey(responseMessageID string, decisionRevision int64) string {
	return "continue-interrupt/" + responseMessageID + "/" + strconv.FormatInt(decisionRevision, 10)
}

// FanoutWakeMeta writes the wake input's meta member
// {"fanout_wake_v1": {decision_revision, parked_execution_id,
// parked_generation, response_message_id, schema}} in the contract canonical
// form (keys sorted, compact). Every other meta member of a wake input is the
// parked execution's frozen meta, without any resume marker; the Worker
// re-enters the parked node and fetches. The parked execution names the only
// writer whose root checkpoint a reclaimed wake may re-enter.
func FanoutWakeMeta(responseMessageID string, decisionRevision int64, parkedExecutionID string, parkedGeneration int64) (json.RawMessage, error) {
	if !noderecovery.ValidResponseMessageID(responseMessageID) || decisionRevision < 1 ||
		!validParkedExecutionID(parkedExecutionID) || parkedGeneration < 1 {
		return nil, errInvalidFanoutWake
	}
	// Only ASCII ids and integers: encoding/json output is already canonical.
	return json.RawMessage(`{"decision_revision":` + strconv.FormatInt(decisionRevision, 10) +
		`,"parked_execution_id":"` + parkedExecutionID +
		`","parked_generation":` + strconv.FormatInt(parkedGeneration, 10) +
		`,"response_message_id":"` + responseMessageID + `","schema":"` + FanoutWakeSchema + `"}`), nil
}

// validParkedExecutionID is the schema's 32 lowercase hex execution id.
func validParkedExecutionID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

var (
	// ErrFanoutWakeNotQueued: the wake row is no longer QUEUED (admitted by
	// another admitter, or closed by stop, regenerate or a new turn). Nothing
	// is admitted and nothing is left to do.
	ErrFanoutWakeNotQueued = errors.New("fanout wake is no longer queued")
	// ErrFanoutWakeActorNotAllowed: the actor of the parked execution lost
	// the right to continue the response. The wake ends FAILED
	// (actor_not_allowed), no job is created.
	ErrFanoutWakeActorNotAllowed = errors.New("fanout wake actor may no longer continue the response")
	// ErrFanoutWakeTargetGone: the response is no longer the parked one the
	// wake names. The wake ends FAILED (response_not_parked).
	ErrFanoutWakeTargetGone = errors.New("fanout wake response is no longer parked")
)

// Wake failure codes stored on execution_interrupt_wakes.failure_code.
const (
	FanoutWakeFailureActorNotAllowed    = "actor_not_allowed"
	FanoutWakeFailureResponseNotParked  = "response_not_parked"
	FanoutWakeFailureParkedInput        = "parked_input_invalid"
	FanoutWakeFailureAdmission          = "admission_refused"
	FanoutWakeFailureGovernedCredential = "governed_credential_refused"
	FanoutWakeFailureExhausted          = "admission_exhausted"
)

// CurrentFanoutWakeTurn is the server-owned side of one wake admission. Every
// field comes from the durable wake row and the parked execution's job,
// binding and response; nothing comes from a browser. The admission
// transaction rechecks the wake row, the actor and the parked response.
type CurrentFanoutWakeTurn struct {
	ProjectID           int64
	ActorUserID         int64
	ConversationUUID    string
	ResponseMessageID   string
	ExecutionGeneration string
	ThreadID            string
	DecisionRevision    int64
	ParkedExecutionID   string
	ParkedGeneration    int64
}

func (turn CurrentFanoutWakeTurn) Validate() error {
	if turn.ProjectID <= 0 || turn.ActorUserID <= 0 || !validUUID(turn.ConversationUUID) ||
		!noderecovery.ValidResponseMessageID(turn.ResponseMessageID) || !validStoredUUID(turn.ExecutionGeneration) ||
		turn.ThreadID == "" || len(turn.ThreadID) > 256 || strings.ContainsRune(turn.ThreadID, '\x00') ||
		turn.DecisionRevision < 1 || !validFanoutWakeExecutionID(turn.ParkedExecutionID) || turn.ParkedGeneration < 1 {
		return ErrInvalidCurrentAgentStart
	}
	return nil
}

func (turn *CurrentFanoutWakeTurn) Clone() *CurrentFanoutWakeTurn {
	if turn == nil {
		return nil
	}
	clone := *turn
	return &clone
}

func validFanoutWakeExecutionID(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, ch := range value {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

// FanoutWakeTarget is one claimed wake with what its admission needs: the
// parked execution's identity and its exact frozen request entry.
type FanoutWakeTarget struct {
	Turn                CurrentFanoutWakeTurn
	TenantID            string
	ResourceProjectID   string
	ProjectionProjectID string
	ActorID             string
	CapabilityID        string
	TriggerOrigin       executiondomain.TriggerOrigin
	FrozenInput         []byte
	FrozenInputDigest   []byte
}

// fanoutWakeResumeMarkers are the continuation discriminators of other kinds
// a parked execution's frozen meta may still carry; a wake drops them.
var fanoutWakeResumeMarkers = []string{"pipeline_static_resume_v1", "pipeline_static_tool_resume_v1"}

// FanoutWakeInput derives the wake input from the parked execution's frozen
// request (contract §8.1): every frozen field is the parked execution's;
// hitl_resume and should_continue are false, user_input is the empty string,
// hitl_decisions is empty and meta.fanout_wake_v1 names the response and
// revision. The frozen bytes must match their stored digest, so the input is
// deterministic and a retried admission collapses on its idempotency key.
func FanoutWakeInput(target FanoutWakeTarget) (*runtimev1.AgentExecutionInputV1, error) {
	turn := target.Turn
	if turn.Validate() != nil || len(target.FrozenInput) == 0 ||
		len(target.FrozenInput) > executiondomain.MaxAgentExecutionInputBytes {
		return nil, errInvalidFanoutWake
	}
	digest := sha256.Sum256(target.FrozenInput)
	if !bytes.Equal(digest[:], target.FrozenInputDigest) {
		return nil, errInvalidFanoutWake
	}
	var input runtimev1.AgentExecutionInputV1
	if proto.Unmarshal(target.FrozenInput, &input) != nil || input.GetSchemaRevision() != agentInputSchemaRevision ||
		input.GetExecutionGeneration() != turn.ExecutionGeneration ||
		(input.ConversationId != nil && input.GetConversationId() != turn.ConversationUUID) ||
		(input.ThreadId != nil && input.GetThreadId() != turn.ThreadID) {
		return nil, errInvalidFanoutWake
	}
	var meta map[string]json.RawMessage
	if json.Unmarshal(input.GetMeta(), &meta) != nil || meta == nil {
		return nil, errInvalidFanoutWake
	}
	for _, marker := range fanoutWakeResumeMarkers {
		delete(meta, marker)
	}
	wake, err := FanoutWakeMeta(turn.ResponseMessageID, turn.DecisionRevision, turn.ParkedExecutionID, turn.ParkedGeneration)
	if err != nil {
		return nil, err
	}
	meta["fanout_wake_v1"] = wake
	encodedMeta, err := json.Marshal(meta)
	if err != nil {
		return nil, errInvalidFanoutWake
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
	return &input, nil
}
