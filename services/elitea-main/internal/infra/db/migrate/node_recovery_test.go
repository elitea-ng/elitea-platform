package migrate

import (
	"crypto/sha256"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
	"testing"
)

func TestNodeRecoveryEmbeddedSharedMigrationHasExactChecksum(t *testing.T) {
	shared, err := LoadManifest(platformmigrations.Files, ScopeShared)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := platformmigrations.Files.ReadFile("shared/0146_node_recovery_control.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range shared {
		if migration.Version == 146 {
			if migration.Checksum != sha256.Sum256(raw) {
				t.Fatal("migration checksum not bound")
			}
			return
		}
	}
	t.Fatal("node recovery migration is absent")
}
