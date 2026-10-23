package runtimecomposition

import (
	"context"
	"errors"
	"testing"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

func TestCurrentSDKValidationContractCatalogJoinsAllCurrentSDKTypes(t *testing.T) {
	available, err := configurationapp.LoadPinnedCurrentAvailableCatalog()
	if err != nil {
		t.Fatal(err)
	}
	sdk, err := LoadPinnedCurrentSDKConfigurationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	contracts, err := NewCurrentSDKValidationContractCatalog(available, sdk)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := available.CompleteEntries()
	if err != nil {
		t.Fatal(err)
	}
	resolved := 0
	for _, entry := range entries {
		if !entry.UsesSDKValidation() {
			continue
		}
		contract, resolveErr := contracts.ResolveCurrentSDKValidationContract(context.Background(), entry.Type)
		if resolveErr != nil {
			t.Fatalf("ResolveCurrentSDKValidationContract(%q) error=%v", entry.Type, resolveErr)
		}
		if contract.ConfigurationType != entry.Type || contract.CatalogRevision != sdk.SDKRevision() ||
			contract.SchemaID != "elitea.configuration."+entry.Type || contract.SettingsEntryID != "settings" ||
			contract.CatalogDigest.IsZero() || contract.SchemaDigest.IsZero() {
			t.Fatalf("type %q contract=%+v", entry.Type, contract)
		}
		resolved++
	}
	if resolved != 31 {
		t.Fatalf("resolved = %d, want 31", resolved)
	}
}

func TestCurrentSDKValidationContractCatalogFailsClosedOnDrift(t *testing.T) {
	available, err := configurationapp.LoadPinnedCurrentAvailableCatalog()
	if err != nil {
		t.Fatal(err)
	}
	sdk, err := LoadPinnedCurrentSDKConfigurationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	binding := sdk.entries["github"]
	binding.Section = "wrong"
	sdk.entries["github"] = binding
	if _, err := NewCurrentSDKValidationContractCatalog(available, sdk); !errors.Is(err, ErrCurrentSDKConfigurationCatalogInvalid) {
		t.Fatalf("error=%v", err)
	}
}

func TestCurrentSDKValidationContractCatalogRejectsUnknownType(t *testing.T) {
	available, err := configurationapp.LoadPinnedCurrentAvailableCatalog()
	if err != nil {
		t.Fatal(err)
	}
	sdk, err := LoadPinnedCurrentSDKConfigurationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	contracts, err := NewCurrentSDKValidationContractCatalog(available, sdk)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contracts.ResolveCurrentSDKValidationContract(context.Background(), "llm_model"); !errors.Is(err, ErrCurrentSDKConfigurationCatalogInvalid) {
		t.Fatalf("error=%v", err)
	}
}
