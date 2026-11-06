package storage

import (
	"context"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/browserruntime"
)

// runtimeContextStageBrowser labels a failed `browser` field resolution.
const runtimeContextStageBrowser = "browser_context"

// BrowserContextResolver builds the runtime-context `browser` field for a
// claimed execution (browserruntime.Resolver). It returns nil and no error
// when the project does not enable the cloud browser.
type BrowserContextResolver interface {
	ResolveBrowserContext(ctx context.Context, claim browserruntime.Claim) (*browserruntime.RuntimeContext, error)
}

// WithBrowserContext lets the service serve the `browser` field (ADR-0049,
// #1596). A nil resolver (or a typed nil) leaves it off.
func (s *EliteaClientTokenService) WithBrowserContext(resolver BrowserContextResolver) *EliteaClientTokenService {
	if s == nil {
		return s
	}
	if typed, ok := resolver.(*browserruntime.Resolver); ok && typed == nil {
		resolver = nil
	}
	s.browser = resolver
	return s
}

// resolveBrowser adds the `browser` field to value when the worker accepted
// it and the project enables the cloud browser. A resolution failure fails
// the whole redemption, as the workspace field does: a run that the
// project lets use the browser must not start without its caps.
func (s *EliteaClientTokenService) resolveBrowser(
	ctx context.Context,
	claim ContentClaim,
	authorization RuntimeContextAuthorization,
	accept RuntimeContextAccept,
	value *EliteaClientTokenContext,
) error {
	if s.browser == nil || !accept.Browser {
		return nil
	}
	browser, err := s.browser.ResolveBrowserContext(ctx, browserruntime.Claim{
		ExecutionID: claim.ExecutionID,
		ProjectID:   authorization.ResourceProjectID,
		Governed:    accept.GovernedRun,
	})
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		return runtimeContextUnavailable(runtimeContextStageBrowser)
	}
	value.Browser = browser
	return nil
}
