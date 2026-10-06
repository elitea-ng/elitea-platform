package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/security/securefile"
)

func codeMaterialDeployment(t *testing.T) deployment {
	t.Helper()
	fixture := newMaterialFixture(t)
	fixture.keys["owner-client.crt"] = fixture.clientChain
	fixture.keys["owner-client.key"] = fixture.clientKeyPEM
	fixture.keys["code-platform-content-keys.json"] = []byte(`{"revision":1,"current_key_id":"a","keys":[{"id":"a","key_base64url":"AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"}]}`)
	fixture.keys["agent-checkpoint-connection"] = []byte("postgres://fixture-only\n")
	pod := newDeployment(t, fixture, secretVolumeMode)
	pod.environment["ELITEA_RUNTIME_AGENT_EXECUTION_DISPATCH_ENABLED"] = "true"
	pod.environment["ELITEA_RUNTIME_CURRENT_MAIN_BASE_URL"] = "https://main.invalid"
	pod.environment["ELITEA_RUNTIME_AGENT_EXECUTION_COMMAND_STREAM"] = "commands.v1.agent.execute.agents.shared.1.0"
	pod.environment["ELITEA_RUNTIME_AGENT_EXECUTION_CONSUMER_GROUP"] = "elitea-agent-worker-v1"
	pod.environment["ELITEA_RUNTIME_AGENT_EXECUTION_STREAM_MAX_ENTRIES"] = "64"
	pod.environment["ELITEA_RUNTIME_SANDBOX_AUDIENCES"] = "spiffe://elitea/supervisor/one"
	pod.environment["ELITEA_RUNTIME_CODE_OWNER_RECOVERY_ENABLED"] = "true"
	pod.environment["ELITEA_RUNTIME_CODE_PLATFORM_ENABLED"] = "true"
	pod.environment["ELITEA_RUNTIME_CODE_DEBUG_ARTIFACTS_ENABLED"] = "true"
	pod.environment[runtimecomposition.CodeDebugAgentStateDSNFileEnv] = pod.path("agent-checkpoint-connection")
	owner := runtimecomposition.CodeOwnerConfig{MainWorkloadIdentity: "spiffe://elitea/main", CertificateChainPath: pod.path("owner-client.crt"), PrivateKeyPath: pod.path("owner-client.key"), ServerCAPath: pod.path(caName), Supervisors: []runtimecomposition.CodeOwnerSupervisor{{Audience: "spiffe://elitea/supervisor/one", HTTPSOrigin: "https://supervisor.invalid"}}}
	platform := runtimecomposition.CodePlatformDeploymentConfig{Revision: 1, ContentKeysFile: pod.path("code-platform-content-keys.json"), BrokerPolicies: []storage.CodeBrokerPolicy{{Revision: 1, MaxCalls: 32, MaxTotalBytes: 1048576}}}
	for name, value := range map[string]any{"ELITEA_RUNTIME_CODE_OWNER_RECOVERY_CONFIG": owner, "ELITEA_RUNTIME_CODE_PLATFORM_CONFIG": platform} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		pod.environment[name] = string(raw)
	}
	return pod
}

func TestInstallCopiesCodeOwnerBrokerAndDebugPrivateMaterial(t *testing.T) {
	pod := codeMaterialDeployment(t)
	if _, err := install(pod.source, pod.lookup); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"owner-client.key", "code-platform-content-keys.json", "agent-checkpoint-connection"} {
		contents, err := securefile.Read(pod.path(name), 1<<20, securefile.PrivateMaterial)
		if err != nil {
			t.Fatal("Code private material was not installed with its owning profile", name, err)
		}
		clear(contents)
	}
	config := runtimecomposition.CodePlatformConfig{Enabled: true, ContentKeysFile: pod.path("code-platform-content-keys.json")}
	if _, err := config.LoadContentKeys(); err != nil {
		t.Fatal("installed private broker content keys cannot be parsed", err)
	}
}

func TestInstallRefusesEachMissingCodeMaterialFile(t *testing.T) {
	for _, name := range []string{"owner-client.crt", "owner-client.key", "code-platform-content-keys.json", "agent-checkpoint-connection"} {
		t.Run(name, func(t *testing.T) {
			pod := codeMaterialDeployment(t)
			removeSecretKey(t, pod, name)
			if _, err := install(pod.source, pod.lookup); err == nil || !strings.Contains(err.Error(), name) {
				t.Fatal("missing Code material was accepted or its safe name was omitted", err)
			}
		})
	}
}
