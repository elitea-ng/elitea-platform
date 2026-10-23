package api

// The application engines' platform services in the composition (#1243, #1244):
// the one list of clients RouterConfig carries, and the two journals it is
// wired into — the project provisioner's cleanup journal (one step per
// application, recorded on the row when the project is deleted) and the
// toolkit cleanup journal (one row per Inventory toolkit deleted).

import (
	"context"
	"log/slog"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/toolkitcleanup"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhost/platformops"
)

// ApplicationDataClient is one application engine's platform service as the
// composition sees it. *platformops.Client satisfies it.
type ApplicationDataClient interface {
	// App is the application's name: platformops.AppDeepWiki or AppInventory.
	App() string
	projectprovisioning.ApplicationDataRemover
	// DeleteToolkitData removes what the engine holds for one toolkit.
	// platformops.ErrNotSupported is the answer of an application that keeps
	// nothing per toolkit; ErrRetryLater one that holds it for now.
	DeleteToolkitData(ctx context.Context, projectID, toolkitID int64) (platformops.ToolkitDeletion, error)
}

// applicationDataOptions are the provisioner options that give the cleanup
// journal one step per wired application.
func applicationDataOptions(clients []ApplicationDataClient) []projectprovisioning.Option {
	options := make([]projectprovisioning.Option, 0, len(clients))
	for _, client := range clients {
		if client == nil {
			continue
		}
		options = append(options, projectprovisioning.WithApplicationData(client.App(), client))
	}
	return options
}

// toolkitCleanupOptions are the journal options that give the toolkit cleanup
// journal one remover per wired application.
func toolkitCleanupOptions(clients []ApplicationDataClient) []toolkitcleanup.Option {
	options := make([]toolkitcleanup.Option, 0, len(clients))
	for _, client := range clients {
		if client == nil {
			continue
		}
		options = append(options, toolkitcleanup.WithRemover(client.App(), client))
	}
	return options
}

// MemoryData is what elitea-main asks of the vector store for the memory
// toolkit: delete a toolkit's points, delete a user's personal memories.
// *vectorclient.Client satisfies it. cfg.MemoryData is a nil INTERFACE when the
// deployment has no client (never a typed nil).
type MemoryData = toolkitcleanup.MemoryRemover

// newToolkitCleanupJournal builds the toolkit cleanup journal over the shared
// pool and the wired engines. The toolkit handler enqueues into it; the
// process's reconciler (RouterConfig.OnToolkitCleanupJournal) drains it.
func newToolkitCleanupJournal(cfg RouterConfig) *toolkitcleanup.Journal {
	options := toolkitCleanupOptions(cfg.ApplicationData)
	// A memory toolkit's memories are vectors (worker unit M9): the journal
	// pays them with the elitea-vector client, and records the obligation only
	// where memories live there (the Rust indexing runtime).
	options = append(options, toolkitcleanup.WithMemory(cfg.MemoryData, cfg.MemoryDataRecorded))
	return toolkitcleanup.New(cfg.Pool, slog.Default(), options...)
}
