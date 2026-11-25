package patexpiry_test

// The pass's own arithmetic and its stopping rule, with a fake store — the
// half no database can show, because "the store said it already notified this
// one" and "the store failed" are indistinguishable in a produced-row count.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/patexpiry"
)

type fakeStore struct {
	candidates []patexpiry.Candidate
	listErr    error
	// produced answers NotifyPATExpiring in order; a short list repeats its
	// last value.
	produced  []bool
	notifyErr error
	// calls records what was asked for, so a window that never reached the
	// store is visible.
	listedWithin time.Duration
	notifyCalls  int
}

func (f *fakeStore) ListPATsNeedingExpiryNotice(
	_ context.Context, _ time.Time, within time.Duration, _ int32,
) ([]patexpiry.Candidate, error) {
	f.listedWithin = within
	return f.candidates, f.listErr
}

func (f *fakeStore) NotifyPATExpiring(
	_ context.Context, _ patexpiry.Candidate, _ time.Time,
) (bool, error) {
	index := f.notifyCalls
	f.notifyCalls++
	if f.notifyErr != nil {
		return false, f.notifyErr
	}
	if len(f.produced) == 0 {
		return true, nil
	}
	if index >= len(f.produced) {
		index = len(f.produced) - 1
	}
	return f.produced[index], nil
}

func candidates(n int) []patexpiry.Candidate {
	rows := make([]patexpiry.Candidate, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, patexpiry.Candidate{TokenID: int64(i + 1), UserID: 1, Name: "key"})
	}
	return rows
}

func TestRunCountsProducedAndSkippedSeparately(t *testing.T) {
	store := &fakeStore{candidates: candidates(3), produced: []bool{true, false, true}}
	notifier, err := patexpiry.New(store)
	if err != nil {
		t.Fatal(err)
	}

	result, err := notifier.Run(context.Background(), time.Now(), time.Hour)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// A single "produced" number would report 2 here and 2 for a pass that
	// examined 2 and skipped none — which is the difference between the dedupe
	// holding and the candidate query having gone blind.
	if result.Examined != 3 || result.Produced != 2 || result.Skipped != 1 {
		t.Errorf("result = %+v, want examined 3, produced 2, skipped 1", result)
	}
}

func TestRunStopsAtTheFirstWriteFailure(t *testing.T) {
	store := &fakeStore{candidates: candidates(4), notifyErr: errors.New("mark could not be written")}
	notifier, err := patexpiry.New(store)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := notifier.Run(context.Background(), time.Now(), time.Hour); err == nil {
		t.Fatal("Run reported success after a write failure")
	}
	// Carrying on past a failed MARK is how a sweep produces the duplicate the
	// mark exists to prevent, so the pass must stop at the first one.
	if store.notifyCalls != 1 {
		t.Errorf("the pass made %d write attempts after the first failure, want 1", store.notifyCalls)
	}
}

func TestRunReportsAFailedRead(t *testing.T) {
	store := &fakeStore{listErr: errors.New("relation does not exist")}
	notifier, err := patexpiry.New(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := notifier.Run(context.Background(), time.Now(), time.Hour); err == nil {
		t.Fatal("a failed candidate read was reported as an empty pass")
	}
}

func TestRunFallsBackToTheProductionWindow(t *testing.T) {
	for name, within := range map[string]time.Duration{"zero": 0, "negative": -time.Hour} {
		t.Run(name, func(t *testing.T) {
			store := &fakeStore{}
			notifier, err := patexpiry.New(store)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := notifier.Run(context.Background(), time.Now(), within); err != nil {
				t.Fatal(err)
			}
			// A window of nothing would silently announce nobody, for ever.
			if store.listedWithin != patexpiry.DefaultWindow {
				t.Errorf("listed with %v, want the production window %v", store.listedWithin, patexpiry.DefaultWindow)
			}
		})
	}
}

func TestNewRefusesANilStore(t *testing.T) {
	if _, err := patexpiry.New(nil); err == nil {
		t.Fatal("a notifier was built over no store")
	}
}

// inheritStore is the fake of the `inherit` half: the owners still without a
// delivery, and an outbox whose sender is a mailer fake. A queued notice is
// sent at once through mailer; an accepted one stores the delivery time on
// each listed token, a refused one stores nothing, as the access notice
// sender does (its real behaviour is in
// internal/adminaccess/notices_postgres_integration_test.go).
type inheritStore struct {
	owners    []patexpiry.InheritOwner
	mailer    *inheritMailer
	delivered map[int64]time.Time
	queued    int
	lists     int
	// failFor is an owner whose queue write fails.
	failFor int64
}

type inheritMailer struct {
	failing bool
	sent    []string
}

func (s *inheritStore) ListInheritTokenOwners(context.Context, time.Time, int32) ([]patexpiry.InheritOwner, error) {
	s.lists++
	var due []patexpiry.InheritOwner
	for _, owner := range s.owners {
		for _, token := range owner.Tokens {
			if _, ok := s.delivered[token.ID]; !ok {
				due = append(due, owner)
				break
			}
		}
	}
	return due, nil
}

func (s *inheritStore) QueueInheritTokenNotice(_ context.Context, owner patexpiry.InheritOwner, sunsetAfter string, now time.Time) (bool, error) {
	if sunsetAfter == "" {
		return false, errors.New("queued with no sunset date")
	}
	if owner.UserID == s.failFor {
		return false, errors.New("the outbox refused the row")
	}
	s.queued++
	if s.mailer.failing {
		return true, nil
	}
	s.mailer.sent = append(s.mailer.sent, owner.Email)
	for _, token := range owner.Tokens {
		s.delivered[token.ID] = now
	}
	return true, nil
}

func inheritOwners() []patexpiry.InheritOwner {
	return []patexpiry.InheritOwner{
		{UserID: 1, Email: "ada@example.com", Tokens: []patexpiry.InheritToken{{ID: 11, Name: "ci"}, {ID: 12, Name: "backup"}}},
		{UserID: 2, Email: "grace@example.com", Tokens: []patexpiry.InheritToken{{ID: 21}}},
	}
}

func newInheritNotifier(t *testing.T, store *inheritStore, sunsetAfter string) *patexpiry.Notifier {
	t.Helper()
	notifier, err := patexpiry.New(&fakeStore{})
	if err != nil {
		t.Fatal(err)
	}
	if err := notifier.WithInheritNotices(store, func() string { return sunsetAfter }); err != nil {
		t.Fatal(err)
	}
	return notifier
}

// One notice per owner of an `inherit` token, listing that owner's tokens,
// and the delivery time stored on each of them (ADR-0059 R3.15b).
func TestInheritPassSendsOneNoticePerOwnerAndStoresTheDelivery(t *testing.T) {
	store := &inheritStore{owners: inheritOwners(), mailer: &inheritMailer{}, delivered: map[int64]time.Time{}}
	now := time.Date(2027, 1, 10, 9, 0, 0, 0, time.UTC)
	result, err := newInheritNotifier(t, store, "2027-01-31").Run(context.Background(), now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if result.InheritOwners != 2 || result.InheritQueued != 2 {
		t.Fatalf("result = %+v, want two owners, two notices", result)
	}
	if got := store.mailer.sent; len(got) != 2 || got[0] != "ada@example.com" || got[1] != "grace@example.com" {
		t.Fatalf("sent = %v, want one notice per owner", got)
	}
	for _, id := range []int64{11, 12, 21} {
		if !store.delivered[id].Equal(now) {
			t.Errorf("token %d delivery = %v, want %v", id, store.delivered[id], now)
		}
	}
	// Delivered: nobody is listed again.
	again, err := newInheritNotifier(t, store, "2027-01-31").Run(context.Background(), now.Add(time.Hour), time.Hour)
	if err != nil || again.InheritOwners != 0 || len(store.mailer.sent) != 2 {
		t.Fatalf("second pass = %+v, %v; sent %v", again, err, store.mailer.sent)
	}
}

// While the sunset date is empty the half sends nothing and reads nothing.
func TestInheritPassDoesNothingWithoutTheSunsetDate(t *testing.T) {
	store := &inheritStore{owners: inheritOwners(), mailer: &inheritMailer{}, delivered: map[int64]time.Time{}}
	result, err := newInheritNotifier(t, store, "").Run(context.Background(), time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if store.lists != 0 || store.queued != 0 || len(store.mailer.sent) != 0 || result.InheritOwners != 0 {
		t.Fatalf("with no sunset date: lists %d, queued %d, sent %v, result %+v",
			store.lists, store.queued, store.mailer.sent, result)
	}
}

// A failed send stores no delivery time, and the next pass retries.
func TestInheritPassRetriesAFailedSend(t *testing.T) {
	store := &inheritStore{owners: inheritOwners()[:1], mailer: &inheritMailer{failing: true}, delivered: map[int64]time.Time{}}
	now := time.Date(2027, 1, 10, 9, 0, 0, 0, time.UTC)
	if _, err := newInheritNotifier(t, store, "2027-01-31").Run(context.Background(), now, time.Hour); err != nil {
		t.Fatal(err)
	}
	if len(store.delivered) != 0 || len(store.mailer.sent) != 0 {
		t.Fatalf("a failed send stored %v", store.delivered)
	}
	store.mailer.failing = false
	retry := now.Add(15 * time.Minute)
	result, err := newInheritNotifier(t, store, "2027-01-31").Run(context.Background(), retry, time.Hour)
	if err != nil || result.InheritOwners != 1 {
		t.Fatalf("retry = %+v, %v", result, err)
	}
	if len(store.mailer.sent) != 1 || !store.delivered[11].Equal(retry) || !store.delivered[12].Equal(retry) {
		t.Fatalf("after the retry: sent %v, delivered %v", store.mailer.sent, store.delivered)
	}
}

func TestWithInheritNoticesRefusesMissingParts(t *testing.T) {
	notifier, err := patexpiry.New(&fakeStore{})
	if err != nil {
		t.Fatal(err)
	}
	if err := notifier.WithInheritNotices(nil, func() string { return "" }); err == nil {
		t.Error("no store was accepted")
	}
	if err := notifier.WithInheritNotices(&inheritStore{}, nil); err == nil {
		t.Error("no sunset date was accepted")
	}
}

// One owner whose queue write fails does not hold back the owners after
// them, and an owner with no address is counted, not queued.
func TestInheritPassCarriesOnPastAFailingOwner(t *testing.T) {
	owners := append(inheritOwners(), patexpiry.InheritOwner{UserID: 3, Email: " ", Tokens: []patexpiry.InheritToken{{ID: 31}}})
	store := &inheritStore{owners: owners, mailer: &inheritMailer{}, delivered: map[int64]time.Time{}, failFor: 1}
	result, err := newInheritNotifier(t, store, "2027-01-31").Run(context.Background(), time.Now(), time.Hour)
	if err == nil {
		t.Fatal("the failing owner was not reported")
	}
	if result.InheritOwners != 3 || result.InheritQueued != 1 || result.InheritUnreachable != 1 {
		t.Fatalf("result = %+v, want 3 owners, 1 queued, 1 unreachable", result)
	}
	if got := store.mailer.sent; len(got) != 1 || got[0] != "grace@example.com" {
		t.Fatalf("sent = %v, want the owner after the failing one", got)
	}
}

// A failing expiry half does not stop the `inherit` half.
func TestInheritPassRunsWhenTheExpiryHalfFails(t *testing.T) {
	store := &inheritStore{owners: inheritOwners(), mailer: &inheritMailer{}, delivered: map[int64]time.Time{}}
	notifier, err := patexpiry.New(&fakeStore{listErr: errors.New("the notifications table is gone")})
	if err != nil {
		t.Fatal(err)
	}
	if err := notifier.WithInheritNotices(store, func() string { return "2027-01-31" }); err != nil {
		t.Fatal(err)
	}
	result, err := notifier.Run(context.Background(), time.Now(), time.Hour)
	if err == nil {
		t.Fatal("the expiry failure was not reported")
	}
	if result.InheritQueued != 2 {
		t.Fatalf("result = %+v, want both inherit notices queued", result)
	}
}
