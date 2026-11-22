package indexing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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

	// MaxCurrentIndexMetaSafeErrorBytes bounds the failure message a terminal
	// effect carries into the index registry. It is its own limit, not the
	// 256-byte admission string limit: worker failure messages routinely run
	// past 256 bytes, and refusing one stalled the terminal reconciler.
	MaxCurrentIndexMetaSafeErrorBytes = 4 * 1024

	// MaxCurrentIndexMetaTerminalInvalidAttempts is how many claims a terminal
	// effect that fails validation gets before it is quarantined. Validation
	// is deterministic, so the retries only cover a fix shipped meanwhile.
	MaxCurrentIndexMetaTerminalInvalidAttempts = 5

	currentIndexMetaSafeErrorTruncationMarker = " [truncated]"
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
	if err := c.CurrentIndexMetaTerminalRequest.Validate(); err != nil {
		return err
	}
	if c.ClaimToken == "" || !validOptionalText(c.ClaimToken, maxIndexAdmissionStringBytes) {
		return invalidCurrentIndexMetaTerminal("the claim token is empty or malformed")
	}
	return nil
}

func (r CurrentIndexMetaTerminalRequest) Validate() error {
	switch {
	case r.ExecutionID == "" || !validOptionalText(r.ExecutionID, maxIndexAdmissionStringBytes):
		return invalidCurrentIndexMetaTerminal("the execution id is empty or malformed")
	case r.Generation == 0 || r.Generation > math.MaxInt64:
		return invalidCurrentIndexMetaTerminal("the generation is out of range")
	case r.OccurredAt.IsZero():
		return invalidCurrentIndexMetaTerminal("the terminal time is missing")
	}
	switch r.State {
	case CurrentIndexMetaFailed:
		if err := validateCurrentIndexMetaSafeError(r.SafeError); err != nil {
			return err
		}
		return nil
	case CurrentIndexMetaCancelled:
		if r.SafeError != "" {
			return invalidCurrentIndexMetaTerminal("a cancelled terminal carries an error message")
		}
		return nil
	default:
		return invalidCurrentIndexMetaTerminal(fmt.Sprintf("the terminal state %q is unknown", r.State))
	}
}

// invalidCurrentIndexMetaTerminal names the check that refused a terminal
// effect. It still matches ErrCurrentIndexMetaInitializationInvalid, so callers
// that classify by the sentinel keep working, but a log line says which check
// failed instead of only that one did.
func invalidCurrentIndexMetaTerminal(reason string) error {
	return fmt.Errorf("%w: %s", ErrCurrentIndexMetaInitializationInvalid, reason)
}

func validateCurrentIndexMetaSafeError(value string) error {
	switch {
	case value == "":
		return invalidCurrentIndexMetaTerminal("a failed terminal has no error message")
	case len(value) > MaxCurrentIndexMetaSafeErrorBytes:
		return invalidCurrentIndexMetaTerminal(fmt.Sprintf(
			"the error message is %d bytes, over the %d-byte limit",
			len(value), MaxCurrentIndexMetaSafeErrorBytes,
		))
	case !utf8.ValidString(value):
		return invalidCurrentIndexMetaTerminal("the error message is not valid UTF-8")
	case strings.ContainsRune(value, '\x00'):
		return invalidCurrentIndexMetaTerminal("the error message contains a NUL byte")
	}
	return nil
}

// BoundCurrentIndexMetaSafeError makes a worker-supplied failure message fit a
// terminal effect. The output projection accepts index failure messages of up
// to 48 KiB with line breaks, so the terminal path must not refuse what the
// projection already stored: it drops NUL bytes and invalid UTF-8, keeps line
// breaks, and cuts a message over MaxCurrentIndexMetaSafeErrorBytes at a rune
// boundary, ending it with a marker so a reader knows text is missing.
func BoundCurrentIndexMetaSafeError(value string) string {
	value = strings.ReplaceAll(strings.ToValidUTF8(value, ""), "\x00", "")
	if len(value) <= MaxCurrentIndexMetaSafeErrorBytes {
		return value
	}
	cut := MaxCurrentIndexMetaSafeErrorBytes - len(currentIndexMetaSafeErrorTruncationMarker)
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + currentIndexMetaSafeErrorTruncationMarker
}

// CurrentIndexMetaTerminalClaimRef addresses one claimed terminal effect even
// when the rest of the row cannot be trusted.
type CurrentIndexMetaTerminalClaimRef struct {
	ExecutionID string
	Generation  uint64
	ClaimToken  string
}

// CurrentIndexMetaTerminalRejection is one claimed terminal effect that cannot
// be applied, and why.
type CurrentIndexMetaTerminalRejection struct {
	CurrentIndexMetaTerminalClaimRef
	Cause error
}

func (r CurrentIndexMetaTerminalRejection) Error() string {
	return fmt.Sprintf(
		"index metadata terminal %q generation %d: %v",
		r.ExecutionID, r.Generation, r.Cause,
	)
}

func (r CurrentIndexMetaTerminalRejection) Unwrap() error { return r.Cause }

// CurrentIndexMetaTerminalRejections is the error a claim returns NEXT TO its
// valid claims when some claimed rows failed validation. The caller applies the
// valid claims and quarantines or backs off each rejected one: one bad row must
// never cost the rest of the batch its pass.
type CurrentIndexMetaTerminalRejections []CurrentIndexMetaTerminalRejection

func (r CurrentIndexMetaTerminalRejections) Error() string {
	return errors.Join(r.Unwrap()...).Error()
}

func (r CurrentIndexMetaTerminalRejections) Unwrap() []error {
	errs := make([]error, len(r))
	for index := range r {
		errs[index] = r[index]
	}
	return errs
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
