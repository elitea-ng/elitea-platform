package indexing

import (
	"errors"
	"testing"
)

func TestCurrentManualStopCleanupContractsRejectInvalidIdentity(t *testing.T) {
	for name, value := range map[string]CurrentManualStopCleanup{
		"missing execution": {
			MetaID:          "meta-1",
			Generation:      1,
			IndexGeneration: 1,
			IndexName:       "Docs",
			ToolkitID:       19,
		},
		"missing logical generation": {
			MetaID:      "meta-1",
			ExecutionID: "execution-1",
			Generation:  1,
			IndexName:   "Docs",
			ToolkitID:   19,
		},
		"missing toolkit": {
			MetaID:          "meta-1",
			ExecutionID:     "execution-1",
			Generation:      1,
			IndexGeneration: 1,
			IndexName:       "Docs",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if !errors.Is(
				value.Validate(),
				ErrCurrentIndexMetaInitializationInvalid,
			) {
				t.Fatalf("value=%+v", value)
			}
		})
	}
}
