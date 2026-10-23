package platformconfig

// The IN-APP SUPPORT ASSISTANT's resolved settings.
//
// It lives beside the announcements resolver for the same reason that one does:
// the writer is the admin Features page (`internal/api/v2/admin`), every reader
// is somewhere else (`internal/api/v2/supportassistant` today), and a flag whose
// only reader is the page that wrote it is the defect unit A14 was opened to
// remove. The support assistant is the sharpest example of that defect in this
// corpus — its section carried an `unavailable_reason` saying, in as many words,
// that turning the switch on "would change a flag no rendered surface reads".
//
// # Why the ids are resolved here rather than held in process state
//
// The reference keeps them in legacy module state
// (`legacy/plugins/support_assistant/module.py`: `self._support_project_id`,
// memoised on first read and written back through `descriptor.save_state()`),
// which is why every field on that page carried `requires_restart`. There is no
// restart signal to offer here and no module state to hold, so each call reads
// the rows. That is what lets an operator point the assistant at a different
// agent and have the next message use it.
//
// # Absence is not zero
//
// `ProjectID`, `AgentProjectID` and `AgentID` are 0 when the operator has not
// chosen one, and `Values.Int` is a two-value read precisely so that 0 cannot
// arrive from a stored `0`. The distinction is load-bearing: `Ready` refuses to
// serve the assistant at all while the agent is unchosen, and a zero that looked
// like a choice would send every support conversation at whatever row id 0
// resolves to.

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Defaults, transcribed from `legacy/plugins/support_assistant/config.yml` and
// its `admin_schema.json`. They are the values the widget renders before an
// operator has typed anything, so they are the reference's strings verbatim
// rather than new copy.
const (
	DefaultSupportAssistantName = "ELITEA Support"
	DefaultSupportWelcome       = "Hello! How can I help you today?"
	DefaultSupportPlaceholder   = "Type a message..."
)

// SupportAssistant is the resolved state of the support assistant section.
type SupportAssistant struct {
	// Enabled is the operator's switch. It is the ONLY thing an operator has to
	// set to make the section's other fields matter, and on its own it is not
	// enough to serve a conversation — see Ready.
	Enabled bool
	// ProjectID is the hidden project support conversations live in. Zero means
	// it has not been bootstrapped yet; the bootstrapper writes it back to this
	// key, so a deployment bootstraps once and every replica reads the same id.
	ProjectID int64
	// AgentProjectID is where the support AGENT lives, which need not be the
	// project the conversations live in — the reference allows an operator to
	// point at an agent maintained in a real project and keep the transcripts in
	// the hidden one. Zero means "the same project as ProjectID", which is the
	// fallback `sio/support.py` applies.
	AgentProjectID int64
	// AgentID is the application the assistant talks to. Zero means unchosen,
	// and unchosen means the assistant refuses rather than guesses.
	AgentID int64
	// Name, WelcomeMessage and Placeholder are presentation, echoed to the
	// widget by `GET /support_assistant/config`.
	Name           string
	WelcomeMessage string
	Placeholder    string
}

// AgentProject resolves which project the agent is read from, applying the
// reference's fallback (`agent_project_id or support_project_id`).
func (s SupportAssistant) AgentProject() int64 {
	if s.AgentProjectID > 0 {
		return s.AgentProjectID
	}
	return s.ProjectID
}

// Ready reports whether the assistant can actually serve a conversation.
//
// It is deliberately stricter than Enabled. An operator who flips the switch and
// stops has an assistant with nowhere to put transcripts and nothing to answer
// with, and the honest response to that is the reference's 503 — not a widget
// that opens, accepts a question and fails on send.
func (s SupportAssistant) Ready() bool {
	return s.Enabled && s.ProjectID > 0 && s.AgentID > 0
}

// LoadSupportAssistant resolves the section.
//
// Failure is permissive in the same direction every read in this package is: an
// unreadable store yields the DISABLED state, so a database hiccup hides a
// support widget rather than inventing one that cannot work. The error is
// returned alongside so a caller that wants to log it can.
func LoadSupportAssistant(ctx context.Context, pool *pgxpool.Pool) (SupportAssistant, error) {
	values, err := Load(ctx, pool, SectionSupportAssistant)
	if err != nil {
		return SupportAssistant{}, err
	}
	return values.SupportAssistant(), nil
}

// SupportAssistant decodes already-loaded rows. It is separate from
// LoadSupportAssistant so the admin write path can echo back exactly what a
// reader will see next, without a second query against rows it just wrote.
func (v Values) SupportAssistant() SupportAssistant {
	resolved := SupportAssistant{
		Enabled:        v.Bool(KeySupportAssistantEnabled, false),
		Name:           v.String(KeySupportAssistantName, DefaultSupportAssistantName),
		WelcomeMessage: v.String(KeySupportWelcomeMessage, DefaultSupportWelcome),
		Placeholder:    v.String(KeySupportPlaceholder, DefaultSupportPlaceholder),
	}
	resolved.ProjectID, _ = v.Int(KeySupportProjectID)
	resolved.AgentProjectID, _ = v.Int(KeySupportAgentProjectID)
	resolved.AgentID, _ = v.Int(KeySupportAgentID)
	return resolved
}
