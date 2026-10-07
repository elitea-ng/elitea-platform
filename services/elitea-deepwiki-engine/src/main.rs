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

const USAGE: &str =
    "usage: elitea-deepwiki-engine [serve | healthcheck | migrate | worker | --version]";

fn main() -> ExitCode {
    let command = std::env::args().nth(1);
    match command.as_deref() {
        None | Some("serve") => on_runtime(None, serve()),
        Some("healthcheck") => on_runtime(None, healthcheck()),
        Some("migrate") => on_runtime(None, migrate()),
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
