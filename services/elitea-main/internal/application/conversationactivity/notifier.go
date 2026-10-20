// Package conversationactivity tells the conversation lists of the web app
// and the desktop that a conversation's activity status changed (client
// contract 1.12), so a row's glyph moves while the list is on screen without
// a poll of the list.
//
// PostgreSQL is the authority. A frame carries the conversation's state as
// read AFTER the change committed, never the change itself, so a late, lost
// or repeated frame cannot make a client wrong for longer than its next list
// read: the frame is a hint to patch a cached row, and the `changes_since`
// delta re-delivers the row on every turn transition (tenant/0153).
//
// The transport is the project SSE stream's live-update bus (NATS core,
// elitea.events.project.<id>.conversations), published by the replica that
// committed the change. Each frame names its audience; the stream relays it
// only to that audience and strips it.
package conversationactivity

import (
	"context"
	"log/slog"
	"time"
)

// EventType is the SSE event name of a frame.
const EventType = "conversation_activity"

// Frame is what one change tells a reader. It holds ids, a state and times:
// never a name, a message or a participant.
type Frame struct {
	ConversationID   int64  `json:"conversation_id"`
	ConversationUUID string `json:"conversation_uuid,omitempty"`
	// Source is the conversation's origin (`elitea`, `local_work`), so the
	// desktop patches its Local work list and its chat list apart.
	Source string `json:"source,omitempty"`
	// State, Since and LastActivityAt are the list row's `activity` and
	// `last_activity_at`. Absent on a read frame.
	State          string `json:"state,omitempty"`
	Since          string `json:"since,omitempty"`
	LastActivityAt string `json:"last_activity_at,omitempty"`
	// UnreadHint is true when a turn finished, failed or paused: an answer
	// arrived that a reader who does not have the conversation open has not
	// read. False on a read frame (the reader read it on another device).
	// Absent when a turn starts. A client applies a true hint only when
	// LastActivityAt is newer than the row it holds, so a repeated frame
	// does not make a row unread again after the reader opened it.
	UnreadHint *bool `json:"unread_hint,omitempty"`
	// ReadAt is the reader's new read marker, on a read frame only.
	ReadAt string `json:"read_at,omitempty"`
}

// Audience is who may receive a frame.
type Audience struct {
	// Project: every caller of the project stream who may list
	// conversations (a public conversation).
	Project bool
	// Users: these user ids only (a private conversation's user
	// participants and the project admins who list it, or the reader of a
	// read frame).
	Users []int64
}

// Snapshot is a conversation's committed state, as the store reads it.
type Snapshot struct {
	ProjectID int64
	Audience  Audience
	Frame     Frame
	// Hidden conversations (support transcripts, wiki chats) are on no list:
	// no frame is sent for them.
	Hidden bool
}

// Store reads the committed state of the conversation a change touched.
// found is false when the conversation does not exist (any more) or the
// reference names nothing.
type Store interface {
	ConversationByUUID(ctx context.Context, projectID int64, conversationUUID string) (Snapshot, bool, error)
	ConversationByExecution(ctx context.Context, executionID string) (Snapshot, bool, error)
	ConversationByMessage(ctx context.Context, projectID int64, messageUUID string) (Snapshot, bool, error)
}

// Bus publishes a frame to its audience.
type Bus interface {
	Publish(ctx context.Context, projectID int64, audience Audience, frame Frame) error
}

// readTimeout bounds one read of the committed state.
const readTimeout = 5 * time.Second

// maxInFlight bounds the detached reads one replica runs at a time. A change
// that finds no slot is dropped and logged: the delta and the next list read
// repair it, and a burst of turns must not queue unbounded database reads.
const maxInFlight = 64

// Notifier turns committed changes into frames. Every method is safe on a nil
// Notifier (no live-update plane: nothing is sent).
type Notifier struct {
	store  Store
	bus    Bus
	logger *slog.Logger
	slots  chan struct{}
	// sync runs the reads on the caller's goroutine (tests).
	sync bool
}

// Option configures a Notifier.
type Option func(*Notifier)

// Synchronous makes every notification run before its method returns. For
// tests: production notifications are detached from the caller.
func Synchronous() Option { return func(n *Notifier) { n.sync = true } }

// New builds a Notifier. It answers nil when store or bus is nil.
func New(store Store, bus Bus, logger *slog.Logger, opts ...Option) *Notifier {
	if store == nil || bus == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	n := &Notifier{store: store, bus: bus, logger: logger, slots: make(chan struct{}, maxInFlight)}
	for _, opt := range opts {
		opt(n)
	}
	return n
}

// ConversationChanged: a turn of the conversation was admitted (it runs).
func (n *Notifier) ConversationChanged(ctx context.Context, projectID int64, conversationUUID string) {
	if n == nil || projectID <= 0 || conversationUUID == "" {
		return
	}
	n.run(ctx, "conversation", func(ctx context.Context) (Snapshot, bool, error) {
		return n.store.ConversationByUUID(ctx, projectID, conversationUUID)
	})
}

// ExecutionChanged: an agent execution's result was projected or the
// execution settled (it finished, failed, was cancelled or paused).
func (n *Notifier) ExecutionChanged(ctx context.Context, executionID string) {
	if n == nil || executionID == "" {
		return
	}
	n.run(ctx, "execution", func(ctx context.Context) (Snapshot, bool, error) {
		return n.store.ConversationByExecution(ctx, executionID)
	})
}

// MessageChanged: the turn that answers with the message group was stopped.
func (n *Notifier) MessageChanged(ctx context.Context, projectID int64, messageUUID string) {
	if n == nil || projectID <= 0 || messageUUID == "" {
		return
	}
	n.run(ctx, "message", func(ctx context.Context) (Snapshot, bool, error) {
		return n.store.ConversationByMessage(ctx, projectID, messageUUID)
	})
}

// Read tells the reader's other devices that the reader read the
// conversation up to readAt. Synchronous: the read route already wrote it.
func (n *Notifier) Read(ctx context.Context, projectID, readerID, conversationID int64, conversationUUID string, readAt string) {
	if n == nil || projectID <= 0 || readerID <= 0 || conversationID <= 0 {
		return
	}
	read := false
	frame := Frame{ConversationID: conversationID, ConversationUUID: conversationUUID, UnreadHint: &read, ReadAt: readAt}
	if err := n.bus.Publish(context.WithoutCancel(ctx), projectID, Audience{Users: []int64{readerID}}, frame); err != nil {
		n.logger.WarnContext(ctx, "conversation activity: publish read failed", "project_id", projectID, "err", err)
	}
}

func (n *Notifier) run(ctx context.Context, kind string, read func(context.Context) (Snapshot, bool, error)) {
	work := func() {
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), readTimeout)
		defer cancel()
		snapshot, found, err := read(readCtx)
		if err != nil {
			n.logger.WarnContext(readCtx, "conversation activity: read failed", "kind", kind, "err", err)
			return
		}
		if !found || snapshot.Hidden || snapshot.ProjectID <= 0 {
			return
		}
		frame := snapshot.Frame
		if frame.State != "" && frame.State != "running" {
			hint := true
			frame.UnreadHint = &hint
		}
		if err := n.bus.Publish(readCtx, snapshot.ProjectID, snapshot.Audience, frame); err != nil {
			n.logger.WarnContext(readCtx, "conversation activity: publish failed", "kind", kind,
				"project_id", snapshot.ProjectID, "conversation_id", frame.ConversationID, "err", err)
		}
	}
	if n.sync {
		work()
		return
	}
	select {
	case n.slots <- struct{}{}:
	default:
		n.logger.WarnContext(ctx, "conversation activity: dropped a change, too many in flight", "kind", kind)
		return
	}
	go func() {
		defer func() { <-n.slots }()
		defer func() {
			if r := recover(); r != nil {
				n.logger.Error("conversation activity: notification panicked", "kind", kind, "recovered", r)
			}
		}()
		work()
	}()
}
