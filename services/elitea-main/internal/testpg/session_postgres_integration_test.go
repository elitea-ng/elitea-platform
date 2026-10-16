package testpg

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestDSNReachesTheSession connects through DSN in both of its forms and
// reads the session TimeZone back, so a driver change that stopped passing
// the `timezone` startup parameter cannot switch the guard off unnoticed.
func TestDSNReachesTheSession(t *testing.T) {
	raw := os.Getenv("ELITEA_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("set ELITEA_TEST_DATABASE_URL to check the session time zone end to end")
	}
	parsed, err := pgx.ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	keyword := "host=" + parsed.Host + " port=" + itoa(int(parsed.Port)) + " user=" + parsed.User +
		" dbname=" + parsed.Database + " sslmode=disable"
	if parsed.Password != "" {
		keyword += " password=" + parsed.Password
	}
	for name, dsn := range map[string]string{"url": DSN(raw), "keyword": DSN(keyword)} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			cancel()
			t.Fatalf("%s: connect: %v", name, err)
		}
		var zone string
		err = conn.QueryRow(ctx, "SELECT current_setting('TimeZone')").Scan(&zone)
		_ = conn.Close(ctx)
		cancel()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if zone != TimeZone() {
			t.Errorf("%s DSN: session TimeZone = %q, want %q", name, zone, TimeZone())
		}
	}
}

// Apply is what a guarded fixture uses instead of DSN.
func TestApplySetsTheRuntimeParameterOnce(t *testing.T) {
	t.Setenv(TimeZoneEnv, "")
	params := map[string]string{}
	Apply(params)
	if params["timezone"] != DefaultTimeZone {
		t.Fatalf("Apply set %v", params)
	}
	kept := map[string]string{"TimeZone": "UTC"}
	Apply(kept)
	if len(kept) != 1 || kept["TimeZone"] != "UTC" {
		t.Fatalf("Apply overrode an explicit zone: %v", kept)
	}
}
