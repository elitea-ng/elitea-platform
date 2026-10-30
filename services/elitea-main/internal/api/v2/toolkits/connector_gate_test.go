package toolkits_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/toolkits"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/connectorapproval"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

type connectorGrants struct {
	approved []connectorapproval.ApprovedConnector
	err      error
}

func (g connectorGrants) ApprovedConnectors(context.Context, int64) ([]connectorapproval.ApprovedConnector, error) {
	return g.approved, g.err
}

func connectorRouter(t *testing.T, repo toolkits.Repository, mode platformconfig.CatalogueMode, grants connectorGrants) *chi.Mux {
	t.Helper()
	gate, err := connectorapproval.NewGate(func(context.Context) (platformconfig.CatalogueSettings, error) {
		return platformconfig.CatalogueSettings{Mode: mode}, nil
	}, grants, false)
	if err != nil {
		t.Fatal(err)
	}
	handler := toolkits.NewHandlerWithRepo(repo, toolkits.WithConnectorGate(gate))
	router := chi.NewRouter()
	router.Get("/toolkit_types/prompt_lib/{projectID}", handler.ListTypes)
	router.Post("/tools/prompt_lib/{projectID}", handler.Create)
	router.Put("/tool/prompt_lib/{projectID}/{toolkitID}", handler.Update)
	router.Post("/fork_toolkit/prompt_lib/{projectID}", handler.ForkToolkit)
	return router
}

// In approved_only every toolkit write naming an unapproved connector is
// refused with 403 connector_not_approved and the target a request would
// approve (ADR-0044 decision 1, #1501).
func TestApprovedOnlyRefusesUnapprovedConnectorWrites(t *testing.T) {
	router := connectorRouter(t, &mockRepo{}, platformconfig.CatalogueApprovedOnly, connectorGrants{
		approved: []connectorapproval.ApprovedConnector{{TargetKind: connectorapproval.TargetToolkitType, Target: "jira"}},
	})
	for _, testCase := range []struct {
		name, method, path string
		body               map[string]any
		target             string
	}{
		{"create", http.MethodPost, "/tools/prompt_lib/1", map[string]any{"type": "confluence", "name": "x"}, "confluence"},
		{"update", http.MethodPut, "/tool/prompt_lib/1/7", map[string]any{"type": "confluence", "name": "x"}, "confluence"},
		{"fork", http.MethodPost, "/fork_toolkit/prompt_lib/1", map[string]any{"type": "confluence", "name": "x"}, "confluence"},
		{"custom MCP", http.MethodPost, "/tools/prompt_lib/1",
			map[string]any{"type": "mcp", "name": "x", "settings": map[string]any{"url": "https://mcp.example.com/mcp"}},
			"https://mcp.example.com/mcp"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := guardrailDo(t, router, testCase.method, testCase.path, testCase.body)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body["code"] != connectorapproval.ConnectorNotApprovedCode || body["target"] != testCase.target {
				t.Fatalf("body %v", body)
			}
		})
	}
	if recorder := guardrailDo(t, router, http.MethodPost, "/tools/prompt_lib/1", map[string]any{"type": "jira", "name": "x"}); recorder.Code == http.StatusForbidden {
		t.Fatalf("an approved type was refused: %s", recorder.Body.String())
	}
}

func TestOpenCatalogueServesEveryType(t *testing.T) {
	router := connectorRouter(t, &mockRepo{types: []string{"confluence"}}, platformconfig.CatalogueOpen, connectorGrants{})
	if recorder := guardrailDo(t, router, http.MethodPost, "/tools/prompt_lib/1", map[string]any{"type": "confluence", "name": "x"}); recorder.Code == http.StatusForbidden {
		t.Fatalf("open mode refused a write: %s", recorder.Body.String())
	}
	if rows := listedTypes(t, router); !slices.Contains(rows, "confluence") {
		t.Fatalf("open mode hid a type: %v", rows)
	}
}

func TestApprovedOnlyListsApprovedAndInternalTypes(t *testing.T) {
	router := connectorRouter(t, &mockRepo{types: []string{"confluence", "jira"}}, platformconfig.CatalogueApprovedOnly, connectorGrants{
		approved: []connectorapproval.ApprovedConnector{{TargetKind: connectorapproval.TargetToolkitType, Target: "jira"}},
	})
	rows := listedTypes(t, router)
	if !slices.Contains(rows, "jira") || slices.Contains(rows, "confluence") {
		t.Fatalf("listed %v", rows)
	}

	// An unreadable approval list offers the internal types only, and a
	// write is refused with 503 rather than allowed.
	failing := connectorRouter(t, &mockRepo{types: []string{"jira"}}, platformconfig.CatalogueApprovedOnly, connectorGrants{err: errors.New("down")})
	if rows := listedTypes(t, failing); slices.Contains(rows, "jira") {
		t.Fatalf("an unreadable catalogue listed %v", rows)
	}
	if recorder := guardrailDo(t, failing, http.MethodPost, "/tools/prompt_lib/1", map[string]any{"type": "jira", "name": "x"}); recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d", recorder.Code)
	}
}

func listedTypes(t *testing.T, router *chi.Mux) []string {
	t.Helper()
	recorder := guardrailDo(t, router, http.MethodGet, "/toolkit_types/prompt_lib/1", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d", recorder.Code)
	}
	var body struct {
		Rows []string `json:"rows"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Rows
}
