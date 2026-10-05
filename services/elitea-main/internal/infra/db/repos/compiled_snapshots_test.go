package repos

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
)

type snapshotProofQuery struct {
	values    []any
	statement string
}

func (q *snapshotProofQuery) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	q.statement = sql
	return snapshotProofRow{q.values}
}

type snapshotProofRow struct{ values []any }

func (r snapshotProofRow) Scan(dest ...any) error {
	for i, v := range r.values {
		reflect.ValueOf(dest[i]).Elem().Set(reflect.ValueOf(v))
	}
	return nil
}
func TestCompiledSnapshotOriginalReceiptAndCleanupProof(t *testing.T) {
	for _, name := range []string{"captured", "captured-takeover", "completed", "complete-requires-terminal", "no-cleanup", "wrong-output", "nonzero-exit", "cancelled", "failed", "expired-lease", "no-export", "wrong-runtime", "wrong-request", "wrong-base", "cleanup-before-completion"} {
		t.Run(name, func(t *testing.T) {
			raw, _ := os.ReadFile("testdata/compiled-snapshot-v1/descriptor.json")
			root := domain.SnapshotContentSHA256(raw)
			d, err := domain.ParseRustSnapshotDescriptor(raw, root)
			if err != nil {
				t.Fatal(err)
			}
			key := d.SnapshotKeySHA256
			digest, _ := domain.SnapshotJobDigest("compile", d.Binding, "")
			runtimeID := "original-runtime"
			var receipt *string
			var cleanup *time.Time
			phase := "dispatched"
			live := true
			exported := true
			cancelled := false
			complete := false
			epoch := int64(3)
			output, _ := domain.SnapshotJSON(map[string]any{"revision": 1, "compiled_artifact": d})
			envelope := map[string]any{"revision": 1, "status": "completed", "exit_code": 0, "stdout": string(output) + "\n", "stderr": ""}
			body, _ := json.Marshal(envelope)
			receiptValue := string(body)
			now := time.Unix(1000, 0)
			valid := name == "captured" || name == "captured-takeover" || name == "expired-lease" || name == "completed"
			switch name {
			case "captured-takeover":
				epoch = 4
			case "completed":
				phase = "completed"
				receipt = &receiptValue
				cleanup = &now
				complete = true
			case "complete-requires-terminal":
				complete = true
			case "no-cleanup":
				phase = "completed"
				receipt = &receiptValue
				complete = true
			case "wrong-output":
				phase = "completed"
				receiptValue = strings.Replace(receiptValue, "compiled_artifact", "other_artifact", 1)
				receipt = &receiptValue
				cleanup = &now
				complete = true
			case "nonzero-exit":
				phase = "completed"
				receiptValue = strings.Replace(receiptValue, `"exit_code":0`, `"exit_code":1`, 1)
				receipt = &receiptValue
				cleanup = &now
				complete = true
			case "cancelled":
				cancelled = true
			case "failed":
				phase = "failed"
			case "expired-lease":
				live = false
			case "no-export":
				exported = false
			case "wrong-runtime":
				runtimeID = ""
			case "wrong-request":
				digest = strings.Repeat("a", 64)
			case "wrong-base":
				d.Binding.BasePreparedRequestSHA256 = strings.Repeat("a", 64)
			case "cleanup-before-completion":
				cleanup = &now
			}
			query := &snapshotProofQuery{values: []any{"compile", snapshotBytes(key), snapshotBytes(d.Binding.BasePreparedRequestSHA256), raw, exported, int64(3), snapshotBytes(digest), &runtimeID, epoch, phase, receipt, cleanup, cancelled, live}}
			c, err := snapshotCandidate(context.Background(), query, domain.SnapshotScope{TenantID: d.Binding.TenantID, ProjectID: d.Binding.ProjectID}, key, strings.Repeat("b", 64), complete)
			if (err == nil) != valid {
				t.Fatalf("proof status %v expected valid=%v", err, valid)
			}
			if valid && c.CompilationLeaseEpoch != 3 {
				t.Fatal("publication takeover changed immutable export epoch")
			}
			if valid && name == "completed" && !domain.SnapshotDigest(c.ReceiptSHA256) {
				t.Fatal("missing exact receipt hash")
			}
			if !strings.Contains(query.statement, "FOR SHARE") {
				t.Fatal("unlocked original proof")
			}
		})
	}
}
func TestCompiledSnapshotQuotaConfigurationBounds(t *testing.T) {
	q := SnapshotQuota{GlobalEntries: 100, GlobalBytes: 1 << 30, TenantEntries: 10, TenantBytes: 1 << 28, PublishingTTL: time.Minute, ReadyTTL: time.Hour}
	if q.Validate() != nil {
		t.Fatal("valid quota rejected")
	}
	for _, change := range []func(*SnapshotQuota){func(q *SnapshotQuota) { q.GlobalEntries = 100001 }, func(q *SnapshotQuota) { q.TenantEntries = 101 }, func(q *SnapshotQuota) { q.GlobalBytes = 1 << 41 }, func(q *SnapshotQuota) { q.TenantBytes = q.GlobalBytes + 1 }, func(q *SnapshotQuota) { q.PublishingTTL = 6 * time.Minute }, func(q *SnapshotQuota) { q.ReadyTTL = 25 * time.Hour }} {
		c := q
		change(&c)
		if c.Validate() == nil {
			t.Fatal("unbounded quota accepted")
		}
	}
}

func TestCompiledSnapshotOriginalExecutionRecoveryWithoutCacheRead(t *testing.T) {
	for _, name := range []string{"dispatched", "completed", "failed", "cancelled", "uncertain", "reserved", "ordinary", "wrong-root", "wrong-digest", "missing-runtime"} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile("testdata/compiled-snapshot-v1/descriptor.json")
			if err != nil {
				t.Fatal(err)
			}
			root := domain.SnapshotContentSHA256(raw)
			d, err := domain.ParseRustSnapshotDescriptor(raw, root)
			if err != nil {
				t.Fatal(err)
			}
			purpose := "execute"
			var role *string = &purpose
			runtimeID := "original-execution-runtime"
			phase := name
			digest, _ := domain.SnapshotJobDigest("execute", d.Binding, root)
			valid := name == "dispatched" || name == "completed" || name == "failed" || name == "cancelled" || name == "uncertain"
			switch name {
			case "ordinary":
				role = nil
				phase = "completed"
			case "wrong-root":
				root = strings.Repeat("a", 64)
				phase = "completed"
			case "wrong-digest":
				digest = strings.Repeat("a", 64)
				phase = "completed"
			case "missing-runtime":
				runtimeID = ""
				phase = "dispatched"
			}
			query := &snapshotProofQuery{values: []any{role, snapshotBytes(d.SnapshotKeySHA256), snapshotBytes(d.Binding.BasePreparedRequestSHA256), raw, snapshotBytes(digest), &runtimeID, phase}}
			original, found, err := originalSnapshotExecution(context.Background(), query, domain.SnapshotScope{TenantID: d.Binding.TenantID, ProjectID: d.Binding.ProjectID}, d.SnapshotKeySHA256, strings.Repeat("b", 64), root)
			if name == "reserved" {
				if found || err != nil {
					t.Fatal("reserved bypasses content admission", found, err)
				}
				return
			}
			if !found || (err == nil) != valid {
				t.Fatal(found, err)
			}
			if valid && (!bytes.Equal(original.DescriptorJSON, raw) || original.RuntimeID != runtimeID) {
				t.Fatal("changed original execution")
			}
			if strings.Contains(query.statement, "rust_compiled_snapshots") {
				t.Fatal("recovery renewed cache lookup")
			}
		})
	}
}
