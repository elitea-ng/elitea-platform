package eliteacore

// The audit trail's chain verification (ADR-0042 decision 3, issue #1499):
//
//	GET /elitea_core/audit_chain_verify/administration?from=&limit=
//
// Shared migration 0206 hash-chains centry.audit_events; this route walks the
// chain from the retention prune anchor (or from `from`) and answers whether
// it holds, how many rows it checked, and the first break. It reads the same
// table as the four audit-trail reads in audit.go and takes the same gate,
// `models.admin.audit_trail.view` in administration mode. A STATIC
// `administration` segment, not `{mode}`: no other mode exists for it.
//
// The walk runs in one REPEATABLE READ, read-only transaction, so a retention
// pass that commits mid-walk cannot read as missing rows. No row content is
// returned or logged: a break names a chain position, a row id and a reason.

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

type auditChainBreak struct {
	PartitionSeq int64  `json:"partition_seq"`
	RowID        *int64 `json:"row_id"`
	Reason       string `json:"reason"`
}

type auditChainReport struct {
	OK            bool             `json:"ok"`
	Partition     string           `json:"partition"`
	From          int64            `json:"from"`
	Checked       int64            `json:"checked"`
	HeadSeq       int64            `json:"head_seq"`
	PrunedThrough int64            `json:"pruned_through"`
	Truncated     bool             `json:"truncated"`
	NextFrom      *int64           `json:"next_from"`
	Unchained     int64            `json:"unchained"`
	Break         *auditChainBreak `json:"break"`
}

// AuditChainVerify serves `GET /elitea_core/audit_chain_verify/administration`.
func (h *Handler) AuditChainVerify(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	var from, limit int64
	if raw := query.Get("from"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 1 {
			apierr.Write(w, apierr.BadRequest("from must be a positive integer"))
			return
		}
		from = parsed
	}
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 1 || parsed > audit.MaxChainVerifyRows {
			apierr.Write(w, apierr.BadRequest("limit must be a whole number from 1 to 1000000"))
			return
		}
		limit = parsed
	}
	if h.pool == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "the audit trail store is not configured")
		return
	}

	ctx := r.Context()
	tx, err := h.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		auditReadFailed(w, "audit chain")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	report, err := audit.VerifyChain(ctx, tx, from, limit)
	if errors.Is(err, audit.ErrChainNotInstalled) {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "the audit_events chain is not installed (shared migration 0206)")
		return
	}
	if err != nil {
		auditReadFailed(w, "audit chain")
		return
	}

	view := auditChainReport{
		OK: report.Break == nil, Partition: report.Partition, From: report.From, Checked: report.Checked,
		HeadSeq: report.HeadSeq, PrunedThrough: report.PrunedThrough, Truncated: report.Truncated,
		Unchained: report.Unchained,
	}
	if report.Truncated {
		next := report.NextFrom
		view.NextFrom = &next
	}
	if b := report.Break; b != nil {
		view.Break = &auditChainBreak{PartitionSeq: b.PartitionSeq, Reason: b.Reason}
		if b.Seq > 0 {
			rowID := b.Seq
			view.Break.RowID = &rowID
		}
	}
	writeJSON(w, http.StatusOK, view)
}
