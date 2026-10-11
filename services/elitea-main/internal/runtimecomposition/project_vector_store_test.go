package runtimecomposition

// These cases pin the rules that decide whether the project_pgvector step does
// anything, and what its compensation removes. The provisioning itself needs a
// live PostgreSQL and is proven in
// application/projectprovisioning/provisioner_pgvector_postgres_integration_test.go.

import (
	"context"
	"errors"
	"testing"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	vectorstoreapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/vectorstore"
	"github.com/jackc/pgx/v5"
)

func TestProvisionProjectVectorStoreSkipsWhenNoBootstrapIsConfigured(t *testing.T) {
	t.Parallel()

	materials := &projectVectorStoreMaterialsStub{}
	store := newProjectVectorStoreForTest(t, &projectVectorStoreFinderStub{}, &projectVectorStoreUnsecreterStub{}, true)
	store.materials = materials

	if err := store.ProvisionProjectVectorStore(context.Background(), 7); err != nil {
		t.Fatalf("ProvisionProjectVectorStore() = %v, want nil when the deployment runs no vector store", err)
	}
	// Nothing may be written for a project that gets no vector store. Material
	// stored here would name a database the step did not provision.
	if materials.writes != 0 {
		t.Fatalf("vault material was written without a bootstrap: %d writes", materials.writes)
	}
}

func TestProvisionProjectVectorStoreReportsAnUnusableBootstrap(t *testing.T) {
	t.Parallel()

	for name, connectionString := range map[string]any{
		"not a URL":            "elitea-pgvector",
		"unsupported scheme":   "mysql://user:pass@host:3306/db",
		"unredeemed reference": "{{secret.pgvector_connstr}}",
		"empty":                "",
		"not a string":         42,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			materials := &projectVectorStoreMaterialsStub{}
			finder := &projectVectorStoreFinderStub{
				found: true,
				data:  map[string]any{"connection_string": connectionString},
			}
			// An unsecreter that fails stands in for the public project having
			// no vault, which is the normal case for a clear-text bootstrap.
			store := newProjectVectorStoreForTest(t, finder,
				&projectVectorStoreUnsecreterStub{err: errors.New("no vault")}, true)
			store.materials = materials

			err := store.ProvisionProjectVectorStore(context.Background(), 7)
			if !errors.Is(err, ErrProjectVectorStoreBootstrap) {
				t.Fatalf("error = %v, want ErrProjectVectorStoreBootstrap", err)
			}
			if materials.writes != 0 {
				t.Fatalf("vault material was written for an unusable bootstrap: %d writes", materials.writes)
			}
		})
	}
}

// TestProvisionProjectVectorStoreRedeemsOnlyAReference pins the reason the
// unsecreter is called conditionally.
//
// It loads the OWNING project's vault and fails when that vault does not exist.
// The public project has no vault until something writes a secret there, so
// calling it for a bootstrap stored in the clear would reject the configuration
// every single-tenant deployment actually has.
func TestProvisionProjectVectorStoreRedeemsOnlyAReference(t *testing.T) {
	t.Parallel()

	t.Run("clear text is not redeemed", func(t *testing.T) {
		t.Parallel()
		unsecreter := &projectVectorStoreUnsecreterStub{err: errors.New("public project has no vault")}
		finder := &projectVectorStoreFinderStub{
			found: true,
			// Port 1 refuses immediately, so this reaches the connect attempt
			// without waiting for a timeout.
			data: map[string]any{"connection_string": "postgresql+psycopg://u:p@127.0.0.1:1/elitea"},
		}
		store := newProjectVectorStoreForTest(t, finder, unsecreter, true)

		// The provision fails at the database, which is past resolution. What
		// this case asserts is that the failure is NOT the unsecreter's.
		err := store.ProvisionProjectVectorStore(context.Background(), 7)
		if errors.Is(err, ErrProjectVectorStoreBootstrap) {
			t.Fatalf("a clear-text bootstrap was rejected during resolution: %v", err)
		}
		if unsecreter.calls != 0 {
			t.Fatalf("the unsecreter ran for a bootstrap with no reference: %d calls", unsecreter.calls)
		}
	})

	t.Run("a reference is redeemed", func(t *testing.T) {
		t.Parallel()
		unsecreter := &projectVectorStoreUnsecreterStub{
			data: map[string]any{"connection_string": "postgresql://u:p@127.0.0.1:1/elitea"},
		}
		finder := &projectVectorStoreFinderStub{
			found: true,
			data:  map[string]any{"connection_string": "{{secret.pgvector_connstr}}"},
		}
		store := newProjectVectorStoreForTest(t, finder, unsecreter, true)

		err := store.ProvisionProjectVectorStore(context.Background(), 7)
		if errors.Is(err, ErrProjectVectorStoreBootstrap) {
			t.Fatalf("a redeemed bootstrap was rejected during resolution: %v", err)
		}
		if unsecreter.calls != 1 {
			t.Fatalf("the unsecreter ran %d times for a bootstrap reference, want 1", unsecreter.calls)
		}
		if unsecreter.projectID != referencePublicProjectID {
			t.Fatalf("the reference was redeemed against project %d, want the public project %d",
				unsecreter.projectID, referencePublicProjectID)
		}
	})
}

// TestRemoveProjectVectorStoreRemovesTheConfigurationRow covers what this
// method still owns.
//
// IT NO LONGER REMOVES THE VAULT (#399). The project_secrets step owns the
// vault, and removeProjectSecrets removes it. A second remover here would be a
// second owner, which is the shape that let two creators disagree about the
// master key in the first place.
func TestRemoveProjectVectorStoreRemovesTheConfigurationRow(t *testing.T) {
	t.Parallel()

	configurations := &projectVectorStoreConfigurationsStub{}
	store := newProjectVectorStoreForTest(t, &projectVectorStoreFinderStub{},
		&projectVectorStoreUnsecreterStub{}, true)
	store.configurations = configurations

	if err := store.RemoveProjectVectorStore(context.Background(), 7); err != nil {
		t.Fatalf("RemoveProjectVectorStore() = %v", err)
	}
	if configurations.deleted != 1 || configurations.title != vectorstoreapp.DefaultProjectPgvectorTitle {
		t.Fatalf("configuration delete = %d calls, title %q", configurations.deleted, configurations.title)
	}
}

// TestRemoveProjectVectorStoreSkipsAnAbsentTenant covers project deletion, which
// runs every remove including those for steps that never created anything.
func TestRemoveProjectVectorStoreSkipsAnAbsentTenant(t *testing.T) {
	t.Parallel()

	configurations := &projectVectorStoreConfigurationsStub{}
	store := newProjectVectorStoreForTest(t, &projectVectorStoreFinderStub{},
		&projectVectorStoreUnsecreterStub{}, false)
	store.configurations = configurations

	if err := store.RemoveProjectVectorStore(context.Background(), 7); err != nil {
		t.Fatalf("RemoveProjectVectorStore() = %v, want nil for a tenant that is already gone", err)
	}
	if configurations.deleted != 0 {
		t.Fatalf("the delete ran against an absent tenant: %d calls", configurations.deleted)
	}
}

// TestRemoveProjectVectorStoreReportsARowFailure keeps the compensation honest.
// A remove that swallowed the error would report a clean rollback while the
// tenant kept a configuration row naming a database nothing can reach.
func TestRemoveProjectVectorStoreReportsARowFailure(t *testing.T) {
	t.Parallel()

	configurations := &projectVectorStoreConfigurationsStub{err: errors.New("row delete failed")}
	store := newProjectVectorStoreForTest(t, &projectVectorStoreFinderStub{},
		&projectVectorStoreUnsecreterStub{}, true)
	store.configurations = configurations

	if err := store.RemoveProjectVectorStore(context.Background(), 7); err == nil {
		t.Fatal("RemoveProjectVectorStore() reported success while the row delete failed")
	}
}

/* ── fixture ───────────────────────────────────────────────────────────── */

const referencePublicProjectID = 1

func newProjectVectorStoreForTest(
	t *testing.T,
	finder *projectVectorStoreFinderStub,
	unsecreter *projectVectorStoreUnsecreterStub,
	tenantPresent bool,
) *ProjectVectorStore {
	t.Helper()
	store, err := newProjectVectorStore(
		referencePublicProjectID,
		projectVectorStoreSchemasStub{present: tenantPresent},
		finder,
		unsecreter,
		&projectVectorStoreMaterialsStub{},
		&projectVectorStoreConfigurationsStub{},
		nil,
	)
	if err != nil {
		t.Fatalf("newProjectVectorStore() error = %v", err)
	}
	return store
}

type projectVectorStoreFinderStub struct {
	found bool
	err   error
	data  map[string]any
	calls int
}

func (s *projectVectorStoreFinderStub) FindByEliteaTitle(
	_ context.Context, projectID int32, _ string, _ bool,
) (configurationapp.CurrentExpansionConfiguration, bool, error) {
	s.calls++
	if s.err != nil {
		return configurationapp.CurrentExpansionConfiguration{}, false, s.err
	}
	return configurationapp.CurrentExpansionConfiguration{
		ProjectID: projectID,
		Type:      vectorstoreapp.ProjectPgvectorType,
		Data:      s.data,
	}, s.found, nil
}

type projectVectorStoreUnsecreterStub struct {
	data      map[string]any
	err       error
	calls     int
	projectID int32
}

func (s *projectVectorStoreUnsecreterStub) Unsecret(
	_ context.Context, projectID int32, data map[string]any,
) (map[string]any, error) {
	s.calls++
	s.projectID = projectID
	if s.err != nil {
		return nil, s.err
	}
	if s.data != nil {
		return s.data, nil
	}
	return data, nil
}

type projectVectorStoreConfigurationsStub struct {
	deleted int
	title   string
	err     error
}

func (s *projectVectorStoreConfigurationsStub) UpsertProjectPgvectorConfiguration(
	_ context.Context, _ vectorstoreapp.ProjectConfiguration,
) (int32, error) {
	return 1, nil
}

func (s *projectVectorStoreConfigurationsStub) DeleteProjectPgvectorConfiguration(
	_ context.Context, _ int64, title string,
) (bool, error) {
	s.deleted++
	s.title = title
	return true, s.err
}

type projectVectorStoreMaterialsStub struct{ writes int }

func (*projectVectorStoreMaterialsStub) LoadProjectPgvectorMaterial(
	_ context.Context, _ int64,
) (vectorstoreapp.ProjectMaterial, error) {
	return vectorstoreapp.ProjectMaterial{}, nil
}

func (s *projectVectorStoreMaterialsStub) StoreProjectPgvectorMaterial(
	_ context.Context, _ int64, _ string, _ string,
) error {
	s.writes++
	return nil
}

type projectVectorStoreSchemasStub struct{ present bool }

func (s projectVectorStoreSchemasStub) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	return projectVectorStoreRowStub{tenantPresent: s.present}
}

type projectVectorStoreRowStub struct{ tenantPresent bool }

func (r projectVectorStoreRowStub) Scan(dest ...any) error {
	if len(dest) != 1 {
		return errors.New("unexpected scan arity")
	}
	target, ok := dest[0].(*bool)
	if !ok {
		return errors.New("unexpected scan type")
	}
	*target = r.tenantPresent
	return nil
}

// The drop reads the bootstrap whatever hadStore says, because a database can
// exist without its configuration row (#1211). hadStore decides only what the
// ABSENCE of a bootstrap means: with none configured, no recorded store is a
// skip ("", nil), a recorded one is ErrProjectVectorStoreBootstrapMissing.
func TestDropProjectVectorStoreAttemptsWhateverTheProbeSaid(t *testing.T) {
	t.Parallel()

	t.Run("a configured bootstrap is used even for an unrecorded store", func(t *testing.T) {
		t.Parallel()
		finder := &projectVectorStoreFinderStub{
			found: true,
			err:   errors.New("the bootstrap is read"),
		}
		store := newProjectVectorStoreForTest(t, finder, &projectVectorStoreUnsecreterStub{}, true)

		database, err := store.DropProjectVectorStore(context.Background(), 7, false)
		if err == nil || database != "project_7" {
			t.Fatalf("DropProjectVectorStore(hadStore=false) = (%q, %v), want project_7 and the bootstrap error", database, err)
		}
		if finder.calls != 1 {
			t.Fatalf("the bootstrap was read %d times, want 1", finder.calls)
		}
	})

	t.Run("no bootstrap: a skip when none was recorded", func(t *testing.T) {
		t.Parallel()
		finder := &projectVectorStoreFinderStub{found: false}
		store := newProjectVectorStoreForTest(t, finder, &projectVectorStoreUnsecreterStub{}, true)

		database, err := store.DropProjectVectorStore(context.Background(), 7, false)
		if err != nil || database != "" {
			t.Fatalf("DropProjectVectorStore(hadStore=false, no bootstrap) = (%q, %v), want a skip", database, err)
		}
	})

	t.Run("no bootstrap: a recorded store is a leak", func(t *testing.T) {
		t.Parallel()
		finder := &projectVectorStoreFinderStub{found: false}
		store := newProjectVectorStoreForTest(t, finder, &projectVectorStoreUnsecreterStub{}, true)

		database, err := store.DropProjectVectorStore(context.Background(), 7, true)
		if !errors.Is(err, ErrProjectVectorStoreBootstrapMissing) || database != "project_7" {
			t.Fatalf("DropProjectVectorStore(hadStore=true, no bootstrap) = (%q, %v), want project_7 and ErrProjectVectorStoreBootstrapMissing", database, err)
		}
	})
}
