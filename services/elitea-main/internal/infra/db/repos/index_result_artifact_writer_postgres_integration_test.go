package repos

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	s3sdk "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage/s3"
)

// indexArtifactWriterTestBucket is this test file's own dedicated physical
// S3 bucket — separate from tests/contract's artifactContractBucket and
// internal/infra/storage/conformance's own buckets, so a run of this suite
// never collides with either against the same long-lived local RustFS
// container.
const indexArtifactWriterTestBucket = "elitea-repos-index-artifact-test"

// newIndexArtifactWriterTestStore builds a real S3-compatible ObjectStore
// against RustFS, the same raw S3-config-by-env-var construction
// tests/contract/artifact_harness_test.go's newArtifactContractStore
// already establishes — deliberately not storage.ConfigFromEnv (S5), which
// selects one backend for production use.
func newIndexArtifactWriterTestStore(t *testing.T) storage.ObjectStore {
	t.Helper()
	endpoint := os.Getenv("S3_ENDPOINT_URL")
	if endpoint == "" {
		t.Skip("set S3_ENDPOINT_URL (and S3_ACCESS_KEY/S3_SECRET_KEY/S3_REGION/S3_FORCE_PATH_STYLE) to run the index artifact writer's real-backend integration test")
	}
	accessKey := os.Getenv("S3_ACCESS_KEY")
	secretKey := os.Getenv("S3_SECRET_KEY")
	region := os.Getenv("S3_REGION")
	if region == "" {
		region = "us-east-1"
	}
	forcePathStyle := os.Getenv("S3_FORCE_PATH_STYLE") == "true"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		t.Fatalf("load aws config: %v", err)
	}
	raw := s3sdk.NewFromConfig(awsCfg, func(o *s3sdk.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = forcePathStyle
	})
	if _, err := raw.CreateBucket(ctx, &s3sdk.CreateBucketInput{Bucket: aws.String(indexArtifactWriterTestBucket)}); err != nil {
		var alreadyOwned *s3types.BucketAlreadyOwnedByYou
		var alreadyExists *s3types.BucketAlreadyExists
		if !errors.As(err, &alreadyOwned) && !errors.As(err, &alreadyExists) {
			t.Fatalf("create index artifact writer test bucket: %v", err)
		}
	}

	store, err := s3.New(ctx, s3.Config{
		Endpoint: endpoint, AccessKey: accessKey, SecretKey: secretKey,
		Region: region, ForcePathStyle: forcePathStyle, Bucket: indexArtifactWriterTestBucket,
	})
	if err != nil {
		t.Fatalf("build index artifact writer test store: %v", err)
	}
	return store
}

func putIndexArtifactWriterTestBytes(t *testing.T, url, contentType string, body []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build presigned index artifact PUT: %v", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.ContentLength = int64(len(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("presigned index artifact PUT: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("presigned index artifact PUT status = %d, want 2xx", resp.StatusCode)
	}
}

func getIndexArtifactWriterTestBytes(t *testing.T, url string) []byte {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("presigned index artifact GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("presigned index artifact GET status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read presigned index artifact GET body: %v", err)
	}
	return body
}

// TestIndexIngestArtifactGrantCommitResolveAndIngestRoundTrip proves S20c's
// IndexResultArtifactWriter end to end against real PostgreSQL and a real
// S3-compatible backend: CreateArtifactGrant issues a real presigned
// upload, CommitArtifact verifies and durably records it, and — the
// integration this stage exists for — the pre-existing, already-shipped
// ArtifactVerifier.VerifyDurable/IndexIngestService.IngestIndex path
// (index_ingest_results.go) accepts that durable row with NO test-only raw
// SQL seed (seedPostgresIndexArtifactAttestation, used by every other
// index-output integration test in this package, is deliberately not
// called here). ResolveArtifact then proves the committed bytes are
// actually readable back through a fresh presigned GET.
func TestIndexIngestArtifactGrantCommitResolveAndIngestRoundTrip(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	store := newIndexArtifactWriterTestStore(t)

	dispatchPolicy := IndexIngestDispatchPolicy{
		StreamName:        "elitea:runtime:index:commands",
		CapabilityVersion: "1",
		ResourceClass:     "indexing",
		IsolationClass:    "project",
		Priority:          1,
		DeadlineTTL:       time.Hour,
		LimitsRevision:    "index-limits-v1",
		MaxOutstanding:    1,
	}
	jobs, err := NewIndexIngestJobsRepository(pool, dispatchPolicy)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := newPostgresIndexAdmissionService(t, jobs, "artifact-writer").Submit(
		context.Background(),
		postgresIndexSubmitRequest("request-artifact-writer", "artifact-writer"),
	)
	if err != nil || !admitted.Created {
		t.Fatalf("admit index execution: outcome=%+v err=%v", admitted, err)
	}

	outputPolicy := IndexIngestOutputPolicy{
		LimitsRevision:    dispatchPolicy.LimitsRevision,
		ArtifactMediaType: "application/json",
		MaxArtifactBytes:  1024 * 1024,
	}
	results, err := NewIndexIngestResultsRepository(pool, outputPolicy)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := results.ExpectedIndexIngest(context.Background(), admitted.ExecutionID, 1)
	if err != nil {
		t.Fatalf("load admitted index binding: %v", err)
	}
	fence := claimPostgresIndexExecution(t, pool, expected)
	frame, _ := postgresIndexOutputFrame(t, expected, fence)
	artifactRef := frame.Result.ResultArtifact
	artifactBytes := []byte("durable index artifact bytes remain outside PostgreSQL")
	if runtimedomain.SHA256(artifactBytes) != artifactRef.Digest {
		t.Fatalf("test fixture assumption broken: postgresIndexOutputFrame's artifact bytes changed")
	}

	// Same policy ExpectedIndexIngest's own ArtifactContract is built from
	// (index_ingest_results.go), so artifactRef — sourced from that same
	// contract via postgresIndexOutputFrame — passes the writer's own
	// policy check below without drifting from what the read side expects.
	writer, err := NewIndexResultArtifactWriter(pool, store, outputPolicy)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	createRequest := outputapp.CreateArtifactGrantRequest{
		TenantID:            expected.TenantID,
		ResourceProjectID:   expected.ResourceProjectID,
		ProjectionProjectID: expected.ProjectionProjectID,
		CommandID:           expected.CommandID,
		ExecutionID:         expected.ExecutionID,
		Generation:          expected.Generation,
		Artifact:            artifactRef,
	}
	grant, err := writer.CreateArtifactGrant(ctx, createRequest)
	if err != nil || grant.GrantID == "" || grant.URL == "" || !grant.ExpiresAt.After(time.Now()) {
		t.Fatalf("CreateArtifactGrant = %+v, err=%v", grant, err)
	}
	putIndexArtifactWriterTestBytes(t, grant.URL, artifactRef.MediaType, artifactBytes)

	durable, err := writer.CommitArtifact(ctx, outputapp.CommitArtifactRequest{GrantID: grant.GrantID, CreateArtifactGrantRequest: createRequest})
	if err != nil || durable.Reference != artifactRef || durable.StorageRecordID != grant.GrantID || durable.VerifiedAt.IsZero() {
		t.Fatalf("CommitArtifact = %+v, err=%v, want Reference=%+v StorageRecordID=%s", durable, err, artifactRef, grant.GrantID)
	}

	// The integration this stage exists to prove: the pre-existing
	// ArtifactVerifier/IndexIngestService path accepts what this writer
	// just committed, with no test-only raw SQL seed in between.
	service := newPostgresIndexOutputService(t, pool, results)
	inserted, err := service.IngestIndex(ctx, frame)
	if err != nil || !inserted.Inserted || inserted.Cursor == 0 {
		t.Fatalf("project durable index result from a genuinely committed artifact: outcome=%+v err=%v", inserted, err)
	}
	replayed, err := service.IngestIndex(ctx, frame)
	if err != nil || replayed.Inserted || replayed.Cursor != inserted.Cursor {
		t.Fatalf("exact index output replay changed durable position: first=%+v replay=%+v err=%v", inserted, replayed, err)
	}

	// Recommitting the same, already-consumed grant must fail closed.
	if _, err := writer.CommitArtifact(ctx, outputapp.CommitArtifactRequest{GrantID: grant.GrantID, CreateArtifactGrantRequest: createRequest}); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("CommitArtifact on an already-consumed grant = %v, want storage.ErrAlreadyExists", err)
	}

	resolved, err := writer.ResolveArtifact(ctx, outputapp.ArtifactVerificationRequest{
		TenantID:            expected.TenantID,
		ResourceProjectID:   expected.ResourceProjectID,
		ProjectionProjectID: expected.ProjectionProjectID,
		CommandID:           expected.CommandID,
		ExecutionID:         expected.ExecutionID,
		Generation:          expected.Generation,
		Artifact:            artifactRef,
	})
	if err != nil || resolved.URL == "" || !resolved.ExpiresAt.After(time.Now()) {
		t.Fatalf("ResolveArtifact = %+v, err=%v", resolved, err)
	}
	if got := getIndexArtifactWriterTestBytes(t, resolved.URL); string(got) != string(artifactBytes) {
		t.Fatalf("ResolveArtifact's grant served %q, want %q", got, artifactBytes)
	}

	t.Run("exact retry with a fresh grant is idempotent", func(t *testing.T) {
		retryGrant, err := writer.CreateArtifactGrant(ctx, createRequest)
		if err != nil {
			t.Fatal(err)
		}
		putIndexArtifactWriterTestBytes(t, retryGrant.URL, artifactRef.MediaType, artifactBytes)
		retryDurable, err := writer.CommitArtifact(ctx, outputapp.CommitArtifactRequest{GrantID: retryGrant.GrantID, CreateArtifactGrantRequest: createRequest})
		if err != nil || retryDurable.Reference != artifactRef || retryDurable.StorageRecordID != durable.StorageRecordID {
			t.Fatalf("idempotent retry via a fresh grant = %+v, err=%v, want the original commit's own StorageRecordID=%s unchanged", retryDurable, err, durable.StorageRecordID)
		}
	})

	t.Run("a fresh grant claiming the same identity with different content conflicts", func(t *testing.T) {
		conflictingBytes := []byte("a different artifact body entirely")
		conflicting := createRequest
		conflicting.Artifact.Digest = runtimedomain.SHA256(conflictingBytes)
		conflicting.Artifact.ByteLength = uint64(len(conflictingBytes))
		conflictGrant, err := writer.CreateArtifactGrant(ctx, conflicting)
		if err != nil {
			t.Fatal(err)
		}
		putIndexArtifactWriterTestBytes(t, conflictGrant.URL, conflicting.Artifact.MediaType, conflictingBytes)
		_, err = writer.CommitArtifact(ctx, outputapp.CommitArtifactRequest{GrantID: conflictGrant.GrantID, CreateArtifactGrantRequest: conflicting})
		if !errors.Is(err, outputapp.ErrArtifactGrantConflict) {
			t.Fatalf("CommitArtifact for a colliding identity with different content = %v, want ErrArtifactGrantConflict", err)
		}
	})

	// Swarm-review findings (docs/plans/storage-migration-plan.md S20c's
	// own "Swarm review" writeup), each closed with a real fix, not just a
	// note — regression-tested here.

	t.Run("CreateArtifactGrant rejects an artifact outside the admitted policy", func(t *testing.T) {
		tooLarge := createRequest
		tooLarge.Artifact.ArtifactID = "artifact-policy-oversized"
		tooLarge.Artifact.ByteLength = outputPolicy.MaxArtifactBytes + 1
		if _, err := writer.CreateArtifactGrant(ctx, tooLarge); !errors.Is(err, outputapp.ErrIndexIngestArtifactMismatch) {
			t.Fatalf("CreateArtifactGrant for an oversized artifact = %v, want ErrIndexIngestArtifactMismatch", err)
		}

		wrongType := createRequest
		wrongType.Artifact.ArtifactID = "artifact-policy-wrong-type"
		wrongType.Artifact.MediaType = "application/octet-stream"
		if _, err := writer.CreateArtifactGrant(ctx, wrongType); !errors.Is(err, outputapp.ErrIndexIngestArtifactMismatch) {
			t.Fatalf("CreateArtifactGrant for a media type outside the policy = %v, want ErrIndexIngestArtifactMismatch", err)
		}
	})

	t.Run("CommitArtifact rejects a GET grant laundered from S15's own grant table", func(t *testing.T) {
		bucketRow, err := writer.requireBucket(ctx, mustParseProjectID(t, expected.ResourceProjectID))
		if err != nil {
			t.Fatal(err)
		}
		getGrantID, err := generateArtifactGrantID()
		if err != nil {
			t.Fatal(err)
		}
		getRequest := createRequest
		getRequest.Artifact.ArtifactID = "artifact-get-method-grant"
		digestAlg := "sha256"
		digest := getRequest.Artifact.Digest
		if _, err := writer.grants.CreateTransferGrant(ctx, NewTransferGrantInput{
			ID: getGrantID, ProjectID: mustParseProjectID(t, expected.ResourceProjectID), BucketID: bucketRow.ID,
			Key: getGrantID, Method: "GET", ContentType: getRequest.Artifact.MediaType,
			MaxBytes: int64(getRequest.Artifact.ByteLength), DigestAlg: &digestAlg, Digest: digest[:],
			ExpiresAt: time.Now().Add(indexArtifactGrantTTL),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.CommitArtifact(ctx, outputapp.CommitArtifactRequest{GrantID: getGrantID, CreateArtifactGrantRequest: getRequest}); !errors.Is(err, outputapp.ErrInvalidIndexIngestOutput) {
			t.Fatalf("CommitArtifact against a GET-method grant = %v, want ErrInvalidIndexIngestOutput", err)
		}
	})

	t.Run("CommitArtifact rejects a grant minted for a different bucket", func(t *testing.T) {
		projectID := mustParseProjectID(t, expected.ResourceProjectID)
		foreignBucket, err := writer.buckets.CreateBucket(ctx, NewBucketInput{
			ProjectID: projectID, Name: "foreign-bucket-not-index-artifacts", DisplayName: "foreign", BucketType: "system",
		})
		if err != nil {
			t.Fatal(err)
		}
		foreignGrantID, err := generateArtifactGrantID()
		if err != nil {
			t.Fatal(err)
		}
		foreignRequest := createRequest
		foreignRequest.Artifact.ArtifactID = "artifact-foreign-bucket-grant"
		digestAlg := "sha256"
		digest := foreignRequest.Artifact.Digest
		if _, err := writer.grants.CreateTransferGrant(ctx, NewTransferGrantInput{
			ID: foreignGrantID, ProjectID: projectID, BucketID: foreignBucket.ID,
			Key: foreignGrantID, Method: "PUT", ContentType: foreignRequest.Artifact.MediaType,
			MaxBytes: int64(foreignRequest.Artifact.ByteLength), DigestAlg: &digestAlg, Digest: digest[:],
			ExpiresAt: time.Now().Add(indexArtifactGrantTTL),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.CommitArtifact(ctx, outputapp.CommitArtifactRequest{GrantID: foreignGrantID, CreateArtifactGrantRequest: foreignRequest}); !errors.Is(err, outputapp.ErrInvalidIndexIngestOutput) {
			t.Fatalf("CommitArtifact against a grant minted for a different bucket = %v, want ErrInvalidIndexIngestOutput", err)
		}
	})

	t.Run("CommitArtifact checks size before hashing the full body", func(t *testing.T) {
		oversized := createRequest
		oversized.Artifact.ArtifactID = "artifact-oversized-upload"
		oversizedGrant, err := writer.CreateArtifactGrant(ctx, oversized)
		if err != nil {
			t.Fatal(err)
		}
		// Upload MORE bytes than the grant's own MaxBytes declares — a real
		// backend accepts the PUT (nothing server-side caps a presigned PUT
		// URL's size), so this can only be caught at commit time.
		overBytes := append(append([]byte{}, artifactBytes...), artifactBytes...)
		putIndexArtifactWriterTestBytes(t, oversizedGrant.URL, oversized.Artifact.MediaType, overBytes)
		if _, err := writer.CommitArtifact(ctx, outputapp.CommitArtifactRequest{GrantID: oversizedGrant.GrantID, CreateArtifactGrantRequest: oversized}); !errors.Is(err, outputapp.ErrIndexIngestArtifactMismatch) {
			t.Fatalf("CommitArtifact for an over-large upload = %v, want ErrIndexIngestArtifactMismatch", err)
		}
	})
}

func mustParseProjectID(t *testing.T, value string) int64 {
	t.Helper()
	id, err := parseProjectID(value)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
