package configurations

// Platform default model (#6826 part 1).
//
// ## Where the value lives
//
// The platform default is the default model stored in the PUBLIC project's
// vault: the keys `default_<section>_model_name` and
// `default_<section>_model_project_id`. No new store is added. The catalogue
// reader already falls back to those keys (then to the admin vault) for every
// project that stored no default of its own, which is the legacy
// EngineBase.get_all_secrets precedence. The admin console now reads and
// writes the same keys, so the console and every model picker read one value.
//
// The public project is also the catalogue project (UI-PD-2, #1031): its
// shared rows are the platform models every project sees. Its own stored
// default and the platform default are therefore the same value.
//
// ## Precedence, for one project's catalogue read
//
//  1. The project's own stored default, when it names a model in the
//     project's catalogue.
//  2. The platform default, when it names a model in the project's
//     catalogue. A shared model the project holds no grant for is not in
//     that catalogue, so it is skipped here.
//  3. The first model of the catalogue. This is the legacy behaviour for
//     "no default", and it is unchanged: a catalogue that has models always
//     reports one default, so the chat never starts with no model.
//  4. No default, only when the catalogue is empty.
//
// A stored id never reaches a caller unless it resolves, so a deleted or
// disabled model cannot make a read fail.
//
// ## New projects
//
// Provisioning copies the platform default into the new project's vault
// (SeedProjectModelDefault). The project then owns that choice: a later
// change of the platform default does not move it. A project that was
// created before this change, or whose seed failed, still reads the platform
// default through step 2 or the public fallback.
//
// ## Removal
//
// A platform default must be offered to EVERY project (grant scope `all`),
// because a new project is in no `projects` grant list. When a model is
// deleted, ReleaseDeletedModelDefault clears each stored default that names it.
// That includes the low-tier and high-tier LLM defaults.
//
//   - Nothing is cleared while another row of the owning project still serves
//     the same section and name. For the public project, that row must be
//     shared. A stored default names a model by (name, project), not by row.
//   - For a platform model, the public and admin vaults are cleared FIRST.
//     The per-project fan-out comes after it, is paged and best effort, and
//     locks only a vault whose unlocked read names the model.
//   - A non-shared public row was never offered to other projects, so its
//     delete clears only the platform default.
//
// A model that is only disabled (status_ok false, or a narrower grant) is not
// cleared, so it comes back when it is enabled again. While it is disabled the
// precedence above skips it.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	// ErrPlatformModelDefaultIneligible refuses a platform default that is not
	// a shared model of the public project offered to every project.
	ErrPlatformModelDefaultIneligible = errors.New("the model is not a platform model offered to every project")
	// ErrInvalidPlatformModelDefault refuses a malformed request.
	ErrInvalidPlatformModelDefault = errors.New("invalid platform default model request")
)

const (
	// MaxPlatformModelDefaultProjects bounds the fan-out of a usage count or a
	// release. The project ids are read in pages, so it is a safety bound and
	// not the size of one read.
	MaxPlatformModelDefaultProjects = 200_000
	// PlatformModelDefaultProjectPage is the size of one page of project ids.
	PlatformModelDefaultProjectPage = 1_000
	// platformModelDefaultUsageTTL is how long a usage count is reused. The
	// count decrypts every project vault, so a repeated dialog must not repeat
	// that work.
	platformModelDefaultUsageTTL = 30 * time.Second
	// maxPlatformModelDefaultUsageEntries bounds the usage cache.
	maxPlatformModelDefaultUsageEntries = 256
)

// CurrentModelDefaultClear names one stored default to clear.
//
// Section is the key prefix of the default: a model section, or one of the
// LLM tier prefixes (CurrentModelDefaultKeys). Match, when set, limits the
// clear to a stored default that names exactly that model, so a concurrent new
// choice is never removed. Admin selects the admin vault instead of the
// project vault.
type CurrentModelDefaultClear struct {
	ProjectID int32
	Admin     bool
	Section   string
	Match     *CurrentModelDefault
}

// CurrentModelDefaultKeys lists the key prefixes of the stored defaults of a
// section. The LLM section also stores a low-tier and a high-tier default.
func CurrentModelDefaultKeys(section CurrentModelSection) []string {
	if section == CurrentModelSectionLLM {
		return []string{string(section), "llm_low_tier", "llm_high_tier"}
	}
	return []string{string(section)}
}

// IsCurrentModelDefaultKey reports whether key is the key prefix of a stored
// default.
func IsCurrentModelDefaultKey(key string) bool {
	if IsSupportedCurrentModelSection(CurrentModelSection(key)) {
		return true
	}
	return key == "llm_low_tier" || key == "llm_high_tier"
}

// PlatformModelDefaultStore reads stored defaults without any fallback.
type PlatformModelDefaultStore interface {
	CurrentPlatformModelDefaultLoader
	// LoadProjectModelDefault reads one project's own stored default for one
	// key prefix (CurrentModelDefaultKeys) without a lock. A project with no
	// vault answers an empty default.
	LoadProjectModelDefault(context.Context, int32, string) (CurrentModelDefault, error)
}

// PlatformModelDefaultWriter writes and clears stored defaults.
type PlatformModelDefaultWriter interface {
	SetCurrentModelDefault(context.Context, CurrentModelDefaultSelection) error
	// ClearCurrentModelDefault removes both keys and reports whether it
	// removed anything. An absent vault is not an error.
	ClearCurrentModelDefault(context.Context, CurrentModelDefaultClear) (bool, error)
}

// PlatformModelDefaultProjects pages through the active projects.
type PlatformModelDefaultProjects interface {
	// ListActiveProjectIDsAfter returns at most limit active project ids
	// greater than after, in ascending order.
	ListActiveProjectIDsAfter(ctx context.Context, after int32, limit int) ([]int32, error)
}

// PlatformModelDefaultRowQuery asks whether a model row can still resolve a
// stored default.
type PlatformModelDefaultRowQuery struct {
	ProjectID int32
	Section   CurrentModelSection
	// Name is data.name, or elitea_title for a vector storage row.
	Name string
	// SharedOnly counts shared rows only.
	SharedOnly bool
	// ExcludeID leaves one row out: the row that is about to be deleted.
	ExcludeID int32
}

// PlatformModelDefaultRows reads the model rows of a project.
type PlatformModelDefaultRows interface {
	ModelRowExists(context.Context, PlatformModelDefaultRowQuery) (bool, error)
}

// PlatformModelDefaultModelRow identifies the model row a usage count or a
// release is about.
type PlatformModelDefaultModelRow struct {
	OwnerProjectID int32
	// RowID is the id of the row. A usage count reads it before the delete
	// and does not count the row itself as a survivor. A release reads it
	// after the delete, where it is 0.
	RowID   int32
	Section CurrentModelSection
	Name    string
	Shared  bool
}

// PlatformModelDefaultVaultCreator creates the empty vault of a project that
// has none. The public project of a fresh install has no vault, because
// migrations create it without the provisioner.
type PlatformModelDefaultVaultCreator interface {
	EnsureProjectVault(ctx context.Context, projectID string) error
}

// PlatformModelDefaultCandidate is one model the admin can choose.
type PlatformModelDefaultCandidate struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

// PlatformModelDefaultView is what the admin console shows.
type PlatformModelDefaultView struct {
	Section string `json:"section"`
	// ModelName and ModelProjectID are the stored value, empty when none.
	ModelName      string `json:"model_name"`
	ModelProjectID *int32 `json:"model_project_id"`
	// Available is false when a value is stored and it is not one of the
	// candidates: the model was deleted, disabled or narrowed. The console
	// asks for a replacement.
	Available  bool                            `json:"available"`
	Candidates []PlatformModelDefaultCandidate `json:"candidates"`
}

// PlatformModelDefaultUsage counts the stored defaults that name one model.
type PlatformModelDefaultUsage struct {
	ModelName string `json:"model_name"`
	// PlatformDefault is true when the model is the platform default.
	PlatformDefault bool `json:"platform_default"`
	// Projects counts the OTHER projects whose own default names the model,
	// as the default or as a low-tier or high-tier default.
	Projects int `json:"projects"`
	// ServedByAnotherRow is true when another row serves the same model, so
	// a delete of this row releases no default.
	ServedByAnotherRow bool `json:"served_by_another_row"`
}

// PlatformModelDefaultService manages the platform default model.
type PlatformModelDefaultService struct {
	candidates      CurrentModelCandidateRepository
	store           PlatformModelDefaultStore
	writer          PlatformModelDefaultWriter
	projects        PlatformModelDefaultProjects
	rows            PlatformModelDefaultRows
	vaults          PlatformModelDefaultVaultCreator
	publicProjectID int32

	now        func() time.Time
	usageMutex sync.Mutex
	usageCache map[PlatformModelDefaultModelRow]platformModelDefaultUsageEntry
}

type platformModelDefaultUsageEntry struct {
	usage   PlatformModelDefaultUsage
	expires time.Time
}

// NewPlatformModelDefaultService composes the service. vaults may be nil: the
// write then requires a public vault that already exists.
func NewPlatformModelDefaultService(
	candidates CurrentModelCandidateRepository,
	store PlatformModelDefaultStore,
	writer PlatformModelDefaultWriter,
	projects PlatformModelDefaultProjects,
	rows PlatformModelDefaultRows,
	vaults PlatformModelDefaultVaultCreator,
	publicProjectID int32,
) (*PlatformModelDefaultService, error) {
	if candidates == nil || store == nil || writer == nil || projects == nil || rows == nil || publicProjectID <= 0 {
		return nil, ErrInvalidPlatformModelDefault
	}
	return &PlatformModelDefaultService{
		candidates: candidates, store: store, writer: writer, projects: projects, rows: rows,
		vaults: vaults, publicProjectID: publicProjectID, now: time.Now,
		usageCache: map[PlatformModelDefaultModelRow]platformModelDefaultUsageEntry{},
	}, nil
}

// PublicProjectID is the catalogue project the service is bound to.
func (s *PlatformModelDefaultService) PublicProjectID() int32 { return s.publicProjectID }

// Get reports the stored platform default and the models it may name.
func (s *PlatformModelDefaultService) Get(
	ctx context.Context, section CurrentModelSection,
) (PlatformModelDefaultView, error) {
	if ctx == nil || !IsSupportedCurrentModelSection(section) {
		return PlatformModelDefaultView{}, ErrInvalidPlatformModelDefault
	}
	candidates, err := s.eligible(ctx, section)
	if err != nil {
		return PlatformModelDefaultView{}, err
	}
	stored, err := s.store.LoadPlatformModelDefault(ctx, s.publicProjectID, section)
	if err != nil {
		return PlatformModelDefaultView{}, currentModelCatalogDependencyError(ctx, "load platform model default", err)
	}
	view := PlatformModelDefaultView{
		Section:    string(section),
		ModelName:  stored.Name,
		Available:  true,
		Candidates: candidates,
	}
	if projectID, ok := parsePlatformModelDefaultProjectID(stored.ProjectID); ok {
		view.ModelProjectID = &projectID
	}
	if stored.Name != "" {
		view.Available = view.ModelProjectID != nil && *view.ModelProjectID == s.publicProjectID &&
			containsPlatformModelDefaultCandidate(candidates, stored.Name)
	}
	return view, nil
}

// Set stores name as the platform default. name must be an eligible model.
func (s *PlatformModelDefaultService) Set(ctx context.Context, section CurrentModelSection, name string) error {
	if ctx == nil || !IsSupportedCurrentModelSection(section) || name == "" || name != strings.TrimSpace(name) {
		return ErrInvalidPlatformModelDefault
	}
	candidates, err := s.eligible(ctx, section)
	if err != nil {
		return err
	}
	if !containsPlatformModelDefaultCandidate(candidates, name) {
		return ErrPlatformModelDefaultIneligible
	}
	if s.vaults != nil {
		if err := s.vaults.EnsureProjectVault(ctx, strconv.FormatInt(int64(s.publicProjectID), 10)); err != nil {
			return currentModelCatalogDependencyError(ctx, "ensure the public project vault", err)
		}
	}
	defer s.forgetUsage()
	return s.writer.SetCurrentModelDefault(ctx, CurrentModelDefaultSelection{
		ProjectID:       s.publicProjectID,
		Name:            name,
		TargetProjectID: int64(s.publicProjectID),
		Section:         string(section),
	})
}

// Clear removes the platform default from the public vault and from the
// legacy admin vault, so no stored value is left to show through.
func (s *PlatformModelDefaultService) Clear(ctx context.Context, section CurrentModelSection) error {
	if ctx == nil || !IsSupportedCurrentModelSection(section) {
		return ErrInvalidPlatformModelDefault
	}
	defer s.forgetUsage()
	if _, err := s.writer.ClearCurrentModelDefault(ctx, CurrentModelDefaultClear{
		ProjectID: s.publicProjectID, Section: string(section),
	}); err != nil {
		return err
	}
	_, err := s.writer.ClearCurrentModelDefault(ctx, CurrentModelDefaultClear{
		Admin: true, Section: string(section),
	})
	return err
}

// SeedProjectModelDefault copies the platform default LLM into a new project's
// vault. It writes nothing when no platform default is stored, or when the
// stored one is no longer eligible: the project then reads the precedence at
// request time.
func (s *PlatformModelDefaultService) SeedProjectModelDefault(ctx context.Context, projectID int64) error {
	if ctx == nil || projectID <= 0 || projectID > int64(^uint32(0)>>1) {
		return ErrInvalidPlatformModelDefault
	}
	if int32(projectID) == s.publicProjectID {
		return nil
	}
	view, err := s.Get(ctx, CurrentModelSectionLLM)
	if err != nil {
		return err
	}
	if view.ModelName == "" || !view.Available {
		return nil
	}
	return s.writer.SetCurrentModelDefault(ctx, CurrentModelDefaultSelection{
		ProjectID:       int32(projectID),
		Name:            view.ModelName,
		TargetProjectID: int64(s.publicProjectID),
		Section:         string(CurrentModelSectionLLM),
	})
}

// Usage counts the stored defaults that name one model row: what its delete
// would release. A project-owned model can be the default of its own project
// only. A platform model can be the default of every project. A count is
// reused for platformModelDefaultUsageTTL.
func (s *PlatformModelDefaultService) Usage(
	ctx context.Context, row PlatformModelDefaultModelRow,
) (PlatformModelDefaultUsage, error) {
	if ctx == nil || !s.validModelRow(row) {
		return PlatformModelDefaultUsage{}, ErrInvalidPlatformModelDefault
	}
	if usage, ok := s.cachedUsage(row); ok {
		return usage, nil
	}
	usage := PlatformModelDefaultUsage{ModelName: row.Name}
	served, err := s.servedByAnotherRow(ctx, row)
	if err != nil {
		return PlatformModelDefaultUsage{}, err
	}
	if served {
		usage.ServedByAnotherRow = true
		s.storeUsage(row, usage)
		return usage, nil
	}
	match := s.match(row)
	if row.OwnerProjectID == s.publicProjectID {
		platform, err := s.store.LoadPlatformModelDefault(ctx, s.publicProjectID, row.Section)
		if err != nil {
			return PlatformModelDefaultUsage{}, currentModelCatalogDependencyError(ctx, "load platform model default", err)
		}
		usage.PlatformDefault = platform == match
	}
	if row.OwnerProjectID != s.publicProjectID || s.fansOut(row) {
		var mutex sync.Mutex
		err = s.forEachAffectedProject(ctx, row.OwnerProjectID, func(projectID int32) error {
			keys, err := s.matchingKeys(ctx, projectID, row.Section, match)
			if err != nil {
				return err
			}
			if len(keys) > 0 {
				mutex.Lock()
				usage.Projects++
				mutex.Unlock()
			}
			return nil
		})
		if err != nil {
			return PlatformModelDefaultUsage{}, err
		}
	}
	s.storeUsage(row, usage)
	return usage, nil
}

// ReleaseDeletedModelDefault clears every stored default that names a model
// row deleted from row.OwnerProjectID: first the platform-level defaults, then
// the per-project ones. It is called after the row delete commits. A failure
// leaves a stored id that the catalogue precedence already skips, so it never
// makes a read fail.
func (s *PlatformModelDefaultService) ReleaseDeletedModelDefault(
	ctx context.Context, row PlatformModelDefaultModelRow,
) error {
	fanOut, err := s.ReleaseDeletedPlatformModelDefault(ctx, row)
	if !fanOut {
		return err
	}
	// The fan-out runs even when the first step failed: one sealed vault must
	// not keep every other project's stale default.
	return errors.Join(err, s.ReleaseDeletedProjectModelDefaults(ctx, row))
}

// ReleaseDeletedPlatformModelDefault is the first, short step of a release.
// It does nothing when another row still serves the model. Otherwise it clears
// the public and admin vault defaults of a platform model, or the owning
// project's default of a project model. It reports whether the per-project
// fan-out (ReleaseDeletedProjectModelDefaults) is still to do.
func (s *PlatformModelDefaultService) ReleaseDeletedPlatformModelDefault(
	ctx context.Context, row PlatformModelDefaultModelRow,
) (bool, error) {
	if ctx == nil || !s.validModelRow(row) {
		return false, ErrInvalidPlatformModelDefault
	}
	served, err := s.servedByAnotherRow(ctx, row)
	if err != nil || served {
		return false, err
	}
	defer s.forgetUsage()
	match := s.match(row)
	var failures []error
	for _, key := range CurrentModelDefaultKeys(row.Section) {
		if row.OwnerProjectID != s.publicProjectID {
			_, err := s.writer.ClearCurrentModelDefault(ctx, CurrentModelDefaultClear{
				ProjectID: row.OwnerProjectID, Section: key, Match: &match,
			})
			failures = append(failures, err)
			continue
		}
		_, publicErr := s.writer.ClearCurrentModelDefault(ctx, CurrentModelDefaultClear{
			ProjectID: s.publicProjectID, Section: key, Match: &match,
		})
		_, adminErr := s.writer.ClearCurrentModelDefault(ctx, CurrentModelDefaultClear{
			Admin: true, Section: key, Match: &match,
		})
		failures = append(failures, publicErr, adminErr)
	}
	return s.fansOut(row), errors.Join(failures...)
}

// ReleaseDeletedProjectModelDefaults is the second, long step of the release of
// a platform model: it clears each other project's own default that names it.
// It reads each vault without a lock and locks only a vault that names the
// model. It is best effort: one project that fails does not stop the others.
func (s *PlatformModelDefaultService) ReleaseDeletedProjectModelDefaults(
	ctx context.Context, row PlatformModelDefaultModelRow,
) error {
	if ctx == nil || !s.validModelRow(row) {
		return ErrInvalidPlatformModelDefault
	}
	if !s.fansOut(row) {
		return nil
	}
	defer s.forgetUsage()
	match := s.match(row)
	var failed sync.Mutex
	failures, firstFailure := 0, error(nil)
	err := s.forEachAffectedProject(ctx, row.OwnerProjectID, func(projectID int32) error {
		keys, err := s.matchingKeys(ctx, projectID, row.Section, match)
		for _, key := range keys {
			if err != nil {
				break
			}
			_, err = s.writer.ClearCurrentModelDefault(ctx, CurrentModelDefaultClear{
				ProjectID: projectID, Section: key, Match: &match,
			})
		}
		if err != nil && ctx.Err() == nil {
			failed.Lock()
			failures++
			if firstFailure == nil {
				firstFailure = err
			}
			failed.Unlock()
			slog.WarnContext(ctx, "a project still names a deleted model as its default; the catalogue skips it",
				"project_id", projectID, "section", string(row.Section), "err", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if failures > 0 {
		return fmt.Errorf("release the deleted model default in %d projects: %w", failures, firstFailure)
	}
	return nil
}

// fansOut reports whether other projects can name the model. Only a shared row
// of the public project is offered to them.
func (s *PlatformModelDefaultService) fansOut(row PlatformModelDefaultModelRow) bool {
	return row.OwnerProjectID == s.publicProjectID && row.Shared
}

func (s *PlatformModelDefaultService) validModelRow(row PlatformModelDefaultModelRow) bool {
	return row.OwnerProjectID > 0 && row.RowID >= 0 && IsSupportedCurrentModelSection(row.Section) && row.Name != ""
}

func (s *PlatformModelDefaultService) match(row PlatformModelDefaultModelRow) CurrentModelDefault {
	return CurrentModelDefault{Name: row.Name, ProjectID: strconv.FormatInt(int64(row.OwnerProjectID), 10)}
}

// servedByAnotherRow reports whether another row of the owning project still
// resolves a stored default that names the model. A row of the public project
// must be shared to resolve for other projects.
func (s *PlatformModelDefaultService) servedByAnotherRow(
	ctx context.Context, row PlatformModelDefaultModelRow,
) (bool, error) {
	served, err := s.rows.ModelRowExists(ctx, PlatformModelDefaultRowQuery{
		ProjectID:  row.OwnerProjectID,
		Section:    row.Section,
		Name:       row.Name,
		SharedOnly: row.OwnerProjectID == s.publicProjectID,
		ExcludeID:  row.RowID,
	})
	if err != nil {
		return false, currentModelCatalogDependencyError(ctx, "read the model rows", err)
	}
	return served, nil
}

// matchingKeys reads one project's own defaults without a lock and returns the
// key prefixes that name the model.
func (s *PlatformModelDefaultService) matchingKeys(
	ctx context.Context, projectID int32, section CurrentModelSection, match CurrentModelDefault,
) ([]string, error) {
	var keys []string
	for _, key := range CurrentModelDefaultKeys(section) {
		stored, err := s.store.LoadProjectModelDefault(ctx, projectID, key)
		if err != nil {
			return nil, currentModelCatalogDependencyError(ctx, "load project model default", err)
		}
		if stored == match {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

func (s *PlatformModelDefaultService) cachedUsage(row PlatformModelDefaultModelRow) (PlatformModelDefaultUsage, bool) {
	s.usageMutex.Lock()
	defer s.usageMutex.Unlock()
	entry, ok := s.usageCache[row]
	if !ok || !s.now().Before(entry.expires) {
		return PlatformModelDefaultUsage{}, false
	}
	return entry.usage, true
}

func (s *PlatformModelDefaultService) storeUsage(row PlatformModelDefaultModelRow, usage PlatformModelDefaultUsage) {
	s.usageMutex.Lock()
	defer s.usageMutex.Unlock()
	if len(s.usageCache) >= maxPlatformModelDefaultUsageEntries {
		clear(s.usageCache)
	}
	s.usageCache[row] = platformModelDefaultUsageEntry{
		usage: usage, expires: s.now().Add(platformModelDefaultUsageTTL),
	}
}

// forgetUsage drops every cached count. A write can change any of them.
func (s *PlatformModelDefaultService) forgetUsage() {
	s.usageMutex.Lock()
	defer s.usageMutex.Unlock()
	clear(s.usageCache)
}

// eligible lists the public project's shared models offered to every project.
func (s *PlatformModelDefaultService) eligible(
	ctx context.Context, section CurrentModelSection,
) ([]PlatformModelDefaultCandidate, error) {
	items, err := s.candidates.List(ctx, s.publicProjectID, section, true)
	if err != nil {
		return nil, currentModelCatalogDependencyError(ctx, "list platform models", err)
	}
	seen := make(map[string]struct{}, len(items))
	candidates := make([]PlatformModelDefaultCandidate, 0, len(items))
	for _, item := range items {
		if !item.Shared || item.ProjectID != s.publicProjectID || item.Name == "" ||
			item.Grant.Scope != ModelShareScopeAll {
			continue
		}
		if _, duplicate := seen[item.Name]; duplicate {
			continue
		}
		seen[item.Name] = struct{}{}
		candidates = append(candidates, PlatformModelDefaultCandidate{
			Name: item.Name, DisplayName: currentModelDisplayName(item),
		})
	}
	sort.SliceStable(candidates, func(left, right int) bool {
		return strings.ToLower(candidates[left].DisplayName) < strings.ToLower(candidates[right].DisplayName)
	})
	return candidates, nil
}

// forEachAffectedProject runs fn for the owning project of a project model,
// or for each active project other than the public one, page by page, with
// bounded concurrency. It stops on an error fn returns, and above
// MaxPlatformModelDefaultProjects projects.
func (s *PlatformModelDefaultService) forEachAffectedProject(
	ctx context.Context, ownerProjectID int32, fn func(int32) error,
) error {
	if ownerProjectID != s.publicProjectID {
		return s.forEachProject(ctx, []int32{ownerProjectID}, fn)
	}
	after, seen := int32(0), 0
	for {
		page, err := s.projects.ListActiveProjectIDsAfter(ctx, after, PlatformModelDefaultProjectPage)
		if err != nil {
			return currentModelCatalogDependencyError(ctx, "list active projects", err)
		}
		seen += len(page)
		if seen > MaxPlatformModelDefaultProjects {
			return ErrCurrentConfigurationLifecycleInternalLimit
		}
		projectIDs := make([]int32, 0, len(page))
		for _, projectID := range page {
			if projectID <= after {
				return errors.New("the active project page is not in ascending order")
			}
			after = projectID
			if projectID != s.publicProjectID {
				projectIDs = append(projectIDs, projectID)
			}
		}
		if err := s.forEachProject(ctx, projectIDs, fn); err != nil {
			return err
		}
		if len(page) < PlatformModelDefaultProjectPage {
			return nil
		}
	}
}

// forEachProject runs fn for each project with bounded concurrency and joins
// the errors.
func (s *PlatformModelDefaultService) forEachProject(
	ctx context.Context, projectIDs []int32, fn func(int32) error,
) error {
	if len(projectIDs) == 0 {
		return nil
	}
	jobs := make(chan int, len(projectIDs))
	for index := range projectIDs {
		jobs <- index
	}
	close(jobs)
	failures := make([]error, len(projectIDs))
	var group sync.WaitGroup
	for range min(MaxCurrentDeletedLLMConcurrency, len(projectIDs)) {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range jobs {
				if ctx.Err() != nil {
					return
				}
				failures[index] = fn(projectIDs[index])
			}
		}()
	}
	group.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.Join(failures...)
}

func containsPlatformModelDefaultCandidate(candidates []PlatformModelDefaultCandidate, name string) bool {
	for _, candidate := range candidates {
		if candidate.Name == name {
			return true
		}
	}
	return false
}

func parsePlatformModelDefaultProjectID(value string) (int32, bool) {
	parsed, err := strconv.ParseInt(value, 10, 32)
	if err != nil || parsed <= 0 {
		return 0, false
	}
	return int32(parsed), true
}
