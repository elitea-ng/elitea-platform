package configurations

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"
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
	// projects holds each project's own default of the section key.
	projects map[int32]CurrentModelDefault
	// keyed holds the defaults of the other keys, by "<project>/<key>".
	keyed        map[string]CurrentModelDefault
	err          error
	projectLoads int
}

func (s *fakeDefaultStore) LoadPlatformModelDefault(context.Context, int32, CurrentModelSection) (CurrentModelDefault, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.platform, s.err
}

func (s *fakeDefaultStore) LoadProjectModelDefault(_ context.Context, projectID int32, key string) (CurrentModelDefault, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.projectLoads++
	if IsSupportedCurrentModelSection(CurrentModelSection(key)) {
		return s.projects[projectID], s.err
	}
	return s.keyed[strconv.Itoa(int(projectID))+"/"+key], s.err
}

type fakeDefaultWriter struct {
	mu      sync.Mutex
	sets    []CurrentModelDefaultSelection
	clears  []CurrentModelDefaultClear
	setErr  error
	clearOK bool
	// fail makes a clear of these projects fail; -1 is the admin vault.
	fail map[int32]bool
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
	target := request.ProjectID
	if request.Admin {
		target = -1
	}
	if w.fail[target] {
		return false, errors.New("the vault will not open")
	}
	return w.clearOK, nil
}

func (w *fakeDefaultWriter) clearedProjects() []int32 {
	w.mu.Lock()
	defer w.mu.Unlock()
	seen := map[int32]bool{}
	var projects []int32
	for _, clear := range w.clears {
		if clear.Admin || seen[clear.ProjectID] {
			continue
		}
		seen[clear.ProjectID] = true
		projects = append(projects, clear.ProjectID)
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i] < projects[j] })
	return projects
}

type fakeProjectLister struct {
	ids   []int32
	err   error
	pages int
}

func (l *fakeProjectLister) ListActiveProjectIDsAfter(_ context.Context, after int32, limit int) ([]int32, error) {
	l.pages++
	if l.err != nil {
		return nil, l.err
	}
	sorted := append([]int32(nil), l.ids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var page []int32
	for _, id := range sorted {
		if id > after && len(page) < limit {
			page = append(page, id)
		}
	}
	return page, nil
}

type fakeModelRows struct {
	mu      sync.Mutex
	exists  bool
	queries []PlatformModelDefaultRowQuery
}

func (r *fakeModelRows) ModelRowExists(_ context.Context, query PlatformModelDefaultRowQuery) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queries = append(r.queries, query)
	return r.exists, nil
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

type platformDefaultFixture struct {
	store   *fakeDefaultStore
	writer  *fakeDefaultWriter
	lister  *fakeProjectLister
	rows    *fakeModelRows
	service *PlatformModelDefaultService
}

func newPlatformDefaultFixture(
	t *testing.T, store *fakeDefaultStore, writer *fakeDefaultWriter, projects []int32, vaults *fakeVaultCreator,
) platformDefaultFixture {
	t.Helper()
	var creator PlatformModelDefaultVaultCreator
	if vaults != nil {
		creator = vaults
	}
	lister := &fakeProjectLister{ids: projects}
	rows := &fakeModelRows{}
	service, err := NewPlatformModelDefaultService(platformCandidates(), store, writer, lister, rows, creator, 1)
	if err != nil {
		t.Fatal(err)
	}
	return platformDefaultFixture{store: store, writer: writer, lister: lister, rows: rows, service: service}
}

func newTestPlatformDefaultService(
	t *testing.T, store *fakeDefaultStore, writer *fakeDefaultWriter, projects []int32, vaults *fakeVaultCreator,
) *PlatformModelDefaultService {
	t.Helper()
	return newPlatformDefaultFixture(t, store, writer, projects, vaults).service
}

func platformRow(name string) PlatformModelDefaultModelRow {
	return PlatformModelDefaultModelRow{OwnerProjectID: 1, Section: CurrentModelSectionLLM, Name: name, Shared: true}
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
		// Project 6 names the model only as its low-tier default.
		keyed: map[string]CurrentModelDefault{"6/llm_low_tier": {Name: "gpt-all", ProjectID: "1"}},
	}
	service := newTestPlatformDefaultService(t, store, &fakeDefaultWriter{}, []int32{1, 2, 3, 4, 5, 6}, nil)

	usage, err := service.Usage(context.Background(), PlatformModelDefaultModelRow{
		OwnerProjectID: 1, RowID: 9, Section: CurrentModelSectionLLM, Name: "gpt-all", Shared: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := PlatformModelDefaultUsage{ModelName: "gpt-all", PlatformDefault: true, Projects: 3}
	if usage != want {
		t.Fatalf("usage = %+v, want %+v (the public project is the platform default, not a project)", usage, want)
	}

	usage, err = service.Usage(context.Background(), PlatformModelDefaultModelRow{
		OwnerProjectID: 3, Section: CurrentModelSectionLLM, Name: "gpt-all",
	})
	if err != nil {
		t.Fatal(err)
	}
	if usage != (PlatformModelDefaultUsage{ModelName: "gpt-all", Projects: 1}) {
		t.Fatalf("project-owned usage = %+v", usage)
	}
}

func TestPlatformDefaultUsageOfAModelAnotherRowServesIsEmpty(t *testing.T) {
	store := &fakeDefaultStore{
		platform: CurrentModelDefault{Name: "gpt-all", ProjectID: "1"},
		projects: map[int32]CurrentModelDefault{2: {Name: "gpt-all", ProjectID: "1"}},
	}
	fixture := newPlatformDefaultFixture(t, store, &fakeDefaultWriter{}, []int32{2}, nil)
	fixture.rows.exists = true

	usage, err := fixture.service.Usage(context.Background(), PlatformModelDefaultModelRow{
		OwnerProjectID: 1, RowID: 9, Section: CurrentModelSectionLLM, Name: "gpt-all", Shared: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if usage != (PlatformModelDefaultUsage{ModelName: "gpt-all", ServedByAnotherRow: true}) {
		t.Fatalf("usage = %+v; another shared gpt-all row still serves the model", usage)
	}
	want := []PlatformModelDefaultRowQuery{{
		ProjectID: 1, Section: CurrentModelSectionLLM, Name: "gpt-all", SharedOnly: true, ExcludeID: 9,
	}}
	if !reflect.DeepEqual(fixture.rows.queries, want) {
		t.Fatalf("survivor query = %+v, want %+v (the row itself must not count)", fixture.rows.queries, want)
	}
	if store.projectLoads != 0 {
		t.Fatalf("the count read %d project vaults for a model that stays", store.projectLoads)
	}
}

func TestPlatformDefaultUsageOfANonSharedPublicRowReadsNoProject(t *testing.T) {
	store := &fakeDefaultStore{
		platform: CurrentModelDefault{Name: "scratch", ProjectID: "1"},
		projects: map[int32]CurrentModelDefault{2: {Name: "scratch", ProjectID: "1"}},
	}
	service := newTestPlatformDefaultService(t, store, &fakeDefaultWriter{}, []int32{2}, nil)
	row := platformRow("scratch")
	row.Shared = false
	usage, err := service.Usage(context.Background(), row)
	if err != nil {
		t.Fatal(err)
	}
	if usage != (PlatformModelDefaultUsage{ModelName: "scratch", PlatformDefault: true}) || store.projectLoads != 0 {
		t.Fatalf("usage = %+v loads=%d; a non-shared row was offered to no other project", usage, store.projectLoads)
	}
}

func TestPlatformDefaultUsageIsCachedUntilAWrite(t *testing.T) {
	store := &fakeDefaultStore{projects: map[int32]CurrentModelDefault{2: {Name: "gpt-all", ProjectID: "1"}}}
	fixture := newPlatformDefaultFixture(t, store, &fakeDefaultWriter{}, []int32{2}, nil)
	now := time.Unix(1_700_000_000, 0)
	fixture.service.now = func() time.Time { return now }
	row := platformRow("gpt-all")

	count := func() int {
		t.Helper()
		usage, err := fixture.service.Usage(context.Background(), row)
		if err != nil {
			t.Fatal(err)
		}
		return usage.Projects
	}
	for range 2 {
		if count() != 1 {
			t.Fatal("unexpected count")
		}
	}
	if fixture.lister.pages != 1 {
		t.Fatalf("a repeated count within the TTL read the projects %d times", fixture.lister.pages)
	}
	now = now.Add(platformModelDefaultUsageTTL)
	count()
	if fixture.lister.pages != 2 {
		t.Fatalf("an expired count was reused: pages=%d", fixture.lister.pages)
	}
	if err := fixture.service.Clear(context.Background(), CurrentModelSectionLLM); err != nil {
		t.Fatal(err)
	}
	count()
	if fixture.lister.pages != 3 {
		t.Fatalf("a count was reused after a write: pages=%d", fixture.lister.pages)
	}
}

func TestPlatformDefaultReleaseClearsThePlatformFirstAndOnlyMatchingProjects(t *testing.T) {
	store := &fakeDefaultStore{
		projects: map[int32]CurrentModelDefault{
			2: {Name: "gpt-all", ProjectID: "1"},
			3: {Name: "other", ProjectID: "1"},
		},
		keyed: map[string]CurrentModelDefault{"4/llm_high_tier": {Name: "gpt-all", ProjectID: "1"}},
	}
	writer := &fakeDefaultWriter{}
	service := newTestPlatformDefaultService(t, store, writer, []int32{1, 2, 3, 4, 5}, nil)

	if err := service.ReleaseDeletedModelDefault(context.Background(), platformRow("gpt-all")); err != nil {
		t.Fatal(err)
	}
	match := CurrentModelDefault{Name: "gpt-all", ProjectID: "1"}
	keys := []string{"llm", "llm_low_tier", "llm_high_tier"}
	var platform []CurrentModelDefaultClear
	for _, key := range keys {
		platform = append(platform,
			CurrentModelDefaultClear{ProjectID: 1, Section: key, Match: &match},
			CurrentModelDefaultClear{Admin: true, Section: key, Match: &match},
		)
	}
	if len(writer.clears) < len(platform) || !reflect.DeepEqual(writer.clears[:len(platform)], platform) {
		t.Fatalf("the platform defaults were not cleared first, for every key: %+v", writer.clears)
	}
	projectClears := writer.clears[len(platform):]
	sort.Slice(projectClears, func(i, j int) bool { return projectClears[i].ProjectID < projectClears[j].ProjectID })
	wantProjects := []CurrentModelDefaultClear{
		{ProjectID: 2, Section: "llm", Match: &match},
		{ProjectID: 4, Section: "llm_high_tier", Match: &match},
	}
	if !reflect.DeepEqual(projectClears, wantProjects) {
		t.Fatalf("project clears = %+v, want only the vaults that name the model: %+v", projectClears, wantProjects)
	}
}

func TestPlatformDefaultReleaseOfAModelNobodyChoseLocksNoProjectVault(t *testing.T) {
	store := &fakeDefaultStore{projects: map[int32]CurrentModelDefault{2: {Name: "other", ProjectID: "1"}}}
	writer := &fakeDefaultWriter{}
	service := newTestPlatformDefaultService(t, store, writer, []int32{2, 3, 4}, nil)
	if err := service.ReleaseDeletedModelDefault(context.Background(), platformRow("gpt-all")); err != nil {
		t.Fatal(err)
	}
	if projects := writer.clearedProjects(); !reflect.DeepEqual(projects, []int32{1}) {
		t.Fatalf("cleared vaults = %v, want the public vault only", projects)
	}
}

func TestPlatformDefaultReleaseSkipsAModelAnotherRowServes(t *testing.T) {
	store := &fakeDefaultStore{
		platform: CurrentModelDefault{Name: "gpt-4o", ProjectID: "1"},
		projects: map[int32]CurrentModelDefault{2: {Name: "gpt-4o", ProjectID: "1"}},
	}
	fixture := newPlatformDefaultFixture(t, store, &fakeDefaultWriter{}, []int32{2}, nil)
	fixture.rows.exists = true

	fanOut, err := fixture.service.ReleaseDeletedPlatformModelDefault(context.Background(), platformRow("gpt-4o"))
	if err != nil || fanOut {
		t.Fatalf("fanOut=%v err=%v", fanOut, err)
	}
	if err := fixture.service.ReleaseDeletedModelDefault(context.Background(), platformRow("gpt-4o")); err != nil {
		t.Fatal(err)
	}
	if len(fixture.writer.clears) != 0 {
		t.Fatalf("a model still served by another row was released: %+v", fixture.writer.clears)
	}
	if query := fixture.rows.queries[0]; !query.SharedOnly || query.ProjectID != 1 || query.Name != "gpt-4o" {
		t.Fatalf("survivor query = %+v; a public survivor must be shared", query)
	}
}

func TestPlatformDefaultReleaseOfANonSharedPublicRowClearsOnlyThePlatform(t *testing.T) {
	writer := &fakeDefaultWriter{}
	store := &fakeDefaultStore{projects: map[int32]CurrentModelDefault{2: {Name: "scratch", ProjectID: "1"}}}
	fixture := newPlatformDefaultFixture(t, store, writer, []int32{2}, nil)
	row := platformRow("scratch")
	row.Shared = false
	if err := fixture.service.ReleaseDeletedModelDefault(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if projects := writer.clearedProjects(); !reflect.DeepEqual(projects, []int32{1}) || fixture.lister.pages != 0 {
		t.Fatalf("cleared = %v pages=%d; a non-shared row must not fan out", projects, fixture.lister.pages)
	}
}

func TestPlatformDefaultReleaseIsBestEffortAcrossProjects(t *testing.T) {
	store := &fakeDefaultStore{projects: map[int32]CurrentModelDefault{
		2: {Name: "gpt-all", ProjectID: "1"},
		3: {Name: "gpt-all", ProjectID: "1"},
		4: {Name: "gpt-all", ProjectID: "1"},
	}}
	// The admin vault and project 3 will not open.
	writer := &fakeDefaultWriter{fail: map[int32]bool{-1: true, 3: true}}
	service := newTestPlatformDefaultService(t, store, writer, []int32{2, 3, 4}, nil)

	err := service.ReleaseDeletedModelDefault(context.Background(), platformRow("gpt-all"))
	if err == nil {
		t.Fatal("the failures were not reported")
	}
	if projects := writer.clearedProjects(); !reflect.DeepEqual(projects, []int32{1, 2, 3, 4}) {
		t.Fatalf("cleared = %v; one failure must not stop the other projects", projects)
	}
}

func TestPlatformDefaultReleaseClearsThePlatformWhenTheProjectListFails(t *testing.T) {
	writer := &fakeDefaultWriter{}
	fixture := newPlatformDefaultFixture(t, &fakeDefaultStore{}, writer, nil, nil)
	fixture.lister.err = errors.New("the project directory is down")

	if err := fixture.service.ReleaseDeletedModelDefault(context.Background(), platformRow("gpt-all")); err == nil {
		t.Fatal("the list failure was not reported")
	}
	admin := 0
	for _, clear := range writer.clears {
		if clear.Admin {
			admin++
		}
	}
	if projects := writer.clearedProjects(); !reflect.DeepEqual(projects, []int32{1}) || admin != 3 {
		t.Fatalf("cleared = %v admin=%d; the platform default must be cleared first", projects, admin)
	}
}

func TestPlatformDefaultReleasePagesThroughEveryProject(t *testing.T) {
	total := PlatformModelDefaultProjectPage*2 + 17
	ids := make([]int32, 0, total)
	defaults := map[int32]CurrentModelDefault{}
	for id := int32(2); len(ids) < total; id++ {
		ids = append(ids, id)
		defaults[id] = CurrentModelDefault{Name: "gpt-all", ProjectID: "1"}
	}
	writer := &fakeDefaultWriter{}
	fixture := newPlatformDefaultFixture(t, &fakeDefaultStore{projects: defaults}, writer, ids, nil)
	if err := fixture.service.ReleaseDeletedModelDefault(context.Background(), platformRow("gpt-all")); err != nil {
		t.Fatal(err)
	}
	if got := len(writer.clearedProjects()); got != total+1 {
		t.Fatalf("cleared %d vaults, want %d projects and the public one", got, total)
	}
	if fixture.lister.pages != 3 {
		t.Fatalf("pages = %d, want 3", fixture.lister.pages)
	}
}

func TestPlatformDefaultReleaseOfAProjectModelTouchesOnlyThatProject(t *testing.T) {
	writer := &fakeDefaultWriter{}
	fixture := newPlatformDefaultFixture(t, &fakeDefaultStore{}, writer, []int32{1, 2, 3}, nil)

	if err := fixture.service.ReleaseDeletedModelDefault(context.Background(), PlatformModelDefaultModelRow{
		OwnerProjectID: 3, Section: CurrentModelSectionLLM, Name: "mine",
	}); err != nil {
		t.Fatal(err)
	}
	match := &CurrentModelDefault{Name: "mine", ProjectID: strconv.Itoa(3)}
	want := []CurrentModelDefaultClear{
		{ProjectID: 3, Section: "llm", Match: match},
		{ProjectID: 3, Section: "llm_low_tier", Match: match},
		{ProjectID: 3, Section: "llm_high_tier", Match: match},
	}
	if !reflect.DeepEqual(writer.clears, want) {
		t.Fatalf("clears = %+v, want %+v", writer.clears, want)
	}
	if fixture.lister.pages != 0 || fixture.rows.queries[0].SharedOnly {
		t.Fatalf("a project model fanned out (pages=%d) or required a shared survivor", fixture.lister.pages)
	}
}

func TestPlatformDefaultServiceRefusesIncompleteComposition(t *testing.T) {
	if _, err := NewPlatformModelDefaultService(nil, &fakeDefaultStore{}, &fakeDefaultWriter{}, &fakeProjectLister{}, &fakeModelRows{}, nil, 1); err == nil {
		t.Fatal("a service with no candidates was composed")
	}
	if _, err := NewPlatformModelDefaultService(platformCandidates(), &fakeDefaultStore{}, &fakeDefaultWriter{}, &fakeProjectLister{}, nil, nil, 1); err == nil {
		t.Fatal("a service with no row reader was composed")
	}
	if _, err := NewPlatformModelDefaultService(platformCandidates(), &fakeDefaultStore{}, &fakeDefaultWriter{}, &fakeProjectLister{}, &fakeModelRows{}, nil, 0); err == nil {
		t.Fatal("a service with no public project was composed")
	}
}

// ── Mutation-route delete (lifecycle reconciler) ────────────────────────────

type recordingDefaultReleaser struct {
	rows []PlatformModelDefaultModelRow
	err  error
}

func (r *recordingDefaultReleaser) ReleaseDeletedModelDefault(_ context.Context, row PlatformModelDefaultModelRow) error {
	r.rows = append(r.rows, row)
	return r.err
}

func TestLifecycleDeleteReleasesTheDefaultsOfAProjectModel(t *testing.T) {
	releaser := &recordingDefaultReleaser{}
	reconciler := (&CurrentConfigurationLifecycleEffectsReconciler{
		policy: CurrentProviderProjectPolicy{PublicProjectID: 1},
	}).WithDeletedModelDefaults(releaser)

	for _, before := range []CurrentConfigurationLifecycleSnapshot{
		{ProjectID: 7, Section: "llm", Data: map[string]any{"name": "gpt-own"}},
		{ProjectID: 7, Section: "vectorstorage", EliteaTitle: "pgvector", Shared: true},
		// Not released: the public project, a credential, no name.
		{ProjectID: 1, Section: "llm", Data: map[string]any{"name": "gpt-all"}, Shared: true},
		{ProjectID: 7, Section: "ai_credentials", Data: map[string]any{"name": "x"}},
		{ProjectID: 7, Section: "embedding", Data: map[string]any{}},
	} {
		if _, err, failed := reconciler.releaseCurrentDeletedModelDefault(context.Background(), before); failed || err != nil {
			t.Fatalf("release of %+v failed: %v", before, err)
		}
	}
	want := []PlatformModelDefaultModelRow{
		{OwnerProjectID: 7, Section: CurrentModelSectionLLM, Name: "gpt-own"},
		{OwnerProjectID: 7, Section: CurrentModelSectionVectorStorage, Name: "pgvector", Shared: true},
	}
	if !reflect.DeepEqual(releaser.rows, want) {
		t.Fatalf("releases = %+v, want %+v", releaser.rows, want)
	}

	// A vault failure is best effort; only a context error retries.
	releaser.err = errors.New("sealed")
	if _, err, failed := reconciler.releaseCurrentDeletedModelDefault(context.Background(), CurrentConfigurationLifecycleSnapshot{
		ProjectID: 7, Section: "llm", Data: map[string]any{"name": "gpt-own"},
	}); failed || err != nil {
		t.Fatalf("a vault failure retried the event: failed=%v err=%v", failed, err)
	}
	releaser.err = context.DeadlineExceeded
	if _, err, failed := reconciler.releaseCurrentDeletedModelDefault(context.Background(), CurrentConfigurationLifecycleSnapshot{
		ProjectID: 7, Section: "llm", Data: map[string]any{"name": "gpt-own"},
	}); !failed || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a deadline did not retry: failed=%v err=%v", failed, err)
	}
}
