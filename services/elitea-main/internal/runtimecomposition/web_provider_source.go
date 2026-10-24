package runtimecomposition

import (
	"context"
	"errors"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/toolkitcatalogue"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/jackc/pgx/v5/pgxpool"
)

// currentWebProviderSource is what the web and host_intel toolkits resolve
// their providers from (#1238): the admin web_search section, configuration
// rows by type, the public project, and the toolkit-type catalogue.
type currentWebProviderSource struct {
	pool            *pgxpool.Pool
	rows            *repos.CurrentConfigurationsRepository
	types           *toolkitcatalogue.Store
	publicProjectID int32
}

func newCurrentWebProviderSource(
	pool *pgxpool.Pool,
	configurations *CurrentConfigurationsRuntime,
) (*currentWebProviderSource, error) {
	if pool == nil || configurations == nil || configurations.rows == nil || configurations.publicProjectID <= 0 {
		return nil, errors.New("current web provider source dependencies are required")
	}
	return &currentWebProviderSource{
		pool:            pool,
		rows:            configurations.rows,
		types:           toolkitcatalogue.NewStore(pool),
		publicProjectID: configurations.publicProjectID,
	}, nil
}

func (s *currentWebProviderSource) WebProviderPolicy(ctx context.Context) (configurationapp.WebProviderPolicy, error) {
	settings, err := platformconfig.LoadWebSearchSettings(ctx, s.pool)
	if err != nil {
		return configurationapp.WebProviderPolicy{}, err
	}
	return configurationapp.WebProviderPolicy{
		EnabledProviders:      settings.EnabledProviders,
		DirectFetchEnabled:    settings.DirectFetchEnabled,
		DefaultSearchProvider: settings.DefaultSearchProvider,
		FetchAllowDomains:     settings.FetchAllowDomains,
		FetchDenyDomains:      settings.FetchDenyDomains,
		AllowHTTP:             settings.AllowHTTP,
		MaxResultsCap:         settings.MaxResultsCap,
		HostMaxResultsCap:     settings.HostMaxResultsCap,
	}, nil
}

func (s *currentWebProviderSource) FindConfigurationByType(
	ctx context.Context,
	projectID int32,
	configurationType string,
	sharedOnly bool,
) (configurationapp.CurrentExpansionConfiguration, bool, error) {
	return s.rows.FindByType(ctx, projectID, configurationType, sharedOnly)
}

func (s *currentWebProviderSource) PublicProjectID(context.Context) (int32, error) {
	return s.publicProjectID, nil
}

func (s *currentWebProviderSource) ToolkitTypeAllowed(ctx context.Context, projectID int32, toolkitType string) (bool, error) {
	filter, err := s.types.ProjectFilter(ctx, int64(projectID))
	if err != nil {
		return false, err
	}
	return filter.Allows(toolkitType), nil
}
