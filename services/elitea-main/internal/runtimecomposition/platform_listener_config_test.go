package runtimecomposition

// The platform listener (#1359): elitea-main's product API on an internal
// mutual-TLS port, the replacement of the platform-edge proxy. These pin
// the configuration contract; cmd/elitea-runtime-material's
// TestPlatformListenerAdmitsOnlyRuntimeCAClients proves the handshake.

import (
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/security/securefile"
)

func platformEnvironment() map[string]string {
	environment := validEnvironment()
	environment["ELITEA_RUNTIME_PLATFORM_ADDRESS"] = ":9446"
	environment["ELITEA_RUNTIME_PLATFORM_TLS_CERT_FILE"] = "/run/secrets/content-server.pem"
	environment["ELITEA_RUNTIME_PLATFORM_TLS_KEY_FILE"] = "/run/secrets/content-server-key.pem"
	environment["ELITEA_RUNTIME_PLATFORM_TLS_CLIENT_CA_FILE"] = "/run/secrets/content-client-ca.pem"
	return environment
}

func TestPlatformListenerIsOptional(t *testing.T) {
	config, err := ConfigFromEnv(mapLookup(validEnvironment()))
	if err != nil {
		t.Fatal(err)
	}
	if config.PlatformAddress != "" || config.PlatformTLS.CertificateChainPath != "" {
		t.Fatalf("no platform listener was named, got %q", config.PlatformAddress)
	}
}

// The platform listener opens with the runtime OFF too (#1359 U5): its
// services run the public handler's routes in process, and a stack with no
// runtime plane (the E2E one) still serves the sub-application hosts'
// artifact calls on it. Its TLS rules hold the same.
func TestPlatformListenerOpensWithTheRuntimeOff(t *testing.T) {
	off := map[string]string{
		"ELITEA_RUNTIME_PLATFORM_ADDRESS":            ":9446",
		"ELITEA_RUNTIME_PLATFORM_TLS_CERT_FILE":      "/certs/platform.crt",
		"ELITEA_RUNTIME_PLATFORM_TLS_KEY_FILE":       "/certs/platform.key",
		"ELITEA_RUNTIME_PLATFORM_TLS_CLIENT_CA_FILE": "/certs/ca.crt",
	}
	config, err := ConfigFromEnv(mapLookup(off))
	if err != nil {
		t.Fatal(err)
	}
	if config.Enabled || config.PlatformAddress != ":9446" || config.PlatformTLS.ClientCAPath != "/certs/ca.crt" {
		t.Fatalf("config %+v", config)
	}
	delete(off, "ELITEA_RUNTIME_PLATFORM_TLS_KEY_FILE")
	if _, err := ConfigFromEnv(mapLookup(off)); err == nil || !strings.Contains(err.Error(), "ELITEA_RUNTIME_PLATFORM_TLS_KEY_FILE") {
		t.Fatalf("a listener without its key: %v", err)
	}
	delete(off, "ELITEA_RUNTIME_PLATFORM_ADDRESS")
	if _, err := ConfigFromEnv(mapLookup(off)); err == nil {
		t.Fatal("TLS files without an address were accepted")
	}
}

func TestPlatformListenerReadsItsTLSFiles(t *testing.T) {
	config, err := ConfigFromEnv(mapLookup(platformEnvironment()))
	if err != nil {
		t.Fatal(err)
	}
	if config.PlatformAddress != ":9446" ||
		config.PlatformTLS.CertificateChainPath != "/run/secrets/content-server.pem" ||
		config.PlatformTLS.PrivateKeyPath != "/run/secrets/content-server-key.pem" ||
		config.PlatformTLS.ClientCAPath != "/run/secrets/content-client-ca.pem" {
		t.Fatalf("platform listener configuration = %q %+v", config.PlatformAddress, config.PlatformTLS)
	}
	files, err := config.MaterialFiles()
	if err != nil {
		t.Fatal(err)
	}
	// The shared keypair appears once, with the private profile on the key.
	found := map[string]securefile.Permissions{}
	for _, file := range files {
		found[file.Path] = file.Permissions
	}
	if found["/run/secrets/content-server-key.pem"] != securefile.PrivateMaterial {
		t.Fatalf("the platform listener key is not private material: %v", found)
	}
}

func TestPlatformListenerRefusals(t *testing.T) {
	for name, testCase := range map[string]struct {
		mutate func(map[string]string)
		want   string
	}{
		"an address without TLS files": {
			mutate: func(environment map[string]string) {
				delete(environment, "ELITEA_RUNTIME_PLATFORM_TLS_CLIENT_CA_FILE")
			},
			want: "ELITEA_RUNTIME_PLATFORM_TLS_CLIENT_CA_FILE",
		},
		"TLS files without an address": {
			mutate: func(environment map[string]string) {
				delete(environment, "ELITEA_RUNTIME_PLATFORM_ADDRESS")
			},
			want: "ELITEA_RUNTIME_PLATFORM_ADDRESS is not",
		},
		"the content listener's address": {
			mutate: func(environment map[string]string) {
				environment["ELITEA_RUNTIME_PLATFORM_ADDRESS"] = ":9445"
			},
			want: "distinct addresses",
		},
		"the public listener's port": {
			mutate: func(environment map[string]string) {
				environment["ELITEA_RUNTIME_PLATFORM_ADDRESS"] = ":8080"
			},
			want: "public HTTP listener",
		},
		"an address with no port": {
			mutate: func(environment map[string]string) {
				environment["ELITEA_RUNTIME_PLATFORM_ADDRESS"] = "elitea-main"
			},
			want: "numeric TCP port",
		},
		"a TLS path with a line break": {
			mutate: func(environment map[string]string) {
				environment["ELITEA_RUNTIME_PLATFORM_TLS_KEY_FILE"] = "/run/secrets/content\nserver-key.pem"
			},
			want: "path is invalid",
		},
	} {
		t.Run(name, func(t *testing.T) {
			environment := platformEnvironment()
			testCase.mutate(environment)
			_, err := ConfigFromEnv(mapLookup(environment))
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("ConfigFromEnv error = %v, want one naming %q", err, testCase.want)
			}
		})
	}
}

// elitea-main never calls its own platform listener (#1359 H7: the
// next-input-suggestion self call is in process; U5: the internal MCP builders
// are InternalToolService, which the worker calls), so it needs no client
// identity of its own, and the retired main-platform-client files are not
// material.
func TestPlatformListenerNeedsNoMainClientIdentity(t *testing.T) {
	environment := platformEnvironment()
	environment["ELITEA_RUNTIME_AGENT_EXECUTION_DISPATCH_ENABLED"] = "true"
	environment["ELITEA_RUNTIME_AGENT_EXECUTION_COMMAND_STREAM"] = "ELITEA_RT_V1_AGENT"
	environment["ELITEA_RUNTIME_PLATFORM_CLIENT_CERT_FILE"] = "/run/secrets/main-platform-client.crt"
	environment["ELITEA_RUNTIME_PLATFORM_CLIENT_KEY_FILE"] = "/run/secrets/main-platform-client.key"
	config, err := ConfigFromEnv(mapLookup(environment))
	if err != nil {
		t.Fatalf("a platform-listener MCP origin without a Main client identity was refused: %v", err)
	}
	files, err := config.MaterialFiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.Contains(file.Path, "main-platform-client") {
			t.Fatalf("the retired main-platform-client file %s is still runtime material", file.Path)
		}
	}
}
