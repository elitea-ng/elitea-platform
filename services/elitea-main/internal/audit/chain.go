package audit

// The hash chain over centry.audit_events (ADR-0042 decision 3, issue #1499).
//
// Shared migration 0206 chains every inserted row in its inserting
// transaction with a BEFORE INSERT trigger, so the batched recorder in this
// package, the browser session relay and the cloud executor attestation are
// all chained without each carrying a copy of the logic. The link is
// securityaudit.ChainHash: sha256(prev_hash || canonical), where canonical is
// centry.audit_event_canonical(row). This file verifies it with
// securityaudit.ChainVerifier, reading the canonical form from that same
// function so the bytes hashed and the bytes checked cannot drift apart.
//
// Rows written before 0206 have NULL chain columns and are counted, not
// checked. Retention deletes the oldest rows as a contiguous prefix and moves
// the head's prune anchor in the same statement
// (services/elitea-scheduler/internal/auditretention), so verification starts
// at the anchor rather than reporting the deleted prefix as missing.

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// ChainPartition is the one partition of the audit_events chain: the
// platform partition, as in the security audit. Migration 0206 says why the
// chain is not split per project.
var ChainPartition = securityaudit.PartitionKeyFor(0)

// DefaultChainVerifyRows and MaxChainVerifyRows bound one verification
// request. A longer chain is verified in steps, each starting where the
// previous one stopped.
const (
	DefaultChainVerifyRows = 100_000
	MaxChainVerifyRows     = 1_000_000
	chainVerifyPage        = 1000
)

// ChainReport is the result of one verification.
type ChainReport struct {
	Partition string
	// From is the first partition_seq checked.
	From int64
	// Checked counts the rows that verified.
	Checked int64
	// HeadSeq is the chain head when the verification started.
	HeadSeq int64
	// PrunedThrough is the last partition_seq retention deleted (0: none).
	PrunedThrough int64
	// Truncated is true when the row limit stopped the walk before the head;
	// NextFrom is then where to continue.
	Truncated bool
	NextFrom  int64
	// Unchained counts rows written before the chain existed.
	Unchained int64
	// Break is the first place the chain does not verify; nil when it holds.
	// Break.Seq is the row id.
	Break *securityaudit.Break
}

// ChainDB is what VerifyChain reads through: a transaction, so every page
// and the head come from one snapshot.
type ChainDB interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ErrChainNotInstalled reports a database without migration 0206.
var ErrChainNotInstalled = errors.New("the audit_events chain is not installed")

const chainHeadSQL = `
SELECT last_seq, pruned_seq, pruned_hash
  FROM elitea_runtime.audit_events_chain_heads
 WHERE partition_key = $1`

const chainUnchainedSQL = `SELECT count(*) FROM centry.audit_events WHERE partition_seq IS NULL`

const chainAnchorSQL = `
SELECT hash FROM centry.audit_events WHERE partition_key = $1 AND partition_seq = $2`

const chainPageSQL = `
SELECT e.id, e.partition_key, e.partition_seq, centry.audit_event_canonical(e), e.prev_hash, e.hash
  FROM centry.audit_events e
 WHERE e.partition_key = $1 AND e.partition_seq >= $2
 ORDER BY e.partition_seq
 LIMIT $3`

// VerifyChain recomputes the chain from partition_seq from (0: from the
// prune anchor) over at most limit rows (0: DefaultChainVerifyRows) and
// reports the first break. Run it in a REPEATABLE READ transaction: a
// retention pass that commits between two pages would otherwise read as
// missing rows.
func VerifyChain(ctx context.Context, db ChainDB, from, limit int64) (ChainReport, error) {
	if db == nil {
		return ChainReport{}, errors.New("audit chain verify needs a database")
	}
	if from < 0 || limit < 0 || limit > MaxChainVerifyRows {
		return ChainReport{}, fmt.Errorf("from must be 0 or more and limit from 0 to %d", MaxChainVerifyRows)
	}
	if limit == 0 {
		limit = DefaultChainVerifyRows
	}
	report := ChainReport{Partition: ChainPartition}

	var installed bool
	if err := db.QueryRow(ctx, `SELECT to_regclass('elitea_runtime.audit_events_chain_heads') IS NOT NULL`).Scan(&installed); err != nil {
		return report, fmt.Errorf("check the audit chain: %w", err)
	}
	if !installed {
		return report, ErrChainNotInstalled
	}
	var prunedHash []byte
	err := db.QueryRow(ctx, chainHeadSQL, ChainPartition).Scan(&report.HeadSeq, &report.PrunedThrough, &prunedHash)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// No head: nothing was ever chained.
		prunedHash = securityaudit.ZeroHash[:]
	case err != nil:
		return report, fmt.Errorf("read the audit chain head: %w", err)
	}
	if err := db.QueryRow(ctx, chainUnchainedSQL).Scan(&report.Unchained); err != nil {
		return report, fmt.Errorf("count unchained audit events: %w", err)
	}

	// The anchor: the prune anchor when starting at it, else the hash of the
	// row before from.
	report.From = report.PrunedThrough + 1
	anchor, err := toHash(prunedHash)
	if err != nil {
		return report, fmt.Errorf("the audit chain prune anchor: %w", err)
	}
	if from > report.From {
		report.From = from
		var hash []byte
		err := db.QueryRow(ctx, chainAnchorSQL, ChainPartition, from-1).Scan(&hash)
		if errors.Is(err, pgx.ErrNoRows) {
			report.Break = &securityaudit.Break{PartitionSeq: from - 1, Reason: "row missing: the row before from is not stored"}
			return report, nil
		}
		if err != nil {
			return report, fmt.Errorf("read the anchor row: %w", err)
		}
		if anchor, err = toHash(hash); err != nil {
			report.Break = &securityaudit.Break{PartitionSeq: from - 1, Reason: "hash length is wrong"}
			return report, nil
		}
	}

	verifier := securityaudit.NewChainVerifierWith(ChainPartition, report.From, anchor,
		func(row securityaudit.StoredRow) ([]byte, error) { return row.Event, nil })
	next := report.From
	for report.Checked < limit {
		page := min(int64(chainVerifyPage), limit-report.Checked)
		rows, err := db.Query(ctx, chainPageSQL, ChainPartition, next, page)
		if err != nil {
			return report, fmt.Errorf("read audit events: %w", err)
		}
		n := int64(0)
		for rows.Next() {
			var (
				row       securityaudit.StoredRow
				canonical string
			)
			if err := rows.Scan(&row.Seq, &row.PartitionKey, &row.PartitionSeq, &canonical, &row.PrevHash, &row.Hash); err != nil {
				rows.Close()
				return report, fmt.Errorf("scan audit events: %w", err)
			}
			row.Event = []byte(canonical)
			n++
			if broken := verifier.Check(row); broken != nil {
				rows.Close()
				report.Checked = verifier.Checked()
				report.Break = broken
				return report, nil
			}
			next = row.PartitionSeq + 1
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return report, fmt.Errorf("read audit events: %w", err)
		}
		report.Checked = verifier.Checked()
		if n < page {
			// The walk ran out of rows. Rows missing after the last stored one
			// are invisible to it; the head says how far the chain must reach.
			if last := verifier.Next() - 1; last < report.HeadSeq {
				report.Break = &securityaudit.Break{PartitionSeq: verifier.Next(),
					Reason: fmt.Sprintf("row missing: the chain head is at partition_seq %d", report.HeadSeq)}
			}
			return report, nil
		}
	}
	if verifier.Next()-1 < report.HeadSeq {
		report.Truncated = true
		report.NextFrom = verifier.Next()
	}
	return report, nil
}

func toHash(raw []byte) (securityaudit.Hash, error) {
	var out securityaudit.Hash
	if len(raw) != len(out) {
		return out, fmt.Errorf("hash has length %d", len(raw))
	}
	copy(out[:], raw)
	return out, nil
}
