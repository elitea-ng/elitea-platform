package main

import (
	"errors"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/runtimecomposition"
)

func validateRuntimeComposition(
	configurations currentConfigurationsConfig,
	runtime runtimecomposition.Config,
) error {
	// Index ingest requires the Configurations chain only. It used to also
	// require the LiteLLM facade (the retired ELITEA_LITELLM_BASE_URL plus its
	// master key),
	// because the embedding binding asked that proxy's registry which model
	// groups existed. The binding is now resolved from the Configurations rows
	// themselves — the same rows the Bifrost gateway reads per project — so the
	// index plane composes on a gateway-only stack.
	if runtime.IndexIngestDispatchEnabled && !configurations.Enabled {
		return errors.New("runtime index ingest requires current Configurations")
	}
	if configurations.MutationEnabled && !runtime.Enabled {
		return errors.New("current Configurations mutation requires the production runtime")
	}
	return nil
}
