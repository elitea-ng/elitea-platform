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
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	"github.com/EliteaAI/elitea-platform/libs/go/natsconn/natstest"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/commandbus"
)

const (
	systemTestOptIn = "ELITEA_RUNTIME_SYSTEM_TEST"
	workloadSession = "system-session-1"
	producerID      = "system-producer-1"
	workloadID      = "spiffe://elitea.test/runtime/python-worker"
	signingKeyID    = "system-ed25519-key-1"
	revisionID      = "configuration-revision-system-1"

	publicSecret = "system-public-session-secret-5681"

	catalogRevision = "a78d3654f99d8ff89ca7233f20a66d676e564f79"
	catalogDigest   = "4a96e3ab8e3842ebf2645a851aeb12e3e2343f28e7d024c1a2960eb4ec254351"
	schemaID        = "elitea.configuration.openapi"
	schemaRevision  = catalogRevision
	schemaDigest    = "1c43c41a5304c6f73c68deebd37ba70f8c2266a59dfd4f9d4fa20b819e7ab3f1"
)

// TestProductionRuntimeCrossProcessSystem drives the production publisher,
// control, content and output components through a private admission seam,
// over the NATS JetStream command bus: a nats-server started from the NATS
// chart's own permission table (libs/go/natsconn/natstest), its assets created
// by the real bootstrap.sh, elitea-main publishing as elitea-main-runtime and
// the Python worker pulling as elitea-worker. Public route/RBAC compatibility
// remains a separate deployment gate.
func TestProductionRuntimeCrossProcessSystem(t *testing.T) {
	if os.Getenv(systemTestOptIn) != "1" {
		t.Skip("set ELITEA_RUNTIME_SYSTEM_TEST=1 to run the Docker-backed cross-process runtime system test")
	}

	repositoryRoot := findRepositoryRoot(t)
	python := systemPython(t, repositoryRoot)
	requireCommand(t, "docker")
	requirePythonRuntime(t, python, repositoryRoot)
	natsServer := natstest.Start(t)
	natsServer.Bootstrap(t, nil)
	bus := newRuntimeBus(t, natsServer)

	root := canonicalTempDir(t)
	pki := generateRuntimePKI(t, root)
	signing := generateSigningMaterial(t, root)
	spoolRoot := filepath.Join(root, "spool")
	mustMkdir(t, spoolRoot, 0o700)
	spoolKeyPath := filepath.Join(root, "spool.key")
	writeFile(t, spoolKeyPath, bytes.Repeat([]byte{0x5a}, 32), 0o600)
	// A worker that dead-letters a delivery also records it in its spool's
	// local quarantine (execution/quarantine.py) and would park it again on
	// every later delivery. Each unauthorized worker is a separate pod in
	// production, so each gets its own spool; the authorized worker keeps
	// one spool across its restart, which the output-ACK recovery needs.
	unauthorizedSpool := func(name string) string {
		directory := filepath.Join(root, "spool-"+name)
		mustMkdir(t, directory, 0o700)
		return directory
	}

	postgresPort := freePort(t)
	publicPort := freePort(t)
	controlPort := freePort(t)
	outputPort := freePort(t)
	contentPort := freePort(t)
	authConfigPath := writeRuntimeAuthConfig(t, root, publicPort)

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

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
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

	settingsMarker := "COMMAND-BUS-MUST-NEVER-CONTAIN-SETTINGS-5681-" + strings.Repeat("x", 24*1024)
	settings := []byte(`{"scope":"` + settingsMarker + `"}`)
	seedRuntimeFixtures(t, ctx, pool, settings)

	expectedViolations := assertBusLeastPrivilege(t, ctx, bus)

	mainLog := filepath.Join(root, "elitea-main.log")
	mainEnvironment := runtimeMainEnvironment(
		databaseURL,
		natsServer,
		publicPort,
		controlPort,
		outputPort,
		contentPort,
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

	// The command is admitted and published BEFORE the first worker starts,
	// so its live copy can be read (reference-only body) while it is still
	// in the stream: the bad-signature worker below terminates it.
	admission := submitValidationPrivate(t, ctx, pool, settings)
	deliveryID := outboxDeliveryID(t, ctx, pool, admission.ExecutionID)
	subject, err := commandbus.DeliverySubject(commandStream, deliveryID)
	if err != nil {
		t.Fatal(err)
	}
	deadLetterKey, err := commandbus.DeadLetterKey(commandStream, deliveryID)
	if err != nil {
		t.Fatal(err)
	}
	settledCommand := bus.waitForLiveCommand(t, ctx, subject, deliveryID, settingsMarker)

	badSignatureConfigPath := writeWorkerConfig(t, root, "bad-signature", natsServer, controlPort, workerOutputPort, contentPort, publicPort, pki, signing.badKeyringPath, unauthorizedSpool("bad-signature"), spoolKeyPath)
	badSignatureWorker := startWorker(t, python, repositoryRoot, badSignatureConfigPath, filepath.Join(root, "worker-bad-signature.log"))
	t.Cleanup(func() { badSignatureWorker.stop(t) })
	// A signature failure can never become valid: one dead-letter record,
	// then Term (#1081 review S2) — the stream's capacity is freed at once,
	// and there is never a claim.
	bus.waitForDeadLetter(t, ctx, deadLetterKey, badSignatureWorker)
	bus.waitForTerminated(t, ctx, badSignatureWorker)
	assertNoClaim(t, ctx, pool, admission.ExecutionID)
	badSignatureWorker.stop(t)
	// PostgreSQL re-offers the still-dispatched outbox row (the visibility
	// repair, once the 2m duplicate window has passed); the test publishes
	// the same bytes now instead of waiting for it.
	bus.replaySettledCommand(t, ctx, settledCommand)

	// Preserve a real pending delivery through every durable infrastructure
	// process restart before any authorized worker can claim it: the NATS
	// server (same store; sync_interval always), PostgreSQL and elitea-main.
	// The server's in-memory log starts over on restart, so the violations
	// so far are checked first.
	assertOnlyExpectedViolations(t, natsServer, expectedViolations)
	natsServer.Restart(t)
	containers.restart(t, postgresName)
	waitForPostgresPool(t, ctx, pool, containers, postgresName)
	mainProcess.stop(t)
	mainProcess = startChild(t, "elitea-main", mainLog, filepath.Join(repositoryRoot, "services", "elitea-main"), mainEnvironment, mainBinary)
	waitForMain(t, ctx, publicBaseURL, mainProcess)
	bus.waitForLiveCommand(t, ctx, subject, deliveryID, settingsMarker)
	var delivered uint64

	wrongIdentityPKI := pki
	wrongIdentityPKI.workerCertPath = pki.wrongIdentityWorkerCertPath
	wrongIdentityPKI.workerKeyPath = pki.wrongIdentityWorkerKeyPath
	badIdentityConfigPath := writeWorkerConfig(t, root, "bad-identity", natsServer, controlPort, workerOutputPort, contentPort, publicPort, wrongIdentityPKI, signing.goodKeyringPath, unauthorizedSpool("bad-identity"), spoolKeyPath)
	badIdentityWorker := startWorker(t, python, repositoryRoot, badIdentityConfigPath, filepath.Join(root, "worker-bad-identity.log"))
	t.Cleanup(func() { badIdentityWorker.stop(t) })
	delivered = bus.waitForUnsettledDelivery(t, ctx, pool, admission.ExecutionID, delivered, "worker-bad-identity", badIdentityWorker)
	assertNoClaim(t, ctx, pool, admission.ExecutionID)
	badIdentityWorker.stop(t)
	bus.redeliverNow(t, ctx)

	untrustedPKI := pki
	untrustedPKI.workerCertPath = pki.untrustedWorkerCertPath
	untrustedPKI.workerKeyPath = pki.untrustedWorkerKeyPath
	badTLSConfigPath := writeWorkerConfig(t, root, "bad-tls", natsServer, controlPort, workerOutputPort, contentPort, publicPort, untrustedPKI, signing.goodKeyringPath, unauthorizedSpool("bad-tls"), spoolKeyPath)
	badTLSWorker := startWorker(t, python, repositoryRoot, badTLSConfigPath, filepath.Join(root, "worker-bad-tls.log"))
	t.Cleanup(func() { badTLSWorker.stop(t) })
	bus.waitForUnsettledDelivery(t, ctx, pool, admission.ExecutionID, delivered, "worker-bad-tls", badTLSWorker)
	assertNoClaim(t, ctx, pool, admission.ExecutionID)
	badTLSWorker.stop(t)
	bus.redeliverNow(t, ctx)

	outputACKDropped := outputFaultProxy.armCommittedACKDrop(t)
	goodConfigPath := writeWorkerConfig(t, root, "good", natsServer, controlPort, workerOutputPort, contentPort, publicPort, pki, signing.goodKeyringPath, spoolRoot, spoolKeyPath)
	goodWorker := startWorker(t, python, repositoryRoot, goodConfigPath, filepath.Join(root, "worker-good.log"))
	t.Cleanup(func() { goodWorker.stop(t) })

	waitForFault(t, ctx, "committed output ACK loss", outputACKDropped, goodWorker)
	goodWorker.stop(t)
	mainProcess.stop(t)
	containers.restart(t, postgresName)
	waitForPostgresPool(t, ctx, pool, containers, postgresName)
	mainProcess = startChild(t, "elitea-main", mainLog, filepath.Join(repositoryRoot, "services", "elitea-main"), mainEnvironment, mainBinary)
	waitForMain(t, ctx, publicBaseURL, mainProcess)
	// The stopped worker left the delivery ack-pending (graceful shutdown
	// neither acks nor naks); redeliver it now instead of after AckWait.
	bus.redeliverNow(t, ctx)
	outputFaultProxy.releaseCommittedACKDrop(t)
	goodWorker = startWorker(t, python, repositoryRoot, goodConfigPath, filepath.Join(root, "worker-good-restarted.log"))

	bus.waitForSettlementAndRetirement(t, ctx, pool, admission.ExecutionID, subject, goodWorker)
	assertDurableTerminalState(t, ctx, pool, admission.ExecutionID)

	// The JetStream counterpart of the Redis retirement-response loss: the
	// settlement is durable but the ack never reached the server, so the
	// command is delivered again. The worker must reach the claim, which
	// answers settled, and double-ack it: no second claim, no output, no
	// dead-letter record.
	_, deadLettersBeforeReplay := bus.deadLetterState(t, ctx, deadLetterKey)
	_, consumerBeforeReplay := bus.requireState(t, ctx)
	bus.replaySettledCommand(t, ctx, settledCommand)
	bus.waitForReplayRetired(t, ctx, consumerBeforeReplay.Delivered.Consumer, goodWorker)
	if records, deadLettersAfterReplay := bus.deadLetterState(t, ctx, deadLetterKey); records != 1 || deadLettersAfterReplay != deadLettersBeforeReplay {
		t.Fatalf("a redelivered settled command was dead-lettered instead of acknowledged: records=%d bucket-sequence=%d->%d\n%s",
			records, deadLettersBeforeReplay, deadLettersAfterReplay, goodWorker.logs())
	}
	assertDurableTerminalState(t, ctx, pool, admission.ExecutionID)
	assertSpoolEmpty(t, spoolRoot)
	assertOnlyExpectedViolations(t, natsServer, expectedViolations)
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

// outboxDeliveryID is the delivery ID the producer published under: the
// outbox ID, which is also the signed command's idempotency key.
func outboxDeliveryID(t *testing.T, ctx context.Context, pool *pgxpool.Pool, executionID string) string {
	t.Helper()
	var deliveryID string
	var rows int
	if err := pool.QueryRow(ctx, `
SELECT min(outbox_id), count(*)
FROM elitea_runtime.command_outbox
WHERE execution_id = $1 AND stream_name = $2`, executionID, commandStream).Scan(&deliveryID, &rows); err != nil {
		t.Fatalf("read the admission's outbox row: %v", err)
	}
	if rows != 1 || deliveryID == "" {
		t.Fatalf("admission must have one outbox row on %s, got %d", commandStream, rows)
	}
	return deliveryID
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
		LimitsRevision:    "elitea.runtime.limits.conformance.v3",
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

// assertReferenceOnlyCommand reads the live command the way the producer's
// conflict check does (DIRECT.GET last_by_subj, as elitea-main-runtime) and
// asserts the bus carries a reference, never the settings: one bounded
// message on the delivery's own subject, headers naming the delivery only,
// a body that is exactly the production signed envelope. It returns the
// message for the settled-command replay later.
func assertReferenceOnlyCommand(t *testing.T, ctx context.Context, bus *runtimeBus, subject, deliveryID, settingsMarker string) *jetstream.RawStreamMsg {
	t.Helper()
	stream, _ := bus.requireState(t, ctx)
	if stream.State.Msgs != 1 || stream.State.NumSubjects != 1 {
		t.Fatalf("command stream must hold one live command on one subject, got msgs=%d subjects=%d", stream.State.Msgs, stream.State.NumSubjects)
	}
	live, err := bus.liveCommand(ctx, subject)
	if err != nil {
		t.Fatalf("read the live command on its delivery subject: %v", err)
	}
	raw := live.Data
	if live.Subject != subject || len(raw) == 0 || len(raw) > 48*1024 {
		t.Fatalf("live command has unexpected subject/size: %q/%d", live.Subject, len(raw))
	}
	if strings.Contains(string(raw), settingsMarker) {
		t.Fatal("command bus message body contains settings data")
	}
	headerBytes := 0
	for name, values := range live.Header {
		for _, value := range values {
			headerBytes += len(name) + len(value)
			if strings.Contains(value, settingsMarker) {
				t.Fatalf("command bus header %s contains settings data", name)
			}
		}
	}
	if len(raw)+headerBytes > commandbus.MaxMessageBytes {
		t.Fatalf("live command exceeds max_transport_message_bytes: %d", len(raw)+headerBytes)
	}
	if got := live.Header.Get(commandbus.HeaderMsgID); got != commandbus.DeliveryToken(deliveryID) {
		t.Fatalf("live command %s header is %q, want the subject token", commandbus.HeaderMsgID, got)
	}
	if got := live.Header.Get(commandbus.HeaderDeliveryID); got != deliveryID {
		t.Fatalf("live command %s header is %q, want %q", commandbus.HeaderDeliveryID, got, deliveryID)
	}
	envelope := &runtimev1.SignedWorkerCommandEnvelopeV1{}
	if err := proto.Unmarshal(raw, envelope); err != nil {
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
	if command.GetIdempotencyKey() != deliveryID {
		t.Fatalf("subject names delivery %q but the signed command's idempotency key is %q", deliveryID, command.GetIdempotencyKey())
	}
	return live
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
