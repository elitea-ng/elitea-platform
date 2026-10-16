//! Test `PostgreSQL` sessions run in a non-UTC time zone.
//!
//! The pylon-era tables keep `created_at`/`updated_at` as `timestamp`
//! WITHOUT time zone holding the UTC wall clock, beside timestamptz columns.
//! Postgres converts between the two through the SESSION time zone, so a
//! defect that mixes them is visible only off UTC; CI's `PostgreSQL` runs in
//! UTC. This is the Rust twin of elitea-main's `internal/testpg`: every test
//! that reads a database URL from the environment calls [`database_url_var`]
//! instead of `std::env::var`, which adds the libpq `options` startup
//! parameter `-c TimeZone=<zone>` (America/Toronto, or
//! `ELITEA_TEST_SESSION_TIMEZONE`). sqlx, libpq and pgx all accept it.
//! `test_pg_guard_every_database_url_read_goes_through_test_pg` fails a test
//! file that reads one directly.

use std::env::VarError;

/// Overrides the session time zone every test connection uses.
pub(crate) const TIME_ZONE_ENV: &str = "ELITEA_TEST_SESSION_TIMEZONE";

/// The session time zone a test connection runs in unless [`TIME_ZONE_ENV`]
/// names another. Deliberately not UTC.
pub(crate) const DEFAULT_TIME_ZONE: &str = "America/Toronto";

/// The session time zone test connections run in.
pub(crate) fn time_zone() -> String {
    match std::env::var(TIME_ZONE_ENV) {
        Ok(zone) if !zone.trim().is_empty() => zone.trim().to_owned(),
        _ => DEFAULT_TIME_ZONE.to_owned(),
    }
}

/// `std::env::var(name)` for a database URL, with the test session time zone
/// applied. An unset or empty variable is returned as it is, so a test's
/// "skip when unset" check is unchanged.
pub(crate) fn database_url_var(name: &str) -> Result<String, VarError> {
    let raw = std::env::var(name)?;
    Ok(with_session_time_zone(&raw, &time_zone()))
}

/// Adds `options=-c TimeZone=<zone>` to a postgres:// URL, or to a
/// keyword/value DSN, unless it already names a time zone. A space travels
/// as `%20`: libpq does not decode `+` in a URI query.
pub(crate) fn with_session_time_zone(dsn: &str, zone: &str) -> String {
    if dsn.trim().is_empty() || dsn.to_ascii_lowercase().contains("timezone") {
        return dsn.to_owned();
    }
    let setting = format!("-c TimeZone={zone}");
    if dsn.starts_with("postgres://") || dsn.starts_with("postgresql://") {
        let encoded: String =
            percent_encoding::utf8_percent_encode(&setting, percent_encoding::NON_ALPHANUMERIC)
                .to_string();
        let (base, fragment) = match dsn.split_once('#') {
            Some((base, fragment)) => (base, Some(fragment)),
            None => (dsn, None),
        };
        let mut url = if let Some((path, query)) = base.split_once('?') {
            let mut merged = Vec::new();
            let mut extended = false;
            for pair in query.split('&').filter(|pair| !pair.is_empty()) {
                if let Some(existing) = pair.strip_prefix("options=") {
                    merged.push(format!("options={existing}%20{encoded}"));
                    extended = true;
                } else {
                    merged.push(pair.to_owned());
                }
            }
            if !extended {
                merged.push(format!("options={encoded}"));
            }
            format!("{path}?{}", merged.join("&"))
        } else {
            format!("{base}?options={encoded}")
        };
        if let Some(fragment) = fragment {
            url.push('#');
            url.push_str(fragment);
        }
        return url;
    }
    format!("{dsn} options='{setting}'")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_pg_adds_the_session_time_zone() {
        let zone = "America/Toronto";
        let cases = [
            ("", ""),
            (
                "postgres://u:p@h:5432/db?sslmode=disable",
                "postgres://u:p@h:5432/db?sslmode=disable&options=%2Dc%20TimeZone%3DAmerica%2FToronto",
            ),
            (
                "postgres://u@h/db",
                "postgres://u@h/db?options=%2Dc%20TimeZone%3DAmerica%2FToronto",
            ),
            (
                "postgres://u@h/db?options=-c%20TimeZone%3DUTC",
                "postgres://u@h/db?options=-c%20TimeZone%3DUTC",
            ),
            (
                "postgres://u@h/db?options=-c%20work_mem%3D64MB",
                "postgres://u@h/db?options=-c%20work_mem%3D64MB%20%2Dc%20TimeZone%3DAmerica%2FToronto",
            ),
            (
                "host=h dbname=db",
                "host=h dbname=db options='-c TimeZone=America/Toronto'",
            ),
        ];
        for (input, want) in cases {
            assert_eq!(with_session_time_zone(input, zone), want, "input {input:?}");
        }
    }

    /// The guard is guarded: a test that reads a database URL with
    /// `std::env::var` would run in the server's time zone (UTC on CI).
    #[test]
    fn test_pg_guard_every_database_url_read_goes_through_test_pg() {
        let root = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("src");
        let mut offenders = Vec::new();
        walk(&root, &mut |path, text| {
            if path.ends_with("test_pg.rs") {
                return;
            }
            offenders.extend(offending_reads(&path.display().to_string(), text));
        });
        assert!(
            offenders.is_empty(),
            "these reads of a database URL bypass test_pg::database_url_var, so their sessions run in the server's time zone (UTC on CI):\n  {}",
            offenders.join("\n  ")
        );
    }

    #[test]
    fn test_pg_guard_refuses_a_direct_read() {
        for (source, refuse) in [
            (
                r#"let url = std::env::var("ELITEA_TEST_DATABASE_URL");"#,
                true,
            ),
            (r#"let url = env::var("ELITEA_TEST_DATABASE_URL");"#, true),
            (
                r#"const TEST_DATABASE_URL: &str = "ELITEA_TEST_DATABASE_URL"; let u = env::var(TEST_DATABASE_URL);"#,
                true,
            ),
            (r#"let url = std::env::var_os("SOME_WAREHOUSE_DSN");"#, true),
            (r#"let url = std::env::var("PGHOST");"#, true),
            (
                r#"let name = std::env::var("ELITEA_COMPACTION_TEST_DATABASE");"#,
                false,
            ),
            (
                r#"let url = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL");"#,
                false,
            ),
            (r#"let url = std::env::var("ELITEA_TEST_NATS_URL");"#, false),
        ] {
            assert_eq!(
                !offending_reads("scratch.rs", source).is_empty(),
                refuse,
                "{source}"
            );
        }
    }

    fn offending_reads(name: &str, text: &str) -> Vec<String> {
        let mut constants = std::collections::HashMap::new();
        for line in text.lines() {
            if let Some((left, right)) = line.split_once(": &str = \"")
                && let Some(identifier) = left.split_whitespace().last()
                && let Some(value) = right.split('"').next()
            {
                constants.insert(identifier.to_owned(), value.to_owned());
            }
        }
        let mut offenders = Vec::new();
        for (number, line) in text.lines().enumerate() {
            for marker in ["env::var(", "env::var_os("] {
                let mut rest = line;
                while let Some(index) = rest.find(marker) {
                    let after = &rest[index + marker.len()..];
                    let argument = after.split(')').next().unwrap_or("").trim();
                    let resolved = argument
                        .strip_prefix('"')
                        .and_then(|value| value.strip_suffix('"'))
                        .map(str::to_owned)
                        .or_else(|| constants.get(argument).cloned())
                        .unwrap_or_default();
                    if is_database_location(&resolved) {
                        offenders.push(format!("{name}:{} reads {resolved}", number + 1));
                    }
                    rest = after;
                }
            }
        }
        offenders
    }

    /// A variable that holds a connection string (`*DATABASE_URL`, `*_DSN`,
    /// libpq's `PGHOST`/`PGDATABASE`). A bare `*_DATABASE` holds a database
    /// NAME that a test combines with the wrapped URL, so it stays raw.
    fn is_database_location(name: &str) -> bool {
        name.ends_with("DATABASE_URL")
            || name.ends_with("_DSN")
            || name.contains("_DSN_")
            || name == "PGHOST"
            || name == "PGDATABASE"
    }

    fn walk(directory: &std::path::Path, visit: &mut dyn FnMut(&std::path::Path, &str)) {
        let Ok(entries) = std::fs::read_dir(directory) else {
            return;
        };
        for entry in entries.flatten() {
            let path = entry.path();
            if path.is_dir() {
                walk(&path, visit);
            } else if path.extension().is_some_and(|extension| extension == "rs")
                && let Ok(text) = std::fs::read_to_string(&path)
            {
                visit(&path, &text);
            }
        }
    }
}
