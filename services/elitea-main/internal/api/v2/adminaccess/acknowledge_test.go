package adminaccess_test

// The acknowledgement route (ADR-0059 R2.9c, issue 2604) behind the gate the
// production router puts in front of it, with a fake store: a holder of
// admin gets 403, a super_admin 200 on an unreviewed_no_reviewer elevation
// and 409 on any other state, and a token 403.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	registry "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	console "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// fakeAcknowledgements applies the ledger's rules the handler relies on.
type fakeAcknowledgements struct {
	elevation registry.ElevationRecord
	notes     []registry.ElevationNote
	noteID    uuid.UUID
}

func (f *fakeAcknowledgements) Acknowledge(_ context.Context, note registry.ElevationNote) (registry.NoteResult, error) {
	f.notes = append(f.notes, note)
	switch {
	case note.ID != f.elevation.ID:
		return registry.NoteResult{}, registry.ErrElevationNotFound
	case f.elevation.ReviewState != registry.ElevationUnreviewedNoReviewer:
		return registry.NoteResult{}, registry.ErrElevationNotAcknowledgeable
	case f.noteID != uuid.Nil:
		return registry.NoteResult{}, registry.ErrElevationAcknowledged
	}
	f.noteID = uuid.New()
	return registry.NoteResult{NoteID: f.noteID, Elevation: f.elevation}, nil
}

func acknowledgeRouter(store console.AcknowledgeStore, d *directory) http.Handler {
	r := chi.NewRouter()
	console.NewAcknowledgeHandler(store).Register(r,
		apimw.RequireCentralPermissions(d, auth.PermissionModeAdministration, registry.SuperAdminPermission))
	return r
}

func unreviewedElevation() *fakeAcknowledgements {
	store := pendingReview()
	elevation := store.elevation
	elevation.ReviewState = registry.ElevationUnreviewedNoReviewer
	return &fakeAcknowledgements{elevation: elevation}
}

func TestOnlyAStandingSuperAdminAcknowledgesAnUnreviewedElevation(t *testing.T) {
	t.Parallel()
	store := unreviewedElevation()
	h := acknowledgeRouter(store, requestDirectory())
	path := "/access/elevations/" + store.elevation.ID.String() + "/acknowledge"
	body := `{"note":"the holder was the only administrator on call"}`

	// A holder of admin, and the auditor, are refused by the gate.
	for _, person := range []int64{approver, auditor} {
		if rec := call(t, h, browser(person), http.MethodPost, path, body); rec.Code != http.StatusForbidden {
			t.Errorf("%d: %d %s, want 403", person, rec.Code, rec.Body)
		}
	}
	// A token of the super_admin is refused before the gate.
	token := auth.User{ID: itoa(superAdmin), UserID: itoa(superAdmin), AuthType: "token",
		CredentialKind: auth.CredentialKindToken, TokenID: "77"}
	if rec := call(t, h, token, http.MethodPost, path, body); rec.Code != http.StatusForbidden ||
		refusalCode(t, rec) != console.CodeElevationCredentialRefused {
		t.Errorf("a token: %d %s, want 403 %s", rec.Code, rec.Body, console.CodeElevationCredentialRefused)
	}
	if len(store.notes) != 0 {
		t.Fatalf("a refused acknowledgement reached the ledger: %+v", store.notes)
	}
	// An empty note is a 400.
	if rec := call(t, h, browser(superAdmin), http.MethodPost, path, `{"note":"  "}`); rec.Code != http.StatusBadRequest ||
		refusalCode(t, rec) != console.CodeElevationRequestInvalid {
		t.Errorf("an empty note: %d %s, want 400", rec.Code, rec.Body)
	}

	rec := call(t, h, browser(superAdmin), http.MethodPost, path, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("the super_admin's acknowledgement: %d %s", rec.Code, rec.Body)
	}
	var got struct {
		ElevationID  string `json:"elevation_id"`
		NoteID       string `json:"note_id"`
		State        string `json:"state"`
		Acknowledged bool   `json:"acknowledged"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.NoteID != store.noteID.String() || got.State != registry.ElevationUnreviewedNoReviewer || !got.Acknowledged {
		t.Fatalf("response = %+v, want the note id and the unchanged state", got)
	}
	last := store.notes[len(store.notes)-1]
	if last.AuthorUserID != superAdmin || last.Source != registry.NoteSourceConsole || !last.Acknowledge ||
		last.Text != "the holder was the only administrator on call" {
		t.Fatalf("the note passed on: %+v", last)
	}

	// A second one, and one on any other state, are 409.
	if rec := call(t, h, browser(superAdmin), http.MethodPost, path, body); rec.Code != http.StatusConflict ||
		refusalCode(t, rec) != console.CodeElevationAcknowledged {
		t.Errorf("a second acknowledgement: %d %s, want 409 %s", rec.Code, rec.Body, console.CodeElevationAcknowledged)
	}
	for _, state := range []string{registry.ElevationReviewPending, registry.ElevationReviewed} {
		other := unreviewedElevation()
		other.elevation.ReviewState = state
		h := acknowledgeRouter(other, requestDirectory())
		rec := call(t, h, browser(superAdmin), http.MethodPost,
			"/access/elevations/"+other.elevation.ID.String()+"/acknowledge", body)
		if rec.Code != http.StatusConflict || refusalCode(t, rec) != console.CodeElevationNotAcknowledgeable {
			t.Errorf("%s: %d %s, want 409 %s", state, rec.Code, rec.Body, console.CodeElevationNotAcknowledgeable)
		}
	}
}
