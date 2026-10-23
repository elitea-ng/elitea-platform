package main

import (
	"errors"
	"testing"
)

func TestDatabasePoolConfig_MaxConns(t *testing.T) {
	const dsn = "postgres://scheduler@db.invalid:5432/elitea"
	lookup := func(value string, set bool) func(string) (string, bool) {
		return func(key string) (string, bool) {
			if key != databaseMaxConnsEnv {
				return "", false
			}
			return value, set
		}
	}
	unset, err := databasePoolConfig(dsn, lookup("", false))
	if err != nil {
		t.Fatal(err)
	}
	set, err := databasePoolConfig(dsn, lookup("3", true))
	if err != nil {
		t.Fatal(err)
	}
	if set.MaxConns != 3 {
		t.Fatalf("MaxConns = %d, want 3", set.MaxConns)
	}
	if empty, err := databasePoolConfig(dsn, lookup("", true)); err != nil || empty.MaxConns != unset.MaxConns {
		t.Fatalf("an empty value must keep the pgx default %d: got %v, %v", unset.MaxConns, empty, err)
	}
	for _, bad := range []string{"0", "65", "-1", "03", "three"} {
		if _, err := databasePoolConfig(dsn, lookup(bad, true)); !errors.Is(err, errDatabaseMaxConns) {
			t.Errorf("%q: err = %v, want errDatabaseMaxConns", bad, err)
		}
	}
}
