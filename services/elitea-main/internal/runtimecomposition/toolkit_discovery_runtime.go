package runtimecomposition

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	indexingapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexing"
	toolkitcalltoolapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	discovery "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/toolkitdiscovery"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/depfailure"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/commandbus"
	"github.com/jackc/pgx/v5/pgxpool"
)

func newCurrentToolkitDiscoveryRuntime(pool *pgxpool.Pool, toolkits indexingapp.CurrentToolkitReader, settings indexingapp.CurrentToolkitSettingsValidator, catalogue *CurrentToolkitCatalogueSnapshot, capability *WorkerToolkitCapability, producer *commandbus.ToolkitAvailableToolsProducer, policy repos.ToolkitAvailableToolsDispatchPolicy, artifacts *repos.ToolkitDiscoveryArtifactRepository, logger *slog.Logger) (*currentToolkitDiscoveryRuntime, error) {
	if toolkits == nil || settings == nil || artifacts == nil {
		return nil, fmt.Errorf("toolkit discovery requires shared toolkit input services")
	}
	guardrails, err := platformconfig.NewGuardrailPolicyAdapter(pool)
	if err != nil {
		return nil, err
	}
	reader, err := newCurrentActorToolkitReader(toolkits)
	if err != nil {
		return nil, err
	}
	resolver, err := discovery.NewCurrentAuthoritativeInputResolver(reader, settings, guardrails, toolkitcalltoolapp.WithMCPAuthorization(currentToolkitMCPTokenStore(pool)))
	if err != nil {
		return nil, err
	}
	inputs, err := discovery.NewInputBundleFactory(discovery.InputProfile{Classification: inputClassification, RequiredGrantAudience: inputGrantAudience}, currentRuntimeID)
	if err != nil {
		return nil, err
	}
	jobs, err := repos.NewToolkitAvailableToolsJobsRepository(pool, policy)
	if err != nil {
		return nil, err
	}
	admissions, err := discovery.NewAdmissionService(jobs, inputs, nil, currentRuntimeID)
	if err != nil {
		return nil, err
	}
	dispatcher, err := discovery.NewDispatcher(jobs, producer)
	if err != nil {
		return nil, err
	}
	service, err := discovery.NewService(resolver, toolkitTypeVerdictAdapter{catalogue: catalogue, capability: capability}, admissions, dispatcher, jobs, artifacts, discovery.DispatchPolicy{CapabilityVersion: policy.CapabilityVersion, ResourceClass: policy.ResourceClass, IsolationClass: policy.IsolationClass, Priority: uint32(policy.Priority), LimitsRevision: policy.LimitsRevision}, currentRuntimeID, 0)
	if err != nil {
		return nil, err
	}
	return &currentToolkitDiscoveryRuntime{service: service, jobs: jobs, artifacts: artifacts, logger: logger}, nil
}

// toolkitDiscoveryRetirement and toolkitDiscoveryArtifactSweep are the two
// housekeeping passes the runner repeats.
type toolkitDiscoveryRetirement interface {
	ReclaimExpiredToolkitAvailableToolsRuns(ctx context.Context, limit int) (int, error)
}

type toolkitDiscoveryArtifactSweep interface {
	SweepOrphanToolkitDiscoveryArtifacts(ctx context.Context, limit int) (int, error)
}

// toolkitDiscoveryPassInterval is the pause between two clean passes.
const toolkitDiscoveryPassInterval = 10 * time.Second

type currentToolkitDiscoveryRuntime struct {
	service   *discovery.Service
	jobs      toolkitDiscoveryRetirement
	artifacts toolkitDiscoveryArtifactSweep
	logger    *slog.Logger

	// Seams for tests; production uses the real timer and jitter.
	wait   func(context.Context, time.Duration) error
	jitter func(time.Duration) time.Duration
}

// Run repeats the retirement and artifact sweep passes until ctx ends. A
// failed pass is logged and retried after a bounded backoff (1 s doubling to
// 30 s, reset by a clean pass): a database outage never stops the runner, so
// it never stops Main (FU-275). Run returns only ctx's own error.
func (r *currentToolkitDiscoveryRuntime) Run(ctx context.Context) error {
	failures := 0
	for {
		delay := toolkitDiscoveryPassInterval
		if err := r.pass(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			failures++
			delay = runnerRestartDelay(failures, r.jitterSource())
			r.log().Error(
				fmt.Sprintf("The toolkit discovery housekeeping pass failed; retrying in %s", delay.Round(time.Millisecond)),
				"attempt", failures,
				"cause", depfailure.Describe(err),
				"err", err,
			)
		} else {
			failures = 0
		}
		if err := r.waitFor(ctx, delay); err != nil {
			return err
		}
	}
}

func (r *currentToolkitDiscoveryRuntime) pass(ctx context.Context) error {
	if _, err := r.jobs.ReclaimExpiredToolkitAvailableToolsRuns(ctx, 32); err != nil {
		return fmt.Errorf("retire expired toolkit discovery: %w", err)
	}
	if _, err := r.artifacts.SweepOrphanToolkitDiscoveryArtifacts(ctx, 32); err != nil {
		return fmt.Errorf("sweep toolkit discovery artifacts: %w", err)
	}
	return nil
}

func (r *currentToolkitDiscoveryRuntime) waitFor(ctx context.Context, delay time.Duration) error {
	if r.wait != nil {
		return r.wait(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (r *currentToolkitDiscoveryRuntime) jitterSource() func(time.Duration) time.Duration {
	if r.jitter != nil {
		return r.jitter
	}
	return randomRestartJitter
}

func (r *currentToolkitDiscoveryRuntime) log() *slog.Logger {
	if r.logger != nil {
		return r.logger
	}
	return slog.New(slog.DiscardHandler)
}
