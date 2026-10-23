// Package testpg puts every PostgreSQL connection a test opens into a
// session time zone that is NOT UTC.
//
// The legacy-era tables keep created_at/updated_at as `timestamp` WITHOUT
// time zone holding the UTC wall clock, beside timestamptz columns. Postgres
// converts between the two through the SESSION time zone, so a writer that
// stores a bare now() or a reader that compares a naive column with a
// timestamptz is right only while the session is UTC. CI's PostgreSQL images
// run in UTC, so without this every such defect passed CI and showed up only
// on a server in another zone. A zone west of UTC, with daylight saving,
// makes every such mix visible.
//
// Every test that reads a database location from the environment passes it
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
// but the ones it names. Such a read carries ApplyMarker on its line, and its
// function must call Apply on the RuntimeParams of the config it parses.
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

// NotADatabaseMarker, as a comment on the line of an environment read whose
// name is not a literal or a constant (a helper's parameter), tells the guard
// test that the read never yields a database location. The comment must say
// why: the guard cannot check it.
const NotADatabaseMarker = "testpg:not-a-database"

// DSN returns dsn with the test session time zone set as the libpq `options`
// startup parameter (`-c TimeZone=<zone>`), in the DSN's own form: a
// postgres:// URL or keyword/value pairs. pgx, libpq (psql) and psycopg all
// accept it, so a subprocess handed the DSN runs in the same zone. An empty
// dsn stays empty, so a test's "skip when unset" check is unchanged; a dsn
// that already names a time zone is returned as it is, and an existing
// `options` value keeps its own settings.
func DSN(dsn string) string {
	if strings.TrimSpace(dsn) == "" {
		return dsn
	}
	setting := "-c TimeZone=" + TimeZone()
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
		if existing := query.Get("options"); existing != "" {
			if namesTimeZone(existing) {
				return dsn
			}
			setting = existing + " " + setting
		}
		query.Set("options", setting)
		// libpq decodes %20 but not '+' in a URI query, so a space must
		// travel as %20 or psql and psycopg read "+TimeZone" as the name.
		parsed.RawQuery = strings.ReplaceAll(query.Encode(), "+", "%20")
		return parsed.String()
	}
	lower := strings.ToLower(dsn)
	if strings.Contains(lower, "timezone=") {
		return dsn
	}
	if index := strings.Index(lower, "options='"); index >= 0 {
		// Merge into the quoted value: a second options= would replace it.
		valueStart := index + len("options='")
		if valueEnd := strings.IndexByte(dsn[valueStart:], '\''); valueEnd >= 0 {
			at := valueStart + valueEnd
			return dsn[:at] + " " + setting + dsn[at:]
		}
	}
	return dsn + " options='" + strings.ReplaceAll(setting, "'", `\'`) + "'"
}

func namesTimeZone(options string) bool {
	return strings.Contains(strings.ToLower(options), "timezone")
}
