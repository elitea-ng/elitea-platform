package vectorclient

// An Inventory graph's entity vectors (#1359): the Inventory engine keeps each
// toolkit's graph rows in its own PostgreSQL store and the graph's vectors in
// elitea-vector, source INVENTORY, one namespace per graph. The engine writes
// and ranks them with the callback bearer of each invocation; a DELETED
// toolkit has no invocation, so the platform removes its namespace itself, as
// elitea-vector's administrator, after the engine deleted the rows (the toolkit
// cleanup journal, api.WithInventoryGraphVectors). A deleted PROJECT needs
// nothing here: DeleteProjectData's DropProject removes every source's points,
// Inventory's among them.

import (
	"context"
	"fmt"

	vectorv1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/vector/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhost/platformops"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// inventoryGraphNamespaceRoot is the UUIDv5 namespace of every Inventory graph's
// vector namespace: UUIDv5(NAMESPACE_URL,
// "https://elitea.ai/ns/inventory/graph-vectors"). It is the Inventory
// engine's store::vectors::NAMESPACE_ROOT
// (services/elitea-inventory-engine/src/store/vectors.rs); both sides pin the
// same ids in a test. Never change it: every stored vector is addressed under
// it.
var inventoryGraphNamespaceRoot = uuid.MustParse("634fdd4c-7b6b-5648-80b0-46de40560443")

// InventoryGraphNamespace is the elitea-vector namespace_id of the Inventory
// graph of toolkit toolkitID in project projectID: the canonical lower-case
// text of UUIDv5(inventoryGraphNamespaceRoot, "<projectID>/<toolkitID>").
func InventoryGraphNamespace(projectID, toolkitID int64) string {
	return uuid.NewSHA1(inventoryGraphNamespaceRoot, fmt.Appendf(nil, "%d/%d", projectID, toolkitID)).String()
}

// DeleteInventoryGraph removes every entity vector of one Inventory toolkit's
// graph: Delete of its whole namespace (source INVENTORY) in every collection.
// Idempotent: a namespace that holds nothing is a clean success. The errors
// are platformops's classes, like DeleteMemoryToolkit's.
func (c *Client) DeleteInventoryGraph(ctx context.Context, projectID, toolkitID int64) error {
	if c == nil {
		return fmt.Errorf("%w: no elitea-vector client", platformops.ErrConfig)
	}
	if projectID <= 0 || projectID > 1<<31-1 || toolkitID <= 0 {
		return fmt.Errorf("%w: project %d and toolkit %d must be positive", platformops.ErrConfig, projectID, toolkitID)
	}
	namespace := InventoryGraphNamespace(projectID, toolkitID)
	callCtx, cancel := context.WithTimeout(ctx, c.indexTimeout)
	defer cancel()
	response, err := c.vectors.Delete(callCtx, &vectorv1.DeleteRequest{
		ProjectId: projectID,
		Namespace: &vectorv1.Namespace{
			Source:      vectorv1.Source_SOURCE_INVENTORY,
			NamespaceId: namespace,
		},
		Selector: &vectorv1.DeleteRequest_WholeNamespace{WholeNamespace: &vectorv1.WholeNamespace{}},
	})
	if err != nil {
		if ctxErr := callerEnded(ctx); ctxErr != nil {
			return ctxErr
		}
		return c.classifyInventoryDelete(projectID, toolkitID, err)
	}
	c.logger.Info("inventory graph vectors deleted", "project_id", projectID, "toolkit_id", toolkitID,
		"namespace_id", namespace, "collections", response.GetCollections())
	return nil
}

// classifyInventoryDelete turns a failed Delete into one of platformops's
// errors, as classifyMemoryDelete does.
func (c *Client) classifyInventoryDelete(projectID, toolkitID int64, err error) error {
	code := status.Code(err)
	message := status.Convert(err).Message()
	switch code {
	case codes.FailedPrecondition, codes.Aborted, codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted:
		return fmt.Errorf("%w: elitea-vector delete of inventory toolkit %d (project %d): %s (%s)",
			platformops.ErrRetryLater, toolkitID, projectID, message, code)
	case codes.Unauthenticated, codes.PermissionDenied:
		c.logger.Error("elitea-vector refused this process's client certificate; its ELITEA_VECTOR_ADMIN_IDENTITIES must name this process's identity, and the deleted Inventory toolkit's vectors wait until it does",
			"project_id", projectID, "toolkit_id", toolkitID, "code", code.String(), "message", message)
		return fmt.Errorf("%w: elitea-vector delete of inventory toolkit %d (project %d): %s (%s). Its ELITEA_VECTOR_ADMIN_IDENTITIES must name this process's client certificate identity",
			platformops.ErrNotAllowed, toolkitID, projectID, message, code)
	case codes.InvalidArgument:
		return fmt.Errorf("%w: elitea-vector delete of inventory toolkit %d (project %d): %s (%s)",
			platformops.ErrConfig, toolkitID, projectID, message, code)
	}
	return fmt.Errorf("elitea-vector delete of inventory toolkit %d (project %d) failed: %s (%s)",
		toolkitID, projectID, message, code)
}
