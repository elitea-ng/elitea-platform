// Package moderation serves the app-request table, `centry.moderation_state`.
//
// The handlers live in requests.go, which also documents what these rows are,
// who creates them, what a decision on one actually causes, and why neither the
// requester nor the moderator may write the fields they are not writing.
package moderation

import (
	"context"
	"reflect"

	"github.com/jackc/pgx/v5/pgxpool"

	appmailer "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/mailer"
)

type Handler struct {
	pool *pgxpool.Pool
	// Outbound e-mail for decision notices (ADR-0024 WP7); nil means the
	// in-app notification row is the only delivery, as before.
	mailer DecisionMailer
	// provisioner and personalProjects back the project-request half (#871,
	// project_requests.go). Both nil until wired: approving a Project
	// Request without a provisioner fails closed (502), and filing one
	// without an ensurer fails closed (503) — see that file's Option docs.
	provisioner      ProjectProvisioner
	personalProjects PersonalProjectEnsurer
	// events backs the moderation.request.decided producer (#876's second
	// half). nil leaves AdministrationRequestUpdate exactly as before — no
	// webhook fires on a decision.
	events EventEmitter
}

// EventEmitter is the seam to internal/events.Publisher, declared locally
// (like DecisionMailer above) so this package does not import
// internal/events and close a cycle. *events.Publisher satisfies this
// structurally.
type EventEmitter interface {
	Emit(ctx context.Context, projectID, eventType string, payload any)
}

// WithEvents wires the moderation.request.decided producer.
//
// Guarded by eventEmitterPresent (see conversations.Handler.WithEvents'
// identical comment in that package): a nil *events.Publisher boxed into
// this interface parameter is non-nil, and unguarded would panic
// AdministrationRequestUpdate's first Emit call.
func WithEvents(emitter EventEmitter) Option {
	return func(h *Handler) {
		if eventEmitterPresent(emitter) {
			h.events = emitter
		}
	}
}

func eventEmitterPresent(emitter EventEmitter) bool {
	if emitter == nil {
		return false
	}
	v := reflect.ValueOf(emitter)
	switch v.Kind() {
	case reflect.Ptr, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func, reflect.Interface:
		return !v.IsNil()
	default:
		return true
	}
}

// DecisionMailer is the seam to internal/application/mailer.
//
// Configured takes a context for the reason `eliteacore.InviteMailer` states:
// the mail configuration is a database read, not a boot-time fact (gap G7).
type DecisionMailer interface {
	Configured(ctx context.Context) bool
	SendModerationDecision(ctx context.Context, decision appmailer.ModerationDecision) error
}

// Option configures a Handler.
type Option func(*Handler)

// WithMailer supplies the composer decision notices are mailed through.
func WithMailer(m DecisionMailer) Option {
	return func(h *Handler) {
		if m != nil {
			h.mailer = m
		}
	}
}

func NewHandler(pool *pgxpool.Pool, options ...Option) *Handler {
	handler := &Handler{pool: pool}
	for _, option := range options {
		option(handler)
	}
	return handler
}
