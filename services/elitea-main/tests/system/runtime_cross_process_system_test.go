package system_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	configurationdomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/configurations"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/jackc/pgx/v5/pgxpool"
	redis "github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"
)

const (
	systemTestOptIn = "ELITEA_RUNTIME_SYSTEM_TEST"
	commandStream   = "elitea:runtime:commands"
	consumerGroup   = "elitea-runtime-v1"
	workloadSession = "system-session-1"
	producerID      = "system-producer-1"
	workloadID      = "spiffe://elitea.test/runtime/python-worker"
	signingKeyID    = "system-ed25519-key-1"
	revisionID      = "configuration-revision-system-1"

	producerPassword = "system-producer-password-5681"
	workerPassword   = "system-worker-password-5681"
	observerPassword = "system-observer-password-5681"
	authPassword     = "system-auth-password-5681"
	publicSecret     = "system-public-session-secret-5681"
	testReclaimIdle  = 60 * time.Second

	catalogRevision = "a78d3654f99d8ff89ca7233f20a66d676e564f79"
	catalogDigest   = "4a96e3ab8e3842ebf2645a851aeb12e3e2343f28e7d024c1a2960eb4ec254351"
	schemaID        = "elitea.configuration.openapi"
	schemaRevision  = catalogRevision
	schemaDigest    = "1c43c41a5304c6f73c68deebd37ba70f8c2266a59dfd4f9d4fa20b819e7ab3f1"
)

// TestProductionRuntimeCrossProcessSystem drives the production publisher,
// control, content and output components through a private admission seam.
// Public route/RBAC compatibility remains a separate deployment gate.
func TestProductionRuntimeCrossProcessSystem(t *testing.T) {
	if os.Getenv(systemTestOptIn) != "1" {
		t.Skip("set ELITEA_RUNTIME_SYSTEM_TEST=1 to run the Docker-backed cross-process runtime system test")
	}

	repositoryRoot := findRepositoryRoot(t)
	python := systemPython(t, repositoryRoot)
	requireCommand(t, "docker")
	requirePythonRuntime(t, python, repositoryRoot)

	root := canonicalTempDir(t)
	pki := generateRuntimePKI(t, root)
	signing := generateSigningMaterial(t, root)
	spoolRoot := filepath.Join(root, "spool")
	mustMkdir(t, spoolRoot, 0o700)
	spoolKeyPath := filepath.Join(root, "spool.key")
	writeFile(t, spoolKeyPath, bytes.Repeat([]byte{0x5a}, 32), 0o600)
	producerPasswordPath := filepath.Join(root, "producer.password")
	workerPasswordPath := filepath.Join(root, "worker.password")
	writeFile(t, producerPasswordPath, []byte(producerPassword), 0o600)
	writeFile(t, workerPasswordPath, []byte(workerPassword), 0o600)

	postgresPort := freePort(t)
	legacyRedisPort := freePort(t)
	controlRedisPort := freePort(t)
	publicPort := freePort(t)
	controlPort := freePort(t)
	outputPort := freePort(t)
	contentPort := freePort(t)
	authConfigPath := writeRuntimeAuthConfig(t, root, controlRedisPort, publicPort, pki)

	containers := &containerSet{}
	t.Cleanup(containers.stopAll)
	postgresName := containers.start(t,
		"postgres", "postgres:16-alpine",
		[]string{
			"-e", "POSTGRES_USER=elitea",
			"-e", "POSTGRES_PASSWORD=elitea",
			"-e", "POSTGRES_DB=elitea",
			"-p", fmt.Sprintf("127.0.0.1:%d:5432", postgresPort),
		},
	)
	legacyRedisName := containers.start(t,
		"legacy-redis", "redis:7-alpine",
		[]string{"-p", fmt.Sprintf("127.0.0.1:%d:6379", legacyRedisPort)},
		"redis-server", "--save", "", "--appendonly", "no",
	)
	_ = legacyRedisName

	redisConfigDir := filepath.Join(root, "redis")
	mustMkdir(t, redisConfigDir, 0o755)
	prepareTLSRedisConfig(t, redisConfigDir, pki)
	controlRedisName := containers.start(t,
		"control-redis", "redis:7-alpine",
		[]string{
			"-p", fmt.Sprintf("127.0.0.1:%d:6379", controlRedisPort),
			"-v", redisConfigDir + ":/runtime:ro",
		},
		"redis-server", "/runtime/redis.conf",
	)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	databaseURL := fmt.Sprintf("postgres://elitea:elitea@127.0.0.1:%d/elitea?sslmode=disable", postgresPort)
	pool := waitForPostgres(t, ctx, databaseURL, containers, postgresName)
	defer pool.Close()
	bootstrapDatabase(t, ctx, repositoryRoot, pool)

	mainBinary := filepath.Join(root, "elitea-main")
	migrateBinary := filepath.Join(root, "elitea-migrate")
	buildGoBinary(t, repositoryRoot, mainBinary, "./cmd/elitea-main")
	buildGoBinary(t, repositoryRoot, migrateBinary, "./cmd/elitea-migrate")
	runCommand(t, filepath.Join(repositoryRoot, "services", "elitea-main"), []string{"DATABASE_URL=" + databaseURL}, migrateBinary, "-all-tenants")

	settingsMarker := "REDIS-MUST-NEVER-CONTAIN-SETTINGS-5681-" + strings.Repeat("x", 24*1024)
	settings := []byte(`{"scope":"` + settingsMarker + `"}`)
	seedRuntimeFixtures(t, ctx, pool, settings)

	observer := newControlRedisClient(t, controlRedisPort, "observer", observerPassword, pki.caPath)
	defer func() { _ = observer.Close() }()
	waitForRedis(t, ctx, observer, containers, controlRedisName)
	if err := observer.XGroupCreateMkStream(ctx, commandStream, consumerGroup, "0-0").Err(); err != nil {
		t.Fatalf("provision single runtime consumer group: %v", err)
	}
	assertBrokerLeastPrivilege(t, ctx, controlRedisPort, pki.caPath, observer)
	redisFaultProxy := startRedisRetirementResponseDropProxy(
		t,
		pki,
		fmt.Sprintf("localhost:%d", controlRedisPort),
	)
	workerRedisPort := redisFaultProxy.Port()

	mainLog := filepath.Join(root, "elitea-main.log")
	mainEnvironment := runtimeMainEnvironment(
		databaseURL,
		legacyRedisPort,
		controlRedisPort,
		publicPort,
		controlPort,
		outputPort,
		contentPort,
		producerPasswordPath,
		authConfigPath,
		pki,
		signing,
	)
	mainProcess := startChild(t, "elitea-main", mainLog, filepath.Join(repositoryRoot, "services", "elitea-main"), mainEnvironment, mainBinary)
	t.Cleanup(func() { mainProcess.stop(t) })
	publicBaseURL := fmt.Sprintf("http://127.0.0.1:%d", publicPort)
	waitForMain(t, ctx, publicBaseURL, mainProcess)
	outputFaultProxy := startOutputACKDropProxy(t, fmt.Sprintf("localhost:%d", outputPort), pki)
	workerOutputPort := outputFaultProxy.port(t)

	badSignatureConfigPath := writeWorkerConfig(t, root, "bad-signature", workerRedisPort, controlPort, workerOutputPort, contentPort, publicPort, workerPasswordPath, pki, signing.badKeyringPath, spoolRoot, spoolKeyPath)
	badSignatureWorker := startWorker(t, python, repositoryRoot, badSignatureConfigPath, filepath.Join(root, "worker-bad-signature.log"))
	t.Cleanup(func() { badSignatureWorker.stop(t) })
	waitForWorkerConsumer(t, ctx, observer, "worker-bad-signature", badSignatureWorker)

	admission := submitValidationPrivate(t, ctx, pool, settings)
	waitForPendingDelivery(t, ctx, observer, pool, admission.ExecutionID, "worker-bad-signature", badSignatureWorker)
	assertReferenceOnlyRedisEntry(t, ctx, observer, settingsMarker)
	assertNoClaim(t, ctx, pool, admission.ExecutionID)
	badSignatureWorker.stop(t)

	// Preserve a real pending delivery through every durable infrastructure
	// process restart before any authorized worker can claim it.
	containers.restart(t, controlRedisName)
	waitForRedis(t, ctx, observer, containers, controlRedisName)
	containers.restart(t, postgresName)
	waitForPostgresPool(t, ctx, pool, containers, postgresName)
	mainProcess.stop(t)
	mainProcess = startChild(t, "elitea-main", mainLog, filepath.Join(repositoryRoot, "services", "elitea-main"), mainEnvironment, mainBinary)
	waitForMain(t, ctx, publicBaseURL, mainProcess)

	agePendingDelivery(t, ctx, controlRedisPort, pki.caPath, "worker-bad-signature")

	wrongIdentityPKI := pki
	wrongIdentityPKI.workerCertPath = pki.wrongIdentityWorkerCertPath
	wrongIdentityPKI.workerKeyPath = pki.wrongIdentityWorkerKeyPath
	badIdentityConfigPath := writeWorkerConfig(t, root, "bad-identity", workerRedisPort, controlPort, workerOutputPort, contentPort, publicPort, workerPasswordPath, wrongIdentityPKI, signing.goodKeyringPath, spoolRoot, spoolKeyPath)
	badIdentityWorker := startWorker(t, python, repositoryRoot, badIdentityConfigPath, filepath.Join(root, "worker-bad-identity.log"))
	t.Cleanup(func() { badIdentityWorker.stop(t) })
	waitForPendingDelivery(t, ctx, observer, pool, admission.ExecutionID, "worker-bad-identity", badIdentityWorker)
	assertNoClaim(t, ctx, pool, admission.ExecutionID)
	badIdentityWorker.stop(t)
	agePendingDelivery(t, ctx, controlRedisPort, pki.caPath, "worker-bad-identity")

	untrustedPKI := pki
	untrustedPKI.workerCertPath = pki.untrustedWorkerCertPath
	untrustedPKI.workerKeyPath = pki.untrustedWorkerKeyPath
	badTLSConfigPath := writeWorkerConfig(t, root, "bad-tls", workerRedisPort, controlPort, workerOutputPort, contentPort, publicPort, workerPasswordPath, untrustedPKI, signing.goodKeyringPath, spoolRoot, spoolKeyPath)
	badTLSWorker := startWorker(t, python, repositoryRoot, badTLSConfigPath, filepath.Join(root, "worker-bad-tls.log"))
	t.Cleanup(func() { badTLSWorker.stop(t) })
	waitForPendingDelivery(t, ctx, observer, pool, admission.ExecutionID, "worker-bad-tls", badTLSWorker)
	assertNoClaim(t, ctx, pool, admission.ExecutionID)
	badTLSWorker.stop(t)
	agePendingDelivery(t, ctx, controlRedisPort, pki.caPath, "worker-bad-tls")

	outputACKDropped := outputFaultProxy.armCommittedACKDrop(t)
	redisFaultProxy.Arm()
	goodConfigPath := writeWorkerConfig(t, root, "good", workerRedisPort, controlPort, workerOutputPort, contentPort, publicPort, workerPasswordPath, pki, signing.goodKeyringPath, spoolRoot, spoolKeyPath)
	goodWorker := startWorker(t, python, repositoryRoot, goodConfigPath, filepath.Join(root, "worker-good.log"))
	t.Cleanup(func() { goodWorker.stop(t) })

	waitForFault(t, ctx, "committed output ACK loss", outputACKDropped, goodWorker)
	goodWorker.stop(t)
	mainProcess.stop(t)
	containers.restart(t, postgresName)
	waitForPostgresPool(t, ctx, pool, containers, postgresName)
	mainProcess = startChild(t, "elitea-main", mainLog, filepath.Join(repositoryRoot, "services", "elitea-main"), mainEnvironment, mainBinary)
	waitForMain(t, ctx, publicBaseURL, mainProcess)
	agePendingDelivery(t, ctx, controlRedisPort, pki.caPath, "worker-good")
	outputFaultProxy.releaseCommittedACKDrop(t)
	goodWorker = startWorker(t, python, repositoryRoot, goodConfigPath, filepath.Join(root, "worker-good-restarted.log"))

	waitForSettlementAndRetirement(t, ctx, pool, observer, admission.ExecutionID, goodWorker)
	waitForFault(t, ctx, "committed Redis retirement response loss", redisFaultProxy.Dropped(), goodWorker)
	assertDurableTerminalState(t, ctx, pool, admission.ExecutionID)
	assertSpoolEmpty(t, spoolRoot)
}

func waitForFault(t *testing.T, ctx context.Context, description string, observed <-chan struct{}, process *childProcess) {
	t.Helper()
	for {
		select {
		case <-observed:
			return
		case <-ctx.Done():
			t.Fatalf("did not observe %s: %v\n%s", description, ctx.Err(), process.logs())
		case <-time.After(100 * time.Millisecond):
			process.ensureRunning(t)
		}
	}
}

func assertBrokerLeastPrivilege(t *testing.T, ctx context.Context, port int, caPath string, observer *redis.Client) {
	t.Helper()
	worker := newControlRedisClient(t, port, "worker", workerPassword, caPath)
	defer func() { _ = worker.Close() }()
	producer := newControlRedisClient(t, port, "producer", producerPassword, caPath)
	defer func() { _ = producer.Close() }()

	assertNOPERM := func(operation string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(strings.ToUpper(err.Error()), "NOPERM") {
			t.Fatalf("Redis ACL allowed forbidden %s operation: %v", operation, err)
		}
	}
	assertNOPERM("worker XADD", worker.XAdd(ctx, &redis.XAddArgs{
		Stream: commandStream,
		Values: map[string]any{"signed_envelope": "forbidden"},
	}).Err())
	assertNOPERM("worker PUBLISH", worker.Publish(ctx, commandStream, "forbidden").Err())
	assertNOPERM("worker HSET", worker.HSet(ctx, commandStream+":delivery-index.v1", "forbidden", "0-0").Err())
	assertNOPERM("producer XREADGROUP", producer.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    consumerGroup,
		Consumer: "forbidden-producer",
		Streams:  []string{commandStream, ">"},
		Count:    1,
	}).Err())
	assertNOPERM("producer XACK", producer.XAck(ctx, commandStream, consumerGroup, "0-0").Err())
	assertNOPERM("producer XDEL", producer.XDel(ctx, commandStream, "0-0").Err())
	assertNOPERM("observer XCLAIM", observer.Do(
		ctx,
		"XCLAIM",
		commandStream,
		consumerGroup,
		"forbidden-observer",
		0,
		"0-0",
		"JUSTID",
	).Err())

	length, err := observer.XLen(ctx, commandStream).Result()
	if err != nil || length != 0 {
		t.Fatalf("forbidden broker operations changed the command stream: length=%d err=%v", length, err)
	}
	mappings, err := observer.HLen(ctx, commandStream+":delivery-index.v1").Result()
	if err != nil || mappings != 0 {
		t.Fatalf("settled broker delivery retained mappings: mappings=%d err=%v", mappings, err)
	}
}

type admissionResponse struct {
	ExecutionID string `json:"execution_id"`
	CommandID   string `json:"command_id"`
	Created     bool   `json:"created"`
}

func submitValidationPrivate(t *testing.T, ctx context.Context, pool *pgxpool.Pool, settings []byte) admissionResponse {
	t.Helper()
	catalog, err := runtimedomain.ParseDigest(catalogDigest)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := runtimedomain.ParseDigest(schemaDigest)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := repos.NewExecutionJobsRepository(pool, repos.ValidationDispatchPolicy{
		StreamName:        commandStream,
		CapabilityVersion: "1",
		ResourceClass:     "validation-small",
		IsolationClass:    "shared-claim-scoped-authority",
		Priority:          1,
		DeadlineTTL:       time.Minute,
		LimitsRevision:    "elitea.runtime.limits.conformance.v2",
		MaxOutstanding:    16,
	})
	if err != nil {
		t.Fatal(err)
	}
	bundles := executionapp.NewConformanceValidationInputBundleFactory(nil)
	bundle, err := bundles.BuildValidationInput(ctx, revisionID, "settings-system-1", "1", settings)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := executionapp.NewSubmitJobService(repository, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := jobs.SubmitValidation(ctx, executionapp.SubmitValidationRequest{
		Identity: executionapp.AdmissionIdentity{
			TenantID:            "tenant-1",
			ResourceProjectID:   "1",
			ProjectionProjectID: "1",
			ActorID:             "1",
		},
		IdempotencyKey: "system-validation-private-1",
		InputBundle:    bundle,
		Command: configurationdomain.ValidationCommand{
			ConfigurationRevisionID: revisionID,
			ConfigurationType:       "openapi",
			CatalogRevision:         catalogRevision,
			CatalogDigest:           catalog,
			SchemaID:                schemaID,
			SchemaRevision:          schemaRevision,
			SchemaDigest:            schema,
			SettingsEntryID:         "settings-system-1",
		},
	})
	if err != nil {
		t.Fatalf("submit private validation: %v", err)
	}
	if outcome.ExecutionID == "" || outcome.CommandID == "" || !outcome.Created {
		t.Fatalf("private admission returned invalid outcome: %+v", outcome)
	}
	if bundle.MediaType != executiondomain.InputBundleManifestMediaType {
		t.Fatalf("private admission built wrong bundle media type: %q", bundle.MediaType)
	}
	return admissionResponse{
		ExecutionID: outcome.ExecutionID,
		CommandID:   outcome.CommandID,
		Created:     outcome.Created,
	}
}

func assertReferenceOnlyRedisEntry(t *testing.T, ctx context.Context, client *redis.Client, settingsMarker string) {
	t.Helper()
	entries, err := client.XRangeN(ctx, commandStream, "-", "+", 2).Result()
	if err != nil {
		t.Fatalf("read bounded control stream: %v", err)
	}
	if len(entries) != 1 || len(entries[0].Values) != 1 {
		t.Fatalf("control stream must contain one one-field reference entry, got %#v", entries)
	}
	raw, ok := entries[0].Values["signed_envelope"].(string)
	if !ok || len(raw) == 0 || len(raw) > 48*1024 {
		t.Fatalf("signed_envelope field has unexpected type/size: %T/%d", entries[0].Values["signed_envelope"], len(raw))
	}
	if strings.Contains(raw, settingsMarker) {
		t.Fatal("Redis control entry contains settings data")
	}
	envelope := &runtimev1.SignedWorkerCommandEnvelopeV1{}
	if err := proto.Unmarshal([]byte(raw), envelope); err != nil {
		t.Fatalf("decode production signed envelope: %v", err)
	}
	if envelope.GetSignatureProfile() != runtimev1.SignatureProfileV1_SIGNATURE_PROFILE_V1_ED25519 || envelope.GetKeyId() != signingKeyID {
		t.Fatalf("unexpected production signature profile/key: %s/%q", envelope.GetSignatureProfile(), envelope.GetKeyId())
	}
	command := &runtimev1.WorkerCommandV1{}
	if err := proto.Unmarshal(envelope.GetWorkerCommandBytes(), command); err != nil {
		t.Fatalf("decode reference command: %v", err)
	}
	if command.GetInputBundleRef() == nil || command.GetInputBundleRef().GetByteLength() == 0 || command.GetConfigurationValidation() == nil {
		t.Fatalf("command lost immutable input reference: %v", command)
	}
}

func assertNoClaim(t *testing.T, ctx context.Context, pool *pgxpool.Pool, executionID string) {
	t.Helper()
	var claimCount int
	var state string
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.execution_claims WHERE execution_id = $1`, executionID).Scan(&claimCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT state FROM elitea_runtime.execution_jobs WHERE execution_id = $1`, executionID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if claimCount != 0 || state != "DISPATCHED" {
		t.Fatalf("wrong-key worker crossed the signature boundary: claims=%d state=%s", claimCount, state)
	}
}

func assertDurableTerminalState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, executionID string) {
	t.Helper()
	var state string
	var claims, inbox, results, settlements, replayEvents, replayCursors int
	var minimumReplayCursor, maximumReplayCursor int64
	if err := pool.QueryRow(ctx, `
SELECT j.state,
       (SELECT count(*) FROM elitea_runtime.execution_claims AS c WHERE c.execution_id = j.execution_id),
       (SELECT count(*) FROM elitea_runtime.output_inbox AS i WHERE i.execution_id = j.execution_id),
       (SELECT count(*) FROM elitea_runtime.configuration_validation_results AS r WHERE r.execution_id = j.execution_id),
       (SELECT count(*) FROM elitea_runtime.execution_settlements AS s WHERE s.execution_id = j.execution_id),
       (SELECT count(*) FROM elitea_runtime.execution_replay_events AS e WHERE e.execution_id = j.execution_id),
       (SELECT count(DISTINCT cursor) FROM elitea_runtime.execution_replay_events AS e WHERE e.execution_id = j.execution_id),
       (SELECT min(cursor) FROM elitea_runtime.execution_replay_events AS e WHERE e.execution_id = j.execution_id),
       (SELECT max(cursor) FROM elitea_runtime.execution_replay_events AS e WHERE e.execution_id = j.execution_id)
FROM elitea_runtime.execution_jobs AS j
WHERE j.execution_id = $1`, executionID).Scan(
		&state,
		&claims,
		&inbox,
		&results,
		&settlements,
		&replayEvents,
		&replayCursors,
		&minimumReplayCursor,
		&maximumReplayCursor,
	); err != nil {
		t.Fatal(err)
	}
	if state != "SUCCEEDED" || claims != 1 || inbox != 1 || results != 1 || settlements != 1 || replayEvents != 1 || replayCursors != replayEvents || minimumReplayCursor <= 0 || maximumReplayCursor < minimumReplayCursor {
		t.Fatalf(
			"terminal durability mismatch state=%s claims=%d inbox=%d results=%d settlements=%d replay=%d distinct-cursors=%d cursor-range=%d..%d",
			state,
			claims,
			inbox,
			results,
			settlements,
			replayEvents,
			replayCursors,
			minimumReplayCursor,
			maximumReplayCursor,
		)
	}
}

func assertSpoolEmpty(t *testing.T, spoolRoot string) {
	t.Helper()
	err := filepath.WalkDir(spoolRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path != spoolRoot && !entry.IsDir() {
			return fmt.Errorf("retained output spool file %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
