package configurations

import (
	"context"
	"errors"
	"fmt"
)

var ErrInvalidCurrentModelCatalogRequest = errors.New("invalid current model catalog request")

// CurrentModelCatalogQuery is the authorized identity and selection input for
// one current model-list request. Project IDs are database identities, never
// schema names supplied directly to a repository.
type CurrentModelCatalogQuery struct {
	Section         CurrentModelSection
	ProjectID       int32
	PublicProjectID int32
	IncludeShared   bool
}

// CurrentModelCandidateRepository reads one bounded, tenant-scoped candidate
// list. sharedOnly is true only for the public-project fallback query.
type CurrentModelCandidateRepository interface {
	List(context.Context, int32, CurrentModelSection, bool) ([]CurrentModelCatalogItem, error)
}

// CurrentModelDefaultsLoader reads the current project/public vault sources.
// Secret precedence and encrypted-storage compatibility belong to its adapter.
type CurrentModelDefaultsLoader interface {
	Load(context.Context, int32, int32, CurrentModelSection) (CurrentModelCatalogDefaults, error)
}

// CurrentPlatformModelDefaultLoader reads the platform default model: the
// public project's stored default, then the admin vault's. It is optional on
// the defaults loader. Without it a project whose own default no longer
// resolves falls back to the first catalogue model, which was the behaviour
// before #6826.
type CurrentPlatformModelDefaultLoader interface {
	LoadPlatformModelDefault(context.Context, int32, CurrentModelSection) (CurrentModelDefault, error)
}

// CurrentModelCatalogService orchestrates the current configuration rows and
// vault defaults, then delegates response parity to BuildCurrentModelCatalog.
type CurrentModelCatalogService struct {
	candidates CurrentModelCandidateRepository
	defaults   CurrentModelDefaultsLoader
}

func NewCurrentModelCatalogService(
	candidates CurrentModelCandidateRepository,
	defaults CurrentModelDefaultsLoader,
) (*CurrentModelCatalogService, error) {
	if candidates == nil || defaults == nil {
		return nil, errors.New("current model catalog dependencies are required")
	}
	return &CurrentModelCatalogService{candidates: candidates, defaults: defaults}, nil
}

func (s *CurrentModelCatalogService) Get(
	ctx context.Context,
	query CurrentModelCatalogQuery,
) (CurrentModelCatalogResponse, error) {
	if err := validateCurrentModelCatalogQuery(ctx, query); err != nil {
		return CurrentModelCatalogResponse{}, err
	}

	projectItems, err := s.candidates.List(ctx, query.ProjectID, query.Section, false)
	if err != nil {
		return CurrentModelCatalogResponse{}, currentModelCatalogDependencyError(ctx, "list project model configurations", err)
	}
	if err := ctx.Err(); err != nil {
		return CurrentModelCatalogResponse{}, err
	}

	var publicSharedItems []CurrentModelCatalogItem
	if query.IncludeShared && query.ProjectID != query.PublicProjectID {
		publicSharedItems, err = s.candidates.List(ctx, query.PublicProjectID, query.Section, true)
		if err != nil {
			return CurrentModelCatalogResponse{}, currentModelCatalogDependencyError(ctx, "list public model configurations", err)
		}
		if err := ctx.Err(); err != nil {
			return CurrentModelCatalogResponse{}, err
		}
	}

	defaults, err := s.defaults.Load(ctx, query.ProjectID, query.PublicProjectID, query.Section)
	if err != nil {
		return CurrentModelCatalogResponse{}, currentModelCatalogDependencyError(ctx, "load current model defaults", err)
	}
	if err := ctx.Err(); err != nil {
		return CurrentModelCatalogResponse{}, err
	}

	request := CurrentModelCatalogRequest{
		Section:           query.Section,
		ProjectID:         query.ProjectID,
		PublicProjectID:   query.PublicProjectID,
		IncludeShared:     query.IncludeShared,
		ProjectItems:      projectItems,
		PublicSharedItems: publicSharedItems,
		Defaults:          defaults,
	}
	response := BuildCurrentModelCatalog(request)
	platform, ok := s.defaults.(CurrentPlatformModelDefaultLoader)
	if !ok || !currentProjectModelDefaultUnresolved(defaults, response) {
		return response, nil
	}
	// The project's own default names a model that is gone from its
	// catalogue. The platform default is read only now, so a project whose
	// default resolves pays no second vault read.
	request.Defaults.Platform, err = platform.LoadPlatformModelDefault(ctx, query.PublicProjectID, query.Section)
	if err != nil {
		return CurrentModelCatalogResponse{}, currentModelCatalogDependencyError(ctx, "load platform model default", err)
	}
	if err := ctx.Err(); err != nil {
		return CurrentModelCatalogResponse{}, err
	}
	return BuildCurrentModelCatalog(request), nil
}

func validateCurrentModelCatalogQuery(ctx context.Context, query CurrentModelCatalogQuery) error {
	if ctx == nil || query.ProjectID <= 0 || query.PublicProjectID <= 0 || !IsSupportedCurrentModelSection(query.Section) {
		return ErrInvalidCurrentModelCatalogRequest
	}
	return ctx.Err()
}

func currentModelCatalogDependencyError(ctx context.Context, operation string, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%s: %w", operation, err)
}
