package runtimegrpc

import (
	"errors"
	"strings"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func TestStrictProtoCodecRejectsDuplicateUnknownAndConflictingOneofBeforeDecode(t *testing.T) {
	codec, err := NewStrictProtoCodec(64 * 1024)
	if err != nil {
		t.Fatal(err)
	}
	frame := &runtimev1.ExecutionOutputFrameV1{
		OutputSchemaRevision: "elitea.runtime.execution-output.v1",
		StreamId:             "execution-1:1",
		Identity: &runtimev1.ExecutionIdentityV1{
			TenantId: "tenant-1", ExecutionId: "execution-1", Generation: 1,
		},
		Payload: &runtimev1.ExecutionOutputFrameV1_ConfigurationValidation{
			ConfigurationValidation: &runtimev1.ConfigurationValidationResultV1{ConfigurationRevisionId: "revision-1"},
		},
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{name: "duplicate singular", mutate: func(encoded []byte) []byte {
			encoded = protowire.AppendTag(encoded, 2, protowire.BytesType)
			return protowire.AppendString(encoded, "execution-2:1")
		}},
		{name: "unknown reserved", mutate: func(encoded []byte) []byte {
			encoded = protowire.AppendTag(encoded, 31, protowire.VarintType)
			return protowire.AppendVarint(encoded, 1)
		}},
		{name: "conflicting payload oneof", mutate: func(encoded []byte) []byte {
			failure, marshalErr := proto.Marshal(&runtimev1.RuntimeErrorV1{
				Code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL,
			})
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			encoded = protowire.AppendTag(encoded, 21, protowire.BytesType)
			return protowire.AppendBytes(encoded, failure)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var target runtimev1.ExecutionOutputFrameV1
			if err := codec.Unmarshal(test.mutate(append([]byte(nil), raw...)), &target); !errors.Is(err, ErrStrictProto) {
				t.Fatalf("strict wire mutation was decoded: %v", err)
			}
		})
	}
}

func TestStrictProtoCodecEnforcesListenerBoundBeforeDecode(t *testing.T) {
	codec, err := NewStrictProtoCodec(8)
	if err != nil {
		t.Fatal(err)
	}
	var target runtimev1.ExecutionOutputFrameV1
	if err := codec.Unmarshal(make([]byte, 9), &target); !errors.Is(err, ErrStrictProto) {
		t.Fatalf("oversize protobuf reached decode: %v", err)
	}
}

func TestStrictProtoCodecEnforcesDirectionalWholeMessageBoundaries(t *testing.T) {
	const (
		maxRequestBytes  = 64 * 1024
		maxResponseBytes = 80 * 1024
	)
	codec, err := NewDirectionalStrictProtoCodec(maxRequestBytes, maxResponseBytes)
	if err != nil {
		t.Fatal(err)
	}

	requestAtLimit := exactSizeClaimRequest(t, maxRequestBytes)
	requestBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(requestAtLimit)
	if err != nil {
		t.Fatal(err)
	}
	var decodedRequest runtimev1.ClaimCommandRequestV1
	if err := codec.Unmarshal(requestBytes, &decodedRequest); err != nil {
		t.Fatalf("request at exact receive limit was rejected: %v", err)
	}
	requestOverLimit := exactSizeClaimRequest(t, maxRequestBytes+1)
	requestBytes, err = proto.MarshalOptions{Deterministic: true}.Marshal(requestOverLimit)
	if err != nil {
		t.Fatal(err)
	}
	if err := codec.Unmarshal(requestBytes, &decodedRequest); !errors.Is(err, ErrStrictProto) {
		t.Fatalf("request over receive limit was accepted: %v", err)
	}

	responseAtLimit := exactSizeClaimResponse(t, maxResponseBytes)
	responseBytes, err := codec.Marshal(responseAtLimit)
	if err != nil {
		t.Fatalf("response at exact send limit was rejected: %v", err)
	}
	if len(responseBytes) != maxResponseBytes {
		t.Fatalf("response size = %d, want %d", len(responseBytes), maxResponseBytes)
	}
	responseOverLimit := exactSizeClaimResponse(t, maxResponseBytes+1)
	if _, err := codec.Marshal(responseOverLimit); !errors.Is(err, ErrStrictProto) {
		t.Fatalf("response over send limit was accepted: %v", err)
	}
}

func TestControlResponseLimitIncludesMaximumManifestAndReceiptEnvelope(t *testing.T) {
	const (
		maxManifestBytes = 64 * 1024
		maxResponseBytes = 80 * 1024
	)
	manifest := exactSizeInputBundle(t, maxManifestBytes)
	response := &runtimev1.ClaimCommandResponseV1{
		Receipt: &runtimev1.ClaimReceiptV1{
			Disposition: runtimev1.ClaimDispositionV1_CLAIM_DISPOSITION_V1_ACCEPTED,
			InputBundle: manifest,
			ClaimId:     "claim-1",
		},
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) <= maxManifestBytes || len(encoded) > maxResponseBytes {
		t.Fatalf("claim response size = %d; want (%d, %d]", len(encoded), maxManifestBytes, maxResponseBytes)
	}
	codec, err := NewDirectionalStrictProtoCodec(1, maxResponseBytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.Marshal(response); err != nil {
		t.Fatalf("maximum manifest no longer fits its control response: %v", err)
	}
}

func exactSizeClaimRequest(t *testing.T, size int) *runtimev1.ClaimCommandRequestV1 {
	t.Helper()
	message := &runtimev1.ClaimCommandRequestV1{}
	setStringForExactMessageSize(t, message, size, func(value string) {
		message.WorkloadSessionId = value
	})
	return message
}

func exactSizeClaimResponse(t *testing.T, size int) *runtimev1.ClaimCommandResponseV1 {
	t.Helper()
	message := &runtimev1.ClaimCommandResponseV1{Rejection: &runtimev1.RuntimeErrorV1{}}
	setStringForExactMessageSize(t, message, size, func(value string) {
		message.Rejection.SafeMessage = value
	})
	return message
}

func exactSizeInputBundle(t *testing.T, size int) *runtimev1.ExecutionInputBundleV1 {
	t.Helper()
	message := &runtimev1.ExecutionInputBundleV1{}
	setStringForExactMessageSize(t, message, size, func(value string) {
		message.InputBundleId = value
	})
	return message
}

func setStringForExactMessageSize(t *testing.T, message proto.Message, size int, set func(string)) {
	t.Helper()
	low, high := 0, size
	for low <= high {
		middle := low + (high-low)/2
		set(strings.Repeat("x", middle))
		encodedSize := proto.Size(message)
		switch {
		case encodedSize < size:
			low = middle + 1
		case encodedSize > size:
			high = middle - 1
		default:
			return
		}
	}
	t.Fatalf("cannot construct %T at exact size %d", message, size)
}
