package agentexecution

import (
	"context"
	"strconv"
)

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

// conversationHasSeveralAgents reports whether the conversation has two or
// more agent participants, the condition for per-participant session
// threads. A service without a floor store reads none, as planFloor does.
func (service *CurrentApplicationStartService) conversationHasSeveralAgents(
	ctx context.Context, projectID int64, conversationUUID string,
) (bool, error) {
	if service.floor == nil {
		return false, nil
	}
	conversation, err := service.floor.ConversationFloor(ctx, projectID, conversationUUID)
	if err != nil {
		return false, err
	}
	return len(conversation.Agents) >= 2, nil
}
