package buildcontext

import (
	"go/version"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The standard-library security floors. A Go image ships the standard library
// of the toolchain that built it, so a vulnerable toolchain is a vulnerable
// binary however current every module is.
//
// govulncheck on 2026-10-08 found seven standard-library vulnerabilities that
// elitea-main's code reaches: GO-2026-5026 (net/http), GO-2026-5972
// (encoding/asn1), GO-2026-6088 (encoding/xml), GO-2026-6089 (net/http),
// GO-2026-6090 (crypto/tls), GO-2026-6091 (html/template) and GO-2026-6218
// (net/url). Every one is fixed in go1.25.13 and in go1.26.6.
//
// govulncheck on 2026-10-09 found thirteen more that the services reach or
// import: GO-2026-6599 and GO-2026-6600 (html/template), GO-2026-6604 (os),
// GO-2026-6605, GO-2026-6609 and GO-2026-6613 (net/http), GO-2026-6607
// (crypto/tls), GO-2026-6608 (net/textproto), and the HTTP/2 server and
// transport set GO-2026-6603, 6610, 6611, 6612 and 6617 (net/http). They are
// fixed in go1.26.9 and in no 1.25 release, so every module moved to the 1.26
// series and the 1.25 floor was retired.
//
// The builder images stay on a SERIES tag (`golang:1.26-trixie`) so they keep
// taking patch releases; an exact patch pin freezes the standard library (see
// services/elitea-llm-gateway/Containerfile). A series tag alone guarantees
// nothing, though: a stale cached image builds with whatever patch it holds.
// The `go` directive is what turns the floor into a guarantee. With the
// golang image's GOTOOLCHAIN=local, an older toolchain refuses the module
//
//	go: go.mod requires go >= 1.26.9 (running go 1.26.8; GOTOOLCHAIN=local)
//
// and with GOTOOLCHAIN=auto it fetches the floor toolchain instead. Either way
// no binary is built on a standard library below the floor.
//
// Raise a floor when govulncheck reports a reachable standard-library finding.
// Never lower one.
//
// scripts/ci/check-gateway-toolchain.sh reads this constant by name from an
// indented `stdlibFloorGo126 = "…"` line, so it stays inside a const block.
const (
	stdlibFloorGo126 = "1.26.9"
)

// toolchainFloors lists every Go module that ships an image, plus the
// workspace that CI and `task test` build with. containerfile is empty for the
// workspace: it ships no image.
var toolchainFloors = []struct {
	name          string
	goFile        string
	containerfile string
	floor         string
}{
	{name: "go.work", goFile: "go.work", floor: stdlibFloorGo126},
	{
		name:          "elitea-main",
		goFile:        "services/elitea-main/go.mod",
		containerfile: "services/elitea-main/Containerfile",
		floor:         stdlibFloorGo126,
	},
	{
		name:          "elitea-scheduler",
		goFile:        "services/elitea-scheduler/go.mod",
		containerfile: "services/elitea-scheduler/Containerfile",
		floor:         stdlibFloorGo126,
	},
	{
		name:          "elitea-subapp-host",
		goFile:        "services/elitea-subapp-host/go.mod",
		containerfile: "services/elitea-subapp-host/Containerfile",
		floor:         stdlibFloorGo126,
	},
	{
		name:          "elitea-llm-gateway",
		goFile:        "services/elitea-llm-gateway/go.mod",
		containerfile: "services/elitea-llm-gateway/Containerfile",
		floor:         stdlibFloorGo126,
	},
}

var (
	goDirective  = regexp.MustCompile(`(?m)^go\s+(\S+)\s*$`)
	golangImage  = regexp.MustCompile(`(?m)^\s*FROM\s+(?:--\S+\s+)*golang:([0-9][0-9.]*)(?:-\S*)?(?:\s|$)`)
	toolchainKey = regexp.MustCompile(`(?m)^toolchain\s+(\S+)\s*$`)
)

func TestEveryShippedGoBuildIsAtOrAboveTheStdlibSecurityFloor(t *testing.T) {
	root := repoRoot(t)

	for _, mod := range toolchainFloors {
		t.Run(mod.name, func(t *testing.T) {
			floor := "go" + mod.floor
			body := readFile(t, filepath.Join(root, mod.goFile))

			directives := goDirective.FindAllStringSubmatch(body, -1)
			if len(directives) != 1 {
				t.Fatalf("read %d `go` directives from %s, want exactly 1. "+
					"A comparison against nothing passes, so this is a failure and not a skip.",
					len(directives), mod.goFile)
			}
			declared := "go" + directives[0][1]
			if !version.IsValid(declared) {
				t.Fatalf("%s: `go %s` is not a Go version", mod.goFile, directives[0][1])
			}
			if version.Compare(declared, floor) < 0 {
				t.Errorf("%s declares go %s, below the standard-library security floor %s.\n"+
					"A builder image older than the floor would then compile it with a vulnerable standard library.\n"+
					"Run `go mod edit -go=%s` (or `go work edit -go=%s`).",
					mod.goFile, directives[0][1], mod.floor, mod.floor, mod.floor)
			}
			// A `toolchain` line below the floor would name a vulnerable toolchain
			// for GOTOOLCHAIN=auto to select.
			if match := toolchainKey.FindStringSubmatch(body); match != nil && match[1] != "default" {
				if !version.IsValid(match[1]) || version.Compare(match[1], floor) < 0 {
					t.Errorf("%s names toolchain %s, below the security floor %s", mod.goFile, match[1], mod.floor)
				}
			}

			if mod.containerfile == "" {
				return
			}
			images := golangImage.FindAllStringSubmatch(readFile(t, filepath.Join(root, mod.containerfile)), -1)
			if len(images) != 1 {
				t.Fatalf("read %d `FROM golang:<version>` builders from %s, want exactly 1",
					len(images), mod.containerfile)
			}
			image := "go" + images[0][1]
			if !version.IsValid(image) {
				t.Fatalf("%s: golang:%s does not name a Go version", mod.containerfile, images[0][1])
			}
			if version.Lang(image) != version.Lang(floor) {
				t.Errorf("%s builds with golang %s, outside the %s series of its floor %s",
					mod.containerfile, images[0][1], strings.TrimPrefix(version.Lang(floor), "go"), mod.floor)
			}
			// A series tag supplies the newest patch and the `go` directive
			// enforces the floor. An exact patch pin must itself meet it.
			if image != version.Lang(image) && version.Compare(image, floor) < 0 {
				t.Errorf("%s pins golang %s, below the security floor %s", mod.containerfile, images[0][1], mod.floor)
			}
		})
	}
}
