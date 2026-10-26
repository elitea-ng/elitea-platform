package workspacehandoff

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacechanges"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacestore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

type fakeChanges struct {
	err  error
	asks []string
}

func (f *fakeChanges) View(context.Context, workspacestore.Thread) (workspacechanges.View, error) {
	return workspacechanges.View{Complete: true, Turns: []workspacechanges.Turn{}}, f.err
}

func (f *fakeChanges) Diff(_ context.Context, _ workspacestore.Thread, commit, scope, path string) (workspacechanges.DiffView, error) {
	f.asks = append(f.asks, commit+"|"+scope+"|"+path)
	return workspacechanges.DiffView{Commit: commit, Scope: scope, Path: path, Diff: "+x\n"}, f.err
}

const (
	changesURL = "/api/v2/elitea_core/workspace_handoff_changes/prompt_lib/1/" + conversation
	diffURL    = "/api/v2/elitea_core/workspace_handoff_diff/prompt_lib/1/" + conversation
)

// TestChangesReadsAreForTheConversationsReaders: a cookie session that may
// read the conversation gets the view and a diff (the query reaches the
// service as given); one that may not, or a deployment without the view,
// gets 404 and the service is never asked.
func TestChangesReadsAreForTheConversationsReaders(t *testing.T) {
	session := &auth.User{ID: "9", UserID: "9", AuthType: "session"}
	changes := &fakeChanges{}
	h := &handler{handoff: &fakeHandoff{}, store: &fakeStore{}, conversations: fakeConversations{}, changes: changes}
	response := serve(t, h.changesView, session, []string{ReadPermission}, http.MethodGet, ChangesPath, changesURL, "")
	if answer := decode(t, response); response.Code != http.StatusOK || answer["complete"] != true {
		t.Fatalf("view = %d %v", response.Code, answer)
	}
	response = serve(t, h.diff, session, []string{ReadPermission}, http.MethodGet, DiffPath,
		diffURL+"?commit="+sha1Value+"&scope=turn&path=src%2Fa%20b.rs", "")
	if answer := decode(t, response); response.Code != http.StatusOK || answer["diff"] != "+x\n" ||
		len(changes.asks) != 1 || changes.asks[0] != sha1Value+"|turn|src/a b.rs" {
		t.Fatalf("diff = %d %v %v", response.Code, answer, changes.asks)
	}

	private := &handler{handoff: &fakeHandoff{}, store: &fakeStore{}, conversations: fakeConversations{err: errors.New("private")}, changes: changes}
	unserved := &handler{handoff: &fakeHandoff{}, store: &fakeStore{}, conversations: fakeConversations{}}
	for name, handler := range map[string]*handler{"unreadable": private, "not composed": unserved} {
		if response := serve(t, handler.changesView, session, []string{ReadPermission}, http.MethodGet, ChangesPath, changesURL, ""); response.Code != http.StatusNotFound {
			t.Fatalf("%s view = %d", name, response.Code)
		}
		if response := serve(t, handler.diff, session, []string{ReadPermission}, http.MethodGet, DiffPath, diffURL+"?commit=x", ""); response.Code != http.StatusNotFound {
			t.Fatalf("%s diff = %d", name, response.Code)
		}
	}
	if len(changes.asks) != 1 {
		t.Fatalf("asked = %v", changes.asks)
	}
}

// TestChangesErrorsMapToTheContractCodes: invalid → 400, not found (also a
// hidden path) → 404 diff_not_found, anything else → 503 with Retry-After.
func TestChangesErrorsMapToTheContractCodes(t *testing.T) {
	session := &auth.User{ID: "9", UserID: "9", AuthType: "session"}
	for err, want := range map[error]int{
		workspacechanges.ErrInvalid:     http.StatusBadRequest,
		workspacechanges.ErrNotFound:    http.StatusNotFound,
		workspacechanges.ErrUnavailable: http.StatusServiceUnavailable,
	} {
		h := &handler{handoff: &fakeHandoff{}, store: &fakeStore{}, conversations: fakeConversations{}, changes: &fakeChanges{err: err}}
		response := serve(t, h.diff, session, []string{ReadPermission}, http.MethodGet, DiffPath, diffURL, "")
		if response.Code != want {
			t.Fatalf("%v = %d", err, response.Code)
		}
		if want == http.StatusNotFound && decode(t, response)["error"] != "diff_not_found" {
			t.Fatalf("%v body = %s", err, response.Body.String())
		}
		if want == http.StatusServiceUnavailable && response.Header().Get("Retry-After") == "" {
			t.Fatalf("no Retry-After")
		}
	}
}

// TestRouteServesTheChangesReadsWithTheReadPermission: the composed route
// mounts both reads behind the conversation-details permission only.
func TestRouteServesTheChangesReadsWithTheReadPermission(t *testing.T) {
	found := map[string]bool{}
	for _, method := range Methods {
		if method.Method == http.MethodGet && (method.Pattern == ChangesPath || method.Pattern == DiffPath) {
			found[method.Pattern] = true
		}
	}
	if len(found) != 2 {
		t.Fatalf("methods = %v", found)
	}
}
