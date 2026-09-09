package runtimecomposition

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	executionapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/executions"
	indexingapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/indexing"
	secretsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	toolkitrun "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkitrun"
	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	indexscheduleapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexschedule"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/pipelineruns"
	schedulingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/scheduling"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/currentcore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/pgvector"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/redisdispatch"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc/control"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc/output"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/workloadauth"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

const (
	protocolRevision       = "elitea.runtime.v1"
	envelopeSchemaRevision = "elitea.runtime.signed-worker-command.v1"
	outputSchemaRevision   = "elitea.runtime.execution-output.v1"
	capabilityVersion      = "1"
	indexCapabilityVersion = "2"
	agentCapabilityVersion = "1"
	limitsRevision         = "elitea.runtime.limits.conformance.v1"

	resourceClass          = "validation-small"
	isolationClass         = "shared-claim-scoped-authority"
	inputClassification    = "tenant-confidential"
	inputGrantAudience     = "elitea.runtime.input.read.v1"
	indexArtifactMediaType = "application/json"

	maxWorkerCommandBytes         = 32 * 1024
	maxSignedEnvelopeBytes        = 48 * 1024
	maxRedisFieldBytes            = 48 * 1024
	maxInputManifestBytes         = 64 * 1024
	maxInputEntries               = 16
	maxInputContentBytes          = executiondomain.MaxAgentExecutionInputBytes
	maxOutputFrameBytes           = 64 * 1024
	maxSafeStringBytes            = 256
	maxGRPCRequestBytes           = 64 * 1024
	maxGRPCResponseBytes          = 80 * 1024
	maxContentRequests            = 16
	maxIndexArtifactBytes         = 1 * 1024 * 1024
	claimLeaseTTL                 = 30 * time.Second
	productionIndexRedisEntrySize = (64 * 1024) - 1
	agentResourceClass            = "agents"
	agentIsolationClass           = "project"
	agentDeadlineTTL              = 24 * time.Hour
)

type Dependencies struct {
	AdmissionPool                    *pgxpool.Pool
	ControlPool                      *pgxpool.Pool
	OutputPool                       *pgxpool.Pool
	ReplayPool                       *pgxpool.Pool
	TerminalEffectsPool              *pgxpool.Pool
	ContentPool                      *pgxpool.Pool
	CurrentConfigurations            *CurrentConfigurationsRuntime
	ConfigurationLifecycleReconciler configurationapp.CurrentConfigurationLifecycleReconciler
	ActorTokenIssuer                 storage.ActorTokenIssuer
	ProjectTokenValidator            storage.ProjectTokenValidator
	ProjectSystemTokenSource         ProjectSystemTokenSource
	PermissionResolver               auth.PermissionResolver
	Logger                           *slog.Logger

	// ObjectStore backs the artifact retention sweeper (S14). A nil store
	// leaves current index scheduling enabled but omits that separate
	// capability from the shared scheduler registry.
	ObjectStore storage.ObjectStore

	// ToolkitCatalogue and WorkerToolkitCapability are the SAME two objects
	// internal/api/v2/toolkits already holds, threaded in so a tool run and the
	// type catalogue answer "can this deployment run this type" from one place.
	// Both nil is the reading of absence the catalogue itself applies: every
	// type is runnable, as it was before the projection existed.
	ToolkitCatalogue        *CurrentToolkitCatalogueSnapshot
	WorkerToolkitCapability *WorkerToolkitCapability

	// PipelineRuns and DomainEvents together wire pipeline.run.succeeded/
	// failed — the two catalogue events #876 declared but left unwired
	// because the claim-fence/settlement engine below carries no notion of
	// "this execution is a pipeline run". Both nil (the default) leaves
	// SettlementService and RuntimeFailureService composed exactly as they
	// were before this pair of fields existed — no hook, no observer.
	//
	// PipelineRuns is public.pipeline_runs' repository
	// (migrations/shared/0124) — main.go builds ONE instance, shared with
	// internal/api/v2/pipelinetriggers' WithRunTracker, the same "two halves
	// must agree" reason WebhookDispatcher/DomainEvents in
	// internal/api.RouterConfig are each built once. See
	// internal/application/pipelineruns' package doc for the full sequencing
	// story.
	PipelineRuns pipelineruns.Tracker
	// DomainEvents is the SAME *events.Publisher instance
	// cmd/elitea-main/main.go passes to internal/api.RouterConfig — declared
	// here as the narrow EventEmitter seam (structurally satisfied, no
	// internal/events import needed in this package) rather than the
	// concrete type, the same "declared locally" shape
	// pipelinetriggers.EventEmitter uses.
	DomainEvents pipelineruns.EventEmitter
}

func New(ctx context.Context, config Config, dependencies Dependencies) (*Runtime, error) {
	if ctx == nil {
		return nil, errors.New("runtime composition context is required")
	}
	if !config.Enabled {
		return nil, errors.New("runtime composition is disabled")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if err := validateDependencies(dependencies); err != nil {
		return nil, err
	}
	if (config.IndexIngestDispatchEnabled || config.AgentExecutionDispatchEnabled) &&
		(dependencies.ActorTokenIssuer == nil || dependencies.ProjectTokenValidator == nil) {
		return nil, errors.New("runtime worker actor-token bridge is required")
	}
	if config.IndexSchedulingEnabled &&
		dependencies.ProjectSystemTokenSource == nil {
		return nil, errors.New(
			"runtime index scheduling project-system token source is required",
		)
	}
	// Index ingest needs the Configurations runtime and nothing else on the LLM
	// side: the embedding binding is resolved from those same configuration rows
	// the Bifrost gateway reads. It used to also require an LLM facade, which
	// gated the whole index plane on ELITEA_LITELLM_BASE_URL being set.
	if config.IndexIngestDispatchEnabled && dependencies.CurrentConfigurations == nil {
		return nil, errors.New("runtime index ingest current Configurations runtime is required")
	}
	if dependencies.ConfigurationLifecycleReconciler != nil && dependencies.CurrentConfigurations == nil {
		return nil, errors.New("configuration lifecycle requires current Configurations runtime")
	}
	if err := migrate.New(dependencies.AdmissionPool, platformmigrations.Files).CheckHead(ctx, migrate.ScopeShared, "platform"); err != nil {
		return nil, fmt.Errorf("runtime shared migration head is not applied: %w", err)
	}

	privateKey, err := loadEd25519PrivateKey(config.SigningKeyFile)
	if err != nil {
		return nil, err
	}
	verificationKeys, err := loadEd25519VerificationKeyring(config.VerificationKeyringFile)
	if err != nil {
		return nil, err
	}
	if err := verificationKeys.requireActiveSigningKey(config.SigningKeyID, privateKey); err != nil {
		return nil, err
	}
	controlTLS, err := runtimegrpc.LoadServerTLSConfig(config.ControlTLS)
	if err != nil {
		return nil, fmt.Errorf("load control listener TLS: %w", err)
	}
	outputTLS, err := runtimegrpc.LoadServerTLSConfig(config.OutputTLS)
	if err != nil {
		return nil, fmt.Errorf("load output listener TLS: %w", err)
	}
	contentTLS, err := runtimegrpc.LoadServerTLSConfig(config.ContentTLS)
	if err != nil {
		return nil, fmt.Errorf("load content listener TLS: %w", err)
	}

	controlRedis, err := NewControlRedisClient(ctx, config)
	if err != nil {
		return nil, err
	}
	closeRedis := true
	defer func() {
		if closeRedis {
			_ = controlRedis.Close()
		}
	}()

	limits := redisdispatch.Limits{
		Revision:               limitsRevision,
		MaxWorkerCommandBytes:  maxWorkerCommandBytes,
		MaxSignedEnvelopeBytes: maxSignedEnvelopeBytes,
		MaxRedisFieldBytes:     maxRedisFieldBytes,
		MaxRedisEntryBytes:     productionRedisEntrySize,
		MaxSignatureBytes:      256,
		MaxStringBytes:         maxSafeStringBytes,
	}
	appenderConfig := redisdispatch.RedisStreamAppenderConfig{
		MaxEntries:    config.StreamMaxEntries,
		MaxEntryBytes: productionRedisEntrySize,
	}
	if appenderConfig.MaxEntryBytes > limits.MaxRedisEntryBytes {
		return nil, errors.New("runtime Redis appender entry bound exceeds the producer bound")
	}
	appender, err := redisdispatch.NewRedisStreamAppender(controlRedis, appenderConfig)
	if err != nil {
		return nil, fmt.Errorf("construct bounded runtime Redis appender: %w", err)
	}
	signer, err := redisdispatch.NewEd25519CommandSigner(config.SigningKeyID, privateKey)
	if err != nil {
		return nil, fmt.Errorf("construct runtime command signer: %w", err)
	}
	producer, err := redisdispatch.NewProducer(redisdispatch.ProducerConfig{
		Stream:                 config.CommandStream,
		ProtocolRevision:       protocolRevision,
		EnvelopeSchemaRevision: envelopeSchemaRevision,
		Limits:                 limits,
	}, signer, appender)
	if err != nil {
		return nil, fmt.Errorf("construct runtime command producer: %w", err)
	}

	dispatchPolicy := repos.ValidationDispatchPolicy{
		StreamName:        config.CommandStream,
		CapabilityVersion: capabilityVersion,
		ResourceClass:     resourceClass,
		IsolationClass:    isolationClass,
		Priority:          1,
		DeadlineTTL:       time.Minute,
		LimitsRevision:    limitsRevision,
		MaxOutstanding:    config.MaxOutstanding,
	}
	indexDispatchPolicy := repos.IndexIngestDispatchPolicy{
		StreamName:        config.IndexIngestCommandStream,
		CapabilityVersion: indexCapabilityVersion,
		ResourceClass:     indexResourceClass,
		IsolationClass:    indexIsolationClass,
		Priority:          1,
		DeadlineTTL:       indexDeadlineTTL,
		LimitsRevision:    limitsRevision,
		MaxOutstanding:    config.MaxOutstanding,
	}
	toolkitCallToolDispatchPolicy := repos.ToolkitCallToolDispatchPolicy{
		StreamName:        config.IndexIngestCommandStream,
		CapabilityVersion: toolkitCallToolCapabilityVersion,
		ResourceClass:     toolkitCallToolResourceClass,
		IsolationClass:    toolkitCallToolIsolationClass,
		Priority:          1,
		DeadlineTTL:       toolkitCallToolDeadlineTTL,
		LimitsRevision:    limitsRevision,
		MaxOutstanding:    config.MaxOutstanding,
	}
	agentDispatchPolicy := repos.AgentExecutionDispatchPolicy{
		StreamName:        config.AgentExecutionCommandStream,
		CapabilityVersion: agentCapabilityVersion,
		ResourceClass:     agentResourceClass,
		IsolationClass:    agentIsolationClass,
		Priority:          1,
		DeadlineTTL:       agentDeadlineTTL,
		LimitsRevision:    limitsRevision,
		MaxOutstanding:    config.MaxOutstanding,
	}
	jobs, err := repos.NewExecutionJobsRepository(dependencies.AdmissionPool, dispatchPolicy)
	if err != nil {
		return nil, fmt.Errorf("construct runtime execution jobs: %w", err)
	}
	targets, err := repos.NewConfigurationTargetsRepository(dependencies.AdmissionPool)
	if err != nil {
		return nil, fmt.Errorf("construct runtime configuration targets: %w", err)
	}
	bundles, err := executionapp.NewValidationInputBundleFactory(executionapp.ValidationInputProfile{
		SemanticRole:          "configuration.settings",
		Classification:        inputClassification,
		RequiredGrantAudience: inputGrantAudience,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("construct runtime input bundle factory: %w", err)
	}
	jobSubmitter, err := executionapp.NewSubmitJobService(jobs, nil, nil)
	if err != nil {
		return nil, err
	}
	var currentSDKConfigurationValidator configurationapp.CurrentSDKConfigurationValidator
	if dependencies.CurrentConfigurations != nil {
		validationCandidates, candidateErr := repos.NewCurrentSDKValidationCandidatesRepository(dependencies.AdmissionPool)
		if candidateErr != nil {
			return nil, fmt.Errorf("construct current SDK validation candidates: %w", candidateErr)
		}
		currentSDKConfigurationValidator, candidateErr = newCurrentSDKConfigurationValidator(
			dependencies.CurrentConfigurations.AvailableCatalog(),
			validationCandidates,
			bundles,
			jobSubmitter,
			currentRuntimeID,
		)
		if candidateErr != nil {
			return nil, fmt.Errorf("construct current SDK configuration validator: %w", candidateErr)
		}
	}
	validationSubmitter, err := configurationapp.NewSubmitValidationService(targets, bundles, jobSubmitter)
	if err != nil {
		return nil, err
	}

	var indexMetaTerminalEffect *currentIndexMetaTerminalProcessor
	var indexManualStopCleanupEffect *currentIndexManualStopCleanupProcessor
	var currentIndexMetaWriter *pgvector.CurrentIndexMetaWriter
	if config.IndexIngestDispatchEnabled {
		currentIndexMetaWriter = pgvector.NewCurrentIndexMetaWriter()
		indexMetaTerminalEffect, err = newCurrentIndexMetaTerminalProcessor(
			dependencies.TerminalEffectsPool,
			dependencies.CurrentConfigurations,
			currentIndexMetaWriter,
			func(err error) {
				dependencies.Logger.Error(
					"current index metadata terminal item requeued",
					"err",
					err,
				)
			},
		)
		if err != nil {
			return nil, fmt.Errorf("construct current index metadata terminal effect: %w", err)
		}
		indexManualStopCleanupEffect, err =
			newCurrentIndexManualStopCleanupProcessor(
				dependencies.TerminalEffectsPool,
				dependencies.CurrentConfigurations,
				currentIndexMetaWriter,
				func(err error) {
					dependencies.Logger.Error(
						"current index manual Stop cleanup item requeued",
						"err",
						err,
					)
				},
			)
		if err != nil {
			return nil, fmt.Errorf(
				"construct current index manual Stop cleanup effect: %w",
				err,
			)
		}
	}
	indexMetaTaskRestampReconciler, err :=
		newConfiguredCurrentIndexMetaTaskRestampReconciler(
			config.IndexIngestDispatchEnabled,
			dependencies.TerminalEffectsPool,
			dependencies.CurrentConfigurations,
			currentIndexMetaWriter,
			func(err error) {
				dependencies.Logger.Error(
					"current index metadata task restamp item requeued",
					"err",
					err,
				)
			},
			func(err error) {
				dependencies.Logger.Error(
					"current index metadata task restamp reconciliation failed",
					"err",
					err,
				)
			},
		)
	if err != nil {
		return nil, fmt.Errorf(
			"construct current index metadata task restamp reconciler: %w",
			err,
		)
	}

	outbox, err := repos.NewCommandOutboxRepository(dependencies.AdmissionPool, config.CommandStream)
	if err != nil {
		return nil, fmt.Errorf("construct runtime command outbox: %w", err)
	}
	dispatcher, err := executionapp.NewValidationDispatcher(outbox, producer)
	if err != nil {
		return nil, err
	}
	publisher, err := executionapp.NewOutboxPublisher(outbox, dispatcher, executionapp.OutboxPublisherConfig{
		PollInterval:      250 * time.Millisecond,
		VisibilityTimeout: 30 * time.Second,
		BatchSize:         64,
		MaxConcurrent:     8,
		ReportFailure: func(err error) {
			dependencies.Logger.Error("runtime outbox publisher cycle failed", "err", err)
		},
	})
	if err != nil {
		return nil, err
	}
	var indexPublisher publisherRunner
	var toolkitCallToolProducer *redisdispatch.ToolkitCallToolProducer
	if config.IndexIngestDispatchEnabled {
		indexLimits := limits
		indexLimits.MaxRedisEntryBytes = productionIndexRedisEntrySize
		indexAppender, err := redisdispatch.NewRedisStreamAppender(controlRedis, redisdispatch.RedisStreamAppenderConfig{
			MaxEntries:    config.IndexIngestStreamMaxEntries,
			MaxEntryBytes: productionIndexRedisEntrySize,
		})
		if err != nil {
			return nil, fmt.Errorf("construct bounded index ingest Redis appender: %w", err)
		}
		indexProducer, err := redisdispatch.NewIndexIngestProducer(redisdispatch.IndexIngestProducerConfig{
			Stream:                 config.IndexIngestCommandStream,
			ConsumerGroup:          config.IndexIngestConsumerGroup,
			ValidationStream:       config.CommandStream,
			ProtocolRevision:       protocolRevision,
			EnvelopeSchemaRevision: envelopeSchemaRevision,
			CapabilityVersion:      indexCapabilityVersion,
			Limits:                 indexLimits,
		}, signer, indexAppender)
		if err != nil {
			return nil, fmt.Errorf("construct index ingest Redis producer: %w", err)
		}
		indexOutbox, err := repos.NewCommandOutboxRepository(dependencies.AdmissionPool, config.IndexIngestCommandStream)
		if err != nil {
			return nil, fmt.Errorf("construct index ingest command outbox: %w", err)
		}
		indexDispatcher, err := indexingapp.NewIndexIngestDispatcher(indexOutbox, indexProducer)
		if err != nil {
			return nil, err
		}
		indexPublisher, err = indexingapp.NewIndexIngestOutboxPublisher(indexOutbox, indexDispatcher, executionapp.OutboxPublisherConfig{
			PollInterval:      250 * time.Millisecond,
			VisibilityTimeout: 30 * time.Second,
			BatchSize:         64,
			MaxConcurrent:     8,
			ReportFailure: func(err error) {
				dependencies.Logger.Error("index ingest outbox publisher cycle failed", "err", err)
			},
		})
		if err != nil {
			return nil, err
		}
		// The tool-run producer, on the SAME stream and the same appender. The
		// worker dispatches by capability_id from one Redis stream
		// (serve.py:781-813), so a second stream would need a second worker to
		// consume it and would deliver nothing on every deployment there is.
		//
		// There is NO outbox publisher beside it. A tool run is dispatched
		// inline by the request that admitted it — see
		// internal/application/toolkitcalltool/doc.go — so a background poller
		// would have nothing to publish.
		toolkitCallToolProducer, err = redisdispatch.NewToolkitCallToolProducer(
			redisdispatch.ToolkitCallToolProducerConfig{
				Stream:                 config.IndexIngestCommandStream,
				ConsumerGroup:          config.IndexIngestConsumerGroup,
				ValidationStream:       config.CommandStream,
				ProtocolRevision:       protocolRevision,
				EnvelopeSchemaRevision: envelopeSchemaRevision,
				CapabilityVersion:      toolkitCallToolCapabilityVersion,
				Limits:                 indexLimits,
			}, signer, indexAppender)
		if err != nil {
			return nil, fmt.Errorf("construct tool-run Redis producer: %w", err)
		}
	}
	var agentJobs *repos.AgentExecutionJobsRepository
	var agentStart *agentexecutionapp.CurrentApplicationStartService
	var agentCancel *agentexecutionapp.CurrentAgentCancellationService
	var agentTaskStatus *agentexecutionapp.CurrentAgentTaskStatusService
	var agentPublisher publisherRunner
	var agentMaterializer *storage.CurrentConfigurationsMaterializer
	// Both are built inside the agent-execution block below but consumed with
	// the private content listener further down, where the claim authorizer
	// they have to be combined with is constructed.
	var agentFreezer agentexecutionapp.CurrentApplicationVersionFreezer
	var agentNestedVersions storage.CurrentApplicationVersionSource
	if config.AgentExecutionDispatchEnabled {
		agentLimits := limits
		agentLimits.MaxRedisEntryBytes = productionIndexRedisEntrySize
		agentAppender, err := redisdispatch.NewRedisStreamAppender(controlRedis, redisdispatch.RedisStreamAppenderConfig{
			MaxEntries:    config.AgentExecutionStreamMaxEntries,
			MaxEntryBytes: productionIndexRedisEntrySize,
		})
		if err != nil {
			return nil, fmt.Errorf("construct bounded agent execution Redis appender: %w", err)
		}
		agentProducer, err := redisdispatch.NewAgentExecutionProducer(
			redisdispatch.AgentExecutionProducerConfig{
				Stream:                       config.AgentExecutionCommandStream,
				ConsumerGroup:                config.AgentExecutionConsumerGroup,
				ValidationStream:             config.CommandStream,
				IndexIngestStream:            config.IndexIngestCommandStream,
				ProtocolRevision:             protocolRevision,
				EnvelopeSchemaRevision:       envelopeSchemaRevision,
				ApplicationCapabilityVersion: agentCapabilityVersion,
				AdhocCapabilityVersion:       agentCapabilityVersion,
				Limits:                       agentLimits,
			},
			signer,
			agentAppender,
		)
		if err != nil {
			return nil, fmt.Errorf("construct agent execution Redis producer: %w", err)
		}
		// The catalogue project is the SAME id the freezer resolves shared
		// models against, taken from the same place, so the one project a turn
		// may borrow a published agent from and the one project it may borrow a
		// shared model from cannot drift apart. Both the resolve and the turn
		// INSERT read it, and both are built here.
		if dependencies.CurrentConfigurations == nil ||
			dependencies.CurrentConfigurations.publicProjectID <= 0 {
			return nil, errors.New("the agent execution plane needs the public project id")
		}
		catalogueProjectID := dependencies.CurrentConfigurations.publicProjectID
		agentJobs, err = repos.NewAgentExecutionJobsRepository(
			dependencies.AdmissionPool,
			agentDispatchPolicy,
			catalogueProjectID,
		)
		if err != nil {
			return nil, fmt.Errorf("construct agent execution jobs: %w", err)
		}
		agentInputs, inputErr := agentexecutionapp.NewInputBundleFactory(
			agentexecutionapp.InputProfile{
				Classification:        inputClassification,
				RequiredGrantAudience: inputGrantAudience,
			},
			currentRuntimeID,
		)
		if inputErr != nil {
			return nil, fmt.Errorf("construct agent execution input bundle factory: %w", inputErr)
		}
		agentAdmissions, admissionErr := agentexecutionapp.NewAdmissionService(
			agentJobs,
			agentInputs,
			nil,
			currentRuntimeID,
		)
		if admissionErr != nil {
			return nil, fmt.Errorf("construct agent execution admission: %w", admissionErr)
		}
		agentTargets, targetErr := repos.NewCurrentAgentStartRepository(
			dependencies.AdmissionPool,
			catalogueProjectID,
		)
		if targetErr != nil {
			return nil, fmt.Errorf("construct current agent start resolver: %w", targetErr)
		}
		agentCancellation, cancelErr := repos.NewCurrentAgentCancelRepository(dependencies.AdmissionPool)
		if cancelErr != nil {
			return nil, fmt.Errorf("construct current agent cancellation repository: %w", cancelErr)
		}
		agentCancel, cancelErr = agentexecutionapp.NewCurrentAgentCancellationService(agentCancellation)
		if cancelErr != nil {
			return nil, fmt.Errorf("construct current agent cancellation service: %w", cancelErr)
		}
		// The read twin of the canceller (issue 254 P2). It is built in the same
		// block and from the same pool, so the poll surface exists exactly when
		// the stop surface does.
		agentTaskState, statusErr := repos.NewCurrentAgentTaskStatusRepository(dependencies.AdmissionPool)
		if statusErr != nil {
			return nil, fmt.Errorf("construct current agent task status repository: %w", statusErr)
		}
		agentTaskStatus, statusErr = agentexecutionapp.NewCurrentAgentTaskStatusService(agentTaskState)
		if statusErr != nil {
			return nil, fmt.Errorf("construct current agent task status service: %w", statusErr)
		}
		agentVersions, targetErr := newCurrentAgentVersionFreezer(
			dependencies.AdmissionPool,
			dependencies.CurrentConfigurations,
		)
		if targetErr != nil {
			return nil, fmt.Errorf("construct current agent version freezer: %w", targetErr)
		}
		// The SAME freezer instance the interactive start path uses. A nested
		// child has to be frozen by the identical rules as its parent —
		// blocked toolkits dropped, `internal_mcp` removed, `openai`
		// normalized to `agent` — because the native runtime decodes both
		// documents with one decoder and would refuse the child on a
		// difference the author never made.
		agentFreezer = agentVersions
		agentNestedVersions, targetErr = repos.NewCurrentNestedApplicationVersionRepository(
			dependencies.AdmissionPool,
		)
		if targetErr != nil {
			return nil, fmt.Errorf("construct nested application version reader: %w", targetErr)
		}
		agentMaterializer, targetErr = storage.NewCurrentConfigurationsMaterializer(
			dependencies.CurrentConfigurations.unsecreter,
		)
		if targetErr != nil {
			return nil, fmt.Errorf("construct current agent configuration materializer: %w", targetErr)
		}
		currentMainClient, targetErr := currentcore.NewTLSClient(config.RedisCAFile)
		if targetErr != nil {
			return nil, fmt.Errorf("construct current Main policy client: %w", targetErr)
		}
		nextInputSuggestionResolver, targetErr := currentcore.NewNextInputSuggestionResolver(
			config.CurrentMainBaseURL,
			dependencies.ActorTokenIssuer,
			currentMainClient,
		)
		if targetErr != nil {
			return nil, fmt.Errorf("construct next-input-suggestion policy resolver: %w", targetErr)
		}
		agentGuardrails, targetErr := platformconfig.NewGuardrailPolicyAdapter(dependencies.AdmissionPool)
		if targetErr != nil {
			return nil, fmt.Errorf("construct current agent guardrail policy source: %w", targetErr)
		}
		agentStart, targetErr = agentexecutionapp.NewCurrentApplicationStartService(
			agentTargets,
			agentTargets,
			agentTargets,
			agentTargets,
			nextInputSuggestionResolver,
			agentGuardrails,
			agentVersions,
			agentAdmissions,
		)
		if targetErr != nil {
			return nil, fmt.Errorf("construct current agent start service: %w", targetErr)
		}
		// Persistent, cross-conversation personal memory (#870). WithMemories
		// is a post-construction setter — see its own comment — precisely so
		// this one line is the whole diff needed here, with none of the six
		// *_test.go constructors under agentexecution touched. Built against
		// the SAME admission pool every other resolver in this block uses
		// (agentGuardrails, agentVersions above), not RouterConfig's
		// MemoriesRepo (main.go) — see that field's own comment.
		agentStart = agentStart.WithMemories(repos.NewMemoriesRepo(dependencies.AdmissionPool))
		agentDispatcher, err := agentexecutionapp.NewDispatcher(agentJobs, agentProducer)
		if err != nil {
			return nil, err
		}
		agentPublisher, err = agentexecutionapp.NewOutboxPublisher(
			agentJobs,
			agentDispatcher,
			executionapp.OutboxPublisherConfig{
				PollInterval:      250 * time.Millisecond,
				VisibilityTimeout: 30 * time.Second,
				BatchSize:         64,
				MaxConcurrent:     8,
				ReportFailure: func(err error) {
					dependencies.Logger.Error("agent execution outbox publisher cycle failed", "err", err)
				},
			},
		)
		if err != nil {
			return nil, err
		}
	}
	publisherRoot, err := newConfiguredPublisherSet(config.IndexIngestDispatchEnabled, publisher, indexPublisher)
	if err != nil {
		return nil, err
	}
	if config.AgentExecutionDispatchEnabled {
		publisherRoot, err = newPublisherSet(publisherRoot, agentPublisher)
		if err != nil {
			return nil, fmt.Errorf("compose agent execution publisher: %w", err)
		}
	}
	var nodeEvents *repos.NodeEventsRepository
	var replayWake *redisExecutionReplayWakeBus
	var replayWaiter executionapi.ReplayWaiter = pollingReplayWaiter{
		interval: phaseOneReplayPollInterval,
	}
	if config.IndexIngestDispatchEnabled || config.AgentExecutionDispatchEnabled {
		nodeEvents, err = repos.NewNodeEventsRepository(dependencies.OutputPool)
		if err != nil {
			return nil, fmt.Errorf("construct node event replay repository: %w", err)
		}
		replayWake, err = newRedisExecutionReplayWakeBus(controlRedis, dependencies.Logger)
		if err != nil {
			return nil, fmt.Errorf("construct execution replay wake bus: %w", err)
		}
		publisherRoot, err = newPublisherSet(publisherRoot, replayWake)
		if err != nil {
			return nil, fmt.Errorf("compose execution replay wake bus: %w", err)
		}
		replayWaiter = replayWake
		replayRetention, retentionErr := newExecutionReplayRetentionJanitor(
			nodeEvents,
			executionReplayRetentionPollInterval,
			func(err error) {
				dependencies.Logger.Error(
					"execution replay retention cycle failed",
					"err",
					err,
				)
			},
		)
		if retentionErr != nil {
			return nil, fmt.Errorf("construct execution replay retention janitor: %w", retentionErr)
		}
		publisherRoot, err = newPublisherSet(publisherRoot, replayRetention)
		if err != nil {
			return nil, fmt.Errorf("compose execution replay retention janitor: %w", err)
		}

		// The reconcilers below are index-ingest machinery, but they used to sit
		// under the combined `IndexIngest || AgentExecution` condition above.
		// indexMetaTerminalEffect and indexManualStopCleanupEffect are only
		// constructed when index ingest is enabled, so a runtime with agent
		// execution ON and index ingest OFF — the only shape available to a
		// deployment without the LiteLLM facade, since validateRuntimeComposition
		// requires it for index ingest — panicked here on a nil dereference
		// (`2*indexMetaTerminalEffect.concurrency`) before serving a request.
		// The node-event replay repository, wake bus and retention janitor above
		// stay shared: agent execution needs all three.
		if config.IndexIngestDispatchEnabled {
			indexMetaReconciler, reconcileErr := newCurrentIndexMetaTerminalReconciler(
				indexMetaTerminalEffect,
				500*time.Millisecond,
				2*indexMetaTerminalEffect.concurrency,
				func(err error) {
					dependencies.Logger.Error(
						"current index metadata terminal reconciliation failed",
						"err",
						err,
					)
				},
			)
			if reconcileErr != nil {
				return nil, fmt.Errorf(
					"construct current index metadata terminal reconciler: %w",
					reconcileErr,
				)
			}
			publisherRoot, err = newPublisherSet(publisherRoot, indexMetaReconciler)
			if err != nil {
				return nil, fmt.Errorf(
					"compose current index metadata terminal reconciler: %w",
					err,
				)
			}
			indexManualStopCleanupReconciler, reconcileErr :=
				newCurrentIndexManualStopCleanupReconciler(
					indexManualStopCleanupEffect,
					500*time.Millisecond,
					4,
					func(err error) {
						dependencies.Logger.Error(
							"current index manual Stop cleanup reconciliation failed",
							"err",
							err,
						)
					},
				)
			if reconcileErr != nil {
				return nil, fmt.Errorf(
					"construct current index manual Stop cleanup reconciler: %w",
					reconcileErr,
				)
			}
			publisherRoot, err = newPublisherSet(
				publisherRoot,
				indexManualStopCleanupReconciler,
			)
			if err != nil {
				return nil, fmt.Errorf(
					"compose current index manual Stop cleanup reconciler: %w",
					err,
				)
			}
			publisherRoot, err = newPublisherSet(
				publisherRoot,
				indexMetaTaskRestampReconciler,
			)
			if err != nil {
				return nil, fmt.Errorf(
					"compose current index metadata task restamp reconciler: %w",
					err,
				)
			}
		}
	}
	if dependencies.ConfigurationLifecycleReconciler != nil {
		configurationLifecycle, lifecycleErr := newCurrentConfigurationLifecyclePublisher(
			dependencies.ControlPool,
			dependencies.ConfigurationLifecycleReconciler,
			dependencies.Logger,
		)
		if lifecycleErr != nil {
			return nil, fmt.Errorf("construct current configuration lifecycle processor: %w", lifecycleErr)
		}
		publisherRoot, err = newPublisherSet(publisherRoot, configurationLifecycle)
		if err != nil {
			return nil, fmt.Errorf("compose current configuration lifecycle processor: %w", err)
		}
	}

	controlSessions, err := repos.NewWorkloadSessionsRepository(dependencies.ControlPool)
	if err != nil {
		return nil, fmt.Errorf("construct control workload sessions: %w", err)
	}
	controlPeerAuthorizer, err := workloadauth.NewPeerAuthorizer(controlSessions)
	if err != nil {
		return nil, err
	}
	capabilityVersions := map[string]string{
		executiondomain.ConfigurationValidationCapability: capabilityVersion,
	}
	if config.IndexIngestDispatchEnabled {
		capabilityVersions[executiondomain.IndexIngestCapability] = indexCapabilityVersion
	}
	if config.AgentExecutionDispatchEnabled {
		capabilityVersions[executiondomain.AgentApplicationCapability] = agentCapabilityVersion
		capabilityVersions[executiondomain.AgentAdhocCapability] = agentCapabilityVersion
	}
	verifier, err := control.NewProductionCommandVerifier(control.ProductionVerifierConfig{
		EnvelopeSchemaRevision: envelopeSchemaRevision,
		ProtocolRevision:       protocolRevision,
		CapabilityVersions:     capabilityVersions,
		LimitsRevision:         limitsRevision,
		MaxWorkerCommandBytes:  maxWorkerCommandBytes,
		MaxInputManifestBytes:  maxInputManifestBytes,
		MaxStringBytes:         maxSafeStringBytes,
	}, verificationKeys)
	if err != nil {
		return nil, err
	}
	controlClaimsRepository, err := repos.NewClaimsRepository(dependencies.ControlPool)
	if err != nil {
		return nil, fmt.Errorf("construct runtime claims: %w", err)
	}
	controlClaims, err := executionapp.NewClaimService(controlClaimsRepository, nil, claimLeaseTTL)
	if err != nil {
		return nil, err
	}
	inputs, err := repos.NewInputBundlesRepository(dependencies.ControlPool)
	if err != nil {
		return nil, fmt.Errorf("construct runtime input bundles: %w", err)
	}
	settlementsRepository, err := repos.NewSettlementsRepository(dependencies.ControlPool)
	if err != nil {
		return nil, fmt.Errorf("construct runtime settlements: %w", err)
	}
	var settlementOptions []executionapp.SettlementOption
	if dependencies.PipelineRuns != nil && dependencies.DomainEvents != nil {
		// pipeline.run.succeeded/failed — see Dependencies.PipelineRuns' own
		// doc comment for the full story. Both must be present: a hook with
		// no emitter to call, or an emitter with no tracker to look the
		// execution up in, could do nothing useful anyway.
		settlementOptions = append(settlementOptions,
			executionapp.WithAfterSettleHooks(pipelineruns.NewSettlementHook(dependencies.PipelineRuns, dependencies.DomainEvents)))
	}
	settlements, err := executionapp.NewSettlementService(settlementsRepository, settlementOptions...)
	if err != nil {
		return nil, err
	}
	controlServer, err := control.NewServer(control.ServerConfig{
		MaxInputManifestBytes: maxInputManifestBytes,
		MaxInputEntries:       maxInputEntries,
		MaxInputContentBytes:  maxInputContentBytes,
		MaxStringBytes:        maxSafeStringBytes,
	}, controlPeerAuthorizer, verifier, controlClaims, inputs, settlements)
	if err != nil {
		return nil, err
	}

	outputInbox, err := repos.NewOutputInboxRepository(dependencies.OutputPool)
	if err != nil {
		return nil, fmt.Errorf("construct runtime output inbox: %w", err)
	}
	results, err := repos.NewConfigurationValidationResultsRepository(dependencies.OutputPool)
	if err != nil {
		return nil, fmt.Errorf("construct runtime validation projector: %w", err)
	}
	runtimeFailureResults, err := repos.NewRuntimeFailureResultsRepository(dependencies.OutputPool)
	if err != nil {
		return nil, fmt.Errorf("construct runtime failure projector: %w", err)
	}
	outputSessions, err := repos.NewWorkloadSessionsRepository(dependencies.OutputPool)
	if err != nil {
		return nil, fmt.Errorf("construct output workload sessions: %w", err)
	}
	outputPeerAuthorizer, err := workloadauth.NewPeerAuthorizer(outputSessions)
	if err != nil {
		return nil, err
	}
	outputClaimsRepository, err := repos.NewClaimsRepository(dependencies.OutputPool)
	if err != nil {
		return nil, fmt.Errorf("construct output claims: %w", err)
	}
	outputClaims, err := executionapp.NewClaimService(outputClaimsRepository, nil, claimLeaseTTL)
	if err != nil {
		return nil, err
	}
	validationOutput, err := outputapp.NewConfigurationValidationService(outputInbox, outputClaims, results)
	if err != nil {
		return nil, err
	}
	var runtimeFailureOptions []outputapp.RuntimeFailureOption
	if dependencies.PipelineRuns != nil {
		// The error-text half of pipeline.run.failed — see
		// output.FailureObserver's own doc comment for why this is captured
		// here, before settlement, rather than at the AfterSettle hook above.
		runtimeFailureOptions = append(runtimeFailureOptions,
			outputapp.WithFailureObserver(pipelineruns.NewFailureObserver(dependencies.PipelineRuns)))
	}
	runtimeFailures, err := outputapp.NewRuntimeFailureService(
		outputInbox,
		outputClaims,
		runtimeFailureResults,
		runtimeFailureOptions...,
	)
	if err != nil {
		return nil, err
	}
	outputServerConfig := output.ServerConfig{
		OutputSchemaRevision: outputSchemaRevision,
		MaxFrameBytes:        maxOutputFrameBytes,
		CreditFrames:         1,
		CreditBytes:          maxOutputFrameBytes,
	}
	var outputServer *output.Server
	var outputServerErr error
	var agentOutput *outputapp.AgentExecutionService
	if config.AgentExecutionDispatchEnabled {
		agentResults, err := repos.NewAgentExecutionResultsRepository(dependencies.OutputPool)
		if err != nil {
			return nil, fmt.Errorf("construct agent execution result repository: %w", err)
		}
		agentOutput, err = outputapp.NewAgentExecutionService(
			agentJobs,
			outputClaims,
			agentResults,
		)
		if err != nil {
			return nil, err
		}
	}
	var nodeEventOutput *outputapp.NodeEventService
	if config.IndexIngestDispatchEnabled || config.AgentExecutionDispatchEnabled {
		nodeEventOutput, err = outputapp.NewNodeEventService(outputClaims, nodeEvents)
		if err != nil {
			return nil, err
		}
	}
	var agentOutputIngestor output.AgentExecutionIngestor = agentOutput
	var nodeEventOutputIngestor output.NodeEventIngestor = nodeEventOutput
	if replayWake != nil {
		if agentOutput != nil {
			agentOutputIngestor = wakingAgentExecutionIngestor{
				next: agentOutput,
				wake: replayWake,
			}
		}
		if nodeEventOutput != nil {
			nodeEventOutputIngestor = wakingNodeEventIngestor{
				next: nodeEventOutput,
				wake: replayWake,
			}
		}
	}
	// toolkitCallToolResults is built HERE rather than inside the tool-run
	// runtime below, because the output listener is composed before the index
	// graph the run service needs. It writes only `output_inbox`, so it depends
	// on nothing but the output pool.
	var toolkitCallToolResults *repos.ToolkitCallToolResultsRepository
	if config.IndexIngestDispatchEnabled {
		indexResults, err := repos.NewIndexIngestResultsRepository(dependencies.OutputPool, repos.IndexIngestOutputPolicy{
			LimitsRevision:    limitsRevision,
			ArtifactMediaType: indexArtifactMediaType,
			MaxArtifactBytes:  maxIndexArtifactBytes,
		})
		if err != nil {
			return nil, fmt.Errorf("construct index ingest result repository: %w", err)
		}
		indexOutput, err := outputapp.NewIndexIngestService(indexResults, outputClaims, indexResults, indexResults)
		if err != nil {
			return nil, err
		}
		// The tool-run terminal arm rides the SAME listener and the SAME worker
		// as index ingest, which is why it is composed under this flag: a
		// deployment whose worker runs an index is the deployment whose worker
		// can run one of that toolkit's tools.
		toolkitCallToolResults, err = repos.NewToolkitCallToolResultsRepository(dependencies.OutputPool)
		if err != nil {
			return nil, fmt.Errorf("construct tool-run result repository: %w", err)
		}
		toolkitCallToolOutput, err := outputapp.NewToolkitCallToolService(
			toolkitCallToolResults, outputClaims, toolkitCallToolResults,
		)
		if err != nil {
			return nil, err
		}
		// Assigned through a nil INTERFACE where a capability is absent, never
		// a typed nil: ingestMessage decides on `== nil`, and a typed nil would
		// pass that check and then call a method on a nil pointer.
		var agents output.AgentExecutionIngestor
		if config.AgentExecutionDispatchEnabled {
			agents = agentOutputIngestor
		}
		outputServer, outputServerErr = output.NewServerWithCapabilities(
			outputServerConfig,
			outputPeerAuthorizer,
			validationOutput,
			runtimeFailures,
			indexOutput,
			agents,
			nodeEventOutputIngestor,
			output.WithToolkitCallTool(toolkitCallToolOutput),
		)
	} else if config.AgentExecutionDispatchEnabled {
		outputServer, outputServerErr = output.NewServerWithAgentAndNodeEvents(
			outputServerConfig,
			outputPeerAuthorizer,
			validationOutput,
			runtimeFailures,
			agentOutputIngestor,
			nodeEventOutputIngestor,
		)
	} else {
		outputServer, outputServerErr = output.NewServer(
			outputServerConfig,
			outputPeerAuthorizer,
			validationOutput,
			runtimeFailures,
		)
	}
	if outputServerErr != nil {
		return nil, outputServerErr
	}

	contentRepository, err := storage.NewPostgresContentRepository(dependencies.ContentPool)
	if err != nil {
		return nil, fmt.Errorf("construct runtime content repository: %w", err)
	}
	var contentServer *storage.ContentServer
	var indexStart indexingapi.StartUseCase
	var toolkitCallTool toolkitrun.UseCase
	var currentIndex *currentIndexRuntime
	var projectSystemTokens storage.ProjectSystemTokenIssuer
	if config.IndexSchedulingEnabled {
		projectSystemTokens = currentProjectSystemTokenAdapter{
			source: dependencies.ProjectSystemTokenSource,
		}
	}
	var runtimeToken *storage.EliteaClientTokenService
	if config.IndexIngestDispatchEnabled || config.AgentExecutionDispatchEnabled {
		// projectSecretsHeader is what stops the SDK sending the literal
		// "secret" (#408). The worker asks for it with its bearer token and
		// puts it on every call it makes back to this service, so the
		// version-details route can refuse a project that has no value instead
		// of accepting a value printed in the pylon source.
		projectSecretsHeader := secretsapi.NewHandler(dependencies.ContentPool)
		if projectSystemTokens != nil {
			runtimeToken, err = storage.NewEliteaClientTokenServiceWithSchedules(
				contentRepository,
				dependencies.ActorTokenIssuer,
				projectSystemTokens,
				dependencies.ProjectTokenValidator,
				projectSecretsHeader,
			)
		} else {
			runtimeToken, err = storage.NewEliteaClientTokenService(
				contentRepository,
				dependencies.ActorTokenIssuer,
				dependencies.ProjectTokenValidator,
				projectSecretsHeader,
			)
		}
		if err != nil {
			return nil, fmt.Errorf("construct runtime worker client-token context: %w", err)
		}
	}
	// nestedApplicationVersions is what makes agent-as-tool executable at all:
	// without this route the native worker reaches
	// `resolve_application_version` and fails the whole turn with
	// `native_agent.dependency_unavailable`, because it refuses by design to
	// fall back to the mutable public version endpoint
	// (services/elitea-worker-rust/src/transport/runtime_context.rs:328-333).
	var nestedApplicationVersions *storage.RuntimeApplicationVersionService
	if config.AgentExecutionDispatchEnabled {
		nestedApplicationVersions, err = storage.NewRuntimeApplicationVersionService(
			contentRepository,
			agentNestedVersions,
			agentFreezer,
		)
		if err != nil {
			return nil, fmt.Errorf("construct nested application version context: %w", err)
		}
	}
	// attachmentObjects is what lets an attached DOCUMENT reach the model as
	// text rather than as a filename. The native runtime has no other channel
	// to object storage (no vault, no `artifact` toolkit family, and an egress
	// allowlist that reaches the model gateway alone), so without this route a
	// chunk flagged `needs_content_extraction` is announced and skipped
	// (services/elitea-worker-rust/src/agents/attachments.rs).
	//
	// A nil ObjectStore leaves it nil, and the route unregistered: mixed
	// deployments keep the Centry artifacts capability authoritative and run
	// this service with no Go object store at all, and there the honest answer
	// is that the runtime keeps announcing files it cannot read — not a route
	// that exists and fails.
	var attachmentObjects *storage.RuntimeAttachmentObjectService
	if config.AgentExecutionDispatchEnabled && dependencies.ObjectStore != nil {
		attachmentSource, attachmentErr := repos.NewCurrentAttachmentObjectRepository(
			dependencies.AdmissionPool,
			dependencies.ObjectStore,
		)
		if attachmentErr != nil {
			return nil, fmt.Errorf("construct attachment object reader: %w", attachmentErr)
		}
		attachmentObjects, err = storage.NewRuntimeAttachmentObjectService(
			contentRepository,
			attachmentSource,
		)
		if err != nil {
			return nil, fmt.Errorf("construct attachment object context: %w", err)
		}
	}
	if config.IndexIngestDispatchEnabled {
		currentIndex, err = newCurrentIndexRuntime(
			dependencies.AdmissionPool,
			dependencies.CurrentConfigurations,
			config,
			indexDispatchPolicy,
			currentIndexMetaWriter,
			func(err error) {
				dependencies.Logger.Error(
					"current index metadata initialization requeued",
					"err",
					err,
				)
			},
		)
		if err != nil {
			return nil, fmt.Errorf("construct current index runtime: %w", err)
		}
		// One listener serves both capabilities, so when agent execution is
		// dispatched alongside index ingest the nested route belongs here too.
		// Registering it is not a widening, but only because the service asks
		// for AgentRuntimeContextAuthorizer: the claim authorizer selects the
		// project and actor from the execution row, while the REQUEST names
		// the application and version inside that project — so an index claim
		// on this shared listener could otherwise read any agent version in
		// the project it was admitted for. The capability filter in
		// AuthorizeAgentRuntimeContext is what makes that false.
		if nestedApplicationVersions != nil && attachmentObjects != nil {
			contentServer, err = storage.NewAgentAttachmentRuntimeContentServerWithLimits(
				contentRepository,
				contentRepository,
				currentIndex.materializer,
				runtimeToken,
				nestedApplicationVersions,
				attachmentObjects,
				maxInputContentBytes,
				maxContentRequests,
			)
		} else if nestedApplicationVersions != nil {
			contentServer, err = storage.NewNestedAgentRuntimeContentServerWithLimits(
				contentRepository,
				contentRepository,
				currentIndex.materializer,
				runtimeToken,
				nestedApplicationVersions,
				maxInputContentBytes,
				maxContentRequests,
			)
		} else {
			contentServer, err = storage.NewMaterializingRuntimeContentServerWithLimits(
				contentRepository,
				contentRepository,
				currentIndex.materializer,
				runtimeToken,
				maxInputContentBytes,
				maxContentRequests,
			)
		}
		if err != nil {
			return nil, err
		}
		indexStart = currentIndex.start
		// The tool-run producer, composed on the index graph's own toolkit
		// reader and settings resolver.
		// The analytics record (issue 618). It is composed here, on the
		// admission pool, because the explicit run's record must commit on the
		// same database its execution_jobs row does.
		toolCallRecords, recordsErr := repos.NewToolCallRecordsRepository(dependencies.AdmissionPool)
		if recordsErr != nil {
			return nil, recordsErr
		}
		toolkitCallToolRuntime, toolRunErr := newCurrentToolkitCallToolRuntime(
			dependencies.AdmissionPool,
			toolkitCallToolResults,
			currentIndex,
			dependencies.ToolkitCatalogue,
			dependencies.WorkerToolkitCapability,
			toolkitCallToolProducer,
			toolkitCallToolDispatchPolicy,
			0,
			toolCallRecords,
		)
		if toolRunErr != nil {
			return nil, toolRunErr
		}
		toolkitCallTool = toolkitCallToolRuntime.run
		indexPublishers := []publisherRunner{
			publisherRoot,
			currentIndex.initializer,
		}
		if config.IndexSchedulingEnabled {
			catalog, catalogErr := repos.NewCurrentIndexScheduleCatalog(
				dependencies.AdmissionPool,
			)
			if catalogErr != nil {
				return nil, fmt.Errorf(
					"construct current index schedule catalog: %w",
					catalogErr,
				)
			}
			systemIdentity, identityErr :=
				storage.NewProjectSystemIdentityService(
					projectSystemTokens,
					dependencies.ProjectTokenValidator,
				)
			if identityErr != nil {
				return nil, fmt.Errorf(
					"construct current index schedule identity: %w",
					identityErr,
				)
			}
			inspector, inspectorErr := newCurrentIndexScheduleInspector(
				currentIndex.exact,
			)
			if inspectorErr != nil {
				return nil, inspectorErr
			}
			executor, executorErr := indexscheduleapp.NewCurrentExecutor(
				currentIndex.toolkits,
				currentIndex.settings,
				systemIdentity,
				inspector,
				currentIndex.inputs,
				currentIndex.start,
			)
			if executorErr != nil {
				return nil, executorErr
			}
			notifications, notificationErr :=
				repos.NewCurrentIndexScheduleNotificationRepository(
					dependencies.ControlPool,
				)
			if notificationErr != nil {
				return nil, notificationErr
			}
			failures, failureErr := newCurrentIndexScheduleFailureRecorder(
				currentIndex.toolkits,
				currentIndex.settings,
				currentIndexMetaWriter,
				notifications,
			)
			if failureErr != nil {
				return nil, failureErr
			}
			indexRunner, runnerErr := indexscheduleapp.NewRunner(
				catalog,
				currentIndexSchedulingAvailability{},
				executor,
				failures,
			)
			if runnerErr != nil {
				return nil, runnerErr
			}
			indexDueWork, dueWorkErr := newCurrentIndexScheduleDueWork(
				indexRunner,
			)
			if dueWorkErr != nil {
				return nil, dueWorkErr
			}
			patchRepository, patchErr :=
				repos.NewCurrentIndexSchedulePatchRepository(
					dependencies.AdmissionPool,
				)
			if patchErr != nil {
				return nil, patchErr
			}
			scheduleUpdate, updateErr := indexscheduleapp.NewService(
				patchRepository,
			)
			if updateErr != nil {
				return nil, updateErr
			}
			deleteRepository, deleteErr :=
				repos.NewCurrentIndexScheduleDeleteRepository(
					dependencies.AdmissionPool,
				)
			if deleteErr != nil {
				return nil, deleteErr
			}
			scheduleDelete, deleteErr := indexscheduleapp.NewDeleteService(
				deleteRepository,
			)
			if deleteErr != nil {
				return nil, deleteErr
			}
			currentIndex.scheduleUpdate = scheduleUpdate
			currentIndex.scheduleDelete = scheduleDelete
			currentIndex.scheduleAction = indexDueWork
			schedule, scheduleErr := schedulingapp.ParseCron(
				currentIndexScheduleCadence,
			)
			if scheduleErr != nil {
				return nil, fmt.Errorf(
					"parse current index schedule cadence: %w",
					scheduleErr,
				)
			}

			// The retention sweeper (S14) piggybacks on this registry when the
			// Go artifacts capability is enabled. Mixed deployments keep the
			// current Centry artifacts capability authoritative, so a nil store
			// deliberately omits only this job, not index scheduling itself.
			var retentionHandler schedulingapp.Handler
			var retentionSchedule schedulingapp.Schedule
			if dependencies.ObjectStore != nil {
				artifactBuckets, artifactBucketsErr := repos.NewArtifactBucketsRepository(dependencies.AdmissionPool)
				if artifactBucketsErr != nil {
					return nil, fmt.Errorf("construct artifact buckets repository: %w", artifactBucketsErr)
				}
				artifactObjects, artifactObjectsErr := repos.NewArtifactObjectsRepository(dependencies.AdmissionPool)
				if artifactObjectsErr != nil {
					return nil, fmt.Errorf("construct artifact objects repository: %w", artifactObjectsErr)
				}
				artifactNotifications, artifactNotificationsErr := repos.NewArtifactRetentionNotificationRepository(dependencies.AdmissionPool)
				if artifactNotificationsErr != nil {
					return nil, fmt.Errorf("construct artifact retention notification repository: %w", artifactNotificationsErr)
				}
				// S20a: reclaims elitea_storage.attachment_chunks rows left by
				// abandoned chunked attachment uploads — see
				// artifactRetentionAttachmentChunksRepository's own doc comment.
				attachmentChunks, attachmentChunksErr := repos.NewAttachmentChunksRepository(dependencies.AdmissionPool)
				if attachmentChunksErr != nil {
					return nil, fmt.Errorf("construct attachment chunks repository: %w", attachmentChunksErr)
				}
				// Reclaims the bytes behind an expired, never-committed
				// transfer grant — see sweepExpiredGrants's own doc comment
				// for why no other sweep can see them.
				artifactGrants, artifactGrantsErr := repos.NewArtifactTransferGrantsRepository(dependencies.AdmissionPool)
				if artifactGrantsErr != nil {
					return nil, fmt.Errorf("construct artifact transfer grants repository: %w", artifactGrantsErr)
				}
				retentionSweep, retentionSweepErr := newArtifactRetentionSweep(
					artifactObjects, artifactBuckets, artifactNotifications, artifactGrants, dependencies.ObjectStore,
				)
				if retentionSweepErr != nil {
					return nil, fmt.Errorf("construct artifact retention sweep: %w", retentionSweepErr)
				}
				retentionHandler = retentionSweep.WithAttachmentChunks(attachmentChunks)
				parsedRetentionSchedule, retentionScheduleErr := schedulingapp.ParseCron(
					artifactRetentionSweepCadence,
				)
				if retentionScheduleErr != nil {
					return nil, fmt.Errorf(
						"parse artifact retention sweep cadence: %w",
						retentionScheduleErr,
					)
				}
				retentionSchedule = parsedRetentionSchedule
			}

			// One scan observes all projects and may cover the same due index
			// occurrence as an older scan. Claiming scan occurrences in
			// parallel only makes the product runner's overlap guard release
			// them for retry. Claim one at a time, with a bounded catch-up
			// budget; PostgreSQL occurrence leases and stable per-index
			// idempotency remain the cross-replica correctness boundary.
			schedulerConfig := schedulingapp.Config{
				InstanceID:      config.SchedulerInstanceID,
				LeaseDuration:   currentIndexScheduleLeaseDuration,
				MaxParallel:     1,
				PageSize:        1,
				MaxPagesPerTick: 4,
			}
			registry, registryErr := scheduledJobRegistry(
				schedulerConfig.LeaseDuration,
				scheduledJobs(indexDueWork, retentionHandler, schedule, retentionSchedule)...,
			)
			if registryErr != nil {
				return nil, fmt.Errorf(
					"register current index scheduled admission: %w",
					registryErr,
				)
			}
			occurrences, occurrenceErr :=
				repos.NewScheduleOccurrenceRepository(
					dependencies.AdmissionPool,
				)
			if occurrenceErr != nil {
				return nil, fmt.Errorf(
					"construct scheduled occurrence repository: %w",
					occurrenceErr,
				)
			}
			scheduler, schedulerErr := schedulingapp.NewRunner(
				occurrences,
				registry,
				schedulerConfig,
				dependencies.Logger,
			)
			if schedulerErr != nil {
				return nil, fmt.Errorf(
					"construct current index scheduler: %w",
					schedulerErr,
				)
			}
			indexPublishers = append(indexPublishers, scheduler)
		}
		publisherRoot, err = newPublisherSet(indexPublishers...)
		if err != nil {
			return nil, fmt.Errorf(
				"compose current index lifecycle: %w",
				err,
			)
		}
	} else if config.AgentExecutionDispatchEnabled {
		if attachmentObjects != nil {
			contentServer, err = storage.NewAgentAttachmentRuntimeContentServerWithLimits(
				contentRepository,
				contentRepository,
				agentMaterializer,
				runtimeToken,
				nestedApplicationVersions,
				attachmentObjects,
				maxInputContentBytes,
				maxContentRequests,
			)
		} else {
			contentServer, err = storage.NewNestedAgentRuntimeContentServerWithLimits(
				contentRepository,
				contentRepository,
				agentMaterializer,
				runtimeToken,
				nestedApplicationVersions,
				maxInputContentBytes,
				maxContentRequests,
			)
		}
		if err != nil {
			return nil, err
		}
	} else {
		contentServer, err = storage.NewContentServerWithLimits(contentRepository, contentRepository, maxInputContentBytes, maxContentRequests)
		if err != nil {
			return nil, err
		}
	}
	privateServers, err := runtimegrpc.NewPrivateServerSet(runtimegrpc.PrivateServerConfig{
		ControlAddress:          config.ControlAddress,
		OutputAddress:           config.OutputAddress,
		ContentAddress:          config.ContentAddress,
		ControlTLS:              controlTLS,
		OutputTLS:               outputTLS,
		ContentTLS:              contentTLS,
		ControlMaxRequestBytes:  maxGRPCRequestBytes,
		ControlMaxResponseBytes: maxGRPCResponseBytes,
		OutputMaxRequestBytes:   maxGRPCRequestBytes,
		OutputMaxResponseBytes:  maxGRPCResponseBytes,
		ControlGRPC:             phaseOneGRPCPolicy(16),
		OutputGRPC:              phaseOneGRPCPolicy(4),
		ContentMaxConnections:   64,
		ContentMaxStreams:       maxContentRequests,
		ContentReadTimeout:      10 * time.Second,
		ContentWriteTimeout:     30 * time.Second,
		ContentIdleTimeout:      time.Minute,
		ContentMaxHeaderBytes:   16 * 1024,
		ShutdownTimeout:         15 * time.Second,
	}, runtimegrpc.PrivateServices{
		Control: controlServer,
		Output:  outputServer,
		Content: contentServer.Routes(),
	})
	if err != nil {
		return nil, err
	}

	replay, err := repos.NewReplayEventsRepository(dependencies.ReplayPool)
	if err != nil {
		return nil, fmt.Errorf("construct runtime replay repository: %w", err)
	}
	publicAuthorizer, err := newPostgresPublicAuthorizer(
		sqlcgen.New(dependencies.AdmissionPool),
		sqlcgen.New(dependencies.ReplayPool),
		dependencies.PermissionResolver,
	)
	if err != nil {
		return nil, err
	}
	publicRoutes, err := newPublicRoutes(
		publicAuthorizer,
		validationSubmitter,
		replay,
		replayWaiter,
		indexStart,
		agentStart,
		toolkitCallTool,
		int(dependencies.ReplayPool.Config().MaxConns),
	)
	if err != nil {
		return nil, err
	}
	if currentIndex != nil {
		publicRoutes.IndexCancel = currentIndex.cancel
		publicRoutes.IndexMeta = currentIndex.indexMeta
		publicRoutes.IndexMetaDelete = currentIndex.indexDelete
		if config.IndexSchedulingEnabled {
			publicRoutes.IndexScheduleUpdate = currentIndex.scheduleUpdate
			publicRoutes.IndexScheduleDelete = currentIndex.scheduleDelete
		}
	}
	if agentCancel != nil {
		publicRoutes.AgentCancel = agentCancel
	}
	if agentTaskStatus != nil {
		publicRoutes.AgentTaskStatus = agentTaskStatus
	}

	closeRedis = false
	return &Runtime{
		publisher:              publisherRoot,
		private:                privateServers,
		controlRedis:           controlRedis,
		publicRoutes:           publicRoutes,
		configurationValidator: currentSDKConfigurationValidator,
	}, nil
}

func phaseOneGRPCPolicy(maxConcurrentStreams uint32) runtimegrpc.GRPCServerPolicy {
	return runtimegrpc.GRPCServerPolicy{
		MaxConcurrentStreams:  maxConcurrentStreams,
		MaxConnections:        64,
		MinClientPingInterval: 30 * time.Second,
		KeepaliveTime:         time.Minute,
		KeepaliveTimeout:      10 * time.Second,
		MaxConnectionIdle:     5 * time.Minute,
		MaxConnectionAge:      30 * time.Minute,
		MaxConnectionAgeGrace: 30 * time.Second,
	}
}
