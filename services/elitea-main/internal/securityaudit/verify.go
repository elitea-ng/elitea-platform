package securityaudit

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Source selects the table the verifier reads.
type Source string

const (
	// SourceEvents is the durable store (default).
	SourceEvents Source = "events"
	// SourceOutbox is the outbox. Retention deletes its old exported rows,
	// so its chain may start after partition_seq 1.
	SourceOutbox Source = "outbox"
)

func (s Source) table() (string, error) {
	switch s {
	case SourceEvents:
		return "elitea_identity.security_audit_events", nil
	case SourceOutbox:
		return "elitea_runtime.security_audit_outbox", nil
	}
	return "", fmt.Errorf("unknown source %q: want %q or %q", s, SourceEvents, SourceOutbox)
}

// VerifyReport is the verifier's result. Break is nil when the chain holds.
type VerifyReport struct {
	PartitionKey string
	From         int64
	// Anchored is false when the first row's prev_hash could not be checked
	// against an earlier row (an outbox whose older rows retention removed).
	Anchored bool
	Checked  int64
	Break    *Break
}

const verifyPageSize = 1000

// Verify recomputes the chain of one partition from partition sequence from
// (1 or more) and reports the first break.
func Verify(ctx context.Context, db Querier, source Source, tenantProjectID, from int64) (VerifyReport, error) {
	if db == nil {
		return VerifyReport{}, errors.New("security audit verify needs a database")
	}
	table, err := source.table()
	if err != nil {
		return VerifyReport{}, err
	}
	if tenantProjectID < 0 || from < 1 {
		return VerifyReport{}, errors.New("tenant must be 0 or more and from must be 1 or more")
	}
	pager, ok := db.(rowQuerier)
	if !ok {
		return VerifyReport{}, errors.New("security audit verify needs a database that can query rows")
	}
	partition := PartitionKeyFor(tenantProjectID)
	report := VerifyReport{PartitionKey: partition, From: from, Anchored: true}

	anchor := ZeroHash
	if from > 1 || source == SourceOutbox {
		anchor, report.Anchored, report.From, err = findAnchor(ctx, db, table, partition, from)
		if err != nil {
			return report, err
		}
	}
	verifier := NewChainVerifier(partition, report.From, anchor)
	selectPage := `SELECT seq, partition_key, partition_seq, event_id::text, event::text, prev_hash, hash
  FROM ` + table + `
 WHERE partition_key = $1 AND partition_seq >= $2
 ORDER BY partition_seq
 LIMIT $3`
	next := report.From
	for {
		rows, err := pager.Query(ctx, selectPage, partition, next, verifyPageSize)
		if err != nil {
			return report, fmt.Errorf("read %s: %w", table, err)
		}
		n := 0
		var broken *Break
		for rows.Next() {
			var (
				row   StoredRow
				event string
			)
			if err := rows.Scan(&row.Seq, &row.PartitionKey, &row.PartitionSeq, &row.EventID, &event, &row.PrevHash, &row.Hash); err != nil {
				rows.Close()
				return report, fmt.Errorf("scan %s: %w", table, err)
			}
			row.Event = []byte(event)
			n++
			if broken = verifier.Check(row); broken != nil {
				break
			}
			next = row.PartitionSeq + 1
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return report, fmt.Errorf("read %s: %w", table, err)
		}
		report.Checked = verifier.Checked()
		if broken != nil {
			report.Break = broken
			return report, nil
		}
		if n < verifyPageSize {
			if source == SourceEvents {
				report.Break, err = checkTail(ctx, db, partition, verifier)
			}
			return report, err
		}
	}
}

// expectedStoredEndSQL is the last partition sequence the durable store must
// hold: the head, or the row before the oldest outbox row not yet stored.
const expectedStoredEndSQL = `
SELECT COALESCE(
    (SELECT min(partition_seq) - 1 FROM elitea_runtime.security_audit_outbox
      WHERE partition_key = $1 AND stored_at IS NULL),
    (SELECT last_seq FROM elitea_runtime.security_audit_heads WHERE partition_key = $1),
    0)`

// checkTail reports rows missing after the last stored one. The chain walk
// alone cannot see a deleted tail.
func checkTail(ctx context.Context, db Querier, partition string, verifier *ChainVerifier) (*Break, error) {
	var end int64
	if err := db.QueryRow(ctx, expectedStoredEndSQL, partition).Scan(&end); err != nil {
		return nil, fmt.Errorf("read the partition head: %w", err)
	}
	if last := verifier.Next() - 1; last < end {
		return &Break{PartitionSeq: verifier.Next(), Reason: fmt.Sprintf("row missing: the chain head is at partition_seq %d", end)}, nil
	}
	return nil, nil
}

type rowQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// findAnchor returns the prev hash the chain at from must start with.
//
// The row before from anchors it. Without that row: from 1 anchors on
// ZeroHash; the outbox (whose older rows retention deletes) starts at its
// first row at or after from, unanchored. In the durable store a missing
// earlier row is itself a break, which the chain walk reports.
func findAnchor(ctx context.Context, db Querier, table, partition string, from int64) (Hash, bool, int64, error) {
	if from > 1 {
		var hash []byte
		err := db.QueryRow(ctx, `SELECT hash FROM `+table+` WHERE partition_key = $1 AND partition_seq = $2`, partition, from-1).Scan(&hash)
		switch {
		case err == nil:
			if len(hash) != len(Hash{}) {
				return Hash{}, false, from, fmt.Errorf("anchor row %d has a hash of the wrong length", from-1)
			}
			var anchor Hash
			copy(anchor[:], hash)
			return anchor, true, from, nil
		case !errors.Is(err, pgx.ErrNoRows):
			return Hash{}, false, from, fmt.Errorf("read anchor row: %w", err)
		}
	}
	if table != "elitea_runtime.security_audit_outbox" {
		// No earlier row: anchor on zero; the walk reports the gap if from > 1.
		return ZeroHash, from == 1, from, nil
	}
	var (
		first    int64
		prevHash []byte
	)
	err := db.QueryRow(ctx, `SELECT partition_seq, prev_hash FROM `+table+`
 WHERE partition_key = $1 AND partition_seq >= $2 ORDER BY partition_seq LIMIT 1`, partition, from).Scan(&first, &prevHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return ZeroHash, true, from, nil
	}
	if err != nil {
		return Hash{}, false, from, fmt.Errorf("read first outbox row: %w", err)
	}
	if first == 1 {
		return ZeroHash, true, 1, nil
	}
	if len(prevHash) != len(Hash{}) {
		return Hash{}, false, first, fmt.Errorf("outbox row %d has a hash of the wrong length", first)
	}
	var anchor Hash
	copy(anchor[:], prevHash)
	return anchor, false, first, nil
}

// Verify CLI exit codes.
const (
	VerifyExitOK           = 0
	VerifyExitFailure      = 1
	VerifyExitInvalidUsage = 2
	VerifyExitBroken       = 3
)

// VerifyCommand is the elitea-main subcommand name: "security-audit verify".
const VerifyCommand = "security-audit verify"

const verifyRunTimeout = 30 * time.Minute

// Connect opens the database the CLI reads. Tests replace it.
type Connect func(ctx context.Context, databaseURL string) (VerifyDB, error)

// VerifyDB is the connection the CLI uses.
type VerifyDB interface {
	Querier
	rowQuerier
	Close(ctx context.Context) error
}

func connectPGX(ctx context.Context, databaseURL string) (VerifyDB, error) {
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// RunVerify is `elitea-main security-audit verify --tenant <id> [--from N]
// [--source events|outbox]`. args are the arguments after "verify". It
// reads DATABASE_URL through lookup. Exit codes: 0 the chain holds, 3 it is
// broken (the first break is printed), 2 invalid usage, 1 any other failure.
func RunVerify(ctx context.Context, args []string, lookup func(string) (string, bool), stdout, stderr io.Writer) int {
	return runVerify(ctx, args, lookup, stdout, stderr, connectPGX)
}

func runVerify(ctx context.Context, args []string, lookup func(string) (string, bool), stdout, stderr io.Writer, connect Connect) int {
	flags := flag.NewFlagSet(VerifyCommand, flag.ContinueOnError)
	flags.SetOutput(stderr)
	tenant := flags.Int64("tenant", -1, "the tenant project id whose chain to verify (0 is the platform partition)")
	from := flags.Int64("from", 1, "the first partition sequence to verify")
	source := flags.String("source", string(SourceEvents), "the table to verify: events (the durable store) or outbox")
	flags.Usage = func() {
		say(stderr, "usage: elitea-main %s --tenant <id> [--from N] [--source events|outbox]\n", VerifyCommand)
		say(stderr, "reads DATABASE_URL (the platform database)\n\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return VerifyExitInvalidUsage
	}
	if *tenant < 0 {
		say(stderr, "--tenant is required and must be 0 or more\n")
		return VerifyExitInvalidUsage
	}
	if *from < 1 {
		say(stderr, "--from must be 1 or more\n")
		return VerifyExitInvalidUsage
	}
	if _, err := Source(*source).table(); err != nil {
		say(stderr, "--source: %v\n", err)
		return VerifyExitInvalidUsage
	}
	if lookup == nil {
		say(stderr, "DATABASE_URL is required\n")
		return VerifyExitInvalidUsage
	}
	databaseURL, _ := lookup("DATABASE_URL")
	if strings.TrimSpace(databaseURL) == "" {
		say(stderr, "DATABASE_URL is required\n")
		return VerifyExitInvalidUsage
	}

	ctx, cancel := context.WithTimeout(ctx, verifyRunTimeout)
	defer cancel()
	db, err := connect(ctx, databaseURL)
	if err != nil {
		// The driver error can quote the DSN; say what failed, not with what.
		say(stderr, "connect to the platform database (DATABASE_URL) failed\n")
		return VerifyExitFailure
	}
	defer func() { _ = db.Close(context.WithoutCancel(ctx)) }()

	report, err := Verify(ctx, db, Source(*source), *tenant, *from)
	if err != nil {
		say(stderr, "verify: %v\n", err)
		return VerifyExitFailure
	}
	say(stdout, "partition %s (%s): checked %d event(s) from partition_seq %d\n",
		report.PartitionKey, *source, report.Checked, report.From)
	if !report.Anchored {
		say(stdout, "note: partition_seq %d has no earlier row here; its prev_hash is taken as given\n", report.From)
	}
	if report.Break != nil {
		say(stdout, "BROKEN at partition_seq %d (seq %d): %s\n", report.Break.PartitionSeq, report.Break.Seq, report.Break.Reason)
		return VerifyExitBroken
	}
	say(stdout, "chain OK\n")
	return VerifyExitOK
}

func say(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}
