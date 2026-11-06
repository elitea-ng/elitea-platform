package runtimecomposition

import (
	"log/slog"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/browserruntime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/egresstoken"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

// newBrowserContextResolver builds the runtime-context `browser` field
// (ADR-0049, #1596). It reuses the workspace runtime's egress proxy URL and
// run-token minter: the browser reaches the network only through that
// proxy. It returns a nil interface (never a typed nil) without a content
// pool.
func newBrowserContextResolver(dependencies Dependencies) (storage.BrowserContextResolver, error) {
	if dependencies.ContentPool == nil {
		return nil, nil
	}
	sources, err := repos.NewBrowserRuntimeSources(dependencies.ContentPool)
	if err != nil {
		return nil, err
	}
	egress := browserruntime.Egress{ProxyURL: dependencies.Workspace.Egress.ProxyURL}
	// egresstoken.Minter signs both token kinds; a minter that cannot sign
	// an open token is left out, so every browser run reads "no signing key".
	// A typed nil *egresstoken.Minter is no minter.
	if minter, ok := dependencies.Workspace.Egress.Minter.(browserruntime.Minter); ok && !isTypedNilMinter(minter) {
		egress.Minter = minter
	}
	logger := dependencies.Logger
	if logger == nil {
		logger = slog.Default()
	}
	resolver := browserruntime.NewResolver(sources, sources, sources, sources, egress, logger)
	if resolver == nil {
		return nil, nil
	}
	return resolver, nil
}

func isTypedNilMinter(minter browserruntime.Minter) bool {
	typed, ok := minter.(*egresstoken.Minter)
	return minter == nil || (ok && typed == nil)
}
