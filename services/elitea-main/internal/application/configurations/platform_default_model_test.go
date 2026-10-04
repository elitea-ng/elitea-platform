package configurations

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"testing"
)

// ── Resolution precedence (#6826) ────────────────────────────────────────────

func platformDefaultCatalogItems() []CurrentModelCatalogItem {
	return []CurrentModelCatalogItem{
		{Name: "own-model", ProjectID: 7},
		{Name: "platform-model", ProjectID: 1, Shared: true},
		{Name: "other-platform", ProjectID: 1, Shared: true},
	}
}

func TestDefaultPrecedenceProjectDefaultWinsWhenItResolves(t *testing.T) {
	response := BuildCurrentModelCatalog(CurrentModelCatalogRequest{
		Section: CurrentModelSectionLLM, ProjectID: 7, PublicProjectID: 1,
		ProjectItems: platformDefaultCatalogItems(),
		Defaults: CurrentModelCatalogDefaults{
			Model:    CurrentModelDefaultSources{Project: CurrentModelDefault{Name: "own-model", ProjectID: "7"}},
			Platform: CurrentModelDefault{Name: "platform-model", ProjectID: "1"},
		},
	})
	assertCatalogDefault(t, response, "own-model", 7)
}

func TestDefaultPrecedenceDanglingProjectDefaultFallsBackToPlatform(t *testing.T) {
	response := BuildCurrentModelCatalog(CurrentModelCatalogRequest{
		Section: CurrentModelSectionLLM, ProjectID: 7, PublicProjectID: 1,
		ProjectItems: platformDefaultCatalogItems(),
		Defaults: CurrentModelCatalogDefaults{
			Model:    CurrentModelDefaultSources{Project: CurrentModelDefault{Name: "deleted", ProjectID: "7"}},
			Platform: CurrentModelDefault{Name: "platform-model", ProjectID: "1"},
		},
	})
	assertCatalogDefault(t, response, "platform-model", 1)
	for _, item := range response.Items {
		if item.Default != (item.Name == "platform-model") {
			t.Fatalf("item %q default flag = %v", item.Name, item.Default)
		}
	}
}

func TestDefaultPrecedencePlatformDefaultOutsideTheCatalogueFallsBackToFirst(t *testing.T) {
	// A shared model the project holds no grant for is not in its catalogue.
	// The precedence skips it and keeps the legacy first-model answer.
	response := BuildCurrentModelCatalog(CurrentModelCatalogRequest{
		Section: CurrentModelSectionLLM, ProjectID: 7, PublicProjectID: 1,
		ProjectItems: []CurrentModelCatalogItem{{Name: "own-model", ProjectID: 7}},
		Defaults: CurrentModelCatalogDefaults{
			Platform: CurrentModelDefault{Name: "withheld", ProjectID: "1"},
		},
	})
	assertCatalogDefault(t, response, "own-model", 7)
}

func TestDefaultPrecedenceEmptyCatalogueHasNoDefault(t *testing.T) {
	response := BuildCurrentModelCatalog(CurrentModelCatalogRequest{
		Section: CurrentModelSectionLLM, ProjectID: 7, PublicProjectID: 1,
		Defaults: CurrentModelCatalogDefaults{
			Model:    CurrentModelDefaultSources{Project: CurrentModelDefault{Name: "deleted", ProjectID: "7"}},
			Platform: CurrentModelDefault{Name: "platform-model", ProjectID: "1"},
		},
	})
	if response.DefaultModelName != nil || response.DefaultModelProjectID != nil {
		t.Fatalf("an empty catalogue reported a default: %v/%v", response.DefaultModelName, response.DefaultModelProjectID)
	}
}

func assertCatalogDefault(t *testing.T, response CurrentModelCatalogResponse, name string, projectID int32) {
	t.Helper()
	if response.DefaultModelName == nil || *response.DefaultModelName != name ||
		response.DefaultModelProjectID == nil || *response.DefaultModelProjectID != projectID {
		t.Fatalf("default = %v/%v, want %s/%d", response.DefaultModelName, response.DefaultModelProjectID, name, projectID)
	}
}

type platformAwareDefaultsLoader struct {
	currentModelDefaultsLoaderStub
	platform      CurrentModelDefault
	platformErr   error
	platformCalls int
}

func (l *platformAwareDefaultsLoader) LoadPlatformModelDefault(
	context.Context, int32, CurrentModelSection,
) (CurrentModelDefault, error) {
	l.platformCalls++
	return l.platform, l.platformErr
}

func TestCatalogServiceReadsThePlatformDefaultOnlyForADanglingProjectDefault(t *testing.T) {
	repository := &currentModelCandidateRepositoryStub{list: func(
		_ context.Context, projectID int32, _ CurrentModelSection, sharedOnly bool,
	) ([]CurrentModelCatalogItem, error) {
		if sharedOnly {
			return []CurrentModelCatalogItem{{Name: "platform-model", ProjectID: projectID, Shared: true}}, nil
		}
		return []CurrentModelCatalogItem{{Name: "own-model", ProjectID: projectID}}, nil
	}}

	for _, tc := range []struct {
		name          string
		own           CurrentModelDefault
		wantName      string
		wantPlatforms int
	}{
		{name: "own default resolves", own: CurrentModelDefault{Name: "own-model", ProjectID: "7"}, wantName: "own-model"},
		{name: "no own default", wantName: "own-model"},
		{name: "own default dangling", own: CurrentModelDefault{Name: "deleted", ProjectID: "7"},
			wantName: "platform-model", wantPlatforms: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loader := &platformAwareDefaultsLoader{platform: CurrentModelDefault{Name: "platform-model", ProjectID: "1"}}
			loader.load = func(context.Context, int32, int32, CurrentModelSection) (CurrentModelCatalogDefaults, error) {
				return CurrentModelCatalogDefaults{Model: CurrentModelDefaultSources{Project: tc.own}}, nil
			}
			service, err := NewCurrentModelCatalogService(repository, loader)
			if err != nil {
				t.Fatal(err)
			}
			response, err := service.Get(context.Background(), CurrentModelCatalogQuery{
				Section: CurrentModelSectionLLM, ProjectID: 7, PublicProjectID: 1, IncludeShared: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if response.DefaultModelName == nil || *response.DefaultModelName != tc.wantName {
				t.Fatalf("default = %v, want %s", response.DefaultModelName, tc.wantName)
			}
			if loader.platformCalls != tc.wantPlatforms {
				t.Fatalf("platform reads = %d, want %d", loader.platformCalls, tc.wantPlatforms)
			}
		})
	}
}

func TestCatalogServiceReportsAPlatformDefaultReadFailure(t *testing.T) {
	repository := &currentModelCandidateRepositoryStub{list: func(
		context.Context, int32, CurrentModelSection, bool,
	) ([]CurrentModelCatalogItem, error) {
		return []CurrentModelCatalogItem{{Name: "own-model", ProjectID: 7}}, nil
	}}
	loader := &platformAwareDefaultsLoader{platformErr: errors.New("vault down")}
	loader.load = func(context.Context, int32, int32, CurrentModelSection) (CurrentModelCatalogDefaults, error) {
		return CurrentModelCatalogDefaults{Model: CurrentModelDefaultSources{
			Project: CurrentModelDefault{Name: "deleted", ProjectID: "7"},
		}}, nil
	}
	service, err := NewCurrentModelCatalogService(repository, loader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Get(context.Background(), CurrentModelCatalogQuery{
		Section: CurrentModelSectionLLM, ProjectID: 7, PublicProjectID: 1,
	}); err == nil {
		t.Fatal("a failed platform default read was reported as a catalogue")
	}
}

// ── PlatformModelDefaultService ─────────────────────────────────────────────

type fakeDefaultStore struct {
	mu       sync.Mutex
	platform CurrentModelDefault
	projects map[int32]CurrentModelDefault
	err      error
}

func (s *fakeDefaultStore) LoadPlatformModelDefault(context.Context, int32, CurrentModelSection) (CurrentModelDefault, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.platform, s.err
}

func (s *fakeDefaultStore) LoadProjectModelDefault(_ context.Context, projectID int32, _ CurrentModelSection) (CurrentModelDefault, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.projects[projectID], s.err
}

type fakeDefaultWriter struct {
	mu      sync.Mutex
	sets    []CurrentModelDefaultSelection
	clears  []CurrentModelDefaultClear
	setErr  error
	clearOK bool
}

func (w *fakeDefaultWriter) SetCurrentModelDefault(_ context.Context, selection CurrentModelDefaultSelection) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sets = append(w.sets, selection)
	return w.setErr
}

func (w *fakeDefaultWriter) ClearCurrentModelDefault(_ context.Context, request CurrentModelDefaultClear) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.clears = append(w.clears, request)
	return w.clearOK, nil
}

type fakeProjectLister struct{ ids []int32 }

func (l fakeProjectLister) ListActiveCurrentProjectIDs(context.Context, int) ([]int32, error) {
	return l.ids, nil
}

type fakeVaultCreator struct{ ensured []string }

func (c *fakeVaultCreator) EnsureProjectVault(_ context.Context, projectID string) error {
	c.ensured = append(c.ensured, projectID)
	return nil
}

func platformCandidates() *currentModelCandidateRepositoryStub {
	return &currentModelCandidateRepositoryStub{list: func(
		_ context.Context, projectID int32, _ CurrentModelSection, sharedOnly bool,
	) ([]CurrentModelCatalogItem, error) {
		if projectID != 1 || !sharedOnly {
			return nil, errors.New("the candidates must be the public project's shared rows")
		}
		label := "Zeta"
		return []CurrentModelCatalogItem{
			{Name: "gpt-all", ProjectID: 1, Shared: true, Grant: ModelGrant{Scope: ModelShareScopeAll}},
			{Name: "gpt-narrow", ProjectID: 1, Shared: true, Grant: ModelGrant{Scope: ModelShareScopeProjects, Projects: []int32{7}}},
			{Name: "gpt-none", ProjectID: 1, Shared: true, Grant: ModelGrant{Scope: ModelShareScopeNone}},
			{Name: "alpha", DisplayName: &label, ProjectID: 1, Shared: true, Grant: ModelGrant{Scope: ModelShareScopeAll}},
		}, nil
	}}
}

func newTestPlatformDefaultService(
	t *testing.T, store *fakeDefaultStore, writer *fakeDefaultWriter, projects []int32, vaults *fakeVaultCreator,
) *PlatformModelDefaultService {
	t.Helper()
	var creator PlatformModelDefaultVaultCreator
	if vaults != nil {
		creator = vaults
	}
	service, err := NewPlatformModelDefaultService(platformCandidates(), store, writer, fakeProjectLister{ids: projects}, creator, 1)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestPlatformDefaultGetListsOnlyModelsOfferedToEveryProject(t *testing.T) {
	store := &fakeDefaultStore{platform: CurrentModelDefault{Name: "gpt-all", ProjectID: "1"}}
	service := newTestPlatformDefaultService(t, store, &fakeDefaultWriter{}, nil, nil)

	view, err := service.Get(context.Background(), CurrentModelSectionLLM)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(view.Candidates))
	for _, candidate := range view.Candidates {
		names = append(names, candidate.Name)
	}
	if !reflect.DeepEqual(names, []string{"gpt-all", "alpha"}) {
		t.Fatalf("candidates = %v, want the two `all` models sorted by display name", names)
	}
	if view.ModelName != "gpt-all" || view.ModelProjectID == nil || *view.ModelProjectID != 1 || !view.Available {
		t.Fatalf("view = %+v", view)
	}
}

func TestPlatformDefaultGetReportsAnUnavailableStoredModel(t *testing.T) {
	for _, stored := range []CurrentModelDefault{
		{Name: "deleted-model", ProjectID: "1"},
		{Name: "gpt-narrow", ProjectID: "1"},
		{Name: "gpt-all", ProjectID: "7"},
	} {
		store := &fakeDefaultStore{platform: stored}
		service := newTestPlatformDefaultService(t, store, &fakeDefaultWriter{}, nil, nil)
		view, err := service.Get(context.Background(), CurrentModelSectionLLM)
		if err != nil {
			t.Fatal(err)
		}
		if view.Available {
			t.Fatalf("stored %+v reported as available; the console would not ask for a replacement", stored)
		}
	}
	// No stored value is not "unavailable": there is nothing to replace.
	service := newTestPlatformDefaultService(t, &fakeDefaultStore{}, &fakeDefaultWriter{}, nil, nil)
	view, err := service.Get(context.Background(), CurrentModelSectionLLM)
	if err != nil || !view.Available || view.ModelName != "" || view.ModelProjectID != nil {
		t.Fatalf("empty view = %+v err=%v", view, err)
	}
}

func TestPlatformDefaultSetWritesThePublicVault(t *testing.T) {
	writer := &fakeDefaultWriter{}
	vaults := &fakeVaultCreator{}
	service := newTestPlatformDefaultService(t, &fakeDefaultStore{}, writer, nil, vaults)

	if err := service.Set(context.Background(), CurrentModelSectionLLM, "gpt-all"); err != nil {
		t.Fatal(err)
	}
	want := []CurrentModelDefaultSelection{{ProjectID: 1, Name: "gpt-all", TargetProjectID: 1, Section: "llm"}}
	if !reflect.DeepEqual(writer.sets, want) {
		t.Fatalf("writes = %+v, want %+v", writer.sets, want)
	}
	if !reflect.DeepEqual(vaults.ensured, []string{"1"}) {
		t.Fatalf("the public vault was not ensured before the write: %v", vaults.ensured)
	}
}

func TestPlatformDefaultSetRefusesAModelNotOfferedToEveryProject(t *testing.T) {
	for _, name := range []string{"gpt-narrow", "gpt-none", "missing", ""} {
		writer := &fakeDefaultWriter{}
		service := newTestPlatformDefaultService(t, &fakeDefaultStore{}, writer, nil, nil)
		err := service.Set(context.Background(), CurrentModelSectionLLM, name)
		if err == nil {
			t.Fatalf("Set(%q) succeeded", name)
		}
		if name != "" && !errors.Is(err, ErrPlatformModelDefaultIneligible) {
			t.Fatalf("Set(%q) err = %v, want ErrPlatformModelDefaultIneligible", name, err)
		}
		if len(writer.sets) != 0 {
			t.Fatalf("Set(%q) wrote %+v", name, writer.sets)
		}
	}
}

func TestPlatformDefaultClearRemovesThePublicAndTheAdminValue(t *testing.T) {
	writer := &fakeDefaultWriter{}
	service := newTestPlatformDefaultService(t, &fakeDefaultStore{}, writer, nil, nil)
	if err := service.Clear(context.Background(), CurrentModelSectionLLM); err != nil {
		t.Fatal(err)
	}
	want := []CurrentModelDefaultClear{
		{ProjectID: 1, Section: "llm"},
		{Admin: true, Section: "llm"},
	}
	if !reflect.DeepEqual(writer.clears, want) {
		t.Fatalf("clears = %+v, want %+v", writer.clears, want)
	}
}

func TestPlatformDefaultSeedCopiesAnAvailableDefaultIntoTheNewProject(t *testing.T) {
	writer := &fakeDefaultWriter{}
	store := &fakeDefaultStore{platform: CurrentModelDefault{Name: "gpt-all", ProjectID: "1"}}
	service := newTestPlatformDefaultService(t, store, writer, nil, nil)

	if err := service.SeedProjectModelDefault(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	want := []CurrentModelDefaultSelection{{ProjectID: 42, Name: "gpt-all", TargetProjectID: 1, Section: "llm"}}
	if !reflect.DeepEqual(writer.sets, want) {
		t.Fatalf("seed writes = %+v, want %+v", writer.sets, want)
	}
}

func TestPlatformDefaultSeedWritesNothingWithoutAnAvailableDefault(t *testing.T) {
	for _, stored := range []CurrentModelDefault{{}, {Name: "deleted-model", ProjectID: "1"}, {Name: "gpt-narrow", ProjectID: "1"}} {
		writer := &fakeDefaultWriter{}
		service := newTestPlatformDefaultService(t, &fakeDefaultStore{platform: stored}, writer, nil, nil)
		if err := service.SeedProjectModelDefault(context.Background(), 42); err != nil {
			t.Fatal(err)
		}
		if len(writer.sets) != 0 {
			t.Fatalf("stored %+v: seed wrote %+v", stored, writer.sets)
		}
	}
	// The public project's own default IS the platform default.
	writer := &fakeDefaultWriter{}
	store := &fakeDefaultStore{platform: CurrentModelDefault{Name: "gpt-all", ProjectID: "1"}}
	service := newTestPlatformDefaultService(t, store, writer, nil, nil)
	if err := service.SeedProjectModelDefault(context.Background(), 1); err != nil || len(writer.sets) != 0 {
		t.Fatalf("seed of the public project wrote %+v err=%v", writer.sets, err)
	}
}

func TestPlatformDefaultUsageCountsProjectsAndThePlatform(t *testing.T) {
	store := &fakeDefaultStore{
		platform: CurrentModelDefault{Name: "gpt-all", ProjectID: "1"},
		projects: map[int32]CurrentModelDefault{
			1: {Name: "gpt-all", ProjectID: "1"},
			2: {Name: "gpt-all", ProjectID: "1"},
			3: {Name: "gpt-all", ProjectID: "3"},
			4: {Name: "other", ProjectID: "1"},
			5: {Name: "gpt-all", ProjectID: "1"},
		},
	}
	service := newTestPlatformDefaultService(t, store, &fakeDefaultWriter{}, []int32{1, 2, 3, 4, 5, 6}, nil)

	usage, err := service.Usage(context.Background(), 1, CurrentModelSectionLLM, "gpt-all")
	if err != nil {
		t.Fatal(err)
	}
	want := PlatformModelDefaultUsage{ModelName: "gpt-all", PlatformDefault: true, Projects: 2}
	if usage != want {
		t.Fatalf("usage = %+v, want %+v (the public project is the platform default, not a project)", usage, want)
	}

	usage, err = service.Usage(context.Background(), 3, CurrentModelSectionLLM, "gpt-all")
	if err != nil {
		t.Fatal(err)
	}
	if usage != (PlatformModelDefaultUsage{ModelName: "gpt-all", Projects: 1}) {
		t.Fatalf("project-owned usage = %+v", usage)
	}
}

func TestPlatformDefaultReleaseClearsMatchingDefaultsEverywhereForAPlatformModel(t *testing.T) {
	writer := &fakeDefaultWriter{}
	service := newTestPlatformDefaultService(t, &fakeDefaultStore{}, writer, []int32{1, 2, 3}, nil)

	if err := service.ReleaseDeletedModelDefault(context.Background(), 1, CurrentModelSectionLLM, "gpt-all"); err != nil {
		t.Fatal(err)
	}
	match := CurrentModelDefault{Name: "gpt-all", ProjectID: "1"}
	var projects []int32
	admin := 0
	for _, clear := range writer.clears {
		if clear.Match == nil || *clear.Match != match {
			t.Fatalf("an unconditional clear on release: %+v", clear)
		}
		if clear.Admin {
			admin++
			continue
		}
		projects = append(projects, clear.ProjectID)
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i] < projects[j] })
	if !reflect.DeepEqual(projects, []int32{1, 2, 3}) || admin != 1 {
		t.Fatalf("released projects = %v admin=%d", projects, admin)
	}
}

func TestPlatformDefaultReleaseOfAProjectModelTouchesOnlyThatProject(t *testing.T) {
	writer := &fakeDefaultWriter{}
	service := newTestPlatformDefaultService(t, &fakeDefaultStore{}, writer, []int32{1, 2, 3}, nil)

	if err := service.ReleaseDeletedModelDefault(context.Background(), 3, CurrentModelSectionLLM, "mine"); err != nil {
		t.Fatal(err)
	}
	want := []CurrentModelDefaultClear{{ProjectID: 3, Section: "llm", Match: &CurrentModelDefault{Name: "mine", ProjectID: strconv.Itoa(3)}}}
	if !reflect.DeepEqual(writer.clears, want) {
		t.Fatalf("clears = %+v, want %+v", writer.clears, want)
	}
}

func TestPlatformDefaultServiceRefusesIncompleteComposition(t *testing.T) {
	if _, err := NewPlatformModelDefaultService(nil, &fakeDefaultStore{}, &fakeDefaultWriter{}, fakeProjectLister{}, nil, 1); err == nil {
		t.Fatal("a service with no candidates was composed")
	}
	if _, err := NewPlatformModelDefaultService(platformCandidates(), &fakeDefaultStore{}, &fakeDefaultWriter{}, fakeProjectLister{}, nil, 0); err == nil {
		t.Fatal("a service with no public project was composed")
	}
}
