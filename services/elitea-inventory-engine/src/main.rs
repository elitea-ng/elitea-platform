//! `elitea-inventory-engine [serve | healthcheck | migrate | import-graph |
//! export-graph | --version]`.

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

use elitea_engine_sidecar::{healthcheck, server, telemetry};
use elitea_inventory_engine::build_runner;
use elitea_inventory_engine::config::Settings;
use elitea_inventory_engine::store::{self, GraphKey};
use elitea_inventory_engine::transfer;
use std::process::ExitCode;
use std::time::Duration;

const USAGE: &str = "usage: elitea-inventory-engine [serve | healthcheck | migrate | --version]
       elitea-inventory-engine import-graph --project-id N --application-id N [--file PATH | -] [--replace-ingestion-state]
       elitea-inventory-engine export-graph --project-id N --application-id N [--file PATH | -]";

/// The OTLP `service.name` of this engine's spans.
const SERVICE_NAME: &str = "elitea-inventory-engine";

fn main() -> ExitCode {
    match std::env::args().nth(1).as_deref() {
        None | Some("serve") => on_runtime(serve()),
        Some("healthcheck") => on_runtime(probe()),
        Some("migrate") => on_runtime(migrate()),
        Some(command @ ("import-graph" | "export-graph")) => {
            let arguments: Vec<String> = std::env::args().skip(2).collect();
            match GraphArguments::parse(command, &arguments) {
                Ok(parsed) => on_runtime(transfer(parsed)),
                Err(error) => {
                    eprintln!("{error}\n{USAGE}");
                    ExitCode::from(2)
                }
            }
        }
        Some("--version") => {
            println!("elitea-inventory-engine {}", env!("CARGO_PKG_VERSION"));
            ExitCode::SUCCESS
        }
        Some(_) => {
            eprintln!("{USAGE}");
            ExitCode::from(2)
        }
    }
}

fn on_runtime(future: impl Future<Output = ExitCode>) -> ExitCode {
    match tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
    {
        Ok(runtime) => {
            let code = runtime.block_on(future);
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

async fn probe() -> ExitCode {
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

/// Apply the graph-store migrations to `ELITEA_INVENTORY_DATABASE_URL`
/// (the `DeepWiki` engine's `migrate`, for this engine's schema). Exit 0 when
/// the database is at the newest migration, whether or not this run applied
/// one. It reads only that variable, so a migration Job needs no runner
/// settings.
async fn migrate() -> ExitCode {
    let telemetry = telemetry::init(SERVICE_NAME);
    let code = migrate_database().await;
    telemetry.shutdown().await;
    code
}

async fn migrate_database() -> ExitCode {
    let dsn = std::env::var(store::DSN_ENV).unwrap_or_default();
    if dsn.trim().is_empty() {
        tracing::error!(
            "{} is not set, so there is no database to migrate",
            store::DSN_ENV
        );
        return ExitCode::FAILURE;
    }
    let pool = match store::lazy_pool(&dsn, 1) {
        Ok(pool) => pool,
        Err(error) => {
            tracing::error!(%error, "cannot migrate");
            return ExitCode::FAILURE;
        }
    };
    let outcome = store::migrate(&pool).await;
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
    let listener = match server::bind(&settings.engine_socket) {
        Ok(listener) => listener,
        Err(error) => {
            tracing::error!(socket = %settings.engine_socket.display(), %error, "cannot bind the engine socket");
            return ExitCode::FAILURE;
        }
    };
    tracing::info!(socket = %settings.engine_socket.display(), runner = runner.name(), "inventory engine sidecar listening");
    match server::serve(listener, runner, shutdown_signal()).await {
        Ok(()) => ExitCode::SUCCESS,
        Err(error) => {
            tracing::error!(%error, "inventory engine sidecar stopped");
            ExitCode::FAILURE
        }
    }
}

/// SIGTERM (the kubelet) or Ctrl-C.
async fn shutdown_signal() {
    let terminate = async {
        match tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate()) {
            Ok(mut signal) => {
                signal.recv().await;
            }
            Err(_) => std::future::pending::<()>().await,
        }
    };
    tokio::select! {
        _ = tokio::signal::ctrl_c() => {}
        () = terminate => {}
    }
}

/// The arguments of `import-graph` / `export-graph`.
#[derive(Debug, Clone, PartialEq, Eq)]
struct GraphArguments {
    import: bool,
    key: GraphKey,
    /// `None`: standard input / output (`-` or no `--file`).
    file: Option<String>,
    replace_state: bool,
}

impl GraphArguments {
    fn parse(command: &str, arguments: &[String]) -> Result<Self, String> {
        let import = command == "import-graph";
        let (mut project, mut application, mut file, mut replace_state) = (None, None, None, false);
        let mut rest = arguments.iter();
        while let Some(argument) = rest.next() {
            let (flag, inline) = match argument.split_once('=') {
                Some((flag, value)) if flag.starts_with("--") => (flag, Some(value.to_owned())),
                _ => (argument.as_str(), None),
            };
            let mut value = || {
                inline
                    .clone()
                    .or_else(|| rest.next().cloned())
                    .ok_or_else(|| format!("{flag} needs a value"))
            };
            match flag {
                "--project-id" => project = Some(value()?),
                "--application-id" | "--toolkit-id" => application = Some(value()?),
                "--file" => file = Some(value()?),
                "--replace-ingestion-state" if import && inline.is_none() => replace_state = true,
                "-" if inline.is_none() => file = Some("-".to_owned()),
                other => return Err(format!("{command}: unknown argument {other}")),
            }
        }
        let id = |name: &str, value: Option<String>| -> Result<i64, String> {
            value
                .ok_or_else(|| format!("{command} needs {name}"))?
                .trim()
                .parse::<i64>()
                .map_err(|_| format!("{name} must be an integer"))
        };
        let key = GraphKey::new(
            id("--project-id", project)?,
            id("--application-id", application)?,
        )
        .map_err(|e| e.to_string())?;
        Ok(Self {
            import,
            key,
            file: file.filter(|path| path != "-"),
            replace_state,
        })
    }
}

/// `import-graph` / `export-graph` against `ELITEA_INVENTORY_DATABASE_URL`
/// (migrated first, so an import needs no separate `migrate`). Reports go
/// to standard error; an export to standard output is the document only.
async fn transfer(arguments: GraphArguments) -> ExitCode {
    let dsn = std::env::var(store::DSN_ENV).unwrap_or_default();
    if dsn.trim().is_empty() {
        eprintln!("{} is not set, so there is no graph store", store::DSN_ENV);
        return ExitCode::FAILURE;
    }
    let pool = match store::lazy_pool(&dsn, 2) {
        Ok(pool) => pool,
        Err(error) => {
            eprintln!("{error}");
            return ExitCode::FAILURE;
        }
    };
    let code = run_transfer(&pool, &arguments).await;
    pool.close().await;
    code
}

async fn run_transfer(pool: &sqlx::PgPool, arguments: &GraphArguments) -> ExitCode {
    let key = arguments.key;
    if let Err(error) = store::migrate(pool).await {
        eprintln!("migration failed: {error}");
        return ExitCode::FAILURE;
    }
    if arguments.import {
        let text = match &arguments.file {
            Some(path) => {
                std::fs::read_to_string(path).map_err(|e| format!("cannot read {path}: {e}"))
            }
            None => std::io::read_to_string(std::io::stdin())
                .map_err(|e| format!("cannot read standard input: {e}")),
        };
        let text = match text {
            Ok(text) => text,
            Err(error) => {
                eprintln!("{error}");
                return ExitCode::FAILURE;
            }
        };
        return match transfer::import_graph(pool, key, &text, arguments.replace_state).await {
            Ok(report) => {
                eprintln!(
                    "imported {} entities and {} relations into project {}, toolkit {} (revision {}){}",
                    report.entities,
                    report.relations,
                    key.project_id,
                    key.application_id,
                    report.revision,
                    report.embeddings_model.map_or_else(String::new, |model| format!(
                        "; embedded with {model}: semantic search needs that embedding model configured"
                    ))
                );
                ExitCode::SUCCESS
            }
            Err(error) => {
                eprintln!("{error}");
                ExitCode::FAILURE
            }
        };
    }
    let text =
        match transfer::export_graph(pool, key, &elitea_inventory_engine::clock::now_iso()).await {
            Ok(text) => text,
            Err(error) => {
                eprintln!("{error}");
                return ExitCode::FAILURE;
            }
        };
    let written = if let Some(path) = &arguments.file {
        std::fs::write(path, &text).map_err(|e| format!("cannot write {path}: {e}"))
    } else {
        use std::io::Write as _;
        std::io::stdout()
            .lock()
            .write_all(text.as_bytes())
            .map_err(|e| format!("cannot write standard output: {e}"))
    };
    match written {
        Ok(()) => ExitCode::SUCCESS,
        Err(error) => {
            eprintln!("{error}");
            ExitCode::FAILURE
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn parse(command: &str, arguments: &[&str]) -> Result<GraphArguments, String> {
        let owned: Vec<String> = arguments.iter().map(|a| (*a).to_owned()).collect();
        GraphArguments::parse(command, &owned)
    }

    #[test]
    fn graph_arguments_parse_and_refuse() {
        let parsed = parse(
            "import-graph",
            &[
                "--project-id",
                "3",
                "--application-id=42",
                "--file",
                "g.json",
                "--replace-ingestion-state",
            ],
        );
        assert_eq!(
            parsed,
            Ok(GraphArguments {
                import: true,
                key: GraphKey::new(3, 42).expect("key"),
                file: Some("g.json".to_owned()),
                replace_state: true,
            })
        );
        let stdin = parse(
            "export-graph",
            &["--project-id", "3", "--toolkit-id", "4", "-"],
        );
        assert_eq!(stdin.map(|a| (a.import, a.file)), Ok((false, None)));
        for (command, arguments, needle) in [
            (
                "import-graph",
                &["--project-id", "3"][..],
                "needs --application-id",
            ),
            (
                "import-graph",
                &["--project-id", "x", "--application-id", "1"][..],
                "must be an integer",
            ),
            (
                "import-graph",
                &["--project-id", "0", "--application-id", "1"][..],
                "positive",
            ),
            (
                "export-graph",
                &[
                    "--project-id",
                    "1",
                    "--application-id",
                    "1",
                    "--replace-ingestion-state",
                ][..],
                "unknown argument",
            ),
            ("export-graph", &["--file"][..], "--file needs a value"),
        ] {
            let refused = parse(command, arguments);
            assert!(
                refused.as_ref().is_err_and(|e| e.contains(needle)),
                "{arguments:?}: {refused:?}"
            );
        }
    }
}
