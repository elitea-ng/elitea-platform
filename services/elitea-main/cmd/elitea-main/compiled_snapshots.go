package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/security/securefile"
)

func openCompiledSnapshotStatePool(ctx context.Context, config runtimecomposition.Config) (runtimePoolResource, error) {
	return openCompiledSnapshotStatePoolWithFactory(ctx, config, openRuntimePostgresPool)
}
func openCompiledSnapshotStatePoolWithFactory(ctx context.Context, config runtimecomposition.Config, factory runtimePoolFactory) (runtimePoolResource, error) {
	if config.RustCompiledSnapshots == nil {
		return runtimePoolResource{}, nil
	}
	if ctx == nil || factory == nil || config.RustCompiledSnapshots.Validate() != nil {
		return runtimePoolResource{}, errors.New("compiled snapshot state pool configuration is invalid")
	}
	raw, err := securefile.Read(config.RustCompiledSnapshots.AgentStateDSNFile, 16*1024, securefile.PrivateMaterial)
	if err != nil {
		return runtimePoolResource{}, errors.New("compiled snapshot private agentstate DSN file is not readable")
	}
	dsn := strings.TrimSuffix(string(raw), "\n")
	if dsn == "" || strings.ContainsAny(dsn, "\r\n\x00") {
		return runtimePoolResource{}, errors.New("compiled snapshot private agentstate DSN file is malformed")
	}
	openCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resource, err := factory(openCtx, dsn, runtimePoolSpec{role: "compiled-snapshot-agentstate", maxConns: runtimecomposition.CompiledSnapshotAgentStateMaxConns})
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
		return runtimePoolResource{}, errors.New("compiled snapshot original agentstate database pool could not open")
	}
	if resource.pool == nil || resource.close == nil {
		if resource.close != nil {
			resource.close()
		}
		return runtimePoolResource{}, errors.New("compiled snapshot original agentstate database pool is incomplete")
	}
	return resource, nil
}
