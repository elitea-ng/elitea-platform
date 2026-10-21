package runtimecomposition

// The platform listener (#1359): elitea-main's product API on an internal
// mutual-TLS port, the replacement of the Traefik platform-edge. These pin
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
		"a self call on the platform listener with no client identity": {
			mutate: func(environment map[string]string) {
				environment["ELITEA_RUNTIME_AGENT_EXECUTION_DISPATCH_ENABLED"] = "true"
				environment["ELITEA_RUNTIME_AGENT_EXECUTION_COMMAND_STREAM"] = "ELITEA_RT_V1_AGENT"
				environment["ELITEA_RUNTIME_CURRENT_MAIN_BASE_URL"] = "https://elitea-main:9446"
			},
			want: "requires a client certificate",
		},
		"half a client identity": {
			mutate: func(environment map[string]string) {
				environment["ELITEA_RUNTIME_PLATFORM_CLIENT_CERT_FILE"] = "/run/secrets/main-platform-client.crt"
			},
			want: "must be set together",
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

func TestPlatformClientIdentityIsRuntimeMaterial(t *testing.T) {
	environment := platformEnvironment()
	environment["ELITEA_RUNTIME_PLATFORM_CLIENT_CERT_FILE"] = "/run/secrets/main-platform-client.crt"
	environment["ELITEA_RUNTIME_PLATFORM_CLIENT_KEY_FILE"] = "/run/secrets/main-platform-client.key"
	config, err := ConfigFromEnv(mapLookup(environment))
	if err != nil {
		t.Fatal(err)
	}
	files, err := config.MaterialFiles()
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]securefile.Permissions{}
	for _, file := range files {
		found[file.Path] = file.Permissions
	}
	if permissions, ok := found["/run/secrets/main-platform-client.key"]; !ok || permissions != securefile.PrivateMaterial {
		t.Fatalf("the platform client key is not private runtime material: %v", found)
	}
	if _, ok := found["/run/secrets/main-platform-client.crt"]; !ok {
		t.Fatalf("the platform client certificate is not runtime material: %v", found)
	}
}
