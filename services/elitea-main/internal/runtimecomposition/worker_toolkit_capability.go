package runtimecomposition

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"sort"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/indexcatalog"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/rustonly"
)

const (
	currentWorkerToolkitCapabilityVersion = "elitea.worker-toolkit-capability.v1"
	// Both capability files are lists of short identifiers. A kilobyte each is
	// already generous; the ceiling exists so a corrupted or substituted file
	// fails at load instead of being walked.
	maxWorkerToolkitCapabilityBytes = 1 << 14
	maxWorkerToolkitCapabilityNames = 1024

	// RustWorkerImplementation is the one worker image this platform deploys
	// (the Python worker was removed). The name is still carried through the
	// API surfaces that report which worker answers (/runtime/capabilities).
	RustWorkerImplementation = "rust"
)

var ErrWorkerToolkitCapabilityInvalid = errors.New(
	"worker toolkit capability snapshot is invalid",
)

// The capability file is generated from the Rust worker, not written by hand:
// it records the toolkit families libs/rust/agent-runtime/src/toolkits/
// materialize.rs and the worker's src/toolkits/direct_runtime.rs materialize.
// Its gate is TestRustCapabilitySnapshotMatchesTheRustSource, which reads both
// files.
//
//go:embed current_rust_worker_toolkit_capability_snapshot.json
var pinnedRustWorkerToolkitCapabilityJSON []byte

type rustWorkerToolkitCapabilityDocument struct {
	SchemaVersion  string `json:"schema_version"`
	Implementation string `json:"implementation"`
	Source         string `json:"source"`
	// SupportedToolTypes are the stored `type` strings the Rust worker
	// materializes. They are compared with the platform type as-is and are
	// deliberately NOT translated: the Rust worker matches on the type string
	// it reads from the toolkit row, so its "k8s" arm never matches the
	// platform type "kubernetes". Translating here would report a capability
	// the worker does not have.
	SupportedToolTypes []string `json:"supported_tool_types"`
	// SupportedTools lists, per type, the TOOLS the Rust family serves when
	// it serves fewer than the SDK declares (ADR-0027: github, sharepoint
	// and artifact are partial). A type without an entry serves every tool
	// the SDK declares. Its gate is the worker's SDK conformance test
	// (libs/rust/agent-runtime/src/toolkits/sdk_conformance.rs), which
	// builds each family and compares the names it actually serves.
	SupportedTools map[string][]string `json:"supported_tools,omitempty"`
}

// WorkerToolkitCapability answers whether the configured worker can run a
// toolkit type, and says why when it cannot.
//
// The question has to be asked because the catalogue and the runtime disagree.
// The pinned SDK snapshot holds 52 types. The Rust worker materializes 42
// families (several partial, per supported_tools) and
// SKIPS anything else with a warning (agent_toolkit_skipped), so an
// unsupported toolkit attaches to an agent and then quietly does nothing.
//
// A type the worker cannot run is still SERVED. It is served with
// metadata.hidden and an unavailable reason, never dropped, because the
// operator has to be able to see that the type exists and why it is off — a
// missing tile is indistinguishable from a catalogue that never had the type.
type WorkerToolkitCapability struct {
	implementation string
	// supportedToolTypes is an allow set keyed on the type: the Rust worker
	// implements families one at a time, so its answer is the list.
	supportedToolTypes map[string]struct{}
	// supportedTools is the per-tool allow list of a partial Rust family,
	// keyed by type; a type absent from it serves every tool.
	supportedTools map[string]map[string]struct{}
	// withheldReasons says why a type the pinned snapshot lists is withheld by
	// this deployment's configuration; SupportsToolkitType returns the sentence.
	withheldReasons map[string]string
}

// LoadPinnedWorkerToolkitCapability builds the capability projection for one
// worker implementation. Only "rust" exists; anything else (including the
// retired "python") is an error rather than a permissive default.
func LoadPinnedWorkerToolkitCapability(implementation string) (*WorkerToolkitCapability, error) {
	if implementation != RustWorkerImplementation {
		return nil, ErrWorkerToolkitCapabilityInvalid
	}
	return LoadWorkerToolkitCapability(implementation, pinnedRustWorkerToolkitCapabilityJSON)
}

// LoadWorkerToolkitCapability parses one bounded capability document. It is
// exported so that the loader can be exercised against documents the embedded
// files are not: a corrupt or substituted file must fail at load rather than
// serve a catalogue built from half of it.
func LoadWorkerToolkitCapability(
	implementation string,
	data []byte,
) (*WorkerToolkitCapability, error) {
	if implementation != RustWorkerImplementation {
		return nil, ErrWorkerToolkitCapabilityInvalid
	}
	return loadRustWorkerToolkitCapability(data)
}

func loadRustWorkerToolkitCapability(data []byte) (*WorkerToolkitCapability, error) {
	var document rustWorkerToolkitCapabilityDocument
	if err := decodeWorkerToolkitCapability(data, &document); err != nil {
		return nil, err
	}
	if document.SchemaVersion != currentWorkerToolkitCapabilityVersion ||
		document.Implementation != RustWorkerImplementation ||
		len(document.SupportedToolTypes) == 0 ||
		len(document.SupportedToolTypes) > maxWorkerToolkitCapabilityNames {
		return nil, ErrWorkerToolkitCapabilityInvalid
	}
	types, err := workerToolkitCapabilityNameSet(document.SupportedToolTypes)
	if err != nil {
		return nil, err
	}
	if len(document.SupportedTools) > len(types) {
		return nil, ErrWorkerToolkitCapabilityInvalid
	}
	tools := make(map[string]map[string]struct{}, len(document.SupportedTools))
	for toolkitType, names := range document.SupportedTools {
		if _, known := types[toolkitType]; !known ||
			len(names) == 0 || len(names) > maxWorkerToolkitCapabilityNames {
			return nil, ErrWorkerToolkitCapabilityInvalid
		}
		set, err := workerToolkitCapabilityNameSet(names)
		if err != nil {
			return nil, err
		}
		tools[toolkitType] = set
	}
	return &WorkerToolkitCapability{
		implementation:     RustWorkerImplementation,
		supportedToolTypes: types,
		supportedTools:     tools,
	}, nil
}

func decodeWorkerToolkitCapability(data []byte, document any) error {
	if len(data) == 0 || len(data) > maxWorkerToolkitCapabilityBytes {
		return ErrWorkerToolkitCapabilityInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(document); err != nil {
		return ErrWorkerToolkitCapabilityInvalid
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ErrWorkerToolkitCapabilityInvalid
	}
	return nil
}

func workerToolkitCapabilityNameSet(names []string) (map[string]struct{}, error) {
	set := make(map[string]struct{}, len(names))
	previous := ""
	for index, name := range names {
		if !validCurrentToolkitSchemaIdentifier(name) ||
			(index != 0 && name <= previous) {
			return nil, ErrWorkerToolkitCapabilityInvalid
		}
		previous = name
		set[name] = struct{}{}
	}
	return set, nil
}

// Implementation names the worker this projection answers for.
func (c *WorkerToolkitCapability) Implementation() string {
	if c == nil {
		return ""
	}
	return c.implementation
}

// SupportsToolkitType reports whether the configured worker can run one type,
// and gives the reason when it cannot.
//
// importKey is the type's SDK registry key, from the toolkit catalogue. The
// Rust worker answers on the type alone, so it is accepted and not read.
//
// A nil projection supports every type the SDK has and gives no reason: a
// deployment that has not stated its worker keeps serving the catalogue it
// served before this projection existed, rather than losing every tile to a
// missing file. It does NOT support the types only the Rust worker serves
// (docx, xlsx; ADR-0032 P4): absence of a statement is not a statement that
// the Rust worker is there, and offering a tile no running worker can serve is
// the failure this projection exists to prevent.
func (c *WorkerToolkitCapability) SupportsToolkitType(
	toolkitType string,
	_ string,
) (bool, string) {
	if c == nil {
		return rustonly.AbsentTypeVerdict(toolkitType)
	}
	if _, supported := c.supportedToolTypes[toolkitType]; supported {
		return true, ""
	}
	if reason, withheld := c.withheldReasons[toolkitType]; withheld {
		return false, reason
	}
	// A sentence for the person reading it in the UI (the Indexes tab and the
	// toolkit catalogue show it verbatim). "Native worker" and "toolkit
	// family" are internal terms (UI-DC-1).
	return false, "This deployment's agent worker does not support the " +
		toolkitType + " toolkit."
}

// WithRustIndexTools is the capability of a Rust worker whose deployment
// indexes with the Rust runtime (ELITEA_INDEXING_RUNTIME=rust, ADR-0030 I1):
// the five index tools are then served inside every I1 indexing family, so a
// partial family's allow list gains them. The pinned snapshot cannot say this
// itself — the same worker image serves them under one setting and not the
// other — and the SDK conformance gate that generates the snapshot builds the
// families without them, so the rule lives here, beside the setting that
// decides it. A family that is not partial (serves every SDK tool already) is
// returned unchanged.
func (c *WorkerToolkitCapability) WithRustIndexTools() *WorkerToolkitCapability {
	if c == nil {
		return c
	}
	tools := make(map[string]map[string]struct{}, len(c.supportedTools))
	for toolkitType, names := range c.supportedTools {
		copied := make(map[string]struct{}, len(names)+len(indexcatalog.ToolNames))
		for name := range names {
			copied[name] = struct{}{}
		}
		if indexcatalog.Family(toolkitType) {
			for _, name := range indexcatalog.ToolNames {
				copied[name] = struct{}{}
			}
		}
		tools[toolkitType] = copied
	}
	return &WorkerToolkitCapability{
		implementation:     c.implementation,
		supportedToolTypes: c.supportedToolTypes,
		supportedTools:     tools,
	}
}

// ForIndexingRuntime is the capability under the deployment's indexing
// decision: widened by the index tools when config.RustIndexing() — the ONE
// predicate the composition root also keys the registry, the catalog freeze
// and the vector store on — and unchanged otherwise. ELITEA_INDEXING_RUNTIME
// alone is not that decision: `rust` without index dispatch is refused at
// configuration load, and a Config that nonetheless says so indexes with
// nothing, so the catalogue must not offer the tools either.
func (c *WorkerToolkitCapability) ForIndexingRuntime(config Config) *WorkerToolkitCapability {
	if !config.RustIndexing() {
		return c.withoutMemory("This deployment's agent worker serves the memory toolkit only when it indexes with the Rust runtime (the vector store holds the memories).")
	}
	widened := c.WithRustIndexTools()
	if config.MemoryEmbeddingModel == "" {
		return widened.withoutMemory("Memory needs an embedding model configured by the operator (worker.runtime.memoryEmbeddingModel).")
	}
	return widened
}

// withoutMemory is a Rust capability without the memory toolkit, with the
// sentence the catalogue shows for it. The Rust worker keeps memories in
// elitea-vector, reached with the claim's vector token (minted only under the
// Rust indexing runtime) and embeds them with an operator-chosen model; a
// worker lacking either would skip the toolkit in silence while the catalogue
// offered the tile.
func (c *WorkerToolkitCapability) withoutMemory(reason string) *WorkerToolkitCapability {
	if c == nil {
		return c
	}
	types := make(map[string]struct{}, len(c.supportedToolTypes))
	for toolkitType := range c.supportedToolTypes {
		types[toolkitType] = struct{}{}
	}
	delete(types, "memory")
	reasons := make(map[string]string, len(c.withheldReasons)+1)
	for toolkitType, text := range c.withheldReasons {
		reasons[toolkitType] = text
	}
	reasons["memory"] = reason
	return &WorkerToolkitCapability{
		implementation:     c.implementation,
		supportedToolTypes: types,
		supportedTools:     c.supportedTools,
		withheldReasons:    reasons,
	}
}

// SupportsTool reports whether the configured worker serves one TOOL of a
// type it supports (SupportsToolkitType answers for the type). It refuses:
// a tool only the Rust worker serves, on a nil projection (a deployment that
// has not stated its worker); and a tool a partial Rust family does not serve.
// Every other answer is yes.
func (c *WorkerToolkitCapability) SupportsTool(toolkitType, toolName string) (bool, string) {
	if c == nil {
		// The same reading of absence as SupportsToolkitType: the tools only the
		// Rust worker serves are not offered on a deployment that has not said
		// it runs one.
		return rustonly.AbsentToolVerdict(toolkitType, toolName)
	}
	tools, partial := c.supportedTools[toolkitType]
	if !partial {
		return true, ""
	}
	if _, served := tools[toolName]; served {
		return true, ""
	}
	return false, "This deployment's agent worker does not support the " +
		toolName + " tool of the " + toolkitType + " toolkit."
}

// SupportedNames returns the toolkit types this projection admits, sorted.
func (c *WorkerToolkitCapability) SupportedNames() []string {
	if c == nil || len(c.supportedToolTypes) == 0 {
		return nil
	}
	names := make([]string, 0, len(c.supportedToolTypes))
	for name := range c.supportedToolTypes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
