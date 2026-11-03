// Package workspacerun_test gates scripts/go/workspace-run.sh, which is the
// aggregation behind `task test`, `task vet`, `task coverage` and the ci-go.yml
// test job (#409).
//
// # WHY THIS GATE EXISTS
//
// The aggregation used to be an inline shell loop:
//
//	for mod in $(go list -m -f '{{.Dir}}'); do
//	  (cd "$mod" && go test -race -count=1 ./...)
//	done
//
// A shell loop reports the exit status of its LAST iteration. A failed module
// followed by a passed module therefore read as a pass. `task test` is the
// command every agent in this repository uses to decide whether its work is
// correct, so while it could exit 0 over a failed module, every verification
// claim in this repository was unsound.
//
// The coverage variant carried a second copy of the same class of defect: each
// subshell ended with `[ -f coverage.out ] && strip-generated.sh coverage.out`,
// and that trailing command owned the subshell exit status.
//
// This gate builds a small throwaway workspace, makes one module fail, and
// asserts the real exit status and the real output. It does not read the
// production workspace, so it cannot pass because the production workspace
// happens to be green today.
//
// RUN IT WITH -count=1. This gate reads scripts/go/workspace-run.sh, which
// lives outside this module. The Go test cache does not track that file, so it
// can serve a stale pass after the script changes. scripts/go/workspace-run.sh
// always passes -count=1, so `task test` and ci-go.yml both satisfy this.
package workspacerun_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repositoryRoot walks up from the working directory to the directory that
// holds both go.work and the script under test. It FAILS rather than skips: a
// gate that skips itself when it cannot find its subject reports a pass for a
// check it never ran.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatalf("read the working directory: %v", err)
	}
	for {
		_, workErr := os.Stat(filepath.Join(directory, "go.work"))
		_, scriptErr := os.Stat(filepath.Join(directory, "scripts", "go", "workspace-run.sh"))
		if workErr == nil && scriptErr == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatal("cannot find the repository root that holds go.work and scripts/go/workspace-run.sh")
		}
		directory = parent
	}
}

func copyFile(t *testing.T, source string, destination string) {
	t.Helper()
	content, err := os.ReadFile(source) //nolint:gosec // fixed in-repository path
	if err != nil {
		t.Fatalf("read %s: %v", source, err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		t.Fatalf("create the directory for %s: %v", destination, err)
	}
	if err := os.WriteFile(destination, content, 0o600); err != nil {
		t.Fatalf("write %s: %v", destination, err)
	}
}

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("create the directory for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// module writes one throwaway Go module with one test in it.
func module(t *testing.T, root string, directory string, modulePath string, passes bool) {
	t.Helper()
	writeFile(t, filepath.Join(root, directory, "go.mod"), "module "+modulePath+"\n\ngo 1.25\n")
	body := "\tif false {\n\t\tt.Fatal(\"unreachable\")\n\t}\n"
	if !passes {
		body = "\tt.Fatal(\"deliberate failure for the workspace-run gate\")\n"
	}
	writeFile(t, filepath.Join(root, directory, "unit_test.go"),
		"package unit\n\nimport \"testing\"\n\nfunc TestUnit(t *testing.T) {\n"+body+"}\n")
}

// skippingModule writes a module whose only test calls t.Skip. `go test` prints
// `ok` for it, which is the whole defect the skip ledger answers (#423).
func skippingModule(t *testing.T, root string, directory string, modulePath string) {
	t.Helper()
	writeFile(t, filepath.Join(root, directory, "go.mod"), "module "+modulePath+"\n\ngo 1.25\n")
	writeFile(t, filepath.Join(root, directory, "unit_test.go"),
		"package unit\n\nimport \"testing\"\n\n"+
			"func TestSkipsItself(t *testing.T) {\n"+
			"\tt.Skip(\"set THE_THING to run this\")\n}\n")
}

// fixture builds a throwaway workspace beside a copy of the script under test.
// alphaPasses selects whether the FIRST module of the workspace fails, which is
// the case the old inline loop could hide.
//
// The workspace holds two modules. A third module sits on disk but is left out
// of go.work, which stands for services/elitea-llm-gateway.
func fixture(t *testing.T, alphaPasses bool) string {
	t.Helper()
	root := t.TempDir()
	repository := repositoryRoot(t)

	copyFile(t,
		filepath.Join(repository, "scripts", "go", "workspace-run.sh"),
		filepath.Join(root, "scripts", "go", "workspace-run.sh"))
	copyFile(t,
		filepath.Join(repository, "scripts", "coverage", "strip-generated.sh"),
		filepath.Join(root, "scripts", "coverage", "strip-generated.sh"))
	// The skip ledger (#423). The script refuses to run without these, on
	// purpose: a skip reporter that quietly disappears is the same silent
	// absence it exists to find.
	copyFile(t,
		filepath.Join(repository, "scripts", "go", "skip-ledger.py"),
		filepath.Join(root, "scripts", "go", "skip-ledger.py"))
	copyFile(t,
		filepath.Join(repository, "scripts", "go", "skip-gate.py"),
		filepath.Join(root, "scripts", "go", "skip-gate.py"))
	// An EMPTY declaration file, not the repository's own. The fixture must
	// decide its own verdicts, so a production declaration cannot make one of
	// these gates pass.
	writeFile(t, filepath.Join(root, "scripts", "go", "declared-skips.txt"),
		"# the fixture declares nothing by default\n")

	writeFile(t, filepath.Join(root, "go.work"), "go 1.25\n\nuse (\n\t./modalpha\n\t./modbeta\n)\n")
	module(t, root, "modalpha", "example.com/alpha", alphaPasses)
	module(t, root, "modbeta", "example.com/beta", true)

	// On disk, outside the workspace. The summary must name it.
	module(t, root, "modoutside", "example.com/outside", true)

	return root
}

// run executes the copied script inside the fixture and returns its combined
// output and its real exit status.
//
// The status is read from the process, not from a pipeline. A piped status is
// the status of the LAST pipeline element, and reading it as the command's own
// status has produced several false green reports in this repository.
func run(t *testing.T, root string, arguments ...string) (string, int) {
	t.Helper()
	return runWithEnvironment(t, root, nil, arguments...)
}

// runWithEnvironment is run with extra environment entries, for the skip gate.
func runWithEnvironment(t *testing.T, root string, extra []string, arguments ...string) (string, int) {
	t.Helper()
	command := exec.Command("bash", append([]string{filepath.Join(root, "scripts", "go", "workspace-run.sh")}, arguments...)...) //nolint:gosec // fixed script path
	command.Dir = root
	command.Env = append(os.Environ(),
		"GOWORK="+filepath.Join(root, "go.work"),
		"GOFLAGS=",
		// The parent process may already carry this from ci-go.yml. The
		// fixture must control it, so clear it and let each test set it.
		"ELITEA_REQUIRE_DECLARED_SKIPS=",
		"ELITEA_SKIP_LEDGER=",
		// CI shards set this. The fixture must run its whole workspace unless
		// a test names a selection.
		"ELITEA_GO_PACKAGES_FILE=",
	)
	command.Env = append(command.Env, extra...)
	output, err := command.CombinedOutput()
	if err == nil {
		return string(output), 0
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		t.Fatalf("run the script: %v\n%s", err, output)
	}
	return string(output), exitError.ExitCode()
}

// TestFailedModuleFailsTheRunAndIsNamed is the regression for the masked
// status. The FIRST module fails and the SECOND passes. That is the exact
// shape the old inline loop reported as a pass.
func TestFailedModuleFailsTheRunAndIsNamed(t *testing.T) {
	root := fixture(t, false)
	output, status := run(t, root, "test")

	if status == 0 {
		t.Fatalf("a failed module must fail the run; got exit 0\n%s", output)
	}
	if !strings.Contains(output, "FAIL  modalpha") {
		t.Errorf("the summary must name the failed module modalpha\n%s", output)
	}
	if !strings.Contains(output, "workspace-run: 1 module(s) failed test") {
		t.Errorf("the script must report the count of failed modules\n%s", output)
	}

	// The later module must still run. A run that stops at the first failure
	// tells the reader less than a complete one, and it hides a second break.
	if !strings.Contains(output, "PASS  modbeta") {
		t.Errorf("the run must continue past the failed module and report modbeta\n%s", output)
	}

	// Prove the masking shape: the failed module runs BEFORE the passed one.
	failedAt := strings.Index(output, "==> workspace-run test: modalpha")
	passedAt := strings.Index(output, "==> workspace-run test: modbeta")
	if failedAt < 0 || passedAt < 0 {
		t.Fatalf("both modules must announce themselves\n%s", output)
	}
	if failedAt > passedAt {
		t.Errorf("the failed module must run first for this gate to reproduce the defect\n%s", output)
	}
}

// TestCoverageStripCannotOverwriteAFailedTest is the regression for the
// trailing command. The old subshell ended with
// `[ -f coverage.out ] && strip-generated.sh coverage.out`. `go test` still
// writes a profile when a test fails, so the strip step succeeded and its
// status replaced the failure.
func TestCoverageStripCannotOverwriteAFailedTest(t *testing.T) {
	root := fixture(t, false)
	output, status := run(t, root, "coverage")

	if status == 0 {
		t.Fatalf("a failed test must fail the coverage run; got exit 0\n%s", output)
	}
	if !strings.Contains(output, "FAIL  modalpha") {
		t.Errorf("the summary must name the failed module modalpha\n%s", output)
	}
	if !strings.Contains(output, "strip-generated") {
		t.Errorf("the strip step must still run, so this gate proves it did not own the status\n%s", output)
	}
}

// TestPassingWorkspacePassesAndNamesTheUncoveredModule holds the other half of
// the contract. A green run must stay green, and it must say which Go modules
// on disk it did NOT open. services/elitea-llm-gateway is the real one: it
// sits outside go.work on purpose and has its own ci-gateway.yml.
func TestPassingWorkspacePassesAndNamesTheUncoveredModule(t *testing.T) {
	root := fixture(t, true)
	output, status := run(t, root, "vet")

	if status != 0 {
		t.Fatalf("a clean workspace must pass; got exit %d\n%s", status, output)
	}
	if !strings.Contains(output, "workspace-run: every workspace module passed vet") {
		t.Errorf("a clean run must say so\n%s", output)
	}
	if !strings.Contains(output, "NOT COVERED by this command") {
		t.Errorf("the summary must name the modules the run did not cover\n%s", output)
	}
	if !strings.Contains(output, "modoutside") {
		t.Errorf("the module outside go.work must be named as not covered\n%s", output)
	}
}

// TestEmptyWorkspaceIsAFailure holds the guard against the other shape of this
// defect: a loop over nothing that reports success. An absence of modules is
// not a presence of passes.
func TestEmptyWorkspaceIsAFailure(t *testing.T) {
	root := fixture(t, true)

	// A workspace that names no module. `go list -m` then reports nothing.
	writeFile(t, filepath.Join(root, "go.work"), "go 1.25\n\nuse ()\n")

	output, status := run(t, root, "vet")
	if status == 0 {
		t.Fatalf("an empty module list must not report success\n%s", output)
	}
}

// skipFixture builds a workspace whose second module holds one test, and that
// test skips itself. `go test ./...` prints `ok` for it.
func skipFixture(t *testing.T) string {
	t.Helper()
	root := fixture(t, true)
	skippingModule(t, root, "modbeta", "example.com/beta")
	return root
}

// TestSkippedTestIsNamedAndCounted is the regression for #423. Eighteen suites
// called t.Skip on every CI run. `go test` printed `ok` for each, so a suite
// that ran and a suite that did nothing looked the same. The run must name the
// skip and count it.
func TestSkippedTestIsNamedAndCounted(t *testing.T) {
	root := skipFixture(t)
	output, status := run(t, root, "test")

	if status != 0 {
		t.Fatalf("a skip alone must not fail the run without the flag; got exit %d\n%s", status, output)
	}
	if !strings.Contains(output, "workspace-run skip ledger: 1 skipped") {
		t.Errorf("the run must report how many tests skipped\n%s", output)
	}
	if !strings.Contains(output, "TestSkipsItself") {
		t.Errorf("the run must name the skipped test\n%s", output)
	}
	if !strings.Contains(output, "set THE_THING to run this") {
		t.Errorf("the run must print the reason the test gave\n%s", output)
	}
	if !strings.Contains(output, "UNDECLARED (1)") {
		t.Errorf("a skip that no line declares must read as undeclared\n%s", output)
	}
}

// TestUndeclaredSkipFailsWhenRequired holds the gate itself. ci-go.yml sets
// ELITEA_REQUIRE_DECLARED_SKIPS=1, so a suite cannot go back to skipping in
// silence.
func TestUndeclaredSkipFailsWhenRequired(t *testing.T) {
	root := skipFixture(t)
	output, status := runWithEnvironment(t, root,
		[]string{"ELITEA_REQUIRE_DECLARED_SKIPS=1"}, "test")

	if status == 0 {
		t.Fatalf("an undeclared skip must fail the run under the flag; got exit 0\n%s", output)
	}
	if !strings.Contains(output, "TestSkipsItself") {
		t.Errorf("the failure must name the skipped test\n%s", output)
	}
}

// TestDeclaredSkipPassesWhenRequired holds the other half. A skip with a
// written reason is allowed, and the run prints the reason every time. A
// declaration file that could not make a run pass would push everybody to
// disable the gate instead of using it.
func TestDeclaredSkipPassesWhenRequired(t *testing.T) {
	root := skipFixture(t)
	writeFile(t, filepath.Join(root, "scripts", "go", "declared-skips.txt"),
		"# one declaration\nexample.com/beta\tTestSkipsItself\tTHE_THING is a licensed appliance that CI does not hold.\n")

	output, status := runWithEnvironment(t, root,
		[]string{"ELITEA_REQUIRE_DECLARED_SKIPS=1"}, "test")

	if status != 0 {
		t.Fatalf("a declared skip must pass under the flag; got exit %d\n%s", status, output)
	}
	if !strings.Contains(output, "DECLARED (1)") {
		t.Errorf("the run must count the declared skip\n%s", output)
	}
	if !strings.Contains(output, "licensed appliance") {
		t.Errorf("the run must print the declared reason\n%s", output)
	}
}

// TestMissingSkipReporterIsAFailure keeps the reporter itself honest. A gate
// that quietly disappears is the exact defect class this repository keeps
// finding: an absence read as correctness.
func TestMissingSkipReporterIsAFailure(t *testing.T) {
	root := fixture(t, true)
	if err := os.Remove(filepath.Join(root, "scripts", "go", "skip-ledger.py")); err != nil {
		t.Fatalf("remove the skip reporter: %v", err)
	}

	output, status := run(t, root, "test")
	if status == 0 {
		t.Fatalf("a missing skip reporter must not report success\n%s", output)
	}
	if !strings.Contains(output, "skip-ledger.py is missing") {
		t.Errorf("the failure must name the missing file\n%s", output)
	}
}

// TestUnknownModeIsRejected keeps a typed call site honest. A script that
// silently does nothing for an unknown argument reports a pass for a check
// nobody ran.
func TestUnknownModeIsRejected(t *testing.T) {
	root := fixture(t, true)
	output, status := run(t, root, "tset")
	if status == 0 {
		t.Fatalf("an unknown mode must not report success\n%s", output)
	}
	if !strings.Contains(output, "usage:") {
		t.Errorf("an unknown mode must print the usage text\n%s", output)
	}
}

// packagesFile writes a shard selection file (ELITEA_GO_PACKAGES_FILE) and
// returns the environment entry that names it.
func packagesFile(t *testing.T, root string, content string) []string {
	t.Helper()
	path := filepath.Join(root, "shard-packages.tsv")
	writeFile(t, path, content)
	return []string{"ELITEA_GO_PACKAGES_FILE=" + path}
}

// TestPackagesFileRunsOnlyTheSelectedPackages holds the CI shard mode. The
// FIRST module fails, but the shard file names only the second module, so this
// shard passes. The summary must name the first module as another shard's,
// not as a pass.
func TestPackagesFileRunsOnlyTheSelectedPackages(t *testing.T) {
	root := fixture(t, false)
	output, status := runWithEnvironment(t, root,
		packagesFile(t, root, "modbeta\texample.com/beta\n"), "test")

	if status != 0 {
		t.Fatalf("the selected module passes, so the shard must pass; got exit %d\n%s", status, output)
	}
	if !strings.Contains(output, "SHARD modalpha") {
		t.Errorf("the summary must name the module that another shard runs\n%s", output)
	}
	if strings.Contains(output, "PASS  modalpha") {
		t.Errorf("a module that did not run must not read as a pass\n%s", output)
	}
	if !strings.Contains(output, "PASS  modbeta") {
		t.Errorf("the selected module must run and pass\n%s", output)
	}
}

// TestPackagesFileStillFailsAFailedPackage keeps rule 2 true for a shard.
func TestPackagesFileStillFailsAFailedPackage(t *testing.T) {
	root := fixture(t, false)
	output, status := runWithEnvironment(t, root,
		packagesFile(t, root, "modalpha\texample.com/alpha\n"), "coverage")

	if status == 0 {
		t.Fatalf("a failed package in the shard must fail the shard; got exit 0\n%s", output)
	}
	if !strings.Contains(output, "FAIL  modalpha") {
		t.Errorf("the summary must name the failed module\n%s", output)
	}
}

// TestPackagesFileRejectsBadSelections holds the guards. An empty selection,
// a module outside the workspace and a non-test mode each stop the run: each
// one would otherwise be a shard that tests nothing and passes.
func TestPackagesFileRejectsBadSelections(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		content string
		mode    string
		want    string
	}{
		{"empty", "", "test", "missing or empty"},
		{"outside the workspace", "modoutside\texample.com/outside\n", "test", "not a workspace module"},
		{"not a selection line", "modbeta example.com/beta\n", "test", "not <module dir><TAB><import path>"},
		{"vet", "modbeta\texample.com/beta\n", "vet", "applies to test and coverage only"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := fixture(t, true)
			output, status := runWithEnvironment(t, root,
				packagesFile(t, root, testCase.content), testCase.mode)
			if status == 0 {
				t.Fatalf("a bad selection must not report success\n%s", output)
			}
			if !strings.Contains(output, testCase.want) {
				t.Errorf("the failure must say %q\n%s", testCase.want, output)
			}
		})
	}
}
