package storage

import (
	"context"
	"log/slog"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/browserruntime"
)

// runtimeContextStageBrowser labels a `browser` field that could not be
// built, in the log line.
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
// it and the project enables the cloud browser. A browser failure never
// fails the redemption: the resolver degrades a source it cannot read to
// egress `unavailable` (the run starts, and the browser tools answer
// `browser_unavailable`), and an error it still answers (the project setting
// could not be read) serves no field, so the run has no browser at all. The
// log line names the execution and the error, never a token. A cancelled
// redemption still fails.
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
		slog.WarnContext(ctx, "browser context: the field could not be built; the run has no cloud browser",
			"stage", runtimeContextStageBrowser, "execution_id", claim.ExecutionID,
			"project_id", authorization.ResourceProjectID, "error", err.Error())
		return nil
	}
	value.Browser = browser
	return nil
}
