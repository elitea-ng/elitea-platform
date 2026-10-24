package db

import (
	"strings"
	"testing"
)

// readGatewaySQL returns the gateway schema migration SQL. It reads it through
// GatewayMigrationSQL — i.e. out of the LEDGERED corpus that elitea-migrate
// applies (issue #306) — so these shape assertions describe the file production
// runs, not a copy that only the dev bootstrap ever executed.
func readGatewaySQL(t *testing.T) string {
	t.Helper()
	sql, err := GatewayMigrationSQL()
	if err != nil {
		t.Fatalf("read gateway migration: %v", err)
	}
	return sql
}

// TestGatewayMigrationCutoverColumns asserts the cutover-critical columns the
// BF0.4 validator counts are all declared:
//
//	project_budget.nats_fail_mode, project_budget.soft_alert_pct,
//	llm_budget_accumulators.outage_mode, llm_budget_accumulators.reconciled,
//	llm_credentials.rate_policy, gateway_models.input_cost_per_1m_tokens
//
// The four audio price columns are named here for a second reason: they PIN the
// 0086 term of GatewayMigrationSQL's concatenation. Every other assertion in
// this file is satisfied by 0067 alone, so dropping the audio term from that
// concatenation used to leave the whole package green — and because
// TestGatewayMigrationIdempotent only sees the statements the concatenation
// returns, that same regression silently disarmed the idempotence guard over
// 0086 as well. gateway.llm_usage_events in TestGatewayMigrationTables pins the
// 0084 term the same way.
func TestGatewayMigrationCutoverColumns(t *testing.T) {
	sql := readGatewaySQL(t)
	for _, col := range []string{
		"nats_fail_mode",
		"soft_alert_pct",
		"outage_mode",
		"reconciled",
		"rate_policy",
		"input_cost_per_1m_tokens",
		"input_cost_per_1m_seconds",
		"output_cost_per_1m_seconds",
		"input_cost_per_1m_characters",
		"output_cost_per_1m_characters",
	} {
		if !strings.Contains(sql, col) {
			t.Errorf("gateway migration missing cutover-critical column %q", col)
		}
	}
}

// TestGatewayMigrationTables asserts each gateway table is created.
func TestGatewayMigrationTables(t *testing.T) {
	sql := readGatewaySQL(t)
	for _, tbl := range []string{
		"gateway.project_budget",
		"gateway.llm_budget_accumulators",
		"gateway.llm_credentials",
		"gateway.gateway_models",
		"gateway.processed_event_ids",
		// From 0084 — the per-request usage ledger the billing path writes.
		// It pins the middle term of GatewayMigrationSQL's concatenation.
		"gateway.llm_usage_events",
		// From 0174 — the metered tool price catalog. It pins the last term,
		// whose ledger columns the Usage and analytics reads select.
		"gateway.metered_operation_prices",
	} {
		if !strings.Contains(sql, "CREATE TABLE IF NOT EXISTS "+tbl) {
			t.Errorf("gateway migration missing idempotent create for %q", tbl)
		}
	}
}

// migrationStatements normalises SQL into whole statements: it drops line
// comments, splits on the semicolons that end a statement, and collapses each
// run of whitespace to one space.
//
// The scan below used to read the corpus LINE BY LINE, and a statement written
// across two lines escaped it completely. 0067 writes its own ALTER that way:
//
//	ALTER TABLE gateway.gateway_models
//	    ADD COLUMN IF NOT EXISTS input_cost_per_1m_tokens NUMERIC(20,8);
//
// The first line has the ALTER TABLE prefix and no ADD COLUMN, the second has
// ADD COLUMN and no prefix, so neither line matched and the guard reported a
// pass after it inspected nothing. On whole statements the wrapped form and the
// one-line form are the same text.
//
// A $$-quoted body is NOT split on its inner semicolons. A DO block is one unit
// because the existence check at its top is what makes the statements inside it
// idempotent; split apart, those statements would be judged without their
// guard.
//
// The comment strip is deliberately simple — it cuts at the first "--" on the
// line. No string literal in this corpus holds "--"; one that did would need a
// real lexer here.
func migrationStatements(sql string) []string {
	var (
		stmts   []string
		cur     strings.Builder
		inBlock bool // inside a $$-quoted body
	)
	flush := func() {
		if s := strings.Join(strings.Fields(cur.String()), " "); s != "" {
			stmts = append(stmts, s)
		}
		cur.Reset()
	}
	for _, line := range strings.Split(sql, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		for line != "" {
			dollar := strings.Index(line, "$$")
			semi := strings.Index(line, ";")
			switch {
			case !inBlock && semi >= 0 && (dollar < 0 || semi < dollar):
				cur.WriteString(line[:semi])
				flush()
				line = line[semi+1:]
			case dollar >= 0 && (inBlock || semi < 0 || dollar < semi):
				cur.WriteString(line[:dollar+2])
				inBlock = !inBlock
				line = line[dollar+2:]
			default:
				cur.WriteString(line)
				line = ""
			}
		}
		// The newline became a statement-internal separator.
		cur.WriteString(" ")
	}
	flush()
	return stmts
}

// nonIdempotentStatements returns one message per DDL statement that a
// re-application would fail on. It is separate from the test that runs it over
// the real corpus so that TestIdempotenceScanCatchesWrappedALTER can prove the
// scan discriminates — a guard that has never been seen to fail is not evidence
// that the corpus is guarded.
func nonIdempotentStatements(sql string) []string {
	var found []string
	for _, stmt := range migrationStatements(sql) {
		up := strings.ToUpper(stmt)
		switch {
		case strings.HasPrefix(up, "CREATE TABLE ") && !strings.HasPrefix(up, "CREATE TABLE IF NOT EXISTS "):
			found = append(found, "non-idempotent CREATE TABLE: "+shorten(stmt))
		case strings.HasPrefix(up, "CREATE SCHEMA ") && !strings.HasPrefix(up, "CREATE SCHEMA IF NOT EXISTS "):
			found = append(found, "non-idempotent CREATE SCHEMA: "+shorten(stmt))
		case strings.HasPrefix(up, "CREATE INDEX ") && !strings.HasPrefix(up, "CREATE INDEX IF NOT EXISTS "):
			found = append(found, "non-idempotent CREATE INDEX: "+shorten(stmt))
		// Counted, not Contains: a multi-action ALTER must guard EVERY one of
		// its ADD COLUMN clauses, and one guarded clause would otherwise excuse
		// the rest of them.
		case strings.HasPrefix(up, "ALTER TABLE ") &&
			strings.Count(up, "ADD COLUMN ") != strings.Count(up, "ADD COLUMN IF NOT EXISTS "):
			found = append(found, "non-idempotent ADD COLUMN: "+shorten(stmt))
		// A DO block may add a column without IF NOT EXISTS, but only behind its
		// own existence check (0067 adds project_budget.enabled that way, so it
		// can back-fill the column in the same block).
		case strings.HasPrefix(up, "DO ") && strings.Contains(up, "ADD COLUMN") &&
			!strings.Contains(up, "ADD COLUMN IF NOT EXISTS") &&
			!strings.Contains(up, "INFORMATION_SCHEMA.COLUMNS"):
			found = append(found, "unguarded ADD COLUMN in DO block: "+shorten(stmt))
		}
	}
	return found
}

// shorten keeps a failure message readable: a normalised CREATE TABLE is one
// very long line.
func shorten(stmt string) string {
	const max = 120
	if len(stmt) <= max {
		return stmt
	}
	return stmt[:max] + "…"
}

// TestGatewayMigrationIdempotent asserts every DDL statement is guarded so the
// dump-guard-exempt migration set can be re-applied safely.
func TestGatewayMigrationIdempotent(t *testing.T) {
	sql := readGatewaySQL(t)

	// The scan must have SEEN 0067's wrapped ALTER, not skipped it. This is the
	// assertion the line-by-line version could not make: it read that statement
	// as two halves that each failed one half of the match, so it passed by
	// looking at nothing.
	const wrapped = "ALTER TABLE gateway.gateway_models ADD COLUMN IF NOT EXISTS input_cost_per_1m_tokens NUMERIC(20,8)"
	var sawWrapped bool
	for _, stmt := range migrationStatements(sql) {
		if stmt == wrapped {
			sawWrapped = true
			break
		}
	}
	if !sawWrapped {
		t.Errorf("normaliser did not produce 0067's wrapped ALTER as one statement; the guard is inspecting something else")
	}

	for _, msg := range nonIdempotentStatements(sql) {
		t.Error(msg)
	}
}

// TestIdempotenceScanCatchesWrappedALTER proves the scan sees a wrapped
// statement exactly as it sees a single-line one, in BOTH directions: it flags
// the unguarded wrapped ALTER and stays quiet on the guarded one. The guard it
// replaces reported a pass on both.
func TestIdempotenceScanCatchesWrappedALTER(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sql     string
		wantHit bool
	}{
		{
			name:    "wrapped unguarded ADD COLUMN",
			sql:     "ALTER TABLE gateway.gateway_models\n    ADD COLUMN price NUMERIC(20,8);\n",
			wantHit: true,
		},
		{
			name:    "wrapped guarded ADD COLUMN",
			sql:     "ALTER TABLE gateway.gateway_models\n    ADD COLUMN IF NOT EXISTS price NUMERIC(20,8);\n",
			wantHit: false,
		},
		{
			name:    "single-line unguarded ADD COLUMN",
			sql:     "ALTER TABLE gateway.gateway_models ADD COLUMN price NUMERIC(20,8);\n",
			wantHit: true,
		},
		{
			name:    "a comment that quotes the statement is not a statement",
			sql:     "-- ALTER TABLE gateway.gateway_models ADD COLUMN price NUMERIC(20,8);\n",
			wantHit: false,
		},
		{
			name:    "second clause of a multi-action ALTER is unguarded",
			sql:     "ALTER TABLE t ADD COLUMN IF NOT EXISTS a INT,\n    ADD COLUMN b INT;\n",
			wantHit: true,
		},
		{
			name:    "wrapped bare CREATE TABLE",
			sql:     "CREATE TABLE\n    gateway.t (id UUID);\n",
			wantHit: true,
		},
		{
			name: "DO block guarded by an existence check",
			sql: "DO $$\nBEGIN\n    IF NOT EXISTS (SELECT 1 FROM information_schema.columns" +
				" WHERE column_name = 'enabled') THEN\n        ALTER TABLE t\n" +
				"            ADD COLUMN enabled BOOLEAN;\n    END IF;\nEND\n$$;\n",
			wantHit: false,
		},
		{
			name: "DO block with no existence check",
			sql: "DO $$\nBEGIN\n    ALTER TABLE t\n        ADD COLUMN enabled BOOLEAN;\n" +
				"END\n$$;\n",
			wantHit: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := nonIdempotentStatements(tc.sql)
			if tc.wantHit && len(got) == 0 {
				t.Errorf("scan missed a non-idempotent statement in:\n%s", tc.sql)
			}
			if !tc.wantHit && len(got) != 0 {
				t.Errorf("scan flagged a guarded statement %v in:\n%s", got, tc.sql)
			}
		})
	}
}

// TestGatewayMigrationPricePer1M guards the denomination invariant: the price
// column MUST be per-1M-token, never per-1k or per-token (§7.1 1000× bug).
func TestGatewayMigrationPricePer1M(t *testing.T) {
	sql := readGatewaySQL(t)
	if !strings.Contains(sql, "input_cost_per_1m_tokens") {
		t.Fatal("gateway_models must use the per-1M-token denomination")
	}
	if strings.Contains(sql, "input_cost_per_1k_tokens") || strings.Contains(sql, "input_cost_per_token ") {
		t.Error("gateway_models must NOT use a per-1k or per-token denomination (1000× costing bug)")
	}
}

// TestGatewayAudioPricePer1M guards the same denomination invariant for the
// audio columns. A per-second or per-character column name would mean the
// gateway stores the price of ONE unit where it multiplies by a per-1,000,000
// rate, which under-bills audio by 1,000,000×.
func TestGatewayAudioPricePer1M(t *testing.T) {
	sql := readGatewaySQL(t)
	for _, bad := range []string{
		"input_cost_per_second",
		"output_cost_per_second",
		"input_cost_per_character",
		"output_cost_per_character",
	} {
		if strings.Contains(sql, bad+" NUMERIC") {
			t.Errorf("gateway_models declares %q: audio prices are per 1,000,000 units", bad)
		}
	}
}

// TestRatePolicyCheckConstraint asserts the CHECK constraint enumerates exactly
// the three policies from §8.7.
func TestRatePolicyCheckConstraint(t *testing.T) {
	sql := readGatewaySQL(t)
	for _, p := range []string{"'billed'", "'zero-rate-metered'", "'excluded'"} {
		if !strings.Contains(sql, p) {
			t.Errorf("rate_policy CHECK missing %s", p)
		}
	}
}

// TestNatsFailModeCheckConstraint asserts the fail-mode enumeration (§8.5).
func TestNatsFailModeCheckConstraint(t *testing.T) {
	sql := readGatewaySQL(t)
	for _, m := range []string{"'tiered_hybrid'", "'fail_open'", "'fail_closed'"} {
		if !strings.Contains(sql, m) {
			t.Errorf("nats_fail_mode CHECK missing %s", m)
		}
	}
}
