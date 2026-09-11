package runtimecomposition

import (
	"context"
	"errors"
	"fmt"
	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	"time"

	toolkitcalltoolapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/redisdispatch"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	toolkitCallToolCapabilityVersion = "1"
	// A tool run holds a bounded synchronous slot and effects provider work,
	// which is what the index worker's classes already describe, so it takes
	// the same pair rather than inventing a third.
	toolkitCallToolResourceClass  = indexResourceClass
	toolkitCallToolIsolationClass = indexIsolationClass
	// toolkitCallToolDeadlineTTL is the ADMISSION deadline, not the caller's
	// wait. It is far longer than the HTTP bound on purpose: the bounded wait
	// giving up does not cancel the run, and a deadline shorter than the run
	// would retire an execution that is still doing provider work.
	toolkitCallToolDeadlineTTL = time.Hour
)

// currentToolkitCallToolRuntime is the composed producer for one synchronous
// tool run.
type currentToolkitCallToolRuntime struct {
	run     *toolkitcalltoolapp.RunService
	jobs    *repos.ToolkitCallToolJobsRepository
	results *repos.ToolkitCallToolResultsRepository
}

// toolkitTypeVerdictAdapter answers the same question
// internal/api/v2/toolkits asks of the same two sources — see
// `Handler.supportsToolkitType` (type_catalogue.go:243). It is the SAME pair of
// objects, composed once more here rather than a second rule: a tool run that
// disagreed with the catalogue about which types are runnable would offer a
// type it then refuses, or refuse one it offers.
type toolkitTypeVerdictAdapter struct {
	catalogue  *CurrentToolkitCatalogueSnapshot
	capability *WorkerToolkitCapability
}

func (a toolkitTypeVerdictAdapter) SupportsToolkitType(toolkitType string) (bool, string) {
	if a.catalogue == nil || a.capability == nil {
		// The same reading of absence the catalogue applies: a deployment that
		// has not stated its worker keeps behaving as it did before this
		// projection existed.
		return true, ""
	}
	importKey, found := a.catalogue.ToolkitImportKey(toolkitType)
	if !found {
		return true, ""
	}
	return a.capability.SupportsToolkitType(toolkitType, importKey)
}

var _ toolkitcalltoolapp.ToolkitTypeVerdict = toolkitTypeVerdictAdapter{}

// newCurrentToolkitCallToolRuntime composes the tool-run producer using the
// configured worker's shared toolkit reader and settings resolver.
//
// It REUSES them rather than building a second graph, deliberately: the reader
// is where cross-project visibility is decided and the resolver is where a
// credential reference is frozen, and a second answer to either would be a
// second security boundary maintained in parallel with the first.
func newCurrentToolkitCallToolRuntime(
	admissionPool *pgxpool.Pool,
	results *repos.ToolkitCallToolResultsRepository,
	toolkits indexingapp.CurrentToolkitReader, settings indexingapp.CurrentToolkitSettingsValidator,
	catalogue *CurrentToolkitCatalogueSnapshot,
	capability *WorkerToolkitCapability,
	producer *redisdispatch.ToolkitCallToolProducer,
	policy repos.ToolkitCallToolDispatchPolicy,
	deadline time.Duration,
	records *repos.ToolCallRecordsRepository,
) (*currentToolkitCallToolRuntime, error) {
	if admissionPool == nil || results == nil || toolkits == nil || settings == nil || producer == nil {
		return nil, errors.New("tool-run runtime dependencies are required")
	}
	guardrails, err := platformconfig.NewGuardrailPolicyAdapter(admissionPool)
	if err != nil {
		return nil, fmt.Errorf("construct tool-run guardrails: %w", err)
	}
	actorReader, err := newCurrentActorToolkitReader(toolkits)
	if err != nil {
		return nil, err
	}
	var resolverOptions []toolkitcalltoolapp.ResolverOption
	if tokens := currentToolkitMCPTokenStore(admissionPool); tokens != nil {
		resolverOptions = append(resolverOptions, toolkitcalltoolapp.WithMCPAuthorization(tokens))
	}
	resolver, err := toolkitcalltoolapp.NewCurrentAuthoritativeInputResolver(
		actorReader, settings, guardrails, resolverOptions...,
	)
	if err != nil {
		return nil, fmt.Errorf("construct tool-run input resolver: %w", err)
	}
	inputs, err := toolkitcalltoolapp.NewInputBundleFactory(
		toolkitcalltoolapp.InputProfile{
			Classification:        inputClassification,
			RequiredGrantAudience: inputGrantAudience,
		},
		currentRuntimeID,
	)
	if err != nil {
		return nil, fmt.Errorf("construct tool-run input bundle factory: %w", err)
	}
	jobs, err := repos.NewToolkitCallToolJobsRepository(admissionPool, policy)
	if err != nil {
		return nil, fmt.Errorf("construct tool-run jobs repository: %w", err)
	}
	admissions, err := toolkitcalltoolapp.NewAdmissionService(jobs, inputs, nil, currentRuntimeID)
	if err != nil {
		return nil, fmt.Errorf("construct tool-run admission service: %w", err)
	}
	dispatcher, err := toolkitcalltoolapp.NewDispatcher(jobs, producer)
	if err != nil {
		return nil, fmt.Errorf("construct tool-run dispatcher: %w", err)
	}
	run, err := toolkitcalltoolapp.NewRunService(
		resolver,
		toolkitTypeVerdictAdapter{catalogue: catalogue, capability: capability},
		admissions,
		dispatcher,
		jobs,
		toolkitcalltoolapp.DispatchPolicy{
			CapabilityVersion: policy.CapabilityVersion,
			ResourceClass:     policy.ResourceClass,
			IsolationClass:    policy.IsolationClass,
			Priority:          uint32(policy.Priority),
			LimitsRevision:    policy.LimitsRevision,
		},
		currentRuntimeID,
		deadline,
		toolkitcalltoolapp.WithRunRecorder(toolRunRecorderAdapter{records: records}),
	)
	if err != nil {
		return nil, fmt.Errorf("construct tool-run service: %w", err)
	}
	return &currentToolkitCallToolRuntime{run: run, jobs: jobs, results: results}, nil
}

// toolRunRecorderAdapter turns one settled explicit tool run into the shared
// analytics record (issue 618).
//
// It lives here rather than as a method on the repository because the shape it
// translates belongs to the application package: the repository owns a row and
// the tool-run service owns a run, and neither should have to import the other
// to say so.
type toolRunRecorderAdapter struct {
	records *repos.ToolCallRecordsRepository
}

var _ toolkitcalltoolapp.RunRecorder = toolRunRecorderAdapter{}

func (a toolRunRecorderAdapter) RecordToolRun(
	ctx context.Context,
	record toolkitcalltoolapp.ToolRunRecord,
) error {
	if a.records == nil {
		return nil
	}
	return a.records.Record(ctx, repos.ToolCallRecord{
		ProjectID: record.ProjectID,
		Source:    repos.ToolCallSourceExplicitRun,
		// The execution id IS the natural key: an idempotent re-admission of
		// the same run returns the same id, so a replay updates one row rather
		// than counting the call twice.
		SourceRef:   record.ExecutionID,
		ToolkitID:   record.ToolkitID,
		ToolkitType: record.ToolkitType,
		ToolName:    record.ToolName,
		StartedAt:   record.StartedAt,
		FinishedAt:  record.FinishedAt,
		IsError:     record.IsError,
		ActorUserID: record.ActorUserID,
		ExecutionID: record.ExecutionID,
	})
}
