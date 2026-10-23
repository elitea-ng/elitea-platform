package execution

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	configurationdomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/configurations"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

var (
	ErrInvalidAdmission           = errors.New("invalid validation admission")
	ErrIdempotencyConflict        = errors.New("idempotency key already used for a different request")
	ErrAdmissionCapacityExhausted = errors.New("execution admission capacity is exhausted")
)

const (
	admissionCapacityRetryAfter              = time.Second
	validationRequestDigestDomain            = "elitea.configuration.validate.admission.v1\x00"
	validationRequestDigestValueCount        = 20
	validationRequestDigestLengthPrefixBytes = 8
	maxValidationRequestDigestStringBytes    = 256
	maxValidationRequestDigestMaterialBytes  = len(validationRequestDigestDomain) +
		validationRequestDigestValueCount*(validationRequestDigestLengthPrefixBytes+maxValidationRequestDigestStringBytes) +
		validationRequestDigestLengthPrefixBytes + executiondomain.MaxInputEntryContentBytes
)

// AdmissionCapacityError is a retryable rejection of a new durable execution.
// Exact idempotent replays do not consume capacity and must not return this
// error. The fixed retry delay is intentionally small and bounded so public
// transports can emit a safe Retry-After value without exposing queue state.
type AdmissionCapacityError struct {
	CapabilityID   string
	MaxOutstanding int64
}

func (e *AdmissionCapacityError) Error() string {
	return fmt.Sprintf("%s for capability %q at limit %d", ErrAdmissionCapacityExhausted, e.CapabilityID, e.MaxOutstanding)
}

func (e *AdmissionCapacityError) Unwrap() error { return ErrAdmissionCapacityExhausted }

func (e *AdmissionCapacityError) Retryable() bool { return true }

func (e *AdmissionCapacityError) RetryAfter() time.Duration { return admissionCapacityRetryAfter }

type AdmissionIdentity struct {
	TenantID            string
	ResourceProjectID   string
	ProjectionProjectID string
	ActorID             string
}

type SubmitValidationRequest struct {
	Identity       AdmissionIdentity
	IdempotencyKey string
	InputBundle    executiondomain.InputBundle
	Command        configurationdomain.ValidationCommand
}

type ValidationAdmission struct {
	Record  executiondomain.Admission
	Command configurationdomain.ValidationCommand
}

type AdmissionOutcome struct {
	ExecutionID string
	CommandID   string
	Created     bool
	// AdmittedAt and Deadline are the durable store's authoritative clock
	// values. Exact idempotent replay must return the original pair.
	AdmittedAt time.Time
	Deadline   time.Time
}

// AtomicAdmissionStore owns the transaction that inserts the immutable input
// bundle, execution job and command outbox. Implementations must compare the
// stored request digest when an idempotency key already exists and return
// ErrIdempotencyConflict on a different request. Successful creation and
// replay must return the durable AdmittedAt and Deadline values.
type AtomicAdmissionStore interface {
	AdmitValidation(ctx context.Context, admission ValidationAdmission) (AdmissionOutcome, error)
}

type IDGenerator func() (string, error)

type SubmitJobService struct {
	store AtomicAdmissionStore
	now   func() time.Time
	newID IDGenerator
}

func NewSubmitJobService(store AtomicAdmissionStore, now func() time.Time, newID IDGenerator) (*SubmitJobService, error) {
	if store == nil {
		return nil, errors.New("admission store is required")
	}
	if now == nil {
		now = time.Now
	}
	if newID == nil {
		newID = randomID
	}
	return &SubmitJobService{store: store, now: now, newID: newID}, nil
}

func (s *SubmitJobService) SubmitValidation(ctx context.Context, request SubmitValidationRequest) (AdmissionOutcome, error) {
	if err := validateIdentity(request.Identity); err != nil {
		return AdmissionOutcome{}, err
	}
	if request.IdempotencyKey == "" || len(request.IdempotencyKey) > 200 {
		return AdmissionOutcome{}, ErrInvalidAdmission
	}
	if err := request.InputBundle.Validate(); err != nil {
		return AdmissionOutcome{}, fmt.Errorf("%w: %v", ErrInvalidAdmission, err)
	}
	if err := request.Command.Validate(); err != nil {
		return AdmissionOutcome{}, fmt.Errorf("%w: %v", ErrInvalidAdmission, err)
	}
	if len(request.InputBundle.Entries) != 1 || request.Command.SettingsEntryID != request.InputBundle.Entries[0].ID {
		return AdmissionOutcome{}, fmt.Errorf("%w: settings entry mismatch", ErrInvalidAdmission)
	}
	requestDigest, err := validationRequestDigest(request)
	if err != nil {
		return AdmissionOutcome{}, err
	}

	executionID, err := s.newID()
	if err != nil {
		return AdmissionOutcome{}, fmt.Errorf("generate execution ID: %w", err)
	}
	commandID, err := s.newID()
	if err != nil {
		return AdmissionOutcome{}, fmt.Errorf("generate command ID: %w", err)
	}
	outboxID, err := s.newID()
	if err != nil {
		return AdmissionOutcome{}, fmt.Errorf("generate outbox ID: %w", err)
	}

	createdAt := s.now().UTC()
	record := executiondomain.Admission{
		IdempotencyScope: request.Identity.TenantID + "/" + request.Identity.ResourceProjectID + "/" + request.Identity.ActorID,
		IdempotencyKey:   request.IdempotencyKey,
		RequestDigest:    requestDigest,
		InputBundle:      request.InputBundle.Clone(),
		Job: executiondomain.Job{
			ID:                  executionID,
			CommandID:           commandID,
			TenantID:            request.Identity.TenantID,
			ResourceProjectID:   request.Identity.ResourceProjectID,
			ProjectionProjectID: request.Identity.ProjectionProjectID,
			ActorID:             request.Identity.ActorID,
			CapabilityID:        executiondomain.ConfigurationValidationCapability,
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
		return AdmissionOutcome{}, fmt.Errorf("%w: %v", ErrInvalidAdmission, err)
	}

	outcome, err := s.store.AdmitValidation(ctx, ValidationAdmission{Record: record, Command: request.Command})
	if err != nil {
		return AdmissionOutcome{}, fmt.Errorf("admit configuration validation: %w", err)
	}
	if outcome.ExecutionID == "" || outcome.CommandID == "" || outcome.AdmittedAt.IsZero() || !outcome.Deadline.After(outcome.AdmittedAt) {
		return AdmissionOutcome{}, errors.New("admission store returned invalid durable outcome")
	}
	return outcome, nil
}

func validateIdentity(identity AdmissionIdentity) error {
	if identity.TenantID == "" || identity.ResourceProjectID == "" || identity.ProjectionProjectID == "" || identity.ActorID == "" {
		return ErrInvalidAdmission
	}
	return nil
}

func validationRequestDigest(request SubmitValidationRequest) (runtimedomain.Digest, error) {
	// Bind only admitted semantics. Bundle and content IDs, and therefore the
	// serialized manifest digest, are generated by the server for each submit
	// attempt. Including them would make a retry with the same idempotency key
	// conflict with itself before the durable winner can be replayed.
	entry := request.InputBundle.Entries[0]
	values := []string{
		request.Identity.TenantID,
		request.Identity.ResourceProjectID,
		request.Identity.ProjectionProjectID,
		request.Identity.ActorID,
		request.Command.ConfigurationRevisionID,
		request.Command.ConfigurationType,
		request.Command.CatalogRevision,
		request.Command.CatalogDigest.String(),
		request.Command.SchemaID,
		request.Command.SchemaRevision,
		request.Command.SchemaDigest.String(),
		request.InputBundle.Version,
		request.InputBundle.MediaType,
		entry.ID,
		entry.Version,
		entry.SemanticRole,
		entry.MediaType,
		entry.Classification,
		entry.RequiredGrantAudience,
		entry.ContentDigest.String(),
	}

	materialSize, err := validationRequestDigestMaterialSize(values, len(entry.Content))
	if err != nil {
		return runtimedomain.Digest{}, err
	}
	if materialSize < 0 || materialSize > maxValidationRequestDigestMaterialBytes {
		return runtimedomain.Digest{}, fmt.Errorf("%w: validation request digest material exceeds the protocol limit", ErrInvalidAdmission)
	}
	material := make([]byte, 0, materialSize)
	material = append(material, validationRequestDigestDomain...)
	for _, value := range values {
		material = appendLengthPrefixed(material, []byte(value))
	}
	material = appendLengthPrefixed(material, entry.Content)
	return runtimedomain.SHA256(material), nil
}

func validationRequestDigestMaterialSize(values []string, contentLength int) (int, error) {
	if len(values) != validationRequestDigestValueCount || contentLength < 0 {
		return 0, fmt.Errorf("%w: request digest material is invalid", ErrInvalidAdmission)
	}
	size := len(validationRequestDigestDomain)
	for _, value := range values {
		if value == "" || len(value) > maxValidationRequestDigestStringBytes {
			return 0, fmt.Errorf("%w: request digest metadata exceeds protocol limits", ErrInvalidAdmission)
		}
		var ok bool
		size, ok = checkedValidationRequestDigestSize(size, validationRequestDigestLengthPrefixBytes)
		if !ok {
			return 0, fmt.Errorf("%w: request digest size overflow", ErrInvalidAdmission)
		}
		size, ok = checkedValidationRequestDigestSize(size, len(value))
		if !ok {
			return 0, fmt.Errorf("%w: request digest size overflow", ErrInvalidAdmission)
		}
	}
	var ok bool
	size, ok = checkedValidationRequestDigestSize(size, validationRequestDigestLengthPrefixBytes)
	if !ok {
		return 0, fmt.Errorf("%w: request digest size overflow", ErrInvalidAdmission)
	}
	size, ok = checkedValidationRequestDigestSize(size, contentLength)
	if !ok {
		return 0, fmt.Errorf("%w: request digest size overflow", ErrInvalidAdmission)
	}
	if contentLength > executiondomain.MaxInputEntryContentBytes ||
		size > maxValidationRequestDigestMaterialBytes {
		return 0, fmt.Errorf("%w: request digest content exceeds protocol limits", ErrInvalidAdmission)
	}
	return size, nil
}

func checkedValidationRequestDigestSize(size, increment int) (int, bool) {
	if size < 0 || increment < 0 || size > math.MaxInt-increment {
		return 0, false
	}
	return size + increment, true
}

func appendLengthPrefixed(dst, value []byte) []byte {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	dst = append(dst, length[:]...)
	return append(dst, value...)
}

func randomID() (string, error) {
	var value [16]byte
	if _, err := io.ReadFull(rand.Reader, value[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", value[:]), nil
}
