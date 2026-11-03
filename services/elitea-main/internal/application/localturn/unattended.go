package localturn

// Unattended desktop turns (ADR-0045 decisions 1, 3 and 4, #1435, #1436).
//
// The desktop sends the grant on the start of the chain's first turn and on
// every continuation (the same grant_id, and chain_root_execution_id set to
// the first turn's execution id). Main checks the chain limits and adds the
// continuation in the start's transaction; a limit answers 409
// chain_limit_reached. The commit records each answered question in the
// approval ledger (path desktop) with its source, and the last commit of the
// chain carries the chain's outcome.

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// Sources of a HITL exchange (ADR-0045 decision 3). An absent source is a
// person's answer: a client from before this contract sends none.
const (
	SourcePerson  = "person"
	SourceGrant   = "grant"
	SourceBypass  = "bypass"
	SourceTimeout = "timeout"
)

// ErrConfirmationGrantRefused refuses a remote toolkit confirmation whose
// source is the grant when the turn's grant does not answer remote
// confirmations.
var ErrConfirmationGrantRefused = errors.New("the turn's grant does not answer remote toolkit confirmations")

// UnattendedSettingsReader reads the grant settings (approvals section).
type UnattendedSettingsReader interface {
	Get(ctx context.Context) (platformconfig.UnattendedSettings, error)
}

// WithUnattended lets Start admit unattended grants. Without it, a start that
// carries a grant is refused as disabled.
func (s *Service) WithUnattended(settings UnattendedSettingsReader) *Service {
	if s != nil {
		s.unattended = settings
	}
	return s
}

// StartGrant is the admitted grant a start stores: the chain's first turn
// (Root) or a continuation.
type StartGrant struct {
	Grant  unattended.Grant
	Limits unattended.Limits
	// ChainRootExecutionID is empty on the chain's first turn.
	ChainRootExecutionID string
	// Kind is a continuation's link kind (unattended.LinkContinue when the
	// host named none).
	Kind string
	// BriefState is the first turn's run brief state (ADR-0045 decision
	// 11); empty on a continuation.
	BriefState string
}

// UnattendedStarted is the start's answer about the chain.
type UnattendedStarted struct {
	GrantID              string
	ChainRootExecutionID string
	ContinuationIndex    int
	Limits               unattended.Limits
}

// admitGrant checks a start's grant. A desktop grant names no sensitive cloud
// tool, so the platform's category and tier rules have nothing to check.
// The limits of a continuation are the chain's stored ones: the store reads
// them, and admitGrant does not resolve the limits a continuation sends.
// The platform switch is read on every turn: with grants off, a
// continuation is refused as disabled, and a first turn whose grant only
// continues (OnlyContinues) starts with no grant (nil, nil).
func (s *Service) admitGrant(ctx context.Context, request unattended.Request) (*StartGrant, error) {
	if err := unattended.ValidateShape(request); err != nil {
		return nil, err
	}
	if len(request.SensitiveTools) > 0 {
		return nil, unattended.Refused("sensitive_tools", "", unattended.RefusedInvalid)
	}
	if s.unattended == nil {
		return disabledGrant(request)
	}
	settings, err := s.unattended.Get(ctx)
	if err != nil {
		s.logger.ErrorContext(ctx, "local turn: unattended settings unreadable", "err", err)
		return nil, ErrUnavailable
	}
	if !settings.Enabled {
		return disabledGrant(request)
	}
	if request.IsContinuation() {
		// A continuation keeps the chain's stored limits. The store reads
		// them; the limits a client sends again are ignored, so a maximum
		// that an administrator lowers mid-chain does not refuse the chain's
		// next turn.
		kind := request.Kind
		if kind == "" {
			kind = unattended.LinkContinue
		}
		return &StartGrant{
			Grant:                unattended.GrantOf(request, nil),
			ChainRootExecutionID: request.ChainRootExecutionID,
			Kind:                 kind,
		}, nil
	}
	// A desktop turn has no per-run spend cap to multiply: the default chain
	// spend is the platform maximum (none when it is 0).
	limits, err := unattended.ResolveLimits(settings, request.Chain, 0)
	if err != nil {
		return nil, err
	}
	return &StartGrant{
		Grant:                unattended.GrantOf(request, nil),
		Limits:               limits,
		ChainRootExecutionID: request.ChainRootExecutionID,
		BriefState:           unattended.BriefStateAtStart(request.GoalCheck, settings.RunBrief),
	}, nil
}

var exchangeAction = regexp.MustCompile(`^[a-z_]{1,32}$`)

// validExchangeSource admits the four sources and the absent one.
func validExchangeSource(source string) bool {
	switch source {
	case "", SourcePerson, SourceGrant, SourceBypass, SourceTimeout:
		return true
	}
	return false
}

// ExchangeLedgerOutcome maps a person's decision to a ledger outcome.
func ExchangeLedgerOutcome(decision string) string {
	switch strings.ToLower(decision) {
	case "approve", "approved", "allow", "allowed", "accept", "yes", "continue", "always":
		return "approved"
	case "reject", "rejected", "deny", "denied", "decline", "declined", "no", "stop", "block":
		return "rejected"
	case "edit", "edited":
		return "edited"
	case "answer", "answered":
		return "answered"
	}
	return "other"
}

// ExchangeLedgerAction is the decision as a ledger action, or "" when it
// does not have the action shape.
func ExchangeLedgerAction(decision string) string {
	if exchangeAction.MatchString(decision) {
		return decision
	}
	return ""
}

// disabledGrant is the answer to a grant while grants are off: no grant for
// a grant that only continues, a refusal for every other grant.
func disabledGrant(request unattended.Request) (*StartGrant, error) {
	if request.OnlyContinues() {
		return nil, nil
	}
	return nil, unattended.Refused("unattended", "", unattended.RefusedDisabled)
}
