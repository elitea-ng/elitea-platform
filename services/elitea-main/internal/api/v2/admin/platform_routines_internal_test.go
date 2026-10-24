package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/libs/go/routines"
)

type fakeRoutineStore struct {
	rows  []routines.Row
	patch routines.Patch
	id    string
	err   error
}

func (f *fakeRoutineStore) List(context.Context) ([]routines.Row, error) { return f.rows, nil }

func (f *fakeRoutineStore) Update(_ context.Context, id string, patch routines.Patch, _ time.Time) (routines.Row, error) {
	f.id, f.patch = id, patch
	if f.err != nil {
		return routines.Row{}, f.err
	}
	return f.rows[0], nil
}

func routineRouter(h *Handler) http.Handler {
	r := chi.NewRouter()
	r.Get("/platform_routines/administration", h.PlatformRoutines)
	r.Put("/platform_routines/administration/{id}", h.UpdatePlatformRoutine)
	return r
}

func TestPlatformRoutinesUnwiredAnswers503(t *testing.T) {
	h := &Handler{}
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/platform_routines/administration", nil),
		httptest.NewRequest(http.MethodPut, "/platform_routines/administration/x", strings.NewReader(`{"enabled":false}`)),
	} {
		rec := httptest.NewRecorder()
		routineRouter(h).ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: got %d, want 503", req.Method, rec.Code)
		}
	}
}

func TestPlatformRoutinesListAndUpdate(t *testing.T) {
	override := "30m"
	store := &fakeRoutineStore{rows: []routines.Row{{
		Definition:       routines.Definition{ID: "index.schedule.scan.v1", Owner: routines.OwnerMain, DefaultSchedule: "* * * * *"},
		ScheduleOverride: &override, Enabled: true,
	}}}
	h := &Handler{platformRoutines: store}

	rec := httptest.NewRecorder()
	routineRouter(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/platform_routines/administration", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"schedule":"30m"`) {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	routineRouter(h).ServeHTTP(rec, httptest.NewRequest(http.MethodPut,
		"/platform_routines/administration/index.schedule.scan.v1", strings.NewReader(`{"enabled":false}`)))
	if rec.Code != http.StatusOK || store.id != "index.schedule.scan.v1" || store.patch.Enabled == nil || *store.patch.Enabled {
		t.Fatalf("update: %d id=%q patch=%+v", rec.Code, store.id, store.patch)
	}
}

func TestPlatformRoutineUpdateErrorsMapToStatuses(t *testing.T) {
	cases := map[int]error{
		http.StatusNotFound:   routines.ErrNotFound,
		http.StatusBadRequest: routines.ErrBelowMinimum,
		http.StatusConflict:   routines.ErrConflict,
	}
	for want, cause := range cases {
		h := &Handler{platformRoutines: &fakeRoutineStore{err: cause}}
		rec := httptest.NewRecorder()
		routineRouter(h).ServeHTTP(rec, httptest.NewRequest(http.MethodPut,
			"/platform_routines/administration/x", strings.NewReader(`{"schedule":"1s"}`)))
		if rec.Code != want {
			t.Errorf("%v: got %d, want %d", cause, rec.Code, want)
		}
	}
	rec := httptest.NewRecorder()
	routineRouter(&Handler{platformRoutines: &fakeRoutineStore{}}).ServeHTTP(rec, httptest.NewRequest(http.MethodPut,
		"/platform_routines/administration/x", strings.NewReader(`{"bogus":1}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown field: got %d", rec.Code)
	}
}
