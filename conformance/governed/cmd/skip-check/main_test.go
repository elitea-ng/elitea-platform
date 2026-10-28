package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pkg = "example.test/governed"

func ledger(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "declared.txt")
	if err := os.WriteFile(path, []byte("# comment\n\n"+strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func logFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "run.log")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func check(t *testing.T, declared, log string) (int, string) {
	t.Helper()
	var out strings.Builder
	code := run([]string{"-declared", declared, "-package", pkg, log}, &out, &out)
	return code, out.String()
}

const sampleLog = `=== RUN   TestGovernedProfile
=== RUN   TestGovernedProfile/GOV-002
    cases_test.go:1: gate D1
--- SKIP: TestGovernedProfile/GOV-002 (0.00s)
--- PASS: TestGovernedProfile/GOV-003/pgvault_static_secret (12.10s)
    --- SKIP: TestGovernedProfile/GOV-005/auth_source (0.00s)
--- PASS: TestGovernedProfile (40.00s)
PASS
`

func TestDeclaredSkipsPass(t *testing.T) {
	declared := ledger(t,
		pkg+"\tTestGovernedProfile/GOV-002\tgate D1",
		pkg+"\tTestGovernedProfile/GOV-005/auth_source\tgate auth-source",
		pkg+"\tTestGovernedProfile/GOV-009/outbound\tgate vault-key")
	code, out := check(t, declared, logFile(t, sampleLog))
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "not exercised  TestGovernedProfile/GOV-009/outbound") {
		t.Fatalf("an unexercised declaration is not reported:\n%s", out)
	}
}

func TestUndeclaredSkipFails(t *testing.T) {
	declared := ledger(t, pkg+"\tTestGovernedProfile/GOV-002\tgate D1")
	code, out := check(t, declared, logFile(t, sampleLog))
	if code != 1 || !strings.Contains(out, "UNDECLARED SKIP TestGovernedProfile/GOV-005/auth_source") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func TestAnotherPackagesDeclarationDoesNotCover(t *testing.T) {
	declared := ledger(t,
		"other.test/pkg\t*\tnot ours",
		pkg+"\tTestGovernedProfile/GOV-002\tgate D1")
	if code, out := check(t, declared, logFile(t, sampleLog)); code != 1 {
		t.Fatalf("a declaration of another package excused a skip:\n%s", out)
	}
}

func TestWildcardCoversSubtests(t *testing.T) {
	declared := ledger(t, pkg+"\tTestGovernedProfile/GOV-00*\tnot a wildcard form",
		pkg+"\tTestGovernedProfile/GOV-002/*\tgate D1",
		pkg+"\tTestGovernedProfile/GOV-005/*\tgates")
	if code, out := check(t, declared, logFile(t, sampleLog)); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func TestStaleExactDeclarationFails(t *testing.T) {
	declared := ledger(t,
		pkg+"\tTestGovernedProfile/GOV-002\tgate D1",
		pkg+"\tTestGovernedProfile/GOV-005/auth_source\tgate",
		pkg+"\tTestGovernedProfile/GOV-003/pgvault_static_secret\tused to skip")
	code, out := check(t, declared, logFile(t, sampleLog))
	if code != 1 || !strings.Contains(out, "STALE DECLARATION TestGovernedProfile/GOV-003/pgvault_static_secret") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func TestEmptyRunFails(t *testing.T) {
	declared := ledger(t, pkg+"\tTestGovernedProfile/GOV-002\tgate D1")
	if code, out := check(t, declared, logFile(t, "ok  \texample.test/governed\t0.01s [no tests to run]\n")); code != 1 {
		t.Fatalf("an empty run was accepted:\n%s", out)
	}
}

func TestMalformedLedgerIsRefused(t *testing.T) {
	declared := ledger(t, pkg+"\tTestGovernedProfile/GOV-002")
	if code, _ := check(t, declared, logFile(t, sampleLog)); code != 2 {
		t.Fatalf("a declaration without a reason was accepted (exit %d)", code)
	}
}
