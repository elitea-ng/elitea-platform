package runtimecomposition

import (
	"errors"
	"fmt"
	"time"

	toolkitcalltoolapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
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

// newCurrentToolkitCallToolRuntime composes the tool-run producer on top of the
// index runtime's own toolkit reader and settings resolver.
//
// It REUSES them rather than building a second graph, deliberately: the reader
// is where cross-project visibility is decided and the resolver is where a
// credential reference is frozen, and a second answer to either would be a
// second security boundary maintained in parallel with the first.
func newCurrentToolkitCallToolRuntime(
	admissionPool *pgxpool.Pool,
	results *repos.ToolkitCallToolResultsRepository,
	index *currentIndexRuntime,
	catalogue *CurrentToolkitCatalogueSnapshot,
	capability *WorkerToolkitCapability,
	producer *redisdispatch.ToolkitCallToolProducer,
	policy repos.ToolkitCallToolDispatchPolicy,
	deadline time.Duration,
) (*currentToolkitCallToolRuntime, error) {
	if admissionPool == nil || results == nil || index == nil ||
		index.toolkits == nil || index.settings == nil || producer == nil {
		return nil, errors.New("tool-run runtime dependencies are required")
	}
	resolver, err := toolkitcalltoolapp.NewCurrentAuthoritativeInputResolver(
		index.toolkits, index.settings,
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
	)
	if err != nil {
		return nil, fmt.Errorf("construct tool-run service: %w", err)
	}
	return &currentToolkitCallToolRuntime{run: run, jobs: jobs, results: results}, nil
}
