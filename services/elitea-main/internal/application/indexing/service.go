package indexing

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

const indexStartIdempotencyPrefix = "index-start-v1:"

type AuthoritativeInputResolver interface {
	Resolve(context.Context, StartRequest) (AuthoritativeInputs, error)
}

type IndexAdmissionSubmitter interface {
	Submit(context.Context, SubmitRequest) (AdmissionOutcome, error)
}

// StartService is the application boundary between the current UI request and
// the durable index runtime. It resolves saved server-side state before
// admission; client toolkit settings and credentials have no field here.
type StartService struct {
	resolver   AuthoritativeInputResolver
	admissions IndexAdmissionSubmitter
	newID      executionapp.IDGenerator
}

func NewStartService(
	resolver AuthoritativeInputResolver,
	admissions IndexAdmissionSubmitter,
	newID executionapp.IDGenerator,
) (*StartService, error) {
	if resolver == nil || admissions == nil || newID == nil {
		return nil, errors.New("index start dependencies are required")
	}
	return &StartService{resolver: resolver, admissions: admissions, newID: newID}, nil
}

func (s *StartService) StartIndexData(ctx context.Context, request StartRequest) (StartOutcome, error) {
	if ctx == nil {
		return StartOutcome{}, ErrInvalidIndexStart
	}
	if err := ctx.Err(); err != nil {
		return StartOutcome{}, err
	}
	if err := request.Validate(); err != nil || request.ToolkitID > math.MaxInt32 {
		return StartOutcome{}, ErrInvalidIndexStart
	}

	inputs, err := s.resolver.Resolve(ctx, request.Clone())
	if err != nil {
		return StartOutcome{}, err
	}
	idempotencyKey, correlationID, err := s.admissionCorrelation(request)
	if err != nil {
		return StartOutcome{}, err
	}

	return s.submitResolved(ctx, resolvedStart{
		ProjectID:              request.ProjectID,
		AttributionActorUserID: request.ActorUserID,
		ToolkitID:              request.ToolkitID,
		Inputs:                 inputs,
		IdempotencyKey:         idempotencyKey,
		CorrelationID:          correlationID,
		ClientStreamID:         request.StreamID,
		ClientMessageID:        request.MessageID,
		SIOEvent:               request.SIOEvent,
		Initiator:              executiondomain.IndexIngestInitiatorUser,
	})
}

// StartScheduledIndexData enters the same durable index.ingest.v1 path as the
// public start without re-resolving mutable toolkit state or fabricating a
// browser stream. The stored actor remains created_by attribution; schedule
// runtime authorization selects the project-system PAT from the typed
// initiator at claim time.
func (s *StartService) StartScheduledIndexData(
	ctx context.Context,
	request ScheduledStartRequest,
) (StartOutcome, error) {
	if s == nil || s.admissions == nil || ctx == nil ||
		request.Validate() != nil ||
		request.ProjectID > math.MaxInt32 ||
		request.AttributionActorUserID > math.MaxInt32 ||
		request.ToolkitID > math.MaxInt32 {
		return StartOutcome{}, ErrInvalidIndexStart
	}
	if err := ctx.Err(); err != nil {
		return StartOutcome{}, err
	}
	return s.submitResolved(ctx, resolvedStart{
		ProjectID:              request.ProjectID,
		AttributionActorUserID: request.AttributionActorUserID,
		ToolkitID:              request.ToolkitID,
		Inputs:                 request.Inputs,
		IdempotencyKey:         request.IdempotencyKey,
		CorrelationID:          request.CorrelationID,
		Initiator:              executiondomain.IndexIngestInitiatorSchedule,
	})
}

type resolvedStart struct {
	ProjectID              int64
	AttributionActorUserID int64
	ToolkitID              int64
	Inputs                 AuthoritativeInputs
	IdempotencyKey         string
	CorrelationID          string
	ClientStreamID         string
	ClientMessageID        string
	SIOEvent               string
	Initiator              executiondomain.IndexIngestInitiator
}

func (s *StartService) submitResolved(
	ctx context.Context,
	request resolvedStart,
) (StartOutcome, error) {
	projectID := strconv.FormatInt(request.ProjectID, 10)
	actorID := strconv.FormatInt(request.AttributionActorUserID, 10)
	outcome, err := s.admissions.Submit(ctx, SubmitRequest{
		Identity: executionapp.AdmissionIdentity{
			// The current platform has project-local schemas but no independent
			// tenant identifier. Phase one therefore uses the authorized project
			// as the tenant/resource/projection boundary, matching validation.
			TenantID:            projectID,
			ResourceProjectID:   projectID,
			ProjectionProjectID: projectID,
			ActorID:             actorID,
		},
		IdempotencyKey:  request.IdempotencyKey,
		CorrelationID:   request.CorrelationID,
		ClientStreamID:  request.ClientStreamID,
		ClientMessageID: request.ClientMessageID,
		SIOEvent:        request.SIOEvent,
		ToolkitID:       int32(request.ToolkitID),
		Initiator:       request.Initiator,
		Inputs:          request.Inputs.Clone(),
	})
	if err != nil {
		return StartOutcome{}, err
	}
	return currentStartOutcome(outcome)
}

func currentStartOutcome(outcome AdmissionOutcome) (StartOutcome, error) {
	// Both public and scheduled callers observe success only after the initial
	// index metadata (the index registry row) is durable.
	if outcome.ExecutionID == "" || outcome.CommandID == "" ||
		outcome.IndexMetaInitializedAt == nil ||
		outcome.IndexMetaInitializedAt.IsZero() ||
		outcome.AdmittedAt.IsZero() ||
		!outcome.Deadline.After(outcome.AdmittedAt) {
		return StartOutcome{}, errors.New(
			"index admission returned an invalid outcome",
		)
	}
	return StartOutcome{
		TaskID:  outcome.ExecutionID,
		Created: outcome.Created,
	}, nil
}

func (s *StartService) admissionCorrelation(request StartRequest) (string, string, error) {
	correlation := request.MessageID
	if correlation == "" {
		correlation = request.StreamID
	}
	idempotencyCorrelation := request.StreamID + "\x00" + request.MessageID
	if request.StreamID == "" && request.MessageID == "" {
		nonce, err := s.newID()
		if err != nil {
			return "", "", fmt.Errorf("generate index start correlation: %w", err)
		}
		if nonce == "" {
			return "", "", errors.New("index start correlation generator returned an empty ID")
		}
		correlation = nonce
		idempotencyCorrelation = nonce
	}

	material := make([]byte, 0, len(idempotencyCorrelation)+64)
	material = appendLengthPrefixed(material, strconv.FormatInt(request.ProjectID, 10))
	material = appendLengthPrefixed(material, strconv.FormatInt(request.ActorUserID, 10))
	material = appendLengthPrefixed(material, strconv.FormatInt(request.ToolkitID, 10))
	material = appendLengthPrefixed(material, idempotencyCorrelation)
	digest := sha256.Sum256(material)
	return indexStartIdempotencyPrefix + hex.EncodeToString(digest[:]), correlation, nil
}

func appendLengthPrefixed(target []byte, value string) []byte {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	target = append(target, length[:]...)
	return append(target, value...)
}

var _ interface {
	StartIndexData(context.Context, StartRequest) (StartOutcome, error)
	StartScheduledIndexData(context.Context, ScheduledStartRequest) (StartOutcome, error)
} = (*StartService)(nil)
