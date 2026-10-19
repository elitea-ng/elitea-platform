package sdkpin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRootRelative is the path from this package directory to the repository
// root. The lock file the gateway compares itself against is named by its
// repository-relative path in sdk-pin.json, so it is read from disk.
const repoRootRelative = "../../../.."

// lockFile is the shape this test reads out of
// services/elitea-llm-gateway/internal/sdkpin/elitea-sdk.lock.json. Only the source block is
// needed.
type lockFile struct {
	Source struct {
		Revision       string   `json:"revision"`
		PatchRevisions []string `json:"patch_revisions"`
	} `json:"source"`
}

// TestPinParses rejects a pin whose shape would make the comparison below
// vacuous. Load() carries the rules; this asserts they run.
func TestPinParses(t *testing.T) {
	pin, err := Load()
	if err != nil {
		t.Fatalf("sdk-pin.json is not usable: %v", err)
	}
	if pin.VerifiedAgainst.Revision == "" {
		t.Fatal("Load returned an empty revision without an error")
	}
}

// TestPinMatchesTheSDKLockFile is the gate this package exists for.
//
// The gateway must stay compatible with elitea-sdk, and until this test existed
// an SDK bump could not fail any gateway gate: the gateway named the SDK
// nowhere. The lock file at services/elitea-llm-gateway/internal/sdkpin/elitea-sdk.lock.json
// OWNS the revision; sdk-pin.json states which revision the gateway's
// compatibility gates (scripts/contract/test_sdk_budget_contract.py and
// scripts/sdk-conformance/run.sh) were last run against.
//
// Moving the SDK therefore turns this job red with no gateway source change.
// The fix is to run both gates against the new revision and, only if they pass,
// update sdk-pin.json.
//
// Prevents: an SDK whose budget reader, base paths or LangChain dialect moved,
// merged while every gateway test still passed.
func TestPinMatchesTheSDKLockFile(t *testing.T) {
	pin, err := Load()
	if err != nil {
		t.Fatalf("sdk-pin.json is not usable: %v", err)
	}

	path := filepath.Join(repoRootRelative, filepath.FromSlash(pin.LockFile))
	raw, err := os.ReadFile(path) //nolint:gosec // a fixed repository path, not caller input
	if err != nil {
		// A missing lock file must FAIL, never skip. "The other side of the
		// comparison was not there" is the shape this repository keeps
		// mistaking for a pass.
		t.Fatalf("cannot read the lock file %s that sdk-pin.json names (%s): %v\n"+
			"Either the lock file moved — update lock_file in sdk-pin.json — or this "+
			"gate is comparing against nothing.", pin.LockFile, path, err)
	}

	var lock lockFile
	if err := json.Unmarshal(raw, &lock); err != nil {
		t.Fatalf("%s does not parse as JSON: %v", pin.LockFile, err)
	}
	if lock.Source.Revision == "" {
		t.Fatalf("%s carries no source.revision. An empty value would compare "+
			"unequal to every pin and equal to none, so this gate cannot run.", pin.LockFile)
	}
	if len(lock.Source.PatchRevisions) == 0 {
		t.Fatalf("%s carries no source.patch_revisions. The pinned SDK is a revision "+
			"PLUS cherry-picks, and an empty list here would let a dropped patch pass.",
			pin.LockFile)
	}

	if lock.Source.Revision != pin.VerifiedAgainst.Revision {
		t.Fatalf("the elitea-sdk pin moved and the gateway compatibility gates have not "+
			"been re-run.\n  %s names   %s\n  sdk-pin.json verified %s\n"+
			"Run scripts/contract/test_sdk_budget_contract.py and "+
			"services/elitea-llm-gateway/scripts/sdk-conformance/run.sh against the new "+
			"revision. Update sdk-pin.json ONLY after both pass — updating it first "+
			"turns a real incompatibility into a green build.",
			pin.LockFile, lock.Source.Revision, pin.VerifiedAgainst.Revision)
	}

	// The cherry-picks are part of the pinned tree, so a dropped or added patch
	// is an SDK move too. Compared as an ordered list because the workflow
	// applies them in order and a reorder can change the result.
	if len(lock.Source.PatchRevisions) != len(pin.VerifiedAgainst.PatchRevisions) {
		t.Fatalf("%s applies %d patch revision(s), sdk-pin.json was verified against %d",
			pin.LockFile, len(lock.Source.PatchRevisions), len(pin.VerifiedAgainst.PatchRevisions))
	}
	for i, want := range pin.VerifiedAgainst.PatchRevisions {
		if lock.Source.PatchRevisions[i] != want {
			t.Fatalf("patch revision %d differs: %s names %s, sdk-pin.json verified %s",
				i, pin.LockFile, lock.Source.PatchRevisions[i], want)
		}
	}
}

// ── The pin states CONTENT, not only identity (#567) ─────────────────────────

// TestThePinStatesTheContentTheGatesRead is the regression test for #567.
//
// Both compatibility tiers identified the SDK checkout by `git rev-parse HEAD`
// alone. That answers WHICH commit is checked out, not WHAT the working tree
// holds, so a dirty checkout kept the pinned HEAD and an operator could mint a
// green pin from a run that measured other bytes. `git rev-parse` also walks UP
// to an enclosing repository, so a hand-authored directory inside a pinned
// clone reports the pinned revision too.
//
// The fix is a content list. This test fails when that list is dropped or
// emptied, because both gates then hash nothing and pass for any tree.
func TestThePinStatesTheContentTheGatesRead(t *testing.T) {
	pin, err := Load()
	if err != nil {
		t.Fatalf("sdk-pin.json is not usable: %v", err)
	}
	if len(pin.VerifiedAgainst.Contents) == 0 {
		t.Fatal("verified_against.contents is empty. A revision names which commit is " +
			"checked out, not what the tree holds, so the gates would accept a dirty " +
			"checkout again (#567).")
	}
	for _, entry := range pin.VerifiedAgainst.Contents {
		if entry.Path == "" || entry.SHA256 == "" {
			t.Fatalf("contents entry %+v carries an empty field", entry)
		}
	}
}

// TestParseRefusesAPinTheGatesCannotActOn states each shape that would make the
// content check vacuous or unsafe, and sees it refused. A validator that only
// ever runs on the one good file in the tree is a validator nobody has watched
// fail.
func TestParseRefusesAPinTheGatesCannotActOn(t *testing.T) {
	const (
		revision = `"b5113a129329b85d23c2d5c2bf55f18e307414ec"`
		patches  = `["5c9409779ac0a55f8bf74f6ef438977089187a14"]`
		digest   = "510969b0fdd1b153ba96bc1e3673b6e3d2c7ceed95c7196d5a18b0511fd3273d"
	)
	document := func(contents string) []byte {
		return []byte(`{"lock_file":"lock.json","verified_against":{"revision":` +
			revision + `,"patch_revisions":` + patches + `,"contents":` + contents + `}}`)
	}

	good := document(`[{"path":"elitea_sdk/runtime/exceptions.py","sha256":"` + digest + `"}]`)
	if _, err := parse(good); err != nil {
		t.Fatalf("a well-formed pin was refused: %v", err)
	}

	for _, testCase := range []struct {
		name     string
		contents string
		why      string
	}{
		{
			name:     "absent",
			contents: `null`,
			why:      "a gate with no file to hash passes for every checkout",
		},
		{
			name:     "empty",
			contents: `[]`,
			why:      "the same hole, written as a list",
		},
		{
			name:     "no path",
			contents: `[{"path":"","sha256":"` + digest + `"}]`,
			why:      "a digest with no file names nothing to compare",
		},
		{
			name:     "absolute path",
			contents: `[{"path":"/etc/passwd","sha256":"` + digest + `"}]`,
			why:      "a gate must hash a file inside the SDK checkout",
		},
		{
			name:     "path leaves the checkout",
			contents: `[{"path":"elitea_sdk/../../secret.py","sha256":"` + digest + `"}]`,
			why:      "the same escape, written with a parent segment",
		},
		{
			name:     "short digest",
			contents: `[{"path":"a.py","sha256":"510969b0"}]`,
			why:      "a truncated digest matches nothing any hashing tool prints",
		},
		{
			name:     "upper-case digest",
			contents: `[{"path":"a.py","sha256":"` + strings.ToUpper(digest) + `"}]`,
			why:      "the gates compare digests as strings",
		},
		{
			name:     "one file listed twice",
			contents: `[{"path":"a.py","sha256":"` + digest + `"},{"path":"a.py","sha256":"` + digest + `"}]`,
			why:      "two digests for one file cannot both hold",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := parse(document(testCase.contents)); err == nil {
				t.Fatalf("parse accepted contents %s — %s", testCase.contents, testCase.why)
			}
		})
	}
}
