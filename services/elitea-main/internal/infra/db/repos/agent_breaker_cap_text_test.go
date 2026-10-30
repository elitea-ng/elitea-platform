package repos

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// The breaker leaves a refused call out of the tool failure streak by its
// text, which the worker writes (cap_error in run_caps.rs). This test keeps
// the two in step: a changed worker text would otherwise count every refused
// call as a tool failure again.
func TestCapRefusalTextMatchesTheWorker(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller")
	}
	path := filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "..", "..",
		"libs", "rust", "agent-runtime", "src", "run_caps.rs")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the worker's run caps: %v", err)
	}
	if !strings.Contains(string(source), strconv.Quote(capRefusalText)) {
		t.Fatalf("%s no longer writes %q; update capRefusalText and the breaker", path, capRefusalText)
	}
}
