package repos

import (
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/applications"
)

// ADR-0050 decision 1a (issue 1604): the soul is part of the ADR-0040
// release digest, and a version without a soul digests the bytes it did
// before tenant migration 0160. CI-only: skipped without the integration
// database.

func soulTestCandidate(t *testing.T, pool *pgxpool.Pool, store *AgentReleases) [32]byte {
	t.Helper()
	var digest [32]byte
	err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		var err error
		digest, err = store.CandidateDigestTx(t.Context(), tx, 1, 31, 41)
		return err
	})
	if err != nil {
		t.Fatalf("candidate digest: %v", err)
	}
	return digest
}

func soulTestSave(t *testing.T, repo *ApplicationsRepo, soul json.RawMessage) applications.Version {
	t.Helper()
	v := applications.Version{Soul: soul, EditorID: 11}
	v.Present.Soul = true
	saved, err := repo.UpdateVersion(t.Context(), "1", "31", "41", v)
	if err != nil {
		t.Fatalf("save the soul: %v", err)
	}
	return saved
}

func TestAgentSoul_ChangesTheReleaseDigestAndClearingRestoresIt(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedAgentReleaseFixture(t, pool)
	store, resolver := newReleaseTestStores(t, pool)
	repo := NewApplicationsRepo(pool)

	without := soulTestCandidate(t, pool, store)

	saved := soulTestSave(t, repo, json.RawMessage(`{"schema":"elitea.soul/v1","display_name":"Mira","voice":["warm"]}`))
	if string(saved.Soul) == "" {
		t.Fatal("the save did not read the soul back")
	}
	mira := soulTestCandidate(t, pool, store)
	if mira == without {
		t.Fatal("adding a soul left the release digest unchanged")
	}

	soulTestSave(t, repo, json.RawMessage(`{"schema":"elitea.soul/v1","display_name":"Mira","voice":["terse"]}`))
	if terse := soulTestCandidate(t, pool, store); terse == mira || terse == without {
		t.Fatal("editing the soul left the release digest unchanged")
	}

	// The run definition carries the soul for the worker.
	target, err := releaseTestResolve(t, resolver, 0)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Soul map[string]any `json:"soul"`
	}
	if err := json.Unmarshal(target.SourceVersionDetails, &document); err != nil || document.Soul["display_name"] != "Mira" {
		t.Fatalf("the run definition has no soul: %s (%v)", target.SourceVersionDetails, err)
	}

	soulTestSave(t, repo, nil)
	if cleared := soulTestCandidate(t, pool, store); cleared != without {
		t.Fatal("clearing the soul did not restore the original digest: the projection must omit an absent soul")
	}

	// Every soul save appended a revision whose snapshot names the soul.
	var revisions int
	if err := pool.QueryRow(t.Context(), `
SELECT count(*) FROM elitea_lifecycle.version_revisions
WHERE tenant_project_id = 1 AND application_version_id = 41 AND kind = 'save'`).Scan(&revisions); err != nil {
		t.Fatal(err)
	}
	if revisions != 3 {
		t.Fatalf("revisions = %d, want one per soul save", revisions)
	}
}
