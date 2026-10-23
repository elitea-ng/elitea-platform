package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/centrysecrets"
)

func TestCurrentModelDefaultsReaderUsesCurrentProjectPublicAndAdminPrecedence(t *testing.T) {
	project := &fakeSecretVault{
		regular: map[string]string{
			"default_llm_model_name":           "project-model",
			"default_llm_model_project_id":     "7",
			"default_llm_low_tier_model_name":  "",
			"default_llm_high_tier_model_name": "project-high",
		},
		hidden: map[string]string{
			"default_llm_low_tier_model_name":       "project-hidden-must-not-win",
			"default_llm_low_tier_model_project_id": "7",
		},
	}
	public := &fakeSecretVault{
		regular: map[string]string{
			"default_llm_low_tier_model_name": "public-low",
		},
		hidden: map[string]string{
			"default_llm_low_tier_model_name":       "public-hidden-must-not-win",
			"default_llm_low_tier_model_project_id": "001",
		},
	}
	admin := &fakeSecretVault{regular: map[string]string{
		"default_llm_low_tier_model_name":        "admin-must-not-win",
		"default_llm_low_tier_model_project_id":  "99",
		"default_llm_high_tier_model_project_id": "1",
	}}
	vaults := &fakeSecretVaultLoader{
		projects: map[int64]SecretVault{7: project, 1: public},
		admin:    admin, projectLoads: map[int64]int{},
	}
	reader, err := NewCurrentModelDefaultsReader(vaults)
	if err != nil {
		t.Fatal(err)
	}

	defaults, err := reader.Load(context.Background(), 7, 1, configurationapp.CurrentModelSectionLLM)
	if err != nil {
		t.Fatal(err)
	}
	if defaults.Model.Project.Name != "project-model" || defaults.Model.Project.ProjectID != "7" ||
		defaults.Model.Public != (configurationapp.CurrentModelDefault{}) {
		t.Fatalf("ordinary defaults=%#v", defaults.Model)
	}
	if defaults.LowTier.Project.Name != "" || defaults.LowTier.Project.ProjectID != "" ||
		defaults.LowTier.Public.Name != "public-low" || defaults.LowTier.Public.ProjectID != "1" {
		t.Fatalf("low-tier defaults=%#v", defaults.LowTier)
	}
	if defaults.HighTier.Project.Name != "project-high" || defaults.HighTier.Project.ProjectID != "" ||
		defaults.HighTier.Public.Name != "" || defaults.HighTier.Public.ProjectID != "1" {
		t.Fatalf("high-tier defaults=%#v", defaults.HighTier)
	}
	if vaults.projectLoads[7] != 1 || vaults.projectLoads[1] != 1 || vaults.adminLoads != 1 {
		t.Fatalf("vault loads=%#v admin=%d", vaults.projectLoads, vaults.adminLoads)
	}
}

func TestCurrentModelDefaultsReaderPublicEmptyValuesOverrideHiddenAndAdmin(t *testing.T) {
	project := &fakeSecretVault{regular: map[string]string{}, hidden: map[string]string{}}
	public := &fakeSecretVault{
		regular: map[string]string{
			"default_embedding_model_name":       "",
			"default_embedding_model_project_id": "",
		},
		hidden: map[string]string{
			"default_embedding_model_name":       "hidden-model",
			"default_embedding_model_project_id": "1",
		},
	}
	admin := &fakeSecretVault{regular: map[string]string{
		"default_embedding_model_name":       "admin-model",
		"default_embedding_model_project_id": "99",
	}}
	reader := currentModelDefaultsReaderForTest(t, &fakeSecretVaultLoader{
		projects: map[int64]SecretVault{7: project, 1: public}, admin: admin,
		projectLoads: map[int64]int{},
	})

	defaults, err := reader.Load(context.Background(), 7, 1, configurationapp.CurrentModelSectionEmbedding)
	if err != nil {
		t.Fatal(err)
	}
	if defaults.Model.Public.Name != "" || defaults.Model.Public.ProjectID != "" {
		t.Fatalf("public regular empty precedence changed: %#v", defaults.Model.Public)
	}
}

func TestCurrentModelDefaultsReaderLoadsOnlyProjectWhenEveryFieldIsPresent(t *testing.T) {
	sections := []configurationapp.CurrentModelSection{
		configurationapp.CurrentModelSectionLLM,
		configurationapp.CurrentModelSectionEmbedding,
		configurationapp.CurrentModelSectionVectorStorage,
		configurationapp.CurrentModelSectionImageGeneration,
		configurationapp.CurrentModelSectionASR,
		configurationapp.CurrentModelSectionTTS,
	}
	for _, section := range sections {
		t.Run(string(section), func(t *testing.T) {
			regular := map[string]string{}
			prefixes := []string{string(section)}
			if section == configurationapp.CurrentModelSectionLLM {
				prefixes = append(prefixes, "llm_low_tier", "llm_high_tier")
			}
			for _, prefix := range prefixes {
				nameKey, projectIDKey := currentModelDefaultKeys(prefix)
				regular[nameKey] = prefix + "-model"
				regular[projectIDKey] = "7"
			}
			vaults := &fakeSecretVaultLoader{
				projects:     map[int64]SecretVault{7: &fakeSecretVault{regular: regular, hidden: map[string]string{}}},
				projectLoads: map[int64]int{},
			}
			reader := currentModelDefaultsReaderForTest(t, vaults)
			defaults, err := reader.Load(context.Background(), 7, 1, section)
			if err != nil {
				t.Fatal(err)
			}
			if defaults.Model.Project.Name != string(section)+"-model" || defaults.Model.Project.ProjectID != "7" {
				t.Fatalf("section defaults=%#v", defaults.Model)
			}
			if vaults.projectLoads[7] != 1 || vaults.projectLoads[1] != 0 || vaults.adminLoads != 0 {
				t.Fatalf("unnecessary fallback loads=%#v admin=%d", vaults.projectLoads, vaults.adminLoads)
			}
		})
	}
}

func TestCurrentModelDefaultsReaderReusesPublicProjectVaultForHiddenFallback(t *testing.T) {
	public := &fakeSecretVault{
		regular: map[string]string{},
		hidden: map[string]string{
			"default_tts_model_name":       "hidden-voice",
			"default_tts_model_project_id": "1",
		},
	}
	vaults := &fakeSecretVaultLoader{
		projects:     map[int64]SecretVault{1: public},
		admin:        &fakeSecretVault{regular: map[string]string{}, hidden: map[string]string{}},
		projectLoads: map[int64]int{},
	}
	reader := currentModelDefaultsReaderForTest(t, vaults)
	defaults, err := reader.Load(context.Background(), 1, 1, configurationapp.CurrentModelSectionTTS)
	if err != nil {
		t.Fatal(err)
	}
	if defaults.Model.Public.Name != "hidden-voice" || defaults.Model.Public.ProjectID != "1" {
		t.Fatalf("hidden public fallback=%#v", defaults.Model.Public)
	}
	if vaults.projectLoads[1] != 1 || vaults.adminLoads != 1 {
		t.Fatalf("public vault was loaded more than once: %#v admin=%d", vaults.projectLoads, vaults.adminLoads)
	}
}

type currentModelVaultLoaderStub struct {
	loadProject func(context.Context, int64) (SecretVault, error)
	loadAdmin   func(context.Context) (SecretVault, error)
}

func (s *currentModelVaultLoaderStub) LoadProjectVault(ctx context.Context, projectID int64) (SecretVault, error) {
	return s.loadProject(ctx, projectID)
}

func (s *currentModelVaultLoaderStub) LoadAdminVault(ctx context.Context) (SecretVault, error) {
	return s.loadAdmin(ctx)
}

type failingCurrentModelVault struct{ err error }

func (v failingCurrentModelVault) Lookup(string) (centrysecrets.Secret, error) {
	return centrysecrets.Secret{}, v.err
}
func (v failingCurrentModelVault) LookupRegular(string) (centrysecrets.Secret, error) {
	return centrysecrets.Secret{}, v.err
}
func (v failingCurrentModelVault) LookupProjectID(string) (centrysecrets.Secret, error) {
	return centrysecrets.Secret{}, v.err
}
func (v failingCurrentModelVault) LookupRegularProjectID(string) (centrysecrets.Secret, error) {
	return centrysecrets.Secret{}, v.err
}
func (v failingCurrentModelVault) LookupRegularInteger(string) (centrysecrets.Secret, error) {
	return centrysecrets.Secret{}, v.err
}

func TestCurrentModelDefaultsReaderSanitizesOperationalFailures(t *testing.T) {
	canary := errors.New("vault-database-password-canary")
	tests := []struct {
		name   string
		loader SecretVaultLoader
	}{
		{
			name: "load failure",
			loader: &currentModelVaultLoaderStub{
				loadProject: func(context.Context, int64) (SecretVault, error) { return nil, canary },
				loadAdmin:   func(context.Context) (SecretVault, error) { return nil, canary },
			},
		},
		{
			name: "lookup failure",
			loader: &currentModelVaultLoaderStub{
				loadProject: func(context.Context, int64) (SecretVault, error) {
					return failingCurrentModelVault{err: canary}, nil
				},
				loadAdmin: func(context.Context) (SecretVault, error) { return nil, canary },
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := currentModelDefaultsReaderForTest(t, test.loader)
			_, err := reader.Load(context.Background(), 7, 1, configurationapp.CurrentModelSectionLLM)
			if !errors.Is(err, ErrCurrentModelDefaultsUnavailable) || strings.Contains(err.Error(), "canary") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestCurrentModelDefaultsReaderValidatesAndPreservesCancellation(t *testing.T) {
	loads := 0
	loader := &currentModelVaultLoaderStub{
		loadProject: func(ctx context.Context, _ int64) (SecretVault, error) {
			loads++
			return nil, ctx.Err()
		},
		loadAdmin: func(ctx context.Context) (SecretVault, error) { return nil, ctx.Err() },
	}
	reader := currentModelDefaultsReaderForTest(t, loader)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := reader.Load(canceled, 7, 1, configurationapp.CurrentModelSectionLLM)
	if !errors.Is(err, context.Canceled) || loads != 0 {
		t.Fatalf("pre-canceled error=%v loads=%d", err, loads)
	}

	invalid := []struct {
		ctx       context.Context
		projectID int32
		publicID  int32
		section   configurationapp.CurrentModelSection
	}{
		{ctx: nil, projectID: 7, publicID: 1, section: configurationapp.CurrentModelSectionLLM},
		{ctx: context.Background(), projectID: 0, publicID: 1, section: configurationapp.CurrentModelSectionLLM},
		{ctx: context.Background(), projectID: 7, publicID: 0, section: configurationapp.CurrentModelSectionLLM},
		{ctx: context.Background(), projectID: 7, publicID: 1, section: configurationapp.CurrentModelSection("credentials")},
	}
	for _, request := range invalid {
		_, err := reader.Load(request.ctx, request.projectID, request.publicID, request.section)
		if !errors.Is(err, configurationapp.ErrInvalidCurrentModelCatalogRequest) {
			t.Fatalf("invalid request error=%v", err)
		}
	}
	if loads != 0 {
		t.Fatalf("invalid requests loaded vaults=%d", loads)
	}

	deadlineLoader := &currentModelVaultLoaderStub{
		loadProject: func(context.Context, int64) (SecretVault, error) { return nil, context.DeadlineExceeded },
		loadAdmin:   func(context.Context) (SecretVault, error) { return nil, context.DeadlineExceeded },
	}
	deadlineReader := currentModelDefaultsReaderForTest(t, deadlineLoader)
	_, err = deadlineReader.Load(context.Background(), 7, 1, configurationapp.CurrentModelSectionLLM)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline identity=%v", err)
	}

	if _, err := NewCurrentModelDefaultsReader(nil); err == nil {
		t.Fatal("missing vault loader was accepted")
	}
}

func currentModelDefaultsReaderForTest(t *testing.T, vaults SecretVaultLoader) *CurrentModelDefaultsReader {
	t.Helper()
	reader, err := NewCurrentModelDefaultsReader(vaults)
	if err != nil {
		t.Fatal(err)
	}
	return reader
}

// TestCurrentModelDefaultsReaderReadsAnAbsentVaultAsNoDefaults pins the answer
// a FRESH deployment gets.
//
// A project that has stored no secret has no vault row, and a deployment where
// nobody has written an admin secret has no admin vault. Both loads then
// answer ErrVaultAbsent. That used to fail the read, and because the model
// catalogue asks for these defaults on every request, GET
// /api/v2/configurations/models/{projectID} answered 500 for every section on
// a deployment whose model rows were all present — the model picker was empty
// and no cause reached any log.
func TestCurrentModelDefaultsReaderReadsAnAbsentVaultAsNoDefaults(t *testing.T) {
	for _, test := range []struct {
		name   string
		loader SecretVaultLoader
	}{
		{
			name: "no vault anywhere",
			loader: &currentModelVaultLoaderStub{
				loadProject: func(context.Context, int64) (SecretVault, error) { return nil, ErrVaultAbsent },
				loadAdmin:   func(context.Context) (SecretVault, error) { return nil, ErrVaultAbsent },
			},
		},
		{
			name: "project vault only",
			loader: &currentModelVaultLoaderStub{
				loadProject: func(context.Context, int64) (SecretVault, error) {
					return &fakeSecretVault{regular: map[string]string{}, hidden: map[string]string{}}, nil
				},
				loadAdmin: func(context.Context) (SecretVault, error) { return nil, ErrVaultAbsent },
			},
		},
		{
			name: "admin vault only",
			loader: &currentModelVaultLoaderStub{
				loadProject: func(context.Context, int64) (SecretVault, error) { return nil, ErrVaultAbsent },
				loadAdmin: func(context.Context) (SecretVault, error) {
					return &fakeSecretVault{regular: map[string]string{}, hidden: map[string]string{}}, nil
				},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := currentModelDefaultsReaderForTest(t, test.loader)
			defaults, err := reader.Load(context.Background(), 7, 1, configurationapp.CurrentModelSectionLLM)
			if err != nil {
				t.Fatalf("an absent vault must read as no defaults, got %v", err)
			}
			if defaults != (configurationapp.CurrentModelCatalogDefaults{}) {
				t.Fatalf("defaults=%#v", defaults)
			}
		})
	}
}

// A vault that EXISTS and will not open keeps failing the read. Its values are
// there and unread, so "no default has been chosen" would be a claim this
// process cannot make.
//
// The failure is built the way the loader really builds it — wrapping
// ErrContentUnavailable — because ErrVaultAbsent wraps that same sentinel, and
// the two are told apart by which one errors.Is matches. An error that wraps
// nothing passes this test no matter how the check is written, so it would not
// notice the plausible simplification of broadening the absent branch to
// errors.Is(err, ErrContentUnavailable). That broadening swallows every
// undecryptable vault as "no defaults", which is the silent version of the
// outage this file exists to prevent.
//
// Both loads are covered: the project load is checked inline, and the admin
// load goes through loadFallbackVault, which is where the broadening would be
// written.
func TestCurrentModelDefaultsReaderStillFailsOnAnUnreadableVault(t *testing.T) {
	unreadable := fmt.Errorf("open encrypted secret vault: %w", ErrContentUnavailable)
	if !errors.Is(unreadable, ErrContentUnavailable) || errors.Is(unreadable, ErrVaultAbsent) {
		t.Fatal("the fixture must be an unreadable vault, not an absent one")
	}
	empty := func() SecretVault {
		return &fakeSecretVault{regular: map[string]string{}, hidden: map[string]string{}}
	}
	for _, test := range []struct {
		name   string
		loader SecretVaultLoader
	}{
		{
			name: "project vault will not open",
			loader: &currentModelVaultLoaderStub{
				loadProject: func(context.Context, int64) (SecretVault, error) { return nil, unreadable },
				loadAdmin:   func(context.Context) (SecretVault, error) { return nil, ErrVaultAbsent },
			},
		},
		{
			name: "admin vault will not open",
			loader: &currentModelVaultLoaderStub{
				loadProject: func(context.Context, int64) (SecretVault, error) { return empty(), nil },
				loadAdmin:   func(context.Context) (SecretVault, error) { return nil, unreadable },
			},
		},
		{
			name: "public project vault will not open",
			loader: &currentModelVaultLoaderStub{
				loadProject: func(_ context.Context, projectID int64) (SecretVault, error) {
					if projectID == 1 {
						return nil, unreadable
					}
					return empty(), nil
				},
				loadAdmin: func(context.Context) (SecretVault, error) { return empty(), nil },
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := currentModelDefaultsReaderForTest(t, test.loader)
			if _, err := reader.Load(
				context.Background(), 7, 1, configurationapp.CurrentModelSectionLLM,
			); !errors.Is(err, ErrCurrentModelDefaultsUnavailable) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}
