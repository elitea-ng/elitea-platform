package runtimecomposition

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

const discoveryFlag = "ELITEA_RUNTIME_TOOLKIT_DISCOVERY_ENABLED"

var composeDeployDir = filepath.Join("..", "..", "..", "..", "deploy")

// composeInterpolation matches ${NAME}, ${NAME-default} and ${NAME:-default}.
var composeInterpolation = regexp.MustCompile(`\$\{([A-Za-z0-9_]+)(:?-)?([^}]*)\}`)

// composeMainEnvironment returns the elitea-main environment that `docker
// compose` builds from the given files, in order, with `operator` standing in
// for the shell environment. Later files override earlier keys, as compose does.
func composeMainEnvironment(t *testing.T, operator map[string]string, files ...string) map[string]string {
	t.Helper()
	env := map[string]string{}
	for _, file := range files {
		body, err := os.ReadFile(filepath.Join(composeDeployDir, file)) //nolint:gosec // fixed, test-local path
		require.NoError(t, err)
		var compose struct {
			Services map[string]yaml.Node `yaml:"services"`
		}
		require.NoError(t, yaml.Unmarshal(body, &compose), file)
		main, found := compose.Services["elitea-main"]
		require.True(t, found, "%s has no elitea-main service", file)
		var service struct {
			Environment map[string]string `yaml:"environment"`
		}
		require.NoError(t, main.Decode(&service), file)
		for name, raw := range service.Environment {
			env[name] = composeInterpolation.ReplaceAllStringFunc(raw, func(match string) string {
				parts := composeInterpolation.FindStringSubmatch(match)
				value, set := operator[parts[1]]
				if set && (parts[2] != ":-" || value != "") {
					return value
				}
				return parts[3]
			})
		}
	}
	return env
}

// A compose default that is a fallback in the file but never reaches
// elitea-main (a typo in the variable name, a quoting slip) leaves discovery
// dark again, and the toolkit and MCP pages show "The tool list did not load".
func TestStandaloneComposeEnablesToolkitDiscoveryByDefault(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile(filepath.Join(composeDeployDir, "docker-compose.standalone-full.yml")) //nolint:gosec // fixed, test-local path
	require.NoError(t, err)
	require.Regexp(t, `(?m)^\s+`+discoveryFlag+`: "\$\{`+discoveryFlag+`:-true\}"\s*$`, string(body))

	for _, files := range [][]string{
		{"docker-compose.standalone-full.yml"},
		{"docker-compose.standalone-full.yml", "docker-compose.standalone-rust-agent.yml"},
	} {
		require.Equal(t, "true", composeMainEnvironment(t, nil, files...)[discoveryFlag], "%v", files)
		// An operator can still turn it off.
		off := composeMainEnvironment(t, map[string]string{discoveryFlag: "false"}, files...)
		config, err := ConfigFromEnv(mapLookup(off))
		require.NoError(t, err, "%v", files)
		require.False(t, config.ToolkitDiscoveryEnabled, "%v", files)
	}
}

// The composed environment must pass the same parser elitea-main starts with,
// with discovery on, and the toolkit route must land on the stream the
// deployed worker consumes. Both worker modes are covered: Python follows the
// index-ingest stream, Rust follows the agent stream.
func TestStandaloneComposeToolkitDiscoveryComposesForBothWorkers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		files          []string
		implementation string
		workerConfig   string
		stream         func(Config) string
	}{
		{
			name:           "python worker",
			files:          []string{"docker-compose.standalone-full.yml"},
			implementation: PythonWorkerImplementation,
			workerConfig:   "worker-runtime.json",
			stream:         func(c Config) string { return c.IndexIngestCommandStream },
		},
		{
			name:           "rust worker overlay",
			files:          []string{"docker-compose.standalone-full.yml", "docker-compose.standalone-rust-agent.yml"},
			implementation: RustWorkerImplementation,
			workerConfig:   "worker-runtime.rust.json",
			stream:         func(c Config) string { return c.AgentExecutionCommandStream },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := composeMainEnvironment(t, nil, tc.files...)
			for name, value := range env {
				require.NotContains(t, value, "${", "%s was not resolved", name)
			}
			config, err := ConfigFromEnv(mapLookup(env))
			require.NoError(t, err)
			require.True(t, config.Enabled)
			require.True(t, config.ToolkitDiscoveryEnabled)

			implementation, err := WorkerImplementationFromEnv(mapLookup(env))
			require.NoError(t, err)
			require.Equal(t, tc.implementation, implementation)
			capability, err := LoadPinnedWorkerToolkitCapability(implementation)
			require.NoError(t, err)
			route, err := configuredToolkitRoute(config, capability)
			require.NoError(t, err)
			require.True(t, route.enabled)
			require.Equal(t, tc.implementation == RustWorkerImplementation, route.rust)
			require.NotEmpty(t, route.stream)
			require.Equal(t, tc.stream(config), route.stream)
			require.NotEmpty(t, route.consumer, "the route stream is not a known command stream")

			// The worker in this mode must consume that stream, or discovery
			// commands wait for an answer that never comes.
			raw, err := os.ReadFile(filepath.Join(composeDeployDir, "runtime", tc.workerConfig)) //nolint:gosec // fixed, test-local path
			require.NoError(t, err)
			var worker struct {
				NATSStream string `json:"nats_stream"`
			}
			require.NoError(t, json.Unmarshal(raw, &worker))
			require.Equal(t, route.stream, worker.NATSStream)

			// composition.go refuses discovery without durable object storage;
			// main.go builds the store unless ELITEA_ARTIFACTS_ENABLED=false.
			require.NotEqual(t, "false", env["ELITEA_ARTIFACTS_ENABLED"])
			storageConfig, err := storage.ConfigFromEnv(mapLookup(env))
			require.NoError(t, err)
			require.NotEmpty(t, storageConfig.Backend)
		})
	}
}
