// Package skilltrust decides when a skill's text is untrusted (ADR-0051
// amendment 1, A1.6).
//
// A skill is text. A skill written in another project and forked into this one
// is third-party text that would steer a model with the standing of an
// instruction, so on the chat surfaces (a message's picks, a conversation's
// pins, Try) the worker shows it as quoted untrusted data (ADR-0043 decision
// 4) until this project has reviewed that exact text and trusted it.
//
// The unit of trust is one skill and one revision: the SHA-256 digest of the
// instructions, the same value the worker freezes. Any change to the text is a
// new revision and is untrusted again. Trust per publisher was rejected: it
// would let a later upstream edit take effect with no reading.
//
// Skills attached to an agent version are not covered. They are under the
// agent's own change control (ADR-0040), so nothing here changes them.
package skilltrust

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// Revision is the digest a trust record names: the hex SHA-256 of the skill's
// instructions. It must equal the revision the start path freezes into the
// turn (agentexecution.currentInstructionRevision), which a test pins.
func Revision(instructions string) string {
	digest := sha256.Sum256([]byte(instructions))
	return hex.EncodeToString(digest[:])
}

// Foreign reports the project a skill's base version was forked from, when it
// is not projectID. parentProjectID is the text of the version meta's
// `parent_project_id` ("" when the meta has none). A skill made, imported or
// published in the project has no parent, or has this project as its parent,
// and is never foreign. A parent that is not a positive integer is treated as
// foreign with an unknown (zero) publisher: a malformed lineage must not read
// as trusted.
func Foreign(parentProjectID string, projectID int64) (publisherProjectID int64, foreign bool) {
	parent := strings.TrimSpace(parentProjectID)
	if parent == "" || parent == "null" {
		return 0, false
	}
	id, err := strconv.ParseInt(parent, 10, 64)
	if err != nil || id <= 0 {
		return 0, true
	}
	if id == projectID {
		return 0, false
	}
	return id, true
}

// Untrusted reports whether a skill's current text needs this project's trust
// and does not have it: it is foreign and none of activeRevisions (the
// revisions this project trusts for the skill, not revoked) is the revision of
// its instructions.
func Untrusted(foreign bool, instructions string, activeRevisions []string) bool {
	if !foreign {
		return false
	}
	revision := Revision(instructions)
	for _, trusted := range activeRevisions {
		if trusted == revision {
			return false
		}
	}
	return true
}
