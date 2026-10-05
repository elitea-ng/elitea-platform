package runtimecomposition

import (
	"context"
	"errors"
	"slices"

	toolkitsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkits"
	toolkitcall "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	toolkitexecution "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitexecution"
	scope "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/executionchildscope"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc/control"
)

// Deployment composition supplies the frozen operator policy and capabilities.
// Nil keeps the existing routes and material requirements unchanged.
func configureCodeSourceCapture(config Config, deps Dependencies) (*repos.ExecutionChildScopesRepository, repos.CodeDefinitionSourceReader, error) {
	if err := validateCodeConsumerDependencies(config, deps); err != nil {
		return nil, nil, err
	}
	if config.CodeOwnerRecovery == nil {
		if deps.CodeWorkspaceCapabilities != nil || deps.CodeWorkspacePolicy != nil || deps.CodeDebugStatePool != nil || deps.CodePlatform != nil && deps.CodePlatform.Enabled {
			return nil, nil, errors.New("Code consumers require original Code owner composition")
		}
		return nil, deps.OriginalCodeDefinitions, nil
	}
	if (deps.CodeWorkspaceCapabilities == nil) != (deps.CodeWorkspacePolicy == nil) {
		return nil, nil, errors.New("Code workspace requires its exact operator policy and capabilities")
	}
	purposes := []scope.Purpose{scope.CodeRecovery}
	if deps.CodeWorkspaceCapabilities != nil {
		purposes = append(purposes, scope.CodeWorkspace)
	}
	if deps.CodeDebugStatePool != nil {
		purposes = append(purposes, scope.CodeDebug)
	}
	if deps.CodePlatform != nil && deps.CodePlatform.Enabled {
		purposes = append(purposes, scope.PlatformBroker)
	}
	registry, err := repos.NewExecutionChildScopesRepository(deps.AdmissionPool, nil, purposes)
	if err != nil {
		return nil, nil, err
	}
	catalog, err := storage.NewRuntimeSavedChildCatalogSource(registry)
	if err != nil {
		return nil, nil, err
	}
	registry.WithSavedChildCatalog(catalog)
	sources, err := repos.NewCodeDefinitionSourceAdapter(registry)
	return registry, sources, err
}

func validateCodeConsumerDependencies(config Config, deps Dependencies) error {
	if config.CodeWorkspace != nil && (deps.CodeWorkspaceCapabilities == nil || deps.CodeWorkspacePolicy == nil || *deps.CodeWorkspacePolicy != config.CodeWorkspace.Policy) {
		return errors.New("Code workspace requires its configured capabilities and exact policy")
	}
	if config.CodePlatform != nil && (deps.CodePlatform == nil || !deps.CodePlatform.Enabled ||
		deps.CodePlatform.ContentKeysFile != config.CodePlatform.ContentKeysFile || !slices.Equal(deps.CodeBrokerPolicies, config.CodePlatform.BrokerPolicies)) {
		return errors.New("Code platform requires its configured private material and exact policies")
	}
	if config.CodeDebugArtifacts != nil && deps.CodeDebugStatePool == nil {
		return errors.New("Code debug requires its configured AgentState pool")
	}
	if (deps.CodeWorkspaceCapabilities != nil || deps.CodeDebugStatePool != nil || deps.CodePlatform != nil && deps.CodePlatform.Enabled) && deps.ObjectStore == nil {
		return errors.New("Code consumers require native object storage")
	}
	return nil
}

func attachCodeConsumers(server *storage.ContentServer, config Config, deps Dependencies, visits *repos.CodeIntentRepository,
	client *storage.CodeOwnerClient, signer *storage.CodeOwnerGrantSigner, sandbox *control.SandboxGrantIssuer,
	bundles *storage.SandboxBundleStore, authorizer storage.AgentRuntimeContextAuthorizer,
	toolkits toolkitexecution.CurrentMCPToolkitReader, settings toolkitexecution.CurrentToolkitSettingsResolver,
	runs *toolkitcall.RunService, maintenance *executionReplayRetentionJanitor) error {
	if visits == nil {
		return nil
	}
	workspaceVerifier := deps.OriginalCodeWorkspaceVerifier
	brokerVerifier := deps.OriginalCodeBrokerVerifier
	if deps.CodeWorkspaceCapabilities != nil {
		workspace, err := storage.NewCodeWorkspaceService(visits, toolkits, settings, deps.CodeWorkspaceCapabilities,
			bundles, storage.NewPostgresCodeWorkspaceReceipts(), *deps.CodeWorkspacePolicy, 4)
		if err != nil {
			return err
		}
		reads, err := storage.NewCodeWorkspaceReadServer(signer, visits, workspace)
		if err != nil {
			return err
		}
		workspaceVerifier = workspace
		server.WithCodeWorkspaces(workspace).WithCodeWorkspaceReads(reads)
	}
	if deps.CodePlatform != nil && deps.CodePlatform.Enabled {
		if config.CodeOwnerRecovery == nil || sandbox == nil || deps.CurrentConfigurations == nil || runs == nil {
			return errors.New("Code platform requires the original owner and native toolkit run service")
		}
		journal, err := repos.NewCodePlatformCallsRepository(deps.AdmissionPool)
		if err != nil {
			return err
		}
		policies, err := storage.NewCodeBrokerPolicies(deps.CodeBrokerPolicies)
		if err != nil {
			return err
		}
		brokerVerifier = policies.WithEffectJournal(journal)
		keys, err := deps.CodePlatform.LoadContentKeys()
		if err != nil {
			return err
		}
		privateContent, err := storage.NewCodePlatformContent(deps.ObjectStore, keys)
		if err != nil {
			return err
		}
		secretPolicy, err := storage.NewCurrentCodeSecretPolicy(deps.AdmissionPool)
		if err != nil {
			return err
		}
		secrets, err := storage.NewRuntimeCodeSecretService(authorizer, deps.PermissionResolver,
			deps.CurrentConfigurations.scope, deps.CurrentConfigurations.vaultLoader, secretPolicy)
		if err != nil {
			return err
		}
		factory, err := storage.NewCodePlatformNativeFactory(journal, privateContent, sandbox, authorizer,
			deps.PermissionResolver, secrets, repos.NewApplicationsRepo(deps.AdmissionPool),
			toolkitsapi.NewPostgresRepository(deps.AdmissionPool), runs, journal)
		if err != nil {
			return err
		}
		pump, err := storage.NewCodePlatformPump(visits, client, signer, factory, journal, config.CodeOwnerRecovery.MainWorkloadIdentity)
		if err != nil {
			return err
		}
		server.WithCodePlatformPump(pump)
	}
	visits.WithPreparedExtensions(workspaceVerifier, brokerVerifier)
	if deps.CodeDebugStatePool != nil {
		writer, err := storage.NewPostgresCodeDebugAuthority(deps.CodeDebugStatePool)
		if err != nil {
			return err
		}
		artifacts, err := repos.NewCodeDebugArtifactsRepository(deps.AdmissionPool, deps.ObjectStore, visits, writer)
		if err != nil {
			return err
		}
		debug, err := storage.NewRuntimeCodeDebugArtifactService(artifacts)
		if err != nil {
			return err
		}
		if maintenance == nil || maintenance.pruner == nil {
			return errors.New("Code debug requires its bounded cleanup owner")
		}
		maintenance.pruner = &codeDebugMaintenance{replay: maintenance.pruner, artifacts: artifacts}
		server.WithCodeDebugArtifacts(debug)
	}
	return nil
}

type codeDebugMaintenance struct {
	replay    executionReplayProgressPruner
	artifacts *repos.CodeDebugArtifactsRepository
}

func (m *codeDebugMaintenance) PruneExpiredReplayProgress(ctx context.Context) (int64, error) {
	count, replayErr := m.replay.PruneExpiredReplayProgress(ctx)
	_, cleanupErr := m.artifacts.CleanupCodeDebugStaging(ctx, 16)
	return count, errors.Join(replayErr, cleanupErr)
}
