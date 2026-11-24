package dbaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type stubRuleLoader struct {
	rules map[string]FrozenRule
	err   error
}

func (s stubRuleLoader) LoadFrozenRule(_ context.Context, _, _ int64, ruleName string) (FrozenRule, error) {
	if s.err != nil {
		return FrozenRule{}, s.err
	}
	rule, ok := s.rules[ruleName]
	if !ok {
		return FrozenRule{}, ErrPolicyDenied
	}
	return rule, nil
}

func TestFreezeSnapshotSortsDeduplicatesAndVerifies(t *testing.T) {
	t.Parallel()

	r1 := validReadRule()
	r2 := validWriteRule()
	loader := stubRuleLoader{rules: map[string]FrozenRule{
		r1.Name: {
			Name:             r1.Name,
			RuleID:           r1.ID,
			Revision:         r1.Revision,
			Dialect:          r1.Dialect,
			DatabaseName:     r1.DatabaseName,
			Mode:             r1.Mode,
			ConfigurationID:  r1.ConfigurationID,
			SQLConfiguration: map[string]any{"host": "db.example.com", "username": "etl", "password": "{{secret.PG_PASS}}"},
			PolicyDigest:     r1.PolicyDigest(),
		},
		r2.Name: {
			Name:             r2.Name,
			RuleID:           r2.ID,
			Revision:         r2.Revision,
			Dialect:          r2.Dialect,
			DatabaseName:     r2.DatabaseName,
			Mode:             r2.Mode,
			MarkerTable:      r2.MarkerTable,
			ConfigurationID:  r2.ConfigurationID,
			SQLConfiguration: map[string]any{"host": "mysql.example.com", "username": "etl", "password": "{{secret.MY_PASS}}"},
			PolicyDigest:     r2.PolicyDigest(),
		},
	}}

	// Empty rule list returns nil snapshot.
	empty, err := FreezeSnapshot(context.Background(), 1, 7, 31, 41, nil, loader)
	if err != nil || empty != nil {
		t.Fatalf("empty snapshot = %+v, %v", empty, err)
	}

	snap, err := FreezeSnapshot(context.Background(), 1, 7, 31, 41, []string{r1.Name, r2.Name, r1.Name}, loader)
	if err != nil {
		t.Fatalf("FreezeSnapshot: %v", err)
	}
	if len(snap.Rules) != 2 || snap.Rules[0].Name != r2.Name || snap.Rules[1].Name != r1.Name {
		t.Fatalf("rules not deduplicated and sorted: %+v", snap.Rules)
	}
	if err := snap.Verify(1, 31, 41); err != nil {
		t.Fatalf("Verify failed: %v", err)
	}
	// Wrong version or tampered revision fails closed.
	if err := snap.Verify(1, 31, 42); !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("Verify with wrong version = %v, want ErrPolicyDenied", err)
	}
	snap.Rules[0].Revision++
	if err := snap.Verify(1, 31, 41); !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("Verify after tamper = %v, want ErrPolicyDenied", err)
	}
}

func TestFreezeSnapshotRefusesOverLimit(t *testing.T) {
	t.Parallel()
	names := make([]string, MaxRulesPerProject+1)
	for i := range names {
		names[i] = fmt.Sprintf("rule_%03d", i)
	}
	if _, err := FreezeSnapshot(context.Background(), 1, 7, 31, 41, names, stubRuleLoader{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("over-limit snapshot err = %v, want ErrInvalid", err)
	}
}

func TestMaterializedSQLCredentialValidationAndRedaction(t *testing.T) {
	t.Parallel()

	const sentinel = "SUPER-SECRET-DB-PASS-98765"
	cred, err := ParseMaterializedSQLCredential(DialectPostgres, map[string]any{
		"host":     "pg.example.com",
		"port":     nil,
		"username": "warehouse_ro",
		"password": sentinel,
	})
	if err != nil {
		t.Fatalf("ParseMaterializedSQLCredential: %v", err)
	}
	if cred.Port != 5432 || cred.Host != "pg.example.com" || cred.Username != "warehouse_ro" || cred.Password() != sentinel {
		t.Fatalf("unexpected credential fields: port=%d host=%s", cred.Port, cred.Host)
	}
	for _, formatted := range []string{cred.String(), cred.GoString(), fmt.Sprintf("%v", cred), fmt.Sprintf("%+v", cred)} {
		if strings.Contains(formatted, sentinel) || strings.Contains(formatted, "pg.example.com") {
			t.Fatalf("formatted credential leaked sensitive data: %s", formatted)
		}
	}
	if _, err := json.Marshal(cred); err == nil {
		t.Fatal("MaterializedSQLCredential allowed JSON marshaling")
	}
	cred.Zero()
	if cred.Password() != "" {
		t.Fatal("Zero did not clear password")
	}

	// Unresolved secret placeholder, dialect mismatch, bad port, or bad host fail closed.
	for _, bad := range []map[string]any{
		{"host": "pg.example.com", "username": "u", "password": "{{secret.PG_PASS}}"},
		{"host": "pg.example.com", "username": "u", "password": "ok", "dialect": "mysql"},
		{"host": "pg.example.com", "port": 0, "username": "u", "password": "ok"},
		{"host": "pg.example.com", "port": 70000, "username": "u", "password": "ok"},
		{"host": "pg.example.com.", "username": "u", "password": "ok"},
		{"host": "bad host", "username": "u", "password": "ok"},
		{"host": "pg.example.com", "username": "u\n", "password": "ok"},
		{"host": "pg.example.com", "username": "u", "password": "a\x00b"},
	} {
		if _, err := ParseMaterializedSQLCredential(DialectPostgres, bad); !errors.Is(err, ErrPolicyDenied) {
			t.Errorf("bad config %v = %v, want ErrPolicyDenied", bad, err)
		}
	}
}
