// Package buildcontext holds one gate: every relative `replace` a Go module
// declares must be reachable inside the image that builds that module.
//
// WHY THIS EXISTS
//
// Unit G6 gave elitea-main and elitea-llm-gateway a new shared library,
// libs/go/egresslib, and added the relative `replace` both modules need. Both
// modules compiled, `task test` was green, and every image build in CI died:
//
//	go: github.com/EliteaAI/elitea-platform/libs/go/egresslib@v0.0.0-...
//	(replaced by ../../libs/go/egresslib): reading /src/libs/go/egresslib/go.mod:
//	open /src/libs/go/egresslib/go.mod: no such file or directory
//
// `go mod download` reads a replacement's go.mod before anything else, so a
// directory the Containerfile never copies in fails the WHOLE image, not one
// package. Ten jobs failed on it — every E2E journey, the visual suite, the
// Helm kind smoke and both blocking image scans — and not one named the cause,
// because a build that produced no image reports only a bake error.
//
// Nothing compared the two files. Each file was correct on its own. This test
// is that comparison, and it runs in `task test`, minutes before an image
// build would find the same thing.
package buildcontext

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// modulesUnderTest lists every Go module that declares a relative `replace`
// and also ships an image. Add a module here when it gains either one. Each
// listed path must exist: a renamed file FAILS this test rather than making it
// vacuous.
var modulesUnderTest = []struct {
	name string
	// goMod and containerfile are repository-root relative.
	goMod         string
	containerfile string
	// contextDir is the build context, repository-root relative. Every COPY
	// source resolves against it. All three modules build from the repository
	// root: docker-bake.hcl, the compose files, publish.yml and
	// ci-image-scan.yml all state `context: .`.
	contextDir string
}{
	{
		name:          "elitea-main",
		goMod:         "services/elitea-main/go.mod",
		containerfile: "services/elitea-main/Containerfile",
		contextDir:    ".",
	},
	{
		name:          "elitea-scheduler",
		goMod:         "services/elitea-scheduler/go.mod",
		containerfile: "services/elitea-scheduler/Containerfile",
		contextDir:    ".",
	},
	{
		name:          "elitea-llm-gateway",
		goMod:         "services/elitea-llm-gateway/go.mod",
		containerfile: "services/elitea-llm-gateway/Containerfile",
		contextDir:    ".",
	},
}

// relativeReplace matches `replace <module> => <relative path>` and captures
// the path. A replacement that names a version (`=> other/mod v1.2.3`) is not
// a directory replacement, so it is not matched.
var relativeReplace = regexp.MustCompile(`(?m)^\s*replace\s+\S+\s+=>\s+(\.\.?/\S+)\s*$`)

// copyInstruction matches a `COPY` instruction and captures its arguments.
// `COPY --from=<stage>` is excluded on purpose: a stage copy reads another
// image, not the build context.
var copyInstruction = regexp.MustCompile(`(?m)^\s*COPY\s+(.*)$`)

func TestEveryRelativeReplaceIsInTheBuildContext(t *testing.T) {
	root := repoRoot(t)

	for _, mod := range modulesUnderTest {
		t.Run(mod.name, func(t *testing.T) {
			goModBody := readFile(t, filepath.Join(root, mod.goMod))
			containerfileBody := readFile(t, filepath.Join(root, mod.containerfile))

			replaces := relativeReplace.FindAllStringSubmatch(goModBody, -1)
			if len(replaces) == 0 {
				t.Fatalf("read no relative `replace` from %s.\n"+
					"Either the module lost its local libraries, or the pattern stopped matching. "+
					"A comparison against nothing passes, so this is a failure and not a skip.", mod.goMod)
			}

			copiesEverything, copied := copiedPaths(t, containerfileBody, mod.containerfile)

			moduleDir := filepath.Dir(mod.goMod)
			for _, replace := range replaces {
				// The replacement resolves against the module directory. The
				// COPY source resolves against the build context.
				target := filepath.Clean(filepath.Join(moduleDir, replace[1]))
				relToContext, err := filepath.Rel(filepath.Clean(mod.contextDir), target)
				if err != nil || strings.HasPrefix(relToContext, "..") {
					t.Fatalf("%s replaces a module with %s. That resolves to %s, outside the build context %q. "+
						"No COPY can reach it.", mod.goMod, replace[1], target, mod.contextDir)
				}
				if _, err := os.Stat(filepath.Join(root, target)); err != nil {
					t.Fatalf("%s replaces a module with %s, but %s is not in the tree: %v",
						mod.goMod, replace[1], target, err)
				}
				if copiesEverything || copiesPath(copied, relToContext) {
					continue
				}
				t.Errorf("%s replaces a module with %s, and %s never copies %s into the image.\n"+
					"`go mod download` reads that replacement's go.mod first, so the image build fails with\n"+
					"  reading /src/%s/go.mod: no such file or directory\n"+
					"Add `COPY %s ./%s` before the `go mod download` step.",
					mod.goMod, replace[1], mod.containerfile, relToContext,
					relToContext, relToContext, relToContext)
			}
		})
	}
}

// copiedPaths reads every build-context path a Containerfile copies. The first
// return value reports a `COPY . .`, which brings the whole context in.
func copiedPaths(t *testing.T, body, name string) (bool, []string) {
	t.Helper()

	matches := copyInstruction.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		t.Fatalf("read no COPY instruction from %s. The pattern stopped matching, "+
			"so every assertion below would hold against an empty set.", name)
	}

	var copied []string
	for _, match := range matches {
		fields := strings.Fields(match[1])
		// Drop flags. `--from=` makes the whole instruction irrelevant here.
		fromAnotherStage := false
		var args []string
		for _, field := range fields {
			switch {
			case strings.HasPrefix(field, "--from="):
				fromAnotherStage = true
			case strings.HasPrefix(field, "--"):
				// --chown, --chmod, --link: keep reading the sources.
			default:
				args = append(args, strings.Trim(field, `"`))
			}
		}
		// The last argument is the destination; the rest are sources.
		if fromAnotherStage || len(args) < 2 {
			continue
		}
		for _, source := range args[:len(args)-1] {
			source = filepath.Clean(source)
			if source == "." {
				return true, nil
			}
			copied = append(copied, source)
		}
	}
	return false, copied
}

// copiesPath reports whether a copied source brings `want` into the image.
// A copied ANCESTOR counts: `COPY libs/go ./libs/go` also brings libs/go/egresslib.
func copiesPath(copied []string, want string) bool {
	for _, source := range copied {
		if source == want || strings.HasPrefix(want, source+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

// repoRoot walks up from the test's working directory to the go.work that
// marks the repository root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, statErr := os.Stat(filepath.Join(dir, "go.work")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("found no go.work above %s; this test cannot locate the repository root", dir)
	return ""
}
