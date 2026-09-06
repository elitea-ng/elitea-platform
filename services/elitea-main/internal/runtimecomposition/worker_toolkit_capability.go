package runtimecomposition

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"sort"
)

const (
	currentWorkerToolkitCapabilityVersion = "elitea.worker-toolkit-capability.v1"
	// Both capability files are lists of short identifiers. A kilobyte each is
	// already generous; the ceiling exists so a corrupted or substituted file
	// fails at load instead of being walked.
	maxWorkerToolkitCapabilityBytes = 1 << 14
	maxWorkerToolkitCapabilityNames = 1024

	// PythonWorkerImplementation and RustWorkerImplementation are the two
	// worker images this platform deploys. deploy/scripts/standalone-stack.sh
	// selects between them with STANDALONE_WORKER and the chart with
	// worker.implementation; neither of those reaches this process, so the
	// deployment must state it again here. See WorkerImplementation in config.
	PythonWorkerImplementation = "python"
	RustWorkerImplementation   = "rust"
)

var ErrWorkerToolkitCapabilityInvalid = errors.New(
	"worker toolkit capability snapshot is invalid",
)

// The two capability files are generated from the two workers, not written by
// hand:
//
//   - the Python file records elitea_sdk.tools.FAILED_IMPORTS measured in an
//     environment that installs the image's own extras. Its gate is
//     services/elitea-worker-python/tests/unit/test_toolkit_capability_snapshot.py,
//     which runs in the job that installs exactly those extras.
//   - the Rust file records the toolkit families services/elitea-worker-rust/
//     src/toolkits/materialize.rs actually materializes. Its gate is
//     worker_toolkit_capability_rust_source_test.go, which re-reads that file.
//
//go:embed current_python_worker_toolkit_capability_snapshot.json
var pinnedPythonWorkerToolkitCapabilityJSON []byte

//go:embed current_rust_worker_toolkit_capability_snapshot.json
var pinnedRustWorkerToolkitCapabilityJSON []byte

type pythonWorkerToolkitCapabilityDocument struct {
	SchemaVersion string `json:"schema_version"`
	Implementation string `json:"implementation"`
	SDKRevision   string `json:"sdk_revision"`
	// UnsupportedImportKeys are the SDK registry keys whose import failed in
	// the admitted image. The image installs a measured subset of
	// elitea-sdk[all], so a toolkit whose third-party dependency the subset
	// omits is present in the registry and unusable in the process.
	UnsupportedImportKeys []string `json:"unsupported_import_keys"`
}

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
}

// WorkerToolkitCapability answers whether the configured worker can run a
// toolkit type, and says why when it cannot.
//
// The question has to be asked because the catalogue and the runtime disagree.
// The pinned SDK snapshot holds 52 types. The Python worker image imports 39 of
// them; the other 13 raise at import and fail at the first tool call. The Rust
// worker materializes 22 families and SKIPS anything else with a warning
// (agent_toolkit_skipped), so an unsupported toolkit attaches to an agent and
// then quietly does nothing.
//
// A type the worker cannot run is still SERVED. It is served with
// metadata.hidden and an unavailable reason, never dropped, because the
// operator has to be able to see that the type exists and why it is off — a
// missing tile is indistinguishable from a catalogue that never had the type.
type WorkerToolkitCapability struct {
	implementation string
	// unsupportedImportKeys is the Python answer: a deny set keyed on the SDK
	// import key.
	unsupportedImportKeys map[string]struct{}
	// supportedToolTypes is the Rust answer: an allow set keyed on the type.
	// The two shapes are not interchangeable. The Python image carries the
	// whole registry and loses parts of it, so its answer is the loss. The Rust
	// worker implements families one at a time, so its answer is the list.
	supportedToolTypes map[string]struct{}
}

// WorkerImplementationFromEnv reads which worker image this deployment runs.
//
// The variable is ELITEA_WORKER_IMPLEMENTATION, and it is deliberately NOT
// named ELITEA_RUNTIME_*: that prefix belongs to the runtime dispatch plane,
// which a default install leaves entirely off (deploy/helm/tests/
// render-capabilities.sh fails a default render that carries any such name).
// The toolkit catalogue is served whether or not that plane is enabled.
//
// The default is python, because that is the worker
// deploy/scripts/standalone-stack.sh starts when STANDALONE_WORKER is unset.
// A Helm release defaults to the Rust worker instead, so the chart states the
// value explicitly rather than relying on this default.
//
// An unrecognised value is refused at startup. Falling back to a default would
// mean a typo silently produces the wrong catalogue for the running worker,
// which is the failure this projection exists to prevent.
func WorkerImplementationFromEnv(lookup LookupEnv) (string, error) {
	if lookup == nil {
		return "", errors.New("worker environment lookup is required")
	}
	value, _ := lookup("ELITEA_WORKER_IMPLEMENTATION")
	switch value {
	case "":
		return PythonWorkerImplementation, nil
	case PythonWorkerImplementation, RustWorkerImplementation:
		return value, nil
	default:
		return "", errors.New(
			"ELITEA_WORKER_IMPLEMENTATION must be python or rust",
		)
	}
}

// LoadPinnedWorkerToolkitCapability builds the capability projection for one
// worker implementation. An unknown implementation is an error rather than a
// permissive default: guessing "python" for a Rust deployment would offer 39
// types that the running worker skips in silence.
func LoadPinnedWorkerToolkitCapability(implementation string) (*WorkerToolkitCapability, error) {
	switch implementation {
	case PythonWorkerImplementation:
		return LoadWorkerToolkitCapability(implementation, pinnedPythonWorkerToolkitCapabilityJSON)
	case RustWorkerImplementation:
		return LoadWorkerToolkitCapability(implementation, pinnedRustWorkerToolkitCapabilityJSON)
	default:
		return nil, ErrWorkerToolkitCapabilityInvalid
	}
}

// LoadWorkerToolkitCapability parses one bounded capability document. It is
// exported so that the loader can be exercised against documents the embedded
// files are not: a corrupt or substituted file must fail at load rather than
// serve a catalogue built from half of it.
func LoadWorkerToolkitCapability(
	implementation string,
	data []byte,
) (*WorkerToolkitCapability, error) {
	switch implementation {
	case PythonWorkerImplementation:
		return loadPythonWorkerToolkitCapability(data)
	case RustWorkerImplementation:
		return loadRustWorkerToolkitCapability(data)
	default:
		return nil, ErrWorkerToolkitCapabilityInvalid
	}
}

func loadPythonWorkerToolkitCapability(data []byte) (*WorkerToolkitCapability, error) {
	var document pythonWorkerToolkitCapabilityDocument
	if err := decodeWorkerToolkitCapability(data, &document); err != nil {
		return nil, err
	}
	if document.SchemaVersion != currentWorkerToolkitCapabilityVersion ||
		document.Implementation != PythonWorkerImplementation ||
		!validCurrentToolkitSchemaIdentifier(document.SDKRevision) ||
		len(document.UnsupportedImportKeys) > maxWorkerToolkitCapabilityNames {
		return nil, ErrWorkerToolkitCapabilityInvalid
	}
	keys, err := workerToolkitCapabilityNameSet(document.UnsupportedImportKeys)
	if err != nil {
		return nil, err
	}
	return &WorkerToolkitCapability{
		implementation:        PythonWorkerImplementation,
		unsupportedImportKeys: keys,
	}, nil
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
	return &WorkerToolkitCapability{
		implementation:     RustWorkerImplementation,
		supportedToolTypes: types,
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
// importKey is the type's SDK registry key, from the toolkit catalogue. An
// empty key means the SDK imports the toolkit unconditionally.
//
// A nil projection supports everything and gives no reason. That is the
// deliberate reading of absence: a deployment that has not stated its worker
// must keep serving the catalogue it served before this projection existed,
// rather than losing every tile to a missing file.
func (c *WorkerToolkitCapability) SupportsToolkitType(
	toolkitType string,
	importKey string,
) (bool, string) {
	if c == nil {
		return true, ""
	}
	switch c.implementation {
	case PythonWorkerImplementation:
		if importKey == "" {
			// A runtime toolkit: artifact, memory, mcp, sandbox, vectorstore.
			// The SDK imports these with itself, so no import of them can fail.
			return true, ""
		}
		if _, unsupported := c.unsupportedImportKeys[importKey]; unsupported {
			return false, "the admitted Python worker image does not carry the " +
				"dependencies of the " + importKey + " toolkit"
		}
		return true, ""
	case RustWorkerImplementation:
		if _, supported := c.supportedToolTypes[toolkitType]; supported {
			return true, ""
		}
		return false, "the native worker has no toolkit family for " + toolkitType
	default:
		return true, ""
	}
}

// UnsupportedNames returns the names this projection refuses, sorted. The
// admin surface reads it to show which half of the catalogue the deployment
// cannot run; the shape is the projection's own (import keys for Python,
// nothing for Rust, whose answer is an allow list).
func (c *WorkerToolkitCapability) UnsupportedNames() []string {
	if c == nil || len(c.unsupportedImportKeys) == 0 {
		return nil
	}
	names := make([]string, 0, len(c.unsupportedImportKeys))
	for name := range c.unsupportedImportKeys {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// SupportedNames returns the names this projection admits, sorted, for an
// allow-list projection. Nil for a deny-list projection.
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
