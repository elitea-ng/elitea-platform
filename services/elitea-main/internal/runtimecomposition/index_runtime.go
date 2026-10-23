package runtimecomposition

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	indexingapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/indexing"
	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	indexingapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexing"
	indexmetaapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexmeta"
	indexregistryapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexregistry"
	indexscheduleapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexschedule"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	indexResourceClass  = "indexing"
	indexIsolationClass = "project"
	indexDeadlineTTL    = 24 * time.Hour
)

// currentIndexExactFinder is the one-index read the schedule inspector makes.
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
	// indexMeta, indexDelete, indexConfig and exact are the registry service.
	// The routes and the schedule kernel see only these interfaces.
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
	registry *indexRegistryComposition,
	reportInitializationFailure func(error),
) (*currentIndexRuntime, error) {
	// The index registry is the one metadata store of the runtime (the Rust
	// indexing runtime is the only one since the Python worker was removed).
	if registry == nil {
		return nil, errors.New("current index runtime needs the composed index registry")
	}
	if pool == nil || configurations == nil || configurations.rows == nil || configurations.scope == nil ||
		configurations.unsecreter == nil || configurations.expander == nil || configurations.models == nil || configurations.vaultLoader == nil ||
		!config.IndexIngestDispatchEnabled || configurations.publicProjectID <= 0 ||
		reportInitializationFailure == nil {
		return nil, errors.New("current index runtime dependencies are required")
	}

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
	// The registry row needs no frozen toolkit and no pgvector connection
	// string: it is addressed by the admission identity alone.
	indexMetaInitializer, err := indexingapp.NewRegistryIndexMetaInitializer(registry.repo)
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
	// registry metadata effect. The outbox remains invisible until the
	// exact initialization transition succeeds.
	start, err := indexingapp.NewStartService(inputs, initializedAdmissions, currentRuntimeID)
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
	// The one registry Service of the deployment, which the worker's
	// remove_index runs through as well (review round 2).
	service := registry.service
	if service == nil {
		return nil, errors.New("current index runtime needs the composed index registry service")
	}
	runtime.indexMeta, runtime.indexDelete, runtime.indexConfig, runtime.exact =
		service, service, service, service
	return runtime, nil
}

// indexRegistryComposition is what the rust indexing runtime composes the
// registry from. It is nil while index ingest dispatch is off.
type indexRegistryComposition struct {
	repo *repos.IndexRegistryRepository
	// vectors is the hook to elitea-vector's Delete (ADR-0031 decision 4).
	vectors indexingapp.IndexVectorDeleter
	// report receives failures that must not fail the user's request, such as a
	// tombstone that could not be purged.
	report func(error)
	// service is THE registry Service of the deployment (review round 2):
	// the index REST routes, the runtime's index_data path and the worker's
	// remove_index (the claim remover) all run the same one. Built once by
	// newIndexRegistryService in the composition root.
	service *indexregistryapp.Service
}

func currentRuntimeID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

var _ executionapp.IDGenerator = currentRuntimeID
