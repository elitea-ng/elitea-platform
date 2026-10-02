// Package deployedge_test gates the Traefik edge configuration this
// repository ships (#338).
//
// A Traefik router that names a middleware nobody defines does not fail loudly.
// Traefik logs `middleware "x@file" does not exist`, drops the router, and
// keeps serving. In deploy/centry-hybrid the dropped routers are the ones that
// select Go, and base.yml holds a PathPrefix("/") catch-all to pylon at
// priority 1. The caller then gets HTTP 200 from pylon for a path the
// configuration says goes to elitea-main, and the header-stripping middleware
// that the dropped router carried never runs either.
//
// This test resolves every middleware reference against the definitions that
// load with it, and it FAILS CLOSED: a configuration set whose files vanished,
// or an edge file that no set covers, is a failure and not a skip.
//
// A router names two more things, and each one fails in the same silent way.
// Two sibling gates read the configuration sets declared below:
//
//   - edge_services_test.go resolves the SERVICE each router names, and
//     compares the published port in the authority header with the Compose
//     default (#379);
//   - edge_auth_target_test.go follows the edgeAuth ADDRESS each loaded
//     router reaches, and checks that the stack registers it (#378).
//
// RUN IT WITH -count=1. The files this gate reads live in deploy/, outside
// this module. The Go test cache does not track them, so it can serve a stale
// pass after those files change. `task test`, `task deploy:check-edge` and the
// No Binaries workflow all pass -count=1.
//
// The No Binaries workflow runs this gate on every pull request. ci-go.yml
// also runs it, but ci-go.yml is path-filtered and does not select
// deploy/centry-hybrid/**, so it alone would miss an edge-only change.
package deployedge_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// dynamicConfig is the subset of the Traefik dynamic configuration this gate
// reads. Fields the gate does not use are ignored by the YAML decoder.
type dynamicConfig struct {
	HTTP struct {
		Routers map[string]struct {
			Middlewares []string `yaml:"middlewares"`
		} `yaml:"routers"`
		Middlewares map[string]struct {
			Chain struct {
				Middlewares []string `yaml:"middlewares"`
			} `yaml:"chain"`
		} `yaml:"middlewares"`
	} `yaml:"http"`
}

// configSet is one Traefik file-provider unit: the exact set of files a single
// Traefik process loads together. Middleware references resolve only inside
// their own set, so the set boundary is the thing that must be modelled
// correctly. Each set records the compose file that creates it, so a reviewer
// can check the claim.
type configSet struct {
	name string
	// dir loads every *.yml in one directory. This models a directory mount.
	dir string
	// files loads exactly these paths. This models single-file mounts.
	files []string
	// externalDefinitions names middlewares that a file OUTSIDE this
	// repository defines for this set. Keep this list exact and small. It is
	// the private-repository boundary, written down so that drift in the
	// tracked file still fails the gate.
	externalDefinitions []string
	// externalServices names load-balancer services that a file OUTSIDE this
	// repository defines for this set. It is the same boundary as
	// externalDefinitions, for the reference edge_services_test.go resolves. A
	// service name that is absent from both this list and the tracked files
	// still fails the gate.
	externalServices []string
	// composeFiles names the tracked Compose files that create this set's
	// stack. edge_auth_target_test.go reads them to answer one question: does
	// the process behind an edgeAuth address register that address?
	//
	// Leave it empty when the Compose model is not in this repository. A set
	// with no tracked Compose file and an edgeAuth reference fails, because
	// nothing can answer that question for it.
	composeFiles []string
	// mountedBy is the evidence for the set boundary above.
	mountedBy string
}

func configSets() []configSet {
	return []configSet{
		{
			name: "centry-hybrid foundation edge",
			// This set holds two files and NOT the whole directory.
			// deploy/centry-hybrid/traefik/index-routes.yml is excluded on
			// purpose: its routers name an edge-auth endpoint that this
			// stack does not register (#378). The exclusion is not a claim
			// this file makes on its own —
			// TestHybridFoundationEdgeLoadsTheComposeMount in
			// edge_auth_target_test.go reads the Compose volume list and
			// fails when the two stop agreeing.
			files: []string{
				"deploy/centry-hybrid/traefik/base.yml",
				"deploy/centry-hybrid/traefik/middlewares.yml",
			},
			composeFiles: []string{"deploy/centry-hybrid/docker-compose.yml"},
			mountedBy:    "deploy/centry-hybrid/docker-compose.yml mounts these two files under /etc/traefik/dynamic",
		},
		{
			name:  "centry-hybrid PoV edge",
			files: []string{"deploy/centry-hybrid/traefik/index-routes.yml"},
			// deploy/centry-hybrid/compose.sh mounts ONLY this file, as
			// /etc/traefik/dynamic/index.yml, into the private centry
			// auth_gateway. That gateway supplies its own base.yml, which
			// defines these three names against its own elitea-main-auth
			// service. Those definitions are not readable from here, so they
			// are declared rather than parsed. A fourth name appearing in
			// index-routes.yml still fails this gate.
			externalDefinitions: []string{
				"strip-caller-auth-context",
				"normalize-runtime-public-authority",
				"go-main-auth",
			},
			// The same private base.yml defines the two services these routers
			// name. It points them at the private stack's own containers. A
			// third service name in index-routes.yml still fails the gate.
			externalServices: []string{"elitea-main", "current-main"},
			// The Compose model of this stack is in the private centry
			// repository, so no tracked file can be read for it. The set also
			// contributes no edgeAuth address: the definitions above are
			// external, so this gate never sees an address to check.
			mountedBy: "deploy/centry-hybrid/compose.sh ELITEA_INDEX_ROUTE_FILE -> /etc/traefik/dynamic/index.yml",
		},
		{
			name:         "standalone edge",
			files:        []string{"deploy/traefik/dynamic.yml"},
			composeFiles: []string{"deploy/docker-compose.yml"},
			mountedBy:    "deploy/docker-compose.yml mounts it alone at /etc/traefik/dynamic.yml",
		},
		{
			name:  "standalone e2e edge",
			files: []string{"deploy/traefik/dynamic.e2e.yml"},
			composeFiles: []string{
				"deploy/docker-compose.e2e-standalone.yml",
				"deploy/docker-compose.standalone-full.yml",
			},
			mountedBy: "deploy/docker-compose.e2e-standalone.yml and deploy/docker-compose.standalone-full.yml mount it alone",
		},
		{
			name:         "runtime platform edge",
			files:        []string{"deploy/runtime/platform-edge-dynamic.yml"},
			composeFiles: []string{"deploy/docker-compose.standalone-full.yml"},
			mountedBy:    "deploy/docker-compose.standalone-full.yml mounts it alone at /etc/traefik/dynamic.yml",
		},
	}
}

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

// resolve returns the files of a set, failing when the set is empty.
func (s configSet) resolve(t *testing.T, root string) []string {
	t.Helper()
	if s.dir != "" {
		pattern := filepath.Join(root, s.dir, "*.yml")
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("set %q: glob %s: %v", s.name, pattern, err)
		}
		if len(matches) == 0 {
			t.Fatalf(
				"set %q declares directory %s, but it holds no *.yml file. "+
					"The configuration moved and this gate stopped gating. "+
					"Update configSets(). Mount evidence: %s",
				s.name, s.dir, s.mountedBy,
			)
		}
		sort.Strings(matches)
		return matches
	}
	resolved := make([]string, 0, len(s.files))
	for _, rel := range s.files {
		abs := filepath.Join(root, rel)
		if _, err := os.Stat(abs); err != nil {
			t.Fatalf(
				"set %q declares file %s, which does not exist. "+
					"The configuration moved and this gate stopped gating. "+
					"Update configSets(). Mount evidence: %s",
				s.name, rel, s.mountedBy,
			)
		}
		resolved = append(resolved, abs)
	}
	return resolved
}

func parseDynamic(t *testing.T, path string) dynamicConfig {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var parsed dynamicConfig
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse %s as Traefik dynamic configuration: %v", path, err)
	}
	return parsed
}

// providerLocalName drops the provider suffix. Traefik accepts both
// `name` and `name@file` in a reference, and both mean the same middleware
// when the provider is the file provider.
func providerLocalName(reference string) string {
	if index := strings.LastIndex(reference, "@"); index >= 0 {
		return reference[:index]
	}
	return reference
}

// TestEveryRouterMiddlewareResolves is the gate. Every middleware a router
// names must be defined by a file that loads with it.
func TestEveryRouterMiddlewareResolves(t *testing.T) {
	root := repoRoot(t)
	sets := configSets()
	if len(sets) == 0 {
		t.Fatal("no configuration set is declared, so nothing is gated")
	}

	totalRouters := 0
	for _, set := range sets {
		paths := set.resolve(t, root)

		defined := map[string]bool{}
		for _, name := range set.externalDefinitions {
			defined[name] = true
		}
		type reference struct {
			router string
			file   string
			name   string
		}
		var references []reference
		routers := 0

		for _, path := range paths {
			parsed := parseDynamic(t, path)
			relative, err := filepath.Rel(root, path)
			if err != nil {
				relative = path
			}
			for name := range parsed.HTTP.Middlewares {
				defined[name] = true
			}
			for router, definition := range parsed.HTTP.Routers {
				routers++
				for _, named := range definition.Middlewares {
					references = append(references, reference{
						router: router, file: relative,
						name: providerLocalName(named),
					})
				}
			}
			// A chain middleware references other middlewares and fails the
			// same way a router does.
			for name, definition := range parsed.HTTP.Middlewares {
				for _, named := range definition.Chain.Middlewares {
					references = append(references, reference{
						router: "chain middleware " + name, file: relative,
						name: providerLocalName(named),
					})
				}
			}
		}

		if routers == 0 {
			t.Fatalf(
				"set %q loaded %d file(s) but declares no http.routers. "+
					"Either the files moved or the shape changed, and this "+
					"gate stopped gating. Mount evidence: %s",
				set.name, len(paths), set.mountedBy,
			)
		}
		totalRouters += routers

		for _, ref := range references {
			if !defined[ref.name] {
				known := make([]string, 0, len(defined))
				for name := range defined {
					known = append(known, name)
				}
				sort.Strings(known)
				t.Errorf(
					"set %q: router %q in %s names middleware %q, which no "+
						"file in this set defines.\n"+
						"Traefik does not fail the stack for this. It drops "+
						"the router and keeps serving, so the traffic falls "+
						"to whichever router matches next.\n"+
						"Defined in this set: %s\n"+
						"Set boundary: %s",
					set.name, ref.router, ref.file, ref.name,
					strings.Join(known, ", "), set.mountedBy,
				)
			}
		}
	}

	if totalRouters == 0 {
		t.Fatal("no router was inspected, so this gate proved nothing")
	}
}

// TestEveryEdgeFileBelongsToASet stops a new edge file from escaping the gate.
// Adding deploy/somewhere/new-edge.yml with routers must either join a set or
// fail this test. Without it, the gate above silently keeps passing while the
// new file goes unchecked.
func TestEveryEdgeFileBelongsToASet(t *testing.T) {
	root := repoRoot(t)

	covered := map[string]bool{}
	for _, set := range configSets() {
		for _, path := range set.resolve(t, root) {
			covered[path] = true
		}
	}

	deployDir := filepath.Join(root, "deploy")
	if _, err := os.Stat(deployDir); err != nil {
		t.Fatalf("deploy/ does not exist at %s, so this gate stopped gating: %v", deployDir, err)
	}

	var uncovered []string
	err := filepath.WalkDir(deployDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if ext := filepath.Ext(path); ext != ".yml" && ext != ".yaml" {
			return nil
		}
		if covered[path] {
			return nil
		}
		parsed := parseDynamicQuiet(path)
		if parsed == nil || len(parsed.HTTP.Routers) == 0 {
			return nil
		}
		names := 0
		for _, definition := range parsed.HTTP.Routers {
			names += len(definition.Middlewares)
		}
		if names == 0 {
			return nil
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			relative = path
		}
		uncovered = append(uncovered, relative)
		return nil
	})
	if err != nil {
		t.Fatalf("walk deploy/: %v", err)
	}

	if len(uncovered) > 0 {
		sort.Strings(uncovered)
		t.Fatalf(
			"these files declare Traefik routers with middleware references "+
				"but belong to no configuration set, so nothing checks that "+
				"their middlewares exist:\n  %s\n"+
				"Add each one to configSets() in this file, with the compose "+
				"mount that proves which files load beside it.",
			strings.Join(uncovered, "\n  "),
		)
	}
}

// parseDynamicQuiet returns nil for a file that is not a Traefik dynamic
// configuration. Discovery must not fail on unrelated YAML, but note that
// TestEveryRouterMiddlewareResolves parses declared files strictly.
func parseDynamicQuiet(path string) *dynamicConfig {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var parsed dynamicConfig
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		return nil
	}
	return &parsed
}
