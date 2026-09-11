package configurations

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type fixedTracingAccess bool

func (allowed fixedTracingAccess) CanManageTracing(context.Context, int64, int64) (bool, error) {
	return bool(allowed), nil
}

func TestTracingTypeDetectionUsesRegistryCategoryAndFallback(t *testing.T) {
	handler := NewHandler(nil)
	entry := configurationapp.CurrentAvailableConfigurationType{
		Type: "future_tracing_provider",
		ConfigSchema: json.RawMessage(`{
			"properties":{"data":{"metadata":{"categories":["observability","Tracing"]}}}
		}`),
	}
	if !currentConfigurationEntryHasCategory(entry, "tracing") {
		t.Fatal("registry tracing category is not recognized case-insensitively")
	}
	if !handler.isTracingConfigurationType("langfuse") {
		t.Fatal("langfuse is not recognized as a tracing type")
	}
	if handler.isTracingConfigurationType("open_ai") {
		t.Fatal("open_ai is incorrectly recognized as a tracing type")
	}
}

func TestAvailableConcealsTracingTypesForAnUnauthorizedProjectActor(t *testing.T) {
	for _, test := range []struct {
		name         string
		allowed      bool
		wantLangfuse bool
	}{
		{name: "project member", allowed: false, wantLangfuse: false},
		{name: "project administrator", allowed: true, wantLangfuse: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := NewHandler(nil)
			handler.tracingAccess = fixedTracingAccess(test.allowed)
			request := httptest.NewRequest(http.MethodGet, "/available/?project_id=7", nil)
			request = request.WithContext(auth.ContextWithUser(
				request.Context(), auth.User{ID: "41", UserID: "41"},
			))
			recorder := httptest.NewRecorder()
			handler.Available(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
			}
			var entries []map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &entries); err != nil {
				t.Fatalf("decode available catalogue: %v", err)
			}
			found := false
			for _, entry := range entries {
				if entry["type"] == "langfuse" {
					found = true
				}
			}
			if found != test.wantLangfuse {
				t.Fatalf("langfuse visible = %v, want %v", found, test.wantLangfuse)
			}
		})
	}
}
