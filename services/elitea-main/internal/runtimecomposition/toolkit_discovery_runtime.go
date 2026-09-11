package runtimecomposition

import (
	"context"
	"fmt"
	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	discovery "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitdiscovery"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/redisdispatch"
	"github.com/jackc/pgx/v5/pgxpool"
	"time"
)

func newCurrentToolkitDiscoveryRuntime(pool *pgxpool.Pool, toolkits indexingapp.CurrentToolkitReader, settings indexingapp.CurrentToolkitSettingsValidator, catalogue *CurrentToolkitCatalogueSnapshot, capability *WorkerToolkitCapability, producer *redisdispatch.ToolkitAvailableToolsProducer, policy repos.ToolkitAvailableToolsDispatchPolicy, artifacts *repos.ToolkitDiscoveryArtifactRepository) (*currentToolkitDiscoveryRuntime, error) {
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
	resolver, err := discovery.NewCurrentAuthoritativeInputResolver(reader, settings, guardrails)
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
	return &currentToolkitDiscoveryRuntime{service: service, jobs: jobs, artifacts: artifacts}, nil
}

type currentToolkitDiscoveryRuntime struct {
	service   *discovery.Service
	jobs      *repos.ToolkitAvailableToolsJobsRepository
	artifacts *repos.ToolkitDiscoveryArtifactRepository
}

func (r *currentToolkitDiscoveryRuntime) Run(ctx context.Context) error {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		if _, err := r.jobs.ReclaimExpiredToolkitAvailableToolsRuns(ctx, 32); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("retire expired toolkit discovery: %w", err)
		}
		if _, err := r.artifacts.SweepOrphanToolkitDiscoveryArtifacts(ctx, 32); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("sweep toolkit discovery artifacts: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
