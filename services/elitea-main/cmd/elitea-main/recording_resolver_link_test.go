package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// recordingResolverPackage holds the recording permission resolver of the
// administration surface helper (ADR-0059 R0.1b). It grants whatever its probe
// asks, so it must never be reachable from production wiring.
const recordingResolverPackage = "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/adminsurface"

// TestBinaryDoesNotLinkTheRecordingResolver lists the non-test dependencies
// of every command of this module, for the graph the images build (linux on
// both published architectures, cgo off), and fails if one is the recording
// resolver's package. It lists the commands, not ./..., because the helper
// package is itself part of ./... .
func TestBinaryDoesNotLinkTheRecordingResolver(t *testing.T) {
	const self = "github.com/elitea-ng/elitea-platform/services/elitea-main/cmd/elitea-main"
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("go tool not found on PATH: %v", err)
	}
	for _, goarch := range []string{"amd64", "arm64"} {
		cmd := exec.Command(goTool, "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", "./cmd/...")
		cmd.Dir = filepath.Join("..", "..") // module root
		cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+goarch, "CGO_ENABLED=0")
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("linux/%s: go list -deps ./cmd/...: %v\n%s", goarch, err, stderr.String())
		}
		sawSelf := false
		for _, pkg := range strings.Fields(string(out)) {
			if pkg == self {
				sawSelf = true
			}
			if pkg == recordingResolverPackage || strings.HasPrefix(pkg, recordingResolverPackage+"/") {
				t.Errorf("linux/%s: a command of this module links %s, the recording resolver", goarch, pkg)
			}
		}
		if !sawSelf {
			t.Fatalf("linux/%s: go list -deps ./cmd/... did not report %s; the scan did not cover the binary", goarch, self)
		}
	}
}
