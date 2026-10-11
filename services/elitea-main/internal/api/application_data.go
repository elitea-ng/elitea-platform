package api

// The application engines' platform services in the composition (#1243, #1244):
// the one list of clients RouterConfig carries, and the two places it is wired
// into — the project provisioner's cleanup journal (one step per application)
// and the toolkit handler's delete (the engine before the row).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkits"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/platformops"
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

// applicationToolkitData is the toolkit handler's remover: it finds the engine
// a toolkit's type belongs to and asks it to delete the toolkit's data.
type applicationToolkitData struct {
	clients map[string]ApplicationDataClient
	logger  *slog.Logger
}

func newApplicationToolkitData(clients []ApplicationDataClient, logger *slog.Logger) toolkits.ToolkitDataRemover {
	if logger == nil {
		logger = slog.Default()
	}
	byApp := make(map[string]ApplicationDataClient, len(clients))
	for _, client := range clients {
		if client != nil {
			byApp[client.App()] = client
		}
	}
	return &applicationToolkitData{clients: byApp, logger: logger}
}

// DeleteToolkitData implements toolkits.ToolkitDataRemover.
//
// A type no application covers is nil: the toolkit has no engine data. An
// application that is NOT wired on this process is nil too, logged at Warn:
// the row delete must not be refused for an engine the deployment did not
// configure, and the log is where an operator learns the graph stayed. The
// engine's "nothing per toolkit" (DeepWiki) is nil; its "held by a run" is
// ErrToolkitDataBusy; anything else is the failure it was.
func (a *applicationToolkitData) DeleteToolkitData(ctx context.Context, projectID, toolkitID int64, toolkitType string) error {
	app := platformops.AppForToolkitType(toolkitType)
	if app == "" {
		return nil
	}
	client, ok := a.clients[app]
	if !ok {
		a.logger.WarnContext(ctx, "toolkit delete: the application's engine is not configured on this process, so its data for the toolkit is not deleted",
			"app", app, "project_id", projectID, "toolkit_id", toolkitID, "toolkit_type", toolkitType)
		return nil
	}
	deletion, err := client.DeleteToolkitData(ctx, projectID, toolkitID)
	switch {
	case err == nil:
		a.logger.InfoContext(ctx, "toolkit delete: application data removed",
			"app", app, "project_id", projectID, "toolkit_id", toolkitID, "deleted", deletion.Deleted)
		return nil
	case errors.Is(err, platformops.ErrNotSupported):
		return nil
	case errors.Is(err, platformops.ErrRetryLater):
		return fmt.Errorf("%w: %w", toolkits.ErrToolkitDataBusy, err)
	}
	return err
}
