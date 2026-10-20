//! Test `PostgreSQL` sessions run in a non-UTC time zone.
//!
//! The pylon-era tables keep `created_at`/`updated_at` as `timestamp`
//! WITHOUT time zone holding the UTC wall clock, beside timestamptz columns.
//! Postgres converts between the two through the SESSION time zone, so a
//! defect that mixes them is visible only off UTC; CI's `PostgreSQL` runs in
//! UTC. This is the Rust twin of elitea-main's `internal/testpg`.
//!
//! The zone cannot travel in the URL. sqlx 0.8 always sends its own
//! `TimeZone=UTC` startup parameter, and the server ranks that above
//! `-c TimeZone` inside `options` and above `ALTER DATABASE … SET`. So every
//! test pool is built from [`pool_options`], whose `after_connect` runs
//! `SET TimeZone` on each new connection; a test that needs a bare
//! connection uses [`connect`]. The zone is America/Toronto, or
//! `ELITEA_TEST_SESSION_TIMEZONE`. Production pools never pass through here
//! and keep sqlx's UTC session.
//!
//! Three layers keep it that way:
//!
//! - **Types.** [`connect_options`] returns [`TestConnectOptions`], which has
//!   no `.connect()`, and [`pool_options`] returns [`TestPoolOptions`], whose
//!   `connect`/`connect_with` check `SHOW TimeZone` on the new pool before
//!   handing it out. A pool from here is checked; nothing can forget.
//! - **Text.** `test_pg_guard_every_test_pool_comes_from_test_pg` fails test
//!   code that builds a Postgres pool or connection itself, parses
//!   `PgConnectOptions` other than through [`connect_options`], or reads a
//!   database URL with `std::env::var` instead of [`database_url_var`].
//! - **Behaviour.** The shared fixtures (`IsolatedPostgres::create`,
//!   `install_schema`, `install_test_schema`, the pipeline fixture's
//!   `activate`) call [`assert_test_session`] on the pool a test hands them,
//!   which fails a UTC pool however it was built.

use std::env::VarError;
use std::time::Duration;

use sqlx::Executor as _;
use sqlx::PgPool;
use sqlx::postgres::{PgConnectOptions, PgConnection, PgPoolOptions};

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

/// `std::env::var(name)` for a test database URL, returned unchanged. It is
/// the one sanctioned read, so the guard can tell a database URL from any
/// other variable; the zone itself is applied by [`pool_options`].
pub(crate) fn database_url_var(name: &str) -> Result<String, VarError> {
    std::env::var(name)
}

/// Connection options for a test database. Deliberately NOT
/// `PgConnectOptions`: that type has `.connect()`, which opens a session in
/// sqlx's `UTC`. This one connects only through [`TestPoolOptions`] or
/// [`connect`], both of which set the test zone.
#[derive(Clone, Debug)]
pub(crate) struct TestConnectOptions(PgConnectOptions);

impl TestConnectOptions {
    #[must_use]
    pub(crate) fn database(self, database: &str) -> Self {
        Self(self.0.database(database))
    }

    #[must_use]
    pub(crate) fn disable_statement_logging(self) -> Self {
        use sqlx::ConnectOptions as _;
        Self(self.0.disable_statement_logging())
    }

    pub(crate) fn get_host(&self) -> &str {
        self.0.get_host()
    }

    pub(crate) fn get_database(&self) -> Option<&str> {
        self.0.get_database()
    }

    pub(crate) fn get_username(&self) -> &str {
        self.0.get_username()
    }
}

/// Parses a test database URL. The one sanctioned parse in test code: the
/// guard refuses a raw `PgConnectOptions` parse there.
pub(crate) fn connect_options(url: &str) -> Result<TestConnectOptions, sqlx::Error> {
    url.parse().map(TestConnectOptions)
}

/// `PgPoolOptions` for tests. Every connection runs `SET TimeZone` before it
/// is handed out, and [`Self::connect`]/[`Self::connect_with`] refuse the
/// new pool unless `SHOW TimeZone` answers the test zone.
#[must_use]
pub(crate) struct TestPoolOptions(PgPoolOptions);

impl TestPoolOptions {
    pub(crate) fn max_connections(self, max: u32) -> Self {
        Self(self.0.max_connections(max))
    }

    pub(crate) fn min_connections(self, min: u32) -> Self {
        Self(self.0.min_connections(min))
    }

    pub(crate) fn acquire_timeout(self, timeout: Duration) -> Self {
        Self(self.0.acquire_timeout(timeout))
    }

    pub(crate) fn idle_timeout(self, timeout: impl Into<Option<Duration>>) -> Self {
        Self(self.0.idle_timeout(timeout))
    }

    /// Connects and checks the session zone of the new pool.
    pub(crate) async fn connect_with(
        self,
        options: TestConnectOptions,
    ) -> Result<PgPool, sqlx::Error> {
        let pool = self.0.connect_with(options.0).await?;
        check_test_session(&pool).await?;
        Ok(pool)
    }

    /// [`Self::connect_with`] for a URL.
    pub(crate) async fn connect(self, url: &str) -> Result<PgPool, sqlx::Error> {
        self.connect_with(connect_options(url)?).await
    }

    /// A pool that never connects (a fixture's unreachable URL). There is no
    /// session to check; any connection it does open still runs `SET TimeZone`.
    pub(crate) fn connect_lazy(self, url: &str) -> Result<PgPool, sqlx::Error> {
        Ok(self.0.connect_lazy_with(connect_options(url)?.0))
    }
}

/// [`TestPoolOptions`]: chain the usual settings onto it.
pub(crate) fn pool_options() -> TestPoolOptions {
    TestPoolOptions(raw_pool_options())
}

fn raw_pool_options() -> PgPoolOptions {
    let statement = set_time_zone_statement(&time_zone());
    PgPoolOptions::new().after_connect(move |connection, _| {
        let statement = statement.clone();
        Box::pin(async move {
            connection.execute(statement.as_str()).await?;
            Ok(())
        })
    })
}

/// One test connection in the test session time zone, for a test that needs
/// a bare `PgConnection` rather than a pool.
pub(crate) async fn connect(options: &TestConnectOptions) -> Result<PgConnection, sqlx::Error> {
    use sqlx::ConnectOptions as _;
    let mut connection = options.0.connect().await?;
    connection
        .execute(set_time_zone_statement(&time_zone()).as_str())
        .await?;
    Ok(connection)
}

async fn session_zone(pool: &PgPool) -> Result<String, sqlx::Error> {
    sqlx::query_scalar("SHOW TimeZone").fetch_one(pool).await
}

fn wrong_zone_message(fixture: &str, zone: &str) -> String {
    format!(
        "{fixture}: this test's PostgreSQL session runs in TimeZone {zone:?}, not the test zone \
         {:?}. The pool was not built by crate::test_pg::pool_options (or test_pg::connect), so \
         sqlx's startup TimeZone=UTC hides naive-timestamp defects; build it with \
         test_pg::pool_options()",
        time_zone()
    )
}

/// The check [`TestPoolOptions`] runs: an error, not a panic, so a fixture's
/// `Drop` cleanup that uses `if let Ok(..)` stays panic-free.
async fn check_test_session(pool: &PgPool) -> Result<(), sqlx::Error> {
    let zone = session_zone(pool).await?;
    if zone == time_zone() {
        Ok(())
    } else {
        Err(sqlx::Error::Configuration(
            wrong_zone_message("test_pg::pool_options", &zone).into(),
        ))
    }
}

/// Panics unless `pool` runs in the test session time zone.
///
/// The text guard matches spellings and the types cover pools built here;
/// this matches behaviour. The shared fixtures call it on the pool a test
/// hands them, so a pool built by hand — however it was spelled — still runs
/// in sqlx's `UTC` session and fails here.
pub(crate) async fn assert_test_session(pool: &PgPool, fixture: &str) {
    let zone = session_zone(pool)
        .await
        .unwrap_or_else(|error| panic!("{fixture}: SHOW TimeZone on the test pool: {error}"));
    assert!(
        zone == time_zone(),
        "{}",
        wrong_zone_message(fixture, &zone)
    );
}

fn set_time_zone_statement(zone: &str) -> String {
    format!("SET TimeZone = '{}'", zone.replace('\'', "''"))
}

/// A random 32-character lowercase hex id. `PostgreSQL` fixtures that share
/// one database derive job scopes and owner ids from it, so tests running in
/// parallel never select, claim or reap each other's rows.
#[allow(
    clippy::format_collect,
    reason = "Keep independent hexadecimal fixture generation separate from production helpers."
)]
pub(crate) fn unique_hex() -> String {
    let mut id = [0_u8; 16];
    ring::rand::SecureRandom::fill(&ring::rand::SystemRandom::new(), &mut id)
        .expect("system randomness for a test id");
    id.iter().map(|byte| format!("{byte:02x}")).collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_pg_quotes_the_zone() {
        assert_eq!(
            set_time_zone_statement("America/Toronto"),
            "SET TimeZone = 'America/Toronto'"
        );
        assert_eq!(set_time_zone_statement("a'b"), "SET TimeZone = 'a''b'");
    }

    /// The point of the module: sqlx sends `TimeZone=UTC` at startup, so only
    /// a session from [`pool_options`] or [`connect`] runs off UTC. Skipped
    /// when `ELITEA_TEST_DATABASE_URL` is unset.
    #[tokio::test]
    async fn test_pg_sessions_run_in_the_test_time_zone() {
        let Ok(url) = database_url_var("ELITEA_TEST_DATABASE_URL") else {
            eprintln!("skipping test_pg session test: set ELITEA_TEST_DATABASE_URL");
            return;
        };
        let options = connect_options(&url).expect("parse database URL");
        let pool = pool_options()
            .max_connections(2)
            .connect_with(options.clone())
            .await
            .expect("connect test pool");
        // Two live connections: the setting is per connection, not per pool.
        let mut first = pool.acquire().await.expect("first connection");
        let mut second = pool.acquire().await.expect("second connection");
        for connection in [&mut first, &mut second] {
            let zone: String = sqlx::query_scalar("SHOW TimeZone")
                .fetch_one(&mut **connection)
                .await
                .expect("show time zone");
            assert_eq!(zone, time_zone());
            assert_ne!(zone, "UTC", "a test pool session must not run in UTC");
        }
        drop((first, second));
        pool.close().await;

        let mut single = connect(&options).await.expect("single connection");
        let zone: String = sqlx::query_scalar("SHOW TimeZone")
            .fetch_one(&mut single)
            .await
            .expect("show time zone");
        assert_eq!(zone, time_zone());
        assert_ne!(zone, "UTC", "a test connection must not run in UTC");

        // The control: sqlx's own session stays UTC even with the zone in
        // `options`, which is why a URL-only wrapper has no effect.
        let plain = PgPoolOptions::new() // test_pg:control
            .max_connections(1)
            .connect_with(options.0.clone().options([("TimeZone", time_zone())]))
            .await
            .expect("connect plain pool");
        let zone: String = sqlx::query_scalar("SHOW TimeZone")
            .fetch_one(&plain)
            .await
            .expect("show time zone");
        assert_eq!(
            zone, "UTC",
            "sqlx's startup TimeZone=UTC no longer wins over options; revisit test_pg"
        );
        plain.close().await;

        // A pool from test_pg is checked before it is handed out: options
        // without the SET TimeZone hook (the shape of any bypass) are refused.
        let unhooked = TestPoolOptions(PgPoolOptions::new()) // test_pg:control
            .max_connections(1)
            .connect_with(options.clone())
            .await;
        match unhooked {
            Err(sqlx::Error::Configuration(message)) => assert!(
                message.to_string().contains("test_pg::pool_options"),
                "{message}"
            ),
            other => panic!("a UTC pool came out of TestPoolOptions: {other:?}"),
        }
    }

    /// The guard is guarded: test code that builds its own pool or reads a
    /// database URL directly runs in UTC, whatever its URL says.
    #[test]
    fn test_pg_guard_every_test_pool_comes_from_test_pg() {
        let root = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("src");
        let mut offenders = Vec::new();
        walk(&root, &mut |path, text| {
            if path.ends_with("test_pg.rs") {
                return;
            }
            let relative = path
                .strip_prefix(&root)
                .unwrap_or(path)
                .display()
                .to_string();
            offenders.extend(offending_reads(&relative, text));
            offenders.extend(offending_pools(&relative, text));
        });
        assert!(
            offenders.is_empty(),
            "these bypass test_pg (use test_pg::database_url_var and test_pg::pool_options or test_pg::connect), so their sessions run in UTC:\n  {}",
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

    #[test]
    fn test_pg_guard_refuses_a_hand_built_test_pool() {
        for (name, source, refuse) in [
            (
                "a_tests.rs",
                "let p = PgPoolOptions::new().connect_with(o);",
                true,
            ),
            (
                "a_tests.rs",
                "let p = sqlx::postgres::PgPoolOptions::new();",
                true,
            ),
            ("a_tests.rs", "let p = PgPool::connect(&url).await;", true),
            ("a_tests.rs", "let p = PgPool::connect_with(o).await;", true),
            ("a_tests.rs", "let p = PgPool::connect_lazy(&url);", true),
            (
                "a_tests.rs",
                "let c = PgConnection::connect(&url).await;",
                true,
            ),
            (
                "a_tests.rs",
                "let p = Pool::<Postgres>::connect(&url).await;",
                true,
            ),
            (
                "a_tests.rs",
                "let p = PoolOptions::<Postgres>::new();",
                true,
            ),
            ("x/fanout_tests/a.rs", "let p = PgPoolOptions::new();", true),
            ("x/a_test_support.rs", "let p = PgPoolOptions::new();", true),
            (
                "a_tests.rs",
                "let c = PgConnectOptions::from_str(&u)?.connect().await;",
                true,
            ),
            (
                "a_tests.rs",
                "let p = crate::test_pg::pool_options().connect_with(o);",
                false,
            ),
            (
                "a_tests.rs",
                "let c = crate::test_pg::connect(&o).await;",
                false,
            ),
            (
                "a_tests.rs",
                "let p = PgPoolOptions::new(); // test_pg:control",
                false,
            ),
            // A tonic channel is not a Postgres session.
            (
                "a_tests.rs",
                "let c = Endpoint::from_static(\"https://x\").connect().await;",
                false,
            ),
            // Production code before any #[cfg(test)] is not test code.
            ("bootstrap.rs", "let p = PgPoolOptions::new();", false),
            (
                "bootstrap.rs",
                "fn f() {}\n#[cfg(test)]\nmod tests { fn g() { let p = PgPoolOptions::new(); } }",
                true,
            ),
        ] {
            assert_eq!(
                !offending_pools(name, source).is_empty(),
                refuse,
                "{name}: {source}"
            );
        }
    }

    /// The forms the first guard missed (PR #1295 review): a parse and a
    /// connect on different lines, an inferred database type, a listener,
    /// an aliased import, sqlx's own test macro.
    #[test]
    fn test_pg_guard_refuses_the_spellings_that_hid_a_test_pool() {
        for (name, source, refuse) in [
            // Options parsed in one statement and connected in a later one.
            (
                "a_tests.rs",
                "let o = PgConnectOptions::from_str(&u)?;\nlet c = o.connect().await;",
                true,
            ),
            (
                "a_tests.rs",
                "let o = sqlx::postgres::PgConnectOptions::from_str(\n    &u,\n)?;",
                true,
            ),
            (
                "a_tests.rs",
                "let c = url.parse::<PgConnectOptions>()?.connect().await;",
                true,
            ),
            (
                "a_tests.rs",
                "let o: PgConnectOptions = url.parse()?;",
                true,
            ),
            (
                "a_tests.rs",
                "let o = PgConnectOptions::new().host(\"h\");\nlet c = o.connect().await;",
                true,
            ),
            // The database type inferred from the binding.
            (
                "a_tests.rs",
                "let p: PgPool = PoolOptions::new().connect(&u).await?;",
                true,
            ),
            (
                "a_tests.rs",
                "let c: PgConnection = sqlx::Connection::connect(&u).await?;",
                true,
            ),
            (
                "a_tests.rs",
                "let l = PgListener::connect(&u).await?;",
                true,
            ),
            (
                "a_tests.rs",
                "let l = sqlx::postgres::PgListener::connect_with(&pool).await?;",
                true,
            ),
            // An alias hides every other marker.
            (
                "a_tests.rs",
                "use sqlx::postgres::PgPoolOptions as P;\nlet p = P::default();",
                true,
            ),
            (
                "a_tests.rs",
                "use sqlx::postgres::{PgConnectOptions as Options, PgPool};",
                true,
            ),
            (
                "a_tests.rs",
                "use sqlx::postgres::PgConnection as Conn;",
                true,
            ),
            // sqlx's own test macro builds the pool for the test.
            (
                "a_tests.rs",
                "#[sqlx::test]\nasync fn t(pool: PgPool) {}",
                true,
            ),
            // The wrapper has no `.connect()` (it would not compile), and a
            // reach through it to the inner options is refused by text too.
            (
                "a_tests.rs",
                "let c = crate::test_pg::connect_options(&u)?.0.connect().await;",
                true,
            ),
            (
                "a_tests.rs",
                "let p = sqlx::Pool::connect_with(crate::test_pg::connect_options(&u)?.0).await;",
                true,
            ),
            ("a_tests.rs", "let p = sqlx::Pool::connect(&u).await;", true),
            (
                "a_tests.rs",
                "let o = crate::test_pg::connect_options(&u)?;",
                false,
            ),
            ("a_tests.rs", "use std::str::FromStr as _;", false),
            (
                "a_tests.rs",
                "use sqlx::postgres::{PgConnectOptions, PgPool};",
                false,
            ),
        ] {
            assert_eq!(
                !offending_pools(name, source).is_empty(),
                refuse,
                "{name}: {source}"
            );
        }
    }

    /// Test code: a whole `*_tests.rs`, `*_test_support.rs` or `tests.rs`
    /// file or any file under a `*_tests/` directory; otherwise the text
    /// from the first `#[cfg(test)]` on.
    fn test_region<'a>(name: &str, text: &'a str) -> (usize, &'a str) {
        let file = name.rsplit('/').next().unwrap_or(name);
        let whole = file.ends_with("_tests.rs")
            || file.ends_with("_test_support.rs")
            || file == "tests.rs"
            || name.contains("_tests/");
        if whole {
            return (0, text);
        }
        match text.find("#[cfg(test)]") {
            Some(index) => (text[..index].lines().count(), &text[index..]),
            None => (0, ""),
        }
    }

    /// A Postgres pool or connection built outside `test_pg`. A line marked
    /// `test_pg:control` is a deliberate UTC control.
    ///
    /// This matches spelling, so it cannot be complete (a `type` alias, a
    /// macro, a helper in another crate). `test_pg::assert_test_session` in the
    /// shared fixtures is the behavioural half that catches what this misses.
    fn offending_pools(name: &str, text: &str) -> Vec<String> {
        // `PoolOptions::new(` also covers `PgPoolOptions::new(` and a
        // `PoolOptions::new()` whose database type is inferred;
        // `Connection::connect` covers `PgConnection::connect` and
        // `sqlx::Connection::connect` with `PgConnection` inferred.
        // `Pool::connect` covers `PgPool::connect*` and `sqlx::Pool::connect*`
        // with the database type inferred.
        const MARKERS: [&str; 12] = [
            "PoolOptions::new(",
            "PoolOptions::<Postgres>::new(",
            "Pool::connect(",
            "Pool::connect_with(",
            "Pool::connect_lazy",
            ".0.connect(",
            "Connection::connect",
            "Pool::<Postgres>::connect",
            ".after_connect(",
            "PgConnectOptions::new(",
            "PgListener::",
            "sqlx::test",
        ];
        // Types whose `use … as Alias` would hide every marker above.
        const ALIASED: [&str; 6] = [
            "PgPoolOptions as ",
            "PgConnectOptions as ",
            "PgConnection as ",
            "PgPool as ",
            "PoolOptions as ",
            "PgListener as ",
        ];
        let (offset, region) = test_region(name, text);
        let mut offenders = Vec::new();
        for (number, line) in region.lines().enumerate() {
            if line.contains("test_pg:control") {
                continue;
            }
            let found = MARKERS
                .iter()
                .chain(ALIASED.iter())
                .find(|marker| line.contains(**marker))
                .map(|marker| (*marker).to_owned())
                // Options parsed outside test_pg::connect_options are one
                // `.connect()` away from a UTC session, on this line or a later one.
                .or_else(|| {
                    (line.contains("PgConnectOptions")
                        && ["from_str", "from_url", "parse::<", ".parse()"]
                            .iter()
                            .any(|parse| line.contains(parse)))
                    .then(|| "a raw PgConnectOptions parse".to_owned())
                });
            if let Some(marker) = found {
                offenders.push(format!(
                    "{name}:{} builds a Postgres session with {marker}",
                    offset + number + 1
                ));
            }
        }
        offenders
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
    /// NAME that a test combines with the URL, so it stays raw.
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
