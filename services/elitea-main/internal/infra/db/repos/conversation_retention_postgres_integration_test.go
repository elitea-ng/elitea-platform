package repos

// Conversation retention and legal hold against a real PostgreSQL (issue
// #1495). The age, the hold predicate and the journal are SQL; a fake would
// only agree with itself. Requires ELITEA_TEST_DATABASE_URL.

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	retentionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/conversationretention"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/retention"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// ageConversation moves a seeded conversation and its messages into the past.
func ageConversation(t *testing.T, repo *ConversationsRepo, numericID string, days int) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		`UPDATE p_1.chat_conversations SET created_at = created_at - make_interval(days => $2) WHERE id = $1::int`,
		`UPDATE p_1.chat_message_group SET created_at = created_at - make_interval(days => $2) WHERE conversation_id = $1::int`,
	} {
		if _, err := repo.pool.Exec(ctx, q, numericID, days); err != nil {
			t.Fatalf("age conversation: %v", err)
		}
	}
}

func retainedConversationExists(t *testing.T, repo *ConversationsRepo, numericID string) bool {
	t.Helper()
	var exists bool
	if err := repo.pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM p_1.chat_conversations WHERE id = $1::int)`, numericID).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	return exists
}

func mustID(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// The seeded platform defaults are today's behaviour: conversations keep
// forever, the gateway log keeps 92 days, admin audit 365.
func TestRetentionSeededDefaultsPreserveBehaviour(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	store := NewRetentionPolicyRepo(pool)
	ctx := context.Background()
	want := map[retention.DataClass]*int{
		retention.ClassConversation:      nil,
		retention.ClassAttachment:        nil,
		retention.ClassGatewayRequestLog: ptrInt(92),
		retention.ClassAdminAudit:        ptrInt(365),
		retention.ClassSecurityAudit:     nil,
	}
	for class, maxDays := range want {
		p, err := store.PlatformPolicy(ctx, class)
		if err != nil {
			t.Fatalf("%s: %v", class, err)
		}
		if (p.MaxAgeDays == nil) != (maxDays == nil) || (maxDays != nil && *p.MaxAgeDays != *maxDays) {
			t.Fatalf("%s: max = %v, want %v", class, p.MaxAgeDays, maxDays)
		}
		if p.UpdatedBy != nil {
			t.Fatalf("%s: a seeded row carries an editor", class)
		}
	}
}

func ptrInt(n int) *int { return &n }

// An expired conversation is deleted with the user delete's sequence and its
// attachments land in the journal; a recent one is not a candidate.
func TestExpiredConversationIsDeletedAndItsAttachmentsJournalled(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	ctx := context.Background()

	oldID, _, oldGroups := seedConversationWithParticipant(t, repo, "question")
	seedAttachmentItem(t, repo, oldGroups[0], "chat-attachments", "conv/old.pdf")
	ageConversation(t, repo, oldID, 400)
	recentID, _, _ := seedConversationWithParticipant(t, repo, "question")

	cutoff := time.Now().Add(-365 * 24 * time.Hour)
	candidates, err := repo.ExpiredConversations(ctx, 1, cutoff, 10)
	if err != nil {
		t.Fatal(err)
	}
	held, err := repo.HeldExpiredConversations(ctx, 1, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if held != 0 || len(candidates) != 1 || candidates[0].ID != mustID(t, oldID) {
		t.Fatalf("candidates = %+v held %d, want only %s", candidates, held, oldID)
	}

	outcome, journalled, err := repo.DeleteExpiredConversation(ctx, 1, mustID(t, oldID), cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != retentionapp.Deleted || journalled != 1 {
		t.Fatalf("outcome %s journalled %d, want deleted/1", outcome, journalled)
	}
	if retainedConversationExists(t, repo, oldID) || !retainedConversationExists(t, repo, recentID) {
		t.Fatal("wrong conversation deleted")
	}

	claimed, err := repo.ClaimAttachmentCleanup(ctx, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].Attachment.Name != "conv/old.pdf" || claimed[0].ProjectID != 1 {
		t.Fatalf("journal = %+v", claimed)
	}
	// Leased: a second claim inside the lease sees nothing.
	if again, err := repo.ClaimAttachmentCleanup(ctx, 10, time.Minute); err != nil || len(again) != 0 {
		t.Fatalf("second claim = %+v, %v", again, err)
	}
	if err := repo.CompleteAttachmentCleanup(ctx, []int64{claimed[0].ID}); err != nil {
		t.Fatal(err)
	}

	// A conversation that became active again is refused under the lock.
	outcome, _, err = repo.DeleteExpiredConversation(ctx, 1, mustID(t, recentID), cutoff)
	if err != nil || outcome != retentionapp.Active {
		t.Fatalf("recent conversation: %s, %v", outcome, err)
	}
}

// Holds on the project, the owner and an agent participant each keep an
// expired conversation, from the sweep and from the user's own delete; a
// released hold keeps nothing.
func TestLegalHoldKeepsConversation(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	holds := NewRetentionPolicyRepo(pool)
	ctx := context.Background()
	cutoff := time.Now().Add(-30 * 24 * time.Hour)

	agentConversation, agentUUID, _ := seedConversationWithParticipant(t, repo, "question")
	var agentParticipant int
	if err := pool.QueryRow(ctx, `INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta)
		VALUES (gen_random_uuid(), 'application', '{"id": 42, "project_id": 1}'::jsonb) RETURNING id`).Scan(&agentParticipant); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO p_1.chat_participant_mapping (conversation_id, participant_id, created_at)
		VALUES ($1::int, $2, now() AT TIME ZONE 'UTC')`, agentConversation, agentParticipant); err != nil {
		t.Fatal(err)
	}
	ageConversation(t, repo, agentConversation, 400)

	cases := []retention.Hold{
		{Scope: retention.HoldProject, ProjectID: ptrInt64(1), Reason: "matter 1", CreatedBy: 1},
		{Scope: retention.HoldUser, UserID: ptrInt64(7), Reason: "matter 2", CreatedBy: 1},
		{Scope: retention.HoldAgent, ProjectID: ptrInt64(1), AgentID: ptrInt64(42), Reason: "matter 3", CreatedBy: 1},
	}
	for _, hold := range cases {
		stored, err := holds.CreateHold(ctx, hold)
		if err != nil {
			t.Fatalf("%s: %v", hold.Scope, err)
		}
		candidates, err := repo.ExpiredConversations(ctx, 1, cutoff, 10)
		if err != nil {
			t.Fatal(err)
		}
		heldCount, err := repo.HeldExpiredConversations(ctx, 1, cutoff)
		if err != nil {
			t.Fatal(err)
		}
		if len(candidates) != 0 || heldCount != 1 {
			t.Fatalf("%s hold: candidates %+v held %d", hold.Scope, candidates, heldCount)
		}
		outcome, _, err := repo.DeleteExpiredConversation(ctx, 1, mustID(t, agentConversation), cutoff)
		if err != nil || outcome != retentionapp.Held {
			t.Fatalf("%s hold: sweep outcome %s, %v", hold.Scope, outcome, err)
		}
		_, err = repo.Delete(ctx, "1", agentUUID)
		var apiErr *apierr.APIError
		if !errors.As(err, &apiErr) || apiErr.Status != 409 {
			t.Fatalf("%s hold: user delete = %v, want 409", hold.Scope, err)
		}
		if _, err := holds.ReleaseHold(ctx, stored.ID, 1, "matter closed"); err != nil {
			t.Fatal(err)
		}
		if _, err := holds.ReleaseHold(ctx, stored.ID, 1, ""); err == nil {
			t.Fatal("a hold was released twice")
		}
	}
	if !retainedConversationExists(t, repo, agentConversation) {
		t.Fatal("the held conversation was deleted")
	}
	outcome, _, err := repo.DeleteExpiredConversation(ctx, 1, mustID(t, agentConversation), cutoff)
	if err != nil || outcome != retentionapp.Deleted {
		t.Fatalf("after release: %s, %v", outcome, err)
	}
}

func ptrInt64(n int64) *int64 { return &n }

// A project override outside the platform bounds is refused and not stored.
func TestProjectOverrideOutsideBoundsIsRefused(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	store := NewRetentionPolicyRepo(pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO centry.project (id) VALUES (1) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetPlatformPolicy(ctx, retention.Policy{
		Class: retention.ClassConversation, MinAgeDays: 30, MaxAgeDays: ptrInt(365), AllowProjectOverride: true,
	}, 1); err != nil {
		t.Fatal(err)
	}
	for _, days := range []int{10, 400} {
		_, err := store.SetProjectOverride(ctx, retention.Policy{
			Class: retention.ClassConversation, ProjectID: ptrInt64(1), MaxAgeDays: ptrInt(days),
		}, 1)
		var apiErr *apierr.APIError
		if !errors.As(err, &apiErr) || apiErr.Status != 400 {
			t.Fatalf("%d days: got %v, want 400", days, err)
		}
	}
	overrides, err := store.ProjectOverrides(ctx, retention.ClassConversation)
	if err != nil || len(overrides) != 0 {
		t.Fatalf("overrides = %+v, %v", overrides, err)
	}
	if _, err := store.SetProjectOverride(ctx, retention.Policy{
		Class: retention.ClassConversation, ProjectID: ptrInt64(1), MaxAgeDays: ptrInt(90),
	}, 1); err != nil {
		t.Fatalf("in-bounds override: %v", err)
	}
}
