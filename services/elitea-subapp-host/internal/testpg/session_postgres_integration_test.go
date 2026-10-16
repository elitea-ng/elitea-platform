package testpg

import (
	"context"
	"os"
	"os/exec"
	"strings"
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
		// libpq reads the same DSN, so a psql, psycopg or other libpq child
		// handed it runs in the same zone. libpq, unlike pgx, does not decode
		// '+' as a space in a URI query.
		psql, lookErr := exec.LookPath("psql")
		if lookErr != nil {
			t.Logf("%s DSN: psql is not on PATH, the libpq leg is not checked", name)
			continue
		}
		output, runErr := exec.Command(psql, dsn, "-Atc", "SHOW TimeZone").CombinedOutput()
		if runErr != nil {
			t.Fatalf("%s DSN: libpq refused it: %v: %s", name, runErr, output)
		}
		if got := strings.TrimSpace(string(output)); got != TimeZone() {
			t.Errorf("%s DSN through libpq: session TimeZone = %q, want %q", name, got, TimeZone())
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
