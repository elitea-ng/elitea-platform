package agentexecution

// start_refusal.go holds the refusals of an agent turn that a person is told
// about. The reasons an unsupported start carries internally are free-form
// (stage names, join descriptions) and go to the log only. A refusal reaches
// the 422 body only when it was raised with one of the typed codes below, and
// the sentence shown is the fixed one registered here, never the error text.

// StartRefusal is a typed reason code for a refusal shown to the sender.
type StartRefusal string

const (
	RefusalConversationMissing    StartRefusal = "conversation_missing"
	RefusalCallerNotParticipant   StartRefusal = "caller_not_participant"
	RefusalAgentNotInConversation StartRefusal = "agent_not_in_conversation"
	RefusalParticipantNotAgent    StartRefusal = "participant_not_agent"
	RefusalForeignProject         StartRefusal = "foreign_project"
	RefusalVersionMissing         StartRefusal = "version_missing"
	RefusalConversationTools      StartRefusal = "conversation_internal_tools"
	RefusalAgentTools             StartRefusal = "agent_internal_tools"
	RefusalConversationSummarized StartRefusal = "conversation_summarized"
	RefusalToolkitParticipant     StartRefusal = "toolkit_participant"
	RefusalAnswerInProgress       StartRefusal = "answer_in_progress"
	RefusalContextMessage         StartRefusal = "context_message"
	RefusalNoTurnRow              StartRefusal = "no_turn_row"
	RefusalModelUnavailable       StartRefusal = "model_unavailable"
)

// startRefusalSentences maps each code to the fixed sentence the sender reads.
// No id, name or other value from the request or conversation may appear here.
var startRefusalSentences = map[StartRefusal]string{
	RefusalConversationMissing:    "this conversation does not exist in this project",
	RefusalCallerNotParticipant:   "you are not a participant of this conversation",
	RefusalAgentNotInConversation: "the agent that should answer is not part of this conversation",
	RefusalParticipantNotAgent:    "the participant that should answer is not an agent",
	RefusalForeignProject:         "the agent belongs to another project and is not published to this one",
	RefusalVersionMissing:         "the version of the agent this conversation uses no longer exists; open the agent, save it, and start a new conversation",
	RefusalConversationTools:      "this conversation enables an internal tool this deployment does not run",
	RefusalAgentTools:             "the agent enables an internal tool this deployment does not run",
	RefusalConversationSummarized: "this conversation was summarized earlier and cannot be continued on this path; start a new conversation",
	RefusalToolkitParticipant:     "a toolkit is a participant of this conversation, which this path does not continue; start a new conversation with the agent",
	RefusalAnswerInProgress:       "the previous answer of this conversation is still being written; wait for it to finish",
	RefusalContextMessage:         "this conversation holds a context message this path cannot continue from; start a new conversation",
	RefusalNoTurnRow:              "the conversation, the agent or its saved version could not be matched for this turn",
	RefusalModelUnavailable:       "the agent's model is not available in this project and the project has no model to fall back to; ask an administrator to share a model with this project",
}

// UnsupportedStartRefused raises an unsupported start the sender is told the
// reason of. An unregistered code carries no sentence and is not shown.
func UnsupportedStartRefused(code StartRefusal) error {
	return &unsupportedCurrentAgentStart{reason: string(code), code: code}
}

// StartRefusalSentence is the fixed sentence of a code ("" when unregistered).
func StartRefusalSentence(code StartRefusal) string { return startRefusalSentences[code] }
