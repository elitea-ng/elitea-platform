package delegationgrants

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	appgrants "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/delegationgrants"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	dg "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/delegationgrant"
)

type fakeService struct {
	err      error
	approve  []dg.Item
	approves int
	view     appgrants.View
}

func (f *fakeService) List(context.Context, int64, int64, bool) ([]appgrants.View, error) {
	return []appgrants.View{f.view}, f.err
}
func (f *fakeService) Get(context.Context, int64, int64, int64) (appgrants.View, error) {
	if f.err == dg.ErrGoverned { // the refusal is the approve route's, the read still works
		return f.view, nil
	}
	return f.view, f.err
}
func (f *fakeService) Propose(context.Context, appgrants.ProposeRequest) (appgrants.View, error) {
	return f.view, f.err
}
func (f *fakeService) Approve(_ context.Context, _, _, _, _ int64, choose []dg.Item) (appgrants.View, error) {
	f.approves++
	f.approve = choose
	return f.view, f.err
}
func (f *fakeService) Revoke(context.Context, int64, int64, int64, int64) (appgrants.View, error) {
	return f.view, f.err
}
func (f *fakeService) Preview(context.Context, appgrants.PreviewRequest) (appgrants.Preview, error) {
	return appgrants.Preview{Callee: dg.Callee{ApplicationID: 1, VersionID: 5, Name: "A"}}, f.err
}

func serve(t *testing.T, f *fakeService, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := NewHandler(f)
	r := chi.NewRouter()
	r.Get("/p/{projectID}/{applicationID}", h.List)
	r.Post("/p/{projectID}/{applicationID}", h.Propose)
	r.Get("/preview/{projectID}/{applicationID}", h.Preview)
	r.Get("/g/{projectID}/{applicationID}/{grantID}", h.Get)
	r.Delete("/g/{projectID}/{applicationID}/{grantID}", h.Revoke)
	r.Post("/g/{projectID}/{applicationID}/{grantID}/approve", h.Approve)
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req = req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: "7", UserID: "7"}))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func grantView() appgrants.View {
	return appgrants.View{Grant: dg.Grant{ID: 3, ProjectID: 1, Edge: dg.Edge{CallerEntityID: 2, CalleeEntityID: 1, Kind: dg.EdgeSubAgent}, State: dg.StatePending}}
}

func TestRoutesAnswerTheViewAndMapRefusals(t *testing.T) {
	ok := &fakeService{view: grantView()}
	if rec := serve(t, ok, http.MethodGet, "/p/1/2", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"grants"`) {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	if rec := serve(t, ok, http.MethodPost, "/p/1/2", `{"callee_application_id":1,"callee_version_id":5,"edge_kind":"sub_agent"}`); rec.Code != http.StatusCreated {
		t.Fatalf("propose: %d %s", rec.Code, rec.Body)
	}
	if rec := serve(t, ok, http.MethodPost, "/p/1/2", `{"callee_application_id":1}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("propose without a version: %d", rec.Code)
	}
	if rec := serve(t, ok, http.MethodPost, "/p/1/2", `{"x":1}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("propose with an unknown member: %d", rec.Code)
	}
	if rec := serve(t, ok, http.MethodGet, "/preview/1/2?callee_application_id=1&callee_version_id=5&edge_kind=sub_agent", ""); rec.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body)
	}
	if rec := serve(t, ok, http.MethodGet, "/preview/1/2?callee_application_id=1", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("preview without a version: %d", rec.Code)
	}
	if rec := serve(t, ok, http.MethodGet, "/g/1/2/x", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("a bad grant id: %d", rec.Code)
	}
	for err, want := range map[error]int{
		dg.ErrNotFound: 404, dg.ErrCalleeNotFound: 404, dg.ErrActiveEdgeExists: 409, dg.ErrWrongState: 409,
		dg.ErrSelfApproval: 403, dg.ErrNothingToApprove: 400, dg.ErrInvalid: 400,
	} {
		if rec := serve(t, &fakeService{err: err, view: grantView()}, http.MethodDelete, "/g/1/2/3", ""); rec.Code != want {
			t.Errorf("%v: %d, want %d", err, rec.Code, want)
		}
	}
}

func TestApproveTakesTheChosenItemsAndPointsAGovernedGrantAtItsChangeRequest(t *testing.T) {
	f := &fakeService{view: grantView()}
	body := `{"items":[{"kind":"toolkit","name":"toolkit:11"}]}`
	if rec := serve(t, f, http.MethodPost, "/g/1/2/3/approve", body); rec.Code != http.StatusOK || len(f.approve) != 1 || f.approve[0].Name != "toolkit:11" {
		t.Fatalf("approve with items: %d %s %+v", rec.Code, rec.Body, f.approve)
	}
	// No body, or items: null, keeps everything (nil), an empty list keeps nothing.
	f = &fakeService{view: grantView()}
	serve(t, f, http.MethodPost, "/g/1/2/3/approve", "")
	if f.approves != 1 || f.approve != nil {
		t.Fatalf("no body keeps all: %+v", f.approve)
	}
	serve(t, f, http.MethodPost, "/g/1/2/3/approve", `{"items":[]}`)
	if f.approve == nil || len(f.approve) != 0 {
		t.Fatalf("an empty list keeps none: %+v", f.approve)
	}

	governed := grantView()
	governed.ChangeRequestID = 77
	gf := &fakeService{err: dg.ErrGoverned, view: governed}
	rec := serve(t, gf, http.MethodPost, "/g/1/2/3/approve", "")
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusConflict || out["code"] != GovernedCode || out["change_request_id"] != float64(77) {
		t.Fatalf("governed: %d %s", rec.Code, rec.Body)
	}
}
