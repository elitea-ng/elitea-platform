package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/security/securefile"
	"github.com/jackc/pgx/v5/pgxpool"
)

// codeConsumerStartup owns optional Main readers and the separate AgentState pool.
// A shared compiled receipt pool retains its original lifecycle owner.
type codeConsumerStartup struct {
	workspaceCapabilities *storage.CodeRepositoryCapabilities
	workspacePolicy       *storage.CodeWorkspacePolicy
	platform              *runtimecomposition.CodePlatformConfig
	brokerPolicies        []storage.CodeBrokerPolicy
	debugStatePool        *pgxpool.Pool
	closeWorkspace        func()
	closeDebugState       func()
}

func (c *codeConsumerStartup) Close() {
	if c == nil {
		return
	}
	if c.closeWorkspace != nil {
		c.closeWorkspace()
		c.closeWorkspace = nil
	}
	if c.closeDebugState != nil {
		c.closeDebugState()
		c.closeDebugState = nil
	}
}

func openCodeConsumerStartup(ctx context.Context, config runtimecomposition.Config, compiledState runtimePoolResource) (codeConsumerStartup, error) {
	return openCodeConsumerStartupWithFactory(ctx, config, compiledState, openRuntimePostgresPool)
}

func openCodeConsumerStartupWithFactory(ctx context.Context, config runtimecomposition.Config, compiledState runtimePoolResource, factory runtimePoolFactory) (result codeConsumerStartup, err error) {
	if config.CodeWorkspace == nil && config.CodePlatform == nil && config.CodeDebugArtifacts == nil {
		return result, nil
	}
	if ctx == nil || !config.Enabled || !config.AgentExecutionDispatchEnabled || config.CodeOwnerRecovery == nil {
		return result, errors.New("Code consumer startup requires original Code owner composition")
	}
	defer func() {
		if err != nil {
			result.Close()
			result = codeConsumerStartup{}
		}
	}()
	if config.CodeWorkspace != nil {
		result.workspaceCapabilities, result.closeWorkspace, err = config.CodeWorkspace.OpenCapabilities()
		if err != nil {
			return result, err
		}
		policy := config.CodeWorkspace.Policy
		result.workspacePolicy = &policy
	}
	if config.CodePlatform != nil {
		if err = config.CodePlatform.Validate(); err != nil {
			return result, err
		}
		result.platform = &runtimecomposition.CodePlatformConfig{Enabled: true, ContentKeysFile: config.CodePlatform.ContentKeysFile}
		result.brokerPolicies = append([]storage.CodeBrokerPolicy(nil), config.CodePlatform.BrokerPolicies...)
	}
	if config.CodeDebugArtifacts != nil {
		if err = config.CodeDebugArtifacts.Validate(); err != nil {
			return result, err
		}
		if config.RustCompiledSnapshots != nil && config.CodeDebugArtifacts.AgentStateDSNFile == config.RustCompiledSnapshots.AgentStateDSNFile {
			if compiledState.pool == nil || compiledState.close == nil {
				return result, errors.New("Code debug shared AgentState pool is unavailable")
			}
			result.debugStatePool = compiledState.pool
		} else {
			var resource runtimePoolResource
			resource, err = openCodeDebugStatePoolWithFactory(ctx, *config.CodeDebugArtifacts, factory)
			if err != nil {
				return result, err
			}
			result.debugStatePool, result.closeDebugState = resource.pool, resource.close
		}
	}
	return result, nil
}

func openCodeDebugStatePoolWithFactory(ctx context.Context, config runtimecomposition.CodeDebugArtifactsConfig, factory runtimePoolFactory) (runtimePoolResource, error) {
	if ctx == nil || factory == nil || config.Validate() != nil {
		return runtimePoolResource{}, errors.New("Code debug state pool configuration is invalid")
	}
	raw, err := securefile.Read(config.AgentStateDSNFile, 16*1024, securefile.PrivateMaterial)
	if err != nil {
		return runtimePoolResource{}, errors.New("Code debug private AgentState DSN file is unreadable")
	}
	defer clear(raw)
	dsn := strings.TrimSuffix(string(raw), "\n")
	if dsn == "" || strings.ContainsAny(dsn, "\r\n\x00") {
		return runtimePoolResource{}, errors.New("Code debug private AgentState DSN file is malformed")
	}
	openCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resource, err := factory(openCtx, dsn, runtimePoolSpec{role: "code-debug-agentstate", maxConns: runtimecomposition.CodeDebugAgentStateMaxConns})
	if err != nil {
		if resource.close != nil {
			resource.close()
		}
		if errors.Is(err, context.Canceled) {
			return runtimePoolResource{}, context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return runtimePoolResource{}, context.DeadlineExceeded
		}
		return runtimePoolResource{}, errors.New("Code debug original AgentState database pool could not open")
	}
	if resource.pool == nil || resource.close == nil {
		if resource.close != nil {
			resource.close()
		}
		return runtimePoolResource{}, errors.New("Code debug original AgentState database pool is incomplete")
	}
	return resource, nil
}
