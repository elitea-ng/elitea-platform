package conversationretention

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric/noop"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/retention"
)

type fakePolicies struct {
	platform  retention.Policy
	overrides map[int64]retention.Policy
}

func (f *fakePolicies) PlatformPolicy(context.Context, retention.DataClass) (retention.Policy, error) {
	return f.platform, nil
}

func (f *fakePolicies) ProjectOverrides(context.Context, retention.DataClass) (map[int64]retention.Policy, error) {
	return f.overrides, nil
}

type conversation struct {
	project      int64
	id           int64
	lastActivity time.Time
	held         bool
	attachments  []AttachmentRef
}

// fakeStore keeps conversations in memory and applies the same decisions the
// SQL does: expired = last activity before the cutoff; held rows are never
// candidates and are counted.
type fakeStore struct {
	conversations []*conversation
	journal       []Cleanup
	tenantsListed int
	selects       int
	nextJournalID int64
}

func (f *fakeStore) RetentionTenantProjects(context.Context) ([]int64, error) {
	f.tenantsListed++
	seen := map[int64]bool{}
	var out []int64
	for _, c := range f.conversations {
		if !seen[c.project] {
			seen[c.project] = true
			out = append(out, c.project)
		}
	}
	return out, nil
}

func (f *fakeStore) ExpiredConversations(_ context.Context, projectID int64, cutoff time.Time, limit int) ([]Candidate, int64, error) {
	f.selects++
	var out []Candidate
	var held int64
	for _, c := range f.conversations {
		if c.project != projectID || !c.lastActivity.Before(cutoff) {
			continue
		}
		if c.held {
			held++
			continue
		}
		if len(out) < limit {
			out = append(out, Candidate{ID: c.id, LastActivity: c.lastActivity})
		}
	}
	return out, held, nil
}

func (f *fakeStore) DeleteExpiredConversation(_ context.Context, projectID, id int64, cutoff time.Time) (Outcome, int, error) {
	for i, c := range f.conversations {
		if c.project != projectID || c.id != id {
			continue
		}
		if !c.lastActivity.Before(cutoff) {
			return Active, 0, nil
		}
		if c.held {
			return Held, 0, nil
		}
		f.conversations = append(f.conversations[:i], f.conversations[i+1:]...)
		for _, a := range c.attachments {
			f.nextJournalID++
			f.journal = append(f.journal, Cleanup{ID: f.nextJournalID, ProjectID: projectID, ConversationID: id, Attachment: a})
		}
		return Deleted, len(c.attachments), nil
	}
	return Gone, 0, nil
}

func (f *fakeStore) ClaimAttachmentCleanup(_ context.Context, limit int, _ time.Duration) ([]Cleanup, error) {
	if len(f.journal) < limit {
		limit = len(f.journal)
	}
	return append([]Cleanup(nil), f.journal[:limit]...), nil
}

func (f *fakeStore) CompleteAttachmentCleanup(_ context.Context, ids []int64) error {
	done := map[int64]bool{}
	for _, id := range ids {
		done[id] = true
	}
	kept := f.journal[:0]
	for _, row := range f.journal {
		if !done[row.ID] {
			kept = append(kept, row)
		}
	}
	f.journal = kept
	return nil
}

func (f *fakeStore) FailAttachmentCleanup(context.Context, int64, string, time.Time) error {
	return nil
}

type purged struct {
	project int64
	refs    []AttachmentRef
}

func newSweeper(t *testing.T, policies *fakePolicies, store *fakeStore, gate Gate, purges *[]purged, purgeErr error) *Sweeper {
	t.Helper()
	purge := func(_ context.Context, projectID int64, refs []AttachmentRef) error {
		if purgeErr != nil {
			return purgeErr
		}
		*purges = append(*purges, purged{projectID, refs})
		return nil
	}
	s, err := New(policies, store, purge, gate, Config{Meter: noop.NewMeterProvider().Meter("test")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return testNow }
	return s
}

var testNow = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

func open(context.Context) (bool, error) { return false, nil }

func daysAgo(n int) time.Time { return testNow.Add(-time.Duration(n) * 24 * time.Hour) }
func ptr[T any](v T) *T       { return &v }

func oldConversations() []*conversation {
	return []*conversation{
		{project: 1, id: 10, lastActivity: daysAgo(4000), attachments: []AttachmentRef{{Bucket: "attachments", Name: "a.pdf"}}},
		{project: 1, id: 11, lastActivity: daysAgo(1)},
		{project: 2, id: 20, lastActivity: daysAgo(4000)},
	}
}

// The seeded default (conversation: keep forever, no overrides) deletes
// nothing and reads no tenant.
func TestDefaultPolicyDeletesNothing(t *testing.T) {
	store := &fakeStore{conversations: oldConversations()}
	var purges []purged
	s := newSweeper(t, &fakePolicies{platform: retention.Policy{
		Class: retention.ClassConversation, AllowProjectOverride: true,
	}}, store, open, &purges, nil)

	stats, err := s.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Deleted != 0 || len(store.conversations) != 3 || len(purges) != 0 {
		t.Fatalf("default policy deleted something: %+v, left %d", stats, len(store.conversations))
	}
	if store.tenantsListed != 0 || store.selects != 0 {
		t.Fatalf("default policy read tenants (%d) or candidates (%d)", store.tenantsListed, store.selects)
	}
}

// A conversation past the maximum is deleted, its attachment is journalled in
// the delete and the drain purges the bytes; a recent one survives.
func TestConversationPastMaxIsDeletedWithAttachmentsJournalled(t *testing.T) {
	store := &fakeStore{conversations: oldConversations()}
	var purges []purged
	s := newSweeper(t, &fakePolicies{platform: retention.Policy{
		Class: retention.ClassConversation, MaxAgeDays: ptr(365),
	}}, store, open, &purges, nil)

	stats, err := s.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Deleted != 2 || stats.Journalled != 1 || stats.Purged != 1 {
		t.Fatalf("stats = %+v, want 2 deleted, 1 journalled, 1 purged", stats)
	}
	if len(store.conversations) != 1 || store.conversations[0].id != 11 {
		t.Fatalf("survivors = %+v, want only the recent conversation 11", store.conversations)
	}
	if len(purges) != 1 || purges[0].project != 1 || purges[0].refs[0].Name != "a.pdf" {
		t.Fatalf("purges = %+v", purges)
	}
	if len(store.journal) != 0 {
		t.Fatalf("journal not drained: %+v", store.journal)
	}
	if stats.LagSeconds <= 0 {
		t.Fatal("lag was not reported for an expired backlog")
	}
}

// A failed purge leaves the journal row for the next pass; the conversation
// stays deleted.
func TestFailedPurgeStaysJournalled(t *testing.T) {
	store := &fakeStore{conversations: oldConversations()[:1]}
	var purges []purged
	s := newSweeper(t, &fakePolicies{platform: retention.Policy{
		Class: retention.ClassConversation, MaxAgeDays: ptr(365),
	}}, store, open, &purges, errors.New("object store down"))

	stats, err := s.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Deleted != 1 || stats.Purged != 0 || stats.Errors != 1 || len(store.journal) != 1 {
		t.Fatalf("stats = %+v, journal = %d", stats, len(store.journal))
	}
}

// A held conversation survives and is counted.
func TestHeldConversationSurvives(t *testing.T) {
	conversations := oldConversations()
	conversations[0].held = true
	store := &fakeStore{conversations: conversations}
	var purges []purged
	s := newSweeper(t, &fakePolicies{platform: retention.Policy{
		Class: retention.ClassConversation, MaxAgeDays: ptr(365),
	}}, store, open, &purges, nil)

	stats, err := s.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.SkippedHold != 1 || stats.Deleted != 1 {
		t.Fatalf("stats = %+v, want 1 held and 1 deleted", stats)
	}
	for _, c := range store.conversations {
		if c.id == 10 {
			return
		}
	}
	t.Fatal("the held conversation was deleted")
}

// A project override applies only to its project when the platform keeps
// forever.
func TestOverrideAppliesOnlyToItsProject(t *testing.T) {
	store := &fakeStore{conversations: oldConversations()}
	var purges []purged
	s := newSweeper(t, &fakePolicies{
		platform: retention.Policy{Class: retention.ClassConversation, AllowProjectOverride: true},
		overrides: map[int64]retention.Policy{
			2: {Class: retention.ClassConversation, ProjectID: ptr(int64(2)), MaxAgeDays: ptr(30)},
		},
	}, store, open, &purges, nil)

	stats, err := s.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Deleted != 1 || stats.Projects != 1 || store.tenantsListed != 0 {
		t.Fatalf("stats = %+v, tenants listed %d", stats, store.tenantsListed)
	}
	for _, c := range store.conversations {
		if c.project == 2 {
			t.Fatal("project 2's expired conversation survived its override")
		}
	}
}

// Maintenance pauses the sweep, and an unreadable switch deletes nothing.
func TestMaintenancePausesTheSweep(t *testing.T) {
	policies := &fakePolicies{platform: retention.Policy{Class: retention.ClassConversation, MaxAgeDays: ptr(1)}}
	for name, gate := range map[string]Gate{
		"window open": func(context.Context) (bool, error) { return true, nil },
		"unreadable":  func(context.Context) (bool, error) { return false, errors.New("db down") },
	} {
		store := &fakeStore{conversations: oldConversations()}
		var purges []purged
		s := newSweeper(t, policies, store, gate, &purges, nil)
		stats, err := s.Sweep(context.Background())
		if name == "window open" && (err != nil || !stats.Suppressed) {
			t.Fatalf("%s: stats %+v err %v, want suppressed", name, stats, err)
		}
		if name == "unreadable" && err == nil {
			t.Fatalf("%s: want an error", name)
		}
		if len(store.conversations) != 3 || store.selects != 0 {
			t.Fatalf("%s: the sweep touched conversations", name)
		}
		outcome, _ := s.Pass(context.Background())
		if name == "window open" && outcome != "maintenance" {
			t.Fatalf("%s: Pass outcome %q", name, outcome)
		}
	}
}

func TestPassBudgetTruncates(t *testing.T) {
	var conversations []*conversation
	for i := range 5 {
		conversations = append(conversations, &conversation{project: 1, id: int64(100 + i), lastActivity: daysAgo(400)})
	}
	store := &fakeStore{conversations: conversations}
	var purges []purged
	s := newSweeper(t, &fakePolicies{platform: retention.Policy{
		Class: retention.ClassConversation, MaxAgeDays: ptr(365),
	}}, store, open, &purges, nil)
	s.cfg.BatchSize, s.cfg.MaxPerPass = 2, 3
	stats, err := s.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Deleted != 3 || !stats.Truncated || len(store.conversations) != 2 {
		t.Fatalf("stats = %+v, left %d", stats, len(store.conversations))
	}
}
