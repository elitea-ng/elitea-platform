package migrate

import (
    "testing"
    "github.com/stretchr/testify/require"
    platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

func TestMergedMainLedgerAcceptsMainPrefixAndRefusesUnreconciledFeatureVersion(t *testing.T) {
    manifest, err := LoadManifest(platformmigrations.Files, ScopeShared)
    require.NoError(t, err)
    var mainPrefix []recordedMigration
    var feature125 recordedMigration
    for _, migration := range manifest {
        if migration.Version <= 125 {
            mainPrefix = append(mainPrefix, recordedMigration{version: migration.Version, name: migration.Name, checksum: migration.Checksum[:]})
        }
        if migration.Version == 126 {
            feature125 = recordedMigration{version: 125, name: migration.Name, checksum: migration.Checksum[:]}
        }
    }
    _, err = validateRecordedLedger(manifest, mainPrefix, false)
    require.NoError(t, err)
    _, err = validateRecordedLedger(manifest, []recordedMigration{feature125}, false)
    require.ErrorContains(t, err, "name mismatch")
}
