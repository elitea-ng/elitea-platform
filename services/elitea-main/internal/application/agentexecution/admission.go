package agentexecution

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
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
}

type Admission struct {
	Record                executiondomain.Admission
	Binding               executiondomain.AgentExecutionBinding
	CurrentTurn           *CurrentApplicationTurn
	CurrentAdhocTurn      *CurrentAdhocTurn
	CurrentRegenerateTurn *CurrentRegenerateTurn
	CurrentContinueTurn   *CurrentContinueTurn
}

type AtomicAdmissionStore interface {
	AdmitAgentExecution(context.Context, Admission) (executionapp.AdmissionOutcome, error)
}

type AdmissionService struct {
	store   AtomicAdmissionStore
	factory *InputBundleFactory
	now     func() time.Time
	newID   executionapp.IDGenerator
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
		!agentCapability(request.CapabilityID) {
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

	outcome, err := s.store.AdmitAgentExecution(ctx, Admission{
		Record:                record,
		Binding:               binding,
		CurrentTurn:           request.CurrentTurn.Clone(),
		CurrentAdhocTurn:      request.CurrentAdhocTurn.Clone(),
		CurrentRegenerateTurn: request.CurrentRegenerateTurn.Clone(),
		CurrentContinueTurn:   request.CurrentContinueTurn.Clone(),
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
