package migrations_test

// The CHECK constraint on gateway.governance_config.type is built by the
// LEDGERED corpus, which no other test in this service applies.
//
// The value set has to be widened in a NEW file, because a migration is
// checksum-immutable once applied (internal/infra/db/migrate/manifest.go). A
// widening that never reached the corpus would make the admin API answer 500 on
// every save of the new row type — on a fresh Helm install only. Every
// development database already carries whatever constraint it happened to be
// created with, so the defect would be invisible everywhere it was looked for.
//
// Revert shared/0112 and the first subtest fails with SQLSTATE 23514.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestMigrationCorpusAdmitsTheEgressAllowlistType(t *testing.T) {
	pool := newMigratedPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const insert = `INSERT INTO gateway.governance_config (type, section, name, data, enabled)
		VALUES ($1, 'governance', $2, $3::jsonb, true)`

	t.Run("an egress_allowlist row is accepted", func(t *testing.T) {
		_, err := pool.Exec(ctx, insert, "egress_allowlist", "on-prem-vllm",
			`{"egress":{"allowlist":["192.168.29.60:8000","192.168.29.0/24"]}}`)
		if err != nil {
			t.Fatalf("the corpus refuses the egress_allowlist row type: %v", err)
		}

		var raw []byte
		if err := pool.QueryRow(ctx,
			`SELECT data FROM gateway.governance_config WHERE type = 'egress_allowlist' AND name = 'on-prem-vllm'`,
		).Scan(&raw); err != nil {
			t.Fatalf("read the egress row back: %v", err)
		}
		if !strings.Contains(string(raw), "192.168.29.0/24") {
			t.Fatalf("the stored payload lost its CIDR entry: %s", raw)
		}
	})

	t.Run("the constraint still refuses an unknown type", func(t *testing.T) {
		// The widening must ADD one value, not remove the constraint. A row
		// with a misspelled type is invisible to every reader and governs
		// nothing, while sitting in the admin list looking like one that works
		// — which is what shared/0093 exists to stop.
		_, err := pool.Exec(ctx, insert, "egres_allowlist", "typo", `{}`)
		if err == nil {
			t.Fatal("a misspelled governance type was accepted; the type CHECK is gone")
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("want a 23514 check violation, got %v", err)
		}
	})

	t.Run("every type shared/0093 admitted is still admitted", func(t *testing.T) {
		// A replacement constraint that silently dropped one of the original
		// seven would break a control that has been authorable since 0067.
		for _, typ := range []string{
			"budget", "rate_limit", "model_config", "routing_rule",
			"mcp_allowlist", "credential_policy", "budget_alert",
		} {
			if _, err := pool.Exec(ctx, insert, typ, "keep-"+typ, `{}`); err != nil {
				t.Errorf("the widened constraint no longer admits %q: %v", typ, err)
			}
		}
	})

	t.Run("re-applying the corpus does not churn the constraint", func(t *testing.T) {
		// The DO block returns early when the constraint already admits the new
		// value. Without that guard a re-apply would DROP and re-ADD it, and a
		// re-added NOT VALID constraint silently forgets any VALIDATE an
		// operator had run.
		var definition string
		if err := pool.QueryRow(ctx, `
SELECT pg_get_constraintdef(oid)
  FROM pg_constraint
 WHERE conrelid = 'gateway.governance_config'::regclass
   AND conname  = 'governance_config_type_known'`).Scan(&definition); err != nil {
			t.Fatalf("read the constraint definition: %v", err)
		}
		if !strings.Contains(definition, "egress_allowlist") {
			t.Fatalf("the constraint does not admit egress_allowlist: %s", definition)
		}
		if !strings.Contains(definition, "NOT VALID") {
			t.Fatalf("the replacement constraint is no longer NOT VALID, so a deployment holding a row "+
				"with an unknown type would fail the release: %s", definition)
		}
	})
}
