package configurations

import (
	"errors"
	"testing"

	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

func TestValidationResultAcceptsOnlyCanonicalInvalidValueMessage(t *testing.T) {
	result := ValidationResult{
		Binding: ValidationBinding{
			Command: ValidationCommand{
				ConfigurationRevisionID: "revision-1",
				ConfigurationType:       "openapi",
				CatalogRevision:         "catalog-v1",
				CatalogDigest:           runtimedomain.SHA256([]byte("catalog")),
				SchemaID:                "openapi",
				SchemaRevision:          "schema-v1",
				SchemaDigest:            runtimedomain.SHA256([]byte("schema")),
				SettingsEntryID:         "settings",
			},
			InputBundleID:         "bundle-1",
			InputBundleDigest:     runtimedomain.SHA256([]byte("manifest")),
			SettingsEntryVersion:  "revision-1",
			SettingsContentDigest: runtimedomain.SHA256([]byte("settings")),
		},
		Valid: false,
		Issues: []ValidationIssue{{
			Code:        "INVALID_VALUE",
			JSONPointer: "/custom",
			SafeMessage: "Value does not satisfy the configuration schema.",
		}},
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("canonical SDK fallback issue was rejected: %v", err)
	}
	result.Issues[0].SafeMessage = "worker-controlled detail"
	if err := result.Validate(); !errors.Is(err, ErrInvalidValidationResult) {
		t.Fatalf("arbitrary fallback text was accepted: %v", err)
	}
}
