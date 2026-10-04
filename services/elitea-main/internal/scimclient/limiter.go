package scimclient

import (
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/failurelimit"
)

// FailureLimiter counts FAILED client authentications at the SCIM token
// endpoint. The type moved to internal/infra/failurelimit, which the native
// authorization endpoints (ADR-0025) share; this alias keeps every existing
// caller compiling unchanged.
type FailureLimiter = failurelimit.Limiter

// NewFailureLimiter allows max failures per key per window.
func NewFailureLimiter(max int, window time.Duration) *FailureLimiter {
	return failurelimit.New(max, window)
}
