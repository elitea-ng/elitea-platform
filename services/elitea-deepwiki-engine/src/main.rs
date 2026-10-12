#![cfg_attr(
    not(test),
    deny(
        clippy::expect_used,
        clippy::panic,
        clippy::todo,
        clippy::unimplemented,
        clippy::unwrap_used
    )
)]

use elitea_deepwiki_engine::config::Settings;
use elitea_deepwiki_engine::{build_runner, healthcheck, server, storage, worker};
use elitea_engine_sidecar::telemetry;
use std::future::Future;
use std::process::ExitCode;
use std::time::Duration;

const USAGE: &str = "usage: elitea-deepwiki-engine [serve | healthcheck | migrate | worker | orphans --existing-projects FILE|- [--listed-at RFC3339] [--delete --listed-at RFC3339 [--allow-stale-list]] | --version]";

fn main() -> ExitCode {
    let command = std::env::args().nth(1);
    match command.as_deref() {
        None | Some("serve") => on_runtime(None, serve()),
        Some("healthcheck") => on_runtime(None, healthcheck()),
        Some("migrate") => on_runtime(None, migrate()),
        Some("orphans") => {
            let rest: Vec<String> = std::env::args().skip(2).collect();
            on_runtime(None, orphans(rest))
        }
        // The generate_wiki child: its runtime threads are its settings'.
        Some("worker") => match settings() {
            Some(settings) => {
                let threads = settings.worker.threads;
                on_runtime(Some(threads), async move {
                    let telemetry = telemetry::init(SERVICE_NAME);
                    let code = worker::run(&settings).await;
                    telemetry.shutdown().await;
                    code
                })
            }
            None => ExitCode::FAILURE,
        },
        Some("--version") => {
            println!("elitea-deepwiki-engine {}", env!("CARGO_PKG_VERSION"));
            ExitCode::SUCCESS
        }
        Some(_) => {
            eprintln!("{USAGE}");
            ExitCode::from(2)
        }
    }
}

/// Run `future` on a multi-thread runtime (`threads` workers, or one per
/// core).
fn on_runtime(threads: Option<usize>, future: impl Future<Output = ExitCode>) -> ExitCode {
    let mut builder = tokio::runtime::Builder::new_multi_thread();
    builder.enable_all();
    if let Some(threads) = threads {
        builder.worker_threads(threads.max(1));
    }
    match builder.build() {
        Ok(runtime) => {
            let code = runtime.block_on(future);
            // A blocking read never ends on its own (the worker's stdin
            // stays open while its parent lives): do not wait for it.
            runtime.shutdown_background();
            code
        }
        Err(error) => {
            eprintln!("cannot start the async runtime: {error}");
            ExitCode::FAILURE
        }
    }
}

fn settings() -> Option<Settings> {
    match Settings::from_env() {
        Ok(settings) => Some(settings),
        Err(error) => {
            eprintln!("{error}");
            None
        }
    }
}

async fn healthcheck() -> ExitCode {
    let Some(settings) = settings() else {
        return ExitCode::FAILURE;
    };
    match healthcheck::probe(&settings.engine_socket, Duration::from_secs(2)).await {
        Ok(()) => ExitCode::SUCCESS,
        Err(error) => {
            eprintln!("{error}");
            ExitCode::FAILURE
        }
    }
}

/// The OTLP `service.name` of this engine's spans (the server and the
/// worker child alike).
const SERVICE_NAME: &str = "elitea-deepwiki-engine";

/// `python -m elitea_deepwiki.storage`: apply the service migrations to
/// `ELITEA_DEEPWIKI_DATABASE_URL`. Exit 0 when the database is at the
/// newest migration (whether or not this run applied one), 1 otherwise.
/// It reads only that variable, as the Python entry point does, so a
/// migration Job needs no runner settings.
async fn migrate() -> ExitCode {
    let telemetry = telemetry::init(SERVICE_NAME);
    let code = migrate_database().await;
    telemetry.shutdown().await;
    code
}

async fn migrate_database() -> ExitCode {
    let dsn = std::env::var(storage::DSN_ENV).unwrap_or_default();
    if dsn.trim().is_empty() {
        tracing::error!(
            "{} is not set, so there is no database to migrate. It must name the same database the service itself connects to.",
            storage::DSN_ENV
        );
        return ExitCode::FAILURE;
    }
    let pool = match storage::lazy_pool(&dsn, 1) {
        Ok(pool) => pool,
        Err(error) => {
            tracing::error!(%error, "cannot migrate");
            return ExitCode::FAILURE;
        }
    };
    let outcome = storage::migrate::apply_all(&pool).await;
    pool.close().await;
    match outcome {
        Ok(applied) if applied.is_empty() => {
            tracing::info!("the database is already at the newest migration");
            ExitCode::SUCCESS
        }
        Ok(applied) => {
            tracing::info!(
                "applied {} migration(s): {}",
                applied.len(),
                applied.join(", ")
            );
            ExitCode::SUCCESS
        }
        Err(error) => {
            tracing::error!(%error, "migration failed");
            ExitCode::FAILURE
        }
    }
}

/// The ids in `text`: one per line, `#` comments and blank lines ignored.
/// A line that is not a positive project id is an error, never skipped: a
/// skipped line would make that project look deleted.
fn parse_project_ids(text: &str) -> Result<std::collections::HashSet<i32>, String> {
    let mut ids = std::collections::HashSet::new();
    for (number, line) in text.lines().enumerate() {
        let line = line.split('#').next().unwrap_or_default().trim();
        if line.is_empty() {
            continue;
        }
        match line.parse::<i32>() {
            Ok(id) if id > 0 => {
                ids.insert(id);
            }
            _ => return Err(format!("line {}: {line:?} is not a project id", number + 1)),
        }
    }
    Ok(ids)
}

/// The arguments of `orphans`.
#[derive(Debug, PartialEq, Eq)]
struct OrphanArgs {
    source: String,
    delete: bool,
    listed_at: Option<String>,
    allow_stale_list: bool,
}

/// Whether `text` is an RFC 3339 timestamp in the strict form
/// `YYYY-MM-DDTHH:MM:SS[.fraction](Z|+HH:MM|-HH:MM)`. The database checks the
/// calendar (month 13 is refused there); this checks the shape, so that a
/// date like `yesterday` or `10/10/2026` that PostgreSQL would also read is
/// refused before it is believed.
fn is_rfc3339(text: &str) -> bool {
    let bytes = text.as_bytes();
    let digits = |range: std::ops::Range<usize>| {
        bytes
            .get(range)
            .is_some_and(|b| b.iter().all(u8::is_ascii_digit))
    };
    let at = |index: usize, expected: u8| bytes.get(index) == Some(&expected);
    if !(digits(0..4)
        && at(4, b'-')
        && digits(5..7)
        && at(7, b'-')
        && digits(8..10)
        && at(10, b'T')
        && digits(11..13)
        && at(13, b':')
        && digits(14..16)
        && at(16, b':')
        && digits(17..19))
    {
        return false;
    }
    let mut rest = 19;
    if at(rest, b'.') {
        let start = rest + 1;
        let mut end = start;
        while bytes.get(end).is_some_and(u8::is_ascii_digit) {
            end += 1;
        }
        if end == start {
            return false;
        }
        rest = end;
    }
    match bytes.get(rest) {
        Some(b'Z') => rest + 1 == bytes.len(),
        Some(b'+' | b'-') => {
            digits(rest + 1..rest + 3)
                && at(rest + 3, b':')
                && digits(rest + 4..rest + 6)
                && rest + 6 == bytes.len()
        }
        _ => false,
    }
}

fn parse_orphan_args(args: Vec<String>) -> Result<OrphanArgs, String> {
    let mut source = None;
    let mut delete = false;
    let mut listed_at = None;
    let mut allow_stale_list = false;
    let mut iter = args.into_iter();
    while let Some(arg) = iter.next() {
        match arg.as_str() {
            "--existing-projects" => source = iter.next(),
            "--delete" => delete = true,
            "--listed-at" => listed_at = iter.next(),
            "--allow-stale-list" => allow_stale_list = true,
            _ => return Err(USAGE.to_owned()),
        }
    }
    let Some(source) = source else {
        return Err(USAGE.to_owned());
    };
    if let Some(at) = &listed_at
        && !is_rfc3339(at)
    {
        return Err(format!(
            "--listed-at {at:?} is not an RFC 3339 time such as 2026-10-10T09:30:00Z"
        ));
    }
    if delete && listed_at.is_none() {
        return Err("--delete needs --listed-at <RFC3339>: the time the list of existing projects was taken. A project created after it is not in the list and would look orphaned; the command skips every project with a wiki or build newer than that time".to_owned());
    }
    if allow_stale_list && !delete {
        return Err("--allow-stale-list only applies with --delete".to_owned());
    }
    Ok(OrphanArgs {
        source,
        delete,
        listed_at,
        allow_stale_list,
    })
}

/// `orphans --existing-projects FILE|- [--listed-at T] [--delete --listed-at T [--allow-stale-list]]`:
/// list the projects that have indexed wikis here and no longer exist (issue
/// #1243). The existing ids come from the product database, which this
/// service cannot read, and the time the list was taken is `T`:
///
/// ```text
/// T=$(date -u +%Y-%m-%dT%H:%M:%SZ)
/// psql "$PRODUCT_DB" -Atc 'SELECT id FROM centry.project' | elitea-deepwiki-engine orphans --existing-projects - --listed-at "$T" --delete
/// ```
///
/// A dry run unless `--delete` is given. An empty id list is refused: it
/// would call every project an orphan. `--delete` needs `--listed-at`, refuses
/// a list older than 10 minutes unless `--allow-stale-list`, and skips (and
/// prints) every project with a wiki or build created or published after
/// that time. The publish and staleness settings are the server's
/// (`ELITEA_DEEPWIKI_PUBLISH_*`, `ELITEA_DEEPWIKI_BUILD_STALE_SECONDS`).
/// The exit code is non-zero if any project's deletion failed or did not
/// finish.
async fn orphans(args: Vec<String>) -> ExitCode {
    let args = match parse_orphan_args(args) {
        Ok(args) => args,
        Err(message) => {
            eprintln!("{message}");
            return ExitCode::from(2);
        }
    };
    let settings = match elitea_deepwiki_engine::config::MaintenanceSettings::from_env() {
        Ok(settings) => settings,
        Err(error) => {
            eprintln!("{error}");
            return ExitCode::FAILURE;
        }
    };
    let source = &args.source;
    let text = if source == "-" {
        let mut text = String::new();
        match std::io::Read::read_to_string(&mut std::io::stdin(), &mut text) {
            Ok(_) => text,
            Err(error) => {
                eprintln!("cannot read the project ids from stdin: {error}");
                return ExitCode::FAILURE;
            }
        }
    } else {
        match std::fs::read_to_string(source) {
            Ok(text) => text,
            Err(error) => {
                eprintln!("cannot read {source}: {error}");
                return ExitCode::FAILURE;
            }
        }
    };
    let existing = match parse_project_ids(&text) {
        Ok(ids) if !ids.is_empty() => ids,
        Ok(_) => {
            eprintln!(
                "the list of existing projects is empty; refusing to call every project an orphan"
            );
            return ExitCode::FAILURE;
        }
        Err(error) => {
            eprintln!("{error}");
            return ExitCode::FAILURE;
        }
    };
    let dsn = std::env::var(storage::DSN_ENV).unwrap_or_default();
    if dsn.trim().is_empty() {
        eprintln!("{} is not set", storage::DSN_ENV);
        return ExitCode::FAILURE;
    }
    let pool = match storage::lazy_pool(&dsn, 2) {
        Ok(pool) => pool,
        Err(error) => {
            eprintln!("{error}");
            return ExitCode::FAILURE;
        }
    };
    let code = report_orphans(&pool, &existing, &args, &settings).await;
    pool.close().await;
    code
}

async fn report_orphans(
    pool: &sqlx::PgPool,
    existing: &std::collections::HashSet<i32>,
    args: &OrphanArgs,
    settings: &elitea_deepwiki_engine::config::MaintenanceSettings,
) -> ExitCode {
    let options = storage::delete::SweepOptions {
        listed_at: args.listed_at.as_deref(),
        delete: args.delete,
        allow_stale_list: args.allow_stale_list,
    };
    let limits = storage::delete::ProjectLimits::new(settings.build_stale_after);
    let outcome =
        match storage::delete::sweep_orphans(pool, existing, &options, &settings.publish, &limits)
            .await
        {
            Ok(outcome) => outcome,
            Err(error) => {
                eprintln!("{error}");
                return ExitCode::FAILURE;
            }
        };
    if outcome.deleted.is_empty()
        && outcome.would_delete.is_empty()
        && outcome.skipped.is_empty()
        && outcome.failed.is_empty()
    {
        println!("no orphaned wiki index");
        return ExitCode::SUCCESS;
    }
    println!("project_id\twikis\tlast_updated\tstatus");
    for project in &outcome.would_delete {
        println!(
            "{}\t{}\t{}\twould delete",
            project.project_id, project.wikis, project.last_updated
        );
    }
    for (project, reason) in &outcome.skipped {
        println!(
            "{}\t{}\t{}\tSKIPPED: {reason}",
            project.project_id, project.wikis, project.last_updated
        );
    }
    for (project, done) in &outcome.deleted {
        println!(
            "{}\t{}\t{}\tdeleted {} wiki(s), {} node(s), {} stale build(s); {} live build(s) left to their run",
            project.project_id,
            project.wikis,
            project.last_updated,
            done.wikis.len(),
            done.rows.nodes,
            done.builds,
            done.live_builds
        );
    }
    for (project, error) in &outcome.failed {
        eprintln!("project {}: {error}", project.project_id);
    }
    if !args.delete {
        println!(
            "dry run: {} orphaned project(s) would be deleted, {} skipped; pass --delete with --listed-at to remove their indexes",
            outcome.would_delete.len(),
            outcome.skipped.len()
        );
    } else if !outcome.skipped.is_empty() {
        println!(
            "{} project(s) skipped: a wiki or build newer than the list; run again with a fresh list",
            outcome.skipped.len()
        );
    }
    if outcome.failed.is_empty() {
        ExitCode::SUCCESS
    } else {
        eprintln!(
            "{} project(s) were not deleted completely",
            outcome.failed.len()
        );
        ExitCode::FAILURE
    }
}

/// With a database configured: reconcile the abandoned builds of this
/// owner's earlier runs, then sweep stale ones periodically, in the
/// background. `Settings` already refused the shared default owner.
fn start_reconciler(settings: &Settings) {
    let Some(url) = settings.database_url.as_ref() else {
        return;
    };
    match storage::lazy_pool(url.expose(), 2) {
        Ok(pool) => {
            // The dead 'bm25' statistics branch, removed in bounded
            // statements in the background (migration 0007 is a no-op).
            tokio::spawn(storage::cleanup::run(
                pool.clone(),
                storage::cleanup::Pacing::default(),
            ));
            let space = storage::build::BuildSpace::new(pool, settings.build_owner.clone())
                .with_stale_after(settings.build_stale_after)
                .with_publish_settings(settings.publish);
            tracing::info!(
                owner = %space.owner(),
                boot_id = %space.boot_id(),
                "build reconciliation: this run's builds are recorded under this owner and boot id"
            );
            tokio::spawn(storage::build::run_reconciler(
                space,
                settings.build_stale_after,
            ));
        }
        Err(error) => tracing::error!(%error, "no build reconciliation"),
    }
}

async fn serve() -> ExitCode {
    let telemetry = telemetry::init(SERVICE_NAME);
    let code = serve_engine().await;
    telemetry.shutdown().await;
    code
}

async fn serve_engine() -> ExitCode {
    let Some(settings) = settings() else {
        return ExitCode::FAILURE;
    };
    let runner = match build_runner(&settings) {
        Ok(runner) => runner,
        Err(error) => {
            tracing::error!(%error, "cannot start the runner");
            return ExitCode::FAILURE;
        }
    };
    if let elitea_deepwiki_engine::runner::Runner::Native(native) = &runner {
        native.remove_stale_jobs();
    }
    start_reconciler(&settings);
    let listener = match server::bind(&settings.engine_socket) {
        Ok(listener) => listener,
        Err(error) => {
            tracing::error!(socket = %settings.engine_socket.display(), %error, "cannot bind the engine socket");
            return ExitCode::FAILURE;
        }
    };
    tracing::info!(socket = %settings.engine_socket.display(), runner = runner.name(), "engine sidecar listening");
    match server::serve(listener, runner, shutdown_signal()).await {
        Ok(()) => ExitCode::SUCCESS,
        Err(error) => {
            tracing::error!(%error, "engine sidecar stopped");
            ExitCode::FAILURE
        }
    }
}

async fn shutdown_signal() {
    let interrupt = tokio::signal::ctrl_c();
    #[cfg(unix)]
    {
        let terminate = async {
            match tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate()) {
                Ok(mut signal) => {
                    signal.recv().await;
                }
                Err(_) => std::future::pending::<()>().await,
            }
        };
        tokio::select! {
            _ = interrupt => {}
            () = terminate => {}
        }
    }
    #[cfg(not(unix))]
    {
        let _ = interrupt.await;
    }
}

#[cfg(test)]
mod tests {
    use super::{is_rfc3339, parse_orphan_args, parse_project_ids};

    fn args(list: &[&str]) -> Vec<String> {
        list.iter().map(|a| (*a).to_owned()).collect()
    }

    #[test]
    fn delete_needs_a_listing_time() {
        let refused = parse_orphan_args(args(&["--existing-projects", "-", "--delete"]));
        assert!(
            matches!(&refused, Err(m) if m.contains("--listed-at")),
            "{refused:?}"
        );
        let accepted = parse_orphan_args(args(&[
            "--existing-projects",
            "ids.txt",
            "--delete",
            "--listed-at",
            "2026-10-10T09:30:00Z",
        ]));
        assert_eq!(
            accepted.map(|a| (a.delete, a.listed_at, a.allow_stale_list)),
            Ok((true, Some("2026-10-10T09:30:00Z".to_owned()), false))
        );
        // A dry run does not need one.
        assert!(parse_orphan_args(args(&["--existing-projects", "-"])).is_ok());
    }

    #[test]
    fn the_stale_override_is_only_for_a_deletion_and_the_time_is_rfc3339() {
        let stale_dry =
            parse_orphan_args(args(&["--existing-projects", "-", "--allow-stale-list"]));
        assert!(stale_dry.is_err());
        let stale = parse_orphan_args(args(&[
            "--existing-projects",
            "-",
            "--delete",
            "--listed-at",
            "2026-10-10T09:30:00+02:00",
            "--allow-stale-list",
        ]));
        assert!(matches!(stale, Ok(a) if a.allow_stale_list));
        for bad in [
            "yesterday",
            "2026-10-10",
            "2026-10-10 09:30:00",
            "2026-10-10T09:30:00",
            "2026-10-10T09:30:00+0200",
            "10/10/2026",
            "",
        ] {
            assert!(!is_rfc3339(bad), "{bad:?}");
            assert!(
                parse_orphan_args(args(&["--existing-projects", "-", "--listed-at", bad])).is_err(),
                "{bad:?}"
            );
        }
        for good in [
            "2026-10-10T09:30:00Z",
            "2026-10-10T09:30:00.123456Z",
            "2026-10-10T09:30:00-05:00",
        ] {
            assert!(is_rfc3339(good), "{good:?}");
        }
        assert!(parse_orphan_args(args(&["--bogus"])).is_err());
        assert!(parse_orphan_args(args(&[])).is_err());
    }

    #[test]
    fn project_ids_parse_one_per_line() {
        let ids = parse_project_ids("1\n  2 # two\n\n# note\n30\n");
        assert_eq!(ids.map(|s| s.len()), Ok(3));
    }

    #[test]
    fn a_bad_line_is_an_error_not_a_skip() {
        for bad in ["abc", "0", "-4", "1 2"] {
            assert!(parse_project_ids(bad).is_err(), "{bad}");
        }
    }
}
