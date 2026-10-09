package localturn

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"regexp"
	"strconv"
)

// A sensitive remote toolkit call's confirmation (ADR-0029 decision 5b) is
// bound to the ONE call it approves and is used ONCE.
//
// The call is named by its call digest: the turn, the toolkit, the agent
// version, the tool and the SHA-256 of its arguments. The interrupt id the
// server hands out in 409 confirmation_required is derived from that digest
// and a sequence number: how many confirmations of the same call this turn has
// already consumed. The retry must echo the id; the server recomputes it from
// the call it actually received, so an approval of one call cannot run
// another (other arguments, another tool, another turn). Once consumed, the
// same call needs a fresh approval, whose id is the next in the sequence. A
// retry of the consuming request itself (the same Idempotency-Key) is the same
// call, not a second use.

const (
	confirmationCallDomain      = "elitea.local-turn.confirmation-call.v1\x00"
	confirmationInterruptDomain = "elitea.local-turn.confirmation-interrupt.v1\x00"
)

var confirmationInterruptShape = regexp.MustCompile(`^hitl_[0-9a-f]{32}$`)

// ValidConfirmationInterruptID reports whether id has the shape the server
// hands out.
func ValidConfirmationInterruptID(id string) bool { return confirmationInterruptShape.MatchString(id) }

// ConfirmationCall names one remote toolkit call for its confirmation.
type ConfirmationCall struct {
	ExecutionID     string
	ToolkitID       int64
	ApplicationID   int64
	VersionID       int64
	ToolName        string
	ArgumentsSHA256 string
}

// Digest is the call digest: 64 lowercase hex characters.
func (c ConfirmationCall) Digest() string {
	digest := sha256.New()
	_, _ = digest.Write([]byte(confirmationCallDomain))
	for _, part := range []string{
		c.ExecutionID,
		strconv.FormatInt(c.ToolkitID, 10),
		strconv.FormatInt(c.ApplicationID, 10),
		strconv.FormatInt(c.VersionID, 10),
		c.ToolName,
		c.ArgumentsSHA256,
	} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		_, _ = digest.Write(length[:])
		_, _ = digest.Write([]byte(part))
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// ConfirmationInterruptID is the interrupt id of the sequence-th confirmation
// (from 0) of the call whose digest is callDigest.
func ConfirmationInterruptID(callDigest string, sequence int) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte(confirmationInterruptDomain))
	_, _ = digest.Write([]byte(callDigest))
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(sequence))
	_, _ = digest.Write(counter[:])
	return "hitl_" + hex.EncodeToString(digest.Sum(nil)[:16])
}

// ConfirmationClaim is one confirmed call presenting its interrupt id.
type ConfirmationClaim struct {
	ExecutionID    string
	CallDigest     string
	InterruptID    string
	IdempotencyKey string
}

// ConfirmationOutcome answers a claim: Accepted, or the interrupt id the
// caller must have approved instead (NextInterruptID).
type ConfirmationOutcome struct {
	Accepted        bool
	NextInterruptID string
}

// ConfirmationLedger records the confirmations a turn's calls consumed
// (elitea_runtime.local_turn_confirmations).
type ConfirmationLedger interface {
	// NextConfirmationInterruptID answers the interrupt id the call's next
	// confirmation must carry.
	NextConfirmationInterruptID(ctx context.Context, executionID, callDigest string) (string, error)
	// ConsumeConfirmation consumes the claim's interrupt id for the call when
	// it is the call's next one, or accepts a repeat of the request that
	// consumed it (same call, same Idempotency-Key). Otherwise it answers the
	// next interrupt id, unaccepted.
	ConsumeConfirmation(ctx context.Context, claim ConfirmationClaim) (ConfirmationOutcome, error)
}
