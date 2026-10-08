package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// advisoryPackagesWithoutFix are golang.org/x/crypto packages whose advisories
// this module cannot take the fix for. No binary built from this module may link
// them, which keeps each advisory a module-level govulncheck finding with no code
// path behind it.
//
//   - golang.org/x/crypto/openpgp: GO-2026-5932. Unmaintained; no fix will ship.
var advisoryPackagesWithoutFix = []string{
	"golang.org/x/crypto/openpgp",
}

// TestNoBinaryLinksAdvisoryPackagesWithoutFix lists every non-test dependency of
// every package in the module (all shipped binaries) and fails if one is, or is
// under, an advisoryPackagesWithoutFix entry.
func TestNoBinaryLinksAdvisoryPackagesWithoutFix(t *testing.T) {
	const self = "github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/cmd/elitea-llm-gateway"
	assertNoLinkedAdvisoryPackages(t, self, advisoryPackagesWithoutFix)
}

func assertNoLinkedAdvisoryPackages(t *testing.T, self string, banned []string) {
	t.Helper()
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("go tool not found on PATH: %v", err)
	}
	cmd := exec.Command(goTool, "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", "./...")
	cmd.Dir = filepath.Join("..", "..") // module root
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps ./...: %v\n%s", err, stderr.String())
	}
	sawSelf := false
	for _, pkg := range strings.Fields(string(out)) {
		if pkg == self {
			sawSelf = true
		}
		for _, b := range banned {
			if pkg == b || strings.HasPrefix(pkg, b+"/") {
				t.Errorf("%s is linked into a binary of this module; it carries an advisory with no fix this module can take", pkg)
			}
		}
	}
	if !sawSelf {
		t.Fatalf("go list -deps ./... did not report %s; the scan did not cover this module", self)
	}
}
