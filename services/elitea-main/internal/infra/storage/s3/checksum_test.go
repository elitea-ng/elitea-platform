package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

func checksumBackend(t *testing.T, endpoint, bucket string) *Backend {
	t.Helper()
	backend, err := New(context.Background(), Config{
		Endpoint: endpoint, AccessKey: "testaccesskey", SecretKey: "testsecretkey",
		Region: "us-east-1", ForcePathStyle: true, Bucket: bucket,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return backend
}

func checksumRef(t *testing.T) storage.ObjectRef {
	t.Helper()
	ref, err := storage.NewObjectRef("7", "elitea-workspaces", "t/seg/a.seg")
	if err != nil {
		t.Fatalf("ref: %v", err)
	}
	return ref
}

// The presigned PUT signs the checksum header: the client must send that
// exact value, and the store refuses bytes that do not match it.
func TestPresignPutSignsTheChecksumHeader(t *testing.T) {
	backend := checksumBackend(t, "http://127.0.0.1:1", "physical")
	digest := sha256.Sum256([]byte("segment"))
	raw, err := backend.PresignPut(context.Background(), checksumRef(t), time.Minute, storage.PutOptions{
		ContentType: "application/x-git-bundle", ChecksumSHA256: digest[:],
	})
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	signed := parsed.Query().Get("X-Amz-SignedHeaders")
	if !strings.Contains(signed, storage.ChecksumSHA256Header) {
		t.Fatalf("signed headers %q do not include %s (url %s)", signed, storage.ChecksumSHA256Header, raw)
	}
	if strings.Contains(strings.ToLower(raw), "x-amz-checksum-sha256=") {
		t.Fatalf("checksum was hoisted into the query: %s", raw)
	}
}

func TestPresignPutWithoutAChecksumSignsNoChecksumHeader(t *testing.T) {
	backend := checksumBackend(t, "http://127.0.0.1:1", "physical")
	raw, err := backend.PresignPut(context.Background(), checksumRef(t), time.Minute, storage.PutOptions{})
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	if strings.Contains(raw, storage.ChecksumSHA256Header) {
		t.Fatalf("unexpected checksum header in %s", raw)
	}
}

// Stat asks for the checksum and reports a full-object SHA-256 only.
func TestStatReportsTheFullObjectChecksum(t *testing.T) {
	digest := sha256.Sum256([]byte("segment"))
	encoded := base64.StdEncoding.EncodeToString(digest[:])
	var mu sync.Mutex
	mode := ""
	checksum, kind := encoded, "FULL_OBJECT"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		mode = r.Header.Get("x-amz-checksum-mode")
		w.Header().Set("Content-Length", "7")
		if checksum != "" {
			w.Header().Set("x-amz-checksum-sha256", checksum)
			w.Header().Set("x-amz-checksum-type", kind)
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	backend := checksumBackend(t, server.URL, "physical")

	info, err := backend.Stat(context.Background(), checksumRef(t))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	mu.Lock()
	seenMode := mode
	mu.Unlock()
	if seenMode != "ENABLED" {
		t.Fatalf("x-amz-checksum-mode = %q, want ENABLED", seenMode)
	}
	if !bytes.Equal(info.DigestSHA256, digest[:]) || info.Size != 7 {
		t.Fatalf("info = %+v, want the full-object digest", info)
	}

	for _, tc := range []struct{ name, checksum, kind string }{
		{"composite", encoded + "-3", "COMPOSITE"},
		{"composite type", encoded, "COMPOSITE"},
		{"absent", "", ""},
		{"malformed", "not-base64!", "FULL_OBJECT"},
	} {
		mu.Lock()
		checksum, kind = tc.checksum, tc.kind
		mu.Unlock()
		info, err := backend.Stat(context.Background(), checksumRef(t))
		if err != nil {
			t.Fatalf("%s: Stat: %v", tc.name, err)
		}
		if info.DigestSHA256 != nil {
			t.Fatalf("%s: digest = %x, want none", tc.name, info.DigestSHA256)
		}
	}
}

func TestPutSendsTheChecksum(t *testing.T) {
	digest := sha256.Sum256([]byte("segment"))
	var mu sync.Mutex
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		got = r.Header.Get("x-amz-checksum-sha256")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	backend := checksumBackend(t, server.URL, "physical")
	if _, err := backend.Put(context.Background(), checksumRef(t), bytes.NewReader([]byte("segment")), storage.PutOptions{
		ContentLength: 7, ChecksumSHA256: digest[:],
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got != base64.StdEncoding.EncodeToString(digest[:]) {
		t.Fatalf("checksum header = %q", got)
	}
}

// TestLiveChecksumBoundPresignedPut runs against RustFS (CI's S3 emulator):
// the store refuses a PUT whose bytes do not match the signed checksum, and
// HEAD reports the digest of the bytes it kept.
func TestLiveChecksumBoundPresignedPut(t *testing.T) {
	endpoint := os.Getenv("S3_ENDPOINT_URL")
	if endpoint == "" {
		t.Skip("S3_ENDPOINT_URL not set; skipping (see deploy/docker-compose.storage-test.yml)")
	}
	ctx := context.Background()
	const bucket = "elitea-checksum-test"
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			os.Getenv("S3_ACCESS_KEY"), os.Getenv("S3_SECRET_KEY"), "")))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	raw := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
	if _, err := raw.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		var owned *types.BucketAlreadyOwnedByYou
		var exists *types.BucketAlreadyExists
		if !errors.As(err, &owned) && !errors.As(err, &exists) {
			t.Fatalf("create bucket: %v", err)
		}
	}
	backend, err := New(ctx, Config{
		Endpoint: endpoint, AccessKey: os.Getenv("S3_ACCESS_KEY"), SecretKey: os.Getenv("S3_SECRET_KEY"),
		Region: "us-east-1", ForcePathStyle: true, Bucket: bucket,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// RustFS implements GetBucketEncryption: a bucket without a default
	// rule is a refusal, not "cannot report", so no attestation applies.
	if confirmed, err := backend.DefaultEncryptionConfirmed(ctx); confirmed || err != nil {
		t.Fatalf("DefaultEncryptionConfirmed on a bucket without a rule = %t, %v; want false, nil", confirmed, err)
	}
	ref, err := storage.NewObjectRef("7", "elitea-workspaces", "t/seg/"+time.Now().Format("150405.000000")+".seg")
	if err != nil {
		t.Fatalf("ref: %v", err)
	}
	content := []byte("checksum-bound segment")
	digest := sha256.Sum256(content)
	encoded := base64.StdEncoding.EncodeToString(digest[:])
	putURL, err := backend.PresignPut(ctx, ref, time.Minute, storage.PutOptions{
		ContentType: "application/octet-stream", ChecksumSHA256: digest[:],
	})
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	put := func(body []byte) int {
		request, _ := http.NewRequestWithContext(ctx, http.MethodPut, putURL, bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/octet-stream")
		request.Header.Set(storage.ChecksumSHA256Header, encoded)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("PUT: %v", err)
		}
		_ = response.Body.Close()
		return response.StatusCode
	}
	if status := put([]byte("checksum-bound segmenX")); status < 400 {
		t.Fatalf("PUT of other bytes answered %d, want a refusal", status)
	}
	if _, err := backend.Stat(ctx, ref); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Stat after a refused PUT = %v, want not found", err)
	}
	if status := put(content); status != http.StatusOK {
		t.Fatalf("PUT answered %d", status)
	}
	info, err := backend.Stat(ctx, ref)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !bytes.Equal(info.DigestSHA256, digest[:]) || info.Size != int64(len(content)) {
		t.Fatalf("Stat = %+v, want size %d and the digest", info, len(content))
	}
	// A server-side copy keeps the full-object digest (the facade's promote).
	copied, err := storage.NewObjectRef("7", "elitea-workspaces", ref.Key()+".copy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Copy(ctx, ref, copied); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	copiedInfo, err := backend.Stat(ctx, copied)
	if err != nil || !bytes.Equal(copiedInfo.DigestSHA256, digest[:]) || copiedInfo.Size != int64(len(content)) {
		t.Fatalf("Stat of the copy = %+v, %v; want the same size and digest", copiedInfo, err)
	}
	_ = backend.Delete(ctx, copied)
	_ = backend.Delete(ctx, ref)
}
