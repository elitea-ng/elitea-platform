package api_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoPylonBridgeWiringReturns is the gate that replaces the bridges #383
// deleted.
//
// Four bridges once joined this service to pylon, and every one of them was
// dead: a shadow comparator that mirrored traffic to pylon and diffed the two
// answers, a cutover reverse proxy to LEGACY_URL with a Redis endpoint-state
// tracker behind it, and a Redis remote-call client that asked pylon_auth to
// validate a credential. None had a composition site outside tests. They
// survived for a year because nothing proved they were absent — the same class
// this repository has produced repeatedly (#115, #123, #134, #136, #149).
//
// Deleting them is not what keeps them gone. A future parity task can
// reasonably reach for the same shapes, and the router would compile and pass
// every other test with a pylon proxy mounted in it again. This test refuses
// that at the source level.
//
// It reads SOURCE rather than building a router, because the property is "no
// file in this service names these things", which no runtime assertion can
// observe. It scans internal/ and cmd/ and skips its own file.
func TestNoPylonBridgeWiringReturns(t *testing.T) {
	t.Parallel()

	// Each pattern names one deleted bridge, with the reason it may not come
	// back. The strings are chosen to fire on the WIRING, not on prose: a
	// comment explaining the history is allowed, an import or a call is not.
	banned := []struct {
		pattern string
		why     string
	}{
		{
			pattern: "internal/api/shadow",
			why: "the shadow comparator mirrored every sampled request to pylon and " +
				"diffed the response. There is nothing to compare against: pylon " +
				"serves no elitea-main route.",
		},
		{
			pattern: "cutover.NewRouter",
			why: "the cutover router was a reverse proxy to LEGACY_URL — the only " +
				"pylon reverse proxy this service ever had.",
		},
		{
			pattern: "cutover.NewTracker",
			why: "the cutover tracker held per-endpoint legacy/shadow/canary/go " +
				"state in Redis for a traffic split that no longer exists.",
		},
		{
			pattern: "cutover.NewAdminHandler",
			why: "the /internal/cutover routes drove that traffic split.",
		},
		{
			pattern: "authsvc.New(",
			why: "authsvc.New built the Redis remote-call client that asked " +
				"pylon_auth to validate a credential. Authentication is local: " +
				"authsvc.NewLocalValidator and authsvc.NewPrincipalValidator read " +
				"the database directly.",
		},
		{
			pattern: "pylon_auth:rpc",
			why: "the Redis channel that client published on.",
		},
		{
			pattern: "InternalAdminToken",
			why: "InternalAdminToken gated the /internal/shadow and /internal/cutover " +
				"mounts and nothing else. No deployment ever set it.",
		},
	}

	root := repoRootFrom(t)
	for _, dir := range []string{"internal", "cmd"} {
		walkRoot := filepath.Join(root, dir)
		err := filepath.WalkDir(walkRoot, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			if filepath.Base(path) == "pylon_bridge_absence_test.go" {
				return nil
			}
			content, readErr := os.ReadFile(path) //nolint:gosec // walk of a fixed, test-local tree
			if readErr != nil {
				return readErr
			}
			relative, _ := filepath.Rel(root, path)
			for _, line := range splitSourceLines(string(content)) {
				for _, ban := range banned {
					if !strings.Contains(line.text, ban.pattern) {
						continue
					}
					t.Errorf("%s:%d names the deleted pylon bridge %q.\n"+
						"  %s\n"+
						"  #383 deleted it. If a replacement is genuinely needed, it needs an ADR\n"+
						"  and a composition site in cmd/elitea-main — not a nil-gated field that\n"+
						"  no deployment fills.\n"+
						"  Line: %s",
						relative, line.number, ban.pattern, ban.why, strings.TrimSpace(line.text))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", walkRoot, err)
		}
	}
}

// TestAuthenticationMiddlewareHoldsNoRedisAuthority is the structural form of
// the assertion TestAuthLocalValidationNeverConsultsSharedRedisCache used to
// make behaviourally (internal/api/middleware).
//
// That test carried a Redis-backed validator on AuthConfig and proved the
// middleware never dialled it. The field is gone, so the stronger statement is
// available: this package imports no Redis client at all, so no credential
// decision in it can consult a shared cache that any writer of that cache
// could poison.
func TestAuthenticationMiddlewareHoldsNoRedisAuthority(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(repoRootFrom(t), "internal", "api", "middleware")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		content := readFile(t, filepath.Join(dir, entry.Name()))
		for _, line := range splitSourceLines(content) {
			if !strings.Contains(line.text, `"github.com/redis/go-redis`) {
				continue
			}
			t.Errorf("internal/api/middleware/%s:%d imports a Redis client.\n"+
				"  Authentication in this package reads the database directly. A Redis\n"+
				"  cache in front of it is an authentication authority that every writer of\n"+
				"  that cache shares — which is what the pylon RPC validator was (#383).",
				entry.Name(), line.number)
		}
	}
}

// sourceLine is one line of a Go file with its 1-based number.
type sourceLine struct {
	number int
	text   string
}

// splitSourceLines returns every line of content, numbered. It does not strip
// comments: a banned pattern inside a comment is still reported, because the
// history is recorded in this test and in the issue rather than in scattered
// mentions that a reader cannot tell from live wiring.
func splitSourceLines(content string) []sourceLine {
	raw := strings.Split(content, "\n")
	lines := make([]sourceLine, 0, len(raw))
	for i, text := range raw {
		lines = append(lines, sourceLine{number: i + 1, text: text})
	}
	return lines
}
