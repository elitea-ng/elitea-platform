package projectcloudexecutor

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	handoffapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

type fakeSwitch struct {
	err   error
	got   *bool
	set   bool
	actor int64
}

func (f *fakeSwitch) State(_ context.Context, id int64) (handoffapp.ProjectSwitchState, error) {
	return handoffapp.ProjectSwitchState{ProjectID: id, Source: handoffapp.ProjectSwitchDefault}, f.err
}

func (f *fakeSwitch) Set(_ context.Context, id, actorID int64, enabled *bool) (handoffapp.ProjectSwitchState, error) {
	f.got, f.set, f.actor = enabled, true, actorID
	return handoffapp.ProjectSwitchState{ProjectID: id, Explicit: enabled}, f.err
}

func serve(handler *Handler, method, id, body string) *httptest.ResponseRecorder {
	return serveAs(handler, &auth.User{ID: "41", UserID: "41"}, method, id, body)
}

func serveAs(handler *Handler, user *auth.User, method, id, body string) *httptest.ResponseRecorder {
	router := chi.NewRouter()
	router.Get(ProjectPath, handler.Get)
	router.Put(ProjectPath, handler.Put)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, "/project_cloud_executor/prompt_lib/"+id, bytes.NewBufferString(body))
	if user != nil {
		request = request.WithContext(auth.ContextWithUser(request.Context(), *user))
	}
	router.ServeHTTP(recorder, request)
	return recorder
}

// The write's actor is the signed-in user; a PUT with no user id is refused
// before anything is written.
func TestHandlerPassesTheActor(t *testing.T) {
	fake := &fakeSwitch{}
	if got := serve(NewHandler(fake), http.MethodPut, "7", `{"enabled": false}`); got.Code != http.StatusOK || fake.actor != 41 {
		t.Fatalf("PUT = %d, actor %d; want 200 and 41", got.Code, fake.actor)
	}
	for name, user := range map[string]*auth.User{
		"anonymous": nil,
		"token":     {ID: "t", TokenID: "9", AuthType: "token"},
	} {
		fake := &fakeSwitch{}
		if got := serveAs(NewHandler(fake), user, http.MethodPut, "7", `{"enabled": false}`); got.Code != http.StatusUnauthorized || fake.set {
			t.Errorf("%s: PUT = %d (set %v), want 401 and no write", name, got.Code, fake.set)
		}
	}
}

func TestHandlerDecodesAndMapsErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		method, id, body string
		err              error
		want             int
		set              bool
		value            *bool
	}{
		"get":             {http.MethodGet, "7", "", nil, http.StatusOK, false, nil},
		"put on":          {http.MethodPut, "7", `{"enabled": true}`, nil, http.StatusOK, true, ptr(true)},
		"put null":        {http.MethodPut, "7", `{"enabled": null}`, nil, http.StatusOK, true, nil},
		"missing key":     {http.MethodPut, "7", `{}`, nil, http.StatusBadRequest, false, nil},
		"extra key":       {http.MethodPut, "7", `{"enabled": true, "shell": true}`, nil, http.StatusBadRequest, false, nil},
		"not a boolean":   {http.MethodPut, "7", `{"enabled": 1}`, nil, http.StatusBadRequest, false, nil},
		"not json":        {http.MethodPut, "7", `on`, nil, http.StatusBadRequest, false, nil},
		"bad id":          {http.MethodGet, "x", "", nil, http.StatusBadRequest, false, nil},
		"zero id":         {http.MethodGet, "0", "", nil, http.StatusBadRequest, false, nil},
		"unknown project": {http.MethodGet, "7", "", handoffapp.ErrProjectNotFound, http.StatusNotFound, false, nil},
		"platform off":    {http.MethodPut, "7", `{"enabled": true}`, handoffapp.ErrProjectSwitchPlatformOff, http.StatusConflict, true, ptr(true)},
		"store failure":   {http.MethodGet, "7", "", errors.New("boom"), http.StatusInternalServerError, false, nil},
	} {
		fake := &fakeSwitch{err: tc.err}
		got := serve(NewHandler(fake), tc.method, tc.id, tc.body)
		if got.Code != tc.want || fake.set != tc.set || (tc.set && !samePtr(fake.got, tc.value)) {
			t.Errorf("%s: %d (set %v %v), want %d", name, got.Code, fake.set, fake.got, tc.want)
		}
	}
	if got := serve(NewHandler(nil), http.MethodGet, "7", ""); got.Code != http.StatusServiceUnavailable {
		t.Fatalf("no store = %d, want 503", got.Code)
	}
}

func ptr(value bool) *bool { return &value }

func samePtr(a, b *bool) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
