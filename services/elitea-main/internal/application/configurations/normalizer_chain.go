package configurations

import (
	"errors"
	"fmt"
)

// NewCurrentConfigurationDataNormalizer composes the create-time owners of
// every type in the current catalog. Construction fails closed when a type is
// unowned or claimed by more than one boundary; PUT falls through to the
// current shallow-update behavior in CurrentPoVDataNormalizer.
func NewCurrentConfigurationDataNormalizer(
	catalog *CurrentAvailableCatalog,
	expander CurrentConfigurationExpander,
	validator CurrentSDKConfigurationValidator,
) (CurrentConfigurationDataNormalizer, error) {
	if catalog == nil || !catalog.Complete() || expander == nil || validator == nil {
		return nil, errors.New("current configuration normalizer dependencies are required")
	}
	entries, err := catalog.CompleteEntries()
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		owners := 0
		if entry.UsesSDKValidation() {
			owners++
		}
		if currentLocalConfigurationType(entry.Type) {
			owners++
		}
		if currentLiteLLMCredentialType(entry.Type) {
			owners++
		}
		if currentGatewayCredentialType(entry.Type) {
			owners++
		}
		if currentArtifactsConfigurationType(entry.Type) {
			owners++
		}
		if owners != 1 {
			return nil, fmt.Errorf("current configuration type %q has %d create normalizer owners", entry.Type, owners)
		}
	}

	var fallback CurrentConfigurationDataNormalizer = CurrentPoVDataNormalizer{}
	fallback = NewCurrentLocalDataNormalizer(fallback)
	fallback = NewCurrentLiteLLMDataNormalizer(fallback)
	fallback = NewCurrentGatewayCredentialDataNormalizer(fallback)
	fallback = NewCurrentArtifactsDataNormalizer(fallback)
	return NewCurrentSDKDataNormalizer(catalog, expander, validator, fallback)
}
