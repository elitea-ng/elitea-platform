package runtimecomposition

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/protobuf/proto"
)

func TestProductionGRPCWireLimitsMatchCheckedProtocolProfile(t *testing.T) {
	t.Parallel()
	_, sourceFile, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	profilePath := filepath.Clean(filepath.Join(
		filepath.Dir(sourceFile),
		"../../../../testdata/proto/runtime/v1/configuration-validation/conformance-limits.pb",
	))
	raw, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	profile := &runtimev1.ProtocolLimitsV1{}
	if err := proto.Unmarshal(raw, profile); err != nil {
		t.Fatal(err)
	}
	if profile.GetMaxGrpcRequestBytes() != maxGRPCRequestBytes || profile.GetMaxGrpcResponseBytes() != maxGRPCResponseBytes {
		t.Fatalf(
			"production gRPC limits (%d, %d) drifted from profile (%d, %d)",
			maxGRPCRequestBytes,
			maxGRPCResponseBytes,
			profile.GetMaxGrpcRequestBytes(),
			profile.GetMaxGrpcResponseBytes(),
		)
	}
	if profile.GetMaxSignedEnvelopeBytes() >= profile.GetMaxGrpcRequestBytes() || profile.GetMaxOutputFrameBytes() > profile.GetMaxGrpcRequestBytes() {
		t.Fatal("gRPC request limit cannot carry a maximum protocol-v1 request")
	}
	if profile.GetMaxInputManifestBytes() >= profile.GetMaxGrpcResponseBytes() {
		t.Fatal("gRPC response limit cannot carry a maximum manifest plus its receipt")
	}
}
