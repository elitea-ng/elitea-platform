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
// deleted, ReleaseDeletedModelDefault clears each stored default that names it,
// in the owning project or, for a platform model, in every active project and
// the public project. A model that is only disabled (status_ok false, or a
// narrower grant) is not cleared, so it comes back when it is enabled again;
// while it is disabled the precedence above skips it.

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
)

var (
	// ErrPlatformModelDefaultIneligible refuses a platform default that is not
	// a shared model of the public project offered to every project.
	ErrPlatformModelDefaultIneligible = errors.New("the model is not a platform model offered to every project")
	// ErrInvalidPlatformModelDefault refuses a malformed request.
	ErrInvalidPlatformModelDefault = errors.New("invalid platform default model request")
)

// MaxPlatformModelDefaultUsageProjects bounds the fan-out of a usage count or
// a release. It is the same bound the deleted-LLM reference repair uses.
const MaxPlatformModelDefaultUsageProjects = MaxCurrentDeletedLLMProjects

// CurrentModelDefaultClear names one stored default to clear.
//
// Match, when set, limits the clear to a stored default that names exactly
// that model, so a concurrent new choice is never removed. Admin selects the
// admin vault instead of the project vault.
type CurrentModelDefaultClear struct {
	ProjectID int32
	Admin     bool
	Section   string
	Match     *CurrentModelDefault
}

// PlatformModelDefaultStore reads stored defaults without any fallback.
type PlatformModelDefaultStore interface {
	CurrentPlatformModelDefaultLoader
	// LoadProjectModelDefault reads one project's own stored default. A
	// project with no vault answers an empty default.
	LoadProjectModelDefault(context.Context, int32, CurrentModelSection) (CurrentModelDefault, error)
}

// PlatformModelDefaultWriter writes and clears stored defaults.
type PlatformModelDefaultWriter interface {
	SetCurrentModelDefault(context.Context, CurrentModelDefaultSelection) error
	// ClearCurrentModelDefault removes both keys and reports whether it
	// removed anything. An absent vault is not an error.
	ClearCurrentModelDefault(context.Context, CurrentModelDefaultClear) (bool, error)
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
	// Projects counts the OTHER projects whose own default names the model.
	Projects int `json:"projects"`
}

// PlatformModelDefaultService manages the platform default model.
type PlatformModelDefaultService struct {
	candidates      CurrentModelCandidateRepository
	store           PlatformModelDefaultStore
	writer          PlatformModelDefaultWriter
	projects        CurrentDeletedLLMProjectRepository
	vaults          PlatformModelDefaultVaultCreator
	publicProjectID int32
}

// NewPlatformModelDefaultService composes the service. vaults may be nil: the
// write then requires a public vault that already exists.
func NewPlatformModelDefaultService(
	candidates CurrentModelCandidateRepository,
	store PlatformModelDefaultStore,
	writer PlatformModelDefaultWriter,
	projects CurrentDeletedLLMProjectRepository,
	vaults PlatformModelDefaultVaultCreator,
	publicProjectID int32,
) (*PlatformModelDefaultService, error) {
	if candidates == nil || store == nil || writer == nil || projects == nil || publicProjectID <= 0 {
		return nil, ErrInvalidPlatformModelDefault
	}
	return &PlatformModelDefaultService{
		candidates: candidates, store: store, writer: writer, projects: projects,
		vaults: vaults, publicProjectID: publicProjectID,
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

// Usage counts the stored defaults that name one model of ownerProjectID. A
// project-owned model can be the default of its own project only. A platform
// model can be the default of every project.
func (s *PlatformModelDefaultService) Usage(
	ctx context.Context, ownerProjectID int32, section CurrentModelSection, name string,
) (PlatformModelDefaultUsage, error) {
	if ctx == nil || ownerProjectID <= 0 || !IsSupportedCurrentModelSection(section) || name == "" {
		return PlatformModelDefaultUsage{}, ErrInvalidPlatformModelDefault
	}
	usage := PlatformModelDefaultUsage{ModelName: name}
	match := CurrentModelDefault{Name: name, ProjectID: strconv.FormatInt(int64(ownerProjectID), 10)}
	if ownerProjectID == s.publicProjectID {
		platform, err := s.store.LoadPlatformModelDefault(ctx, s.publicProjectID, section)
		if err != nil {
			return PlatformModelDefaultUsage{}, currentModelCatalogDependencyError(ctx, "load platform model default", err)
		}
		usage.PlatformDefault = platform == match
	}
	projectIDs, err := s.affectedProjects(ctx, ownerProjectID)
	if err != nil {
		return PlatformModelDefaultUsage{}, err
	}
	var mutex sync.Mutex
	err = s.forEachProject(ctx, projectIDs, func(projectID int32) error {
		stored, err := s.store.LoadProjectModelDefault(ctx, projectID, section)
		if err != nil {
			return currentModelCatalogDependencyError(ctx, "load project model default", err)
		}
		if stored == match {
			mutex.Lock()
			usage.Projects++
			mutex.Unlock()
		}
		return nil
	})
	if err != nil {
		return PlatformModelDefaultUsage{}, err
	}
	return usage, nil
}

// ReleaseDeletedModelDefault clears every stored default that names a model
// deleted from ownerProjectID. It is called after the row delete commits. A
// failure leaves a stored id that the catalogue precedence already skips, so
// it never makes a read fail.
func (s *PlatformModelDefaultService) ReleaseDeletedModelDefault(
	ctx context.Context, ownerProjectID int32, section CurrentModelSection, name string,
) error {
	if ctx == nil || ownerProjectID <= 0 || !IsSupportedCurrentModelSection(section) || name == "" {
		return ErrInvalidPlatformModelDefault
	}
	match := CurrentModelDefault{Name: name, ProjectID: strconv.FormatInt(int64(ownerProjectID), 10)}
	projectIDs, err := s.affectedProjects(ctx, ownerProjectID)
	if err != nil {
		return err
	}
	if ownerProjectID == s.publicProjectID {
		projectIDs = append(projectIDs, s.publicProjectID)
	}
	err = s.forEachProject(ctx, projectIDs, func(projectID int32) error {
		_, err := s.writer.ClearCurrentModelDefault(ctx, CurrentModelDefaultClear{
			ProjectID: projectID, Section: string(section), Match: &match,
		})
		return err
	})
	if err != nil || ownerProjectID != s.publicProjectID {
		return err
	}
	_, err = s.writer.ClearCurrentModelDefault(ctx, CurrentModelDefaultClear{
		Admin: true, Section: string(section), Match: &match,
	})
	return err
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

// affectedProjects lists the projects other than the public one whose stored
// default can name a model of ownerProjectID.
func (s *PlatformModelDefaultService) affectedProjects(ctx context.Context, ownerProjectID int32) ([]int32, error) {
	if ownerProjectID != s.publicProjectID {
		return []int32{ownerProjectID}, nil
	}
	projectIDs, err := s.projects.ListActiveCurrentProjectIDs(ctx, MaxPlatformModelDefaultUsageProjects+1)
	if err != nil {
		return nil, currentModelCatalogDependencyError(ctx, "list active projects", err)
	}
	if len(projectIDs) > MaxPlatformModelDefaultUsageProjects {
		return nil, ErrCurrentConfigurationLifecycleInternalLimit
	}
	result := make([]int32, 0, len(projectIDs))
	for _, projectID := range projectIDs {
		if projectID > 0 && projectID != s.publicProjectID {
			result = append(result, projectID)
		}
	}
	return result, nil
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
