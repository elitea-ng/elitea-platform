package indexing

import (
	"math"

	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

type CurrentManualStopCleanupRequest struct {
	ExecutionID string
	Generation  uint64
}

func (r CurrentManualStopCleanupRequest) Validate() error {
	if !validOptionalText(r.ExecutionID, maxIndexAdmissionStringBytes) ||
		r.ExecutionID == "" ||
		r.Generation == 0 ||
		r.Generation > math.MaxInt64 {
		return ErrCurrentIndexMetaInitializationInvalid
	}
	return nil
}

type CurrentManualStopCleanupResolution string

const (
	CurrentManualStopCleanupApplied    CurrentManualStopCleanupResolution = "APPLIED"
	CurrentManualStopCleanupSuperseded CurrentManualStopCleanupResolution = "SUPERSEDED"
)

type CurrentManualStopCleanupClaim struct {
	CurrentManualStopCleanupRequest
	ClaimToken string
}

func (c CurrentManualStopCleanupClaim) Validate() error {
	if c.CurrentManualStopCleanupRequest.Validate() != nil ||
		!validOptionalText(c.ClaimToken, maxIndexAdmissionStringBytes) ||
		c.ClaimToken == "" {
		return ErrCurrentIndexMetaInitializationInvalid
	}
	return nil
}

// CurrentManualStopCleanup is derived only from immutable admission evidence.
// It contains no worker-selected tenant, project, schema, or collection
// authority.
type CurrentManualStopCleanup struct {
	MetaID          string
	ExecutionID     string
	Generation      uint64
	IndexGeneration uint64
	IndexName       string
	ToolkitID       int32
}

func (c CurrentManualStopCleanup) Validate() error {
	if !validOptionalText(c.MetaID, executiondomain.MaxIndexMetaIDBytes) ||
		c.MetaID == "" ||
		!validOptionalText(c.ExecutionID, maxIndexAdmissionStringBytes) ||
		c.ExecutionID == "" ||
		c.Generation == 0 ||
		c.Generation > math.MaxInt64 ||
		c.IndexGeneration == 0 ||
		c.IndexGeneration > math.MaxInt64 ||
		c.IndexName == "" ||
		len(c.IndexName) > maxIndexAdmissionStringBytes ||
		c.ToolkitID <= 0 {
		return ErrCurrentIndexMetaInitializationInvalid
	}
	return nil
}
