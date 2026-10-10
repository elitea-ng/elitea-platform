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

const USAGE: &str = "usage: elitea-deepwiki-engine [serve | healthcheck | migrate | worker | orphans --existing-projects FILE|- [--delete] | --version]";

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

/// `orphans --existing-projects FILE|- [--delete]`: list the projects that
/// have indexed wikis here and no longer exist (issue #1243). The existing
/// ids come from the product database, which this service cannot read:
///
/// ```text
/// psql "$PRODUCT_DB" -Atc 'SELECT id FROM centry.project' | elitea-deepwiki-engine orphans --existing-projects -
/// ```
///
/// A dry run unless `--delete` is given. An empty id list is refused: it
/// would call every project an orphan.
async fn orphans(args: Vec<String>) -> ExitCode {
    let mut source = None;
    let mut delete = false;
    let mut iter = args.into_iter();
    while let Some(arg) = iter.next() {
        match arg.as_str() {
            "--existing-projects" => source = iter.next(),
            "--delete" => delete = true,
            _ => {
                eprintln!("{USAGE}");
                return ExitCode::from(2);
            }
        }
    }
    let Some(source) = source else {
        eprintln!("{USAGE}");
        return ExitCode::from(2);
    };
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
        match std::fs::read_to_string(&source) {
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
    let code = report_orphans(&pool, &existing, delete).await;
    pool.close().await;
    code
}

async fn report_orphans(
    pool: &sqlx::PgPool,
    existing: &std::collections::HashSet<i32>,
    delete: bool,
) -> ExitCode {
    let found = match storage::delete::orphans(pool, existing).await {
        Ok(found) => found,
        Err(error) => {
            eprintln!("cannot list the orphans: {error}");
            return ExitCode::FAILURE;
        }
    };
    if found.is_empty() {
        println!("no orphaned wiki index");
        return ExitCode::SUCCESS;
    }
    println!("project_id\twikis\tlast_updated");
    for project in &found {
        println!(
            "{}\t{}\t{}",
            project.project_id, project.wikis, project.last_updated
        );
    }
    if !delete {
        println!(
            "dry run: {} orphaned project(s); pass --delete to remove their indexes",
            found.len()
        );
        return ExitCode::SUCCESS;
    }
    let settings = storage::build::PublishSettings::default();
    let mut failed = false;
    for project in found {
        let Some(scope) = storage::ProjectScope::new(project.project_id) else {
            continue;
        };
        match storage::delete::delete_project(pool, scope, &settings).await {
            Ok(done) => println!(
                "deleted project {}: {} wiki(s), {} node(s)",
                project.project_id,
                done.wikis.len(),
                done.rows.nodes
            ),
            Err(error) => {
                eprintln!("project {}: {error}", project.project_id);
                failed = true;
            }
        }
    }
    if failed {
        ExitCode::FAILURE
    } else {
        ExitCode::SUCCESS
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
    use super::parse_project_ids;

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
