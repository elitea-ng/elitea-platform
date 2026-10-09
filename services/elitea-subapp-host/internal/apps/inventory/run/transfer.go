package run

// Graph transfer (descriptor revision legacy-v2): `import_graph` and
// `export_graph`, the native engine's `import-graph` / `export-graph`
// operator commands as tools.
//
// export_graph needs nothing from the host: the engine returns graph.json as
// an artifact, and the ordinary composition and upload put it in the
// toolkit's bucket as a knowledge_graph object.
//
// import_graph needs the DOCUMENT, and the document is in the toolkit's
// artifact bucket: the Python engine kept each toolkit's graph there as
// graph.json. The host reads it, not the engine, because the host holds the
// artifact transport (the callback bearer the facade minted for this
// invocation). The engine receives the text in GraphDocumentParam.
//
// The document never comes from the caller. The SPI caps a request body at
// 4 MiB, far below a real graph; and a caller-written document would bypass
// the bucket's own access check. The host therefore OVERWRITES
// GraphDocumentParam on every import_graph call, whatever the body held.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/artifacts"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

const (
	// ImportTool reads the bucket's graph document into the graph store.
	ImportTool = "import_graph"
	// ExportTool writes the stored graph to the bucket as graph.json.
	ExportTool = "export_graph"
	// GraphDocumentParam is the merged parameter the host writes the
	// downloaded document into. Host-owned: a caller's value is replaced.
	GraphDocumentParam = "graph_document"
	// DefaultGraphArtifact is the key the Python engine wrote its graph to.
	DefaultGraphArtifact = "graph.json"
	// MaxGraphImportBytes is the largest document import_graph reads. It is
	// the artifact read bound; the engine socket and the engine's own body
	// limit are both above it. A larger graph goes through the engine's
	// `import-graph` command.
	MaxGraphImportBytes = artifacts.MaxDownloadBytes
)

// graphReader is the half of artifacts.Store an import needs.
type graphReader interface {
	Download(ctx context.Context, bucket, key string) ([]byte, error)
}

// GraphArtifactName is the bucket key import_graph reads: `artifact_name`,
// default graph.json. A key is a path inside the toolkit's bucket, so an
// absolute path or a `..` segment is refused rather than escaped.
//
// The refusal texts avoid the words "artifact" and "download": the frozen
// classifier (spi.Classify) reads them as artifact_error, and these are the
// caller's input faults.
func GraphArtifactName(params Params) (string, error) {
	name := strings.TrimSpace(str(params["artifact_name"]))
	if name == "" {
		return DefaultGraphArtifact, nil
	}
	if len(name) > 512 || strings.HasPrefix(name, "/") || strings.ContainsRune(name, 0) {
		return "", spi.Failf(spi.KindValue, "%q is not a key inside the toolkit's bucket", name)
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", spi.Failf(spi.KindValue, "%q is not a key inside the toolkit's bucket", name)
		}
	}
	return name, nil
}

// ResolveGraphDocument downloads the graph document an import_graph call
// names, from the toolkit's bucket, over the invocation's artifact transport.
func (r *Runner) ResolveGraphDocument(ctx context.Context, params Params, tc *spi.Context) (string, error) {
	name, err := GraphArtifactName(params)
	if err != nil {
		return "", err
	}
	bucket := ResolveBucket(params)
	llmSettings := object(params["llm_settings"])
	if llmSettings == nil {
		llmSettings = map[string]any{}
	}
	var client ArtifactClient
	if r.Artifacts != nil {
		if client, err = r.Artifacts(llmSettings); err != nil {
			r.logger().Error("building the bucket transport failed", "error", err)
			return "", spi.Failf(spi.KindRuntime, "%s: the bucket transport cannot be built", ImportTool)
		}
	}
	reader, ok := client.(graphReader)
	if client == nil || !ok {
		return "", spi.Failf(spi.KindValue,
			"%s reads %s from this toolkit's bucket, and the call carries no bucket transport. "+
				"Run the tool through the platform rather than calling the provider directly.",
			ImportTool, name)
	}
	if err := tc.Thinking(ctx, fmt.Sprintf("Reading %s from bucket %s", name, bucket)); err != nil {
		return "", err
	}
	data, err := reader.Download(ctx, bucket, name)
	switch {
	case errors.Is(err, context.Canceled) || errors.Is(err, spi.ErrCancelled):
		return "", err
	case errors.Is(err, artifacts.ErrNotFound):
		return "", spi.Failf(spi.KindNotFound,
			"bucket %s holds no %s, so there is no graph to import", bucket, name)
	case errors.Is(err, artifacts.ErrTooLarge):
		return "", spi.Failf(spi.KindValue,
			"%s in bucket %s is over %d MiB; import it with the engine's import-graph command",
			name, bucket, MaxGraphImportBytes>>20)
	case err != nil:
		r.logger().Error("reading the graph document failed", "name", name, "bucket", bucket, "error", err)
		return "", spi.Failf(spi.KindRuntime, "%s could not be read from bucket %s", name, bucket)
	}
	if len(data) > MaxGraphImportBytes {
		return "", spi.Failf(spi.KindValue,
			"%s in bucket %s is over %d MiB; import it with the engine's import-graph command",
			name, bucket, MaxGraphImportBytes>>20)
	}
	return string(data), nil
}
