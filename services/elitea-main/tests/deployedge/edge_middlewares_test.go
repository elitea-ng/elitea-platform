// Package deployedge_test gates the browser edges this repository ships.
//
// There is one: elitea-main itself, with its web relay turned on
// (internal/api/webedge, compose_edge_harness_test.go). The compose Traefik
// that used to stand in front of it is gone (#1359 P2), and in a cluster the
// Ingress or Gateway API HTTPRoute is a single `/` rule to elitea-main
// (deploy/gateway-api/httproute.yaml, edge_gateway_api_test.go; #1359 P3).
//
// This file holds the one rule that replaced the Traefik-only gates (#338,
// #379): no Traefik configuration ships at all. Those gates existed because a
// Traefik router that names a middleware or a service nobody defines does not
// fail loudly; Traefik drops the router and keeps serving, so the traffic falls
// to whichever router matches next and the header strip that router carried
// never runs. A second Traefik edge reintroduced into a compose file would
// bring that failure class back with nothing reading it, so its return fails
// here. What the routers DID — route, strip, redirect — is now gated live
// against the Go router in the sibling files.
//
// RUN IT WITH -count=1. The files these gates read live in deploy/, outside
// this module. The Go test cache does not track them, so it can serve a stale
// pass after those files change. `task test`, `task deploy:check-edge` and the
// No Binaries workflow all pass -count=1.
package deployedge_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// repoRoot walks up from the test's directory to the directory that holds
// go.work. It never returns a guessed path: a gate that reads a hardcoded
// path stops gating the moment a file moves, and this repository has already
// been bitten by that.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolve the working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.work is not in any parent directory, so the repository root is unknown")
		}
		dir = parent
	}
}

// edgeComposeService is the part of a compose service the edge gates read.
type edgeComposeService struct {
	Image       string    `yaml:"image"`
	Ports       yaml.Node `yaml:"ports"`
	Labels      yaml.Node `yaml:"labels"`
	Environment yaml.Node `yaml:"environment"`
}

// environment reads a compose `environment:` value in either form, a map or a
// list of KEY=VALUE.
func (s edgeComposeService) environment() map[string]string {
	values := map[string]string{}
	switch s.Environment.Kind {
	case yaml.MappingNode:
		for index := 0; index+1 < len(s.Environment.Content); index += 2 {
			values[s.Environment.Content[index].Value] = s.Environment.Content[index+1].Value
		}
	case yaml.SequenceNode:
		for _, item := range s.Environment.Content {
			key, value, _ := strings.Cut(item.Value, "=")
			values[key] = value
		}
	}
	return values
}

// shortPorts returns the short-syntax `ports:` entries. A long-syntax entry
// comes back as published:target, so every caller reads one shape.
func (s edgeComposeService) shortPorts() []string {
	var ports []string
	for _, item := range s.Ports.Content {
		switch item.Kind {
		case yaml.ScalarNode:
			ports = append(ports, item.Value)
		case yaml.MappingNode:
			fields := map[string]string{}
			for index := 0; index+1 < len(item.Content); index += 2 {
				fields[item.Content[index].Value] = item.Content[index+1].Value
			}
			ports = append(ports, fields["published"]+":"+fields["target"])
		}
	}
	return ports
}

type edgeComposeFile struct {
	Services map[string]edgeComposeService `yaml:"services"`
}

// composeFilesUnderDeploy parses every compose file under deploy/, keyed by
// path relative to the repository root. Helm templates are not YAML and are
// skipped; a file that is not a compose file parses to no services.
func composeFilesUnderDeploy(t *testing.T, root string) map[string]edgeComposeFile {
	t.Helper()
	deployDir := filepath.Join(root, "deploy")
	if _, err := os.Stat(deployDir); err != nil {
		t.Fatalf("deploy/ does not exist at %s, so this gate stopped gating: %v", deployDir, err)
	}
	found := map[string]edgeComposeFile{}
	err := filepath.WalkDir(deployDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "helm" || entry.Name() == "certs" {
				return filepath.SkipDir
			}
			return nil
		}
		if ext := filepath.Ext(path); ext != ".yml" && ext != ".yaml" {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		var parsed edgeComposeFile
		if yaml.Unmarshal(raw, &parsed) != nil || len(parsed.Services) == 0 {
			return nil
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			relative = path
		}
		found[relative] = parsed
		return nil
	})
	if err != nil {
		t.Fatalf("walk deploy/: %v", err)
	}
	if _, ok := found["deploy/docker-compose.standalone-full.yml"]; !ok {
		t.Fatalf("the walk did not parse deploy/docker-compose.standalone-full.yml, so it reads an incomplete tree: %v", sortedKeys(found))
	}
	return found
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// traefikRouterConfig is the shape of a Traefik dynamic file, as far as this
// gate needs to recognise one.
type traefikRouterConfig struct {
	HTTP struct {
		Routers map[string]any `yaml:"routers"`
	} `yaml:"http"`
}

// TestNoTraefikEdgeShips replaces the middleware and service resolution gates.
// It fails on any of the three ways a Traefik edge could come back: a dynamic
// file with routers anywhere under deploy/, a compose service running a
// traefik image, and Traefik routing labels on any compose service.
func TestNoTraefikEdgeShips(t *testing.T) {
	root := repoRoot(t)

	if _, err := os.Stat(filepath.Join(root, "deploy", "traefik")); err == nil {
		t.Error("deploy/traefik/ exists again. elitea-main is the compose browser edge (internal/api/webedge, #1359 P2); " +
			"a Traefik in front of it brings back a routing layer no gate here reads")
	}

	scanned := 0
	err := filepath.WalkDir(filepath.Join(root, "deploy"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "helm" {
				return filepath.SkipDir
			}
			return nil
		}
		if ext := filepath.Ext(path); ext != ".yml" && ext != ".yaml" {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		scanned++
		var parsed traefikRouterConfig
		if yaml.Unmarshal(raw, &parsed) == nil && len(parsed.HTTP.Routers) > 0 {
			relative, _ := filepath.Rel(root, path)
			t.Errorf("%s declares Traefik http.routers. No compose stack runs Traefik any more (#1359 P2)", relative)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk deploy/: %v", err)
	}
	if scanned == 0 {
		t.Fatal("no YAML file was read under deploy/, so this gate proved nothing")
	}

	files := composeFilesUnderDeploy(t, root)
	for _, relative := range sortedKeys(files) {
		for _, name := range sortedKeys(files[relative].Services) {
			service := files[relative].Services[name]
			image := strings.ToLower(service.Image)
			if name == "traefik" || strings.HasPrefix(image, "traefik") || strings.Contains(image, "/traefik") {
				t.Errorf("%s: service %q runs Traefik (image %q). elitea-main is the compose browser edge (#1359 P2)", relative, name, service.Image)
			}
			if labels := labelText(service.Labels); strings.Contains(labels, "traefik.") {
				t.Errorf("%s: service %q carries Traefik labels. No compose stack runs Traefik any more (#1359 P2)", relative, name)
			}
		}
	}
}

// labelText flattens a compose `labels:` value, which is either a list or a
// map, into one string.
func labelText(node yaml.Node) string {
	var builder strings.Builder
	var walk func(*yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.ScalarNode {
			builder.WriteString(n.Value)
			builder.WriteString("\n")
		}
		for _, child := range n.Content {
			walk(child)
		}
	}
	walk(&node)
	return builder.String()
}
