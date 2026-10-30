package outputcontract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// regexFixture is libs/proto/contracts/output-contract-regex-v1.json. The
// worker test (libs/rust/agent-runtime/src/output_contract.rs) reads the
// same file, so Main and the worker cannot drift.
type regexFixture struct {
	Schema string `json:"schema"`
	Cases  []struct {
		Pattern string `json:"pattern"`
		Main    bool   `json:"main"`
		Worker  bool   `json:"worker"`
		Note    string `json:"note"`
	} `json:"cases"`
}

func loadRegexFixture(t *testing.T) regexFixture {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "../../../../../libs/proto/contracts/output-contract-regex-v1.json"))
	if err != nil {
		t.Fatalf("read the regex fixture: %v", err)
	}
	var fixture regexFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode the regex fixture: %v", err)
	}
	if fixture.Schema != "elitea.output-contract-regex.v1" || len(fixture.Cases) == 0 {
		t.Fatalf("unexpected regex fixture: %q with %d cases", fixture.Schema, len(fixture.Cases))
	}
	return fixture
}

// A pattern that Main accepts compiles in the worker: Main refuses on save
// what the worker would refuse on every run.
func TestRegexContractsMatchTheSharedFixture(t *testing.T) {
	t.Parallel()
	for _, tc := range loadRegexFixture(t).Cases {
		if tc.Main && !tc.Worker {
			t.Errorf("fixture %q (%s): Main must not accept a pattern that the worker refuses", tc.Pattern, tc.Note)
		}
		err := Check(map[string]any{"type": "regex", "pattern": tc.Pattern})
		if accepted := err == nil; accepted != tc.Main {
			t.Errorf("Check(%q) accepted = %v, want %v (%s)", tc.Pattern, accepted, tc.Main, tc.Note)
		}
		if err != nil && reason(t, err) != ReasonPatternInvalid {
			t.Errorf("Check(%q) reason = %q, want %q", tc.Pattern, reason(t, err), ReasonPatternInvalid)
		}
	}
}

// A repetition multiplies the size estimate, and a Unicode class is much
// larger than an ASCII class.
func TestRegexSizeEstimateGrowsWithRepetition(t *testing.T) {
	t.Parallel()
	if !checkRegex(`\w{5}`) || checkRegex(`\w{10}`) {
		t.Fatal(`want \w{5} accepted and \w{10} refused`)
	}
	if !checkRegex(`[A-Za-z0-9_]{1000}`) {
		t.Fatal("an ASCII class repeated 1000 times is small")
	}
}
