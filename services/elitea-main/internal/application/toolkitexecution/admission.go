package toolkitexecution

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

const toolkitReadRequestDigestDomain = "elitea.toolkit.execute.read.admission.v1\x00"

var ErrInvalidToolkitExecuteReadAdmission = errors.New("invalid direct toolkit execution admission")

type SubmitRequest struct {
	Identity       executionapp.AdmissionIdentity
	IdempotencyKey string
	Frozen         FrozenCurrentReadTool
}

type Admission struct {
	Record  executiondomain.Admission
	Binding executiondomain.ToolkitExecuteReadBinding
}

type AtomicAdmissionStore interface {
	AdmitToolkitExecuteRead(context.Context, Admission) (executionapp.AdmissionOutcome, error)
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
		return nil, errors.New("direct toolkit admission dependencies are required")
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
	if !validToolkitReadAdmissionIdentity(request.Identity) ||
		request.IdempotencyKey == "" || len(request.IdempotencyKey) > 200 ||
		strings.ContainsAny(request.IdempotencyKey, "\x00\r\n") {
		return executionapp.AdmissionOutcome{}, ErrInvalidToolkitExecuteReadAdmission
	}
	bundle, binding, err := s.factory.Build(ctx, request.Frozen)
	if err != nil {
		return executionapp.AdmissionOutcome{}, fmt.Errorf("%w: %v", ErrInvalidToolkitExecuteReadAdmission, err)
	}

	executionID, commandID, outboxID, err := s.allocateIDs()
	if err != nil {
		return executionapp.AdmissionOutcome{}, err
	}
	createdAt := s.now().UTC()
	record := executiondomain.Admission{
		IdempotencyScope: request.Identity.TenantID + "/" + request.Identity.ResourceProjectID + "/" + request.Identity.ActorID,
		IdempotencyKey:   request.IdempotencyKey,
		RequestDigest:    toolkitReadRequestDigest(request.Identity, bundle.Entries[0].Content),
		InputBundle:      bundle.Clone(),
		Job: executiondomain.Job{
			ID: executionID, CommandID: commandID,
			TenantID:            request.Identity.TenantID,
			ResourceProjectID:   request.Identity.ResourceProjectID,
			ProjectionProjectID: request.Identity.ProjectionProjectID,
			ActorID:             request.Identity.ActorID,
			CapabilityID:        executiondomain.ToolkitExecuteReadCapability,
			Generation:          1, State: executiondomain.JobPending, CreatedAt: createdAt,
		},
		Outbox: executiondomain.OutboxRecord{
			ID: outboxID, CommandID: commandID, ExecutionID: executionID,
			Generation: 1, CreatedAt: createdAt,
		},
	}
	if err := record.Validate(); err != nil {
		return executionapp.AdmissionOutcome{}, fmt.Errorf("%w: %v", ErrInvalidToolkitExecuteReadAdmission, err)
	}
	if err := binding.Validate(record.InputBundle); err != nil {
		return executionapp.AdmissionOutcome{}, fmt.Errorf("%w: %v", ErrInvalidToolkitExecuteReadAdmission, err)
	}

	outcome, err := s.store.AdmitToolkitExecuteRead(ctx, Admission{Record: record, Binding: binding})
	if err != nil {
		return executionapp.AdmissionOutcome{}, fmt.Errorf("admit direct toolkit execution: %w", err)
	}
	if outcome.ExecutionID == "" || outcome.CommandID == "" ||
		outcome.AdmittedAt.IsZero() || !outcome.Deadline.After(outcome.AdmittedAt) {
		return executionapp.AdmissionOutcome{}, errors.New("direct toolkit admission store returned invalid durable outcome")
	}
	return outcome, nil
}

func (s *AdmissionService) allocateIDs() (string, string, string, error) {
	values := make([]string, 3)
	for index := range values {
		value, err := s.newID()
		if err != nil {
			return "", "", "", fmt.Errorf("generate direct toolkit admission ID: %w", err)
		}
		if !validIdentity(value) {
			return "", "", "", errors.New("direct toolkit admission ID generator returned an invalid ID")
		}
		values[index] = value
	}
	return values[0], values[1], values[2], nil
}

func validToolkitReadAdmissionIdentity(identity executionapp.AdmissionIdentity) bool {
	return validIdentity(identity.TenantID) && validIdentity(identity.ResourceProjectID) &&
		validIdentity(identity.ProjectionProjectID) && validIdentity(identity.ActorID)
}

func toolkitReadRequestDigest(identity executionapp.AdmissionIdentity, content []byte) runtimedomain.Digest {
	values := [][]byte{
		[]byte(identity.TenantID), []byte(identity.ResourceProjectID),
		[]byte(identity.ProjectionProjectID), []byte(identity.ActorID),
		[]byte(executiondomain.ToolkitExecuteReadCapability), content,
	}
	material := make([]byte, 0, len(content)+256)
	material = append(material, toolkitReadRequestDigestDomain...)
	for _, value := range values {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		material = append(material, length[:]...)
		material = append(material, value...)
	}
	return runtimedomain.SHA256(material)
}
