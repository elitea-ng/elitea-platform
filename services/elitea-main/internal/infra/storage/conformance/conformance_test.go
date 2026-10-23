// Package conformance runs one shared table-driven suite against every
// ObjectStore backend the current environment provides a working emulator
// for. Each backend's subtest skips cleanly when its emulator env vars are
// absent, so `go test ./...` is green on a laptop with no containers and
// exhaustive in CI (see deploy/docker-compose.storage-test.yml).
//
// This package deliberately does NOT go through storage.ConfigFromEnv (S5):
// that function selects and validates exactly one backend for production use
// (STORAGE_BACKEND, required STORAGE_CONTAINER, etc.), but this suite needs
// to stand up all three backends independently, side by side, against three
// different emulators in the same run — a different problem with different
// config needs. It reads the same S3_*/AZURE_STORAGE_*/GCS_* variable names
// directly instead, and owns its own fixed physical bucket name
// (conformanceBucket) rather than depending on STORAGE_CONTAINER.
package conformance

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	s3sdk "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage/azure"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage/gcs"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage/s3"
)

// conformanceBucket is the single physical bucket/container every backend's
// leg of this suite provisions and operates against. It is unrelated to the
// case-scoped logical bucket names passed to ObjectRef — see runCases.
const conformanceBucket = "elitea-conformance-test"

// conformanceProjectID is a fixed, valid projectID (per S1's
// ^[1-9][0-9]{0,17}$ rule) used to namespace every ref this suite creates.
const conformanceProjectID = "999000001"

func TestArtifactConformance(t *testing.T) {
	ctx := context.Background()

	t.Run("s3", func(t *testing.T) {
		store := setupS3(t, ctx)
		runCases(t, store)
	})

	t.Run("azure", func(t *testing.T) {
		store := setupAzure(t, ctx)
		runCases(t, store)
	})

	t.Run("gcs", func(t *testing.T) {
		store := setupGCS(t, ctx)
		runCases(t, store)
	})
}

func setupS3(t *testing.T, ctx context.Context) storage.ObjectStore {
	t.Helper()
	endpoint := os.Getenv("S3_ENDPOINT_URL")
	if endpoint == "" {
		t.Skip("S3_ENDPOINT_URL not set; skipping S3 conformance (see deploy/docker-compose.storage-test.yml)")
	}
	accessKey := os.Getenv("S3_ACCESS_KEY")
	secretKey := os.Getenv("S3_SECRET_KEY")
	region := os.Getenv("S3_REGION")
	if region == "" {
		region = "us-east-1"
	}
	forcePathStyle := os.Getenv("S3_FORCE_PATH_STYLE") == "true"

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		t.Fatalf("s3 conformance setup: load aws config: %v", err)
	}
	raw := s3sdk.NewFromConfig(awsCfg, func(o *s3sdk.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = forcePathStyle
	})
	if _, err := raw.CreateBucket(ctx, &s3sdk.CreateBucketInput{Bucket: aws.String(conformanceBucket)}); err != nil {
		var alreadyOwned *s3types.BucketAlreadyOwnedByYou
		var alreadyExists *s3types.BucketAlreadyExists
		if !errors.As(err, &alreadyOwned) && !errors.As(err, &alreadyExists) {
			t.Fatalf("s3 conformance setup: create bucket: %v", err)
		}
	}

	backend, err := s3.New(ctx, s3.Config{
		Endpoint:       endpoint,
		AccessKey:      accessKey,
		SecretKey:      secretKey,
		Region:         region,
		ForcePathStyle: forcePathStyle,
		Bucket:         conformanceBucket,
	})
	if err != nil {
		t.Fatalf("s3.New: %v", err)
	}
	return backend
}

func setupAzure(t *testing.T, ctx context.Context) storage.ObjectStore {
	t.Helper()
	endpoint := os.Getenv("AZURE_STORAGE_ENDPOINT")
	account := os.Getenv("AZURE_STORAGE_ACCOUNT")
	key := os.Getenv("AZURE_STORAGE_KEY")
	// Azurite (this suite's Azure emulator) is shared-key only — running
	// without a key would fail every operation, not only the Presign case,
	// so the whole leg skips unless all three are set.
	if endpoint == "" || account == "" || key == "" {
		t.Skip("AZURE_STORAGE_ENDPOINT / AZURE_STORAGE_ACCOUNT / AZURE_STORAGE_KEY not set; skipping Azure conformance")
	}

	cred, err := azblob.NewSharedKeyCredential(account, key)
	if err != nil {
		t.Fatalf("azure conformance setup: shared key credential: %v", err)
	}
	rawClient, err := azblob.NewClientWithSharedKeyCredential(endpoint, cred, nil)
	if err != nil {
		t.Fatalf("azure conformance setup: raw client: %v", err)
	}
	if _, err := rawClient.CreateContainer(ctx, conformanceBucket, nil); err != nil {
		if !bloberror.HasCode(err, bloberror.ContainerAlreadyExists) {
			t.Fatalf("azure conformance setup: create container: %v", err)
		}
	}

	backend, err := azure.New(ctx, azure.Config{
		Account:       account,
		Key:           key,
		ContainerName: conformanceBucket,
		Endpoint:      endpoint,
	})
	if err != nil {
		t.Fatalf("azure.New: %v", err)
	}
	return backend
}

func setupGCS(t *testing.T, ctx context.Context) storage.ObjectStore {
	t.Helper()
	endpoint := os.Getenv("GCS_ENDPOINT")
	if endpoint == "" {
		t.Skip("GCS_ENDPOINT not set; skipping GCS conformance")
	}

	// fake-gcs-server (this suite's GCS emulator) implements the standard
	// JSON API's bucket-create endpoint and accepts unauthenticated calls,
	// so a plain HTTP POST is enough to provision the physical bucket —
	// no service-account credentials are needed for this step.
	createURL := fmt.Sprintf("%sb?project=elitea-conformance", endpoint)
	resp, err := http.Post(createURL, "application/json",
		strings.NewReader(fmt.Sprintf(`{"name":%q}`, conformanceBucket)))
	if err != nil {
		t.Fatalf("gcs conformance setup: create bucket: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusConflict {
		t.Fatalf("gcs conformance setup: create bucket: unexpected status %d", resp.StatusCode)
	}

	backend, err := gcs.New(ctx, gcs.Config{
		Bucket:   conformanceBucket,
		Endpoint: endpoint,
	})
	if err != nil {
		t.Fatalf("gcs.New: %v", err)
	}
	return backend
}
