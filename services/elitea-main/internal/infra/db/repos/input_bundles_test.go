package repos

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

func TestNodeRecoveryManifestKeepsOriginalFenceAndImmutableIdentity(t *testing.T) {
	manifest, err := proto.Marshal(&runtimev1.ExecutionInputBundleV1{InputBundleId: "original-bundle"})
	if err != nil {
		t.Fatal(err)
	}
	digest := runtimedomain.SHA256(manifest)
	fence := runtimedomain.Fence{ExecutionID: "execution-1", CommandID: "command-1", Generation: 3,
		WorkloadIdentity: "spiffe://elitea.internal/runtime/worker-1", WorkloadSessionID: "session-1",
		ProducerID: "producer-1", ClaimAttempt: 2, LeaseEpoch: 4, Token: runtimedomain.FenceToken{1}}
	reference := &runtimev1.ExecutionInputBundleReferenceV1{InputBundleId: "original-bundle", ImmutableVersion: "version-1",
		MediaType: executiondomain.InputBundleManifestMediaType, ByteLength: uint64(len(manifest)),
		Digest: &runtimev1.DigestV1{Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256, Value: digest[:]}}
	for _, tc := range []struct {
		name     string
		change   func(*scriptedRow)
		expected error
	}{
		{"original", func(*scriptedRow) {}, nil},
		{"stale claim", func(row *scriptedRow) { row.err = pgx.ErrNoRows }, runtimedomain.ErrStaleFence},
		{"other immutable version", func(row *scriptedRow) { row.values[1] = "version-2" }, executiondomain.ErrInvalidInputBundle},
		{"changed manifest", func(row *scriptedRow) { row.values[3] = bytes.Repeat([]byte{9}, len(manifest)) }, executiondomain.ErrInvalidInputBundle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := scriptedRow{values: []any{"original-bundle", "version-1", executiondomain.InputBundleManifestMediaType, manifest, digest[:], int64(len(manifest))}}
			tc.change(&row)
			store := &scriptedExecutor{rowResults: []scriptedRow{row}}
			repository := &InputBundlesRepository{store: store}
			got, err := repository.ResolveClaimInput(t.Context(), fence, reference)
			if !errors.Is(err, tc.expected) {
				t.Fatalf("ResolveClaimInput error = %v, want %v", err, tc.expected)
			}
			if tc.expected == nil && got.GetInputBundleId() != reference.GetInputBundleId() {
				t.Fatal("original manifest changed")
			}
			if len(store.rowCalls) != 1 || len(store.execCalls) != 0 {
				t.Fatal("inspection mutated the claim")
			}
			call := store.rowCalls[0]
			for _, guard := range []string{"c.released_at IS NULL", "c.lease_expires_at > clock_timestamp()", "j.desired_state = 'RUNNING' AND c.recovery_mode <> 'NODE_RECOVERY'", "j.desired_state IN ('SUSPENDED', 'RUNNING') AND c.recovery_mode = 'NODE_RECOVERY'", "'agent.execute.application.v1', 'agent.execute.adhoc.v1'"} {
				if !strings.Contains(call.sql, guard) {
					t.Fatalf("missing inspection guard %s", guard)
				}
			}
			want := []any{fence.ExecutionID, int64(fence.Generation), fence.CommandID, fence.WorkloadIdentity, fence.WorkloadSessionID, fence.ProducerID, int64(fence.ClaimAttempt), int64(fence.LeaseEpoch)}
			for i, expected := range want {
				if call.args[i] != expected {
					t.Fatalf("fence argument %d changed", i)
				}
			}
			if len(call.args) != 10 || !bytes.Equal(call.args[8].([]byte), fence.Token[:]) || call.args[9] != reference.GetInputBundleId() {
				t.Fatal("original fence or bundle selector lost")
			}
		})
	}
}
