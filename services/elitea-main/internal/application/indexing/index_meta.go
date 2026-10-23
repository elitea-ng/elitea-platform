package indexing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strconv"
	"time"

	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
)

const (
	MaxCurrentInitialIndexMetaBytes = 1 << 20

	// MaxCurrentIndexMetaHistoryEntries is the largest number of run-history
	// entries one index metadata row keeps. History accumulates one entry for
	// each run, and each entry clones the top level, so an unbounded history
	// grows the row until it passes MaxCurrentInitialIndexMetaBytes and the
	// index can no longer record a run. The writer discards the oldest entries
	// first.
	MaxCurrentIndexMetaHistoryEntries = 200
)

var (
	ErrCurrentIndexMetaInitializationInvalid      = errors.New("current index metadata initialization is invalid")
	ErrCurrentIndexMetaTargetUnavailable          = errors.New("current index metadata target is unavailable")
	ErrCurrentIndexMetaMaterializationUnavailable = errors.New("current index metadata materialization is unavailable")
	ErrCurrentIndexMetaConflict                   = errors.New("another index execution is already active")
	ErrCurrentIndexMetaSuperseded                 = errors.New("index metadata terminal effect was superseded")
)

type CurrentIndexMetaTerminalState string

const (
	CurrentIndexMetaFailed    CurrentIndexMetaTerminalState = "failed"
	CurrentIndexMetaCancelled CurrentIndexMetaTerminalState = "cancelled"
)

// CurrentIndexMetaTerminalBinding is the immutable admission evidence needed
// to terminalize the matching external metadata generation. The frozen toolkit
// bytes contain references only and are redeemed immediately before the write.
type CurrentIndexMetaTerminalBinding struct {
	ResourceProjectID    int32
	ActorUserID          int32
	ToolkitID            int32
	IndexName            string
	MetaID               string
	ExecutionID          string
	Generation           uint64
	IndexGeneration      uint64
	ToolkitConfiguration json.RawMessage
}

func (b CurrentIndexMetaTerminalBinding) Validate() error {
	if b.ResourceProjectID <= 0 || b.ActorUserID <= 0 || b.ToolkitID <= 0 ||
		b.IndexName == "" || len(b.IndexName) > maxIndexAdmissionStringBytes ||
		!validOptionalText(b.MetaID, executiondomain.MaxIndexMetaIDBytes) || b.MetaID == "" ||
		!validOptionalText(b.ExecutionID, maxIndexAdmissionStringBytes) || b.ExecutionID == "" ||
		b.Generation == 0 || b.Generation > math.MaxInt64 ||
		b.IndexGeneration == 0 || b.IndexGeneration > math.MaxInt64 ||
		len(b.ToolkitConfiguration) == 0 ||
		len(b.ToolkitConfiguration) > executiondomain.MaxInputEntryContentBytes ||
		!validBoundedJSONObject(b.ToolkitConfiguration) {
		return ErrCurrentIndexMetaInitializationInvalid
	}
	return nil
}

type CurrentIndexMetaTerminalRequest struct {
	ExecutionID string
	Generation  uint64
	State       CurrentIndexMetaTerminalState
	OccurredAt  time.Time
	SafeError   string
}

type CurrentIndexMetaTerminalResolution string

const (
	CurrentIndexMetaTerminalApplied    CurrentIndexMetaTerminalResolution = "APPLIED"
	CurrentIndexMetaTerminalSuperseded CurrentIndexMetaTerminalResolution = "SUPERSEDED"
)

type CurrentIndexMetaTerminalClaim struct {
	CurrentIndexMetaTerminalRequest
	ClaimToken string
}

func (c CurrentIndexMetaTerminalClaim) Validate() error {
	if err := c.CurrentIndexMetaTerminalRequest.Validate(); err != nil ||
		!validOptionalText(c.ClaimToken, maxIndexAdmissionStringBytes) ||
		c.ClaimToken == "" {
		return ErrCurrentIndexMetaInitializationInvalid
	}
	return nil
}

func (r CurrentIndexMetaTerminalRequest) Validate() error {
	if !validOptionalText(r.ExecutionID, maxIndexAdmissionStringBytes) || r.ExecutionID == "" ||
		r.Generation == 0 || r.Generation > math.MaxInt64 || r.OccurredAt.IsZero() {
		return ErrCurrentIndexMetaInitializationInvalid
	}
	switch r.State {
	case CurrentIndexMetaFailed:
		if !validOptionalText(r.SafeError, maxIndexAdmissionStringBytes) || r.SafeError == "" {
			return ErrCurrentIndexMetaInitializationInvalid
		}
		return nil
	case CurrentIndexMetaCancelled:
		if r.SafeError != "" {
			return ErrCurrentIndexMetaInitializationInvalid
		}
		return nil
	default:
		return ErrCurrentIndexMetaInitializationInvalid
	}
}

type CurrentTerminalIndexMeta struct {
	MetaID          string
	ExecutionID     string
	Generation      uint64
	IndexGeneration uint64
	IndexName       string
	ToolkitID       int32
	State           CurrentIndexMetaTerminalState
	OccurredAt      time.Time
	SafeError       string
}

func (m CurrentTerminalIndexMeta) Validate() error {
	if !validOptionalText(m.MetaID, executiondomain.MaxIndexMetaIDBytes) || m.MetaID == "" ||
		m.IndexGeneration == 0 || m.IndexGeneration > math.MaxInt64 ||
		m.ToolkitID <= 0 || m.IndexName == "" || len(m.IndexName) > maxIndexAdmissionStringBytes {
		return ErrCurrentIndexMetaInitializationInvalid
	}
	return CurrentIndexMetaTerminalRequest{
		ExecutionID: m.ExecutionID,
		Generation:  m.Generation,
		State:       m.State,
		OccurredAt:  m.OccurredAt,
		SafeError:   m.SafeError,
	}.Validate()
}

type CurrentIndexMetaTerminalBindingRepository interface {
	LoadCurrentIndexMetaTerminalBinding(context.Context, string, uint64) (CurrentIndexMetaTerminalBinding, error)
}

func decodeCurrentIndexMetaObject(raw []byte) (map[string]any, error) {
	if len(raw) == 0 || len(raw) > MaxCurrentInitialIndexMetaBytes {
		return nil, ErrCurrentIndexMetaInitializationInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, ErrCurrentIndexMetaInitializationInvalid
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, ErrCurrentIndexMetaInitializationInvalid
	}
	return object, nil
}

func currentIndexMetaIdentityID(value string) (int32, bool) {
	parsed, err := strconv.ParseInt(value, 10, 32)
	return int32(parsed), err == nil && parsed > 0 && strconv.FormatInt(parsed, 10) == value
}

func currentIndexMetaInitializationError(ctx context.Context, cause error) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if errors.Is(cause, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(cause, ErrCurrentIndexMetaConflict) {
		return ErrCurrentIndexMetaConflict
	}
	if errors.Is(cause, ErrCurrentIndexMetaSuperseded) {
		return ErrCurrentIndexMetaSuperseded
	}
	if errors.Is(cause, ErrCurrentIndexMetaTargetUnavailable) {
		return ErrCurrentIndexMetaTargetUnavailable
	}
	return ErrCurrentIndexMetaMaterializationUnavailable
}
