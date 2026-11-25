package accessevent_test

import (
	"context"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit/accessevent"
)

// The reason code the reason check put on the request reaches the chained
// security event (ADR-0059 R3.8a); a code the caller set is kept.
func TestTheReasonCodeOfTheRequestReachesTheEvent(t *testing.T) {
	ctx := audit.ContextWithReasonCode(context.Background(), "offboarding")
	if got := accessevent.FromRequest(ctx, securityaudit.AdminAccessChanged{}).Change.ReasonCode; got != "offboarding" {
		t.Errorf("reason code = %q, want offboarding", got)
	}
	set := securityaudit.AdminAccessChanged{Change: securityaudit.AccessChange{ReasonCode: "incident"}}
	if got := accessevent.FromRequest(ctx, set).Change.ReasonCode; got != "incident" {
		t.Errorf("reason code = %q, want the caller's incident", got)
	}
	if got := accessevent.FromRequest(context.Background(), securityaudit.AdminAccessChanged{}).Change.ReasonCode; got != "" {
		t.Errorf("reason code = %q without one on the request", got)
	}
}
