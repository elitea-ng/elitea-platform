package localturn

// Cloud child turns of a local turn (issue 1239, ADR-0029 decision 1): a
// nested agent of the agent a desktop runs locally is run as an ordinary
// cloud turn on a worker, in a child conversation of its own. The desktop
// never joins the worker protocol; it starts the child and reads its state
// over public HTTPS, under the live local turn it is running.

import (
	"context"
	"encoding/json"
	"errors"
)

// ChildConversationSource marks the child conversations (a
// `chat_conversations.source` value beside `elitea`, `mcp`, `local_work`).
// They are also `meta.is_hidden`, so the chat list leaves them out by
// default; `?source=local_child&hidden=only` lists them.
const ChildConversationSource = "local_child"

// ErrChildTurnNotFound: no child turn of this caller under this parent turn
// has that execution id.
var ErrChildTurnNotFound = errors.New("child turn not found")

// ChildConversationRequest is one child start to prepare.
type ChildConversationRequest struct {
	ProjectID   int64
	ActorUserID int64
	// ParentExecutionID is the live local turn the child runs under.
	ParentExecutionID string
	// IdempotencyKey is the desktop's key of this one call: a retry with it
	// finds the conversation (and the turn) the first attempt made.
	IdempotencyKey string
	// InputSHA256 is the digest of the child's input and target; the same
	// key with another input is a conflict.
	InputSHA256   string
	ApplicationID int64
	VersionID     int64
	// Name is the conversation's title.
	Name string
}

// ChildConversation is the conversation a child start runs in.
type ChildConversation struct {
	ConversationUUID string
	// ParticipantID is the child agent's participant, the start's target.
	ParticipantID int64
	// Created: this call made the conversation; the turn is not started yet.
	Created     bool
	InputSHA256 string
	// ExecutionID and ResponseMessageID are set once the turn was admitted
	// (by this key's earlier attempt, for a conversation not Created).
	ExecutionID       string
	ResponseMessageID string
}

// ChildToolCall is one tool call of a child turn, for its summary.
type ChildToolCall struct {
	Name    string
	IsError bool
}

// ChildUsage is the model usage the gateway recorded for a child turn's
// executions.
type ChildUsage struct {
	LLMCalls         int64
	PromptTokens     int64
	CompletionTokens int64
}

// ChildTurnState is what one read of a child turn sees: the response message
// group's projected state, the same discriminators the MCP `tools/call`
// bridge reads (internal/api/v2/mcp/execute.go, turnState).
type ChildTurnState struct {
	ConversationUUID  string
	ResponseMessageID string
	// ExecutionID is the group's current execution (a resumed turn runs a
	// continuation under a new one).
	ExecutionID string
	Streaming   bool
	// Removed: the response group is gone (a cancel removes an empty one).
	Removed bool
	IsError bool
	Failure string
	// Interrupt is the pending `hitl_interrupt` (the chat HITL shape), nil
	// when the turn is not paused for a decision.
	Interrupt            json.RawMessage
	AuthorizationPending bool
	OutputLimit          bool
	Text                 string
	ToolCalls            []ChildToolCall
	// Usage is nil when the request log could not be read.
	Usage *ChildUsage
}

// ChildTurnStore keeps the child conversations (*repos.LocalChildTurnsRepo).
type ChildTurnStore interface {
	// PrepareChildConversation answers the conversation of (parent, key):
	// the one an earlier attempt made, or a new hidden one with the caller
	// and the child agent as participants. Concurrent calls with the same
	// key serialise; only one creates.
	PrepareChildConversation(ctx context.Context, request ChildConversationRequest) (ChildConversation, error)
	// RecordChildStart stamps the admitted turn on its conversation.
	RecordChildStart(ctx context.Context, projectID int64, conversationUUID, executionID, responseMessageID string) error
	// DiscardChildConversation removes a child conversation whose turn was
	// never admitted (best effort).
	DiscardChildConversation(ctx context.Context, projectID int64, conversationUUID string)
	// ReadChildTurn reads the child turn the caller started under the parent
	// turn with that execution id, or ErrChildTurnNotFound.
	ReadChildTurn(ctx context.Context, projectID, actorUserID int64, parentExecutionID, childExecutionID string) (ChildTurnState, error)
}
