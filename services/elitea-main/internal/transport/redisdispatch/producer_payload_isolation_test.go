package redisdispatch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"google.golang.org/protobuf/proto"
)

// This is a protocol/unit invariant, not evidence that the current 256 KiB
// PostgreSQL content backend can serve a production-scale 32 MiB image. It
// proves that once bulk content is on a separate data plane, its size and bytes
// cannot scale the Redis command: Redis receives only the bounded bundle ref.
func TestReferenceOnlyCommandDoesNotScaleWith32MiBDataPlaneObject(t *testing.T) {
	marker := []byte("ELITEA_32_MIB_DATA_PLANE_BODY_CANARY")
	smallManifest := referenceManifest(t, marker, 1<<10)
	largeManifest := referenceManifest(t, marker, 32<<20)

	small := validTransportDispatch()
	small.InputBundleByteLength = uint64(len(smallManifest))
	small.InputBundleDigest = runtimedomain.SHA256(smallManifest)
	large := validTransportDispatch()
	large.InputBundleByteLength = uint64(len(largeManifest))
	large.InputBundleDigest = runtimedomain.SHA256(largeManifest)

	prepare := func(dispatchName string, dispatchValue runtimedomain.Digest, dispatchBytes uint64) []byte {
		t.Helper()
		dispatch := validTransportDispatch()
		dispatch.InputBundleDigest = dispatchValue
		dispatch.InputBundleByteLength = dispatchBytes
		dispatch.InputBundleID = "bundle-" + dispatchName
		producer, err := NewProducer(validProducerConfig(), &signerStub{}, &appenderStub{})
		if err != nil {
			t.Fatal(err)
		}
		prepared, err := producer.PrepareValidation(context.Background(), dispatch)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(prepared.Bytes, marker) {
			t.Fatal("Redis command contains a data-plane body canary")
		}
		return prepared.Bytes
	}

	smallEnvelope := prepare("small", small.InputBundleDigest, small.InputBundleByteLength)
	largeEnvelope := prepare("large", large.InputBundleDigest, large.InputBundleByteLength)
	if difference := abs(len(largeEnvelope) - len(smallEnvelope)); difference > 16 {
		t.Fatalf("control envelope scaled with referenced content: small=%d large=%d delta=%d", len(smallEnvelope), len(largeEnvelope), difference)
	}
	if encodedRedisEntryBytes(redisEnvelopeField, largeEnvelope) > validProducerConfig().Limits.MaxTransportMessageBytes {
		t.Fatalf("32 MiB data-plane reference exceeded Redis entry bound: envelope=%d", len(largeEnvelope))
	}
}

func referenceManifest(t *testing.T, marker []byte, contentLength int) []byte {
	t.Helper()
	contentDigest := repeatedMarkerDigest(marker, contentLength)
	manifest := &runtimev1.ExecutionInputBundleV1{
		InputBundleId:    "bundle-reference",
		ImmutableVersion: "v1",
		Entries: []*runtimev1.ExecutionInputEntryV1{{
			EntryId:          "image",
			ImmutableVersion: "v1",
			SemanticRole:     "indexing.image",
			Content: &runtimev1.ScopedContentReferenceV1{
				ContentId:        "content-reference",
				ImmutableVersion: "v1",
				MediaType:        "image/png",
				ByteLength:       uint64(contentLength),
				Digest: &runtimev1.DigestV1{
					Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256,
					Value:     append([]byte(nil), contentDigest[:]...),
				},
				Classification:        "tenant-confidential",
				RequiredGrantAudience: "elitea.runtime.input.read.v1",
			},
		}},
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func repeatedMarkerDigest(marker []byte, total int) runtimedomain.Digest {
	hasher := sha256.New()
	chunk := make([]byte, 32*1024)
	for index := range chunk {
		chunk[index] = marker[index%len(marker)]
	}
	for remaining := total; remaining > 0; {
		count := min(remaining, len(chunk))
		_, _ = hasher.Write(chunk[:count])
		remaining -= count
	}
	var digest runtimedomain.Digest
	copy(digest[:], hasher.Sum(nil))
	return digest
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
