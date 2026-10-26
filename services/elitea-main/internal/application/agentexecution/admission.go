package agentexecution

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

const agentRequestDigestDomain = "elitea.agent.execution.admission.v1\x00"

var ErrInvalidAgentAdmission = errors.New("invalid agent execution admission")

type SubmitRequest struct {
	Identity              executionapp.AdmissionIdentity
	IdempotencyKey        string
	CapabilityID          string
	ClientStreamID        string
	ClientMessageID       string
	SIOEvent              string
	Input                 *runtimev1.AgentExecutionInputV1
	CurrentTurn           *CurrentApplicationTurn
	CurrentAdhocTurn      *CurrentAdhocTurn
	CurrentRegenerateTurn *CurrentRegenerateTurn
	CurrentContinueTurn   *CurrentContinueTurn
	// CurrentFanoutWakeTurn is a Main-admitted wake of a parked fan-out
	// response (continue_fanout_wake.go); it excludes every other turn.
	CurrentFanoutWakeTurn *CurrentFanoutWakeTurn
	// TriggerOrigin is how the run started (shared 0140). Empty means a
	// person at the UI. It is NOT part of the request digest: it describes
	// the caller, not the work, and adding it would make every replay that
	// crosses a deployment an idempotency conflict.
	TriggerOrigin executiondomain.TriggerOrigin
}

type Admission struct {
	// MemoryToolkitIDs are the memory toolkits the run was given, computed
	// once here from the server-frozen toolkit set (memoryToolkitIDs) and
	// stored on the execution; the vector claim token copies them.
	MemoryToolkitIDs      []int64
	Record                executiondomain.Admission
	Binding               executiondomain.AgentExecutionBinding
	CurrentTurn           *CurrentApplicationTurn
	CurrentAdhocTurn      *CurrentAdhocTurn
	CurrentRegenerateTurn *CurrentRegenerateTurn
	CurrentContinueTurn   *CurrentContinueTurn
	CurrentFanoutWakeTurn *CurrentFanoutWakeTurn
}

type AtomicAdmissionStore interface {
	AdmitAgentExecution(context.Context, Admission) (executionapp.AdmissionOutcome, error)
}

type AdmissionService struct {
	store   AtomicAdmissionStore
	factory *InputBundleFactory
	now     func() time.Time
	newID   executionapp.IDGenerator
	// nested reads sub-agents' frozen toolkits (WithNestedMemoryToolkits).
	nested NestedApplicationTools
}

func NewAdmissionService(
	store AtomicAdmissionStore,
	factory *InputBundleFactory,
	now func() time.Time,
	newID executionapp.IDGenerator,
) (*AdmissionService, error) {
	if store == nil || factory == nil || newID == nil {
		return nil, errors.New("agent admission dependencies are required")
	}
	if now == nil {
		now = time.Now
	}
	return &AdmissionService{store: store, factory: factory, now: now, newID: newID}, nil
}

func (s *AdmissionService) Submit(
	ctx context.Context,
	request SubmitRequest,
) (executionapp.AdmissionOutcome, error) {
	if err := ctx.Err(); err != nil {
		return executionapp.AdmissionOutcome{}, err
	}
	if !validAgentIdentity(request.Identity) ||
		request.IdempotencyKey == "" || len(request.IdempotencyKey) > 200 ||
		!agentCapability(request.CapabilityID) || !request.TriggerOrigin.Valid() {
		return executionapp.AdmissionOutcome{}, ErrInvalidAgentAdmission
	}
	bundle, binding, err := s.factory.Build(
		ctx, request.Input, request.ClientStreamID, request.ClientMessageID,
		request.SIOEvent,
	)
	if err != nil {
		return executionapp.AdmissionOutcome{}, fmt.Errorf("%w: %v", ErrInvalidAgentAdmission, err)
	}
	requestDigest := agentRequestDigest(request, bundle.Entries[0].Content)

	executionID, commandID, outboxID, err := s.allocateAdmissionIDs()
	if err != nil {
		return executionapp.AdmissionOutcome{}, err
	}
	createdAt := s.now().UTC()
	record := executiondomain.Admission{
		IdempotencyScope: request.Identity.TenantID + "/" + request.Identity.ResourceProjectID + "/" + request.Identity.ActorID,
		IdempotencyKey:   request.IdempotencyKey,
		RequestDigest:    requestDigest,
		InputBundle:      bundle.Clone(),
		Job: executiondomain.Job{
			ID:                  executionID,
			CommandID:           commandID,
			TenantID:            request.Identity.TenantID,
			ResourceProjectID:   request.Identity.ResourceProjectID,
			ProjectionProjectID: request.Identity.ProjectionProjectID,
			ActorID:             request.Identity.ActorID,
			CapabilityID:        request.CapabilityID,
			Generation:          1,
			State:               executiondomain.JobPending,
			CreatedAt:           createdAt,
			TriggerOrigin:       request.TriggerOrigin,
		},
		Outbox: executiondomain.OutboxRecord{
			ID:          outboxID,
			CommandID:   commandID,
			ExecutionID: executionID,
			Generation:  1,
			CreatedAt:   createdAt,
		},
	}
	if err := record.Validate(); err != nil {
		return executionapp.AdmissionOutcome{}, fmt.Errorf("%w: %v", ErrInvalidAgentAdmission, err)
	}
	if err := binding.Validate(record.InputBundle); err != nil {
		return executionapp.AdmissionOutcome{}, fmt.Errorf("%w: %v", ErrInvalidAgentAdmission, err)
	}
	if request.CurrentTurn != nil {
		if request.CapabilityID != executiondomain.AgentApplicationCapability ||
			request.CurrentRegenerateTurn != nil ||
			request.CurrentContinueTurn != nil ||
			request.CurrentTurn.Validate() != nil ||
			request.CurrentTurn.ResponseMessageID != request.ClientMessageID ||
			request.CurrentTurn.ConversationUUID != request.ClientStreamID ||
			request.CurrentTurn.QuestionID != binding.ClientExecutionGeneration {
			return executionapp.AdmissionOutcome{}, ErrInvalidAgentAdmission
		}
	}
	if request.CurrentAdhocTurn != nil {
		if request.CurrentTurn != nil || request.CurrentRegenerateTurn != nil || request.CurrentContinueTurn != nil ||
			request.CapabilityID != executiondomain.AgentAdhocCapability ||
			request.CurrentAdhocTurn.Validate() != nil ||
			request.CurrentAdhocTurn.ResponseMessageID != request.ClientMessageID ||
			request.CurrentAdhocTurn.ConversationUUID != request.ClientStreamID ||
			request.CurrentAdhocTurn.QuestionID != binding.ClientExecutionGeneration {
			return executionapp.AdmissionOutcome{}, ErrInvalidAgentAdmission
		}
	}
	if request.CurrentRegenerateTurn != nil {
		turn := request.CurrentRegenerateTurn
		if request.CurrentTurn != nil || request.CurrentAdhocTurn != nil || request.CurrentContinueTurn != nil ||
			turn.Validate() != nil || turn.ResponseMessageID != request.ClientMessageID ||
			turn.ConversationUUID != request.ClientStreamID ||
			turn.ExecutionGeneration != binding.ClientExecutionGeneration ||
			(turn.Kind == CurrentRegenerationApplication && request.CapabilityID != executiondomain.AgentApplicationCapability) ||
			(turn.Kind == CurrentRegenerationAdhoc && request.CapabilityID != executiondomain.AgentAdhocCapability) {
			return executionapp.AdmissionOutcome{}, ErrInvalidAgentAdmission
		}
	}
	if request.CurrentContinueTurn != nil {
		turn := request.CurrentContinueTurn
		if request.CurrentTurn != nil || request.CurrentAdhocTurn != nil || request.CurrentRegenerateTurn != nil ||
			turn.Validate() != nil || turn.ProjectionResponseID() != request.ClientMessageID ||
			turn.ConversationUUID != request.ClientStreamID ||
			turn.ExecutionGeneration != binding.ClientExecutionGeneration ||
			request.SIOEvent != "chat_continue_predict" ||
			(turn.Kind == CurrentRegenerationApplication && request.CapabilityID != executiondomain.AgentApplicationCapability) ||
			(turn.Kind == CurrentRegenerationAdhoc && request.CapabilityID != executiondomain.AgentAdhocCapability) {
			return executionapp.AdmissionOutcome{}, ErrInvalidAgentAdmission
		}
	}
	if request.CurrentFanoutWakeTurn != nil {
		turn := request.CurrentFanoutWakeTurn
		if request.CurrentTurn != nil || request.CurrentAdhocTurn != nil || request.CurrentRegenerateTurn != nil ||
			request.CurrentContinueTurn != nil || turn.Validate() != nil ||
			request.IdempotencyKey != FanoutWakeIdempotencyKey(turn.ResponseMessageID, turn.DecisionRevision) ||
			turn.ResponseMessageID != request.ClientMessageID || turn.ConversationUUID != request.ClientStreamID ||
			turn.ExecutionGeneration != binding.ClientExecutionGeneration || request.SIOEvent != "chat_continue_predict" ||
			strconv.FormatInt(turn.ProjectID, 10) != request.Identity.ResourceProjectID ||
			strconv.FormatInt(turn.ActorUserID, 10) != request.Identity.ActorID {
			return executionapp.AdmissionOutcome{}, ErrInvalidAgentAdmission
		}
	}

	memoryToolkits, err := s.runMemoryToolkits(ctx, request)
	if err != nil {
		return executionapp.AdmissionOutcome{}, err
	}
	outcome, err := s.store.AdmitAgentExecution(ctx, Admission{
		MemoryToolkitIDs:      memoryToolkits,
		Record:                record,
		Binding:               binding,
		CurrentTurn:           request.CurrentTurn.Clone(),
		CurrentAdhocTurn:      request.CurrentAdhocTurn.Clone(),
		CurrentRegenerateTurn: request.CurrentRegenerateTurn.Clone(),
		CurrentContinueTurn:   request.CurrentContinueTurn.Clone(),
		CurrentFanoutWakeTurn: request.CurrentFanoutWakeTurn.Clone(),
	})
	if err != nil {
		return executionapp.AdmissionOutcome{}, fmt.Errorf("admit agent execution: %w", err)
	}
	if outcome.ExecutionID == "" || outcome.CommandID == "" ||
		outcome.AdmittedAt.IsZero() || !outcome.Deadline.After(outcome.AdmittedAt) {
		return executionapp.AdmissionOutcome{}, errors.New("agent admission store returned invalid durable outcome")
	}
	return outcome, nil
}

func (s *AdmissionService) allocateAdmissionIDs() (string, string, string, error) {
	values := make([]string, 3)
	for index := range values {
		value, err := s.newID()
		if err != nil {
			return "", "", "", fmt.Errorf("generate agent admission ID: %w", err)
		}
		if value == "" {
			return "", "", "", errors.New("agent admission ID generator returned an empty ID")
		}
		values[index] = value
	}
	return values[0], values[1], values[2], nil
}

func validAgentIdentity(identity executionapp.AdmissionIdentity) bool {
	return identity.TenantID != "" && identity.ResourceProjectID != "" &&
		identity.ProjectionProjectID != "" && identity.ActorID != ""
}

func agentCapability(capabilityID string) bool {
	return capabilityID == executiondomain.AgentApplicationCapability ||
		capabilityID == executiondomain.AgentAdhocCapability
}

func agentRequestDigest(request SubmitRequest, content []byte) runtimedomain.Digest {
	values := [][]byte{
		[]byte(request.Identity.TenantID),
		[]byte(request.Identity.ResourceProjectID),
		[]byte(request.Identity.ProjectionProjectID),
		[]byte(request.Identity.ActorID),
		[]byte(request.CapabilityID),
		[]byte(request.ClientStreamID),
		[]byte(request.ClientMessageID),
		[]byte(request.SIOEvent),
		content,
	}
	material := make([]byte, 0, len(content)+512)
	material = append(material, agentRequestDigestDomain...)
	for _, value := range values {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		material = append(material, length[:]...)
		material = append(material, value...)
	}
	return runtimedomain.SHA256(material)
}

// NestedApplicationTools reads the frozen `tools` of one saved application
// version from the tenant schema of projectID: the same server-side
// projection the nested-agent runtime route freezes a sub-agent from
// (storage.CurrentApplicationVersionSource), never anything the caller sent.
type NestedApplicationTools interface {
	NestedApplicationTools(ctx context.Context, projectID, applicationID, versionID int64) (json.RawMessage, error)
}

// WithNestedMemoryToolkits makes admission include the memory toolkits of the
// run's sub-agents (nested `application` entries, recursively) in the run's
// memory toolkits, read through tools. Without it only the run's own
// toolkits count.
func (s *AdmissionService) WithNestedMemoryToolkits(tools NestedApplicationTools) *AdmissionService {
	if s != nil && tools != nil {
		s.nested = tools
	}
	return s
}

// The bounds of the sub-agent walk: the platform's nesting hop limit and a
// cap on distinct child versions read for one admission.
const (
	maxNestedMemoryDepth    = 25
	maxNestedMemoryVersions = 100
)

// runMemoryToolkits is the run's memory toolkits: its own (memoryToolkitIDs)
// and, with WithNestedMemoryToolkits, those of every sub-agent its frozen
// toolkit set names, read from the run's project. A read failure fails the
// admission: the execution is never stored with a guessed set.
func (s *AdmissionService) runMemoryToolkits(ctx context.Context, request SubmitRequest) ([]int64, error) {
	ids, children, err := frozenToolkitSet(request.CapabilityID, request.Input)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidAgentAdmission, err)
	}
	if s.nested == nil || len(children) == 0 {
		return ids, nil
	}
	project, err := strconv.ParseInt(request.Identity.ResourceProjectID, 10, 64)
	if err != nil || project <= 0 {
		return nil, ErrInvalidAgentAdmission
	}
	seen := map[[2]int64]bool{}
	frontier := children
	for depth := 0; len(frontier) > 0; depth++ {
		if depth >= maxNestedMemoryDepth {
			return nil, fmt.Errorf("%w: sub-agents nest deeper than %d", ErrInvalidAgentAdmission, maxNestedMemoryDepth)
		}
		var next [][2]int64
		for _, child := range frontier {
			if seen[child] {
				continue
			}
			seen[child] = true
			if len(seen) > maxNestedMemoryVersions {
				return nil, fmt.Errorf("%w: more than %d sub-agent versions", ErrInvalidAgentAdmission, maxNestedMemoryVersions)
			}
			tools, err := s.nested.NestedApplicationTools(ctx, project, child[0], child[1])
			if err != nil {
				return nil, fmt.Errorf("read a sub-agent's toolkits for the run's memory toolkits: %w", err)
			}
			childIDs, grandchildren, err := toolkitEntries(tools)
			if err != nil {
				return nil, fmt.Errorf("%w: a sub-agent's frozen toolkit set: %v", ErrInvalidAgentAdmission, err)
			}
			ids = append(ids, childIDs...)
			next = append(next, grandchildren...)
		}
		frontier = next
	}
	slices.Sort(ids)
	return slices.Compact(ids), nil
}

// memoryToolkitIDs lists the memory toolkits of an agent run's OWN frozen
// toolkit set (frozenToolkitSet without the sub-agents).
func memoryToolkitIDs(capabilityID string, input *runtimev1.AgentExecutionInputV1) ([]int64, error) {
	ids, _, err := frozenToolkitSet(capabilityID, input)
	return ids, err
}

// frozenToolkitSet reads the toolkit set Main froze for an agent run: the
// top-level entries of the frozen version's `tools` (an application run,
// `application.version_details.tools`) or of the input's `tools` (an ad-hoc
// run). These are exactly the references the worker materializes toolkits
// from, and Main built them from the project's own elitea_tools rows
// (FreezeCurrentApplicationVersion). It returns the memory toolkit ids (an
// entry of type "memory" with a positive integer `id`) and the sub-agents
// (entries of type "application" naming an application and version id).
// Nothing nested inside an entry is read, so text the caller supplied (user
// input, chat history, a tool's settings or arguments) can never add one. A
// frozen set that cannot be decoded fails the admission.
func frozenToolkitSet(capabilityID string, input *runtimev1.AgentExecutionInputV1) ([]int64, [][2]int64, error) {
	if input == nil {
		return []int64{}, nil, nil
	}
	var raw []byte
	switch capabilityID {
	case executiondomain.AgentApplicationCapability:
		var application struct {
			VersionDetails *struct {
				Tools json.RawMessage `json:"tools"`
			} `json:"version_details"`
		}
		if len(input.GetApplication()) != 0 {
			if err := json.Unmarshal(input.GetApplication(), &application); err != nil {
				return nil, nil, fmt.Errorf("the frozen application is not readable: %w", err)
			}
		}
		if application.VersionDetails != nil {
			raw = application.VersionDetails.Tools
		}
	case executiondomain.AgentAdhocCapability:
		raw = input.GetTools()
	}
	return toolkitEntries(raw)
}

// toolkitEntries reads one frozen `tools` list: the memory toolkit ids and
// the sub-agent (application id, version id) pairs of its top-level entries.
func toolkitEntries(raw json.RawMessage) ([]int64, [][2]int64, error) {
	ids := []int64{}
	if len(raw) == 0 || string(raw) == "null" {
		return ids, nil, nil
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, nil, fmt.Errorf("the frozen toolkit set is not a list: %w", err)
	}
	var children [][2]int64
	for _, entry := range tools {
		var tool struct {
			Type     string          `json:"type"`
			ID       json.RawMessage `json:"id"`
			Settings struct {
				ApplicationID json.RawMessage `json:"application_id"`
				VersionID     json.RawMessage `json:"application_version_id"`
			} `json:"settings"`
		}
		if json.Unmarshal(entry, &tool) != nil {
			continue
		}
		switch tool.Type {
		case "memory":
			if id, ok := positiveJSONInt(tool.ID); ok && !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		case "application":
			application, okApplication := positiveJSONInt(tool.Settings.ApplicationID)
			version, okVersion := positiveJSONInt(tool.Settings.VersionID)
			if okApplication && okVersion {
				children = append(children, [2]int64{application, version})
			}
		}
	}
	slices.Sort(ids)
	return ids, children, nil
}

// positiveJSONInt is a JSON integer above zero.
func positiveJSONInt(raw json.RawMessage) (int64, bool) {
	id, err := strconv.ParseInt(string(raw), 10, 64)
	return id, err == nil && id > 0
}
