package adminaccess_test

// The review routes (ADR-0059 R2.9a, issue 2523) behind the gates the
// production router puts in front of them, with a fake store: the credential
// first, the people involved refused, the outcome checked, and the revert
// list for the platform administrators only.

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	registry "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	console "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

type fakeReviews struct {
	elevation registry.ElevationRecord
	reviewers []int64
	reviews   []registry.ElevationReview
	marked    []uuid.UUID
	reverts   []registry.RevertEntry
}

func (f *fakeReviews) Elevation(_ context.Context, id uuid.UUID) (registry.ElevationRecord, error) {
	if id != f.elevation.ID {
		return registry.ElevationRecord{}, registry.ErrElevationNotFound
	}
	return f.elevation, nil
}

func (f *fakeReviews) Reviewers(context.Context, uuid.UUID) ([]int64, error) { return f.reviewers, nil }

func (f *fakeReviews) Items(context.Context, uuid.UUID) ([]registry.ReviewItem, error) {
	return nil, nil
}

func (f *fakeReviews) Changes(context.Context, uuid.UUID, int, int) ([]registry.ElevationAuditRow, int, error) {
	return nil, 0, nil
}

// Review applies the rules the ledger applies that the handler relies on.
func (f *fakeReviews) Review(_ context.Context, review registry.ElevationReview) (registry.ReviewResult, error) {
	f.reviews = append(f.reviews, review)
	if registry.InvolvedIn(f.elevation, review.ReviewerUserID) {
		return registry.ReviewResult{}, registry.ErrReviewerInvolved
	}
	if !slices.Contains(f.reviewers, review.ReviewerUserID) {
		return registry.ReviewResult{}, registry.ErrReviewerNotEligible
	}
	if f.elevation.ReviewState != registry.ElevationReviewPending {
		return registry.ReviewResult{}, registry.ErrElevationNotReviewable
	}
	reviewed := f.elevation
	reviewed.ReviewState = registry.ElevationReviewed
	return registry.ReviewResult{Elevation: reviewed}, nil
}

func (f *fakeReviews) Reverts(context.Context, bool, int, int) ([]registry.RevertEntry, int, error) {
	return f.reverts, len(f.reverts), nil
}

func (f *fakeReviews) MarkHandled(_ context.Context, id uuid.UUID, _ int64, _ time.Time) (bool, error) {
	f.marked = append(f.marked, id)
	return true, nil
}

func (f *fakeReviews) People(_ context.Context, ids []int64) (map[int64]console.Person, error) {
	people := map[int64]console.Person{}
	for _, id := range ids {
		people[id] = console.Person{ID: id, Name: "Person " + itoa(id)}
	}
	return people, nil
}

func reviewRouter(store console.ReviewStore, d *directory) http.Handler {
	central := func(permission string) func(http.Handler) http.Handler {
		return apimw.RequireCentralPermissions(d, auth.PermissionModeAdministration, permission)
	}
	r := chi.NewRouter()
	console.NewReviewsHandler(store).WithClock(func() time.Time { return requestClock }).
		Register(r, console.ReviewGates{
			Review: central(registry.AccessBreakglassReview),
			Manage: central(registry.AccessBreakglassManage),
		})
	return r
}

func pendingReview() *fakeReviews {
	return &fakeReviews{
		elevation: registry.ElevationRecord{
			ID: uuid.New(), UserID: requester, RequesterUserID: requester, ApproverUserID: approver,
			Path: registry.ElevationPathApproved, Level: registry.ElevationLevelAdmin,
			ReasonCode: registry.ElevationReasonIncident, RequestedAt: requestClock.Add(-2 * time.Hour),
			StartedAt: requestClock.Add(-2 * time.Hour), ExpiresAt: requestClock.Add(-time.Hour),
			EndedAt: requestClock.Add(-90 * time.Minute), EndReason: registry.ElevationEndHolder,
			ReviewState: registry.ElevationReviewPending,
		},
		reviewers: []int64{auditor},
	}
}

// The holder and the approver get 403 on the review; a holder of only
// security_auditor records an outcome.
func TestTheHolderAndTheApproverCannotReview(t *testing.T) {
	t.Parallel()
	store := pendingReview()
	d := requestDirectory()
	// The two involved people hold the review permission too, so the route
	// gate lets them through and the rule refuses them.
	d.users[requester] = append(d.users[requester], registry.RoleSecurityAuditor)
	d.users[approver] = append(d.users[approver], registry.RoleSecurityAuditor)
	h := reviewRouter(store, d)
	path := "/access/elevations/" + store.elevation.ID.String() + "/review"

	for _, person := range []int64{requester, approver} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			rec := call(t, h, browser(person), method, path, `{"outcome":"justified"}`)
			if rec.Code != http.StatusForbidden || refusalCode(t, rec) != console.CodeElevationReviewerInvolved {
				t.Errorf("%d %s: %d %s, want 403 %s", person, method, rec.Code, rec.Body, console.CodeElevationReviewerInvolved)
			}
		}
	}
	rec := call(t, h, browser(auditor), http.MethodPost, path, `{"outcome":"justified","note":"matches the ticket"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("the auditor's review: %d %s", rec.Code, rec.Body)
	}
	last := store.reviews[len(store.reviews)-1]
	if last.ReviewerUserID != auditor || last.Outcome != registry.ReviewOutcomeJustified || last.Note != "matches the ticket" {
		t.Fatalf("the review passed on: %+v", last)
	}
}

// A personal access token of a reviewer answers 403 before the gate; an
// unknown outcome answers 400 and never reaches the ledger.
func TestAReviewerTokenIsRefusedAndTheOutcomeIsChecked(t *testing.T) {
	t.Parallel()
	store := pendingReview()
	h := reviewRouter(store, requestDirectory())
	path := "/access/elevations/" + store.elevation.ID.String() + "/review"
	token := auth.User{ID: itoa(auditor), UserID: itoa(auditor), AuthType: "token", CredentialKind: auth.CredentialKindToken,
		TokenID: "77"}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		rec := call(t, h, token, method, path, `{"outcome":"justified"}`)
		if rec.Code != http.StatusForbidden || refusalCode(t, rec) != console.CodeElevationCredentialRefused {
			t.Errorf("a token %s: %d %s", method, rec.Code, rec.Body)
		}
	}
	rec := call(t, h, browser(auditor), http.MethodPost, path, `{"outcome":"fine"}`)
	if rec.Code != http.StatusBadRequest || refusalCode(t, rec) != console.CodeElevationRequestInvalid {
		t.Fatalf("an unknown outcome: %d %s", rec.Code, rec.Body)
	}
	if len(store.reviews) != 0 {
		t.Fatalf("a refused review reached the ledger: %+v", store.reviews)
	}
}

// A recorded reviewer whose review permission was taken away after the end
// still reads and writes THIS review; a person who is neither recorded nor
// holds the permission is refused by the gate.
func TestARecordedReviewerKeepsTheReview(t *testing.T) {
	t.Parallel()
	store := pendingReview()
	d := requestDirectory()
	d.users[auditor] = []string{registry.RoleLLMManager}
	h := reviewRouter(store, d)
	path := "/access/elevations/" + store.elevation.ID.String() + "/review"
	if rec := call(t, h, browser(auditor), http.MethodGet, path, ""); rec.Code != http.StatusOK {
		t.Fatalf("the recorded reviewer's read: %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, h, browser(auditor), http.MethodPost, path, `{"outcome":"justified"}`); rec.Code != http.StatusOK {
		t.Fatalf("the recorded reviewer's review: %d %s", rec.Code, rec.Body)
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		if rec := call(t, h, browser(bystander), method, path, `{"outcome":"justified"}`); rec.Code != http.StatusForbidden {
			t.Errorf("a bystander %s: %d %s, want 403", method, rec.Code, rec.Body)
		}
	}
}

// A review of an elevation in the terminal state unreviewed_no_reviewer
// (R2.9b) is refused with 409 elevation_not_reviewable.
func TestATerminalReviewIsRefused(t *testing.T) {
	t.Parallel()
	store := pendingReview()
	store.elevation.ReviewState = registry.ElevationUnreviewedNoReviewer
	h := reviewRouter(store, requestDirectory())
	path := "/access/elevations/" + store.elevation.ID.String() + "/review"
	rec := call(t, h, browser(auditor), http.MethodPost, path, `{"outcome":"justified"}`)
	if rec.Code != http.StatusConflict || refusalCode(t, rec) != console.CodeElevationNotReviewable {
		t.Fatalf("a review of an unreviewed_no_reviewer elevation: %d %s", rec.Code, rec.Body)
	}
}

// The revert list is readable by a platform administrator and an entry can
// be marked handled; an auditor can do neither.
func TestTheRevertListIsThePlatformAdministrators(t *testing.T) {
	t.Parallel()
	store := pendingReview()
	entry := uuid.New()
	store.reverts = []registry.RevertEntry{{ReviewItem: registry.ReviewItem{
		ID: entry, ElevationID: store.elevation.ID, Kind: registry.ReviewItemRevert, AuditEventID: 9, Action: "x",
		CreatedAt: requestClock,
	}, HolderUserID: requester}}
	h := reviewRouter(store, requestDirectory())
	handled := "/access/reverts/" + entry.String() + "/handled"

	if rec := call(t, h, browser(approver), http.MethodGet, "/access/reverts", ""); rec.Code != http.StatusOK {
		t.Fatalf("an admin reads the revert list: %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, h, browser(approver), http.MethodPost, handled, ""); rec.Code != http.StatusOK || len(store.marked) != 1 {
		t.Fatalf("an admin marks an entry: %d %s", rec.Code, rec.Body)
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		path := "/access/reverts"
		if method == http.MethodPost {
			path = handled
		}
		if rec := call(t, h, browser(auditor), method, path, ""); rec.Code != http.StatusForbidden {
			t.Errorf("an auditor %s %s: %d %s, want 403", method, path, rec.Code, rec.Body)
		}
	}
	if len(store.marked) != 1 {
		t.Fatalf("an auditor marked an entry: %v", store.marked)
	}
}
