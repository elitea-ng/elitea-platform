package repos

import (
	"context"
	"errors"
	"testing"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/centrysecrets"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// vaultWithModelDefault returns the encrypted test vault with an LLM default
// naming name in targetProjectID, written through SetCurrentModelDefault.
func vaultWithModelDefault(t *testing.T, name string, targetProjectID int64) []byte {
	t.Helper()
	store := &currentVaultSharedStore{
		key: []byte(currentVaultProjectKey), vault: []byte(currentVaultToken),
		updateTag: pgconn.NewCommandTag("UPDATE 1"),
	}
	repository, err := newCurrentSecretVaultRepository(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.SetCurrentModelDefault(context.Background(), configurationapp.CurrentModelDefaultSelection{
		ProjectID: 7, Name: name, TargetProjectID: targetProjectID, Section: "llm",
	}); err != nil {
		t.Fatal(err)
	}
	rewritten, ok := store.execArgs[1].([]byte)
	if !ok {
		t.Fatalf("rewritten argument=%T", store.execArgs[1])
	}
	return rewritten
}

func TestClearCurrentModelDefaultRemovesAMatchingDefault(t *testing.T) {
	store := &currentVaultSharedStore{
		key: []byte(currentVaultProjectKey), vault: vaultWithModelDefault(t, "gpt-all", 1),
		updateTag: pgconn.NewCommandTag("UPDATE 1"),
	}
	repository, err := newCurrentSecretVaultRepository(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	cleared, err := repository.ClearCurrentModelDefault(context.Background(), configurationapp.CurrentModelDefaultClear{
		ProjectID: 7, Section: "llm", Match: &configurationapp.CurrentModelDefault{Name: "gpt-all", ProjectID: "1"},
	})
	if err != nil || !cleared {
		t.Fatalf("cleared=%v err=%v", cleared, err)
	}
	if len(store.queryArgs) != 1 || store.queryArgs[0] != "project-7" {
		t.Fatalf("locked vault=%#v", store.queryArgs)
	}
	rewritten, ok := store.execArgs[1].([]byte)
	if !ok {
		t.Fatalf("rewritten argument=%T", store.execArgs[1])
	}
	vault, err := centrysecrets.OpenUnwrapped([]byte(currentVaultProjectKey), rewritten)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"default_llm_model_name", "default_llm_model_project_id"} {
		if _, err := vault.LookupRegular(key); !errors.Is(err, centrysecrets.ErrSecretNotFound) {
			t.Fatalf("%s survived the clear: %v", key, err)
		}
	}
	if original, err := vault.LookupRegular("normal"); err != nil || original.Value != "normal-canary" {
		t.Fatalf("the clear touched an unrelated secret: %#v err=%v", original, err)
	}
}

func TestClearCurrentModelDefaultKeepsADifferentChoice(t *testing.T) {
	for _, match := range []configurationapp.CurrentModelDefault{
		{Name: "other-model", ProjectID: "1"},
		{Name: "gpt-all", ProjectID: "7"},
	} {
		store := &currentVaultSharedStore{
			key: []byte(currentVaultProjectKey), vault: vaultWithModelDefault(t, "gpt-all", 1),
			updateTag: pgconn.NewCommandTag("UPDATE 1"),
		}
		repository, err := newCurrentSecretVaultRepository(store, nil)
		if err != nil {
			t.Fatal(err)
		}
		cleared, err := repository.ClearCurrentModelDefault(context.Background(), configurationapp.CurrentModelDefaultClear{
			ProjectID: 7, Section: "llm", Match: &match,
		})
		if err != nil || cleared || store.execSQL != "" {
			t.Fatalf("match %+v: cleared=%v err=%v update=%q", match, cleared, err, store.execSQL)
		}
	}
}

func TestClearCurrentModelDefaultWithoutMatchClearsTheAdminVault(t *testing.T) {
	store := &currentVaultSharedStore{
		key: []byte(currentVaultProjectKey), vault: vaultWithModelDefault(t, "gpt-all", 1),
		updateTag: pgconn.NewCommandTag("UPDATE 1"),
	}
	repository, err := newCurrentSecretVaultRepository(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	cleared, err := repository.ClearCurrentModelDefault(context.Background(), configurationapp.CurrentModelDefaultClear{
		Admin: true, Section: "llm",
	})
	if err != nil || !cleared || store.queryArgs[0] != "admin" {
		t.Fatalf("cleared=%v err=%v vault=%#v", cleared, err, store.queryArgs)
	}
}

func TestClearCurrentModelDefaultTreatsAnEmptyOrAbsentVaultAsNothingToClear(t *testing.T) {
	// The canary vault holds no default.
	store := &currentVaultSharedStore{key: []byte(currentVaultProjectKey), vault: []byte(currentVaultToken)}
	repository, err := newCurrentSecretVaultRepository(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	cleared, err := repository.ClearCurrentModelDefault(context.Background(), configurationapp.CurrentModelDefaultClear{
		ProjectID: 7, Section: "llm",
	})
	if err != nil || cleared || store.execSQL != "" {
		t.Fatalf("empty vault: cleared=%v err=%v update=%q", cleared, err, store.execSQL)
	}

	absent := &currentVaultSharedStore{queryErr: pgx.ErrNoRows}
	repository, err = newCurrentSecretVaultRepository(absent, nil)
	if err != nil {
		t.Fatal(err)
	}
	cleared, err = repository.ClearCurrentModelDefault(context.Background(), configurationapp.CurrentModelDefaultClear{
		ProjectID: 7, Section: "llm",
	})
	if err != nil || cleared {
		t.Fatalf("absent vault: cleared=%v err=%v", cleared, err)
	}
}

func TestClearCurrentModelDefaultRefusesAnInvalidRequest(t *testing.T) {
	repository, err := newCurrentSecretVaultRepository(&currentVaultSharedStore{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []configurationapp.CurrentModelDefaultClear{
		{ProjectID: 0, Section: "llm"},
		{ProjectID: 7},
	} {
		if _, err := repository.ClearCurrentModelDefault(context.Background(), request); !errors.Is(err, ErrInvalidCurrentVaultMutation) {
			t.Fatalf("request %+v err=%v", request, err)
		}
	}
}

// vaultWithHiddenModelDefault returns the encrypted test vault with an LLM
// default in the HIDDEN collection only: a legacy vault the public read
// (storage.LoadPlatformModelDefault) still falls back to.
func vaultWithHiddenModelDefault(t *testing.T, name string, targetProjectID int64) []byte {
	t.Helper()
	store := &currentVaultSharedStore{
		key: []byte(currentVaultProjectKey), vault: []byte(currentVaultToken),
		updateTag: pgconn.NewCommandTag("UPDATE 1"),
	}
	repository, err := newCurrentSecretVaultRepository(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.MutateProject(context.Background(), 1, []centrysecrets.Mutation{
		{Collection: centrysecrets.HiddenSecrets, Name: "default_llm_model_name", Value: name},
		{Collection: centrysecrets.HiddenSecrets, Name: "default_llm_model_project_id", IntegerValue: &targetProjectID},
	}); err != nil {
		t.Fatal(err)
	}
	rewritten, ok := store.execArgs[1].([]byte)
	if !ok {
		t.Fatalf("rewritten argument=%T", store.execArgs[1])
	}
	return rewritten
}

func TestClearCurrentModelDefaultRemovesAHiddenProjectDefault(t *testing.T) {
	for _, match := range []*configurationapp.CurrentModelDefault{nil, {Name: "gpt-all", ProjectID: "1"}} {
		store := &currentVaultSharedStore{
			key: []byte(currentVaultProjectKey), vault: vaultWithHiddenModelDefault(t, "gpt-all", 1),
			updateTag: pgconn.NewCommandTag("UPDATE 1"),
		}
		repository, err := newCurrentSecretVaultRepository(store, nil)
		if err != nil {
			t.Fatal(err)
		}
		cleared, err := repository.ClearCurrentModelDefault(context.Background(), configurationapp.CurrentModelDefaultClear{
			ProjectID: 1, Section: "llm", Match: match,
		})
		if err != nil || !cleared {
			t.Fatalf("match %v: cleared=%v err=%v; the platform read would still show the hidden default", match, cleared, err)
		}
		rewritten, ok := store.execArgs[1].([]byte)
		if !ok {
			t.Fatalf("rewritten argument=%T", store.execArgs[1])
		}
		vault, err := centrysecrets.OpenUnwrapped([]byte(currentVaultProjectKey), rewritten)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := vault.Lookup("default_llm_model_name"); !errors.Is(err, centrysecrets.ErrSecretNotFound) {
			t.Fatalf("the hidden default survived the clear: %v", err)
		}
		if _, err := vault.LookupProjectID("default_llm_model_project_id"); !errors.Is(err, centrysecrets.ErrSecretNotFound) {
			t.Fatalf("the hidden project id survived the clear: %v", err)
		}
	}
}

func TestClearCurrentModelDefaultReadsTheAdminVaultRegularOnly(t *testing.T) {
	// No reader takes a hidden admin default, so a clear leaves it alone.
	store := &currentVaultSharedStore{
		key: []byte(currentVaultProjectKey), vault: vaultWithHiddenModelDefault(t, "gpt-all", 1),
		updateTag: pgconn.NewCommandTag("UPDATE 1"),
	}
	repository, err := newCurrentSecretVaultRepository(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	cleared, err := repository.ClearCurrentModelDefault(context.Background(), configurationapp.CurrentModelDefaultClear{
		Admin: true, Section: "llm",
	})
	if err != nil || cleared || store.execSQL != "" {
		t.Fatalf("cleared=%v err=%v update=%q", cleared, err, store.execSQL)
	}
}

func TestClearCurrentModelDefaultTakesATierKeyAndRefusesAnyOtherKey(t *testing.T) {
	repository, err := newCurrentSecretVaultRepository(&currentVaultSharedStore{queryErr: pgx.ErrNoRows}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ClearCurrentModelDefault(context.Background(), configurationapp.CurrentModelDefaultClear{
		ProjectID: 7, Section: "llm_low_tier",
	}); err != nil {
		t.Fatalf("tier key refused: %v", err)
	}
	if _, err := repository.ClearCurrentModelDefault(context.Background(), configurationapp.CurrentModelDefaultClear{
		ProjectID: 7, Section: "openai_api_key",
	}); !errors.Is(err, ErrInvalidCurrentVaultMutation) {
		t.Fatalf("a non-default key was accepted: %v", err)
	}
}
