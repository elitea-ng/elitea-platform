package run

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

const (
	inventoryFamily = "inventory"
	searchFamily    = "inventory_search"
)

// IngestTools are the tools that READ A SOURCE. Every one of them needs the
// facade to have expanded a source object; a body without one is refused here
// rather than in the engine, because the refusal is about the REQUEST and the
// engine should never be started for a call that cannot succeed.
//
// `delta_update` is NOT here, and not because it reads no source — it is in
// DeferredTools instead. See below.
var IngestTools = map[string]bool{
	"run_ingestion": true,
}

// DeferredTools are the advertised tools with nothing behind them. Their
// handlers exist in the copied tool layer — that is precisely why they are
// refused by NAME here: a handler one line of dispatch away from being served,
// with no test behind it and no run on any platform, is not a tool.
//
// Two kinds, and the reason text says which:
//
//   - five the legacy descriptor advertised and the legacy ROUTER never
//     carried, so no request could ever reach them;
//
//   - `delta_update`, which the router DID carry and whose body is a stub:
//
//     # TODO: Implement delta update using EliteAClient
//     return f"Delta update from toolkit {toolkit_id} - Not yet implemented.", []
//
//     On the legacy platform that returned status Completed with the words
//     "Not yet implemented" as the tool's successful result. This port refuses
//     empty successes everywhere else — the unavailable runner, the wired-with-
//     no-socket host, an ingestion that produced no graph — and there is no
//     reason for the rule to stop at a tool whose own author wrote TODO. An
//     agent that reads "Completed" cannot tell the difference; an agent that
//     reads a refusal can.
//
// `query_graph` is deferred only on the `inventory` family; the search family
// routed it and serves it. The map is keyed by family for that reason.
var DeferredTools = map[string]map[string]string{
	inventoryFamily: {
		"query_graph":            "the inventory toolkit never routed it; the inventory_search toolkit serves it",
		"get_type_stats":         "it was declared in the legacy descriptor and never routed",
		"link_toolkits_to_tools": "it was declared in the legacy descriptor and never routed",
		"connect_orphan_nodes":   "it was declared in the legacy descriptor and never routed",
		"validate_relationships": "it was declared in the legacy descriptor and never routed",
		"delta_update":           "its legacy handler is a stub that answered 'Not yet implemented' as a SUCCESS, so it has never updated a graph",
	},
}

// Tool is one engine tool: it takes the arguments the host derived and answers
// the engine's result dict — {"success": true, …} or {"success": false,
// "error": …}. It may use the invocation context for progress and stop
// checkpoints.
type Tool func(ctx context.Context, arguments map[string]any, tc *spi.Context) (map[string]any, error)

// Runner is the legacy handler's invoke path over a table of tools: the
// parameter merge, the deferred refusal, the source check, the call, the
// success check, composition, and the artifact upload the engine used to do
// itself with an admin token.
type Runner struct {
	RunnerName string
	Tools      map[string]Tool
	Artifacts  ArtifactClientFactory
	Logger     *slog.Logger
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
	family := call.Family.Name
	if reason, deferred := DeferredTools[family][call.Tool]; deferred {
		// resource_not_found, not invalid_input: the caller asked for
		// something the descriptor advertises, and there is nothing behind it.
		// An invalid_input would tell them to fix their request, and there is
		// no request that makes this tool run.
		return nil, spi.Failf(spi.KindNotFound,
			"'%s' is not available on the %s toolkit: %s, so no implementation of it has ever run on this platform",
			call.Tool, family, reason)
	}

	tool, ok := r.Tools[call.Tool]
	if !ok {
		return nil, spi.Failf(spi.KindNotFound, "Unknown tool: %s", call.Tool)
	}

	params := MergeParameters(call.Request)
	if err := CheckSource(call.Tool, params); err != nil {
		return nil, err
	}
	identity := ExtractIdentity(family, call.Request, params)

	if err := tc.Checkpoint(); err != nil {
		return nil, err
	}
	if err := tc.Thinking(ctx, "Starting "+call.Tool); err != nil {
		return nil, err
	}

	arguments := ArgumentsFor(family, call.Tool, params, identity)
	// The verified caller (ADR-0028 D3): the engine filters what a caller may
	// read by document ACL. Only from the signed identity the gate verified —
	// never from the request body, which the caller writes.
	if call.Identity.UserID != "" {
		arguments["caller_user_id"] = call.Identity.UserID
	}
	result, err := tool(ctx, arguments, tc)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, spi.Failf(spi.KindRuntime, "%s returned nothing, expected a dict", call.Tool)
	}
	if !Truthy(result["success"]) {
		return nil, EngineError(result)
	}

	objects := ComposeResultObjects(result, ResolveBucket(params))
	objects, err = r.upload(ctx, objects, params, tc)
	if err != nil {
		return nil, err
	}
	return CompletedBody(tc.InvocationID(), objects), nil
}

// CheckSource refuses an ingest call whose body carries no expanded source.
//
// The legacy handler took a bare `toolkit_id` and resolved it ITSELF, with an
// admin platform token, against an id the caller supplied and nothing checked
// the caller could see. Under ADR-0022 decision 6 that expansion is the
// facade's, so an unexpanded reference is refused rather than resolved — the
// same refusal DeepWiki's TransformQueryRequest makes for `wikis_toolkit`.
func CheckSource(tool string, params Params) error {
	if !IngestTools[tool] {
		return nil
	}
	source := params["source"]
	if !Truthy(source) {
		names := make([]string, 0, len(IngestTools))
		for name := range IngestTools {
			names = append(names, name)
		}
		sort.Strings(names)
		return spi.Failf(spi.KindValue,
			"%s needs an expanded source: this service does not resolve toolkit references, the facade does. "+
				"Run the tool through the platform rather than calling the provider directly (%s).",
			tool, strings.Join(names, ", "))
	}
	if object(source) == nil {
		return spi.Failf(spi.KindValue,
			"%s: source must arrive expanded as an object; a bare toolkit id is not resolved here", tool)
	}
	return nil
}

// upload puts every artifact object into its bucket through the transport the
// request carried. No transport: nothing is uploaded, and that is logged rather
// than reported in band. A failed upload does not fail the invocation — the
// objects are still returned inline, so the caller loses nothing it had; what
// it must not get is a success that claims the bucket holds them, so the
// failure is appended as a ⚠️ message and logged loudly.
//
// This is DeepWiki's upload, with one thing that is Inventory's: a failed
// graph upload is not cosmetic. DeepWiki's pages are also returned inline and a
// user can read them; a graph that did not land is a graph the NEXT query tool
// will not find, so the message says which objects those were.
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
	if err := tc.Thinking(ctx, fmt.Sprintf("Uploading %d graph objects", len(pending))); err != nil {
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
			"⚠️ The run completed but %d of %d objects could not be stored in the artifact bucket, "+
				"so the next query will not see them:\n%s",
			len(failures), len(pending), strings.Join(failures, "\n")))), nil
	}
	if err := tc.Thinking(ctx, fmt.Sprintf("Uploaded %d graph objects", len(pending))); err != nil {
		return nil, err
	}
	return objects, nil
}
