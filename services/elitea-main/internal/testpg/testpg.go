// Package testpg puts every PostgreSQL connection a test opens into a
// session time zone that is NOT UTC.
//
// The pylon-era tables keep created_at/updated_at as `timestamp` WITHOUT
// time zone holding the UTC wall clock, beside timestamptz columns. Postgres
// converts between the two through the SESSION time zone, so a writer that
// stores a bare now() or a reader that compares a naive column with a
// timestamptz is right only while the session is UTC. CI's PostgreSQL images
// run in UTC, so without this every such defect passed CI and showed up only
// on a server in another zone. A zone west of UTC, with daylight saving,
// makes every such mix visible.
//
// Every test that reads a database URL from the environment passes it
// through DSN. TestEveryDatabaseURLReadGoesThroughTestpg (in this package)
// fails a test file that reads one without it.
package testpg

import (
	"net/url"
	"os"
	"strings"
)

// TimeZoneEnv overrides the session time zone every test connection uses.
const TimeZoneEnv = "ELITEA_TEST_SESSION_TIMEZONE"

// DefaultTimeZone is the session time zone a test connection runs in unless
// TimeZoneEnv names another. It is deliberately not UTC.
const DefaultTimeZone = "America/Toronto"

// TimeZone is the session time zone test connections run in.
func TimeZone() string {
	if zone := strings.TrimSpace(os.Getenv(TimeZoneEnv)); zone != "" {
		return zone
	}
	return DefaultTimeZone
}

// Apply sets the test session time zone in a parsed connection's runtime
// parameters (pgconn.Config.RuntimeParams). It is for the one kind of caller
// that cannot take DSN: a fixture whose own guard refuses any query parameter
// but the ones it names. Such a read is marked with ApplyMarker on its line.
func Apply(runtimeParams map[string]string) {
	for key := range runtimeParams {
		if strings.EqualFold(key, "timezone") {
			return
		}
	}
	runtimeParams["timezone"] = TimeZone()
}

// ApplyMarker, as a comment on the line of an environment read, tells the
// guard test that the connection gets its time zone from Apply instead.
const ApplyMarker = "testpg:apply"

// DSN returns dsn with the test session time zone set, in the DSN's own form
// (a postgres:// URL or keyword/value pairs), so pgx and libpq both apply it
// as the `timezone` startup parameter. An empty dsn stays empty, so a test's
// "skip when unset" check is unchanged, and a dsn that already names a time
// zone is returned as it is.
func DSN(dsn string) string {
	if strings.TrimSpace(dsn) == "" {
		return dsn
	}
	zone := TimeZone()
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		parsed, err := url.Parse(dsn)
		if err != nil {
			return dsn // the caller's own parse reports it
		}
		query := parsed.Query()
		for key := range query {
			if strings.EqualFold(key, "timezone") {
				return dsn
			}
		}
		query.Set("timezone", zone)
		parsed.RawQuery = query.Encode()
		return parsed.String()
	}
	if strings.Contains(strings.ToLower(dsn), "timezone=") {
		return dsn
	}
	return dsn + " timezone='" + strings.ReplaceAll(zone, "'", `\'`) + "'"
}
