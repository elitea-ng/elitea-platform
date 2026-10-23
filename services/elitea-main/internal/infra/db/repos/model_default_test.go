package repos

import (
	"context"
	"errors"
	"testing"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/centrysecrets"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestCurrentSecretVaultRepositorySetsBothModelDefaultsAtomically(t *testing.T) {
	store := &currentVaultSharedStore{
		key:       []byte(currentVaultProjectKey),
		vault:     []byte(currentVaultToken),
		updateTag: pgconn.NewCommandTag("UPDATE 1"),
	}
	repository, err := newCurrentSecretVaultRepository(store, nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := repository.SetCurrentModelDefault(context.Background(), configurationapp.CurrentModelDefaultSelection{
		ProjectID: 7, Name: "embed-current", TargetProjectID: 2, Section: "embedding",
	}); err != nil {
		t.Fatal(err)
	}
	if len(store.options) != 1 || store.options[0].IsoLevel != pgx.ReadCommitted || store.options[0].AccessMode != pgx.ReadWrite {
		t.Fatalf("transaction options=%#v", store.options)
	}
	if len(store.execArgs) != 2 || store.execArgs[0] != "project-7" {
		t.Fatalf("update args=%#v", store.execArgs)
	}

	rewritten, ok := store.execArgs[1].([]byte)
	if !ok || len(rewritten) == 0 {
		t.Fatalf("rewritten argument=%T", store.execArgs[1])
	}
	vault, err := centrysecrets.OpenUnwrapped([]byte(currentVaultProjectKey), rewritten)
	if err != nil {
		t.Fatal(err)
	}
	name, err := vault.LookupRegular("default_embedding_model_name")
	if err != nil || name.Value != "embed-current" || name.Hidden {
		t.Fatalf("name=%#v err=%v", name, err)
	}
	projectID, err := vault.LookupRegularProjectID("default_embedding_model_project_id")
	if err != nil || projectID.Value != "2" || projectID.Hidden {
		t.Fatalf("project id=%#v err=%v", projectID, err)
	}
	if _, err := vault.LookupRegular("default_embedding_model_project_id"); !errors.Is(err, centrysecrets.ErrInvalidSecret) {
		t.Fatalf("project id was not stored as the current JSON integer: %v", err)
	}
}
