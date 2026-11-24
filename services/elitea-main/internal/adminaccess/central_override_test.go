package adminaccess

import (
	"bufio"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
)

const reviewedCentralOverride = "testdata/central_override.reviewed"

// readReviewed reads the reviewed allowlist: one pair per line, `#` comments
// and blank lines ignored.
func readReviewed(t *testing.T) []string {
	t.Helper()
	file, err := os.Open(reviewedCentralOverride)
	if err != nil {
		t.Fatalf("open %s: %v", reviewedCentralOverride, err)
	}
	defer func() { _ = file.Close() }()
	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

// Every (route, configuration type) pair of the central override has a line
// in the reviewed file, and the file has no line the code lacks. Adding a pair
// is a security review: the file is where the reviewer signs it.
func TestEveryCentralOverridePairIsReviewed(t *testing.T) {
	reviewed := readReviewed(t)
	pairs := CentralOverridePairs()
	if len(pairs) == 0 {
		t.Fatal("the central override has no pair; the check would pass on nothing")
	}
	listed := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		listed = append(listed, pair.String())
		if !slices.Contains(reviewed, pair.String()) {
			t.Errorf("the pair %q is not in %s: add it there only after a security review", pair, reviewedCentralOverride)
		}
	}
	for _, line := range reviewed {
		if !slices.Contains(listed, line) {
			t.Errorf("%s lists %q, which the central override does not have: remove the line", reviewedCentralOverride, line)
		}
	}
}

// The check above fails for a pair that only the code adds.
func TestAnUnreviewedPairFailsTheCheck(t *testing.T) {
	reviewed := readReviewed(t)
	extra := OverridePair{
		OverrideRoute: OverrideRoute{Method: http.MethodPost, Pattern: configurationsPrefix + "/configurations/{projectID}",
			Permission: ProjectsSettingsManage},
		ConfigurationType: "github",
	}
	if slices.Contains(reviewed, extra.String()) {
		t.Fatalf("%s reviews a credential type: %q", reviewedCentralOverride, extra)
	}
}

// No credential, model or toolkit type is on the list, on any route, and an
// update that changes a guarded type into one is not allowed.
func TestTheOverrideReachesOnlyTheGuardedSettings(t *testing.T) {
	for _, pair := range CentralOverridePairs() {
		if !IsGuardedSettingType(pair.ConfigurationType) {
			t.Errorf("%q names a type that is not a guarded setting", pair)
		}
	}
	update := configurationsPrefix + "/configuration/{projectID}/{configID}"
	for _, other := range []string{"github", "open_ai", "llm_model", "jira", "", " cloud_executor", "CLOUD_EXECUTOR"} {
		if OverrideAllows(http.MethodPut, update, other) {
			t.Errorf("the override allows an update of %q", other)
		}
		if OverrideAllows(http.MethodPut, update, "cloud_executor", other) {
			t.Errorf("the override allows cloud_executor to become %q", other)
		}
	}
	if !OverrideAllows(http.MethodPut, update, "cloud_executor", "browser") {
		t.Error("a change between two guarded types is refused")
	}
	if OverrideAllows(http.MethodPut, update) {
		t.Error("a request that names no type is allowed")
	}
	if OverrideRoutePermissions(http.MethodGet, configurationsPrefix+"/models/{projectID}") != nil {
		t.Error("a route outside the allowlist has override permissions")
	}
	if got := OverrideRoutePermissions(http.MethodGet, configurationsPrefix+"/configurations/{projectID}"); !slices.Equal(got,
		[]string{ProjectsSettingsView, ProjectsSettingsManage}) {
		t.Errorf("the list admits %v, want view or manage", got)
	}
	if got := OverrideRoutePermissions(http.MethodDelete, update); !slices.Equal(got, []string{ProjectsSettingsManage}) {
		t.Errorf("the delete admits %v, want manage only", got)
	}
}
