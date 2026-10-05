package storage

import (
	"context"
	"errors"
	"testing"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
)

// absentAwareVaultLoader answers ErrVaultAbsent for a scope with no vault, the
// state of a fresh install.
type absentAwareVaultLoader struct {
	projects map[int64]SecretVault
	admin    SecretVault
	failWith error
}

func (l absentAwareVaultLoader) LoadProjectVault(_ context.Context, projectID int64) (SecretVault, error) {
	if l.failWith != nil {
		return nil, l.failWith
	}
	if vault, ok := l.projects[projectID]; ok {
		return vault, nil
	}
	return nil, ErrVaultAbsent
}

func (l absentAwareVaultLoader) LoadAdminVault(context.Context) (SecretVault, error) {
	if l.admin == nil {
		return nil, ErrVaultAbsent
	}
	return l.admin, nil
}

func TestLoadPlatformModelDefaultReadsThePublicVaultThenTheAdminVault(t *testing.T) {
	public := &fakeSecretVault{regular: map[string]string{"default_llm_model_name": "public-model"}}
	admin := &fakeSecretVault{regular: map[string]string{
		"default_llm_model_name":       "admin-model",
		"default_llm_model_project_id": "1",
	}}
	reader, err := NewCurrentModelDefaultsReader(absentAwareVaultLoader{
		projects: map[int64]SecretVault{1: public}, admin: admin,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.LoadPlatformModelDefault(context.Background(), 1, configurationapp.CurrentModelSectionLLM)
	if err != nil {
		t.Fatal(err)
	}
	want := configurationapp.CurrentModelDefault{Name: "public-model", ProjectID: "1"}
	if got != want {
		t.Fatalf("platform default = %+v, want %+v (per-field public then admin)", got, want)
	}
}

func TestLoadPlatformModelDefaultReadsAbsentVaultsAsNotSet(t *testing.T) {
	reader, err := NewCurrentModelDefaultsReader(absentAwareVaultLoader{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.LoadPlatformModelDefault(context.Background(), 1, configurationapp.CurrentModelSectionLLM)
	if err != nil || got != (configurationapp.CurrentModelDefault{}) {
		t.Fatalf("fresh install = %+v err=%v, want an empty default", got, err)
	}
}

func TestLoadProjectModelDefaultHasNoFallback(t *testing.T) {
	project := &fakeSecretVault{regular: map[string]string{
		"default_llm_model_name":                "own",
		"default_llm_model_project_id":          "7",
		"default_llm_low_tier_model_name":       "cheap",
		"default_llm_low_tier_model_project_id": "1",
	}}
	public := &fakeSecretVault{regular: map[string]string{
		"default_llm_model_name":       "public-model",
		"default_llm_model_project_id": "1",
	}}
	reader, err := NewCurrentModelDefaultsReader(absentAwareVaultLoader{
		projects: map[int64]SecretVault{7: project, 1: public, 8: &fakeSecretVault{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.LoadProjectModelDefault(context.Background(), 7, "llm")
	if err != nil || got != (configurationapp.CurrentModelDefault{Name: "own", ProjectID: "7"}) {
		t.Fatalf("project 7 = %+v err=%v", got, err)
	}
	// The LLM tier defaults are read by their own key prefix.
	got, err = reader.LoadProjectModelDefault(context.Background(), 7, "llm_low_tier")
	if err != nil || got != (configurationapp.CurrentModelDefault{Name: "cheap", ProjectID: "1"}) {
		t.Fatalf("project 7 low tier = %+v err=%v", got, err)
	}
	if _, err = reader.LoadProjectModelDefault(context.Background(), 7, "secret_of_another_kind"); err == nil {
		t.Fatal("a key that is not a default prefix was read")
	}
	for _, projectID := range []int32{8, 9} {
		got, err = reader.LoadProjectModelDefault(context.Background(), projectID, "llm")
		if err != nil || got != (configurationapp.CurrentModelDefault{}) {
			t.Fatalf("project %d = %+v err=%v; a project read must not fall back to the public value", projectID, got, err)
		}
	}
}

func TestPlatformModelDefaultReadsMaskAVaultFailure(t *testing.T) {
	reader, err := NewCurrentModelDefaultsReader(absentAwareVaultLoader{failWith: errors.New("bad key material")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.LoadProjectModelDefault(context.Background(), 7, "llm"); !errors.Is(err, ErrCurrentModelDefaultsUnavailable) {
		t.Fatalf("project read err = %v, want ErrCurrentModelDefaultsUnavailable", err)
	}
	if _, err := reader.LoadPlatformModelDefault(context.Background(), 1, configurationapp.CurrentModelSectionLLM); !errors.Is(err, ErrCurrentModelDefaultsUnavailable) {
		t.Fatalf("platform read err = %v, want ErrCurrentModelDefaultsUnavailable", err)
	}
	if _, err := reader.LoadPlatformModelDefault(context.Background(), 0, configurationapp.CurrentModelSectionLLM); err == nil {
		t.Fatal("an invalid public project id was accepted")
	}
}
