package main

import (
	"errors"
	"testing"
)

func TestDatabasePoolConfig_MaxConns(t *testing.T) {
	const dsn = "postgres://gateway@db.invalid:5432/elitea"
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
	set, err := databasePoolConfig(dsn, lookup("7", true))
	if err != nil {
		t.Fatal(err)
	}
	if set.MaxConns != 7 {
		t.Fatalf("MaxConns = %d, want 7", set.MaxConns)
	}
	if empty, err := databasePoolConfig(dsn, lookup("", true)); err != nil || empty.MaxConns != unset.MaxConns {
		t.Fatalf("an empty value must keep the pgx default %d: got %v, %v", unset.MaxConns, empty, err)
	}
	for _, bad := range []string{"0", "65", "-1", "07", "+7", "seven", " 7"} {
		if _, err := databasePoolConfig(dsn, lookup(bad, true)); !errors.Is(err, errDatabaseMaxConns) {
			t.Errorf("%q: err = %v, want errDatabaseMaxConns", bad, err)
		}
	}
	// A DSN the pool cannot parse is a different error: main keeps its
	// non-fatal path for it.
	if _, err := databasePoolConfig("postgres://%zz", lookup("7", true)); err == nil || errors.Is(err, errDatabaseMaxConns) {
		t.Fatalf("an unparsable DSN: err = %v", err)
	}
}
