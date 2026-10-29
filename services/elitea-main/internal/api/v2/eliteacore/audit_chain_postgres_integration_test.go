package eliteacore_test

// The audit chain verification route over a real centry.audit_events with
// shared migration 0206 applied on top of the bootstrap schema. Skipped
// without ELITEA_TEST_DATABASE_URL, like the audit-trail reads beside it.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
)

type auditChainBody struct {
	OK        bool   `json:"ok"`
	Partition string `json:"partition"`
	From      int64  `json:"from"`
	Checked   int64  `json:"checked"`
	HeadSeq   int64  `json:"head_seq"`
	Truncated bool   `json:"truncated"`
	NextFrom  *int64 `json:"next_from"`
	Unchained int64  `json:"unchained"`
	Break     *struct {
		PartitionSeq int64  `json:"partition_seq"`
		RowID        *int64 `json:"row_id"`
		Reason       string `json:"reason"`
	} `json:"break"`
}

func TestAuditChainVerifyAnswersHoldingAndBroken(t *testing.T) {
	pool := newAuditPool(t)
	ctx := context.Background()
	router := auditChainRouter(eliteacore.NewHandler(pool))

	// Before 0206 there is no chain to verify.
	if got := auditGet(t, router, auditChainPath).Code; got != 503 {
		t.Fatalf("before 0206: status %d, want 503", got)
	}
	migration, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "migrations", "shared", "0206_audit_events_chain.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		t.Fatalf("apply 0206: %v", err)
	}
	seedAuditEvents(t, pool, []seedSpan{
		{offset: 0, eventType: "api", action: "POST /a", statusCode: 200, durationMS: 1.5},
		{offset: time.Second, eventType: "api", action: "POST /b", statusCode: 403, durationMS: 2, isError: true},
		{offset: 2 * time.Second, eventType: "api", action: "POST /c", statusCode: 200, durationMS: 0.25},
	})

	held := decodeOK[auditChainBody](t, auditGet(t, router, auditChainPath), "verify")
	if !held.OK || held.Break != nil || held.Partition != "0" || held.From != 1 || held.Checked != 3 || held.HeadSeq != 3 || held.Truncated {
		t.Fatalf("intact chain: %+v", held)
	}
	paged := decodeOK[auditChainBody](t, auditGet(t, router, auditChainPath+"?limit=2"), "verify limit 2")
	if !paged.OK || !paged.Truncated || paged.NextFrom == nil || *paged.NextFrom != 3 {
		t.Fatalf("paged: %+v", paged)
	}

	var rowID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM centry.audit_events WHERE partition_seq = 2`).Scan(&rowID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
ALTER TABLE centry.audit_events DISABLE TRIGGER audit_events_append_only;
UPDATE centry.audit_events SET status_code = 200, is_error = false WHERE partition_seq = 2;
ALTER TABLE centry.audit_events ENABLE TRIGGER audit_events_append_only`); err != nil {
		t.Fatal(err)
	}
	broken := decodeOK[auditChainBody](t, auditGet(t, router, auditChainPath), "verify after tampering")
	if broken.OK || broken.Break == nil || broken.Break.PartitionSeq != 2 || broken.Break.RowID == nil ||
		*broken.Break.RowID != rowID || broken.Checked != 1 {
		t.Fatalf("tampered row 2: %+v (break %+v)", broken, broken.Break)
	}
}
