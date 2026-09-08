package run

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

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

	// Reader-selected wiki pages, resolved into the question BEFORE the
	// argument set is derived — see contextpaths.go for why it happens here
	// rather than inside a tool. The keys are spent here too, so nothing
	// downstream can prepend the same context twice.
	params, err := ApplyContextPaths(ctx, call.Tool, params, r.Artifacts)
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
