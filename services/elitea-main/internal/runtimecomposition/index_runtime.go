package runtimecomposition

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	indexmetaapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexmeta"
	indexscheduleapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexschedule"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/pgvector"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	indexResourceClass  = "indexing"
	indexIsolationClass = "project"
	indexDeadlineTTL    = 24 * time.Hour
)

type currentIndexRuntime struct {
	start          *indexingapp.StartService
	cancel         *indexingapp.CurrentIndexCancellationService
	initializer    *indexingapp.DurableIndexMetaInitializer
	materializer   *storage.CurrentConfigurationsMaterializer
	indexMeta      *indexmetaapp.Service
	indexDelete    *indexmetaapp.DeleteService
	indexConfig    *indexmetaapp.ConfigurationService
	toolkits       indexingapp.CurrentToolkitReader
	settings       indexingapp.CurrentToolkitSettingsValidator
	inputs         *indexingapp.CurrentAuthoritativeInputResolver
	exact          *indexmetaapp.ExactService
	scheduleUpdate *indexscheduleapp.Service
	scheduleDelete *indexscheduleapp.DeleteService
	scheduleAction *currentIndexScheduleDueWork
}

// durableIndexMetaFrozenToolkitClaimer preserves temporary materialization
// failures for retry while making rejected immutable content a permanent
// initialization failure. Without this boundary, an invalid frozen intent
// would retain the active target through an endless retry loop.
type durableIndexMetaFrozenToolkitClaimer struct {
	delegate indexingapp.FrozenToolkitConfigurationClaimer
}

func (c durableIndexMetaFrozenToolkitClaimer) ClaimFrozenToolkitConfiguration(
	ctx context.Context,
	claim indexingapp.FrozenToolkitConfigurationClaim,
) (json.RawMessage, error) {
	content, err := c.delegate.ClaimFrozenToolkitConfiguration(ctx, claim)
	if errors.Is(err, storage.ErrContentRejected) {
		return nil, indexingapp.ErrCurrentIndexMetaInitializationInvalid
	}
	return content, err
}

// newCurrentIndexRuntime composes the current index_data preparation path from
// Configurations-owned records. Provider credentials remain generic
// configuration data; this graph contains no GitHub, OpenAPI, or other toolkit
// credential implementation. The SDK receives the same expanded toolkit and
// Configurations-derived model metadata that the current Python path supplies.
func newCurrentIndexRuntime(
	pool *pgxpool.Pool,
	configurations *CurrentConfigurationsRuntime,
	config Config,
	policy repos.IndexIngestDispatchPolicy,
	indexMetaWriter indexingapp.CurrentIndexMetaWriter,
	reportInitializationFailure func(error),
) (*currentIndexRuntime, error) {
	if pool == nil || configurations == nil || configurations.rows == nil || configurations.scope == nil ||
		configurations.unsecreter == nil || configurations.expander == nil || configurations.models == nil || configurations.vaultLoader == nil ||
		!config.IndexIngestDispatchEnabled || configurations.publicProjectID <= 0 ||
		indexMetaWriter == nil || reportInitializationFailure == nil {
		return nil, errors.New("current index runtime dependencies are required")
	}
	vaults := configurations.vaultLoader

	builtInSchemas, err := LoadPinnedCurrentToolkitSchemaSnapshot()
	if err != nil {
		return nil, fmt.Errorf("load current SDK toolkit schema snapshot: %w", err)
	}
	schemas, err := NewCurrentCompositeToolkitSchemaCatalog(
		builtInSchemas,
		UnavailableCurrentActorVisibleToolkitSchemas{},
	)
	if err != nil {
		return nil, err
	}
	names, err := NewCurrentBuiltInToolkitNameDeriver(builtInSchemas)
	if err != nil {
		return nil, err
	}

	toolkitRows, err := repos.NewCurrentToolkitsRepository(pool)
	if err != nil {
		return nil, fmt.Errorf("construct current toolkit repository: %w", err)
	}
	toolkits, err := NewCurrentToolkitReaderAdapter(toolkitRows, names)
	if err != nil {
		return nil, err
	}
	nestedToolkits, err := NewCurrentNestedToolkitReaderAdapter(toolkitRows, names)
	if err != nil {
		return nil, err
	}

	models := configurations.models
	modelVisibility, err := NewCurrentModelVisibilityAdapter(models, configurations.publicProjectID)
	if err != nil {
		return nil, err
	}
	settings, err := configurationapp.NewCurrentToolkitSettingsResolver(
		schemas,
		nestedToolkits,
		configurations.expander,
		modelVisibility,
		configurations.unsecreter,
	)
	if err != nil {
		return nil, err
	}
	// The embedding binding is resolved entirely from the Configurations rows
	// this graph already owns. It used to additionally require a LiteLLM
	// administration client to ask whether a `{projectID}_{model}` group had
	// been pushed into that proxy's registry; the Bifrost gateway pulls the same
	// rows at request time, so the index plane no longer depends on any LLM
	// facade being composed.
	embeddings, err := indexingapp.NewCurrentEmbeddingBindingResolver(
		configurations.rows,
		configurations.publicProjectID,
	)
	if err != nil {
		return nil, err
	}
	inputs, err := indexingapp.NewCurrentAuthoritativeInputResolver(
		toolkits,
		models,
		settings,
		embeddings,
		configurations.publicProjectID,
	)
	if err != nil {
		return nil, err
	}

	bundleFactory, err := indexingapp.NewInputBundleFactory(indexingapp.InputProfile{
		Classification:        inputClassification,
		RequiredGrantAudience: inputGrantAudience,
	}, currentRuntimeID)
	if err != nil {
		return nil, err
	}
	jobs, err := repos.NewIndexIngestJobsRepository(pool, policy)
	if err != nil {
		return nil, fmt.Errorf("construct current index admission repository: %w", err)
	}
	cancellations, err := repos.NewCurrentIndexCancellationRepository(pool)
	if err != nil {
		return nil, fmt.Errorf("construct current index cancellation repository: %w", err)
	}
	cancel, err := indexingapp.NewCurrentIndexCancellationService(cancellations)
	if err != nil {
		return nil, fmt.Errorf("construct current index cancellation service: %w", err)
	}
	admissions, err := indexingapp.NewAdmissionService(jobs, bundleFactory, nil, currentRuntimeID)
	if err != nil {
		return nil, err
	}
	materializer, err := storage.NewCurrentConfigurationsMaterializer(configurations.unsecreter)
	if err != nil {
		return nil, err
	}
	toolkitClaimer, err := newCurrentFrozenToolkitConfigurationClaimer(materializer)
	if err != nil {
		return nil, err
	}
	indexMetaInitializer, err := indexingapp.NewCurrentIndexMetaInitializer(
		durableIndexMetaFrozenToolkitClaimer{delegate: toolkitClaimer},
		indexMetaWriter,
	)
	if err != nil {
		return nil, err
	}
	initializationConcurrency := min(int(pool.Config().MaxConns), 4)
	if initializationConcurrency <= 0 {
		return nil, errors.New(
			"current index metadata initialization pool capacity is invalid",
		)
	}
	durableInitializer, err := indexingapp.NewDurableIndexMetaInitializer(
		jobs,
		indexMetaInitializer,
		currentRuntimeID,
		indexingapp.IndexMetaInitializationReconcilerConfig{
			PollInterval:  500 * time.Millisecond,
			ClaimLease:    2 * time.Minute,
			BatchSize:     2 * initializationConcurrency,
			MaxConcurrent: initializationConcurrency,
			ReportFailure: reportInitializationFailure,
		},
	)
	if err != nil {
		return nil, err
	}
	initializedAdmissions, err := indexingapp.NewInitializingAdmissionSubmitter(
		admissions,
		durableInitializer,
	)
	if err != nil {
		return nil, err
	}
	// HTTP success now follows both durable admission and the committed
	// project-PgVector metadata effect. The outbox remains invisible until the
	// exact initialization transition succeeds.
	start, err := indexingapp.NewStartService(inputs, initializedAdmissions, currentRuntimeID)
	if err != nil {
		return nil, err
	}
	indexMetaTimeouts, err := storage.NewCurrentIndexMetaTimeoutResolver(vaults)
	if err != nil {
		return nil, err
	}
	indexMetaReader := pgvector.NewCurrentIndexMetaReader()
	indexMeta, err := indexmetaapp.NewService(
		toolkits,
		settings,
		indexMetaTimeouts,
		indexMetaReader,
	)
	if err != nil {
		return nil, err
	}
	exact, err := indexmetaapp.NewExactService(
		toolkits,
		settings,
		indexMetaReader,
	)
	if err != nil {
		return nil, err
	}
	indexDelete, err := newConfiguredCurrentIndexMetaDeleteService(
		config.IndexIngestDispatchEnabled,
		pool,
		toolkits,
		settings,
	)
	if err != nil {
		return nil, err
	}

	indexConfig, err := indexmetaapp.NewConfigurationService(
		toolkits,
		settings,
		pgvector.NewCurrentIndexConfigurationWriter(),
	)
	if err != nil {
		return nil, fmt.Errorf("construct current index configuration service: %w", err)
	}

	return &currentIndexRuntime{
		start:        start,
		cancel:       cancel,
		initializer:  durableInitializer,
		materializer: materializer,
		indexMeta:    indexMeta,
		indexDelete:  indexDelete,
		indexConfig:  indexConfig,
		toolkits:     toolkits,
		settings:     settings,
		inputs:       inputs,
		exact:        exact,
	}, nil
}

func newConfiguredCurrentIndexMetaDeleteService(
	indexDispatchEnabled bool,
	pool *pgxpool.Pool,
	toolkits indexingapp.CurrentToolkitReader,
	settings indexingapp.CurrentToolkitSettingsValidator,
) (*indexmetaapp.DeleteService, error) {
	if !indexDispatchEnabled {
		return nil, nil
	}
	if pool == nil || toolkits == nil || settings == nil {
		return nil, errors.New(
			"current index metadata delete composition dependencies are required",
		)
	}
	schedules, err := repos.NewCurrentIndexMetaScheduleRepository(pool)
	if err != nil {
		return nil, fmt.Errorf(
			"construct current index metadata schedule repository: %w",
			err,
		)
	}
	service, err := indexmetaapp.NewDeleteService(
		toolkits,
		settings,
		pgvector.NewCurrentIndexMetaRemover(),
		schedules,
	)
	if err != nil {
		return nil, fmt.Errorf("construct current index metadata delete service: %w", err)
	}
	return service, nil
}

func currentRuntimeID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

var _ executionapp.IDGenerator = currentRuntimeID
