package pgvault

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/centrysecrets"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore"
)

const (
	nameCanary  = "namecanary_db_password"
	valueCanary = "valuecanary-s3cr3t"
)

type fakeVault struct {
	values map[string]centrysecrets.Secret
}

func (v fakeVault) Lookup(name string) (centrysecrets.Secret, error) {
	if secret, ok := v.values[name]; ok {
		return secret, nil
	}
	return centrysecrets.Secret{}, centrysecrets.ErrSecretNotFound
}
func (v fakeVault) LookupRegular(string) (centrysecrets.Secret, error) { panic("unused") }
func (v fakeVault) LookupProjectID(string) (centrysecrets.Secret, error) {
	panic("unused")
}
func (v fakeVault) LookupRegularProjectID(string) (centrysecrets.Secret, error) {
	panic("unused")
}
func (v fakeVault) LookupRegularInteger(string) (centrysecrets.Secret, error) {
	panic("unused")
}

type fakeLoader struct {
	vault storage.SecretVault
	err   error
	calls int
}

func (l *fakeLoader) LoadProjectVault(_ context.Context, projectID int64) (storage.SecretVault, error) {
	l.calls++
	if projectID != 7 {
		return nil, storage.ErrVaultAbsent
	}
	return l.vault, l.err
}
func (l *fakeLoader) LoadAdminVault(context.Context) (storage.SecretVault, error) {
	panic("the admin vault must never be read")
}

type fakeSealer struct {
	sealed  []configurationapp.HiddenSecretMutation
	purged  []string
	err     error
	inTx    bool
	project int64
}

func (s *fakeSealer) SealProjectHiddenSecrets(_ context.Context, tx pgx.Tx, projectID int64, mutations []configurationapp.HiddenSecretMutation) error {
	s.inTx, s.project = tx != nil, projectID
	s.sealed = append(s.sealed, mutations...)
	return s.err
}
func (s *fakeSealer) PurgeProjectHiddenSecrets(_ context.Context, tx pgx.Tx, projectID int64, names []string) error {
	s.inTx, s.project = tx != nil, projectID
	s.purged = append(s.purged, names...)
	return s.err
}

type fakeTx struct {
	pgx.Tx
	locks     []string
	committed bool
}

func (tx *fakeTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "pg_advisory_xact_lock") {
		tx.locks = append(tx.locks, args[0].(string))
	}
	return pgconn.NewCommandTag("SELECT 1"), nil
}
func (tx *fakeTx) Commit(context.Context) error   { tx.committed = true; return nil }
func (tx *fakeTx) Rollback(context.Context) error { return nil }

type fakeDB struct{ txs []*fakeTx }

func (d *fakeDB) Begin(context.Context) (pgx.Tx, error) {
	tx := &fakeTx{}
	d.txs = append(d.txs, tx)
	return tx, nil
}

func sc() secretstore.StoreContext {
	return secretstore.StoreContext{
		TenantProjectID: 7, CredentialRefID: "cref-1", DecisionID: "dec-1",
		WorkloadScope: "agent-runtime", PolicyRevisionID: "2026-10-r1",
	}
}

func ref(name string) secretstore.SecretRef {
	return secretstore.SecretRef{Backend: secretstore.BackendPGVault, Path: name}
}

func newStore(t *testing.T, values map[string]centrysecrets.Secret) (*Store, *fakeLoader, *fakeSealer, *fakeDB) {
	t.Helper()
	loader := &fakeLoader{vault: fakeVault{values: values}}
	sealer := &fakeSealer{}
	db := &fakeDB{}
	store, err := New(db, loader, sealer)
	if err != nil {
		t.Fatal(err)
	}
	return store, loader, sealer, db
}

func assertClean(t *testing.T, err error) {
	t.Helper()
	if err != nil && (strings.Contains(err.Error(), "namecanary") || strings.Contains(err.Error(), "valuecanary")) {
		t.Fatalf("error leaks a name or value: %s", err)
	}
}

func TestReadReturnsTheDirectLookupValue(t *testing.T) {
	store, _, _, _ := newStore(t, map[string]centrysecrets.Secret{nameCanary: {Value: valueCanary, Hidden: true}})
	got, err := store.Read(t.Context(), sc(), ref(nameCanary), 1)
	if err != nil || got.Version != 1 || string(got.Value.UnsafeBytes()) != valueCanary {
		t.Fatalf("read = %+v, %v", got, err)
	}
}

func TestReadRefusesVersionZeroAndOthers(t *testing.T) {
	store, loader, _, _ := newStore(t, map[string]centrysecrets.Secret{nameCanary: {Value: valueCanary}})
	if _, err := store.Read(t.Context(), sc(), ref(nameCanary), 0); !errors.Is(err, secretstore.ErrInvalidRequest) {
		t.Fatalf("version 0 = %v", err)
	}
	if _, err := store.Read(t.Context(), sc(), ref(nameCanary), 2); !errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("version 2 = %v", err)
	}
	if loader.calls != 0 {
		t.Fatal("a refused read must not load the vault")
	}
}

func TestReadErrorMapping(t *testing.T) {
	cases := map[string]struct {
		loader *fakeLoader
		sc     func(*secretstore.StoreContext)
		want   error
	}{
		"absent vault":   {&fakeLoader{}, func(c *secretstore.StoreContext) { c.TenantProjectID = 8 }, secretstore.ErrNotFound},
		"no such secret": {&fakeLoader{vault: fakeVault{}}, nil, secretstore.ErrNotFound},
		"unavailable":    {&fakeLoader{err: storage.ErrContentUnavailable}, nil, secretstore.ErrUnavailable},
		"rejected":       {&fakeLoader{err: storage.ErrContentRejected}, nil, secretstore.ErrInvalidRequest},
		"nil vault":      {&fakeLoader{}, nil, secretstore.ErrUnavailable},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store, err := New(&fakeDB{}, tc.loader, &fakeSealer{})
			if err != nil {
				t.Fatal(err)
			}
			context := sc()
			if tc.sc != nil {
				tc.sc(&context)
			}
			_, err = store.Read(t.Context(), context, ref(nameCanary), 1)
			if !errors.Is(err, tc.want) {
				t.Fatalf("read = %v, want %v", err, tc.want)
			}
			assertClean(t, err)
		})
	}
}

func TestRefusesForeignReferences(t *testing.T) {
	store, _, _, _ := newStore(t, nil)
	other := ref(nameCanary)
	other.Backend = secretstore.BackendVault
	if _, err := store.Read(t.Context(), sc(), other, 1); !errors.Is(err, secretstore.ErrMisconfigured) {
		t.Fatalf("vault ref = %v", err)
	}
	for _, bad := range []secretstore.SecretRef{
		{Backend: secretstore.BackendPGVault, Mount: "admin", Path: nameCanary},
		{Backend: secretstore.BackendPGVault, Path: "has-dash"},
		{Backend: secretstore.BackendPGVault, Path: ""},
	} {
		if _, err := store.Read(t.Context(), sc(), bad, 1); !errors.Is(err, secretstore.ErrInvalidRequest) {
			t.Fatalf("ref %+v = %v", bad, err)
		}
	}
}

func TestWriteCreatesVersionOneUnderTheLock(t *testing.T) {
	store, _, sealer, db := newStore(t, map[string]centrysecrets.Secret{})
	version, err := store.Write(t.Context(), sc(), ref(nameCanary), secretstore.NewRuntimeSecret([]byte(valueCanary)), 0)
	if err != nil || version != 1 {
		t.Fatalf("write = %d, %v", version, err)
	}
	if len(sealer.sealed) != 1 || sealer.sealed[0].Name != nameCanary || sealer.sealed[0].Value != valueCanary || !sealer.inTx || sealer.project != 7 {
		t.Fatalf("seal = %+v", sealer)
	}
	if len(db.txs) != 1 || !db.txs[0].committed || len(db.txs[0].locks) != 1 || db.txs[0].locks[0] != "elitea.secretstore.pgvault:project-7" {
		t.Fatalf("tx = %+v", db.txs)
	}
}

func TestWriteRefusals(t *testing.T) {
	existing := map[string]centrysecrets.Secret{nameCanary: {Value: "old"}}
	store, _, sealer, _ := newStore(t, existing)
	value := secretstore.NewRuntimeSecret([]byte(valueCanary))
	if _, err := store.Write(t.Context(), sc(), ref(nameCanary), value, 0); !errors.Is(err, secretstore.ErrVersionConflict) {
		t.Fatalf("existing name = %v", err)
	}
	if _, err := store.Write(t.Context(), sc(), ref(nameCanary), value, 1); !errors.Is(err, secretstore.ErrMisconfigured) {
		t.Fatalf("cas 1 = %v", err)
	}
	if _, err := store.Write(t.Context(), sc(), ref("fresh"), secretstore.NewRuntimeSecret(nil), 0); !errors.Is(err, secretstore.ErrInvalidRequest) {
		t.Fatalf("empty value = %v", err)
	}
	if _, err := store.Write(t.Context(), sc(), ref("fresh"), secretstore.NewRuntimeSecret([]byte{0xff, 0xfe}), 0); !errors.Is(err, secretstore.ErrInvalidRequest) {
		t.Fatalf("non-UTF-8 value = %v", err)
	}
	if len(sealer.sealed) != 0 {
		t.Fatal("a refused write must not seal")
	}
	sealer.err = errors.New("seal failed for " + nameCanary)
	_, err := store.Write(t.Context(), sc(), ref("fresh"), value, 0)
	if !errors.Is(err, secretstore.ErrUnavailable) {
		t.Fatalf("seal failure = %v", err)
	}
	assertClean(t, err)
}

func TestRetire(t *testing.T) {
	store, _, sealer, db := newStore(t, nil)
	if err := store.Retire(t.Context(), sc(), ref(nameCanary), 2); !errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("retire v2 = %v", err)
	}
	if err := store.Retire(t.Context(), sc(), ref(nameCanary), 1); err != nil {
		t.Fatal(err)
	}
	if len(sealer.purged) != 1 || sealer.purged[0] != nameCanary || !db.txs[0].committed {
		t.Fatalf("purge = %+v", sealer)
	}
}

func TestMetadata(t *testing.T) {
	store, _, _, _ := newStore(t, map[string]centrysecrets.Secret{nameCanary: {Value: valueCanary}})
	meta, err := store.Metadata(t.Context(), sc(), ref(nameCanary))
	if err != nil || meta.CurrentVersion != 1 || len(meta.Versions) != 1 || !meta.CASRequired {
		t.Fatalf("metadata = %+v, %v", meta, err)
	}
	if _, err := store.Metadata(t.Context(), sc(), ref("absent")); !errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("absent = %v", err)
	}
}
