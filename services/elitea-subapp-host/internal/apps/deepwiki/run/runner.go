package run

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/engine"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

// Tool is one engine tool: it takes the legacy keyword set (ArgumentsFor)
// and answers the engine's result dict — {"success": true, …} or
// {"success": false, "error": …}. It may use the invocation context for
// progress and stop checkpoints.
type Tool func(ctx context.Context, arguments map[string]any, tc *spi.Context) (map[string]any, error)

// Runner is the legacy handler's invoke path over a table of tools: the
// wikis_query rewrite, the parameter merge, the egress check, the call, the
// success check, composition, and the upload the pylon worker used to do
// and nothing on the Go platform did (the port gap the Python shell closed
// and this inherits).
type Runner struct {
	RunnerName string
	Tools      map[string]Tool
	Egress     spi.EgressPolicy
	Artifacts  ArtifactClientFactory
	Logger     *slog.Logger
	// VerifiedIdentity reports that this host verifies the facade's
	// identity signature (an identity secret is configured). It decides
	// where the index project comes from: see project.go.
	VerifiedIdentity bool

	// deleteProject is the engine's delete_project_wikis, reachable only
	// through DeleteProject (the platform service), never as a toolkit tool.
	// Nil for a runner with no engine.
	deleteProject Tool

	// StopWait is how long a deletion waits for the generations it stopped
	// to end (default DefaultStopWait).
	StopWait time.Duration

	manager *spi.Manager
	// engine is the sidecar client, kept for what the engine says it can do.
	engine *engine.Client
	// ToolsRefresh is how often the engine's tool list is re-read (default
	// engine.DefaultToolsRefresh).
	ToolsRefresh time.Duration
}

// DefaultStopWait is how long a deletion waits for the generations it asked
// to stop.
const DefaultStopWait = 30 * time.Second

// generateTool is the engine tool whose invocations a deletion stops.
const generateTool = "generate_wiki"

// Labels a running generation carries (spi.Context.SetLabel), so a deletion
// can find the ones that would publish into what it deletes.
const (
	labelProject = "project"
	labelWiki    = "wiki"
)

// AttachManager hands the runner the host's invocation registry (spi.Server
// does it once), which is how a deletion finds and stops running generations.
func (r *Runner) AttachManager(manager *spi.Manager) { r.manager = manager }

// Start runs the runner's background work until ctx ends: reading what the
// engine says it serves, now and then every ToolsRefresh.
func (r *Runner) Start(ctx context.Context) {
	if r.engine == nil {
		return
	}
	go r.engine.WatchTools(ctx, r.ToolsRefresh, r.logger())
}

var (
	_ spi.ManagerAware = (*Runner)(nil)
	_ spi.Starter      = (*Runner)(nil)
)

// StopGenerations stops the running generate_wiki invocations of the project
// in ctx — of one wiki, or (wikiID empty) of every wiki — and waits up to
// StopWait for them to end. It fails with spi.ErrGenerationRunning when one is
// still running; the caller deletes nothing then.
//
// A generation whose wiki the host could not name (its label is empty)
// matches EVERY wiki of its project: stopping one generation too many is the
// safe side of a deletion.
func (r *Runner) StopGenerations(ctx context.Context, wikiID string) error {
	if r.manager == nil {
		return nil
	}
	project, err := ProjectFromContext(ctx)
	if err != nil || project == "" {
		return err
	}
	wait := r.StopWait
	if wait <= 0 {
		wait = DefaultStopWait
	}
	remaining := r.manager.StopMatching(ctx, func(run spi.RunningInvocation) bool {
		if run.Tool != generateTool || run.Labels[labelProject] != project {
			return false
		}
		wiki := run.Labels[labelWiki]
		return wikiID == "" || wiki == "" || wiki == wikiID
	}, wait)
	if remaining > 0 {
		r.logger().Warn("generations are still running after the wait; the deletion is refused",
			"project_id", project, "wiki_id", wikiID, "running", remaining, "waited", wait.String())
		return spi.NewFailure(spi.KindRuntime, spi.ErrGenerationRunning)
	}
	return nil
}

// DeleteProject removes the search index of every wiki of one project in the
// engine. It is the platform service's operation (spi.PlatformOps), called
// for elitea-main's project deprovisioning after the service authorised the
// caller by its mTLS client certificate. It deletes the INDEX only; the
// project's artifacts are purged by the platform's own project deletion.
//
// The project's running generations are stopped first (and waited for, a
// bounded time): one that outlives the deletion publishes its wiki back. When
// one is still running the deletion is refused (spi.ErrGenerationRunning) and
// nothing is deleted.
//
// The project comes from the call and is stamped on the engine call exactly
// as an invocation's is; there is no user and no identity here.
func (r *Runner) DeleteProject(ctx context.Context, projectID int32) (*spi.ProjectDeletion, error) {
	if r.deleteProject == nil {
		return nil, spi.Failf(spi.KindRuntime, "this host has no search index engine, so there is no index to delete")
	}
	project, ok := validProject(strconv.FormatInt(int64(projectID), 10))
	if !ok {
		return nil, spi.Failf(spi.KindValue, "%d is not a project id", projectID)
	}
	ctx = withProject(ctx, projectResolution{id: project})
	if err := r.StopGenerations(ctx, ""); err != nil {
		return nil, err
	}
	stamped, err := StampProject(ctx, DeleteProjectWikisTool, map[string]any{})
	if err != nil {
		return nil, err
	}
	result, err := r.deleteProject(ctx, stamped, spi.PlatformContext("delete-project"))
	result, err = spi.EngineOutcome(DeleteProjectWikisTool, result, err, EngineError)
	if err != nil {
		// The engine's "being published" refusal (storage::StorageError::Busy,
		// code busy) is the retryable ErrBusy.
		return nil, spi.ClassifyBusy(err, spi.ErrBusy, legacyBusyText)
	}
	return projectDeletionOf(projectID, result), nil
}

// legacyBusyText recognises the publish-lock refusal of an engine from
// BEFORE the error code existed (release N-1 of elitea-deepwiki-engine), by
// its wording. Rollout fallback only: consulted when the error carries no
// code, and gone once every engine sends codes.
func legacyBusyText(text string) bool { return strings.Contains(text, "is being published") }

// projectDeletionOf reads the engine's delete_project_wikis result.
func projectDeletionOf(project int32, result map[string]any) *spi.ProjectDeletion {
	deletion := &spi.ProjectDeletion{
		ProjectID:          project,
		LiveBuilds:         spi.JSONInt(result["live_builds"]),
		StaleBuildsRemoved: spi.JSONInt(result["builds"]),
	}
	// A build whose heartbeat is live was left alone: the project is not
	// gone until it ends and the deletion is called again.
	deletion.Incomplete = deletion.LiveBuilds > 0
	perWiki, _ := result["per_wiki"].([]any)
	listed := map[string]bool{}
	for _, entry := range perWiki {
		wiki := object(entry)
		id := str(wiki["wiki_id"])
		listed[id] = true
		deletion.Wikis = append(deletion.Wikis, spi.WikiDeletion{
			WikiID: id, Nodes: spi.JSONInt(wiki["nodes"]), Edges: spi.JSONInt(wiki["edges"]),
			Embeddings: spi.JSONInt(wiki["embeddings"]), Statistics: spi.JSONInt(wiki["statistics"]),
		})
	}
	// An engine of an older release lists the wikis without counts.
	ids, _ := result["wikis"].([]any)
	for _, entry := range ids {
		if id := str(entry); id != "" && !listed[id] {
			deletion.Wikis = append(deletion.Wikis, spi.WikiDeletion{WikiID: id})
		}
	}
	return deletion
}

var _ spi.PlatformOps = (*Runner)(nil)
var _ spi.Labeler = (*Runner)(nil)

// Labels names the project and wiki a generation publishes into, for the
// server to put on the invocation AT REGISTRATION (spi.Manager.SubmitLabelled),
// so a deletion's StopGenerations finds it before the run has begun. Invoke
// sets the same labels again once it has resolved the project (harmless:
// the values are the same); a generation whose project cannot be resolved
// here carries none and is refused by Invoke anyway.
func (r *Runner) Labels(call spi.Invoke) map[string]string {
	if call.Tool != generateTool || call.Family.Name == "query" {
		return nil
	}
	params := MergeParameters(call.Request)
	project, err := TrustedProject(call.Identity, r.VerifiedIdentity, params)
	if err != nil || project == "" {
		return nil
	}
	config := ExtractRepoConfig(params)
	return map[string]string{
		labelProject: project,
		labelWiki:    GeneratedWikiID(config.Map(), config.BranchString()),
	}
}

// Name is the runner's name as /health reports it.
func (r *Runner) Name() string {
	if r.RunnerName == "" {
		return "tools"
	}
	return r.RunnerName
}

func (r *Runner) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

// Invoke runs one tool and returns its terminal body.
func (r *Runner) Invoke(ctx context.Context, call spi.Invoke, tc *spi.Context) (map[string]any, error) {
	tool, ok := r.Tools[call.Tool]
	if !ok {
		return nil, spi.Failf(spi.KindNotFound, "Unknown tool: %s", call.Tool)
	}
	request := call.Request
	if call.Family.Name == "query" {
		transformed, err := TransformQueryRequest(request)
		if err != nil {
			return nil, err
		}
		request = transformed
	}
	params := MergeParameters(request)

	// The project the engine's index is scoped to, from authenticated
	// context only (project.go). Resolved once here and carried in ctx, so
	// every engine call this invocation makes — resolve_and_ask's included
	// — is stamped with the same project, and none can name another.
	project, projectErr := TrustedProject(call.Identity, r.VerifiedIdentity, params)
	ctx = withProject(ctx, projectResolution{id: project, err: projectErr})
	if call.Tool == generateTool && project != "" {
		// Found by a deletion of this project or wiki, which stops it
		// before it deletes anything (StopGenerations).
		tc.SetLabel(labelProject, project)
		config := ExtractRepoConfig(params)
		tc.SetLabel(labelWiki, GeneratedWikiID(config.Map(), config.BranchString()))
	}

	// Reader-selected wiki pages, resolved into the question BEFORE the
	// argument set is derived — see contextpaths.go for why it happens here
	// rather than inside a tool. The keys are spent here too, so nothing
	// downstream can prepend the same context twice.
	params, err := ApplyContextPaths(ctx, call.Tool, params, r.Artifacts)
	if err != nil {
		return nil, err
	}

	// Reader-UPLOADED files (#873), resolved the same place and for the same
	// reason as the wiki-page selection above: before the argument set is
	// derived, so the key is spent here and nothing downstream can prepend it
	// twice. See extracontext.go for why it runs second (files are the
	// LEAST authoritative of the two attachment kinds, so their block sits
	// furthest from the question).
	params, err = ApplyExtraContext(call.Tool, params)
	if err != nil {
		return nil, err
	}

	if host, err := CheckEgress(r.Egress, params); err != nil {
		return nil, err
	} else if host != "" {
		r.logger().Info("clone destination permitted by the egress allowlist", "host", host)
	}

	if err := tc.Checkpoint(); err != nil {
		return nil, err
	}
	if err := tc.Thinking(ctx, "Starting "+call.Tool); err != nil {
		return nil, err
	}

	result, err := tool(ctx, ArgumentsFor(call.Tool, params), tc)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, spi.Failf(spi.KindRuntime, "%s returned nothing, expected a dict", call.Tool)
	}
	if !Truthy(result["success"]) {
		return nil, EngineError(result)
	}

	objects := ComposeResultObjects(call.Tool, result)
	if err := CheckWikiHasPages(call.Tool, objects, result); err != nil {
		return nil, err
	}
	objects, err = r.upload(ctx, objects, params, tc)
	if err != nil {
		return nil, err
	}
	return CompletedBody(tc.InvocationID(), objects), nil
}

// upload puts every artifact object into its bucket through the transport
// the request carried. No transport: nothing is uploaded, and that is
// logged rather than reported in band — the composed list is a frozen
// contract and the real path always carries the transport. A failed upload
// does not fail the invocation: the objects are still returned inline, so
// the caller loses nothing it had; what it must not get is a success that
// claims the bucket holds them — so the failure is appended as a ⚠️
// message and logged loudly.
func (r *Runner) upload(ctx context.Context, objects []Object, params Params, tc *spi.Context) ([]Object, error) {
	var pending []Object
	for _, obj := range objects {
		if obj.IsArtifact() && obj.NameString() != "" {
			pending = append(pending, obj)
		}
	}
	if len(pending) == 0 {
		return objects, nil
	}
	llmSettings := object(params["llm_settings"])
	if llmSettings == nil {
		llmSettings = map[string]any{}
	}
	var client ArtifactClient
	if r.Artifacts != nil {
		built, err := r.Artifacts(llmSettings)
		if err != nil {
			return nil, err
		}
		client = built
	}
	if client == nil {
		r.logger().Warn("artifact objects returned inline only: the request carried no artifact transport (llm_settings.api_base / api_key)",
			"objects", len(pending))
		return objects, nil
	}
	if err := tc.Thinking(ctx, fmt.Sprintf("Uploading %d wiki objects", len(pending))); err != nil {
		return nil, err
	}
	var failures []string
	for _, obj := range pending {
		if err := tc.Checkpoint(); err != nil {
			return nil, err
		}
		bucket := obj.ResultBucket
		if bucket == "" {
			bucket = DefaultBucket
		}
		name := obj.NameString()
		if err := client.Upload(ctx, bucket, name, []byte(obj.Data)); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, spi.ErrCancelled) {
				return nil, err
			}
			r.logger().Error("uploading an artifact object failed", "name", name, "bucket", bucket, "error", err)
			failures = append(failures, fmt.Sprintf("- %s: %v", name, err))
		}
	}
	if len(failures) > 0 {
		if err := tc.Thinking(ctx, fmt.Sprintf("Uploading FAILED for %d object(s)", len(failures))); err != nil {
			return nil, err
		}
		return append(objects, Message(fmt.Sprintf(
			"⚠️ The wiki was generated but %d of %d objects could not be uploaded to the artifact bucket, so they are not readable from the wiki browser:\n%s",
			len(failures), len(pending), strings.Join(failures, "\n")))), nil
	}
	if err := tc.Thinking(ctx, fmt.Sprintf("Uploaded %d wiki objects", len(pending))); err != nil {
		return nil, err
	}
	return objects, nil
}
