package runtimecomposition

import (
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/libs/go/natsconn"

	executionapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/executions"
)

func TestConfigFromEnvIsDisabledByDefaultAndFailClosedWhenEnabled(t *testing.T) {
	disabled, err := ConfigFromEnv(mapLookup(map[string]string{}))
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Enabled {
		t.Fatal("runtime unexpectedly enabled")
	}

	_, err = ConfigFromEnv(mapLookup(map[string]string{"ELITEA_RUNTIME_ENABLED": "true"}))
	if err == nil || !strings.Contains(err.Error(), "ELITEA_RUNTIME_COMMAND_STREAM") {
		t.Fatalf("missing enabled config error = %v", err)
	}

	_, err = ConfigFromEnv(mapLookup(map[string]string{"ELITEA_RUNTIME_ENABLED": "yes"}))
	if err == nil {
		t.Fatal("non-boolean runtime enable value was accepted")
	}
}

func TestConfigFromEnvAcceptsCompleteBoundedProductionConfig(t *testing.T) {
	config, err := ConfigFromEnv(mapLookup(validEnvironment()))
	if err != nil {
		t.Fatal(err)
	}
	if !config.Enabled || config.MaxOutstanding != 128 || config.NATSURL != "tls://elitea-nats:4222" ||
		!config.NATSMaterial.Enabled() || config.CommandStream != "ELITEA_RT_V1_VALIDATE" {
		t.Fatalf("unexpected runtime config: %+v", config)
	}
}

func TestConfigIntegerConversionsRespectProtocolBounds(t *testing.T) {
	if err := validateTCPAddress(":65535"); err != nil {
		t.Fatalf("maximum listener TCP port rejected: %v", err)
	}
	if err := validateTCPAddress(":65536"); err == nil {
		t.Fatal("out-of-range listener TCP port was accepted")
	}
}

func TestConfigIndexIngestDispatchIsOptionalAndDedicated(t *testing.T) {
	baseline, err := ConfigFromEnv(mapLookup(validEnvironment()))
	if err != nil {
		t.Fatal(err)
	}
	if baseline.IndexIngestDispatchEnabled {
		t.Fatal("index ingest dispatch unexpectedly enabled")
	}

	environment := validEnvironment()
	environment["ELITEA_RUNTIME_INDEX_INGEST_DISPATCH_ENABLED"] = "true"
	environment["ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM"] = "ELITEA_RT_V1_INDEX"
	config, err := ConfigFromEnv(mapLookup(environment))
	if err != nil {
		t.Fatal(err)
	}
	if !config.IndexIngestDispatchEnabled || config.IndexIngestCommandStream != "ELITEA_RT_V1_INDEX" ||
		consumerFor(config.IndexIngestCommandStream) != "elitea-index-worker-v1" {
		t.Fatalf("unexpected index ingest dispatch config: %+v", config)
	}

	environment["ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM"] = environment["ELITEA_RUNTIME_COMMAND_STREAM"]
	if _, err := ConfigFromEnv(mapLookup(environment)); err == nil || !strings.Contains(err.Error(), "dedicated") {
		t.Fatalf("shared runtime stream was accepted: %v", err)
	}
}

func TestConfigIndexIngestDispatchFailsClosed(t *testing.T) {
	tests := []struct {
		name  string
		apply func(map[string]string)
	}{
		{
			name: "a stream outside the contract",
			apply: func(values map[string]string) {
				values["ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM"] = "commands.v1.index.ingest.indexing.shared.2.0"
			},
		},
		{
			name: "a stream with no permission row",
			apply: func(values map[string]string) {
				values["ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM"] = "ELITEA_RT_V1_INDEX2"
			},
		},
		{
			name: "invalid enable switch",
			apply: func(values map[string]string) {
				values["ELITEA_RUNTIME_INDEX_INGEST_DISPATCH_ENABLED"] = "yes"
			},
		},
		{
			name: "settings without enablement",
			apply: func(values map[string]string) {
				values["ELITEA_RUNTIME_INDEX_INGEST_DISPATCH_ENABLED"] = "false"
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			environment := validEnvironment()
			environment["ELITEA_RUNTIME_INDEX_INGEST_DISPATCH_ENABLED"] = "true"
			environment["ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM"] = "ELITEA_RT_V1_INDEX"
			test.apply(environment)
			if _, err := ConfigFromEnv(mapLookup(environment)); err == nil {
				t.Fatal("invalid index ingest dispatch config was accepted")
			}
		})
	}
}

func TestConfigAgentExecutionDispatchIsOptionalAndMayShareTheIndexStream(t *testing.T) {
	baseline, err := ConfigFromEnv(mapLookup(validEnvironment()))
	if err != nil {
		t.Fatal(err)
	}
	if baseline.AgentExecutionDispatchEnabled {
		t.Fatal("agent execution dispatch unexpectedly enabled")
	}

	environment := validEnvironment()
	environment["ELITEA_RUNTIME_AGENT_EXECUTION_DISPATCH_ENABLED"] = "true"
	environment["ELITEA_RUNTIME_CURRENT_MAIN_BASE_URL"] = "https://elitea-gateway"
	environment["ELITEA_RUNTIME_AGENT_EXECUTION_COMMAND_STREAM"] = "ELITEA_RT_V1_AGENT"
	config, err := ConfigFromEnv(mapLookup(environment))
	if err != nil {
		t.Fatal(err)
	}
	if !config.AgentExecutionDispatchEnabled || config.AgentExecutionCommandStream != "ELITEA_RT_V1_AGENT" ||
		consumerFor(config.AgentExecutionCommandStream) != "elitea-agent-worker-v1" {
		t.Fatalf("unexpected agent execution config: %+v", config)
	}

	environment["ELITEA_RUNTIME_AGENT_EXECUTION_COMMAND_STREAM"] = environment["ELITEA_RUNTIME_COMMAND_STREAM"]
	if _, err := ConfigFromEnv(mapLookup(environment)); err == nil || !strings.Contains(err.Error(), "configuration-validation") {
		t.Fatalf("shared validation/agent stream was accepted: %v", err)
	}

	// The standalone profile: index ingest on the agent stream, one durable.
	environment = validEnvironment()
	environment["ELITEA_RUNTIME_INDEX_INGEST_DISPATCH_ENABLED"] = "true"
	environment["ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM"] = "ELITEA_RT_V1_AGENT"
	environment["ELITEA_RUNTIME_AGENT_EXECUTION_DISPATCH_ENABLED"] = "true"
	environment["ELITEA_RUNTIME_CURRENT_MAIN_BASE_URL"] = "https://elitea-gateway"
	environment["ELITEA_RUNTIME_AGENT_EXECUTION_COMMAND_STREAM"] = "ELITEA_RT_V1_AGENT"
	if _, err := ConfigFromEnv(mapLookup(environment)); err != nil {
		t.Fatalf("shared index/agent stream was rejected: %v", err)
	}
}

func TestConfigAgentExecutionCurrentMainOriginFailsClosed(t *testing.T) {
	for name, value := range map[string]string{
		"plaintext": "http://elitea-gateway",
		"path":      "https://elitea-gateway/api",
		"userinfo":  "https://user@elitea-gateway",
	} {
		t.Run(name, func(t *testing.T) {
			environment := validEnvironment()
			environment["ELITEA_RUNTIME_AGENT_EXECUTION_DISPATCH_ENABLED"] = "true"
			environment["ELITEA_RUNTIME_AGENT_EXECUTION_COMMAND_STREAM"] = "ELITEA_RT_V1_AGENT"
			environment["ELITEA_RUNTIME_CURRENT_MAIN_BASE_URL"] = value
			if _, err := ConfigFromEnv(mapLookup(environment)); err == nil {
				t.Fatalf("unsafe current Main origin accepted: %q", value)
			}
		})
	}

	environment := validEnvironment()
	environment["ELITEA_RUNTIME_CURRENT_MAIN_BASE_URL"] = "https://elitea-gateway"
	if _, err := ConfigFromEnv(mapLookup(environment)); err == nil ||
		!strings.Contains(err.Error(), "explicit enablement") {
		t.Fatalf("current Main origin without agent dispatch error=%v", err)
	}
}

func TestConfigIndexSchedulingIsExplicitAndRequiresDurableIndexAdmission(
	t *testing.T,
) {
	baseline, err := ConfigFromEnv(mapLookup(validEnvironment()))
	if err != nil {
		t.Fatal(err)
	}
	if baseline.IndexSchedulingEnabled {
		t.Fatal("index scheduling unexpectedly enabled")
	}

	withoutIndex := validEnvironment()
	withoutIndex["ELITEA_RUNTIME_INDEX_SCHEDULING_ENABLED"] = "true"
	if _, err := ConfigFromEnv(mapLookup(withoutIndex)); err == nil ||
		!strings.Contains(err.Error(), "requires index ingest dispatch") {
		t.Fatalf("schedule without durable admission error=%v", err)
	}

	enabled := validEnvironment()
	enabled["ELITEA_RUNTIME_INDEX_INGEST_DISPATCH_ENABLED"] = "true"
	enabled["ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM"] = "ELITEA_RT_V1_INDEX"
	enabled["ELITEA_RUNTIME_INDEX_SCHEDULING_ENABLED"] = "true"
	enabled["ELITEA_RUNTIME_SCHEDULER_INSTANCE_ID"] = "elitea-main-pov-1"
	config, err := ConfigFromEnv(mapLookup(enabled))
	if err != nil {
		t.Fatal(err)
	}
	if !config.IndexSchedulingEnabled ||
		config.SchedulerInstanceID != "elitea-main-pov-1" {
		t.Fatalf("index scheduling was not enabled: %+v", config)
	}

	enabled["ELITEA_RUNTIME_INDEX_SCHEDULING_ENABLED"] = "TRUE"
	if _, err := ConfigFromEnv(mapLookup(enabled)); err == nil ||
		!strings.Contains(
			err.Error(),
			"ELITEA_RUNTIME_INDEX_SCHEDULING_ENABLED must be true or false",
		) {
		t.Fatalf("non-canonical scheduling switch error=%v", err)
	}
}

func TestConfigIndexSchedulingRequiresCanonicalUniqueInstanceID(t *testing.T) {
	for _, instanceID := range []string{"", "Elitea-Main-1", "-main", "main/1"} {
		t.Run(instanceID, func(t *testing.T) {
			environment := validEnvironment()
			environment["ELITEA_RUNTIME_INDEX_INGEST_DISPATCH_ENABLED"] = "true"
			environment["ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM"] = "ELITEA_RT_V1_INDEX"
			environment["ELITEA_RUNTIME_INDEX_SCHEDULING_ENABLED"] = "true"
			environment["ELITEA_RUNTIME_SCHEDULER_INSTANCE_ID"] = instanceID
			if _, err := ConfigFromEnv(mapLookup(environment)); err == nil {
				t.Fatalf("invalid scheduler instance ID %q accepted", instanceID)
			}
		})
	}

	disabled := validEnvironment()
	disabled["ELITEA_RUNTIME_SCHEDULER_INSTANCE_ID"] = "elitea-main-1"
	if _, err := ConfigFromEnv(mapLookup(disabled)); err == nil ||
		!strings.Contains(err.Error(), "requires explicit index scheduling") {
		t.Fatalf("disabled scheduler instance error=%v", err)
	}
}

func TestConfigSSEStreamLimitsAreOptionalAndValidated(t *testing.T) {
	baseline, err := ConfigFromEnv(mapLookup(validEnvironment()))
	if err != nil {
		t.Fatal(err)
	}
	if baseline.SSEStreamLimits != executionapi.DefaultSSEStreamLimits() {
		t.Fatalf("default SSE stream limits = %+v", baseline.SSEStreamLimits)
	}

	environment := validEnvironment()
	environment["ELITEA_RUNTIME_SSE_MAX_STREAMS"] = "32"
	environment["ELITEA_RUNTIME_SSE_MAX_STREAMS_PER_PRINCIPAL"] = "8"
	environment["ELITEA_RUNTIME_SSE_MAX_STREAMS_PER_PROJECT"] = "16"
	config, err := ConfigFromEnv(mapLookup(environment))
	if err != nil {
		t.Fatal(err)
	}
	want := executionapi.SSEStreamLimits{MaxStreams: 32, MaxPerPrincipal: 8, MaxPerProject: 16}
	if config.SSEStreamLimits != want {
		t.Fatalf("SSE stream limits = %+v, want %+v", config.SSEStreamLimits, want)
	}
}

func TestConfigSSEStreamLimitsFailClosed(t *testing.T) {
	tests := []struct {
		name  string
		apply func(map[string]string)
		want  string
	}{
		{
			name: "zero global limit",
			apply: func(values map[string]string) {
				values["ELITEA_RUNTIME_SSE_MAX_STREAMS"] = "0"
			},
			want: "ELITEA_RUNTIME_SSE_MAX_STREAMS must be a canonical positive integer",
		},
		{
			name: "non-canonical global limit",
			apply: func(values map[string]string) {
				values["ELITEA_RUNTIME_SSE_MAX_STREAMS"] = "007"
			},
			want: "ELITEA_RUNTIME_SSE_MAX_STREAMS must be a canonical positive integer",
		},
		{
			name: "text principal limit",
			apply: func(values map[string]string) {
				values["ELITEA_RUNTIME_SSE_MAX_STREAMS_PER_PRINCIPAL"] = "abc"
			},
			want: "ELITEA_RUNTIME_SSE_MAX_STREAMS_PER_PRINCIPAL must be a canonical positive integer",
		},
		{
			name: "principal above global",
			apply: func(values map[string]string) {
				values["ELITEA_RUNTIME_SSE_MAX_STREAMS"] = "16"
				values["ELITEA_RUNTIME_SSE_MAX_STREAMS_PER_PRINCIPAL"] = "17"
			},
			want: "ELITEA_RUNTIME_SSE_MAX_STREAMS_PER_PRINCIPAL must not exceed ELITEA_RUNTIME_SSE_MAX_STREAMS",
		},
		{
			name: "project above global",
			apply: func(values map[string]string) {
				values["ELITEA_RUNTIME_SSE_MAX_STREAMS"] = "8"
				values["ELITEA_RUNTIME_SSE_MAX_STREAMS_PER_PROJECT"] = "16"
			},
			want: "ELITEA_RUNTIME_SSE_MAX_STREAMS_PER_PROJECT must not exceed ELITEA_RUNTIME_SSE_MAX_STREAMS",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			environment := validEnvironment()
			test.apply(environment)
			_, err := ConfigFromEnv(mapLookup(environment))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestConfigSSEStreamLimitsRequireExplicitEnablement(t *testing.T) {
	for _, name := range []string{
		"ELITEA_RUNTIME_SSE_MAX_STREAMS",
		"ELITEA_RUNTIME_SSE_MAX_STREAMS_PER_PRINCIPAL",
		"ELITEA_RUNTIME_SSE_MAX_STREAMS_PER_PROJECT",
	} {
		t.Run(name, func(t *testing.T) {
			environment := map[string]string{name: "32"}
			_, err := ConfigFromEnv(mapLookup(environment))
			if err == nil || !strings.Contains(err.Error(), "require explicit enablement") {
				t.Fatalf("%s without enablement error = %v", name, err)
			}
		})
	}
}

func TestIndexIngestProductionMessageBoundIsStrictlyBelow64KiB(t *testing.T) {
	if productionIndexTransportMessageBytes >= 64*1024 {
		t.Fatalf("index ingest message bound=%d, must be below 64 KiB", productionIndexTransportMessageBytes)
	}
}

func TestIndexIngestEmbeddingBindingUsesDedicatedCapabilityVersion(t *testing.T) {
	if capabilityVersion != "1" || indexCapabilityVersion != "2" {
		t.Fatalf(
			"capability versions: configuration=%q index=%q",
			capabilityVersion,
			indexCapabilityVersion,
		)
	}
}

func TestConfigNATSURLAndIdentityContract(t *testing.T) {
	tests := []struct {
		name  string
		apply func(map[string]string)
	}{
		{name: "missing URL", apply: func(v map[string]string) { delete(v, "ELITEA_RUNTIME_NATS_URL") }},
		{name: "nats:// beside client material", apply: func(v map[string]string) { v["ELITEA_RUNTIME_NATS_URL"] = "nats://elitea-nats:4222" }},
		{name: "credential in URL", apply: func(v map[string]string) { v["ELITEA_RUNTIME_NATS_URL"] = "tls://user:secret@elitea-nats:4222" }},
		{name: "token in URL", apply: func(v map[string]string) { v["ELITEA_RUNTIME_NATS_URL"] = "tls://token@elitea-nats:4222" }},
		{name: "half-configured TLS", apply: func(v map[string]string) { delete(v, "ELITEA_RUNTIME_NATS_TLS_KEY_FILE") }},
		{name: "tls:// without material", apply: func(v map[string]string) {
			delete(v, "ELITEA_RUNTIME_NATS_TLS_CA_FILE")
			delete(v, "ELITEA_RUNTIME_NATS_TLS_CERT_FILE")
			delete(v, "ELITEA_RUNTIME_NATS_TLS_KEY_FILE")
		}},
		{name: "whitespace", apply: func(v map[string]string) { v["ELITEA_RUNTIME_NATS_URL"] = "tls://elitea-nats:4222 " }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			environment := validEnvironment()
			test.apply(environment)
			if _, err := ConfigFromEnv(mapLookup(environment)); err == nil {
				t.Fatal("an invalid runtime NATS configuration was accepted")
			}
		})
	}

	// The compose posture: plaintext, no identity.
	environment := validEnvironment()
	environment["ELITEA_RUNTIME_NATS_URL"] = "nats://nats:4222"
	delete(environment, "ELITEA_RUNTIME_NATS_TLS_CA_FILE")
	delete(environment, "ELITEA_RUNTIME_NATS_TLS_CERT_FILE")
	delete(environment, "ELITEA_RUNTIME_NATS_TLS_KEY_FILE")
	config, err := ConfigFromEnv(mapLookup(environment))
	if err != nil || config.NATSMaterial.Enabled() {
		t.Fatalf("the plaintext compose posture: enabled=%t err=%v", config.NATSMaterial.Enabled(), err)
	}
}

// Every Redis-era variable is gone from the configuration surface: a stale
// deployment that still sets them gets no Redis client, and nothing in the
// composition reads them (TestNoRuntimeRedisLookups in nats_test.go).
func TestConfigNeedsNoRedis(t *testing.T) {
	for name := range validEnvironment() {
		if strings.Contains(name, "REDIS") || strings.Contains(name, "CONSUMER_GROUP") || strings.Contains(name, "STREAM_MAX_ENTRIES") {
			t.Errorf("the valid runtime environment still carries %s", name)
		}
	}
}

func mapLookup(values map[string]string) LookupEnv {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func validEnvironment() map[string]string {
	values := map[string]string{
		"ELITEA_RUNTIME_ENABLED":                   "true",
		"ELITEA_RUNTIME_COMMAND_STREAM":            "ELITEA_RT_V1_VALIDATE",
		"ELITEA_RUNTIME_MAX_OUTSTANDING":           "128",
		"ELITEA_RUNTIME_NATS_URL":                  "tls://elitea-nats:4222",
		"ELITEA_RUNTIME_NATS_TLS_CA_FILE":          "/etc/elitea/runtime-nats-client/ca.crt",
		"ELITEA_RUNTIME_NATS_TLS_CERT_FILE":        "/etc/elitea/runtime-nats-client/tls.crt",
		"ELITEA_RUNTIME_NATS_TLS_KEY_FILE":         "/etc/elitea/runtime-nats-client/tls.key",
		"ELITEA_RUNTIME_SIGNING_KEY_ID":            "runtime-key-2026-01",
		"ELITEA_RUNTIME_SIGNING_KEY_FILE":          "/run/secrets/runtime-signing-key.pem",
		"ELITEA_RUNTIME_VERIFICATION_KEYRING_FILE": "/run/config/runtime-signing-keyring.json",
		"ELITEA_RUNTIME_CONTROL_ADDRESS":           ":9443",
		"ELITEA_RUNTIME_OUTPUT_ADDRESS":            ":9444",
		"ELITEA_RUNTIME_CONTENT_ADDRESS":           ":9445",
	}
	for _, prefix := range []string{"CONTROL", "OUTPUT", "CONTENT"} {
		values["ELITEA_RUNTIME_"+prefix+"_TLS_CERT_FILE"] = "/run/secrets/" + strings.ToLower(prefix) + "-server.pem"
		values["ELITEA_RUNTIME_"+prefix+"_TLS_KEY_FILE"] = "/run/secrets/" + strings.ToLower(prefix) + "-server-key.pem"
		values["ELITEA_RUNTIME_"+prefix+"_TLS_CLIENT_CA_FILE"] = "/run/secrets/" + strings.ToLower(prefix) + "-client-ca.pem"
	}
	return values
}

func TestSandboxGrantAudiencesAreOptionalExactAndBounded(t *testing.T) {
	baseline, err := ConfigFromEnv(mapLookup(validEnvironment()))
	if err != nil || len(baseline.SandboxAudiences) != 0 {
		t.Fatalf("sandbox grants must remain disabled by default: %v", err)
	}
	for _, raw := range []string{
		"dns:sandbox.test", "dns:sandbox.test,spiffe://elitea.test/sandbox/rust",
	} {
		env := validEnvironment()
		env["ELITEA_RUNTIME_SANDBOX_AUDIENCES"] = raw
		config, err := ConfigFromEnv(mapLookup(env))
		if err != nil || strings.Join(config.SandboxAudiences, ",") != raw {
			t.Fatalf("exact audiences were not preserved: %v", err)
		}
	}
	for _, raw := range []string{
		"*", "dns:sandbox.test,", " dns:sandbox.test", "dns:sandbox.test,dns:sandbox.test",
		"dns:sandbox.test\n", strings.Repeat("x", 257), strings.Repeat("x,", 16) + "y",
	} {
		env := validEnvironment()
		env["ELITEA_RUNTIME_SANDBOX_AUDIENCES"] = raw
		if _, err := ConfigFromEnv(mapLookup(env)); err == nil {
			t.Fatalf("invalid sandbox audiences accepted: %q", raw)
		}
	}
}

func TestRuntimeNATSTLSNamesAreNatsconns(t *testing.T) {
	// The prefix is the one the NATS permission table's RUNTIME account
	// reserved for elitea-main-runtime.
	if runtimeNATSPrefix != natsconn.EnvPrefixRuntime {
		t.Fatalf("runtimeNATSPrefix = %q, natsconn.EnvPrefixRuntime = %q", runtimeNATSPrefix, natsconn.EnvPrefixRuntime)
	}
	if natsconn.EnvNames(runtimeNATSPrefix) != [3]string{runtimeNATSTLSCAFileEnv, runtimeNATSTLSCertFileEnv, runtimeNATSTLSKeyFileEnv} {
		t.Fatalf("natsconn reads %v", natsconn.EnvNames(runtimeNATSPrefix))
	}
}
