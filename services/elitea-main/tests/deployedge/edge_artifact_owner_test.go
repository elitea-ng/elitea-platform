// The artifact plane has ONE owner in the mixed deployment, and two files say
// so (#337).
//
// Before this gate the hybrid stack routed artifacts to pylon and switched the
// Go object store off, and those were two independent statements in two files.
// The cutover flips both. A change that flips one of them produces a stack that
// starts, serves every other route, and answers every artifact request from the
// wrong place — or from a service that composed no object store at all and
// answers 404. Traefik reports nothing about it: the router loads, resolves,
// and forwards.
//
// So this file holds the two statements together:
//
//  1. every router whose rule names an artifact path resolves to elitea-main;
//  2. the Compose overlay composes the Go object store for that service.
//
// Neither half is derived from a hardcoded router name. The routers are found
// by reading their RULES, so a renamed or newly added artifact router is
// covered the day it is written, and a file that stopped containing any
// artifact router fails rather than passing on an empty set — the
// "nothing found, therefore OK" trap this repository keeps finding.
//
// RUN IT WITH -count=1, for the reason stated in edge_middlewares_test.go: the
// YAML this file reads lives in deploy/, outside this module.
package deployedge_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// hybridIndexRoutes is the edge file the PoV gateway mounts, and
// hybridPoVCompose is the overlay that configures the service those routers
// name. They are a pair; a gate that reads one of them cannot see the defect.
const (
	hybridIndexRoutes = "deploy/centry-hybrid/traefik/index-routes.yml"
	hybridPoVCompose  = "deploy/centry-hybrid/pov-compose.yml"
)

// artifactOwner is the service every artifact router must name. It is the Go
// service, and after #337 it is the only implementation with an object store
// in this stack.
const artifactOwner = "elitea-main"

// edgeRouterRules re-reads the edge YAML with the two fields this gate needs.
// Each gate in this package parses its own shape, so no gate changes what
// another one reads.
type edgeRouterRules struct {
	HTTP struct {
		Routers map[string]struct {
			Rule    string `yaml:"rule"`
			Service string `yaml:"service"`
		} `yaml:"routers"`
	} `yaml:"http"`
}

// namesAnArtifactPath reports a router rule that selects an artifact path. It
// matches on the PATH text rather than on a router name, because a name is a
// label somebody chooses and a path is what the caller sends. Both surfaces
// count: the browser-facing /api/v2/artifacts routes and the root-mounted
// /artifacts/s3 surface the pinned SDK speaks.
func namesAnArtifactPath(rule string) bool {
	return strings.Contains(rule, "/api/v2/artifacts/") ||
		strings.Contains(rule, "/artifacts/s3/")
}

// TestEveryArtifactRouteResolvesToTheGoService is the first half.
func TestEveryArtifactRouteResolvesToTheGoService(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, hybridIndexRoutes)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", hybridIndexRoutes, err)
	}
	var parsed edgeRouterRules
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse %s as Traefik dynamic configuration: %v", hybridIndexRoutes, err)
	}

	var matched []string
	for name, definition := range parsed.HTTP.Routers {
		if !namesAnArtifactPath(definition.Rule) {
			continue
		}
		matched = append(matched, name)
		if definition.Service != artifactOwner {
			t.Errorf(
				"router %q in %s selects an artifact path and resolves to %q.\n"+
					"Every artifact path in the mixed deployment is served by %q "+
					"(#337): the Go S3 verbs are registered and the Compose "+
					"overlay composes the object store behind them.\n"+
					"A router that still names the current platform sends "+
					"artifact traffic to a store nothing writes to any more.",
				name, hybridIndexRoutes, definition.Service, artifactOwner,
			)
		}
	}
	sort.Strings(matched)

	// The floor. Without it this test passes on a file that lost every
	// artifact router — which is exactly the shape that sends the traffic back
	// to pylon, because base.yml holds a PathPrefix("/") catch-all at
	// priority 1 and an absent router is a silent fall-through to it.
	if len(matched) == 0 {
		t.Fatalf(
			"%s declares no router that selects an artifact path, so this "+
				"test measured nothing.\n"+
				"An absent artifact router does not mean artifacts are "+
				"unrouted: base.yml catches every unmatched path at priority 1 "+
				"and sends it to the current platform.",
			hybridIndexRoutes,
		)
	}
	t.Logf("artifact routers resolving to %s: %s", artifactOwner, strings.Join(matched, ", "))
}

// TestTheHybridComposesTheObjectStoreItRoutesTo is the second half.
//
// It reads the Compose overlay as TEXT rather than resolving the merged model.
// Resolving it needs the private Centry repository, so a gate written that way
// could not run here at all — and a gate that cannot run is the defect, not
// the coverage. The overlay is the file that states these values, so reading
// it answers the question this test asks.
func TestTheHybridComposesTheObjectStoreItRoutesTo(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, hybridPoVCompose)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", hybridPoVCompose, err)
	}

	type overlay struct {
		Services map[string]struct {
			Environment map[string]string `yaml:"environment"`
		} `yaml:"services"`
	}
	var parsed overlay
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse %s as a Compose model: %v", hybridPoVCompose, err)
	}

	service, ok := parsed.Services[artifactOwner]
	if !ok {
		t.Fatalf(
			"%s declares no %q service, so this test measured nothing",
			hybridPoVCompose, artifactOwner,
		)
	}

	// ELITEA_ARTIFACTS_ENABLED is read with != "false" in cmd/elitea-main, so
	// an ABSENT variable also composes the store. The overlay states it
	// anyway, and this gate requires the statement: the value that used to be
	// here was "false", and an operator reading the file must be able to see
	// which way it now points without knowing the Go default.
	if got := service.Environment["ELITEA_ARTIFACTS_ENABLED"]; got != "true" {
		t.Errorf(
			"%s sets ELITEA_ARTIFACTS_ENABLED=%q on %s; the artifact routers "+
				"in %s resolve here, so anything but \"true\" routes every "+
				"artifact request to a service that composes no object store "+
				"and answers 404 (#337).",
			hybridPoVCompose, got, artifactOwner, hybridIndexRoutes,
		)
	}

	// storage.ConfigFromEnv returns an ERROR — never a silent default — when
	// either of these is absent, and cmd/elitea-main turns that error into a
	// refusal to boot. Naming them here means the pair is caught in CI rather
	// than in a container that will not start.
	for _, required := range []string{"STORAGE_BACKEND", "STORAGE_CONTAINER"} {
		if service.Environment[required] == "" {
			t.Errorf(
				"%s sets no %s on %s. With ELITEA_ARTIFACTS_ENABLED true, "+
					"storage.ConfigFromEnv refuses an absent value and "+
					"elitea-main does not start.",
				hybridPoVCompose, required, artifactOwner,
			)
		}
	}
}
