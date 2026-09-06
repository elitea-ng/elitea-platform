// Package moderation serves the app-request table, `centry.moderation_state`.
//
// The handlers live in requests.go, which also documents what these rows are,
// who creates them, what a decision on one actually causes, and why neither the
// requester nor the moderator may write the fields they are not writing.
package moderation

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	appmailer "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/mailer"
)

type Handler struct {
	pool *pgxpool.Pool
	// Outbound e-mail for decision notices (ADR-0024 WP7); nil means the
	// in-app notification row is the only delivery, as before.
	mailer DecisionMailer
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
