package run

// The platform operations of Inventory (issue #1244, ADR-0031 phase C0): the
// platform deleting what a deleted Inventory toolkit or project left in the
// engine's graph store.
//
//	DeleteToolkit  one toolkit's graph (a toolkit was deleted)
//	DeleteProject  every graph of the project (the project is being deleted)
//
// They are the gRPC service elitea.subapp.v1.PlatformOperations (spi/platform.go),
// served on a listener of its own and authorised by the verified mTLS client
// certificate of elitea-main alone. They are NOT toolkit tools: the engine's
// delete tools are not in the admission table or the descriptor, the runner's
// Tools map does not hold them, and the facade offers a user or an agent no
// way to name them. Nothing an identity signature, a request body or a
// session says reaches them.
//
// Before either deletes a graph the host stops the project's (or toolkit's)
// running run_ingestion invocations and waits a bounded time for them to end.
// An ingest still running after the wait refuses the whole deletion
// (spi.ErrIngestRunning, FAILED_PRECONDITION) and nothing is deleted: that
// code always means nothing was deleted. The engine's own ingestion lease is
// the final guard: an ingest that starts between the stop and the delete (or
// runs on another replica) holds the graph. For one toolkit the engine
// refuses, which is the same retryable answer. For a project the engine
// deletes every graph it can and reports the held toolkits; that is answered
// OK with Incomplete true and BusyToolkits, never as a failure that would
// hide what was deleted, and the platform calls again.
//
// The engine's own tombstone (migration 0007) is what makes the deletion
// final: an ingest that starts after it — registered as the deletion began,
// or on another replica — is refused by the engine under the ingestion
// lease (code toolkit_deleted) and writes nothing.

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

const (
	// DeleteGraphTool and DeleteProjectGraphsTool are the engine tools the
	// platform operations call. They are engine-only: see the file comment.
	DeleteGraphTool         = "delete_graph"
	DeleteProjectGraphsTool = "delete_project_graphs"

	// platformFamily is the family the engine dispatches these two under.
	// It is a wire detail between this host and its engine socket, not a
	// toolkit: inventory.Toolkits has no such family.
	platformFamily = "platform"

	ingestTool = "run_ingestion"

	// DefaultStopWait is how long a deletion waits for the ingests it asked
	// to stop.
	DefaultStopWait = 30 * time.Second
)

// Labels a running ingest carries (set at registration, Labels), so a
// deletion can find the ones that would write into what it deletes.
const (
	labelProject = "project"
	labelToolkit = "toolkit"
)

// idLabel is an id as a label: the same text for 70, 70.0 (JSON decoded) and
// "70". Empty when the value names no id.
func idLabel(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatInt(int64(v), 10)
	case int:
		return strconv.Itoa(v)
	case int32:
		return strconv.FormatInt(int64(v), 10)
	case int64:
		return strconv.FormatInt(v, 10)
	case json.Number:
		return v.String()
	}
	return ""
}

// AttachManager hands the runner the host's invocation registry (spi.Server
// does it once), which is how a deletion finds and stops running ingests.
func (r *Runner) AttachManager(manager *spi.Manager) { r.manager = manager }

var (
	_ spi.ManagerAware = (*Runner)(nil)
	_ spi.Labeler      = (*Runner)(nil)
	_ spi.PlatformOps  = (*Runner)(nil)
	_ spi.ToolkitOps   = (*Runner)(nil)
)

// Labels names the project and toolkit an ingest writes, for the server to
// put on the invocation AT REGISTRATION (spi.Manager.SubmitLabelled): they
// are then on it, under the manager's lock, before it is in flight, so a
// deletion's StopIngests always sees them. Set from inside the run they
// would be invisible until the run reached that line, and an ingest
// registered while the deletion was stopping its siblings would run on and
// write the graph back. Any other tool carries no labels.
//
// The project is the one the gate VERIFIED when the identity carries one
// (what Invoke uses), else the body's — the same choice Invoke makes.
func (r *Runner) Labels(call spi.Invoke) map[string]string {
	if call.Tool != ingestTool {
		return nil
	}
	identity := ExtractIdentity(call.Family.Name, call.Request, MergeParameters(call.Request))
	if call.Identity.ProjectID != "" {
		identity.ProjectID = call.Identity.ProjectID
	}
	return map[string]string{
		labelProject: idLabel(identity.ProjectID),
		labelToolkit: idLabel(identity.ApplicationID),
	}
}

// StopIngests stops the running run_ingestion invocations of the project - of
// one toolkit, or (toolkit empty) of every toolkit - and waits up to StopWait
// for them to end. It fails with spi.ErrIngestRunning when one is still
// running; the caller deletes nothing then.
//
// An ingest whose toolkit the host could not name (its label is empty)
// matches EVERY toolkit of its project: stopping one ingest too many is the
// safe side of a deletion.
func (r *Runner) StopIngests(ctx context.Context, project, toolkit string) error {
	if r.manager == nil {
		return nil
	}
	wait := r.StopWait
	if wait <= 0 {
		wait = DefaultStopWait
	}
	remaining := r.manager.StopMatching(ctx, func(running spi.RunningInvocation) bool {
		if running.Tool != ingestTool || running.Labels[labelProject] != project {
			return false
		}
		ingesting := running.Labels[labelToolkit]
		return toolkit == "" || ingesting == "" || ingesting == toolkit
	}, wait)
	if remaining > 0 {
		r.logger().Warn("ingests are still running after the wait; the deletion is refused",
			"project_id", project, "toolkit_id", toolkit, "running", remaining, "waited", wait.String())
		return spi.NewFailure(spi.KindRuntime, spi.ErrIngestRunning)
	}
	return nil
}

// DeleteToolkit deletes the graph of one toolkit of one project. It is the
// platform service's operation (spi.ToolkitOps), called when an Inventory
// toolkit is deleted, after the service authorised the caller by its mTLS
// client certificate. A toolkit that had nothing at all (no graph, no source
// state) answers Deleted = false.
func (r *Runner) DeleteToolkit(ctx context.Context, projectID, toolkitID int32) (*spi.ToolkitDeletion, error) {
	if r.DeleteGraph == nil {
		return nil, spi.Failf(spi.KindRuntime, "this host has no Inventory engine, so there is no graph to delete")
	}
	if projectID <= 0 || toolkitID <= 0 {
		return nil, spi.Failf(spi.KindValue, "project %d, toolkit %d: both must be positive ids", projectID, toolkitID)
	}
	project, toolkit := strconv.Itoa(int(projectID)), strconv.Itoa(int(toolkitID))
	if err := r.StopIngests(ctx, project, toolkit); err != nil {
		return nil, err
	}
	document, err := r.callPlatformTool(ctx, r.DeleteGraph, DeleteGraphTool, map[string]any{
		"project_id": projectID, "application_id": toolkitID,
	})
	if err != nil {
		return nil, err
	}
	return &spi.ToolkitDeletion{
		ProjectID: projectID, ToolkitID: toolkitID,
		Deleted: Truthy(document["deleted"]),
		Removed: graphRemoved(document),
	}, nil
}

// DeleteProject deletes every graph of one project. It is the platform
// service's operation (spi.PlatformOps), called for elitea-main's project
// deprovisioning. It deletes the graphs only; the project's artifacts are
// purged by the platform's own project deletion.
//
// It fails (FAILED_PRECONDITION) only when the stop-ingests wait failed, so
// nothing was deleted. A toolkit the engine found held is reported in
// BusyToolkits with Incomplete true; what was deleted is reported too.
func (r *Runner) DeleteProject(ctx context.Context, projectID int32) (*spi.ProjectDeletion, error) {
	if r.DeleteProjectGraphs == nil {
		return nil, spi.Failf(spi.KindRuntime, "this host has no Inventory engine, so there is no graph to delete")
	}
	if projectID <= 0 {
		return nil, spi.Failf(spi.KindValue, "%d is not a project id", projectID)
	}
	if err := r.StopIngests(ctx, strconv.Itoa(int(projectID)), ""); err != nil {
		return nil, err
	}
	document, err := r.callPlatformTool(ctx, r.DeleteProjectGraphs, DeleteProjectGraphsTool, map[string]any{
		"project_id": projectID,
	})
	if err != nil {
		return nil, err
	}
	deletion := &spi.ProjectDeletion{
		ProjectID:     projectID,
		Graphs:        graphRemoved(document),
		GraphToolkits: spi.JSONIDs(document["toolkits"]),
		BusyToolkits:  spi.JSONIDs(document["busy_toolkits"]),
	}
	perToolkit, _ := document["by_toolkit"].([]any)
	for _, entry := range perToolkit {
		counts, _ := entry.(map[string]any)
		if id, ok := spi.JSONID(counts["toolkit_id"]); ok {
			deletion.ToolkitGraphs = append(deletion.ToolkitGraphs, spi.ToolkitGraphDeletion{
				ToolkitID: id, Removed: graphRemoved(counts),
			})
		}
	}
	// The engine says whether it is complete; an engine that does not is
	// complete when it named no busy toolkit.
	if complete, ok := document["complete"]; ok {
		deletion.Incomplete = !Truthy(complete)
	} else {
		deletion.Incomplete = len(deletion.BusyToolkits) > 0
	}
	return deletion, nil
}

// callPlatformTool runs an engine delete tool with the project (and toolkit)
// from the call - there is no user and no identity here - and returns the
// engine's JSON document. The engine's "an ingestion is running" refusal
// (code busy) becomes spi.ErrIngestRunning, which the transport maps to a
// retryable code.
func (r *Runner) callPlatformTool(ctx context.Context, tool Tool, name string, ids map[string]any) (map[string]any, error) {
	arguments := map[string]any{
		"family": platformFamily,
		"tool":   name,
		"params": map[string]any{"output_format": "json"},
	}
	for key, value := range ids {
		arguments[key] = value
	}
	result, err := tool(ctx, arguments, spi.PlatformContext(name))
	result, err = spi.EngineOutcome(name, result, err, EngineError)
	if err != nil {
		return nil, spi.ClassifyBusy(err, spi.ErrIngestRunning, legacyBusyText)
	}
	var document map[string]any
	if err := json.Unmarshal([]byte(str(result["result"])), &document); err != nil {
		return nil, spi.Failf(spi.KindRuntime, "%s answered a result that is not the JSON document: %v", name, err)
	}
	return document, nil
}

// legacyBusyText recognises the lease refusal of an engine from BEFORE the
// error code existed (release N-1 of elitea-inventory-engine), by its
// wording ("an ingestion ... running"). Rollout fallback only: it is
// consulted when the error carries no code, and goes once every engine is
// on a release that sends codes.
func legacyBusyText(text string) bool {
	return strings.Contains(text, "an ingestion") && strings.Contains(text, "running")
}

// graphRemoved reads the counts of the engine's deletion document.
func graphRemoved(document map[string]any) spi.GraphDeletion {
	return spi.GraphDeletion{
		Entities: spi.JSONInt(document["entities"]), Relations: spi.JSONInt(document["relations"]),
		Sources: spi.JSONInt(document["sources"]), Documents: spi.JSONInt(document["documents"]),
	}
}
