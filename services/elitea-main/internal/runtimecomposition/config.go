// Package runtimecomposition owns the optional production runtime dependency
// graph and its lifecycle. The current-baseline HTTP surface remains compatible while
// this package is disabled.
package runtimecomposition

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/elitea-ng/elitea-platform/libs/go/natsconn"

	executionapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/executions"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/commandbus"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/runtimegrpc"
)

const (
	maxConfigPathBytes = 4096
	maxNATSURLBytes    = 2048
	// maxRuntimeOutstanding is the repository's admission ceiling, so the env
	// parse and the CHECKs in shared/0170 cannot drift apart (#966).
	maxRuntimeOutstanding = repos.MaxSupportedOutstandingJobs
	// productionTransportMessageBytes is the validation route's message
	// bound: max_transport_message_bytes, the streams' MaxMsgSize.
	productionTransportMessageBytes = commandbus.MaxMessageBytes
)

// The runtime plane's NATS connection (docs/runtime-command-bus.md): the
// command bus producer and the execution-replay wake-up. The URL is tls://
// with the elitea-main-runtime client identity below in a cluster, nats://
// without it in compose; natsconn.Material.CheckURL refuses any other pairing
// and any credential in the URL.
const (
	runtimeNATSURLEnv = "ELITEA_RUNTIME_NATS_URL"
	runtimeNATSPrefix = "ELITEA_RUNTIME"
	// The three files natsconn.FromEnv(runtimeNATSPrefix) reads, named
	// literally so the env-drift gate sees that the chart sets what the code
	// reads (TestRuntimeNATSTLSNamesAreNatsconns pins them).
	runtimeNATSTLSCAFileEnv   = "ELITEA_RUNTIME_NATS_TLS_CA_FILE"
	runtimeNATSTLSCertFileEnv = "ELITEA_RUNTIME_NATS_TLS_CERT_FILE"
	runtimeNATSTLSKeyFileEnv  = "ELITEA_RUNTIME_NATS_TLS_KEY_FILE"
)

type LookupEnv func(string) (string, bool)

const (
	// IndexingRuntimeEnv names the deployment-wide indexing implementation.
	// It is not ELITEA_RUNTIME_*: that prefix belongs to the dispatch plane,
	// which a default install leaves off, and this choice is not a plane.
	//
	// The Rust runtime is the only implementation since the Python worker was
	// removed: index metadata lives in elitea_runtime.index_registry and the
	// vectors in elitea-vector whenever index ingest dispatch is composed. The
	// variable is still read so that a deployment that pinned the retired
	// value is refused at startup instead of silently switching stores.
	IndexingRuntimeEnv = "ELITEA_INDEXING_RUNTIME"
	// MemoryEmbeddingModelEnv is the gateway embedding model the Rust worker's
	// memory toolkit embeds with (worker unit M9). The chart renders it from
	// the same value as the worker's runtime.json memory_embedding_model, so
	// this process knows whether memory is actually served.
	MemoryEmbeddingModelEnv = "ELITEA_MEMORY_EMBEDDING_MODEL"

	IndexingRuntimeRust = "rust"
)

// checkIndexingRuntimeEnv reads ELITEA_INDEXING_RUNTIME. Unset, empty and
// "rust" are accepted. Anything else, in particular the retired "python"
// (whose per-project index_meta store is gone with #1359), is refused at
// startup: a deployment still pinned to it must be told rather than switched
// silently.
func checkIndexingRuntimeEnv(lookup LookupEnv) error {
	if lookup == nil {
		return errors.New("runtime environment lookup is required")
	}
	value, _ := lookup(IndexingRuntimeEnv)
	switch value {
	case "", IndexingRuntimeRust:
		return nil
	case "python":
		return fmt.Errorf("%s=python is no longer supported: the Python worker was removed and Rust is the only indexing runtime; unset it or set it to rust (indexes created under python are not migrated and must be created again)", IndexingRuntimeEnv)
	default:
		return fmt.Errorf("%s must be rust or unset", IndexingRuntimeEnv)
	}
}

// platformListenerFromEnv reads the platform listener (#1359 D1): optional,
// with its TLS files required only when it is opened, and never on the
// public HTTP listener's port. It is read whether the runtime is on or off.
func platformListenerFromEnv(lookup func(string) (string, bool)) (string, runtimegrpc.ServerTLSFiles, error) {
	names := []string{
		"ELITEA_RUNTIME_PLATFORM_TLS_CERT_FILE",
		"ELITEA_RUNTIME_PLATFORM_TLS_KEY_FILE",
		"ELITEA_RUNTIME_PLATFORM_TLS_CLIENT_CA_FILE",
	}
	address, _ := lookup("ELITEA_RUNTIME_PLATFORM_ADDRESS")
	if address == "" {
		for _, name := range names {
			if value, _ := lookup(name); value != "" {
				return "", runtimegrpc.ServerTLSFiles{}, fmt.Errorf("%s is set but ELITEA_RUNTIME_PLATFORM_ADDRESS is not: "+
					"name the platform listener's address, or remove its TLS files", name)
			}
		}
		return "", runtimegrpc.ServerTLSFiles{}, nil
	}
	files := make([]string, len(names))
	for i, name := range names {
		value, _ := lookup(name)
		if value == "" {
			return "", runtimegrpc.ServerTLSFiles{}, fmt.Errorf("%s is required when ELITEA_RUNTIME_PLATFORM_ADDRESS is set", name)
		}
		files[i] = value
	}
	// The platform listener is its own gRPC server, so it cannot share the
	// public listener's (cleartext HTTP) port.
	publicPort, err := publicListenPort(lookup)
	if err != nil {
		return "", runtimegrpc.ServerTLSFiles{}, err
	}
	if _, platformPort, splitErr := net.SplitHostPort(address); splitErr == nil && platformPort == publicPort {
		return "", runtimegrpc.ServerTLSFiles{}, errors.New("ELITEA_RUNTIME_PLATFORM_ADDRESS must not use the public HTTP listener's port")
	}
	return address, runtimegrpc.ServerTLSFiles{CertificateChainPath: files[0], PrivateKeyPath: files[1], ClientCAPath: files[2]}, nil
}

// RetiredCurrentMainBaseURLEnv named the internal MCP builders' HTTP origin
// until #1359 U5; ConfigFromEnv refuses it.
const RetiredCurrentMainBaseURLEnv = "ELITEA_RUNTIME_CURRENT_MAIN_BASE_URL"

type Config struct {
	Enabled                 bool
	ToolkitDiscoveryEnabled bool

	// CommandStream is the configuration-validation route's JetStream stream
	// (commandbus.StreamValidate in every shipped profile).
	CommandStream  string
	MaxOutstanding int64

	// SSEStreamLimits bounds the execution-event SSE streams this replica
	// serves. ConfigFromEnv parses it from ELITEA_RUNTIME_SSE_*; the defaults
	// stay built in when the variables are unset.
	SSEStreamLimits executionapi.SSEStreamLimits

	IndexIngestDispatchEnabled bool
	// MemoryEmbeddingModel is ELITEA_MEMORY_EMBEDDING_MODEL; empty means the
	// operator configured none and the Rust worker serves no memory toolkit.
	MemoryEmbeddingModel          string
	IndexSchedulingEnabled        bool
	SchedulerInstanceID           string
	IndexIngestCommandStream      string
	AgentExecutionDispatchEnabled bool
	AgentExecutionCommandStream   string

	// ExecutionInterruptsAPIEnabled registers the per-interrupt HITL decision
	// API (ELITEA_RUNTIME_EXECUTION_INTERRUPTS_API_ENABLED). Default off; it
	// requires agent execution dispatch because the ledger it reads and
	// decides exists only with the agent runtime.
	ExecutionInterruptsAPIEnabled bool

	// NATSURL and NATSMaterial: the runtime plane's own NATS connection, as
	// the elitea-main-runtime identity (ELITEA_RUNTIME_NATS_URL,
	// ELITEA_RUNTIME_NATS_TLS_{CA,CERT,KEY}_FILE).
	NATSURL      string
	NATSMaterial natsconn.Material

	// Empty keeps sandbox grant issuance disabled. Values are exact identities.
	SandboxAudiences        []string
	RustCompiledSnapshots   *CompiledSnapshotConfig
	CodeOwnerRecovery       *CodeOwnerConfig
	CodeWorkspace           *CodeWorkspaceConfig
	CodePlatform            *CodePlatformDeploymentConfig
	CodeDebugArtifacts      *CodeDebugArtifactsConfig
	SigningKeyID            string
	SigningKeyFile          string
	VerificationKeyringFile string

	// VectorIntrospectionClients are the client certificate identities
	// admitted to elitea.vector.v1.TokenIntrospectionService on the control
	// listener and on the platform listener (ELITEA_VECTOR_INTROSPECTION_CLIENTS,
	// ADR-0031). Read with the runtime on or off; with it off the platform
	// listener must be open. Empty leaves the service unregistered on both.
	VectorIntrospectionClients []string
	// EgressAuditClients are the egress proxy's client certificate
	// identities admitted to elitea.runtime.v1.EgressAuditService on the
	// control listener (ELITEA_EGRESS_AUDIT_CLIENTS, ADR-0034 decision 9).
	// Empty leaves the service unregistered.
	EgressAuditClients []string
	// EgressAuditRetention is how long the stored per-turn summaries are
	// kept (ELITEA_EGRESS_AUDIT_RETENTION_DAYS, default 30, at most 365).
	// Zero takes the default.
	EgressAuditRetention time.Duration

	ControlAddress string
	OutputAddress  string
	ContentAddress string
	ControlTLS     runtimegrpc.ServerTLSFiles
	OutputTLS      runtimegrpc.ServerTLSFiles
	ContentTLS     runtimegrpc.ServerTLSFiles

	// PlatformAddress is the internal platform gRPC listener
	// (ELITEA_RUNTIME_PLATFORM_ADDRESS, #1359 D1): the port in-cluster
	// workloads — the worker, the DeepWiki/Inventory engines, the
	// sub-application host — call elitea-main's internal gRPC services on.
	// It replaced the `platform-edge` proxy (#1359). Optional: empty opens
	// no listener. Read with the runtime on or off: its services need no
	// runtime plane. When set, PlatformTLS is required and a client certificate
	// that chains to PlatformTLS.ClientCAPath is required at the handshake.
	PlatformAddress string
	PlatformTLS     runtimegrpc.ServerTLSFiles
}

func ConfigFromEnv(lookup LookupEnv) (Config, error) {
	if lookup == nil {
		return Config{}, errors.New("runtime environment lookup is required")
	}
	enabledValue, _ := lookup("ELITEA_RUNTIME_ENABLED")
	if err := checkIndexingRuntimeEnv(lookup); err != nil {
		return Config{}, err
	}
	switch enabledValue {
	case "", "false":
		if _, _, _, err := codeConsumerConfigFromEnv(lookup, nil); err != nil {
			return Config{}, err
		}
		if _, err := codeOwnerConfigFromEnv(lookup, false, false, nil); err != nil {
			return Config{}, err
		}
		if _, err := compiledSnapshotConfigFromEnv(lookup, false, false, nil); err != nil {
			return Config{}, err
		}
		for _, name := range []string{
			executionapi.EnvSSEMaxStreams,
			executionapi.EnvSSEMaxStreamsPrincipal,
			executionapi.EnvSSEMaxStreamsProject,
		} {
			if value, ok := lookup(name); ok && value != "" {
				return Config{}, errors.New("runtime SSE stream limits require explicit enablement")
			}
		}
		if value, _ := lookup("ELITEA_RUNTIME_EXECUTION_INTERRUPTS_API_ENABLED"); value != "" && value != "false" {
			return Config{}, errors.New("ELITEA_RUNTIME_EXECUTION_INTERRUPTS_API_ENABLED requires ELITEA_RUNTIME_ENABLED=true")
		}
		if value, _ := lookup(EgressAuditClientsEnv); strings.TrimSpace(value) != "" {
			return Config{}, errors.New(EgressAuditClientsEnv + " requires ELITEA_RUNTIME_ENABLED=true: the service is served on the runtime control listener")
		}
		// The platform listener does not need the runtime plane: its
		// services run the public handler's routes in process (#1359 U5),
		// so a stack without the runtime (the E2E one) still opens it for
		// the sub-application hosts' artifact calls.
		address, platformTLS, err := platformListenerFromEnv(lookup)
		if err != nil {
			return Config{}, err
		}
		// elitea-vector's token introspection needs no runtime plane either:
		// with the runtime off it is served on the platform listener alone,
		// so that listener must be open.
		vectorClients, err := vectorIntrospectionClients(lookup)
		if err != nil {
			return Config{}, err
		}
		if len(vectorClients) > 0 && address == "" {
			return Config{}, errors.New(VectorIntrospectionClientsEnv +
				" requires ELITEA_RUNTIME_ENABLED=true or ELITEA_RUNTIME_PLATFORM_ADDRESS: the service is served on the runtime control listener or the platform listener")
		}
		return Config{PlatformAddress: address, PlatformTLS: platformTLS, VectorIntrospectionClients: vectorClients}, nil
	case "true":
	default:
		return Config{}, errors.New("ELITEA_RUNTIME_ENABLED must be true or false")
	}

	required := func(name string) (string, error) {
		value, ok := lookup(name)
		if !ok || value == "" {
			return "", fmt.Errorf("%s is required when the runtime is enabled", name)
		}
		return value, nil
	}
	integer := func(name string) (int64, error) {
		value, err := required(name)
		if err != nil {
			return 0, err
		}
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s must be a canonical positive integer", name)
		}
		if parsed <= 0 || strconv.FormatInt(parsed, 10) != value {
			return 0, fmt.Errorf("%s must be a canonical positive integer", name)
		}
		return parsed, nil
	}

	var config Config
	var err error
	config.Enabled = true
	if value, ok := lookup(MemoryEmbeddingModelEnv); ok {
		config.MemoryEmbeddingModel = strings.TrimSpace(value)
	}
	if config.CommandStream, err = required("ELITEA_RUNTIME_COMMAND_STREAM"); err != nil {
		return Config{}, err
	}
	if config.MaxOutstanding, err = integer("ELITEA_RUNTIME_MAX_OUTSTANDING"); err != nil {
		return Config{}, err
	}
	indexIngestEnabled, _ := lookup("ELITEA_RUNTIME_INDEX_INGEST_DISPATCH_ENABLED")
	switch indexIngestEnabled {
	case "", "false":
		for _, name := range []string{"ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM"} {
			if value, ok := lookup(name); ok && value != "" {
				return Config{}, errors.New("runtime index ingest dispatch settings require explicit enablement")
			}
		}
	case "true":
		config.IndexIngestDispatchEnabled = true
		if config.IndexIngestCommandStream, err = required("ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM"); err != nil {
			return Config{}, err
		}
	default:
		return Config{}, errors.New("ELITEA_RUNTIME_INDEX_INGEST_DISPATCH_ENABLED must be true or false")
	}
	// Retired (#1359 U5). It was the HTTP origin of the internal MCP builders
	// handed to the worker; the worker reaches them over InternalToolService
	// on the platform gRPC listener now, which nothing can name by URL. A
	// value left over from an older deployment is refused rather than ignored,
	// so the operator learns the hop changed.
	if value, ok := lookup(RetiredCurrentMainBaseURLEnv); ok && value != "" {
		return Config{}, fmt.Errorf("%s is retired (#1359 U5): the worker reaches the internal MCP builders "+
			"over elitea.runtime.v1.InternalToolService on the platform listener "+
			"(ELITEA_RUNTIME_PLATFORM_ADDRESS), admitted by ELITEA_INTERNAL_TOOL_SERVICE_CLIENTS; remove the variable",
			RetiredCurrentMainBaseURLEnv)
	}
	agentExecutionEnabled, _ := lookup("ELITEA_RUNTIME_AGENT_EXECUTION_DISPATCH_ENABLED")
	switch agentExecutionEnabled {
	case "", "false":
		if value, ok := lookup("ELITEA_RUNTIME_AGENT_EXECUTION_COMMAND_STREAM"); ok && value != "" {
			return Config{}, errors.New("runtime agent execution dispatch settings require explicit enablement")
		}
	case "true":
		config.AgentExecutionDispatchEnabled = true
		if config.AgentExecutionCommandStream, err = required("ELITEA_RUNTIME_AGENT_EXECUTION_COMMAND_STREAM"); err != nil {
			return Config{}, err
		}
	default:
		return Config{}, errors.New("ELITEA_RUNTIME_AGENT_EXECUTION_DISPATCH_ENABLED must be true or false")
	}
	interruptsAPIEnabled, _ := lookup("ELITEA_RUNTIME_EXECUTION_INTERRUPTS_API_ENABLED")
	switch interruptsAPIEnabled {
	case "", "false":
	case "true":
		config.ExecutionInterruptsAPIEnabled = true
	default:
		return Config{}, errors.New("ELITEA_RUNTIME_EXECUTION_INTERRUPTS_API_ENABLED must be true or false")
	}
	discoveryEnabled, _ := lookup("ELITEA_RUNTIME_TOOLKIT_DISCOVERY_ENABLED")
	switch discoveryEnabled {
	case "", "false":
	case "true":
		config.ToolkitDiscoveryEnabled = true
	default:
		return Config{}, errors.New("invalid toolkit discovery activation flag")
	}
	if config.ToolkitDiscoveryEnabled && !config.IndexIngestDispatchEnabled && !config.AgentExecutionDispatchEnabled {
		return Config{}, errors.New("toolkit discovery requires an active worker dispatch")
	}
	indexSchedulingEnabled, _ := lookup("ELITEA_RUNTIME_INDEX_SCHEDULING_ENABLED")
	switch indexSchedulingEnabled {
	case "", "false":
		if value, ok := lookup("ELITEA_RUNTIME_SCHEDULER_INSTANCE_ID"); ok &&
			value != "" {
			return Config{}, errors.New(
				"runtime scheduler instance ID requires explicit index scheduling",
			)
		}
	case "true":
		if !config.IndexIngestDispatchEnabled {
			return Config{}, errors.New(
				"runtime index scheduling requires index ingest dispatch",
			)
		}
		config.IndexSchedulingEnabled = true
		if config.SchedulerInstanceID, err = required(
			"ELITEA_RUNTIME_SCHEDULER_INSTANCE_ID",
		); err != nil {
			return Config{}, err
		}
	default:
		return Config{}, errors.New(
			"ELITEA_RUNTIME_INDEX_SCHEDULING_ENABLED must be true or false",
		)
	}
	if config.NATSURL, err = required(runtimeNATSURLEnv); err != nil {
		return Config{}, err
	}
	if config.NATSMaterial, err = natsconn.FromEnv(runtimeNATSPrefix, func(name string) (string, bool) { return lookup(name) }); err != nil {
		return Config{}, err
	}
	if config.VectorIntrospectionClients, err = vectorIntrospectionClients(lookup); err != nil {
		return Config{}, err
	}
	if config.EgressAuditClients, err = egressAuditClients(lookup); err != nil {
		return Config{}, err
	}
	if config.EgressAuditRetention, err = egressAuditRetention(lookup); err != nil {
		return Config{}, err
	}
	if raw, ok := lookup("ELITEA_RUNTIME_SANDBOX_AUDIENCES"); ok && raw != "" {
		if len(raw) > 16*257 {
			return Config{}, errors.New("runtime sandbox audiences exceed the configuration bound")
		}
		config.SandboxAudiences = strings.Split(raw, ",")
	}
	if config.RustCompiledSnapshots, err = compiledSnapshotConfigFromEnv(lookup, config.Enabled, config.AgentExecutionDispatchEnabled, config.SandboxAudiences); err != nil {
		return Config{}, err
	}
	if config.CodeOwnerRecovery, err = codeOwnerConfigFromEnv(lookup, config.Enabled, config.AgentExecutionDispatchEnabled, config.SandboxAudiences); err != nil {
		return Config{}, err
	}
	if config.CodeWorkspace, config.CodePlatform, config.CodeDebugArtifacts, err = codeConsumerConfigFromEnv(lookup, config.CodeOwnerRecovery); err != nil {
		return Config{}, err
	}

	if config.SigningKeyID, err = required("ELITEA_RUNTIME_SIGNING_KEY_ID"); err != nil {
		return Config{}, err
	}
	if config.SigningKeyFile, err = required("ELITEA_RUNTIME_SIGNING_KEY_FILE"); err != nil {
		return Config{}, err
	}
	if config.VerificationKeyringFile, err = required("ELITEA_RUNTIME_VERIFICATION_KEYRING_FILE"); err != nil {
		return Config{}, err
	}
	if config.ControlAddress, err = required("ELITEA_RUNTIME_CONTROL_ADDRESS"); err != nil {
		return Config{}, err
	}
	if config.OutputAddress, err = required("ELITEA_RUNTIME_OUTPUT_ADDRESS"); err != nil {
		return Config{}, err
	}
	if config.ContentAddress, err = required("ELITEA_RUNTIME_CONTENT_ADDRESS"); err != nil {
		return Config{}, err
	}

	loadTLSFiles := func(prefix string) (runtimegrpc.ServerTLSFiles, error) {
		certificate, err := required(prefix + "_CERT_FILE")
		if err != nil {
			return runtimegrpc.ServerTLSFiles{}, err
		}
		privateKey, err := required(prefix + "_KEY_FILE")
		if err != nil {
			return runtimegrpc.ServerTLSFiles{}, err
		}
		clientCA, err := required(prefix + "_CLIENT_CA_FILE")
		if err != nil {
			return runtimegrpc.ServerTLSFiles{}, err
		}
		return runtimegrpc.ServerTLSFiles{CertificateChainPath: certificate, PrivateKeyPath: privateKey, ClientCAPath: clientCA}, nil
	}
	if config.ControlTLS, err = loadTLSFiles("ELITEA_RUNTIME_CONTROL_TLS"); err != nil {
		return Config{}, err
	}
	if config.OutputTLS, err = loadTLSFiles("ELITEA_RUNTIME_OUTPUT_TLS"); err != nil {
		return Config{}, err
	}
	if config.ContentTLS, err = loadTLSFiles("ELITEA_RUNTIME_CONTENT_TLS"); err != nil {
		return Config{}, err
	}
	if config.PlatformAddress, config.PlatformTLS, err = platformListenerFromEnv(lookup); err != nil {
		return Config{}, err
	}
	// The SSE stream limits are optional overrides of the built-in
	// process-local profile. executionapi reads the three
	// ELITEA_RUNTIME_SSE_* names itself, so they do not appear as literal
	// lookups here.
	if config.SSEStreamLimits, err = executionapi.SSEStreamLimitsFromEnv(lookup); err != nil {
		return Config{}, err
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

// RustIndexing reports that the deployment indexes at all, which since the
// Python worker's removal always means natively: index ingest dispatch is on,
// the index registry holds the metadata and elitea-vector holds the vectors.
func (c Config) RustIndexing() bool {
	return c.IndexIngestDispatchEnabled
}

func (c Config) Validate() error {
	if err := validateCodeOwnerConfig(c.CodeOwnerRecovery, c.Enabled, c.AgentExecutionDispatchEnabled, c.SandboxAudiences); err != nil {
		return err
	}
	if err := validateCodeConsumerConfig(c); err != nil {
		return err
	}
	if c.RustCompiledSnapshots != nil {
		if !c.Enabled || !c.AgentExecutionDispatchEnabled || len(c.SandboxAudiences) == 0 {
			return errors.New("compiled snapshots require active agent dispatch and exact audiences")
		}
		if err := c.RustCompiledSnapshots.Validate(); err != nil {
			return err
		}
	}
	if c.ToolkitDiscoveryEnabled && (!c.Enabled || !c.AgentExecutionDispatchEnabled) {
		return errors.New("toolkit discovery requires active agent execution dispatch")
	}
	if c.ExecutionInterruptsAPIEnabled && (!c.Enabled || !c.AgentExecutionDispatchEnabled) {
		return errors.New("execution interrupts API requires active agent execution dispatch")
	}
	if !c.Enabled {
		return nil
	}
	if c.CommandStream == "" || len(c.CommandStream) > 256 || strings.ContainsAny(c.CommandStream, " \r\n\x00") {
		return errors.New("runtime command stream is invalid")
	}
	if c.MaxOutstanding <= 0 || c.MaxOutstanding > maxRuntimeOutstanding {
		return errors.New("runtime durable admission capacity is invalid")
	}
	if err := commandbus.ValidateRoute(c.CommandStream, ""); err != nil {
		return fmt.Errorf("runtime configuration-validation stream: %w", err)
	}
	if c.IndexIngestDispatchEnabled {
		if err := commandbus.ValidateRoute(c.IndexIngestCommandStream, ""); err != nil {
			return fmt.Errorf("runtime index ingest command stream: %w", err)
		}
		if c.IndexIngestCommandStream == c.CommandStream {
			return errors.New("runtime index ingest requires a dedicated command stream")
		}
	} else if c.IndexIngestCommandStream != "" {
		return errors.New("runtime index ingest dispatch settings require explicit enablement")
	}
	if c.AgentExecutionDispatchEnabled {
		// Sharing the index stream is allowed (the standalone profile runs
		// index ingest on the agent stream): one stream, one durable, so one
		// worker fleet serves both capabilities.
		if err := commandbus.ValidateRoute(c.AgentExecutionCommandStream, ""); err != nil {
			return fmt.Errorf("runtime agent execution command stream: %w", err)
		}
		if c.AgentExecutionCommandStream == c.CommandStream {
			return errors.New("runtime agent execution cannot share the configuration-validation stream")
		}
	} else if c.AgentExecutionCommandStream != "" {
		return errors.New("runtime agent execution dispatch settings require explicit enablement")
	}
	if c.IndexSchedulingEnabled {
		if !c.IndexIngestDispatchEnabled {
			return errors.New("runtime index scheduling requires index ingest dispatch")
		}
		if !validSchedulerInstanceID(c.SchedulerInstanceID) {
			return errors.New("runtime scheduler instance ID is invalid")
		}
	} else if c.SchedulerInstanceID != "" {
		return errors.New(
			"runtime scheduler instance ID requires explicit index scheduling",
		)
	}
	if c.NATSURL == "" || len(c.NATSURL) > maxNATSURLBytes || strings.ContainsAny(c.NATSURL, " \r\n\t\x00") {
		return errors.New("runtime NATS URL is invalid")
	}
	if err := c.NATSMaterial.CheckURL(c.NATSURL); err != nil {
		return fmt.Errorf("%s: %w", runtimeNATSURLEnv, err)
	}
	if c.NATSMaterial.Enabled() {
		for _, path := range []string{c.NATSMaterial.CAFile, c.NATSMaterial.CertFile, c.NATSMaterial.KeyFile} {
			if !validConfigPath(path) {
				return errors.New("runtime NATS TLS file path is invalid")
			}
		}
	}
	for _, path := range []string{
		c.SigningKeyFile, c.VerificationKeyringFile,
		c.ControlTLS.CertificateChainPath, c.ControlTLS.PrivateKeyPath, c.ControlTLS.ClientCAPath,
		c.OutputTLS.CertificateChainPath, c.OutputTLS.PrivateKeyPath, c.OutputTLS.ClientCAPath,
		c.ContentTLS.CertificateChainPath, c.ContentTLS.PrivateKeyPath, c.ContentTLS.ClientCAPath,
	} {
		if !validConfigPath(path) {
			return errors.New("runtime secret or TLS file path is invalid")
		}
	}
	if c.SigningKeyID == "" || len(c.SigningKeyID) > 256 || strings.ContainsAny(c.SigningKeyID, "\r\n\x00") {
		return errors.New("runtime signing key ID is invalid")
	}
	if err := validateSandboxAudiences(c.SandboxAudiences); err != nil {
		return err
	}
	addresses := []string{c.ControlAddress, c.OutputAddress, c.ContentAddress}
	if c.PlatformAddress != "" {
		addresses = append(addresses, c.PlatformAddress)
		for _, path := range []string{
			c.PlatformTLS.CertificateChainPath, c.PlatformTLS.PrivateKeyPath, c.PlatformTLS.ClientCAPath,
		} {
			if !validConfigPath(path) {
				return errors.New("runtime platform listener TLS file path is invalid")
			}
		}
	}
	seen := make(map[string]struct{}, len(addresses))
	for _, address := range addresses {
		if _, repeated := seen[address]; repeated {
			return errors.New("runtime listeners require distinct addresses")
		}
		seen[address] = struct{}{}
	}
	for _, address := range addresses {
		if err := validateTCPAddress(address); err != nil {
			return err
		}
	}
	return nil
}

// publicListenPort is the port `cmd/elitea-main` serves on. The variable, its
// default and its rules are `cmd/elitea-main/http_address.go`'s; this restates
// the PORT half because `runtimecomposition` cannot import package main, and
// the platform listener must not share it.
func publicListenPort(lookup LookupEnv) (string, error) {
	address, ok := lookup("ELITEA_HTTP_ADDRESS")
	if !ok || address == "" {
		address = ":8080"
	}
	if len(address) > 256 || strings.ContainsAny(address, "\r\n\x00") {
		return "", errors.New("ELITEA_HTTP_ADDRESS is invalid")
	}
	_, value, err := net.SplitHostPort(address)
	if err != nil {
		return "", errors.New("ELITEA_HTTP_ADDRESS must include a numeric TCP port")
	}
	port, err := strconv.Atoi(value)
	if err != nil || port <= 0 || port > 65535 || strconv.Itoa(port) != value {
		return "", errors.New("ELITEA_HTTP_ADDRESS must include a numeric TCP port")
	}
	return value, nil
}

func validSchedulerInstanceID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for position := range len(value) {
		character := value[position]
		alphaNumeric := (character >= 'a' && character <= 'z') ||
			(character >= '0' && character <= '9')
		if position == 0 {
			if !alphaNumeric {
				return false
			}
			continue
		}
		if !alphaNumeric && character != '.' &&
			character != '_' && character != '-' {
			return false
		}
	}
	return true
}

func validConfigPath(path string) bool {
	return path != "" && len(path) <= maxConfigPathBytes && !strings.ContainsAny(path, "\r\n\x00")
}

func validPrivateConfigPath(path string) bool {
	return validConfigPath(path) && filepath.IsAbs(path) && filepath.Clean(path) == path
}

func validateTCPAddress(address string) error {
	if len(address) > 256 || strings.ContainsAny(address, "\r\n\x00") {
		return errors.New("runtime listener address is invalid")
	}
	_, portValue, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("runtime listener address must include a numeric TCP port")
	}
	port, err := strconv.ParseUint(portValue, 10, 16)
	if err != nil || port == 0 {
		return errors.New("runtime listener address must include a numeric TCP port")
	}
	return nil
}

func validateSandboxAudiences(audiences []string) error {
	if len(audiences) > 16 {
		return errors.New("runtime sandbox audiences exceed the configuration bound")
	}
	seen := make(map[string]struct{}, len(audiences))
	for _, audience := range audiences {
		if audience == "" || len(audience) > 256 || strings.ContainsAny(audience, " ,*\r\n\t\x00") {
			return errors.New("runtime sandbox audience must be an exact bounded identity")
		}
		if _, exists := seen[audience]; exists {
			return errors.New("runtime sandbox audience is duplicated")
		}
		seen[audience] = struct{}{}
	}
	return nil
}
