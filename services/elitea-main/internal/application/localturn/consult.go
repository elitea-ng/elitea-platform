package localturn

// Consults of thread members, recorded at the caller's commit (ADR-0061
// decision 3e, issue 3065).
//
// A local agent that calls a nested agent which is also an agent of the
// thread consults it: the desktop runs the callee locally, inside the
// caller's turn, and the caller's commit sends each consult it ran. Main
// writes two side messages per consult, before the turn's answer: the
// question, authored by the turn's agent and addressed to the callee, and
// the reply, authored by the callee and addressed to the caller, both with
// cause `delegation` and visibility `side`, so a reload, the web viewer and
// mobile render the same side talk the desktop showed live. The store
// refuses (ErrInvalid) a callee that is not an `application` agent of the
// thread other than the turn's own, and any consult of a cloud member's
// turn: cloud members are leaves in v1. No new route: the commit carries
// them.

// Bounds of a turn's consults.
const (
	// MaxTurnConsults is the consults one turn runs (the host's limit).
	MaxTurnConsults = 3
	// MaxConsultQuestionBytes bounds a consult's task (the runtime's
	// MAX_TASK_CHARS characters, at most four bytes each).
	MaxConsultQuestionBytes = 32 * 1024
	// MaxConsultReplyBytes bounds a consult's reply (the runtime's
	// MAX_RESULT_BYTES, with room for the cut marker).
	MaxConsultReplyBytes = 16 * 1024
	// ConsultMetaKey is the side messages' meta key: the routed message the
	// consult belongs to and the turn that ran it, which handLocalFloor's
	// guard counts.
	ConsultMetaKey = "local_consult"
)

// Consult is one consult a turn ran.
type Consult struct {
	// ParticipantID is the consulted agent of the thread.
	ParticipantID int64  `json:"participant_id"`
	Question      string `json:"question"`
	Reply         string `json:"reply"`
}

func validConsults(consults []Consult) bool {
	if len(consults) > MaxTurnConsults {
		return false
	}
	for _, consult := range consults {
		if consult.ParticipantID <= 0 || consult.ParticipantID > 2147483647 ||
			!validText(consult.Question, MaxConsultQuestionBytes, false) ||
			!validText(consult.Reply, MaxConsultReplyBytes, true) {
			return false
		}
	}
	return true
}
