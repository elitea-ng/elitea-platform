package runtimecomposition

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	indexingapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/indexing"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	indexmetaapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexmeta"
	indexregistryapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexregistry"
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

// currentIndexExactFinder is the one-index read the schedule inspector makes.
// indexmetaapp.ExactService answers it from the project's pgvector database;
// indexregistry.Service answers it from the registry.
type currentIndexExactFinder interface {
	FindSnapshot(
		context.Context,
		indexmetaapp.Request,
		string,
		indexingapp.CurrentToolkitSnapshot,
	) (indexmetaapp.Item, bool, error)
}

type currentIndexRuntime struct {
	start        *indexingapp.StartService
	cancel       *indexingapp.CurrentIndexCancellationService
	initializer  *indexingapp.DurableIndexMetaInitializer
	materializer *storage.CurrentConfigurationsMaterializer
	// indexMeta, indexDelete, indexConfig and exact are the python path's
	// pgvector-backed services, or the registry service in rust mode. The
	// routes and the schedule kernel see only these interfaces.
	indexMeta      indexingapi.CurrentIndexMetaReader
	indexDelete    indexingapi.CurrentIndexMetaDeleter
	indexConfig    indexingapi.CurrentIndexConfigurationSaver
	toolkits       indexingapp.CurrentToolkitReader
	settings       indexingapp.CurrentToolkitSettingsValidator
	inputs         *indexingapp.CurrentAuthoritativeInputResolver
	exact          currentIndexExactFinder
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
	registry *indexRegistryComposition,
	reportInitializationFailure func(error),
) (*currentIndexRuntime, error) {
	// ELITEA_INDEXING_RUNTIME picks exactly one metadata store for the whole
	// runtime: the project's pgvector index_meta rows (python, the default) or
	// the registry (rust). Each mode requires its own dependency and refuses the
	// other's, so a half-composed runtime cannot start.
	rustIndexing := config.IndexingRuntime == IndexingRuntimeRust
	if (rustIndexing && (registry == nil || indexMetaWriter != nil)) ||
		(!rustIndexing && (indexMetaWriter == nil || registry != nil)) {
		return nil, errors.New("current index runtime metadata store does not match the indexing runtime")
	}
	if pool == nil || configurations == nil || configurations.rows == nil || configurations.scope == nil ||
		configurations.unsecreter == nil || configurations.expander == nil || configurations.models == nil || configurations.vaultLoader == nil ||
		!config.IndexIngestDispatchEnabled || configurations.publicProjectID <= 0 ||
		reportInitializationFailure == nil {
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
	var indexMetaInitializer indexingapp.IndexMetaMaterializer
	if rustIndexing {
		// The registry row needs no frozen toolkit and no pgvector connection
		// string: it is addressed by the admission identity alone.
		indexMetaInitializer, err = indexingapp.NewRegistryIndexMetaInitializer(registry.repo)
	} else {
		var toolkitClaimer *currentFrozenToolkitConfigurationClaimer
		toolkitClaimer, err = newCurrentFrozenToolkitConfigurationClaimer(materializer)
		if err != nil {
			return nil, err
		}
		indexMetaInitializer, err = indexingapp.NewCurrentIndexMetaInitializer(
			durableIndexMetaFrozenToolkitClaimer{delegate: toolkitClaimer},
			indexMetaWriter,
		)
	}
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
	runtime := &currentIndexRuntime{
		start:        start,
		cancel:       cancel,
		initializer:  durableInitializer,
		materializer: materializer,
		toolkits:     toolkits,
		settings:     settings,
		inputs:       inputs,
	}
	if rustIndexing {
		schedules, err := repos.NewCurrentIndexMetaScheduleRepository(pool)
		if err != nil {
			return nil, fmt.Errorf("construct current index metadata schedule repository: %w", err)
		}
		service, err := indexregistryapp.NewService(
			toolkits,
			registry.repo,
			schedules,
			registry.vectors,
			registry.report,
		)
		if err != nil {
			return nil, fmt.Errorf("construct index registry service: %w", err)
		}
		runtime.indexMeta, runtime.indexDelete, runtime.indexConfig, runtime.exact =
			service, service, service, service
		return runtime, nil
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

	runtime.indexMeta, runtime.indexDelete, runtime.indexConfig, runtime.exact =
		indexMeta, indexDelete, indexConfig, exact
	return runtime, nil
}

// indexRegistryComposition is what the rust indexing runtime composes the
// registry from. It is nil in python mode.
type indexRegistryComposition struct {
	repo *repos.IndexRegistryRepository
	// vectors is the hook to elitea-vector's Delete (ADR-0031 decision 4).
	vectors indexingapp.IndexVectorDeleter
	// report receives failures that must not fail the user's request, such as a
	// tombstone that could not be purged.
	report func(error)
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
