package runtimecomposition

import (
	"context"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

const currentSDKValidationSettingsEntryID = "settings"

// CurrentSDKValidationContractCatalog joins Main's complete current
// Configurations catalog with the independently generated worker SDK binding
// catalog. Construction fails on any ownership, revision, section, or count
// drift before a command can be admitted.
type CurrentSDKValidationContractCatalog struct {
	sdk *CurrentSDKConfigurationCatalog
}

func NewCurrentSDKValidationContractCatalog(
	available *configurationapp.CurrentAvailableCatalog,
	sdk *CurrentSDKConfigurationCatalog,
) (*CurrentSDKValidationContractCatalog, error) {
	if available == nil || !available.Complete() || sdk == nil || sdk.EntryCount() == 0 {
		return nil, ErrCurrentSDKConfigurationCatalogInvalid
	}
	sources := available.SourceRevisions()
	if sources["elitea_sdk"] != sdk.SDKRevision() || sdk.CatalogRevision() != sdk.SDKRevision() {
		return nil, ErrCurrentSDKConfigurationCatalogInvalid
	}
	entries, err := available.CompleteEntries()
	if err != nil {
		return nil, ErrCurrentSDKConfigurationCatalogInvalid
	}
	owned := 0
	for _, entry := range entries {
		binding, found := sdk.Binding(entry.Type)
		if entry.UsesSDKValidation() {
			owned++
			if !found || !binding.ValidationSupported || binding.Section != entry.Section {
				return nil, ErrCurrentSDKConfigurationCatalogInvalid
			}
			continue
		}
		if found {
			return nil, ErrCurrentSDKConfigurationCatalogInvalid
		}
	}
	if owned != sdk.EntryCount() {
		return nil, ErrCurrentSDKConfigurationCatalogInvalid
	}
	return &CurrentSDKValidationContractCatalog{sdk: sdk}, nil
}

func (c *CurrentSDKValidationContractCatalog) ResolveCurrentSDKValidationContract(
	ctx context.Context,
	configurationType string,
) (configurationapp.CurrentSDKValidationContract, error) {
	if ctx == nil || c == nil || c.sdk == nil || configurationType == "" {
		return configurationapp.CurrentSDKValidationContract{}, ErrCurrentSDKConfigurationCatalogInvalid
	}
	if err := ctx.Err(); err != nil {
		return configurationapp.CurrentSDKValidationContract{}, err
	}
	binding, found := c.sdk.Binding(configurationType)
	if !found || !binding.ValidationSupported {
		return configurationapp.CurrentSDKValidationContract{}, ErrCurrentSDKConfigurationCatalogInvalid
	}
	return configurationapp.CurrentSDKValidationContract{
		ConfigurationType: binding.ConfigurationType,
		CatalogRevision:   c.sdk.CatalogRevision(),
		CatalogDigest:     runtimedomain.Digest(c.sdk.CatalogDigest()),
		SchemaID:          binding.SchemaID,
		SchemaRevision:    binding.SchemaRevision,
		SchemaDigest:      runtimedomain.Digest(binding.SchemaDigest),
		SettingsEntryID:   currentSDKValidationSettingsEntryID,
	}, nil
}

var _ configurationapp.CurrentSDKValidationContractResolver = (*CurrentSDKValidationContractCatalog)(nil)
