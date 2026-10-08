package executioninterrupt

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/noderecovery"
)

// Typed failures. Callers branch with errors.Is; none carries a value, display
// text, tool argument or token.
var (
	ErrInvalidCard     = errors.New("execution interrupt card is invalid")
	ErrInvalidDecision = errors.New("execution interrupt decision is invalid")
	ErrInvalidAck      = errors.New("execution interrupt ack is invalid")
	ErrInvalidFrontier = errors.New("execution interrupt frontier is invalid")
	ErrInvalidRaise    = errors.New("execution interrupt raise is invalid")
	ErrInvalidClose    = errors.New("execution interrupt close is invalid")
	// ErrNotAllowed hides whether a response exists from a caller who may not see it.
	ErrNotAllowed = errors.New("execution interrupt access is not allowed")
	// ErrNotFound means the authorized response has no card under the key.
	ErrNotFound = errors.New("execution interrupt not found")
	// ErrAlreadyResolved is every decision conflict other than an exact replay.
	ErrAlreadyResolved = errors.New("execution interrupt already resolved")
	// ErrRaiseConflict is a raise whose key, event id or interrupt id is taken
	// by different bytes.
	ErrRaiseConflict = errors.New("execution interrupt raise conflicts with a stored card")
	// ErrCapReached is a raise beyond MaxOpenInterrupts open cards.
	ErrCapReached = errors.New("execution interrupt open-card cap reached")
	// ErrAckConflict is an ACK that does not match the fetched decision or a
	// stored ACK.
	ErrAckConflict = errors.New("execution interrupt ack conflicts with the ledger")
	// ErrStaleFence is a private call whose claim is not the live one.
	ErrStaleFence = errors.New("execution interrupt claim fence is not current")
	// ErrLedgerFault is a stored row that breaks a ledger invariant.
	ErrLedgerFault = errors.New("execution interrupt ledger invariant broken")
)

type Kind string

const (
	KindToolGuard     Kind = "tool_guard"
	KindHITLNode      Kind = "hitl_node"
	KindAskUser       Kind = "ask_user"
	KindDelegatedAuth Kind = "delegated_auth"
)

type Action string

const (
	ActionApprove          Action = "approve"
	ActionReject           Action = "reject"
	ActionEdit             Action = "edit"
	ActionBlockWithComment Action = "block_with_comment"
	ActionAnswer           Action = "answer"
	ActionAuthorize        Action = "authorize"
	ActionSkip             Action = "skip"
	ActionContinue         Action = "continue"
)

func (a Action) Valid() bool {
	switch a {
	case ActionApprove, ActionReject, ActionEdit, ActionBlockWithComment, ActionAnswer, ActionAuthorize, ActionSkip, ActionContinue:
		return true
	}
	return false
}

// TakesValue reports whether the action carries a non-empty value. Reject and
// Skip carry none and are never reinterpreted as permission.
func (a Action) TakesValue() bool {
	switch a {
	case ActionEdit, ActionBlockWithComment, ActionAnswer, ActionContinue:
		return true
	}
	return false
}

type State string

const (
	StatePending    State = "PENDING"
	StateDecided    State = "DECIDED"
	StateConsumed   State = "CONSUMED"
	StateCancelled  State = "CANCELLED"
	StateSuperseded State = "SUPERSEDED"
)

// Open reports whether the card counts toward MaxOpenInterrupts.
func (s State) Open() bool { return s == StatePending || s == StateDecided }

// ValidDigest matches the contract's lowercase 64-hex, never all zeros
// (the node recovery id grammar).
func ValidDigest(value string) bool { return noderecovery.ValidID(value) }

// ValidExecutionID matches Main currentRuntimeID: 32 lowercase hex.
func ValidExecutionID(value string) bool { return noderecovery.ValidExecutionID(value) }

// ValidResponseMessageID matches a nonzero lowercase 36-character UUID.
func ValidResponseMessageID(value string) bool { return noderecovery.ValidResponseMessageID(value) }

func safeText(value string, maxBytes int) bool {
	return value != "" && len(value) <= maxBytes && utf8.ValidString(value) &&
		strings.IndexFunc(value, unicode.IsControl) < 0 && !strings.ContainsAny(value, "  ")
}

func safeToken(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && !strings.ContainsRune("_.:-", rune(c)) {
			return false
		}
	}
	return true
}
