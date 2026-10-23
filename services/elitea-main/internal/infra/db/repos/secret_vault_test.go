package repos

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/centrysecrets"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	currentVaultProjectKey = "ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8="
	currentVaultToken      = "gAAAAABlU_EBsLGys7S1tre4ubq7vL2-vx1XMkCj-NAPVrz2qJjob7g8g2X5uZRKHkqRYf3PrTLUC8Q1IHnCMja09Xr6VixBNDJNqcJhTDidsE3D9XlcDpLfJ6e5zNz6DsTP67crLz-PvCJO0qwoNSpc2vwiLlTkf2xnyvlVOAMXlrmueSNrVxUoOGRzpK_fci7UQqhXtn2DDrjEgHLzW77baCUbY6nqH4w48HOBwzsCN7Y6dpkZkns7IK5pFKZs4WwYxYbAU6Q0"
)

func TestCurrentSecretVaultRepositoryMutatesExistingProjectAtomically(t *testing.T) {
	store := &currentVaultSharedStore{
		key:       []byte(currentVaultProjectKey),
		vault:     []byte(currentVaultToken),
		updateTag: pgconn.NewCommandTag("UPDATE 1"),
	}
	repository, err := newCurrentSecretVaultRepository(store, nil)
	if err != nil {
		t.Fatal(err)
	}

	err = repository.MutateProject(context.Background(), 7, []centrysecrets.Mutation{
		{Collection: centrysecrets.HiddenSecrets, Name: "new_hidden", Value: "new-hidden-value"},
		{Collection: centrysecrets.RegularSecrets, Name: "default_embedding_model_name", Value: "embedding-current"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(store.options) != 1 || store.options[0].IsoLevel != pgx.ReadCommitted || store.options[0].AccessMode != pgx.ReadWrite {
		t.Fatalf("transaction options=%#v", store.options)
	}
	if !strings.Contains(store.query, "FOR UPDATE OF k, d") || !strings.Contains(store.query, "centry.secrets_key") || !strings.Contains(store.execSQL, "centry.secrets_data") {
		t.Fatalf("query=%q update=%q", store.query, store.execSQL)
	}
	if len(store.queryArgs) != 1 || store.queryArgs[0] != "project-7" || len(store.execArgs) != 2 || store.execArgs[0] != "project-7" {
		t.Fatalf("query args=%#v update args=%#v", store.queryArgs, store.execArgs)
	}

	rewritten, ok := store.execArgs[1].([]byte)
	if !ok || len(rewritten) == 0 {
		t.Fatalf("rewritten argument=%T", store.execArgs[1])
	}
	vault, err := centrysecrets.OpenUnwrapped([]byte(currentVaultProjectKey), rewritten)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := vault.Lookup("new_hidden")
	if err != nil || secret.Value != "new-hidden-value" || !secret.Hidden {
		t.Fatalf("hidden secret=%#v err=%v", secret, err)
	}
	defaultModel, err := vault.LookupRegular("default_embedding_model_name")
	if err != nil || defaultModel.Value != "embedding-current" || defaultModel.Hidden {
		t.Fatalf("default model=%#v err=%v", defaultModel, err)
	}
	original, err := vault.LookupRegular("normal")
	if err != nil || original.Value != "normal-canary" {
		t.Fatalf("existing value=%#v err=%v", original, err)
	}
}

func TestCurrentSecretVaultRepositoryAdminRoutingAndMutationValidation(t *testing.T) {
	store := &currentVaultSharedStore{
		key:       []byte(currentVaultProjectKey),
		vault:     []byte(currentVaultToken),
		updateTag: pgconn.NewCommandTag("UPDATE 1"),
	}
	repository, err := newCurrentSecretVaultRepository(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.MutateAdmin(context.Background(), []centrysecrets.Mutation{{Collection: centrysecrets.RegularSecrets, Name: "default_llm_model_name", Value: "public-model"}}); err != nil {
		t.Fatal(err)
	}
	if store.queryArgs[0] != "admin" || store.execArgs[0] != "admin" {
		t.Fatalf("admin routing query=%#v update=%#v", store.queryArgs, store.execArgs)
	}

	before := len(store.options)
	invalid := [][]centrysecrets.Mutation{
		nil,
		{{Collection: centrysecrets.SecretCollection(99), Name: "valid", Value: "must-not-appear"}},
	}
	for _, mutations := range invalid {
		err := repository.MutateProject(context.Background(), 7, mutations)
		if !errors.Is(err, ErrInvalidCurrentVaultMutation) {
			t.Fatalf("invalid mutation error=%v", err)
		}
		if err != nil && strings.Contains(err.Error(), "must-not-appear") {
			t.Fatalf("invalid mutation leaked value: %v", err)
		}
	}
	if err := repository.MutateProject(context.Background(), 0, []centrysecrets.Mutation{{Collection: centrysecrets.RegularSecrets, Name: "valid"}}); !errors.Is(err, ErrInvalidCurrentVaultMutation) {
		t.Fatalf("project identity error=%v", err)
	}
	if len(store.options) != before+1 {
		// The structurally invalid collection is detected by the codec inside the
		// transaction; nil and invalid project IDs are rejected before storage.
		t.Fatalf("unexpected transaction calls=%d before=%d", len(store.options), before)
	}
}

func TestCurrentSecretVaultRepositoryMapsStorageFailuresAndCancellation(t *testing.T) {
	tests := []struct {
		name  string
		store *currentVaultSharedStore
	}{
		{name: "missing row", store: &currentVaultSharedStore{queryErr: pgx.ErrNoRows}},
		{name: "query failure", store: &currentVaultSharedStore{queryErr: errors.New("database-sensitive-detail")}},
		{name: "zero update", store: &currentVaultSharedStore{key: []byte(currentVaultProjectKey), vault: []byte(currentVaultToken), updateTag: pgconn.NewCommandTag("UPDATE 0")}},
		{name: "update failure", store: &currentVaultSharedStore{key: []byte(currentVaultProjectKey), vault: []byte(currentVaultToken), execErr: errors.New("database-sensitive-detail")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository, err := newCurrentSecretVaultRepository(test.store, nil)
			if err != nil {
				t.Fatal(err)
			}
			err = repository.MutateProject(context.Background(), 7, []centrysecrets.Mutation{{Collection: centrysecrets.HiddenSecrets, Name: "valid", Value: "must-not-appear"}})
			if !errors.Is(err, ErrCurrentVaultUnavailable) || strings.Contains(err.Error(), "sensitive-detail") || strings.Contains(err.Error(), "must-not-appear") {
				t.Fatalf("mapped error=%v", err)
			}
		})
	}

	store := &currentVaultSharedStore{}
	repository, err := newCurrentSecretVaultRepository(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = repository.MutateProject(ctx, 7, []centrysecrets.Mutation{{Collection: centrysecrets.HiddenSecrets, Name: "valid", Value: "value"}})
	if !errors.Is(err, context.Canceled) || len(store.options) != 0 {
		t.Fatalf("canceled error=%v transactions=%d", err, len(store.options))
	}
}

func TestCurrentSecretVaultRepositoryOwnsAndClearsMasterKey(t *testing.T) {
	master := []byte("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=")
	repository, err := newCurrentSecretVaultRepository(&currentVaultSharedStore{}, master)
	if err != nil {
		t.Fatal(err)
	}
	master[0] = 'X'
	if repository.masterKey[0] == 'X' {
		t.Fatal("repository aliases caller master key")
	}
	repository.Destroy()
	if repository.masterKey != nil {
		t.Fatal("repository retained master key after destroy")
	}
	if err := repository.MutateAdmin(context.Background(), []centrysecrets.Mutation{{Collection: centrysecrets.RegularSecrets, Name: "valid", Value: "value"}}); !errors.Is(err, ErrCurrentVaultUnavailable) {
		t.Fatalf("destroyed repository error=%v", err)
	}

	if _, err := newCurrentSecretVaultRepository(&currentVaultSharedStore{}, []byte("invalid")); err == nil {
		t.Fatal("invalid master key was accepted")
	}
	if _, err := newCurrentSecretVaultRepository(nil, nil); err == nil {
		t.Fatal("nil store was accepted")
	}
}

type currentVaultSharedStore struct {
	key       []byte
	vault     []byte
	queryErr  error
	execErr   error
	updateTag pgconn.CommandTag

	query     string
	queryArgs []any
	execSQL   string
	execArgs  []any
	options   []pgx.TxOptions
}

func (s *currentVaultSharedStore) WithinTx(_ context.Context, options pgx.TxOptions, fn func(sqlExecutor) error) error {
	s.options = append(s.options, options)
	return fn(s)
}

func (s *currentVaultSharedStore) QueryRow(_ context.Context, query string, args ...any) sqlRow {
	s.query = query
	s.queryArgs = append([]any(nil), args...)
	return currentVaultRow{key: append([]byte(nil), s.key...), vault: append([]byte(nil), s.vault...), err: s.queryErr}
}

func (s *currentVaultSharedStore) Exec(_ context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	s.execSQL = query
	s.execArgs = make([]any, len(args))
	copy(s.execArgs, args)
	if len(s.execArgs) > 1 {
		if value, ok := s.execArgs[1].([]byte); ok {
			s.execArgs[1] = append([]byte(nil), value...)
		}
	}
	return s.updateTag, s.execErr
}

func (*currentVaultSharedStore) Query(context.Context, string, ...any) (sqlRows, error) {
	return nil, errors.New("unexpected query")
}

type currentVaultRow struct {
	key   []byte
	vault []byte
	err   error
}

func (r currentVaultRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != 2 {
		return errors.New("unexpected scan")
	}
	*dest[0].(*[]byte) = r.key
	*dest[1].(*[]byte) = r.vault
	return nil
}
