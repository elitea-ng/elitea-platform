package runtimecomposition

import (
	"strings"
	"testing"

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
	if !config.Enabled || config.RedisPoolSize != 8 || config.MaxOutstanding != 128 || config.StreamMaxEntries != 256 {
		t.Fatalf("unexpected runtime config: %+v", config)
	}
}

func TestConfigIntegerConversionsRespectProtocolBounds(t *testing.T) {
	environment := validEnvironment()
	environment["ELITEA_RUNTIME_REDIS_POOL_SIZE"] = "64"
	if _, err := ConfigFromEnv(mapLookup(environment)); err != nil {
		t.Fatalf("maximum Redis pool size rejected: %v", err)
	}

	environment["ELITEA_RUNTIME_REDIS_POOL_SIZE"] = "65"
	if _, err := ConfigFromEnv(mapLookup(environment)); err == nil ||
		!strings.Contains(err.Error(), "runtime Redis pool size is invalid") {
		t.Fatalf("out-of-range Redis pool size error = %v", err)
	}

	if err := validateRedisURL("rediss://runtime@redis.internal:65535/0"); err != nil {
		t.Fatalf("maximum Redis TCP port rejected: %v", err)
	}
	if err := validateRedisURL("rediss://runtime@redis.internal:65536/0"); err == nil {
		t.Fatal("out-of-range Redis TCP port was accepted")
	}
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
	environment["ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM"] = "commands.v1.index.ingest.indexing.shared.1.0"
	environment["ELITEA_RUNTIME_INDEX_INGEST_CONSUMER_GROUP"] = "elitea-indexer-worker-v1"
	environment["ELITEA_RUNTIME_INDEX_INGEST_STREAM_MAX_ENTRIES"] = "64"
	config, err := ConfigFromEnv(mapLookup(environment))
	if err != nil {
		t.Fatal(err)
	}
	if !config.IndexIngestDispatchEnabled || config.IndexIngestCommandStream != environment["ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM"] || config.IndexIngestConsumerGroup != environment["ELITEA_RUNTIME_INDEX_INGEST_CONSUMER_GROUP"] || config.IndexIngestStreamMaxEntries != 64 {
		t.Fatalf("unexpected index ingest dispatch config: %+v", config)
	}

	environment["ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM"] = environment["ELITEA_RUNTIME_COMMAND_STREAM"]
	if _, err := ConfigFromEnv(mapLookup(environment)); err == nil || !strings.Contains(err.Error(), "dedicated") {
		t.Fatalf("shared runtime stream was accepted: %v", err)
	}

	for _, alias := range []struct {
		name       string
		validation string
		index      string
	}{
		{
			name:       "index aliases validation delivery index",
			validation: "commands.validation",
			index:      "commands.validation:delivery-index.v1",
		},
		{
			name:       "validation aliases index delivery index",
			validation: "commands.index:delivery-index.v1",
			index:      "commands.index",
		},
	} {
		t.Run(alias.name, func(t *testing.T) {
			environment := validEnvironment()
			environment["ELITEA_RUNTIME_INDEX_INGEST_DISPATCH_ENABLED"] = "true"
			environment["ELITEA_RUNTIME_COMMAND_STREAM"] = alias.validation
			environment["ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM"] = alias.index
			environment["ELITEA_RUNTIME_INDEX_INGEST_CONSUMER_GROUP"] = "elitea-indexer-worker-v1"
			environment["ELITEA_RUNTIME_INDEX_INGEST_STREAM_MAX_ENTRIES"] = "64"
			if _, err := ConfigFromEnv(mapLookup(environment)); err == nil || !strings.Contains(err.Error(), "dedicated") {
				t.Fatalf("derived Redis key alias was accepted: %v", err)
			}
		})
	}
}

func TestConfigIndexIngestDispatchFailsClosed(t *testing.T) {
	tests := []struct {
		name  string
		apply func(map[string]string)
	}{
		{
			name: "missing group",
			apply: func(values map[string]string) {
				delete(values, "ELITEA_RUNTIME_INDEX_INGEST_CONSUMER_GROUP")
			},
		},
		{
			name: "non canonical capacity",
			apply: func(values map[string]string) {
				values["ELITEA_RUNTIME_INDEX_INGEST_STREAM_MAX_ENTRIES"] = "064"
			},
		},
		{
			name: "capacity above bound",
			apply: func(values map[string]string) {
				values["ELITEA_RUNTIME_INDEX_INGEST_STREAM_MAX_ENTRIES"] = "1025"
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
			environment["ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM"] = "commands.v1.index.ingest.indexing.shared.1.0"
			environment["ELITEA_RUNTIME_INDEX_INGEST_CONSUMER_GROUP"] = "elitea-indexer-worker-v1"
			environment["ELITEA_RUNTIME_INDEX_INGEST_STREAM_MAX_ENTRIES"] = "64"
			test.apply(environment)
			if _, err := ConfigFromEnv(mapLookup(environment)); err == nil {
				t.Fatal("invalid index ingest dispatch config was accepted")
			}
		})
	}
}

func TestConfigAgentExecutionDispatchIsOptionalWorkerPooledAndBounded(t *testing.T) {
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
	environment["ELITEA_RUNTIME_AGENT_EXECUTION_COMMAND_STREAM"] = "commands.v1.agent.execute.agents.shared.1.0"
	environment["ELITEA_RUNTIME_AGENT_EXECUTION_CONSUMER_GROUP"] = "elitea-agent-worker-v1"
	environment["ELITEA_RUNTIME_AGENT_EXECUTION_STREAM_MAX_ENTRIES"] = "64"
	config, err := ConfigFromEnv(mapLookup(environment))
	if err != nil {
		t.Fatal(err)
	}
	if !config.AgentExecutionDispatchEnabled ||
		config.AgentExecutionCommandStream != environment["ELITEA_RUNTIME_AGENT_EXECUTION_COMMAND_STREAM"] ||
		config.AgentExecutionConsumerGroup != "elitea-agent-worker-v1" ||
		config.AgentExecutionStreamMaxEntries != 64 {
		t.Fatalf("unexpected agent execution config: %+v", config)
	}

	environment["ELITEA_RUNTIME_AGENT_EXECUTION_COMMAND_STREAM"] = environment["ELITEA_RUNTIME_COMMAND_STREAM"]
	if _, err := ConfigFromEnv(mapLookup(environment)); err == nil || !strings.Contains(err.Error(), "configuration-validation") {
		t.Fatalf("shared validation/agent stream was accepted: %v", err)
	}

	environment = validEnvironment()
	environment["ELITEA_RUNTIME_INDEX_INGEST_DISPATCH_ENABLED"] = "true"
	environment["ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM"] = "commands.v1.index.ingest.indexing.shared.2.0"
	environment["ELITEA_RUNTIME_INDEX_INGEST_CONSUMER_GROUP"] = "elitea-indexer-worker-v2"
	environment["ELITEA_RUNTIME_INDEX_INGEST_STREAM_MAX_ENTRIES"] = "64"
	environment["ELITEA_RUNTIME_AGENT_EXECUTION_DISPATCH_ENABLED"] = "true"
	environment["ELITEA_RUNTIME_CURRENT_MAIN_BASE_URL"] = "https://elitea-gateway"
	environment["ELITEA_RUNTIME_AGENT_EXECUTION_COMMAND_STREAM"] = environment["ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM"]
	environment["ELITEA_RUNTIME_AGENT_EXECUTION_CONSUMER_GROUP"] = environment["ELITEA_RUNTIME_INDEX_INGEST_CONSUMER_GROUP"]
	environment["ELITEA_RUNTIME_AGENT_EXECUTION_STREAM_MAX_ENTRIES"] = "64"
	if _, err := ConfigFromEnv(mapLookup(environment)); err != nil {
		t.Fatalf("shared index/agent worker pool was rejected: %v", err)
	}
	environment["ELITEA_RUNTIME_AGENT_EXECUTION_CONSUMER_GROUP"] = "different-worker-pool"
	if _, err := ConfigFromEnv(mapLookup(environment)); err == nil || !strings.Contains(err.Error(), "consumer group") {
		t.Fatalf("one stream with competing capability groups was accepted: %v", err)
	}

	environment = validEnvironment()
	environment["ELITEA_RUNTIME_AGENT_EXECUTION_DISPATCH_ENABLED"] = "true"
	environment["ELITEA_RUNTIME_CURRENT_MAIN_BASE_URL"] = "https://elitea-gateway"
	environment["ELITEA_RUNTIME_AGENT_EXECUTION_COMMAND_STREAM"] = "commands.v1.agent.execute.agents.shared.1.0"
	environment["ELITEA_RUNTIME_AGENT_EXECUTION_CONSUMER_GROUP"] = "elitea-agent-worker-v1"
	environment["ELITEA_RUNTIME_AGENT_EXECUTION_STREAM_MAX_ENTRIES"] = "1025"
	if _, err := ConfigFromEnv(mapLookup(environment)); err == nil {
		t.Fatal("unbounded agent execution stream capacity was accepted")
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
			environment["ELITEA_RUNTIME_AGENT_EXECUTION_COMMAND_STREAM"] = "commands.v1.agent.execute.agents.shared.1.0"
			environment["ELITEA_RUNTIME_AGENT_EXECUTION_CONSUMER_GROUP"] = "elitea-agent-worker-v1"
			environment["ELITEA_RUNTIME_AGENT_EXECUTION_STREAM_MAX_ENTRIES"] = "64"
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
	enabled["ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM"] =
		"commands.v1.index.ingest.indexing.shared.2.0"
	enabled["ELITEA_RUNTIME_INDEX_INGEST_CONSUMER_GROUP"] =
		"elitea-indexer-worker-v2"
	enabled["ELITEA_RUNTIME_INDEX_INGEST_STREAM_MAX_ENTRIES"] = "64"
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
			environment["ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM"] =
				"commands.v1.index.ingest.indexing.shared.2.0"
			environment["ELITEA_RUNTIME_INDEX_INGEST_CONSUMER_GROUP"] =
				"elitea-indexer-worker-v2"
			environment["ELITEA_RUNTIME_INDEX_INGEST_STREAM_MAX_ENTRIES"] = "64"
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

func TestIndexIngestProductionRedisEntryBoundIsStrictlyBelow64KiB(t *testing.T) {
	if productionIndexRedisEntrySize >= 64*1024 {
		t.Fatalf("index ingest Redis entry bound=%d, must be below 64 KiB", productionIndexRedisEntrySize)
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

func TestConfigRedisURLContract(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{name: "plaintext", url: "redis://runtime@redis.internal:6379/0"},
		{name: "missing username", url: "rediss://redis.internal:6379/0"},
		{name: "password in URL", url: "rediss://runtime:secret@redis.internal:6379/0"},
		{name: "query", url: "rediss://runtime@redis.internal:6379/0?protocol=2"},
		{name: "fragment", url: "rediss://runtime@redis.internal:6379/0#fragment"},
		{name: "missing port", url: "rediss://runtime@redis.internal/0"},
		{name: "leading-zero port", url: "rediss://runtime@redis.internal:06379/0"},
		{name: "missing database path", url: "rediss://runtime@redis.internal:6379"},
		{name: "nonzero database", url: "rediss://runtime@redis.internal:6379/1"},
		{name: "noncanonical database", url: "rediss://runtime@redis.internal:6379/01"},
		{name: "encoded username", url: "rediss://runtime%2Dworker@redis.internal:6379/0"},
		{name: "unsupported username character", url: "rediss://runtime+worker@redis.internal:6379/0"},
		{name: "unicode username", url: "rediss://runtimé@redis.internal:6379/0"},
		{name: "unicode host", url: "rediss://runtime@rédis.internal:6379/0"},
		{name: "leading whitespace", url: " rediss://runtime@redis.internal:6379/0"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			environment := validEnvironment()
			environment["ELITEA_RUNTIME_REDIS_URL"] = test.url
			if _, err := ConfigFromEnv(mapLookup(environment)); err == nil {
				t.Fatalf("invalid Redis URL %q was accepted", test.url)
			}
		})
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
		"ELITEA_RUNTIME_COMMAND_STREAM":            "commands.v1.configuration.validate.v1.validation-small.shared-claim-scoped-authority.1.0",
		"ELITEA_RUNTIME_MAX_OUTSTANDING":           "128",
		"ELITEA_RUNTIME_STREAM_MAX_ENTRIES":        "256",
		"ELITEA_RUNTIME_REDIS_URL":                 "rediss://runtime@redis.internal:6379/0",
		"ELITEA_RUNTIME_REDIS_PASSWORD_FILE":       "/run/secrets/runtime-redis-password",
		"ELITEA_RUNTIME_REDIS_CA_FILE":             "/run/secrets/runtime-redis-ca.pem",
		"ELITEA_RUNTIME_REDIS_POOL_SIZE":           "8",
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
