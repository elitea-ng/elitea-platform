package api

// A deleted Inventory toolkit's entity vectors (#1359). The Inventory engine
// keeps a graph's rows in its own store and its vectors in elitea-vector
// (source INVENTORY, one namespace per graph). The toolkit cleanup journal
// asks the engine to delete the rows (platformops DeleteToolkitData); the
// engine has no bearer for elitea-vector on that call (the platform makes it,
// not an invocation), so the platform deletes the namespace itself, as
// elitea-vector's administrator, right after the engine answered: the engine's
// deletion tombstones the toolkit, so no later write can put vectors back.
//
// A deleted PROJECT needs nothing extra: the project journal's vector step
// (DropProject) removes every source's points, Inventory's among them.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/platformops"
)

// InventoryGraphVectors deletes one Inventory toolkit's graph vectors.
// *vectorclient.Client satisfies it; the errors are platformops's classes.
type InventoryGraphVectors interface {
	DeleteInventoryGraph(ctx context.Context, projectID, toolkitID int64) error
}

// HoldsInventory reports whether the Inventory engine is among clients: a
// replica that runs Inventory writes vectors into elitea-vector, so a project
// delete it decides must record the vector step.
func HoldsInventory(clients []ApplicationDataClient) bool {
	for _, client := range clients {
		if client != nil && client.App() == platformops.AppInventory {
			return true
		}
	}
	return false
}

// WithInventoryGraphVectors wraps the Inventory client of clients so that a
// toolkit deletion also deletes the toolkit's graph vectors through vectors.
// Every other client, and the Inventory client's project deletion, are
// unchanged.
//
// vectors must be a nil INTERFACE (never a typed nil) when the replica has no
// elitea-vector client. Then the toolkit deletion stays PENDING (the journal
// retries it, logged at Error naming the variable to set), because closing it
// would leave the vectors in Qdrant behind a "deleted" — unless abandoned, the
// operator's ELITEA_VECTOR_CLEANUP_ABANDON switch, under which the rows alone
// are deleted, as the project journal closes its vector step.
func WithInventoryGraphVectors(clients []ApplicationDataClient, vectors InventoryGraphVectors, abandoned bool, logger *slog.Logger) []ApplicationDataClient {
	if logger == nil {
		logger = slog.Default()
	}
	wrapped := make([]ApplicationDataClient, 0, len(clients))
	for _, client := range clients {
		if client != nil && client.App() == platformops.AppInventory && (vectors != nil || !abandoned) {
			client = inventoryWithVectors{ApplicationDataClient: client, vectors: vectors, logger: logger}
		}
		wrapped = append(wrapped, client)
	}
	return wrapped
}

type inventoryWithVectors struct {
	ApplicationDataClient
	vectors InventoryGraphVectors
	logger  *slog.Logger
}

// DeleteToolkitData deletes the graph's rows in the engine, then its vectors.
// A failure of either leaves the journal row open (the engine's deletion is
// idempotent: a retry answers "nothing to delete" and goes on to the vectors).
func (c inventoryWithVectors) DeleteToolkitData(ctx context.Context, projectID, toolkitID int64) (platformops.ToolkitDeletion, error) {
	deletion, err := c.ApplicationDataClient.DeleteToolkitData(ctx, projectID, toolkitID)
	if err != nil {
		return deletion, err
	}
	if c.vectors == nil {
		c.logger.Error("the deleted Inventory toolkit's graph vectors cannot be deleted: this replica has no elitea-vector client; the deletion stays pending until a replica with one runs it",
			"project_id", projectID, "toolkit_id", toolkitID, "set", "ELITEA_VECTOR_GRPC_TARGET")
		return platformops.ToolkitDeletion{}, fmt.Errorf("%w: no elitea-vector client to delete inventory toolkit %d's vectors", platformops.ErrRetryLater, toolkitID)
	}
	if err := c.vectors.DeleteInventoryGraph(ctx, projectID, toolkitID); err != nil {
		if errors.Is(err, platformops.ErrNotAllowed) {
			// A refused certificate is the operator's to fix, and closing the
			// row on it would leave the vectors behind a "failed permanently"
			// for ever: the row stays open, backed off (the client logged what
			// to fix), as the project journal keeps its vector step.
			return platformops.ToolkitDeletion{}, fmt.Errorf("%w: %v", platformops.ErrRetryLater, err)
		}
		return platformops.ToolkitDeletion{}, err
	}
	return deletion, nil
}
