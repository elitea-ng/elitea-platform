package workerruncaps

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runcaps"
)

// The cap override (ADR-0041 decision 3): "Only a user with the
// safety.override_cap permission can continue the run past the cap, once,
// and the override is audited."
//
// A holder of safety.override_cap grants it on a response that ended with
// CAP_REACHED. The grant names that response's cap. The NEXT agent run of
// the same conversation (typically the "Continue" message the holder sends)
// consumes it when Main prepares its command and runs with that one cap
// doubled; every other cap keeps its value. A grant is single use, one per
// stopped response, one open per conversation, and expires unused after
// OverrideTTL. The grant is a security audit event
// (ai.elitea.security.v1.agent.cap.override) in the same transaction.

// OverridePermission is the permission the grant route requires.
const OverridePermission = "safety.override_cap"

// OverrideTTL is how long an unused grant waits for the next run.
const OverrideTTL = time.Hour

// The refusals of a grant.
var (
	// ErrOverrideInvalid is a malformed request.
	ErrOverrideInvalid = errors.New("run cap override: the request is invalid")
	// ErrOverrideNotCapped is a response that did not end with CAP_REACHED,
	// or one that is not in the conversation.
	ErrOverrideNotCapped = errors.New("run cap override: the response did not stop at a run cap")
	// ErrOverrideAlreadyGranted is a second grant on the same response.
	ErrOverrideAlreadyGranted = errors.New("run cap override: this response was already overridden")
	// ErrOverrideOpenGrant is a conversation that holds an unused grant.
	ErrOverrideOpenGrant = errors.New("run cap override: the conversation already holds an unused grant")
	// ErrOverrideNeedsTierCaps is a worker-side cap (tool calls, model turns,
	// records read) while no administrator saved a per-tier cap: Main does
	// not send caps to the workers yet, so it cannot raise one.
	ErrOverrideNeedsTierCaps = errors.New("run cap override: the per-tier run caps are not saved, " +
		"so this cap cannot be raised for one run")
)

// OverrideRequest asks to let a stopped response's conversation continue.
type OverrideRequest struct {
	ProjectID        int64
	ActorUserID      int64
	// ConversationUUID, when set, must be the response's conversation;
	// empty takes the response's own.
	ConversationUUID string
	MessageID        string
	// AllowWorkerCaps is set by the service: whether a worker-side cap may
	// be granted (the per-tier caps are saved).
	AllowWorkerCaps bool
	ExpiresIn       time.Duration
}

// OverrideGrant is a recorded grant.
type OverrideGrant struct {
	ID                int64
	Cap               string
	SourceExecutionID string
	GrantedAt         time.Time
	ExpiresAt         time.Time
}

// OverrideStore records a grant after checking its target, with its audit
// event, in one transaction (*repos.RunCapsRepository).
type OverrideStore interface {
	GrantRunCapOverride(ctx context.Context, request OverrideRequest) (OverrideGrant, error)
}

// OverrideService grants overrides.
type OverrideService struct {
	store    OverrideStore
	settings SettingsReader
}

// NewOverrideService builds the service.
func NewOverrideService(store OverrideStore, settings SettingsReader) (*OverrideService, error) {
	if store == nil || settings == nil {
		return nil, errors.New("run cap override: the store and settings are required")
	}
	return &OverrideService{store: store, settings: settings}, nil
}

var uuidShape = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Grant records a grant. The caller checked safety.override_cap.
func (s *OverrideService) Grant(ctx context.Context, request OverrideRequest) (OverrideGrant, error) {
	if request.ProjectID <= 0 || request.ActorUserID <= 0 ||
		(request.ConversationUUID != "" && !uuidShape.MatchString(request.ConversationUUID)) ||
		!uuidShape.MatchString(request.MessageID) {
		return OverrideGrant{}, ErrOverrideInvalid
	}
	settings, err := s.settings.Get(ctx)
	if err != nil {
		return OverrideGrant{}, fmt.Errorf("run cap override: read the platform run caps: %w", err)
	}
	request.AllowWorkerCaps = settings.TierCapsStored
	request.ExpiresIn = OverrideTTL
	return s.store.GrantRunCapOverride(ctx, request)
}

// WorkerCap reports whether the worker enforces cap (Main must send it).
func WorkerCap(cap string) bool {
	switch cap {
	case runcaps.MaxToolCalls, runcaps.MaxModelTurns, runcaps.MaxRecordsRead:
		return true
	default:
		return false
	}
}
