//! `elitea-inventory-engine [serve | healthcheck | migrate | import-graph |
//! export-graph | orphans | --version]`.

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
       elitea-inventory-engine export-graph --project-id N --application-id N [--file PATH | -]
       elitea-inventory-engine orphans --existing-projects FILE|- [--existing-toolkits FILE] [--listed-at RFC3339] [--delete --listed-at RFC3339 [--allow-stale-list]]";

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
        Some("orphans") => {
            let arguments: Vec<String> = std::env::args().skip(2).collect();
            match OrphanArguments::parse(&arguments) {
                Ok(parsed) => on_runtime(orphans(parsed)),
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
    match server::serve_with_limit(
        listener,
        runner,
        elitea_inventory_engine::MAX_INVOKE_BYTES,
        shutdown_signal(),
    )
    .await
    {
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
    // An import writes, so it brings the schema up to date first. An export
    // only reads: it must not change the database it reads, so it checks
    // the migration ledger and refuses a missing or older schema.
    if arguments.import {
        if let Err(error) = store::migrate(pool).await {
            eprintln!("migration failed: {error}");
            return ExitCode::FAILURE;
        }
    } else {
        match store::schema_gap(pool).await {
            Ok(None) => {}
            Ok(Some(gap)) => {
                eprintln!(
                    "{gap}; export-graph does not migrate. Start the engine (or run import-graph) against this database to apply its migrations, then export again"
                );
                return ExitCode::FAILURE;
            }
            Err(error) => {
                eprintln!("cannot read the migration ledger: {error}");
                return ExitCode::FAILURE;
            }
        }
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

/// The arguments of `orphans`.
#[derive(Debug, Clone, PartialEq, Eq)]
struct OrphanArguments {
    /// The file of existing project ids; `-` is standard input.
    projects: String,
    /// The file of existing `project_id toolkit_id` pairs, if given.
    toolkits: Option<String>,
    delete: bool,
    /// When the lists of existing ids were taken (RFC 3339).
    listed_at: Option<String>,
    /// Accept a list older than ten minutes.
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

impl OrphanArguments {
    fn parse(arguments: &[String]) -> Result<Self, String> {
        let (mut projects, mut toolkits, mut delete) = (None, None, false);
        let (mut listed_at, mut allow_stale_list) = (None, false);
        let mut rest = arguments.iter();
        while let Some(argument) = rest.next() {
            match argument.as_str() {
                "--existing-projects" => {
                    projects = Some(
                        rest.next()
                            .ok_or("--existing-projects needs a value")?
                            .clone(),
                    );
                }
                "--existing-toolkits" => {
                    toolkits = Some(
                        rest.next()
                            .ok_or("--existing-toolkits needs a value")?
                            .clone(),
                    );
                }
                "--delete" => delete = true,
                "--listed-at" => {
                    listed_at = Some(rest.next().ok_or("--listed-at needs a value")?.clone());
                }
                "--allow-stale-list" => allow_stale_list = true,
                other => return Err(format!("orphans: unknown argument {other}")),
            }
        }
        let projects = projects.ok_or("orphans needs --existing-projects")?;
        if toolkits.as_deref() == Some("-") && projects == "-" {
            return Err("orphans: only one list can be read from standard input".to_owned());
        }
        if let Some(at) = &listed_at
            && !is_rfc3339(at)
        {
            return Err(format!(
                "--listed-at {at:?} is not an RFC 3339 time such as 2026-10-10T09:30:00Z"
            ));
        }
        if delete && listed_at.is_none() {
            return Err("orphans: --delete needs --listed-at <RFC3339>: the time the lists of existing projects and toolkits were taken. A project or toolkit created after it is not in the lists and would look orphaned; the command skips every graph written after that time".to_owned());
        }
        if allow_stale_list && !delete {
            return Err("orphans: --allow-stale-list only applies with --delete".to_owned());
        }
        Ok(Self {
            projects,
            toolkits,
            delete,
            listed_at,
            allow_stale_list,
        })
    }
}

/// The positive ids in `text`, one per line (`#` comments and blank lines
/// ignored). A line that is not one is an error, never skipped: a skipped
/// line would make that project look deleted.
fn parse_project_ids(text: &str) -> Result<std::collections::HashSet<i64>, String> {
    let mut ids = std::collections::HashSet::new();
    for (number, line) in text.lines().enumerate() {
        let line = line.split('#').next().unwrap_or_default().trim();
        if line.is_empty() {
            continue;
        }
        match line.parse::<i64>() {
            Ok(id) if id > 0 => {
                ids.insert(id);
            }
            _ => return Err(format!("line {}: {line:?} is not a project id", number + 1)),
        }
    }
    Ok(ids)
}

/// The `project_id toolkit_id` pairs in `text`, one per line, separated by
/// whitespace or a comma. Strict, as [`parse_project_ids`].
fn parse_toolkit_pairs(text: &str) -> Result<std::collections::HashSet<(i64, i64)>, String> {
    let mut pairs = std::collections::HashSet::new();
    for (number, line) in text.lines().enumerate() {
        let line = line.split('#').next().unwrap_or_default().trim();
        if line.is_empty() {
            continue;
        }
        let fields: Vec<&str> = line
            .split(|c: char| c == ',' || c.is_whitespace())
            .filter(|field| !field.is_empty())
            .collect();
        let ids = match fields.as_slice() {
            [project, toolkit] => project.parse::<i64>().ok().zip(toolkit.parse::<i64>().ok()),
            _ => None,
        };
        match ids {
            Some((project, toolkit)) if project > 0 && toolkit > 0 => {
                pairs.insert((project, toolkit));
            }
            _ => {
                return Err(format!(
                    "line {}: {line:?} is not a project id and a toolkit id",
                    number + 1
                ));
            }
        }
    }
    Ok(pairs)
}

fn read_list(source: &str) -> Result<String, String> {
    if source == "-" {
        std::io::read_to_string(std::io::stdin())
            .map_err(|e| format!("cannot read standard input: {e}"))
    } else {
        std::fs::read_to_string(source).map_err(|e| format!("cannot read {source}: {e}"))
    }
}

/// `orphans`: list the Inventory graphs whose project (or toolkit) no longer
/// exists (issue #1244). The platform's projects are not in this database,
/// so the caller supplies the ids that DO exist:
///
/// ```text
/// psql "$PRODUCT_DB" -Atc 'SELECT id FROM centry.project' \
///     | elitea-inventory-engine orphans --existing-projects -
/// ```
///
/// A dry run unless `--delete`. An empty project list is refused: it would
/// call every graph an orphan. With `--existing-toolkits FILE` (lines of
/// `project_id toolkit_id`) a graph of a live project whose toolkit is gone
/// is listed too; without it only graphs of deleted projects are, because
/// the engine cannot tell that a toolkit is gone.
///
/// A project or toolkit created after the lists were taken is not in them
/// and would look orphaned. So `--delete` needs `--listed-at <RFC3339>`, the
/// time the lists were taken, refuses lists older than 10 minutes unless
/// `--allow-stale-list`, and skips (and prints) every graph written or
/// ingested after that time. The check is made again immediately before EACH
/// graph's deletion, inside its transaction and under its lock. The
/// database is the server's: `ELITEA_INVENTORY_DATABASE_URL`, read the way
/// `serve` reads it. The exit code is non-zero if any deletion failed.
async fn orphans(arguments: OrphanArguments) -> ExitCode {
    let parsed = read_list(&arguments.projects)
        .and_then(|text| parse_project_ids(&text))
        .and_then(
            |projects| match arguments.toolkits.as_deref().map(read_list) {
                None => Ok((projects, None)),
                Some(text) => text
                    .and_then(|text| parse_toolkit_pairs(&text))
                    .map(|toolkits| (projects, Some(toolkits))),
            },
        );
    let (projects, toolkits) = match parsed {
        Ok((projects, _)) if projects.is_empty() => {
            eprintln!(
                "the list of existing projects is empty; refusing to call every graph an orphan"
            );
            return ExitCode::FAILURE;
        }
        Ok(lists) => lists,
        Err(error) => {
            eprintln!("{error}");
            return ExitCode::FAILURE;
        }
    };
    // The server's own settings, so the database is the one `serve` uses.
    let Some(settings) = settings() else {
        return ExitCode::FAILURE;
    };
    let Some(dsn) = settings.database_url else {
        eprintln!("{} is not set, so there is no graph store", store::DSN_ENV);
        return ExitCode::FAILURE;
    };
    let pool = match store::lazy_pool(dsn.expose(), 2) {
        Ok(pool) => pool,
        Err(error) => {
            eprintln!("{error}");
            return ExitCode::FAILURE;
        }
    };
    // Read-only unless --delete, so it checks the schema instead of
    // migrating it, as export-graph does.
    match store::schema_gap(&pool).await {
        Ok(None) => {}
        Ok(Some(gap)) => {
            eprintln!("{gap}; orphans does not migrate");
            return ExitCode::FAILURE;
        }
        Err(error) => {
            eprintln!("cannot read the migration ledger: {error}");
            return ExitCode::FAILURE;
        }
    }
    let code = report_orphans(&pool, &projects, toolkits.as_ref(), &arguments).await;
    pool.close().await;
    code
}

/// Split `found` into the graphs nothing was written to since `listed_at`
/// and those something was: written after the lists were taken, a graph is
/// not an orphan whatever the lists say (a project created after them is not
/// in them).
async fn partition_newer(
    pool: &sqlx::PgPool,
    found: Vec<elitea_inventory_engine::store::delete::OrphanGraph>,
    listed_at: Option<&str>,
) -> Result<
    (
        Vec<elitea_inventory_engine::store::delete::OrphanGraph>,
        Vec<(elitea_inventory_engine::store::delete::OrphanGraph, String)>,
    ),
    String,
> {
    use elitea_inventory_engine::store::delete;
    let (mut orphans, mut skipped) = (Vec::new(), Vec::new());
    for orphan in found {
        let newer = match listed_at {
            Some(listed_at) => delete::activity_since(pool, orphan.key, listed_at)
                .await
                .map_err(|error| {
                    format!(
                        "project {} toolkit {}: {error}",
                        orphan.key.project_id, orphan.key.application_id
                    )
                })?,
            None => None,
        };
        match newer {
            None => orphans.push(orphan),
            Some(reason) => skipped.push((orphan, reason)),
        }
    }
    Ok((orphans, skipped))
}

async fn report_orphans(
    pool: &sqlx::PgPool,
    projects: &std::collections::HashSet<i64>,
    toolkits: Option<&std::collections::HashSet<(i64, i64)>>,
    arguments: &OrphanArguments,
) -> ExitCode {
    use elitea_inventory_engine::store::delete::{self, GuardedDeletion};
    let listed_at = arguments.listed_at.as_deref();
    if arguments.delete
        && let Some(listed_at) = listed_at
        && let Err(error) =
            delete::check_listing_fresh(pool, listed_at, arguments.allow_stale_list).await
    {
        eprintln!("{error}");
        return ExitCode::FAILURE;
    }
    let found = match delete::orphans(pool, projects, toolkits).await {
        Ok(found) => found,
        Err(error) => {
            eprintln!("cannot list the orphans: {error}");
            return ExitCode::FAILURE;
        }
    };
    if found.is_empty() {
        println!("no orphaned Inventory graph");
        return ExitCode::SUCCESS;
    }
    let (orphans, mut skipped) = match partition_newer(pool, found, listed_at).await {
        Ok(parts) => parts,
        Err(error) => {
            eprintln!("{error}");
            return ExitCode::FAILURE;
        }
    };
    println!("project_id\ttoolkit_id\treason\tentities\tlast_updated");
    for orphan in &orphans {
        println!(
            "{}\t{}\t{}\t{}\t{}",
            orphan.key.project_id,
            orphan.key.application_id,
            orphan.reason,
            orphan.entities,
            orphan.updated_at
        );
    }
    for (orphan, reason) in &skipped {
        println!(
            "{}\t{}\t{}\t{}\t{}\tSKIPPED: {reason}",
            orphan.key.project_id,
            orphan.key.application_id,
            orphan.reason,
            orphan.entities,
            orphan.updated_at
        );
    }
    if !arguments.delete {
        println!(
            "dry run: {} orphaned graph(s) would be deleted, {} skipped; pass --delete with --listed-at to remove them",
            orphans.len(),
            skipped.len()
        );
        return ExitCode::SUCCESS;
    }
    let mut failed = false;
    for orphan in orphans {
        let (project, toolkit) = (orphan.key.project_id, orphan.key.application_id);
        // The activity check runs again here, inside the deletion's own
        // transaction and under the graph's lock, immediately before the
        // rows go: a write since the check above is seen, or queues behind
        // this deletion.
        match delete::delete_graph_guarded(pool, orphan.key, listed_at).await {
            Ok(GuardedDeletion::Deleted { removed, .. }) => println!(
                "deleted project {project} toolkit {toolkit}: {} entities, {} relations",
                removed.entities, removed.relations
            ),
            Ok(GuardedDeletion::Busy) => {
                // An ingestion holds the graph: it is in use, not an orphan.
                println!(
                    "SKIPPED project {project} toolkit {toolkit}: an ingestion holds it; run orphans --delete again with a fresh list"
                );
                skipped.push((orphan, "an ingestion holds it".to_owned()));
            }
            Ok(GuardedDeletion::Newer(reason)) => {
                println!("SKIPPED project {project} toolkit {toolkit}: {reason}");
                skipped.push((orphan, reason));
            }
            Err(error) => {
                eprintln!("project {project} toolkit {toolkit}: {error}");
                failed = true;
            }
        }
    }
    if !skipped.is_empty() {
        println!(
            "{} graph(s) skipped: written after the list; run again with a fresh list",
            skipped.len()
        );
    }
    if failed {
        eprintln!("some graphs were not deleted");
        ExitCode::FAILURE
    } else {
        ExitCode::SUCCESS
    }
}

#[cfg(test)]
mod tests {
    use super::*;

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
        for bad in ["1", "1 2 3", "x 1", "0 1", "1,-2"] {
            assert!(parse_toolkit_pairs(bad).is_err(), "{bad}");
        }
        assert_eq!(
            parse_toolkit_pairs("1 2\n3,4 # c\n").map(|p| p.len()),
            Ok(2)
        );
    }

    #[test]
    fn orphan_arguments_parse_and_refuse() {
        let parse = |arguments: &[&str]| {
            let owned: Vec<String> = arguments.iter().map(|a| (*a).to_owned()).collect();
            OrphanArguments::parse(&owned)
        };
        assert_eq!(
            parse(&["--existing-projects", "-", "--existing-toolkits", "t.txt"]),
            Ok(OrphanArguments {
                projects: "-".to_owned(),
                toolkits: Some("t.txt".to_owned()),
                delete: false,
                listed_at: None,
                allow_stale_list: false,
            })
        );
        assert!(
            parse(&[
                "--existing-projects",
                "p",
                "--delete",
                "--listed-at",
                "2026-10-10T09:30:00Z"
            ])
            .is_ok_and(|a| a.delete && a.listed_at.is_some() && !a.allow_stale_list)
        );
        for (arguments, needle) in [
            (&["--delete"][..], "needs --existing-projects"),
            (
                &["--existing-projects", "p", "--delete"][..],
                "--delete needs --listed-at",
            ),
            (
                &["--existing-projects", "p", "--allow-stale-list"][..],
                "only applies with --delete",
            ),
            (
                &["--existing-projects", "p", "--listed-at", "yesterday"][..],
                "not an RFC 3339 time",
            ),
            (
                &["--existing-projects", "p", "--listed-at"][..],
                "needs a value",
            ),
            (&["--existing-projects"][..], "needs a value"),
            (
                &["--existing-projects", "p", "--force"][..],
                "unknown argument",
            ),
            (
                &["--existing-projects", "-", "--existing-toolkits", "-"][..],
                "only one list",
            ),
        ] {
            let refused = parse(arguments);
            assert!(
                refused.as_ref().is_err_and(|e| e.contains(needle)),
                "{arguments:?}: {refused:?}"
            );
        }
    }

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
