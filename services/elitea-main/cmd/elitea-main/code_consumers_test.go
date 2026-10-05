package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
	"github.com/jackc/pgx/v5/pgxpool"
)

func codeStartupConfig(t *testing.T, dsn string) runtimecomposition.Config {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "agent-checkpoint-connection")
	if err := os.WriteFile(path, []byte(dsn), 0600); err != nil {
		t.Fatal(err)
	}
	return runtimecomposition.Config{
		Enabled: true, AgentExecutionDispatchEnabled: true, CodeOwnerRecovery: &runtimecomposition.CodeOwnerConfig{},
		CodeWorkspace:      &runtimecomposition.CodeWorkspaceConfig{Revision: 1, RepositoryCapabilities: []string{"github"}, EgressAllowlist: []string{"api.github.com:443"}, Policy: storage.DefaultCodeWorkspacePolicy()},
		CodePlatform:       &runtimecomposition.CodePlatformDeploymentConfig{Revision: 1, ContentKeysFile: filepath.Join(root, "code-platform-content-keys.json"), BrokerPolicies: []storage.CodeBrokerPolicy{{Revision: 1, MaxCalls: 32, MaxTotalBytes: 1048576}}},
		CodeDebugArtifacts: &runtimecomposition.CodeDebugArtifactsConfig{AgentStateDSNFile: path},
	}
}

func TestCodeConsumerStartupDisabledDoesNotReadMaterialOrOpenPools(t *testing.T) {
	result, err := openCodeConsumerStartupWithFactory(nil, runtimecomposition.Config{}, runtimePoolResource{}, func(context.Context, string, runtimePoolSpec) (runtimePoolResource, error) {
		t.Fatal("disabled Code consumer opened a database pool")
		return runtimePoolResource{}, nil
	})
	if err != nil || result.workspaceCapabilities != nil || result.workspacePolicy != nil || result.platform != nil || result.debugStatePool != nil {
		t.Fatal("disabled Code consumer created dependencies", err)
	}
	result.Close()
}

func TestCodeConsumerStartupSuppliesExactOperatorContractsAndOwnsPool(t *testing.T) {
	config := codeStartupConfig(t, "postgres://fixture-only\n")
	closed := 0
	result, err := openCodeConsumerStartupWithFactory(context.Background(), config, runtimePoolResource{}, func(ctx context.Context, dsn string, spec runtimePoolSpec) (runtimePoolResource, error) {
		deadline, bounded := ctx.Deadline()
		if dsn != "postgres://fixture-only" || spec.role != "code-debug-agentstate" || spec.maxConns != 4 || !bounded || time.Until(deadline) > 15*time.Second {
			t.Fatal("Code debug used an unbounded or incorrect database owner")
		}
		return runtimePoolResource{pool: new(pgxpool.Pool), close: func() { closed++ }}, nil
	})
	if err != nil || result.workspaceCapabilities == nil || result.workspacePolicy == nil || *result.workspacePolicy != config.CodeWorkspace.Policy ||
		result.platform == nil || !result.platform.Enabled || result.platform.ContentKeysFile != config.CodePlatform.ContentKeysFile ||
		len(result.brokerPolicies) != 1 || result.brokerPolicies[0] != config.CodePlatform.BrokerPolicies[0] || result.debugStatePool == nil {
		t.Fatal("Code startup failed to supply its exact contracts", err)
	}
	config.CodeWorkspace.Policy.MaxFiles = 1
	config.CodePlatform.BrokerPolicies[0].MaxCalls = 1
	if result.workspacePolicy.MaxFiles != 1024 || result.brokerPolicies[0].MaxCalls != 32 {
		t.Fatal("startup dependencies alias mutable operator configuration")
	}
	result.Close()
	result.Close()
	if closed != 1 {
		t.Fatal("Code debug pool did not close exactly once", closed)
	}
}

func TestCodeDebugSharesExactCompiledReceiptPoolWithoutSecondOwner(t *testing.T) {
	config := codeStartupConfig(t, "postgres://fixture-only")
	config.RustCompiledSnapshots = &runtimecomposition.CompiledSnapshotConfig{AgentStateDSNFile: config.CodeDebugArtifacts.AgentStateDSNFile}
	if err := os.Remove(config.CodeDebugArtifacts.AgentStateDSNFile); err != nil {
		t.Fatal(err)
	}
	closed := 0
	compiled := runtimePoolResource{pool: new(pgxpool.Pool), close: func() { closed++ }}
	result, err := openCodeConsumerStartupWithFactory(context.Background(), config, compiled, func(context.Context, string, runtimePoolSpec) (runtimePoolResource, error) {
		t.Fatal("shared AgentState pool opened twice")
		return runtimePoolResource{}, nil
	})
	if err != nil || result.debugStatePool != compiled.pool {
		t.Fatal("exact compiled receipt pool was not reused", err)
	}
	result.Close()
	if closed != 0 {
		t.Fatal("Code debug stole the compiled receipt pool lifecycle")
	}
	compiled.close()
	if closed != 1 {
		t.Fatal("original pool owner did not close")
	}
	if _, err := openCodeConsumerStartupWithFactory(context.Background(), config, runtimePoolResource{}, nil); err == nil {
		t.Fatal("absent shared compiled pool was accepted")
	}
}

func TestCodeDebugDistinctMaterialUsesItsOwnBoundedPool(t *testing.T) {
	config := codeStartupConfig(t, "postgres://debug-fixture")
	config.RustCompiledSnapshots = &runtimecomposition.CompiledSnapshotConfig{AgentStateDSNFile: "/separate/compiled-agentstate-dsn"}
	compiled := runtimePoolResource{pool: new(pgxpool.Pool), close: func() { t.Fatal("closed unrelated compiled pool") }}
	debug := new(pgxpool.Pool)
	closed := 0
	result, err := openCodeConsumerStartupWithFactory(context.Background(), config, compiled, func(_ context.Context, dsn string, _ runtimePoolSpec) (runtimePoolResource, error) {
		if dsn != "postgres://debug-fixture" {
			t.Fatal("Code debug selected an unrelated AgentState database")
		}
		return runtimePoolResource{pool: debug, close: func() { closed++ }}, nil
	})
	if err != nil || result.debugStatePool != debug {
		t.Fatal("distinct Code debug pool was not opened", err)
	}
	result.Close()
	if closed != 1 {
		t.Fatal("Code debug pool lost lifecycle ownership")
	}
}

func TestCodeDebugRefusesUnsafeMaterialBeforeOpeningPool(t *testing.T) {
	refuseFactory := func(context.Context, string, runtimePoolSpec) (runtimePoolResource, error) {
		t.Fatal("unsafe material reached the pool constructor")
		return runtimePoolResource{}, nil
	}
	for _, dsn := range []string{"", "one\ntwo", "one\r\n", "one\x00", strings.Repeat("x", 16*1024+1)} {
		config := codeStartupConfig(t, dsn)
		if _, err := openCodeDebugStatePoolWithFactory(context.Background(), *config.CodeDebugArtifacts, refuseFactory); err == nil {
			t.Fatal("malformed or oversized DSN material accepted")
		}
	}
	config := codeStartupConfig(t, "postgres://fixture-only")
	path := config.CodeDebugArtifacts.AgentStateDSNFile
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := openCodeDebugStatePoolWithFactory(context.Background(), *config.CodeDebugArtifacts, refuseFactory); err == nil {
		t.Fatal("public debug DSN accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(path), "dsn-link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	config.CodeDebugArtifacts.AgentStateDSNFile = link
	if _, err := openCodeDebugStatePoolWithFactory(context.Background(), *config.CodeDebugArtifacts, refuseFactory); err == nil {
		t.Fatal("symlink debug DSN accepted")
	}
}

func TestCodeDebugPoolFailureSanitizesAndClosesPartialOwnership(t *testing.T) {
	config := codeStartupConfig(t, "postgres://synthetic-secret@fixture")
	closed := 0
	for _, cause := range []error{errors.New("synthetic-secret"), context.Canceled, context.DeadlineExceeded, nil} {
		result, err := openCodeConsumerStartupWithFactory(context.Background(), config, runtimePoolResource{}, func(context.Context, string, runtimePoolSpec) (runtimePoolResource, error) {
			return runtimePoolResource{close: func() { closed++ }}, cause
		})
		if err == nil || strings.Contains(err.Error(), "synthetic-secret") || result.workspaceCapabilities != nil || result.debugStatePool != nil || result.closeWorkspace != nil || result.closeDebugState != nil {
			t.Fatal("failed startup returned material or leaked ownership", err)
		}
		if cause == context.Canceled || cause == context.DeadlineExceeded {
			if !errors.Is(err, cause) {
				t.Fatal("Code startup lost cancellation identity", err)
			}
		}
	}
	if closed != 4 {
		t.Fatal("partial pools did not close", closed)
	}
}
