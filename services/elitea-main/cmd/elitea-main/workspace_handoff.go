package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	agentexecutionapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/agentexecution"
	v2secrets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	workspacehandoffapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/workspacehandoff"
	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspaceexecutor"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspaceruntime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacestore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/events"
	dbrepos "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
)

// workspaceHandoffSweepInterval is how often the handoff sweeper runs.
const workspaceHandoffSweepInterval = 30 * time.Second

// workspaceHandoffComposition is what composeWorkspaceHandoff answers: the
// route, and the two services the later executor unit adapts to gRPC.
type workspaceHandoffComposition struct {
	Route   http.Handler
	Handoff *workspacehandoff.Service
	Store   *workspacestore.Service
	// Encryption is the startup answer for storage encryption at rest
	// (workspaceEncryption*), which the admin form shows.
	Encryption string
}

// The startup answers for the storage encryption at rest of the buckets the
// cloud executor writes (ADR-0034 decision 4).
const (
	// workspaceEncryptionConfirmed: the backend reports a default rule.
	workspaceEncryptionConfirmed = "confirmed"
	// workspaceEncryptionAttested: the backend cannot report it, and the
	// operator attests it (ELITEA_CLOUD_EXECUTOR_STORAGE_ENCRYPTION_ATTESTED).
	workspaceEncryptionAttested = "attested"
	// workspaceEncryptionRefused: no rule, an error, or not attested. Send
	// to cloud stays off.
	workspaceEncryptionRefused = "refused"
)

// workspaceEncryptionAttestedEnv is the operator's attestation (chart
// main.env). It applies ONLY when the backend cannot report its encryption;
// a backend that reports "no default rule" is refused whatever it says.
const workspaceEncryptionAttestedEnv = "ELITEA_CLOUD_EXECUTOR_STORAGE_ENCRYPTION_ATTESTED"

// workspaceHandoffInputs are the deployment values of the handoff.
type workspaceHandoffInputs struct {
	// DedicatedStore is the optional dedicated physical bucket of the
	// `workspace` bucket type (chart storage.workspace.bucket, env
	// STORAGE_WORKSPACE_BUCKET), nil when unset.
	DedicatedStore      storage.ObjectStore
	DefaultBucketName   string
	DedicatedBucketName string
	Resources           platformconfig.CloudExecutorResources
	// EncryptionAttested is workspaceEncryptionAttestedEnv.
	EncryptionAttested bool
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
	liveEvents workspacehandoff.StateEvents,
	logger *slog.Logger,
) (workspaceHandoffComposition, error) {
	if pool == nil || objectStore == nil || groupAuth.PrincipalValidator == nil || device == nil {
		return workspaceHandoffComposition{}, nil
	}
	encryption := checkWorkspaceEncryption(ctx, objectStore, inputs.EncryptionAttested, logger, "default")
	attestedBuckets := []string{}
	if encryption == workspaceEncryptionAttested {
		attestedBuckets = append(attestedBuckets, inputs.DefaultBucketName)
	}
	buckets := workspacestore.Buckets{Default: workspacestore.PhysicalStore{Name: inputs.DefaultBucketName, Store: objectStore}}
	if inputs.DedicatedStore != nil {
		dedicated := checkWorkspaceEncryption(ctx, inputs.DedicatedStore, inputs.EncryptionAttested, logger, "dedicated")
		if dedicated == workspaceEncryptionAttested {
			attestedBuckets = append(attestedBuckets, inputs.DedicatedBucketName)
		}
		encryption = combineWorkspaceEncryption(encryption, dedicated)
		buckets.Dedicated = &workspacestore.PhysicalStore{Name: inputs.DedicatedBucketName, Store: inputs.DedicatedStore}
	}
	encrypted := encryption != workspaceEncryptionRefused
	if encryption == workspaceEncryptionAttested {
		logger.Warn("cloud executor: storage encryption at rest is ATTESTED BY THE OPERATOR, not reported by the "+
			"backend (ADR-0034 decision 4)", "env", workspaceEncryptionAttestedEnv, "buckets", attestedBuckets)
		recorded, err := dbrepos.RecordCloudExecutorEncryptionAttestation(ctx, pool, attestedBuckets)
		if err != nil {
			// The audit row is required evidence: without it the
			// attestation does not apply.
			logger.Error("cloud executor: the attestation could not be audited; Send to cloud stays off", "err", err)
			encryption, encrypted = workspaceEncryptionRefused, false
		} else if recorded {
			logger.Info("cloud executor: the attestation is recorded in the audit trail")
		}
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
	envVault, err := dbrepos.NewWorkspaceEnvVault(vaultWriter, vaultReader, v2secrets.NewHandler(pool))
	if err != nil {
		return workspaceHandoffComposition{}, err
	}
	handoff.WithEnvVault(envVault)
	// Live state changes for the web app and the desktop (scope B); nil when
	// the deployment has no live-update plane, and the clients poll.
	handoff.WithStateEvents(liveEvents)
	route, err := workspacehandoffapi.NewRoute(handoff, store, dbrepos.NewConversationsRepo(pool), groupAuth,
		legacyrbac.NewPostgresResolver(pool))
	if err != nil {
		return workspaceHandoffComposition{}, err
	}
	return workspaceHandoffComposition{Route: route, Handoff: handoff, Store: store, Encryption: encryption}, nil
}

// checkWorkspaceEncryption asks the backend first. Only a backend that cannot
// report its encryption may be attested by the operator; the default is a
// refusal.
func checkWorkspaceEncryption(ctx context.Context, store storage.ObjectStore, attested bool, logger *slog.Logger, which string) string {
	confirmed, err := storage.DefaultEncryptionConfirmed(ctx, store)
	switch {
	case err == nil && confirmed:
		return workspaceEncryptionConfirmed
	case errors.Is(err, storage.ErrEncryptionUnreportable) && attested:
		return workspaceEncryptionAttested
	case errors.Is(err, storage.ErrEncryptionUnreportable):
		logger.Warn("cloud executor: the "+which+" bucket's backend cannot report default encryption at rest; "+
			"Send to cloud stays off until the operator sets "+workspaceEncryptionAttestedEnv+"=true "+
			"(ADR-0034 decision 4)", "err", err)
	default:
		logger.Warn("cloud executor: default encryption at rest of the "+which+" bucket is not confirmed; "+
			"Send to cloud stays off (ADR-0034 decision 4)", "err", err)
	}
	return workspaceEncryptionRefused
}

// combineWorkspaceEncryption: a refusal of either bucket refuses; an
// attestation of either makes the whole answer attested.
func combineWorkspaceEncryption(a, b string) string {
	switch {
	case a == workspaceEncryptionRefused || b == workspaceEncryptionRefused:
		return workspaceEncryptionRefused
	case a == workspaceEncryptionAttested || b == workspaceEncryptionAttested:
		return workspaceEncryptionAttested
	default:
		return workspaceEncryptionConfirmed
	}
}

// runWorkspaceHandoffSweeper runs the handoff's timed transitions and erasures
// and the store's object sweep until ctx ends, and, when the cloud executor
// runs turns (lifecycle not nil), ends the running cloud turns whose
// execution is already terminal. Replicas run it side by side: the claims
// are SKIP LOCKED and every transition is a compare-and-set.
func runWorkspaceHandoffSweeper(ctx context.Context, composition workspaceHandoffComposition,
	lifecycle workspaceruntime.TurnLifecycle, logger *slog.Logger) {
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
			// Finished turns first: a turn whose execution ended but whose
			// finish hook never ran is ended as finished, not reaped as a
			// dead worker by the sweep below.
			if lifecycle != nil {
				if _, err := lifecycle.ReconcileFinished(ctx); err != nil && ctx.Err() == nil {
					logger.Warn("cloud turn reconciliation had failures", "err", err)
				}
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
	if raw, ok := lookup(workspaceEncryptionAttestedEnv); ok && raw != "" {
		attested, err := strconv.ParseBool(raw)
		if err != nil {
			return inputs, fmt.Errorf("%s must be true or false", workspaceEncryptionAttestedEnv)
		}
		inputs.EncryptionAttested = attested
	}
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

// handoffLiveEvents publishes the handoff's state changes on the live-update
// plane (events.WorkspaceChannel), each frame naming its audience for the SSE
// relay to filter on and strip.
type handoffLiveEvents struct {
	publisher *events.Publisher
}

// newHandoffLiveEvents answers nil (no events) without a live-update bus.
func newHandoffLiveEvents(bus events.Bus) workspacehandoff.StateEvents {
	if bus == nil {
		return nil
	}
	return handoffLiveEvents{publisher: events.NewWorkspacePublisher(bus)}
}

func (h handoffLiveEvents) HandoffChanged(ctx context.Context, projectID int64, audience []int64, event workspacehandoff.StateEvent) {
	h.publisher.Emit(ctx, strconv.FormatInt(projectID, 10), workspacehandoff.EventType, struct {
		workspacehandoff.StateEvent
		Audience []int64 `json:"audience"`
	}{StateEvent: event, Audience: audience})
}

// deferredAgentCanceller is the handoff's turn canceller, bound to the
// runtime's stop use case once the runtime plane is composed (the handoff is
// composed first: the executor adapters are runtime dependencies). Unbound,
// a cancel fails, which the handoff logs: no runtime, no cloud turn to stop.
type deferredAgentCanceller struct {
	mu   sync.RWMutex
	next agentexecutionapi.CurrentAgentCanceller
}

func (d *deferredAgentCanceller) bind(next agentexecutionapi.CurrentAgentCanceller) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.next = next
}

func (d *deferredAgentCanceller) Cancel(ctx context.Context, request agentexecutionapp.CurrentAgentCancelRequest) (agentexecutionapp.CurrentAgentCancelOutcome, error) {
	d.mu.RLock()
	next := d.next
	d.mu.RUnlock()
	if next == nil {
		return agentexecutionapp.CurrentAgentCancelOutcome{}, errors.New("no agent runtime is composed to cancel the cloud turn")
	}
	return next.Cancel(ctx, request)
}

// workspaceExecutorComposition is the cloud executor's adapter set; nil when
// the handoff is not composed.
type workspaceExecutorComposition struct {
	adapter *workspaceexecutor.Adapter
}

// dependencies answers all four seams or none (the runtime refuses a partial
// set, and opens the Local work fence only for a complete one).
func (c workspaceExecutorComposition) dependencies(egress workspaceruntime.Egress) runtimecomposition.WorkspaceDependencies {
	if c.adapter == nil {
		return runtimecomposition.WorkspaceDependencies{Egress: egress}
	}
	return runtimecomposition.WorkspaceDependencies{
		Store: c.adapter, Turns: c.adapter, Policies: c.adapter, Lifecycle: c.adapter,
		Egress: egress,
	}
}

// lifecycle is the reconciler the sweeper runs, only when the runtime plane
// (and so the executor) is composed.
func (c workspaceExecutorComposition) lifecycle(runtimeComposed bool) workspaceruntime.TurnLifecycle {
	if c.adapter == nil || !runtimeComposed {
		return nil
	}
	return c.adapter
}

// composeWorkspaceExecutor adapts the composed handoff to the executor seam
// (ADR-0034 decisions 7, 9, 11, 14).
func composeWorkspaceExecutor(pool *pgxpool.Pool, handoff workspaceHandoffComposition,
	resources platformconfig.CloudExecutorResources, logger *slog.Logger) (workspaceExecutorComposition, error) {
	if pool == nil || handoff.Handoff == nil || handoff.Store == nil {
		return workspaceExecutorComposition{}, nil
	}
	parsed, err := parseCloudExecutorResources(resources)
	if err != nil {
		return workspaceExecutorComposition{}, err
	}
	repo, err := dbrepos.NewWorkspaceHandoffsRepository(pool)
	if err != nil {
		return workspaceExecutorComposition{}, err
	}
	adapter, err := workspaceexecutor.New(handoff.Handoff, handoff.Store, workspaceThreads{repo: repo}, parsed, logger)
	if err != nil {
		return workspaceExecutorComposition{}, err
	}
	return workspaceExecutorComposition{adapter: adapter}, nil
}

// workspaceThreads is workspaceexecutor.Threads over the handoffs repository.
type workspaceThreads struct {
	repo *dbrepos.WorkspaceHandoffsRepository
}

func (t workspaceThreads) Read(ctx context.Context, thread workspacehandoff.Thread) (workspacehandoff.Handoff, bool, error) {
	return t.repo.Read(ctx, thread)
}

func (t workspaceThreads) Execution(ctx context.Context, executionID string, generation uint64) (workspaceexecutor.Execution, bool, error) {
	execution, ok, err := t.repo.CloudTurnExecution(ctx, executionID, generation)
	return workspaceexecutor.Execution(execution), ok, err
}

func (t workspaceThreads) FinishedCloudTurns(ctx context.Context, limit int) ([]workspacehandoff.Handoff, error) {
	return t.repo.FinishedCloudTurns(ctx, limit)
}

func (t workspaceThreads) ReclaimPending(ctx context.Context, executionID string) (bool, error) {
	return t.repo.ReclaimPending(ctx, executionID)
}

// parseCloudExecutorResources reads the chart's sandbox limits: CPU as cores
// or millicores ("2", "1.5", "500m"), memory and disk as bytes with an
// optional binary or decimal suffix ("4Gi", "512Mi", "1G"). Empty is zero (the
// worker's deployment default); anything else stops startup.
func parseCloudExecutorResources(resources platformconfig.CloudExecutorResources) (workspaceexecutor.Resources, error) {
	out := workspaceexecutor.Resources{}
	if resources.TurnWallSeconds > 0 {
		out.TurnWallSeconds = uint64(resources.TurnWallSeconds)
	}
	var err error
	if out.CPUMillis, err = parseCPUMillis(resources.CPU); err != nil {
		return out, fmt.Errorf("ELITEA_CLOUD_EXECUTOR_CPU: %w", err)
	}
	if out.MemoryBytes, err = parseQuantityBytes(resources.Memory); err != nil {
		return out, fmt.Errorf("ELITEA_CLOUD_EXECUTOR_MEMORY: %w", err)
	}
	if out.DiskBytes, err = parseQuantityBytes(resources.Disk); err != nil {
		return out, fmt.Errorf("ELITEA_CLOUD_EXECUTOR_DISK: %w", err)
	}
	return out, nil
}

func parseCPUMillis(raw string) (uint64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	if milli, ok := strings.CutSuffix(raw, "m"); ok {
		value, err := strconv.ParseUint(milli, 10, 32)
		if err != nil || value == 0 {
			return 0, fmt.Errorf("%q is not a CPU quantity", raw)
		}
		return value, nil
	}
	cores, err := strconv.ParseFloat(raw, 64)
	if err != nil || cores <= 0 || cores > 4096 {
		return 0, fmt.Errorf("%q is not a CPU quantity", raw)
	}
	return uint64(cores*1000 + 0.5), nil
}

var quantitySuffixes = []struct {
	suffix string
	factor uint64
}{
	{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40},
	{"k", 1e3}, {"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12},
}

func parseQuantityBytes(raw string) (uint64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	number, factor := raw, uint64(1)
	for _, candidate := range quantitySuffixes {
		if trimmed, ok := strings.CutSuffix(raw, candidate.suffix); ok {
			number, factor = trimmed, candidate.factor
			break
		}
	}
	value, err := strconv.ParseUint(number, 10, 64)
	if err != nil || value == 0 || value > (1<<50)/factor {
		return 0, fmt.Errorf("%q is not a byte quantity", raw)
	}
	return value * factor, nil
}
