package system_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// provisionPreflightDatabase creates a uniquely named database on the server
// adminDSN names, drops it when the test ends, and returns its DSN: the admin
// credentials, the new database.
func provisionPreflightDatabase(t *testing.T, ctx context.Context, adminDSN string) string {
	t.Helper()
	parsed, err := url.Parse(adminDSN)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		t.Fatalf("%s must be a postgres:// URL (the database name is replaced in it): %v", testDatabaseURLEnv, err)
	}
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	name := "preflight_" + hex.EncodeToString(suffix)
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect %s: %v", testDatabaseURLEnv, err)
	}
	defer admin.Close(ctx)
	// The name is generated above, never input: no injection surface.
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create the preflight database: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(cleanupCtx, adminDSN)
		if err != nil {
			t.Logf("drop the preflight database %s: %v", name, err)
			return
		}
		defer conn.Close(cleanupCtx)
		if _, err := conn.Exec(cleanupCtx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("drop the preflight database %s: %v", name, err)
		}
	})
	parsed.Path = "/" + name
	return parsed.String()
}
