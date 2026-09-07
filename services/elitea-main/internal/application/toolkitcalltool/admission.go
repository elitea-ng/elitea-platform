package toolkitcalltool

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

// toolRunRequestDigestDomain separates this capability's idempotency digest
// from every other one. Two capabilities that hashed the same material under
// the same scope and key would answer each other's replays.
const toolRunRequestDigestDomain = "elitea.toolkit.call_tool.admission.v1\x00"

var ErrInvalidToolRunAdmission = errors.New("invalid tool-run admission")

type SubmitRequest struct {
	Identity       executionapp.AdmissionIdentity
	IdempotencyKey string
	Inputs         AuthoritativeInputs
}

// Admission is the atomic unit the store persists: input bytes, job and outbox
// commit together or not at all.
type Admission struct {
	Record  executiondomain.Admission
	Binding executiondomain.ToolkitCallToolBinding
}

type AtomicAdmissionStore interface {
	AdmitToolkitCallTool(context.Context, Admission) (executionapp.AdmissionOutcome, error)
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
		return nil, errors.New("tool-run admission dependencies are required")
	}
	if now == nil {
		now = time.Now
	}
	return &AdmissionService{store: store, factory: factory, now: now, newID: newID}, nil
}

// AdmittedRun is what Submit hands back: the durable outcome plus everything the
// synchronous dispatch needs. The dispatch fields are carried IN MEMORY rather
// than reloaded, which is the whole reason this capability owns no binding
// table — see the package doc.
type AdmittedRun struct {
	Outcome executionapp.AdmissionOutcome
	Binding executiondomain.ToolkitCallToolBinding
	// OutboxID and InputBundle identify the exact command the dispatcher signs.
	OutboxID    string
	InputBundle executiondomain.InputBundle
}

func (s *AdmissionService) Submit(ctx context.Context, request SubmitRequest) (AdmittedRun, error) {
	if err := ctx.Err(); err != nil {
		return AdmittedRun{}, err
	}
	if !validToolRunIdentity(request.Identity) ||
		request.IdempotencyKey == "" || len(request.IdempotencyKey) > 200 {
		return AdmittedRun{}, ErrInvalidToolRunAdmission
	}
	bundle, binding, err := s.factory.Build(ctx, request.Inputs.Clone())
	if err != nil {
		return AdmittedRun{}, fmt.Errorf("%w: %v", ErrInvalidToolRunAdmission, err)
	}
	requestDigest := toolRunRequestDigest(request, binding, bundle)

	executionID, commandID, outboxID, err := s.allocateAdmissionIDs()
	if err != nil {
		return AdmittedRun{}, err
	}
	createdAt := s.now().UTC()
	record := executiondomain.Admission{
		IdempotencyScope: request.Identity.TenantID + "/" +
			request.Identity.ResourceProjectID + "/" + request.Identity.ActorID,
		IdempotencyKey: request.IdempotencyKey,
		RequestDigest:  requestDigest,
		InputBundle:    bundle.Clone(),
		Job: executiondomain.Job{
			ID:                  executionID,
			CommandID:           commandID,
			TenantID:            request.Identity.TenantID,
			ResourceProjectID:   request.Identity.ResourceProjectID,
			ProjectionProjectID: request.Identity.ProjectionProjectID,
			ActorID:             request.Identity.ActorID,
			CapabilityID:        executiondomain.ToolkitCallToolCapability,
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
		return AdmittedRun{}, fmt.Errorf("%w: %v", ErrInvalidToolRunAdmission, err)
	}
	if err := binding.Validate(record.InputBundle); err != nil {
		return AdmittedRun{}, fmt.Errorf("%w: %v", ErrInvalidToolRunAdmission, err)
	}

	outcome, err := s.store.AdmitToolkitCallTool(ctx, Admission{
		Record:  record,
		Binding: binding,
	})
	if err != nil {
		return AdmittedRun{}, fmt.Errorf("admit tool run: %w", err)
	}
	if outcome.ExecutionID == "" || outcome.CommandID == "" ||
		outcome.AdmittedAt.IsZero() || !outcome.Deadline.After(outcome.AdmittedAt) {
		return AdmittedRun{}, errors.New("tool-run admission store returned invalid durable outcome")
	}
	return AdmittedRun{
		Outcome:     outcome,
		Binding:     binding,
		OutboxID:    outboxID,
		InputBundle: bundle.Clone(),
	}, nil
}

func (s *AdmissionService) allocateAdmissionIDs() (string, string, string, error) {
	values := make([]string, 3)
	for index := range values {
		value, err := s.newID()
		if err != nil {
			return "", "", "", fmt.Errorf("generate tool-run admission ID: %w", err)
		}
		if value == "" {
			return "", "", "", errors.New("tool-run admission ID generator returned an empty ID")
		}
		values[index] = value
	}
	return values[0], values[1], values[2], nil
}

func validToolRunIdentity(identity executionapp.AdmissionIdentity) bool {
	return identity.TenantID != "" && identity.ResourceProjectID != "" &&
		identity.ProjectionProjectID != "" && identity.ActorID != ""
}

// toolRunRequestDigest covers the identity, the toolkit, the tool and BOTH
// input contents. A replay under the same idempotency key that changed any of
// them is an idempotency conflict rather than a silent re-answer of the first
// run.
func toolRunRequestDigest(
	request SubmitRequest,
	binding executiondomain.ToolkitCallToolBinding,
	bundle executiondomain.InputBundle,
) runtimedomain.Digest {
	values := [][]byte{
		[]byte(request.Identity.TenantID),
		[]byte(request.Identity.ResourceProjectID),
		[]byte(request.Identity.ProjectionProjectID),
		[]byte(request.Identity.ActorID),
		[]byte(executiondomain.ToolkitCallToolCapability),
		[]byte(binding.ToolkitType),
		[]byte(binding.ToolName),
		[]byte(binding.ToolkitVersion),
	}
	for _, entry := range bundle.Entries {
		values = append(values, []byte(entry.SemanticRole), entry.Content)
	}
	material := make([]byte, 0, 1024)
	material = append(material, toolRunRequestDigestDomain...)
	var toolkitID [8]byte
	binary.BigEndian.PutUint64(toolkitID[:], uint64(binding.ToolkitID))
	material = append(material, toolkitID[:]...)
	for _, value := range values {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		material = append(material, length[:]...)
		material = append(material, value...)
	}
	return runtimedomain.SHA256(material)
}
