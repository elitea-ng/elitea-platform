package mandateattachments_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/mandateattachments"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runmandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runmandate/runmandatetest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

const project = int64(5)

// targetStore answers one schedule of version 11 in project, and records
// what was attached.
type targetStore struct {
	attached map[string]string
}

func (s *targetStore) Target(_ context.Context, a repos.MandateAttachment) (runmandate.Target, error) {
	if a.ProjectID != project {
		return runmandate.Target{}, repos.ErrAttachmentTargetNotFound
	}
	input := "scheduled"
	return runmandate.Target{
		Origin: a.Origin, TenantProjectID: a.ProjectID, ResourceID: "3",
		ApplicationVersionID: a.VersionID, Cron: "0 * * * *", Input: &input,
	}, nil
}

func (s *targetStore) Attach(_ context.Context, a repos.MandateAttachment, target runmandate.Target) error {
	s.attached[target.ResourceID] = a.MandateID
	return nil
}

func (s *targetStore) Detach(_ context.Context, _ repos.MandateAttachment, target runmandate.Target) error {
	delete(s.attached, target.ResourceID)
	return nil
}

func newServer(t *testing.T) (http.Handler, *targetStore, *runmandatetest.Store) {
	t.Helper()
	mandates := runmandatetest.New()
	mandates.PutBinding("binding-1", project, 11)
	mandates.PutMandate("auto", project, mandate.KindAutonomous, "binding-1", 0)
	mandates.PutMandate("foreign", project+1, mandate.KindAutonomous, "binding-1", 0)
	mandates.PutMandate("interactive", project, mandate.Kind("interactive_delegation"), "binding-1", 9)
	mandates.PutMandate("narrow", project, mandate.KindAutonomous, "binding-1", 0, runmandate.CronScope("30 2 * * *"))
	store := &targetStore{attached: map[string]string{}}
	handler, err := mandateattachments.New(store, mandates.Service())
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: "1", UserID: "1"})))
		})
	})
	r.Put("/pipeline_schedules/{projectID}/{versionID}", handler.AttachPipelineSchedule)
	r.Delete("/pipeline_schedules/{projectID}/{versionID}", handler.DetachPipelineSchedule)
	return r, store, mandates
}

func call(t *testing.T, server http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(method, path, strings.NewReader(body)))
	var decoded map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &decoded)
	return recorder.Code, decoded
}

func TestAttachAcceptsAnUnattendedMandateOfTheTenant(t *testing.T) {
	server, store, _ := newServer(t)
	status, body := call(t, server, http.MethodPut, "/pipeline_schedules/5/11", `{"mandate_id":"auto"}`)
	if status != http.StatusOK || body["mandate_id"] != "auto" || body["origin"] != "pipeline_schedule" || store.attached["3"] != "auto" {
		t.Fatalf("status=%d body=%v attached=%v", status, body, store.attached)
	}
	status, body = call(t, server, http.MethodDelete, "/pipeline_schedules/5/11", "")
	if status != http.StatusOK || body["mandate_id"] != nil || len(store.attached) != 0 {
		t.Fatalf("detach status=%d body=%v attached=%v", status, body, store.attached)
	}
}

func TestAttachRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		status           int
		code             string
	}{
		{"another tenant", "/pipeline_schedules/5/11", `{"mandate_id":"foreign"}`, http.StatusConflict, runmandate.CodeMandateTenantMismatch},
		{"interactive delegation", "/pipeline_schedules/5/11", `{"mandate_id":"interactive"}`, http.StatusConflict, runmandate.CodeMandateKindNotAllowed},
		{"another version", "/pipeline_schedules/5/12", `{"mandate_id":"auto"}`, http.StatusConflict, runmandate.CodeMandateBindingMismatch},
		{"out of scope", "/pipeline_schedules/5/11", `{"mandate_id":"narrow"}`, http.StatusConflict, runmandate.CodeMandateScopeMismatch},
		{"unknown mandate", "/pipeline_schedules/5/11", `{"mandate_id":"missing"}`, http.StatusNotFound, runmandate.CodeMandateNotFound},
		{"no target", "/pipeline_schedules/6/11", `{"mandate_id":"auto"}`, http.StatusNotFound, ""},
		{"no body", "/pipeline_schedules/5/11", `{}`, http.StatusBadRequest, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, store, _ := newServer(t)
			status, body := call(t, server, http.MethodPut, tc.path, tc.body)
			code, _ := body["code"].(string)
			if status != tc.status || code != tc.code || len(store.attached) != 0 {
				t.Fatalf("status=%d body=%v attached=%v", status, body, store.attached)
			}
		})
	}
}

func TestAttachRefusesARevokedMandate(t *testing.T) {
	server, store, mandates := newServer(t)
	mandates.Update("auto", func(m *mandate.Mandate) { m.State = mandate.StateRevoked })
	status, body := call(t, server, http.MethodPut, "/pipeline_schedules/5/11", `{"mandate_id":"auto"}`)
	if status != http.StatusConflict || body["code"] != runmandate.CodeMandateNotUsable || len(store.attached) != 0 {
		t.Fatalf("status=%d body=%v", status, body)
	}
}
