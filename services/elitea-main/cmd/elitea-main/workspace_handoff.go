package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	agentexecutionapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/agentexecution"
	workspacehandoffapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/workspacehandoff"
	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacestore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
	dbrepos "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

// workspaceHandoffSweepInterval is how often the handoff sweeper runs.
const workspaceHandoffSweepInterval = 30 * time.Second

// workspaceHandoffComposition is what composeWorkspaceHandoff answers: the
// route, and the two services the later executor unit adapts to gRPC.
type workspaceHandoffComposition struct {
	Route   http.Handler
	Handoff *workspacehandoff.Service
	Store   *workspacestore.Service
}

// workspaceHandoffInputs are the deployment values of the handoff.
type workspaceHandoffInputs struct {
	// DedicatedStore is the optional dedicated physical bucket of the
	// `workspace` bucket type (chart storage.workspace.bucket, env
	// STORAGE_WORKSPACE_BUCKET), nil when unset.
	DedicatedStore      storage.ObjectStore
	DefaultBucketName   string
	DedicatedBucketName string
	Resources           platformconfig.CloudExecutorResources
}

// composeWorkspaceHandoff builds the desktop ⇄ cloud handoff (ADR-0034). It
// needs a database, an object store and the credential plane; without any of
// them it answers an empty composition and no route is mounted.
//
// The cloud executor stays off unless the default encryption at rest of every
// physical bucket it would write to is confirmed (decision 4): the policy
// service receives the answer of that startup check.
func composeWorkspaceHandoff(
	ctx context.Context,
	pool *pgxpool.Pool,
	objectStore storage.ObjectStore,
	inputs workspaceHandoffInputs,
	device workspacehandoff.DevicePolicyReader,
	canceller agentexecutionapi.CurrentAgentCanceller,
	recorder audit.Recorder,
	groupAuth apimw.AuthConfig,
	logger *slog.Logger,
) (workspaceHandoffComposition, error) {
	if pool == nil || objectStore == nil || groupAuth.PrincipalValidator == nil || device == nil {
		return workspaceHandoffComposition{}, nil
	}
	encrypted := confirmWorkspaceEncryption(ctx, objectStore, logger, "default")
	buckets := workspacestore.Buckets{Default: workspacestore.PhysicalStore{Name: inputs.DefaultBucketName, Store: objectStore}}
	if inputs.DedicatedStore != nil {
		encrypted = encrypted && confirmWorkspaceEncryption(ctx, inputs.DedicatedStore, logger, "dedicated")
		buckets.Dedicated = &workspacestore.PhysicalStore{Name: inputs.DedicatedBucketName, Store: inputs.DedicatedStore}
	}
	settings, err := dbrepos.NewCloudExecutorSettingsRepository(pool)
	if err != nil {
		return workspaceHandoffComposition{}, err
	}
	policy, err := workspacehandoff.NewPolicyService(settings, settings, device, encrypted, inputs.Resources)
	if err != nil {
		return workspaceHandoffComposition{}, err
	}
	storeRepo, err := dbrepos.NewWorkspaceStoreRepository(pool)
	if err != nil {
		return workspaceHandoffComposition{}, err
	}
	store, err := workspacestore.NewService(storeRepo, buckets, policy, workspacestore.RecorderAudit{Recorder: recorder}, logger)
	if err != nil {
		return workspaceHandoffComposition{}, err
	}
	handoffRepo, err := dbrepos.NewWorkspaceHandoffsRepository(pool)
	if err != nil {
		return workspaceHandoffComposition{}, err
	}
	handoff, err := workspacehandoff.NewService(handoffRepo, policy, store, turnCanceller{canceller: canceller}, recorder, logger)
	if err != nil {
		return workspaceHandoffComposition{}, err
	}
	// The harvested environment's vault (ADR-0034 decision 5, amended). The
	// writer and the reader take the SAME key source, so a secret this
	// process writes is one it can open.
	masterKey, err := encodedVaultMasterKeyFromEnv(os.LookupEnv)
	if err != nil {
		return workspaceHandoffComposition{}, fmt.Errorf("workspace handoff env vault: %w", err)
	}
	vaultWriter, err := dbrepos.NewCurrentSecretVaultRepository(pool, masterKey)
	if err != nil {
		return workspaceHandoffComposition{}, fmt.Errorf("workspace handoff env vault: %w", err)
	}
	vaultReader, err := storage.NewPostgresSecretVaultLoader(pool, masterKey)
	if err != nil {
		return workspaceHandoffComposition{}, fmt.Errorf("workspace handoff env vault: %w", err)
	}
	envVault, err := dbrepos.NewWorkspaceEnvVault(vaultWriter, vaultReader)
	if err != nil {
		return workspaceHandoffComposition{}, err
	}
	handoff.WithEnvVault(envVault)
	route, err := workspacehandoffapi.NewRoute(handoff, store, dbrepos.NewConversationsRepo(pool), groupAuth,
		legacyrbac.NewPostgresResolver(pool))
	if err != nil {
		return workspaceHandoffComposition{}, err
	}
	return workspaceHandoffComposition{Route: route, Handoff: handoff, Store: store}, nil
}

func confirmWorkspaceEncryption(ctx context.Context, store storage.ObjectStore, logger *slog.Logger, which string) bool {
	confirmed, err := storage.DefaultEncryptionConfirmed(ctx, store)
	if err != nil || !confirmed {
		logger.Warn("cloud executor: default encryption at rest of the "+which+" bucket is not confirmed; "+
			"Send to cloud stays off (ADR-0034 decision 4)", "err", err)
		return false
	}
	return true
}

// runWorkspaceHandoffSweeper runs the handoff's timed transitions and erasures
// and the store's object sweep until ctx ends. Replicas run it side by side:
// the claims are SKIP LOCKED and every transition is a compare-and-set.
func runWorkspaceHandoffSweeper(ctx context.Context, composition workspaceHandoffComposition, logger *slog.Logger) {
	if composition.Handoff == nil || composition.Store == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(workspaceHandoffSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if _, err := composition.Handoff.Sweep(ctx); err != nil && ctx.Err() == nil {
				logger.Warn("workspace handoff sweep had failures", "err", err)
			}
			if _, err := composition.Store.SweepObjects(ctx, 100); err != nil && ctx.Err() == nil {
				logger.Warn("workspace store sweep had failures", "err", err)
			}
		}
	}()
}

// turnCanceller stops a running cloud turn through the chat's own stop use
// case, as its owner.
type turnCanceller struct {
	canceller agentexecutionapi.CurrentAgentCanceller
}

func (c turnCanceller) CancelCloudTurn(ctx context.Context, projectID, actorUserID int64, responseMessageID string) error {
	if c.canceller == nil {
		return errors.New("no agent runtime is composed to cancel the cloud turn")
	}
	_, err := c.canceller.Cancel(ctx, agentexecutionapp.CurrentAgentCancelRequest{
		ProjectID: projectID, ActorUserID: actorUserID, ResponseMessageID: responseMessageID,
	})
	if err != nil {
		return fmt.Errorf("cancel cloud turn: %w", err)
	}
	return nil
}

// cloudExecutorResourcesFromEnv reads the chart's sandbox limits (ADR-0034
// decision 11).
func cloudExecutorResourcesFromEnv(lookup func(string) (string, bool)) (platformconfig.CloudExecutorResources, error) {
	resources := platformconfig.CloudExecutorResources{TurnWallSeconds: 3600}
	if raw, ok := lookup("ELITEA_CLOUD_EXECUTOR_TURN_WALL_SECONDS"); ok && raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value <= 0 {
			return resources, fmt.Errorf("ELITEA_CLOUD_EXECUTOR_TURN_WALL_SECONDS must be a positive integer")
		}
		resources.TurnWallSeconds = value
	}
	resources.CPU, _ = lookup("ELITEA_CLOUD_EXECUTOR_CPU")
	resources.Memory, _ = lookup("ELITEA_CLOUD_EXECUTOR_MEMORY")
	resources.Disk, _ = lookup("ELITEA_CLOUD_EXECUTOR_DISK")
	return resources, nil
}

// workspaceStorageFromEnv answers the workspace store's physical buckets: the
// deployment's own (STORAGE_CONTAINER) and, when STORAGE_WORKSPACE_BUCKET is
// set, a dedicated one on the same backend and credentials (ADR-0034
// decision 3). It also reads the chart's sandbox resources.
func workspaceStorageFromEnv(ctx context.Context, cfg storage.Config, lookup func(string) (string, bool)) (workspaceHandoffInputs, error) {
	inputs := workspaceHandoffInputs{DefaultBucketName: cfg.Container}
	resources, err := cloudExecutorResourcesFromEnv(lookup)
	if err != nil {
		return inputs, err
	}
	inputs.Resources = resources
	dedicated, _ := lookup("STORAGE_WORKSPACE_BUCKET")
	if dedicated == "" || dedicated == cfg.Container {
		return inputs, nil
	}
	dedicatedCfg := cfg
	dedicatedCfg.Container = dedicated
	dedicatedCfg.S3.Bucket = dedicated
	dedicatedCfg.Azure.ContainerName = dedicated
	dedicatedCfg.GCS.Bucket = dedicated
	store, err := newObjectStore(ctx, dedicatedCfg)
	if err != nil {
		return inputs, fmt.Errorf("create the dedicated workspace object store: %w", err)
	}
	inputs.DedicatedStore = store
	inputs.DedicatedBucketName = dedicated
	return inputs, nil
}
