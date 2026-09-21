package agentexecution

import (
	"context"
	"errors"
	"testing"
)

// The audience rules (#977), which are the whole of the policy: every other
// part of this feature only persists what `mentionAudience` returns.

func TestMentionAudienceNamesEveryoneTaggedExactlyOnce(t *testing.T) {
	t.Parallel()

	// The same person named twice in ONE message is one recipient — the count
	// follows MESSAGES, not tags (ELITEA-0396). The route deduplicates before
	// this runs; this pins the rule at the layer that owns it, so a route that
	// stopped deduplicating cannot silently double-notify.
	got := mentionAudience(CurrentApplicationStartRequest{
		ActorUserID:      7,
		MentionedUserIDs: []int64{11, 11, 12},
	}, nil)
	if len(got) != 2 || got[0] != 11 || got[1] != 12 {
		t.Fatalf("mentionAudience = %v, want [11 12]", got)
	}
}

func TestMentionAudienceNeverNotifiesTheSender(t *testing.T) {
	t.Parallel()

	// ELITEA-0399's second half: a notification about your own message is
	// noise the reader cannot act on.
	got := mentionAudience(CurrentApplicationStartRequest{
		ActorUserID:      7,
		MentionedUserIDs: []int64{7},
	}, nil)
	if len(got) != 0 {
		t.Fatalf("the sender must never be notified of their own mention; got %v", got)
	}
}

func TestMentionAudienceForEveryoneUsesTheServersMembership(t *testing.T) {
	t.Parallel()

	// The ids the CLIENT sent alongside `@everyone` are ignored: `9` is not a
	// member, and a stale or tampered client must not be able to notify
	// somebody who is not in the project.
	got := mentionAudience(CurrentApplicationStartRequest{
		ActorUserID:      7,
		MentionsEveryone: true,
		MentionedUserIDs: []int64{9},
	}, []int64{7, 11, 12})
	if len(got) != 2 || got[0] != 11 || got[1] != 12 {
		t.Fatalf("mentionAudience(@everyone) = %v, want the project's members without the sender", got)
	}
}

func TestMentionAudienceDropsNonPositiveIDs(t *testing.T) {
	t.Parallel()

	got := mentionAudience(CurrentApplicationStartRequest{
		ActorUserID:      7,
		MentionedUserIDs: []int64{0, -3, 11},
	}, nil)
	if len(got) != 1 || got[0] != 11 {
		t.Fatalf("mentionAudience = %v, want [11]", got)
	}
}

/* ── the producer's own behaviour ──────────────────────────────────────── */

type recordingMentionWriter struct {
	rows    []MentionNotification
	members []int64
	// memberErr makes the `@everyone` resolution fail, to pin that the turn
	// is not affected and that no row is written from a half-known audience.
	memberErr error
	writeErr  error
	writes    int
}

func (writer *recordingMentionWriter) WriteChatMentionNotifications(
	_ context.Context, rows []MentionNotification,
) error {
	writer.writes++
	writer.rows = append(writer.rows, rows...)
	return writer.writeErr
}

func (writer *recordingMentionWriter) ProjectMemberUserIDs(_ context.Context, _ int64) ([]int64, error) {
	if writer.memberErr != nil {
		return nil, writer.memberErr
	}
	return writer.members, nil
}

func TestNotifyMentionedUsersWritesOneRowPerRecipient(t *testing.T) {
	t.Parallel()

	writer := &recordingMentionWriter{}
	service := (&CurrentApplicationStartService{}).WithMentionNotifications(writer)
	service.notifyMentionedUsers(context.Background(), CurrentApplicationStartRequest{
		ProjectID:        42,
		ActorUserID:      7,
		ConversationUUID: "11111111-1111-1111-1111-111111111111",
		QuestionID:       "22222222-2222-2222-2222-222222222222",
		MentionedUserIDs: []int64{11, 12},
	})

	if writer.writes != 1 {
		t.Fatalf("the audience must be written in ONE statement; got %d writes", writer.writes)
	}
	if len(writer.rows) != 2 {
		t.Fatalf("want one row per recipient; got %d", len(writer.rows))
	}
	for _, row := range writer.rows {
		if row.ProjectID != 42 || row.SenderUserID != 7 {
			t.Fatalf("row does not carry the project and the sender: %+v", row)
		}
		// The link back to the exact message is the point of the row: a
		// notification the reader cannot follow is barely a notification.
		if row.ConversationUUID == "" || row.MessageID == "" {
			t.Fatalf("row carries no way back to the message: %+v", row)
		}
	}
}

func TestNotifyMentionedUsersWritesNothingForAnOrdinaryMessage(t *testing.T) {
	t.Parallel()

	writer := &recordingMentionWriter{}
	service := (&CurrentApplicationStartService{}).WithMentionNotifications(writer)
	service.notifyMentionedUsers(context.Background(), CurrentApplicationStartRequest{
		ProjectID: 42, ActorUserID: 7,
	})
	if writer.writes != 0 {
		t.Fatalf("a message with no mentions must write nothing; got %d writes", writer.writes)
	}
}

func TestNotifyMentionedUsersSurvivesAFailedWrite(t *testing.T) {
	t.Parallel()

	// BEST EFFORT, and the assertion is that this returns at all: the message
	// is already admitted and stored by the time this runs, so a notification
	// that cannot be written must cost the mention and never the turn.
	writer := &recordingMentionWriter{writeErr: errors.New("nope")}
	service := (&CurrentApplicationStartService{}).WithMentionNotifications(writer)
	service.notifyMentionedUsers(context.Background(), CurrentApplicationStartRequest{
		ProjectID: 42, ActorUserID: 7, MentionedUserIDs: []int64{11},
	})
}

func TestNotifyMentionedUsersWritesNothingWhenMembershipCannotBeResolved(t *testing.T) {
	t.Parallel()

	// An `@everyone` whose membership read failed must notify NOBODY rather
	// than fall back to the client's list — the fallback is exactly the
	// tampering this design refuses.
	writer := &recordingMentionWriter{memberErr: errors.New("nope")}
	service := (&CurrentApplicationStartService{}).WithMentionNotifications(writer)
	service.notifyMentionedUsers(context.Background(), CurrentApplicationStartRequest{
		ProjectID: 42, ActorUserID: 7, MentionsEveryone: true, MentionedUserIDs: []int64{11},
	})
	if writer.writes != 0 {
		t.Fatalf("a failed membership read must write nothing; got %d writes", writer.writes)
	}
}

func TestNotifyMentionedUsersIsInertWithoutAWriter(t *testing.T) {
	t.Parallel()

	// A service nobody attached the writer to behaves exactly as it did before
	// #977 — which is what every constructor in this package's older tests
	// produces.
	service := &CurrentApplicationStartService{}
	service.notifyMentionedUsers(context.Background(), CurrentApplicationStartRequest{
		ProjectID: 42, ActorUserID: 7, MentionedUserIDs: []int64{11},
	})
}
