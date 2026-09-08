// Package wikichat is the contract between the DeepWiki facade, which
// observes a wiki chat turn, and the tenant chat repository, which stores it.
//
// IT EXISTS BECAUSE THE OBVIOUS PLACE FOR IT IS A CYCLE. The support
// assistant declares its own store interface inside its handler package and
// has internal/infra/db/repos satisfy it, which works because nothing under
// repos imports that handler. The DeepWiki facade is not in that position:
// its credential resolver reaches providerhost/material, which imports repos,
// so a repository that named the facade's types would close the loop
// (repos → deepwiki → material → repos). The three types below are the whole
// contract, they depend on nothing, and both sides import them.
//
// A wiki chat is an ORDINARY TENANT CONVERSATION. What distinguishes it is
// two values — the conversation's `source` and its participant's
// `entity_name` — and both are named here so the writer and every reader
// filter on the same string.
package wikichat

import "context"

const (
	// Source is the `chat_conversations.source` a wiki chat is filed under.
	Source = "deepwiki"
	// ParticipantEntity is the `chat_participants.entity_name` the wiki
	// toolkit takes part under, and what the drawer's listing filters by
	// (`?source=deepwiki&entity_name=toolkit`).
	ParticipantEntity = "toolkit"
)

// Question is one recorded turn's question.
type Question struct {
	ProjectID int64
	UserID    int64
	// ChatKey is the browser's opaque handle on one conversation. It is
	// client-chosen, and choosing it is not a privilege: every statement that
	// resolves it also requires the row to belong to the caller.
	ChatKey     string
	ToolkitID   int64
	ToolkitName string
	// Capability is which of the two agents ran — `ask` or `research`. It is
	// stored on the turn rather than derived on read because the drawer's
	// toggle can move while a question is in flight, and a transcript must
	// say what actually ran.
	Capability   string
	Question     string
	InvocationID string
}

// Answer is the reply to the turn one invocation opened.
type Answer struct {
	ProjectID    int64
	UserID       int64
	InvocationID string
	Content      string
	// IsError marks a turn the provider ended with `Error` or `Stopped`. It
	// is recorded rather than dropped: a conversation that shows a question
	// and then nothing cannot be told apart from one whose answer was never
	// drained.
	IsError bool
}

// Store is where a recorded turn goes.
type Store interface {
	// RecordQuestion opens or resumes the caller's wiki conversation and
	// appends the question, keyed by the accepted invocation.
	RecordQuestion(ctx context.Context, question Question) error
	// RecordAnswer appends the answer to the turn that invocation opened,
	// and reports whether THIS call is the one that wrote it. False with no
	// error means the answer was already there — the ordinary outcome of a
	// second poll, and not a failure.
	RecordAnswer(ctx context.Context, answer Answer) (bool, error)
}
