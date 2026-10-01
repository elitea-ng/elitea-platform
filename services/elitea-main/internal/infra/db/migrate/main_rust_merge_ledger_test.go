package migrate

import (
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestMergedMainLedgerAcceptsMainPrefixAndRefusesUnreconciledFeatureVersion(t *testing.T) {
	manifest, err := LoadManifest(platformmigrations.Files, ScopeShared)
	require.NoError(t, err)
	var mainPrefix []recordedMigration
	var featureToolkit recordedMigration
	for _, migration := range manifest {
		if migration.Version <= 126 {
			mainPrefix = append(mainPrefix, recordedMigration{version: migration.Version, name: migration.Name, checksum: migration.Checksum[:]})
		}
		if migration.Version == 127 {
			featureToolkit = recordedMigration{version: 126, name: migration.Name, checksum: migration.Checksum[:]}
		}
	}
	_, err = validateRecordedLedger(manifest, mainPrefix, false)
	require.NoError(t, err)
	_, err = validateRecordedLedger(manifest, []recordedMigration{featureToolkit}, false)
	require.ErrorContains(t, err, "name mismatch")
}
