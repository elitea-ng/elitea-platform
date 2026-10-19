package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

// The cloud_executor section shows how storage encryption at rest was
// established: "attested by operator" is weaker evidence than a backend that
// reports its rule, and the admin form says which one this deployment has.
func TestCloudExecutorSectionShowsTheEncryptionAnswer(t *testing.T) {
	notice := func(status string) (string, bool) {
		handler := NewHandler(nil, WithCloudExecutorEncryption(status))
		rec := httptest.NewRecorder()
		handler.PluginConfigSchemas(rec, httptest.NewRequest(http.MethodGet, "/admin/plugin_config_schemas/administration", nil))
		var body struct {
			Sections []map[string]any `json:"sections"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		for _, section := range body.Sections {
			if section["id"] == platformconfig.SectionCloudExecutor {
				value, ok := section["notice"].(string)
				return value, ok
			}
		}
		t.Fatal("no cloud_executor section")
		return "", false
	}
	if got, _ := notice("attested"); !strings.Contains(got, "attested by operator") {
		t.Fatalf("attested notice = %q", got)
	}
	if got, _ := notice("confirmed"); !strings.Contains(got, "reported by the storage backend") {
		t.Fatalf("confirmed notice = %q", got)
	}
	if got, _ := notice("refused"); !strings.Contains(got, "not confirmed") {
		t.Fatalf("refused notice = %q", got)
	}
	if got, ok := notice(""); ok {
		t.Fatalf("a deployment without the handoff shows %q", got)
	}
	// The static schema is not changed by a handler: a second handler
	// without the option shows no notice.
	for _, section := range configSections() {
		if _, ok := section["notice"]; ok {
			t.Fatalf("configSections() carries a notice: %v", section["id"])
		}
	}
}
