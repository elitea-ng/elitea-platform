// Package accessevent fills the request facts of an admin.access.changed
// security audit event (ADR-0059 R1.5): the actor, the credential reference,
// the elevation and the trace. It is its own package because
// internal/securityaudit sits below internal/auth.
package accessevent

import (
	"context"
	"strconv"

	"go.opentelemetry.io/otel/trace"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// FromRequest completes change with the facts of the request in ctx: the
// actor (unless the caller named one), the credential reference, the
// elevation id and the reason code (each unless set) and the trace id. It
// never reads a credential value: the reference is the token or session ROW.
func FromRequest(ctx context.Context, change securityaudit.AdminAccessChanged) securityaudit.AdminAccessChanged {
	if principal, ok := auth.UserFromContext(ctx); ok {
		if change.ActorUserID == 0 {
			change.ActorUserID, _ = principal.OwningUserID()
		}
		change.AuthenticationRef = CredentialRef(principal)
	}
	if change.Change.ElevationID == "" {
		change.Change.ElevationID = audit.ElevationIDFromContext(ctx)
	}
	// The reason code of a dangerous action (ADR-0059 R3.8a), never its text.
	if change.Change.ReasonCode == "" {
		change.Change.ReasonCode = audit.ReasonCodeFromContext(ctx)
	}
	if spanContext := trace.SpanContextFromContext(ctx); spanContext.IsValid() {
		change.CorrelationID = spanContext.TraceID().String()
	}
	return change
}

// CredentialRef names the credential behind a change by its row:
// "token:<id>", "session:<ref>", or "" when there is no row.
func CredentialRef(principal auth.User) string {
	credential := principal.Credential()
	switch {
	case credential.TokenID != "":
		return "token:" + credential.TokenID
	case credential.BrowserSessionRef > 0:
		return "session:" + strconv.FormatInt(credential.BrowserSessionRef, 10)
	}
	return ""
}

// DefaultAppender is the PostgreSQL appender of source, or an appender that
// refuses every append when the source is invalid: a route never runs with no
// appender, so a change never happens without its event.
func DefaultAppender(source string) securityaudit.Appender {
	appender, err := securityaudit.NewPostgresAppender(source)
	if err != nil {
		return securityaudit.FailingAppender{Err: err}
	}
	return appender
}
