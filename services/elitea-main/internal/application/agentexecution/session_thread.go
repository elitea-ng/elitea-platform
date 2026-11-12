package agentexecution

import "strconv"

// The worker keys an agent's durable session by the frozen request's thread
// id (with the actor and the project). A turn of a single-agent conversation,
// of an ad hoc chat and of every conversation before ADR-0050 keeps the
// conversation uuid as its thread, so existing sessions keep resolving.
//
// An agent participant of a conversation with several agents gets its own
// thread, `<conversation uuid>:<participant id>` (ADR-0050 decision 9b2, issue
// 1869), so two agents never write one session. A conversation that switches
// to this key starts a fresh session per agent on its next turn: the turn's
// chat history is authoritative, so the session is re-seeded from the
// reader's view, and no session is split or migrated.

// ParticipantSessionThreadID is the worker session thread of an agent
// participant of a conversation with several agents.
func ParticipantSessionThreadID(conversationUUID string, participantID int64) string {
	return conversationUUID + ":" + strconv.FormatInt(participantID, 10)
}

// SessionThreadID is the thread a turn of participantID freezes:
// the participant's own when the conversation has several agents, else the
// conversation's.
func SessionThreadID(conversationUUID string, participantID int64, multiAgent bool) string {
	if multiAgent && participantID > 0 {
		return ParticipantSessionThreadID(conversationUUID, participantID)
	}
	return conversationUUID
}

// IsParticipantSessionThread reports whether threadID is a participant's own
// session thread of the conversation, so no peer shares its session writer.
func IsParticipantSessionThread(threadID, conversationUUID string) bool {
	return len(threadID) > len(conversationUUID)+1 &&
		threadID[:len(conversationUUID)] == conversationUUID &&
		threadID[len(conversationUUID)] == ':'
}

// sessionThreadID is the thread a start freezes for participantID. The floor
// plan is non-nil exactly when the conversation has several agents (planFloor),
// so this is the one place a start decides per-participant threads.
func (plan *floorPlan) sessionThreadID(conversationUUID string, participantID int64) string {
	return SessionThreadID(conversationUUID, participantID, plan != nil)
}

// regenerationSessionThread is the thread a regeneration freezes: the one its
// answer persisted, else (an answer from before the thread was recorded) the
// conversation's.
func regenerationSessionThread(target CurrentRegenerationTarget) string {
	if target.ThreadID != "" {
		return target.ThreadID
	}
	return target.ConversationUUID
}
