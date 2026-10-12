package run

// Graph transfer (descriptor revision legacy-v2): `import_graph` and
// `export_graph`, the native engine's `import-graph` / `export-graph`
// operator commands as tools.
//
// export_graph: the engine returns graph.json as an artifact and a summary
// (counts, revision, size, bucket) as its answer. The host puts the artifact
// in the toolkit's bucket and leaves it OUT of the terminal body (StoreExport):
// the ordinary composition carries every artifact inline too, which for a
// graph meant the whole document (43 MB for a 50-file repository, measured)
// in the poll result as well as the bucket. Because the bucket is the export's
// only output, a call without a bucket transport is refused before the engine
// runs, and a failed upload fails the invocation (the stored graph is
// untouched, so the export can simply run again).
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
	"unicode/utf8"

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
	// above the general artifact read bound (artifacts.MaxDownloadBytes,
	// 32 MiB) on purpose: export_graph writes every entity's embedding, and
	// a 50-file repository embedded at 2560 dimensions exported to 43 MB —
	// so at 32 MiB the tools could not round-trip the smallest real graph.
	// A larger graph goes through the engine's `import-graph` command.
	MaxGraphImportBytes = 64 << 20
	// maxGraphImportEncoded bounds the document as it travels: JSON-escaped
	// once more inside the invoke body, which the engine sidecar caps at
	// 96 MiB (elitea_inventory_engine::MAX_INVOKE_BYTES). Escaping a pretty
	// graph adds about a tenth; the check keeps a pathological one (all
	// quotes) a clear refusal here rather than a socket error there.
	maxGraphImportEncoded = 88 << 20
)

// graphReader is the half of artifacts.Store an import needs.
type graphReader interface {
	DownloadUpTo(ctx context.Context, bucket, key string, limit int) ([]byte, error)
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
	data, err := reader.DownloadUpTo(ctx, bucket, name, MaxGraphImportBytes)
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
	if jsonStringLen(data) > maxGraphImportEncoded {
		return "", spi.Failf(spi.KindValue,
			"%s in bucket %s is too large to send to the engine; import it with the engine's import-graph command",
			name, bucket)
	}
	return string(data), nil
}

// jsonStringLen is len(json.Marshal(string(data))), counted without
// building either copy: encoding/json's string escaping, HTML escaping
// included. A quote, a backslash and \b \f \n \r \t take two bytes; any
// other control character, '<', '>', '&', U+2028, U+2029 and each byte of
// invalid UTF-8 (written as \ufffd) take six; everything else is itself.
func jsonStringLen(data []byte) int {
	n := 2
	for i := 0; i < len(data); {
		if b := data[i]; b < utf8.RuneSelf {
			switch {
			case b == '"' || b == '\\' || b == '\b' || b == '\f' || b == '\n' || b == '\r' || b == '\t':
				n += 2
			case b < 0x20 || b == '<' || b == '>' || b == '&':
				n += 6
			default:
				n++
			}
			i++
			continue
		}
		r, size := utf8.DecodeRune(data[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			n += 6
		case r == '\u2028' || r == '\u2029':
			n += 6
		default:
			n += size
		}
		i += size
	}
	return n
}

// ExportClient is the bucket transport an export_graph call needs, or the
// refusal when the call carries none.
func (r *Runner) ExportClient(params Params) (ArtifactClient, error) {
	llmSettings := object(params["llm_settings"])
	if llmSettings == nil {
		llmSettings = map[string]any{}
	}
	var client ArtifactClient
	if r.Artifacts != nil {
		built, err := r.Artifacts(llmSettings)
		if err != nil {
			r.logger().Error("building the bucket transport failed", "error", err)
			return nil, spi.Failf(spi.KindRuntime, "%s: the bucket transport cannot be built", ExportTool)
		}
		client = built
	}
	if client == nil {
		return nil, spi.Failf(spi.KindValue,
			"%s writes %s to this toolkit's bucket, and the call carries no bucket transport. "+
				"Run the tool through the platform rather than calling the provider directly.",
			ExportTool, DefaultGraphArtifact)
	}
	return client, nil
}

// StoreExport uploads an export's artifact objects and returns the objects
// without them: the terminal body carries the engine's summary, never the
// document. Any upload failure fails the call.
func (r *Runner) StoreExport(ctx context.Context, objects []Object, client ArtifactClient, tc *spi.Context) ([]Object, error) {
	kept := make([]Object, 0, len(objects))
	for _, obj := range objects {
		if !obj.IsArtifact() || obj.NameString() == "" {
			kept = append(kept, obj)
			continue
		}
		if err := tc.Checkpoint(); err != nil {
			return nil, err
		}
		bucket, name := obj.ResultBucket, obj.NameString()
		if bucket == "" {
			bucket = DefaultBucket
		}
		if err := tc.Thinking(ctx, fmt.Sprintf("Writing %s to bucket %s", name, bucket)); err != nil {
			return nil, err
		}
		if err := client.Upload(ctx, bucket, name, []byte(obj.Data)); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, spi.ErrCancelled) {
				return nil, err
			}
			r.logger().Error("storing the exported graph failed", "name", name, "bucket", bucket, "error", err)
			return nil, spi.Failf(spi.KindRuntime,
				"%s could not be written to bucket %s; the stored graph is unchanged, so run %s again",
				name, bucket, ExportTool)
		}
	}
	return kept, nil
}
