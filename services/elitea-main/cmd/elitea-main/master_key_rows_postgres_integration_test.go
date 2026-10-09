package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

// TestRefuseUnwrappedVaultKeys proves the start-up check that follows the
// master-key gate: with a key set, a stored project key that is still in the
// clear (32 raw or 44 encoded bytes) stops the start with the rewrap step
// named, while wrapped rows, an empty table and a database without the table
// all pass.
func TestRefuseUnwrappedVaultKeys(t *testing.T) {
	pool := newAdminUIEmailsPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := refuseUnwrappedVaultKeys(ctx, pool); err != nil {
		t.Fatalf("no centry.secrets_key table: error = %v, want nil", err)
	}
	if _, err := pool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS centry;
		CREATE TABLE centry.secrets_key (id text PRIMARY KEY, data bytea NOT NULL)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if err := refuseUnwrappedVaultKeys(ctx, pool); err != nil {
		t.Fatalf("empty table: error = %v, want nil", err)
	}
	// A Fernet token over a 44-byte key is 140 characters.
	wrapped := append([]byte("gAAAAA"), bytes.Repeat([]byte("x"), 134)...)
	if _, err := pool.Exec(ctx, `INSERT INTO centry.secrets_key VALUES ('wrapped', $1)`, wrapped); err != nil {
		t.Fatalf("insert wrapped: %v", err)
	}
	if err := refuseUnwrappedVaultKeys(ctx, pool); err != nil {
		t.Fatalf("wrapped rows only: error = %v, want nil", err)
	}

	for _, size := range []int{32, 44} {
		if _, err := pool.Exec(ctx, `INSERT INTO centry.secrets_key VALUES ('clear', $1)
			ON CONFLICT (id) DO UPDATE SET data = EXCLUDED.data`, bytes.Repeat([]byte("k"), size)); err != nil {
			t.Fatalf("insert unwrapped: %v", err)
		}
		err := refuseUnwrappedVaultKeys(ctx, pool)
		if err == nil {
			t.Fatalf("%d-byte unwrapped key: error = nil, want a refusal", size)
		}
		for _, want := range []string{"rewrap-centry-vault.py", "SECRETS_MASTER_KEY"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%d-byte unwrapped key: error %q does not name %q", size, err, want)
			}
		}
		if strings.Contains(err.Error(), "kkkk") {
			t.Errorf("error leaks stored key bytes: %q", err)
		}
	}
}
