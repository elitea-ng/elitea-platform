// Package sdkpin carries the gateway's declared relationship to elitea-sdk.
//
// The /llm surface has one first-class client. Its budget reader decides what a
// correct 402 body looks like, and its two base paths decide which routes must
// exist. The gateway used to declare no relationship to it, so an SDK bump
// could not fail a gateway gate — which is how the budget-scope defect shipped.
//
// The pinned revision itself belongs to
// internal/sdkpin/elitea-sdk.lock.json. This package holds the
// SEPARATE statement that the gateway's compatibility gates were run against
// that revision and passed. pin_test.go compares the two, so moving the SDK
// turns the gateway test job red with no gateway source change.
//
// The pin states CONTENT as well as identity (#567). A revision names which
// commit is checked out; it says nothing about what the working tree holds. The
// two compatibility gates read `git rev-parse HEAD` and stopped there, so a
// dirty checkout kept the pinned HEAD and a green run could mint a pin for
// bytes nobody measured. VerifiedAgainst.Contents records the sha256 of every
// SDK file those gates read, and both gates hash the checkout against it.
package sdkpin

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

//go:embed sdk-pin.json
var pinJSON []byte

// hexRevision matches a full 40-character git object name. A short name would
// still resolve for a human but cannot be compared byte-for-byte with the lock
// file, and a comparison that silently never matches is the failure mode this
// whole package exists to prevent.
var hexRevision = regexp.MustCompile(`^[0-9a-f]{40}$`)

// hexDigest matches a lowercase sha256. The gates compare the digest as a
// string, so an upper-case or truncated value would never match a digest any
// hashing tool prints, and the gate would fail for the wrong reason.
var hexDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Content is one SDK file the compatibility gates read, with the digest of the
// bytes they were verified against.
type Content struct {
	// Path is relative to the root of the SDK checkout, with forward slashes.
	Path string `json:"path"`
	// SHA256 is the lowercase hex digest of that file's bytes.
	SHA256 string `json:"sha256"`
}

// VerifiedAgainst is the SDK revision the gateway compatibility gates last
// passed against, with the patch revisions that were applied on top of it and
// the content of the files those gates read.
type VerifiedAgainst struct {
	Revision       string    `json:"revision"`
	PatchRevisions []string  `json:"patch_revisions"`
	Contents       []Content `json:"contents"`
	VerifiedOn     string    `json:"verified_on"`
}

// Pin is the parsed sdk-pin.json.
type Pin struct {
	// LockFile is the repository-relative path of the file that OWNS the
	// revision. It is stored rather than hard-coded in Go so that a move of the
	// lock file is one edit, and so the test reports the path it actually read.
	LockFile        string          `json:"lock_file"`
	VerifiedAgainst VerifiedAgainst `json:"verified_against"`
}

// Load parses the embedded pin and rejects a shape the comparison cannot use.
func Load() (Pin, error) {
	return parse(pinJSON)
}

// parse reads one pin document and rejects a shape the comparison cannot use.
//
// Every check below turns a value that would make the comparison VACUOUS into
// an error. An empty revision compares equal to nothing and unequal to
// everything depending on which side is empty; an empty patch list makes a
// set comparison trivially true; an empty content list lets any bytes pass.
// None may reach the caller.
//
// It takes the document as an argument so the tests can state a bad shape and
// see it refused. A validator that only ever runs on the one good file in the
// tree is a validator nobody has watched fail.
func parse(raw []byte) (Pin, error) {
	var p Pin
	if err := json.Unmarshal(raw, &p); err != nil {
		return Pin{}, fmt.Errorf("sdkpin: sdk-pin.json does not parse: %w", err)
	}
	if p.LockFile == "" {
		return Pin{}, fmt.Errorf("sdkpin: lock_file is empty, so the comparison has no other side to read")
	}
	if !hexRevision.MatchString(p.VerifiedAgainst.Revision) {
		return Pin{}, fmt.Errorf(
			"sdkpin: verified_against.revision %q is not a 40-character git object name",
			p.VerifiedAgainst.Revision)
	}
	if len(p.VerifiedAgainst.PatchRevisions) == 0 {
		return Pin{}, fmt.Errorf(
			"sdkpin: verified_against.patch_revisions is empty. The pinned SDK is a " +
				"revision PLUS cherry-picks; an empty list would compare equal to a lock " +
				"file that dropped them all")
	}
	seen := make(map[string]struct{}, len(p.VerifiedAgainst.PatchRevisions))
	for _, rev := range p.VerifiedAgainst.PatchRevisions {
		if !hexRevision.MatchString(rev) {
			return Pin{}, fmt.Errorf(
				"sdkpin: patch revision %q is not a 40-character git object name", rev)
		}
		if _, dup := seen[rev]; dup {
			return Pin{}, fmt.Errorf("sdkpin: patch revision %q is listed twice", rev)
		}
		seen[rev] = struct{}{}
	}
	if err := validateContents(p.VerifiedAgainst.Contents); err != nil {
		return Pin{}, err
	}
	return p, nil
}

// validateContents rejects a content list the gates cannot act on.
//
// The list is what makes the pin content-addressed (#567). An empty list gives
// the gates nothing to hash, and a gate that hashes nothing passes for every
// tree — which is the exact state this list was added to end. A path that
// escapes the checkout root would make a gate read a file outside the SDK, so
// it is refused here rather than at every call site.
func validateContents(contents []Content) error {
	if len(contents) == 0 {
		return fmt.Errorf(
			"sdkpin: verified_against.contents is empty. The gates would then hash " +
				"nothing and pass for any checkout, dirty or not, which is the hole " +
				"#567 closed")
	}
	seen := make(map[string]struct{}, len(contents))
	for _, entry := range contents {
		switch {
		case entry.Path == "":
			return fmt.Errorf("sdkpin: a contents entry has an empty path")
		case strings.Contains(entry.Path, `\`):
			return fmt.Errorf(
				"sdkpin: contents path %q uses a backslash; paths are relative and "+
					"slash-separated so every reader resolves the same file", entry.Path)
		case strings.HasPrefix(entry.Path, "/"):
			return fmt.Errorf(
				"sdkpin: contents path %q is absolute; it must be relative to the SDK "+
					"checkout root", entry.Path)
		}
		for _, segment := range strings.Split(entry.Path, "/") {
			if segment == ".." {
				return fmt.Errorf(
					"sdkpin: contents path %q leaves the SDK checkout; a gate must not "+
						"hash a file outside the tree it measures", entry.Path)
			}
		}
		if !hexDigest.MatchString(entry.SHA256) {
			return fmt.Errorf(
				"sdkpin: contents path %q carries sha256 %q, which is not a 64-character "+
					"lowercase hex digest", entry.Path, entry.SHA256)
		}
		if _, dup := seen[entry.Path]; dup {
			return fmt.Errorf(
				"sdkpin: contents path %q is listed twice; two digests for one file "+
					"cannot both be checked", entry.Path)
		}
		seen[entry.Path] = struct{}{}
	}
	return nil
}

// MustLoad is Load for callers that cannot report an error. It panics, which is
// correct here: the file is embedded at build time, so a failure is a broken
// build rather than a runtime condition.
func MustLoad() Pin {
	p, err := Load()
	if err != nil {
		panic(err)
	}
	return p
}
