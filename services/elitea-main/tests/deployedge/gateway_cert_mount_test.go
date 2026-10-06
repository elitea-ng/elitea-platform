// This file gates the certificate mounts of the LLM gateway service (#368,
// guarding the fix from #327).
//
// The gateway is the egress-facing process. It loads exactly three files:
// GATEWAY_TLS_CERT_FILE, GATEWAY_TLS_KEY_FILE and GATEWAY_TLS_CA_FILE. It once
// mounted the whole ./certs tree to get them. That tree is also the parent of
// ./certs/runtime, which holds the PAT signing key, the command signing key and
// keyring, the worker client key, the content/control/output server keys, the
// vault master key. The top level holds ca.key,
// which lets its holder mint an elitea-main client certificate. None of that is
// gateway material.
//
// A revert to `- ./certs:/run/certs:ro` keeps the stack working, so no test and
// no gate reported it. This one does.
//
// HOW IT DISCRIMINATES. The check does not read the host filesystem. deploy/certs
// is generated material and is absent from a clean checkout, so a stat-based
// "is it a directory" test would report the wrong answer, or no answer, exactly
// when it matters. The check is structural instead: the set of container paths
// the service mounts must EQUAL the set of container paths the service says it
// loads. A directory mount fails that, because /run/certs is not one of the three
// GATEWAY_TLS_* values. A fourth mount fails it too.
//
// The expected paths are read from the service's own environment, never
// hardcoded here. A hardcoded list drifts from the compose file, and the drift
// makes the gate pass while the mounts and the loader disagree.
//
// RUN IT WITH -count=1, for the reason stated in edge_middlewares_test.go: the
// YAML this file reads lives in deploy/, outside this module, so the Go test
// cache cannot see an edit to it.
package deployedge_test

import (
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// gatewayServiceName is the compose service that runs the LLM gateway.
const gatewayServiceName = "elitea-llm-gateway"

// gatewayFileEnvVars name the three files the gateway loads. They are the whole
// expected mount set: the gateway reads no other host file.
var gatewayFileEnvVars = []string{
	"GATEWAY_TLS_CERT_FILE",
	"GATEWAY_TLS_KEY_FILE",
	"GATEWAY_TLS_CA_FILE",
}

// composeFile is the subset of a compose model this gate reads.
type composeFile struct {
	Services map[string]struct {
		Environment map[string]string `yaml:"environment"`
		Volumes     []string          `yaml:"volumes"`
	} `yaml:"services"`
}

// bindMount is one parsed `source:target[:mode]` entry.
type bindMount struct {
	raw    string
	source string
	target string
	mode   string
}

// parseBindMount splits the short compose syntax. It returns false for a named
// volume (`data:/var/lib/x`), which has no host path and is not what this gate
// is about.
func parseBindMount(entry string) (bindMount, bool) {
	parts := strings.Split(entry, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return bindMount{}, false
	}
	mount := bindMount{raw: entry, source: parts[0], target: parts[1]}
	if len(parts) == 3 {
		mount.mode = parts[2]
	}
	if !strings.HasPrefix(mount.source, ".") && !strings.HasPrefix(mount.source, "/") {
		return bindMount{}, false
	}
	return mount, true
}

// composeFilesDefiningGateway finds every compose file under deploy/ that
// declares the gateway service. Discovery, and not a fixed path, is what stops a
// second compose file from adding a directory mount that nothing checks — the
// same fail-closed rule TestEveryEdgeFileBelongsToASet applies to the edge.
func composeFilesDefiningGateway(t *testing.T, root string) map[string]composeFile {
	t.Helper()

	deployDir := filepath.Join(root, "deploy")
	if _, err := os.Stat(deployDir); err != nil {
		t.Fatalf("deploy/ does not exist at %s, so this gate stopped gating: %v", deployDir, err)
	}

	found := map[string]composeFile{}
	err := filepath.WalkDir(deployDir, func(filePath string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			// Helm charts hold Go templates, which are not valid YAML.
			if entry.Name() == "helm" || entry.Name() == "certs" {
				return filepath.SkipDir
			}
			return nil
		}
		if ext := filepath.Ext(filePath); ext != ".yml" && ext != ".yaml" {
			return nil
		}
		raw, readErr := os.ReadFile(filePath)
		if readErr != nil {
			return nil
		}
		var parsed composeFile
		if yaml.Unmarshal(raw, &parsed) != nil {
			return nil
		}
		if _, ok := parsed.Services[gatewayServiceName]; !ok {
			return nil
		}
		relative, relErr := filepath.Rel(root, filePath)
		if relErr != nil {
			relative = filePath
		}
		found[relative] = parsed
		return nil
	})
	if err != nil {
		t.Fatalf("walk deploy/: %v", err)
	}

	if len(found) == 0 {
		t.Fatalf(
			"no compose file under deploy/ declares a %q service. Either the "+
				"service was renamed or it moved, and this gate stopped gating. "+
				"Update gatewayServiceName in this file.",
			gatewayServiceName,
		)
	}
	return found
}

// TestGatewayMountsCertificateFilesNeverADirectory is the gate.
func TestGatewayMountsCertificateFilesNeverADirectory(t *testing.T) {
	root := repoRoot(t)

	for relative, parsed := range composeFilesDefiningGateway(t, root) {
		service := parsed.Services[gatewayServiceName]

		// Expected set: the container paths the gateway itself says it loads.
		expected := map[string]string{}
		for _, name := range gatewayFileEnvVars {
			value := strings.TrimSpace(service.Environment[name])
			if value == "" {
				t.Fatalf(
					"%s: service %q sets no %s. The gateway serves TLS and "+
						"requires client certificates, so all three must be "+
						"set; with one missing this gate cannot say which "+
						"files belong in the mount set.",
					relative, gatewayServiceName, name,
				)
			}
			expected[value] = name
		}

		var mounts []bindMount
		for _, entry := range service.Volumes {
			mount, ok := parseBindMount(entry)
			if !ok {
				continue
			}
			mounts = append(mounts, mount)
		}
		if len(mounts) == 0 {
			t.Fatalf(
				"%s: service %q declares no host mount, so it cannot load the "+
					"certificates named by %s. Either the shape changed or the "+
					"mounts were dropped; this gate stopped gating either way.",
				relative, gatewayServiceName, strings.Join(gatewayFileEnvVars, ", "),
			)
		}

		seen := map[string]bool{}
		for _, mount := range mounts {
			target := path.Clean(mount.target)
			envVar, wanted := expected[target]
			if !wanted {
				t.Errorf(
					"%s: service %q mounts %q at %q, which is not one of the "+
						"three files it loads (%s).\n"+
						"The gateway is the egress-facing service. A mount "+
						"wider than those files hands it ./certs/runtime — the "+
						"PAT signing key, the command signing keyring, the "+
						"vault master key — plus "+
						"ca.key, which mints an elitea-main client certificate "+
						"(#327).\n"+
						"Mount one line per file, and nothing else:\n  - ./certs/%s:ro",
					relative, gatewayServiceName, mount.source, mount.target,
					strings.Join(gatewayFileEnvVars, ", "),
					strings.Join(mountHints(expected), "\n  - ./certs/"),
				)
				continue
			}
			if seen[target] {
				t.Errorf("%s: service %q mounts %q twice", relative, gatewayServiceName, target)
			}
			seen[target] = true

			// A file mount and a directory mount are told apart by the source,
			// not by the host filesystem: `- ./certs:/run/certs/ca.crt` would
			// place the whole tree behind a file-shaped target.
			if path.Base(path.Clean(mount.source)) != path.Base(target) {
				t.Errorf(
					"%s: service %q mounts source %q at %q (%s). The source "+
						"base name does not match the target base name, so the "+
						"source is not the single file the target claims to be.",
					relative, gatewayServiceName, mount.source, mount.target, envVar,
				)
			}
			if mount.mode != "ro" {
				t.Errorf(
					"%s: service %q mounts %q with mode %q. Certificate "+
						"material is read-only to the gateway; use `:ro`.",
					relative, gatewayServiceName, mount.target, mount.mode,
				)
			}
		}

		for target, envVar := range expected {
			if !seen[path.Clean(target)] {
				t.Errorf(
					"%s: service %q sets %s=%s but mounts no file at that "+
						"path, so the gateway cannot load it. Add:\n"+
						"  - ./certs/%s:%s:ro",
					relative, gatewayServiceName, envVar, target,
					path.Base(target), target,
				)
			}
		}
	}
}

// mountHints renders `<base name>:<target>` for every expected file, which is
// the exact mount line the compose file needs. The order is sorted: Go map
// iteration order is random, and an unstable message is harder to compare
// between two runs.
func mountHints(expected map[string]string) []string {
	hints := make([]string, 0, len(expected))
	for target := range expected {
		hints = append(hints, path.Base(target)+":"+target)
	}
	sort.Strings(hints)
	return hints
}
