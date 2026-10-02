package migrations_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"io"
	"net/url"
	"strconv"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"sync"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// This test applies the authoritative migration corpus to an isolated database.
// It exercises real PostgreSQL locks and retries, without worker or HTTP traffic.
func TestToolkitDiscoveryArtifactAtomicReplayAndFencing(t *testing.T) {
	pool := newMigratedPool(t)
	ctx := context.Background()
	project := seedToolRunProject(t, pool)
	bundle := seedToolRunInputBundle(t, pool, project)
	require.NoError(t, insertToolRunExecutionJob(ctx, pool, "discovery", "toolkit.available_tools.v1", project, bundle))
	settings := []byte(`{"selected_tools":[]}`)
	sourceDigest := sha256.Sum256(settings)
	_, err := pool.Exec(ctx, `INSERT INTO elitea_runtime.input_bundle_entries (
        input_bundle_id, entry_id, entry_version, semantic_role, media_type,
        content_digest, content_size, content_reference, classification,
        required_grant_audience, content_bytes
    ) VALUES ($1,'settings','v1','toolkit.available_tools.settings','application/json',
        $2,$3,'content','tenant-confidential','elitea.runtime.input.read.v1',$4)`, bundle, sourceDigest[:], len(settings), settings)
	require.NoError(t, err)
	workload := "spiffe://elitea.internal/runtime/worker-1"
	_, err = pool.Exec(ctx, `INSERT INTO elitea_runtime.workload_sessions (
        workload_session_id, workload_identity, producer_id, expires_at
    ) VALUES ('session',$1,'producer',clock_timestamp()+interval '10 minutes')`, workload)
	require.NoError(t, err)
	fence := bytes.Repeat([]byte{7}, 32)
	_, err = pool.Exec(ctx, `INSERT INTO elitea_runtime.execution_claims (
        claim_id, execution_id, generation, workload_session_id, workload_identity, producer_id,
        claim_attempt, lease_epoch, fence_token, claimed_at, lease_expires_at, initial_output_watermark
    ) VALUES ('claim','discovery',1,'session',$1,'producer',1,1,$2,clock_timestamp(),clock_timestamp()+interval '5 minutes',0)`, workload, fence)
	require.NoError(t, err)
	identity, err := url.Parse(workload)
	require.NoError(t, err)
	claim := storage.ContentClaim{
		PeerCertificate: &x509.Certificate{URIs: []*url.URL{identity}},
		ExecutionID:     "discovery", Generation: 1, ClaimID: "claim", FenceToken: fence,
		ContentID: "content", ImmutableVersion: "v1",
	}

	// The admitted type is recovered from the existing prepared command, with no
	// discovery job table or ad-hoc metadata owner.
	bundleHash := sha256.Sum256([]byte("manifest"))
	commandBytes, err := proto.Marshal(&runtimev1.WorkerCommandV1{
		CommandId: "discovery:command", ExecutionId: "discovery", Generation: 1, TenantId: "tenant-1",
		ResourceProjectId: strconv.FormatInt(project, 10), ProjectionProjectId: strconv.FormatInt(project, 10), CapabilityId: "toolkit.available_tools.v1",
		InputBundleRef:    &runtimev1.ExecutionInputBundleReferenceV1{InputBundleId: bundle, Digest: &runtimev1.DigestV1{Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256, Value: bundleHash[:]}},
		CapabilityCommand: &runtimev1.WorkerCommandV1_ToolkitAvailableTools{ToolkitAvailableTools: &runtimev1.ToolkitAvailableToolsCommandV1{ToolkitType: "mcp_records", SettingsEntryId: "settings"}},
	})
	require.NoError(t, err)
	commandHash := sha256.Sum256(commandBytes)
	envelope, err := proto.Marshal(&runtimev1.SignedWorkerCommandEnvelopeV1{WorkerCommandBytes: commandBytes, WorkerCommandDigest: &runtimev1.DigestV1{Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256, Value: commandHash[:]}})
	require.NoError(t, err)
	envelopeHash := sha256.Sum256(envelope)
	_, err = pool.Exec(ctx, `INSERT INTO elitea_runtime.command_outbox(outbox_id,execution_id,generation,stream_name,resource_class,isolation_class,priority,deadline,limits_revision,
 prepared_signed_envelope_bytes,prepared_signed_envelope_digest,prepared_signature_profile,prepared_key_id,prepared_at)
 VALUES ('outbox','discovery',1,'commands','indexing','project',1,clock_timestamp()+interval '1 hour','limits',$1,$2,1,'key',clock_timestamp())`, envelope, envelopeHash[:])
	require.NoError(t, err)
	contentRepo, err := storage.NewPostgresContentRepository(pool)
	require.NoError(t, err)
	authorization, err := contentRepo.AuthorizeContent(ctx, claim)
	require.NoError(t, err)
	require.Equal(t, "mcp_records", authorization.ToolkitType)
	objects := &discoveryObjectStore{content: make(map[string][]byte)}
	repo, err := repos.NewToolkitDiscoveryArtifactRepository(pool, objects)
	require.NoError(t, err)
	content := []byte(`{"args_schemas":{},"tools":[]}`)
	// Concurrent first uploads must still attest only one immutable version.
	var initial sync.WaitGroup
	initialErrors := make(chan error, 8)
	for range 8 {
		initial.Add(1)
		go func() {
			defer initial.Done()
			_, e := repo.PutToolkitDiscoveryArtifact(ctx, claim, content)
			initialErrors <- e
		}()
	}
	initial.Wait()
	close(initialErrors)
	for e := range initialErrors {
		require.NoError(t, e)
	}
	first, err := repo.PutToolkitDiscoveryArtifact(ctx, claim, content)
	require.NoError(t, err)
	again, err := repo.PutToolkitDiscoveryArtifact(ctx, claim, content)
	require.NoError(t, err)
	require.True(t, proto.Equal(first, again))
	read, err := repo.GetToolkitDiscoveryArtifact(ctx, claim)
	require.NoError(t, err)
	require.Equal(t, content, read)

	var group sync.WaitGroup
	results := make(chan error, 8)
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := repo.PutToolkitDiscoveryArtifact(ctx, claim, content)
			results <- err
		}()
	}
	group.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	_, err = repo.PutToolkitDiscoveryArtifact(ctx, claim, []byte(`{"tools":["different"]}`))
	require.ErrorIs(t, err, storage.ErrContentRejected)
	var count int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.index_result_artifacts`).Scan(&count))
	require.Equal(t, 1, count)

	for _, mutate := range []func(*storage.ContentClaim){
		func(c *storage.ContentClaim) { c.Generation++ },
		func(c *storage.ContentClaim) { c.ExecutionID = "foreign" },
		func(c *storage.ContentClaim) { c.FenceToken = bytes.Repeat([]byte{8}, 32) },
		func(c *storage.ContentClaim) { c.ImmutableVersion = "v2" },
		func(c *storage.ContentClaim) { c.ContentID = "foreign" },
		func(c *storage.ContentClaim) { c.ClaimID = "replacement" },
		func(c *storage.ContentClaim) {
			other, _ := url.Parse("spiffe://elitea.internal/runtime/foreign")
			c.PeerCertificate = &x509.Certificate{URIs: []*url.URL{other}}
		},
	} {
		rejected := claim
		mutate(&rejected)
		_, err := repo.PutToolkitDiscoveryArtifact(ctx, rejected, content)
		require.ErrorIs(t, err, storage.ErrContentUnauthorized)
	}
	_, err = pool.Exec(ctx, `UPDATE elitea_runtime.workload_sessions SET revoked_at=clock_timestamp() WHERE workload_session_id='session'`)
	require.NoError(t, err)
	_, err = repo.PutToolkitDiscoveryArtifact(ctx, claim, content)
	require.ErrorIs(t, err, storage.ErrContentUnauthorized)
	_, err = pool.Exec(ctx, `UPDATE elitea_runtime.workload_sessions SET revoked_at=NULL WHERE workload_session_id='session'`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE elitea_runtime.execution_claims SET released_at=clock_timestamp(),release_reason='COMPLETED' WHERE claim_id='claim'`)
	require.NoError(t, err)
	_, err = repo.PutToolkitDiscoveryArtifact(ctx, claim, content)
	require.ErrorIs(t, err, storage.ErrContentUnauthorized)
	_, err = pool.Exec(ctx, `UPDATE elitea_runtime.execution_claims SET released_at=NULL,release_reason=NULL WHERE claim_id='claim'`)
	require.NoError(t, err)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = repo.PutToolkitDiscoveryArtifact(canceled, claim, content)
	require.ErrorIs(t, err, context.Canceled)
	_, err = pool.Exec(ctx, `UPDATE elitea_runtime.execution_jobs SET desired_state='CANCELLED' WHERE execution_id='discovery'`)
	require.NoError(t, err)
	_, err = repo.PutToolkitDiscoveryArtifact(ctx, claim, content)
	require.ErrorIs(t, err, storage.ErrContentUnauthorized)
	// A canceled write cannot change the committed bytes.
	var version string
	require.NoError(t, pool.QueryRow(ctx, `SELECT immutable_version FROM elitea_runtime.index_result_artifacts`).Scan(&version))
	require.Equal(t, first.ImmutableVersion, version)
	// Accepted references survive inventory expiry; only losing attempts are swept.
	_, err = pool.Exec(ctx, `UPDATE elitea_storage.transfer_grants SET expires_at=clock_timestamp()-interval '1 minute'`)
	require.NoError(t, err)
	_, err = repo.SweepOrphanToolkitDiscoveryArtifacts(ctx, 32)
	require.NoError(t, err)
	require.Len(t, objects.content, 1)
	// Inventory is durable even when a writer loses authority after upload.
	_, err = pool.Exec(ctx, `UPDATE elitea_runtime.execution_jobs SET desired_state='RUNNING' WHERE execution_id='discovery'`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM elitea_runtime.index_result_artifacts`)
	require.NoError(t, err)
	objects.afterPut = func() {
		_, e := pool.Exec(ctx, `UPDATE elitea_runtime.execution_jobs SET desired_state='CANCELLED' WHERE execution_id='discovery'`)
		require.NoError(t, e)
	}
	_, err = repo.PutToolkitDiscoveryArtifact(ctx, claim, content)
	require.ErrorIs(t, err, storage.ErrContentUnauthorized)
	objects.afterPut = nil
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.index_result_artifacts`).Scan(&count))
	require.Zero(t, count)
	_, err = pool.Exec(ctx, `UPDATE elitea_storage.transfer_grants SET expires_at=clock_timestamp()-interval '1 minute'`)
	require.NoError(t, err)
	objects.failDelete = true
	_, err = repo.SweepOrphanToolkitDiscoveryArtifacts(ctx, 32)
	require.ErrorIs(t, err, storage.ErrContentUnavailable)
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM elitea_storage.transfer_grants`).Scan(&count))
	require.Positive(t, count, "failed deletion retains inventory for retry")
	objects.failDelete = false
	deleted, err := repo.SweepOrphanToolkitDiscoveryArtifacts(ctx, 32)
	require.NoError(t, err)
	require.Positive(t, deleted)
	require.Empty(t, objects.content)
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM elitea_storage.transfer_grants`).Scan(&count))
	require.Zero(t, count)
	// No migration or table for discovery artifact bytes is present.
	var absent bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT to_regclass('elitea_runtime.toolkit_discovery_artifacts') IS NULL`).Scan(&absent))
	require.True(t, absent)
}

// Real database tests use a deterministic object adapter, not a cloud provider.
type discoveryObjectStore struct {
	storage.ObjectStore
	mu         sync.Mutex
	content    map[string][]byte
	afterPut   func()
	failDelete bool
}

func (s *discoveryObjectStore) Put(ctx context.Context, ref storage.ObjectRef, body io.Reader, opts storage.PutOptions) (storage.ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return storage.ObjectInfo{}, err
	}
	b, err := io.ReadAll(body)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	s.mu.Lock()
	s.content[ref.StorageKey("")] = bytes.Clone(b)
	s.mu.Unlock()
	if s.afterPut != nil {
		s.afterPut()
	}
	return storage.ObjectInfo{Size: int64(len(b))}, nil
}
func (s *discoveryObjectStore) Get(ctx context.Context, ref storage.ObjectRef, _ *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, storage.ObjectInfo{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.content[ref.StorageKey("")]
	if !ok {
		return nil, storage.ObjectInfo{}, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(bytes.Clone(b))), storage.ObjectInfo{Size: int64(len(b))}, nil
}
func (s *discoveryObjectStore) Delete(ctx context.Context, ref storage.ObjectRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failDelete {
		return errors.New("object delete failed")
	}
	delete(s.content, ref.StorageKey(""))
	return nil
}
