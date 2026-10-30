package agentexecution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/protobuf/proto"
)

// The platform preamble (ADR-0043 decision 3, #1500) is a prompt, so it is
// data-plane content: admission freezes the newest revision into the run's
// input (AgentExecutionInputV1.platform_preamble, field 67), and the bundle
// digest covers it. The command (WorkerCommandV1.content_controls, field 71)
// carries only its revision and digest, and the worker checks them against
// the input. A change after admission does not reach the admitted turn.

// MaxPlatformPreambleBytes bounds the preamble text (shared 0216 and the
// worker hold the same bound).
const MaxPlatformPreambleBytes = 8 * 1024

// PlatformPreambleSource answers the newest platform preamble revision, or
// nil when there is none or its text is empty.
type PlatformPreambleSource interface {
	PlatformPreamble(ctx context.Context) (*runtimev1.PlatformPreambleSnapshotV1, error)
}

// WithPlatformPreamble makes admission freeze the newest platform preamble
// into each turn's input.
func (s *AdmissionService) WithPlatformPreamble(source PlatformPreambleSource) *AdmissionService {
	if s != nil && source != nil {
		s.preamble = source
	}
	return s
}

// stampPlatformPreamble returns the input with the newest preamble. A read
// error refuses the turn: the degraded value would run agents without the
// organisation's rules. The caller's input is not changed.
func (s *AdmissionService) stampPlatformPreamble(
	ctx context.Context, input *runtimev1.AgentExecutionInputV1,
) (*runtimev1.AgentExecutionInputV1, error) {
	if s.preamble == nil {
		return input, nil
	}
	snapshot, err := s.preamble.PlatformPreamble(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the platform preamble: %w", err)
	}
	if snapshot != nil && !ValidPlatformPreambleSnapshot(snapshot) {
		return nil, fmt.Errorf("%w: the platform preamble revision is malformed", ErrInvalidAuthoritativeAgentInput)
	}
	if snapshot == nil && input.GetPlatformPreamble() == nil {
		return input, nil
	}
	stamped, ok := proto.Clone(input).(*runtimev1.AgentExecutionInputV1)
	if !ok {
		return nil, ErrInvalidAuthoritativeAgentInput
	}
	stamped.PlatformPreamble = snapshot
	return stamped, nil
}

// ValidPlatformPreambleSnapshot reports a well-formed snapshot: a revision,
// a text of at most 8 KiB without NUL, and the lowercase hex SHA-256 of
// that text.
func ValidPlatformPreambleSnapshot(snapshot *runtimev1.PlatformPreambleSnapshotV1) bool {
	text := snapshot.GetText()
	return snapshot.GetRevision() > 0 && text != "" && len(text) <= MaxPlatformPreambleBytes &&
		utf8.ValidString(text) && !strings.ContainsRune(text, 0) &&
		snapshot.GetSha256() == PlatformPreambleDigest(text)
}

// PlatformPreambleDigest is the lowercase hex SHA-256 of the text.
func PlatformPreambleDigest(text string) string {
	digest := sha256.Sum256([]byte(text))
	return hex.EncodeToString(digest[:])
}
