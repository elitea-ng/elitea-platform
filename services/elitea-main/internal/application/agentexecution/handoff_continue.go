package agentexecution

// Send a running task to the cloud and continue (ADR-0045 decision 8,
// #1439).
//
// A desktop that completes a send with `continue` asks Main to start the
// cloud turn at once. Main starts it as a posted message on a handed-off
// thread starts one, with one difference: the turn's admission transaction
// also completes the send (HANDING_OFF → CLOUD_IDLE, the snapshot check and
// the event message) and starts the cloud turn (CLOUD_IDLE → CLOUD_RUNNING,
// bound to the new execution). The send, the person's message, the turn and
// its command commit together or not at all: a refused grant, a refused
// chain continuation or any other refused admission leaves the send
// HANDING_OFF, and the desktop may complete it again without `continue`.

import (
	"regexp"
	"strconv"

	"github.com/google/uuid"
)

// HandoffCompletion is the send a start completes in its admission
// transaction. It is set only by Main (the handoff's complete with
// `continue`); never from a request body.
type HandoffCompletion struct {
	HandoffID  string
	Epoch      int64
	HandoffRef string
	SHA        string
	// TakeBackWhenDone is the send's take_back_when_done (shared 0224).
	TakeBackWhenDone bool
}

// handoffSnapshotRef is workspacestore.RefHandoff1 (this package cannot
// import the store).
const handoffSnapshotRef = "refs/elitea/handoff/1"

var commitSHA1 = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Valid reports a well-formed completion. The handoff service checked it
// first; the admission checks it again before it writes anything.
func (c *HandoffCompletion) Valid() bool {
	return c != nil && validUUID(c.HandoffID) && c.Epoch > 0 &&
		c.HandoffRef == handoffSnapshotRef && commitSHA1.MatchString(c.SHA)
}

// Clone copies the completion.
func (c *HandoffCompletion) Clone() *HandoffCompletion {
	if c == nil {
		return nil
	}
	clone := *c
	return &clone
}

var handoffContinueNamespace = uuid.MustParse("0d6f3c1e-8b2a-4f5e-9a7d-3c4b5e6f7a81")

// HandoffContinueQuestionID is the question of the turn a send continues
// with: derived from the send, so a retried complete is the same request.
func HandoffContinueQuestionID(handoffID string, epoch int64) string {
	return uuid.NewSHA1(handoffContinueNamespace,
		[]byte("handoff-continue\x00"+handoffID+"\x00"+strconv.FormatInt(epoch, 10))).String()
}

// HandoffContinueResponseID is the answer of that turn
// (currentTurnUUID of the question, as every start derives it).
func HandoffContinueResponseID(questionID string) string {
	return currentTurnUUID(questionID, "response-message")
}
