package main

import (
	"os"
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
//   - golang.org/x/crypto/ssh: GO-2026-6354 and GO-2026-6355. Fixed in x/crypto
//     v0.56.0, which declares go 1.26.0 while this module builds on Go 1.25.
//     Drop this entry once the module moves to Go 1.26 and takes that release.
var advisoryPackagesWithoutFix = []string{
	"golang.org/x/crypto/openpgp",
	"golang.org/x/crypto/ssh",
}

// TestNoBinaryLinksAdvisoryPackagesWithoutFix lists every non-test dependency of
// every package in the module (all shipped binaries) and fails if one is, or is
// under, an advisoryPackagesWithoutFix entry. It lists the graph the images
// build: linux on both published architectures, with cgo off.
func TestNoBinaryLinksAdvisoryPackagesWithoutFix(t *testing.T) {
	const self = "github.com/EliteaAI/elitea-platform/services/elitea-main/cmd/elitea-main"
	assertNoLinkedAdvisoryPackages(t, self, advisoryPackagesWithoutFix)
}

func assertNoLinkedAdvisoryPackages(t *testing.T, self string, banned []string) {
	t.Helper()
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("go tool not found on PATH: %v", err)
	}
	for _, goarch := range []string{"amd64", "arm64"} {
		cmd := exec.Command(goTool, "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", "./...")
		cmd.Dir = filepath.Join("..", "..") // module root
		cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+goarch, "CGO_ENABLED=0")
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("linux/%s: go list -deps ./...: %v\n%s", goarch, err, stderr.String())
		}
		sawSelf := false
		for _, pkg := range strings.Fields(string(out)) {
			if pkg == self {
				sawSelf = true
			}
			for _, b := range banned {
				if pkg == b || strings.HasPrefix(pkg, b+"/") {
					t.Errorf("linux/%s: %s is linked into a binary of this module; it carries an advisory with no fix this module can take", goarch, pkg)
				}
			}
		}
		if !sawSelf {
			t.Fatalf("linux/%s: go list -deps ./... did not report %s; the scan did not cover this module", goarch, self)
		}
	}
}
