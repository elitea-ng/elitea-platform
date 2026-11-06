//! FU-287: the capacity fast path of `PostgresSessionService::append`.
//!
//! `ensure_event_capacity` first compares the session's *stored* row count and
//! bytes with the caps, and classifies the payloads only when they come close.
//! That is sound while the active events are a subset of the stored ones. These
//! tests pin it on real `PostgreSQL`, guard the storage assumption it leans on,
//! and measure what it buys in a long chat.

use std::path::{Path, PathBuf};
use std::time::Instant;

use super::runner_view_pg_tests::{
    Fixture, Rng, authority_binds, branch_event, control_event, event, marker,
};
use crate::state::{SessionLimits, active_events_sql, disable_capacity_fast_path};

/// `(count, bytes)` of the rows the SQL keeps, or of every stored row.
async fn totals(fixture: &Fixture, active: bool) -> (i64, i64) {
    let scope = authority_binds();
    let query = if active {
        format!(
            "{} SELECT count(*), COALESCE(sum(payload_bytes), 0)::bigint FROM active_events",
            active_events_sql()
        )
    } else {
        "SELECT count(*), COALESCE(sum(payload_bytes), 0)::bigint \
         FROM elitea_runtime.agent_session_events \
         WHERE tenant_id = $1 AND resource_project_id = $2 AND projection_project_id = $3 \
         AND capability_id = $4 AND session_family = $5 AND definition_digest = $6 \
         AND thread_id = $7 AND app_name = $8 AND user_id = $9 AND session_id = $10"
            .to_owned()
    };
    let query = sqlx::query_as::<_, (i64, i64)>(sqlx::AssertSqlSafe(query))
        .bind(scope.0)
        .bind(scope.1)
        .bind(scope.2)
        .bind(scope.3)
        .bind(scope.4)
        .bind(scope.5.as_slice())
        .bind(scope.6)
        .bind(scope.7)
        .bind(scope.8)
        .bind(scope.9);
    let query = if active {
        query.bind(Option::<&str>::None).bind(false)
    } else {
        query
    };
    query.fetch_one(&fixture.db.pool).await.expect("totals")
}

/// The capacity decision for `candidate`, with or without the fast path.
async fn decision(
    fixture: &Fixture,
    candidate: &adk_rust::Event,
    max_events: usize,
    max_bytes: usize,
    fast_path: bool,
) -> Result<(), &'static str> {
    let _off = (!fast_path).then(disable_capacity_fast_path);
    fixture
        .service
        .capacity_decision(
            candidate,
            SessionLimits {
                max_events,
                max_retained_event_bytes: max_bytes,
                ..SessionLimits::default()
            },
        )
        .await
        .map_err(|error| error.code())
}

/// The smallest cap in `1..=top` which the exact classification admits.
async fn smallest_admitting_cap(
    fixture: &Fixture,
    candidate: &adk_rust::Event,
    top: usize,
    by_events: bool,
) -> usize {
    let (mut low, mut high) = (1_usize, top.max(1));
    while low < high {
        let middle = low + (high - low) / 2;
        let (events, bytes) = if by_events {
            (middle, 64 * 1024 * 1024)
        } else {
            (4096, middle)
        };
        if decision(fixture, candidate, events, bytes, false)
            .await
            .is_ok()
        {
            high = middle;
        } else {
            low = middle + 1;
        }
    }
    low
}

fn candidate(rng: &mut Rng, step: usize) -> adk_rust::Event {
    let id = format!("c{step:03}");
    match rng.below(10) {
        0..=1 => event(&id, "user"),
        2..=3 => event(&id, "agent"),
        4 => control_event(&id),
        5 => marker(&id, "", None, rng.below(600)),
        6 => marker(&id, "", Some("agent"), rng.below(600)),
        7 => marker(&id, "", Some("other"), rng.below(300)),
        8 => branch_event(&id, ["b1", "b2"][rng.below(2)]),
        _ => marker(&id, "b1", None, rng.below(200)),
    }
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // One generated story per seed.
async fn stored_totals_bound_the_active_totals_and_the_fast_path_decides_like_the_exact_check() {
    for seed in 1..=3_u64 {
        let Some(fixture) = Fixture::new(SessionLimits::default()).await else {
            return;
        };
        let mut rng = Rng(0xC2B2_AE3D_27D4_EB4F ^ seed.wrapping_mul(0x9E37_79B9_7F4A_7C15));
        let mut checked = 0_usize;
        for step in 0..45_usize {
            let next = candidate(&mut rng, step);
            let (stored_count, stored_bytes) = totals(&fixture, false).await;
            let (active_count, active_bytes) = totals(&fixture, true).await;
            assert!(
                stored_count >= active_count && stored_bytes >= active_bytes,
                "seed {seed} step {step}: stored ({stored_count}, {stored_bytes}) must bound \
                 active ({active_count}, {active_bytes})"
            );
            // The caps which straddle the totals: the stored ones, and the ones
            // this very candidate's exact check turns on.
            let stored_count = usize::try_from(stored_count).expect("count");
            let stored_bytes = usize::try_from(stored_bytes).expect("bytes");
            let length = fixture.service.encoded_event_len(&next);
            let exact_events =
                smallest_admitting_cap(&fixture, &next, stored_count + 2, true).await;
            let exact_bytes =
                smallest_admitting_cap(&fixture, &next, stored_bytes + length + 2, false).await;
            let mut event_caps = vec![];
            for pivot in [stored_count, exact_events] {
                event_caps.extend([pivot.saturating_sub(1), pivot, pivot + 1]);
            }
            let mut byte_caps = vec![];
            for pivot in [stored_bytes + length, exact_bytes] {
                byte_caps.extend([pivot.saturating_sub(1), pivot, pivot + 1]);
            }
            event_caps.retain(|cap| (1..=4096).contains(cap));
            byte_caps.retain(|cap| (1..=64 * 1024 * 1024).contains(cap));
            event_caps.sort_unstable();
            event_caps.dedup();
            byte_caps.sort_unstable();
            byte_caps.dedup();
            for &events in &event_caps {
                for &bytes in &byte_caps {
                    let fast = decision(&fixture, &next, events, bytes, true).await;
                    let exact = decision(&fixture, &next, events, bytes, false).await;
                    assert_eq!(
                        fast, exact,
                        "seed {seed} step {step}: caps ({events} events, {bytes} bytes) \
                         over stored ({stored_count}, {stored_bytes}), active \
                         ({active_count}, {active_bytes})"
                    );
                    checked += 1;
                }
            }
            fixture.append(next).await.expect("append");
        }
        let (stored_count, stored_bytes) = totals(&fixture, false).await;
        let (active_count, active_bytes) = totals(&fixture, true).await;
        assert!(stored_count >= active_count && stored_bytes >= active_bytes);
        assert!(
            stored_count > active_count,
            "the story retired rows (stored {stored_count}, active {active_count})"
        );
        eprintln!("FU287_CAPACITY seed={seed} decisions={checked}");
        fixture.db.pool.close().await;
    }
}

/// Every `.rs` and `.sql` file under `dir`, recursively.
fn source_files(dir: &Path, files: &mut Vec<PathBuf>) {
    for entry in std::fs::read_dir(dir)
        .expect("read a source directory")
        .flatten()
    {
        let path = entry.path();
        if path.is_dir() {
            source_files(&path, files);
        } else if matches!(
            path.extension().and_then(|extension| extension.to_str()),
            Some("rs" | "sql")
        ) {
            files.push(path);
        }
    }
}

/// A test file by the repository's naming: `*_tests.rs`, `tests.rs`, or a file
/// below a `tests` or `*_tests` directory.
fn is_test_file(path: &Path) -> bool {
    let name = path
        .file_name()
        .and_then(|name| name.to_str())
        .unwrap_or("");
    name.ends_with("_tests.rs")
        || name == "tests.rs"
        || path.components().any(|component| {
            let part = component.as_os_str().to_str().unwrap_or("");
            part == "tests" || part.ends_with("_tests")
        })
}

/// Whether `text` deletes or truncates the event table (case-insensitive,
/// whitespace-insensitive, schema-qualified or not).
fn removes_session_events(text: &str) -> bool {
    let flat = text
        .to_ascii_lowercase()
        .split_whitespace()
        .collect::<Vec<_>>()
        .join(" ");
    [
        "elitea_runtime.agent_session_events",
        "agent_session_events",
    ]
    .iter()
    .any(|table| {
        [
            format!("delete from {table}"),
            format!("truncate {table}"),
            format!("truncate table {table}"),
        ]
        .iter()
        .any(|statement| flat.contains(statement))
    })
}

#[test]
fn the_guard_recognizes_every_spelling_of_a_delete_or_truncate() {
    for statement in [
        "DELETE FROM elitea_runtime.agent_session_events WHERE true",
        "delete from agent_session_events",
        "Delete\n   From   Elitea_Runtime.Agent_Session_Events",
        "TRUNCATE elitea_runtime.agent_session_events",
        "truncate table agent_session_events",
        "TRUNCATE   TABLE\nelitea_runtime.agent_session_events CASCADE",
    ] {
        assert!(removes_session_events(statement), "{statement}");
    }
    assert!(!removes_session_events(
        "DELETE FROM elitea_runtime.agent_sessions WHERE session_id = $1"
    ));
    assert!(!removes_session_events(
        "SELECT * FROM agent_session_events"
    ));
}

/// The capacity fast path in `ensure_event_capacity` counts *stored* rows and
/// bytes as an upper bound of the active ones. Nothing in the Worker may
/// delete or truncate stored events (a whole session's deletion cascades from
/// `agent_sessions`), or that bound's assumptions must be revisited.
#[test]
fn no_worker_source_deletes_or_truncates_stored_session_events() {
    let root = Path::new(env!("CARGO_MANIFEST_DIR")).join("src");
    let mut files = Vec::new();
    source_files(&root, &mut files);
    assert!(files.len() > 100, "the scan found the Worker's sources");
    let offenders = files
        .iter()
        .filter(|path| !is_test_file(path))
        .filter(|path| removes_session_events(&std::fs::read_to_string(path).expect("read source")))
        .map(|path| path.display().to_string())
        .collect::<Vec<_>>();
    assert!(
        offenders.is_empty(),
        "these sources delete or truncate agent_session_events: {offenders:?}. The capacity \
         fast path in `ensure_event_capacity` relies on append-only rows and must be \
         revisited before this lands"
    );
}

/// The migrations of the table: no trigger or rule on it, and the only
/// deletion path is the foreign key from `agent_sessions` (`ON DELETE CASCADE`).
#[test]
fn the_event_table_migrations_have_no_delete_trigger_or_rule() {
    let directory =
        Path::new(env!("CARGO_MANIFEST_DIR")).join("../elitea-main/migrations/agentstate");
    let mut migrations = Vec::new();
    source_files(&directory, &mut migrations);
    assert!(
        !migrations.is_empty(),
        "the agentstate migrations were found"
    );
    for path in migrations
        .iter()
        .filter(|path| path.extension().and_then(|extension| extension.to_str()) == Some("sql"))
    {
        let text = std::fs::read_to_string(path)
            .expect("read migration")
            .to_ascii_lowercase();
        let flat = text.split_whitespace().collect::<Vec<_>>().join(" ");
        for statement in flat.split(';') {
            if !statement.contains("agent_session_events") {
                continue;
            }
            assert!(
                !statement.contains("create trigger")
                    && !statement.contains("create or replace trigger")
                    && !statement.contains("create rule")
                    && !statement.contains("create or replace rule"),
                "{} adds a trigger or rule on agent_session_events: the capacity fast path in \
                 `ensure_event_capacity` relies on append-only rows and must be revisited",
                path.display()
            );
            assert!(
                !removes_session_events(statement),
                "{} deletes or truncates agent_session_events: the capacity fast path in \
                 `ensure_event_capacity` relies on append-only rows and must be revisited",
                path.display()
            );
        }
    }
}

/// Latencies of appends, in microseconds.
#[allow(clippy::cast_precision_loss)] // Microsecond statistics for a printout.
struct Arm(Vec<u128>);

impl Arm {
    #[allow(clippy::cast_precision_loss)] // Microsecond statistics for a printout.
    fn report(&self, label: &str) -> (f64, u128, u128) {
        let mut sorted = self.0.clone();
        sorted.sort_unstable();
        let mean = sorted.iter().sum::<u128>() as f64 / sorted.len() as f64;
        let p50 = sorted[sorted.len() / 2];
        let p95 = sorted[(sorted.len() * 95 / 100).min(sorted.len() - 1)];
        eprintln!(
            "FU287_APPEND {label}: n={} mean_us={mean:.0} p50_us={p50} p95_us={p95}",
            sorted.len()
        );
        (mean, p50, p95)
    }
}

/// One append of a long chat's turn: the user, the agent, and a recovery
/// marker which carries a large request payload (so retired markers pile up).
fn long_chat_event(index: usize, turn: usize) -> adk_rust::Event {
    match index % 3 {
        0 => event(&format!("u{turn}"), "user"),
        1 => event(&format!("a{turn}"), "agent"),
        _ => marker(&format!("m{turn}"), "", None, 64 * 1024),
    }
}

/// Append `count` events alternating the two arms on the same session and
/// database (fast path on for even appends, off for odd ones), so both arms see
/// the same stored history.
async fn alternate_arms(fixture: &Fixture, start_turn: usize, count: usize) -> (Arm, Arm) {
    let (mut on, mut off) = (Arm(Vec::new()), Arm(Vec::new()));
    for index in 0..count {
        let next = long_chat_event(index, start_turn + index / 3);
        let fast_path = index % 2 == 0;
        let _guard = (!fast_path).then(disable_capacity_fast_path);
        let started = Instant::now();
        fixture.append(next).await.expect("append");
        let elapsed = started.elapsed().as_micros();
        if fast_path { &mut on } else { &mut off }.0.push(elapsed);
    }
    (on, off)
}

fn percent(on: f64, off: f64) -> f64 {
    (on - off) / off * 100.0
}

/// Manual measurement: a long chat past the stored caps, fast path on and off.
/// Run: `cargo test --offline -p elitea-worker-rust long_chat_append -- --ignored --nocapture`.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "measurement: long chat, minutes"]
#[allow(clippy::cast_precision_loss)] // Microsecond statistics for a printout.
async fn measure_long_chat_append_latency_past_the_stored_caps() {
    const BELOW_CAPS_TURNS: usize = 200;
    const MEASURED: usize = 200;
    let _cross_check_off = crate::agents::runner_view::disable_cross_check();
    // Below the caps: a fresh session of a few hundred turns.
    let Some(below) = Fixture::new(SessionLimits::default()).await else {
        return;
    };
    for index in 0..BELOW_CAPS_TURNS * 3 {
        below
            .append(long_chat_event(index, index / 3))
            .await
            .expect("build below the caps");
    }
    let (count, bytes) = totals(&below, false).await;
    let (active_count, active_bytes) = totals(&below, true).await;
    eprintln!(
        "FU287_APPEND below: stored=({count} rows, {bytes} B) active=({active_count} rows, \
         {active_bytes} B) caps=(4096 rows, {} B)",
        SessionLimits::default().max_retained_event_bytes
    );
    let (on, off) = alternate_arms(&below, BELOW_CAPS_TURNS, MEASURED).await;
    let (on_stats, off_stats) = (
        on.report("below fast_path=on"),
        off.report("below fast_path=off"),
    );
    eprintln!(
        "FU287_APPEND below diff: mean {:+.1}% p50 {:+.1}% p95 {:+.1}%",
        percent(on_stats.0, off_stats.0),
        percent(on_stats.1 as f64, off_stats.1 as f64),
        percent(on_stats.2 as f64, off_stats.2 as f64)
    );
    below.db.pool.close().await;

    // Past the caps: the default caps, until both rows and bytes are over.
    let limits = SessionLimits::default();
    let Some(over) = Fixture::new(limits).await else {
        return;
    };
    let build_started = Instant::now();
    let mut turn = 0_usize;
    loop {
        for index in 0..3 {
            over.append(long_chat_event(index, turn))
                .await
                .expect("build");
        }
        turn += 1;
        if turn.is_multiple_of(100) {
            let (count, bytes) = totals(&over, false).await;
            if usize::try_from(count).is_ok_and(|rows| rows > limits.max_events)
                && usize::try_from(bytes).is_ok_and(|b| b > limits.max_retained_event_bytes)
            {
                break;
            }
        }
    }
    let (count, bytes) = totals(&over, false).await;
    let (active_count, active_bytes) = totals(&over, true).await;
    eprintln!(
        "FU287_APPEND over: built {turn} turns in {:?}; stored=({count} rows, {bytes} B) \
         active=({active_count} rows, {active_bytes} B) caps=({} rows, {} B)",
        build_started.elapsed(),
        limits.max_events,
        limits.max_retained_event_bytes
    );
    let (on, off) = alternate_arms(&over, turn, MEASURED).await;
    let (on_stats, off_stats) = (
        on.report("over fast_path=on"),
        off.report("over fast_path=off"),
    );
    eprintln!(
        "FU287_APPEND over diff: mean {:+.1}% p50 {:+.1}% p95 {:+.1}%",
        percent(on_stats.0, off_stats.0),
        percent(on_stats.1 as f64, off_stats.1 as f64),
        percent(on_stats.2 as f64, off_stats.2 as f64)
    );
    over.db.pool.close().await;
}
