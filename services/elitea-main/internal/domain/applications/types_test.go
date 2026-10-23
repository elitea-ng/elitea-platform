package applications_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/applications"
)

func TestApplication_JSONRoundTrip(t *testing.T) {
	app := applications.Application{
		ID:          "app-123",
		ProjectID:   "proj-456",
		Name:        "Test Agent",
		Description: "A test agent",
		Type:        "chat",
		Tags:        []string{"test", "demo"},
		Status:      "active",
		CreatedAt:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt:   time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		CreatedBy:   "user-1",
	}

	data, err := json.Marshal(app)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var decoded applications.Application
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if decoded.ID != app.ID {
		t.Errorf("ID mismatch: %q vs %q", decoded.ID, app.ID)
	}
	if decoded.Name != app.Name {
		t.Errorf("Name mismatch: %q vs %q", decoded.Name, app.Name)
	}
	if len(decoded.Tags) != 2 {
		t.Errorf("expected 2 tags, got %d", len(decoded.Tags))
	}
}

func TestListResponse_JSONShape(t *testing.T) {
	resp := applications.ListResponse{
		Rows:       []applications.Application{{ID: "a1", Name: "Agent 1"}},
		Total:      50,
		Page:       1,
		PageSize:   20,
		TotalPages: 3,
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var raw map[string]json.RawMessage
	_ = json.Unmarshal(data, &raw)

	requiredFields := []string{"rows", "total", "page", "page_size", "total_pages"}
	for _, f := range requiredFields {
		if _, ok := raw[f]; !ok {
			t.Errorf("missing required field %q in JSON output", f)
		}
	}
}

func TestVersionConfig_JSONRoundTrip(t *testing.T) {
	cfg := applications.VersionConfig{
		Model:        "gpt-4",
		Temperature:  0.7,
		MaxTokens:    4096,
		SystemPrompt: "You are a helpful assistant.",
		Tools:        []applications.ToolRef{{ToolkitID: "tk-1", ToolName: "search"}},
		Skills:       []applications.SkillRef{{SkillID: "sk-1"}},
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var decoded applications.VersionConfig
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if decoded.Model != "gpt-4" {
		t.Errorf("Model mismatch: %q", decoded.Model)
	}
	if decoded.Temperature != 0.7 {
		t.Errorf("Temperature mismatch: %f", decoded.Temperature)
	}
	if len(decoded.Tools) != 1 || decoded.Tools[0].ToolName != "search" {
		t.Error("Tools round-trip failed")
	}
}

func TestPredictRequest_OmitsEmpty(t *testing.T) {
	req := applications.PredictRequest{
		Input: "Hello",
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var raw map[string]json.RawMessage
	_ = json.Unmarshal(data, &raw)

	if _, ok := raw["variables"]; ok {
		t.Error("expected variables to be omitted when nil")
	}
	if _, ok := raw["stream"]; ok {
		t.Error("expected stream to be omitted when false")
	}
}
