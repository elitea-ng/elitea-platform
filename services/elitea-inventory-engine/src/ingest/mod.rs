//! Ingestion (ADR-0027 P3b): a source's files into the graph.
//!
//! One run reads one source ([`source`]) into the graph of one Inventory
//! toolkit ([`GraphKey`]):
//!
//! 1. take the graph's ingestion lease (one run per graph at a time);
//! 2. mark the source `in_progress`;
//! 3. clone the source's branch head (`elitea-repo-ingest`);
//! 4. select its files as the SDK loader did ([`files`]), and compare
//!    each with the hash the last completed run recorded: an unchanged
//!    file is skipped; a changed or deleted one first loses what it said
//!    ([`Graph::remove_file`]); a new or changed one is read;
//! 5. commit the graph, the new hashes and the `completed` status in one
//!    transaction (`store::sources::complete`). A run that fails or is
//!    stopped before that commits nothing but its `error` status.
//!
//! What a read file contributes: its file node and, for a code file, the
//! symbols and relations its parser found ([`parse`]). The LLM extraction
//! (P3d), communities and embeddings (P3e) add to the same step.

pub mod files;
pub mod ids;
pub mod parse;
pub mod source;

use crate::graph::Graph;
use crate::store::sources::{self as source_store, Completion, RunCounts, SourceStatus};
use crate::store::{self, GraphKey, StoreError};
use elitea_engine_core::errors::{EngineError, ErrorType};
use elitea_engine_core::stream::Context;
use elitea_repo_ingest::IngestSettings;
use files::{Selection, Skipped};
use source::Source;
use sqlx::postgres::PgPool;
use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};

/// How a run went.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Outcome {
    /// Files read this run (new or changed).
    pub documents_processed: usize,
    /// Files whose hash was unchanged.
    pub unchanged: usize,
    /// Files no longer in the source, whose entities were removed.
    pub removed_files: usize,
    /// `add_entity` calls (Python's `entities_added`, merges included).
    pub entities_added: usize,
    pub relations_added: usize,
    pub skipped_whitelist: usize,
    pub skipped_blacklist: usize,
    pub skipped_unsupported: usize,
    pub skipped_empty: usize,
    /// Files that are not UTF-8 text (the API read failed on them too).
    pub skipped_unreadable: usize,
    /// Every file the source has now, with its hash.
    pub hashes: BTreeMap<String, String>,
    /// Files a parser failed on (`path: error`); they still have their
    /// file node.
    pub parse_errors: Vec<String>,
}

/// Every regular file under `root`, as `/`-separated relative paths in
/// byte order (git's tree order). `.git` and symbolic links are skipped:
/// the API listing reported neither as a readable file.
fn list_files(root: &Path) -> std::io::Result<Vec<String>> {
    let mut found = Vec::new();
    let mut pending = vec![PathBuf::new()];
    while let Some(relative) = pending.pop() {
        for entry in std::fs::read_dir(root.join(&relative))? {
            let entry = entry?;
            let name = entry.file_name().to_string_lossy().into_owned();
            let kind = entry.file_type()?;
            let path = relative.join(&name);
            if kind.is_dir() {
                if !(relative.as_os_str().is_empty() && name == ".git") {
                    pending.push(path);
                }
            } else if kind.is_file() {
                found.push(
                    path.components()
                        .map(|part| part.as_os_str().to_string_lossy().into_owned())
                        .collect::<Vec<_>>()
                        .join("/"),
                );
            }
        }
    }
    found.sort_unstable();
    Ok(found)
}

/// A selected file whose content changed (or is new): it is read this run.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct FileToRead {
    pub path: String,
    pub text: String,
    pub hash: String,
}

/// Step 4a: select and read the files of `root`, compare each with the
/// last completed run's hash, and remove what changed or deleted files
/// said. Returns the counts so far and the files to read.
///
/// # Errors
///
/// The tree cannot be listed, or a stop was requested.
pub fn prepare(
    graph: &mut Graph,
    source: &Source,
    root: &Path,
    previous: &BTreeMap<String, String>,
    context: &Context,
) -> Result<(Outcome, Vec<FileToRead>), EngineError> {
    let selection = Selection {
        whitelist: source.whitelist.clone(),
        blacklist: source.blacklist.clone(),
    };
    let listed = list_files(root).map_err(|error| {
        EngineError::new(
            ErrorType::Runtime,
            format!("the checked-out tree cannot be listed: {error}"),
        )
    })?;
    let mut outcome = Outcome::default();
    let mut to_read = Vec::new();
    for path in &listed {
        context.checkpoint()?;
        match selection.admit(path) {
            Err(Skipped::Whitelist) => outcome.skipped_whitelist += 1,
            Err(Skipped::Blacklist) => outcome.skipped_blacklist += 1,
            Err(Skipped::UnsupportedExtension) => outcome.skipped_unsupported += 1,
            Ok(()) => {
                let Ok(bytes) = std::fs::read(root.join(path)) else {
                    outcome.skipped_unreadable += 1;
                    continue;
                };
                if bytes.is_empty() {
                    outcome.skipped_empty += 1;
                    continue;
                }
                let Ok(text) = String::from_utf8(bytes) else {
                    outcome.skipped_unreadable += 1;
                    continue;
                };
                let hash = files::content_hash(&text);
                outcome.hashes.insert(path.clone(), hash.clone());
                if previous.get(path) == Some(&hash) {
                    outcome.unchanged += 1;
                } else {
                    to_read.push(FileToRead {
                        path: path.clone(),
                        text,
                        hash,
                    });
                }
            }
        }
    }
    for gone in previous
        .keys()
        .filter(|path| !outcome.hashes.contains_key(*path))
    {
        graph.remove_file(&source.name, gone);
        outcome.removed_files += 1;
    }
    for file in &to_read {
        if previous.contains_key(&file.path) {
            graph.remove_file(&source.name, &file.path);
        }
    }
    Ok((outcome, to_read))
}

/// Step 4b: parse the files to read (one batch per language) and add each
/// one's file node and entities, in path order. Returns the relations the
/// files contributed, to be added once every file is in the graph.
///
/// # Errors
///
/// A stop was requested.
pub fn assemble(
    graph: &mut Graph,
    source: &Source,
    root: &Path,
    to_read: &[FileToRead],
    outcome: &mut Outcome,
    context: &Context,
) -> Result<Vec<parse::PendingRelation>, EngineError> {
    context.checkpoint()?;
    if !to_read.is_empty() {
        context.thinking(format!("[extract] Parsing {} files", to_read.len()));
    }
    let paths: Vec<&str> = to_read.iter().map(|file| file.path.as_str()).collect();
    let parsed = parse::parse_tree(root, &paths);
    let mut relations = Vec::new();
    for file in to_read {
        context.checkpoint()?;
        let extraction = parsed
            .get(&file.path)
            .map(|result| parse::extract(&file.path, result, &source.name, &file.hash))
            .unwrap_or_default();
        if let Some(error) = &extraction.error {
            outcome.parse_errors.push(format!("{}: {error}", file.path));
        }
        let node = files::file_node(
            &file.path,
            &file.text,
            &source.name,
            parse::counts(&extraction.entities),
        );
        graph.add_entity(
            &node.id,
            &node.name,
            node.entity_type,
            Some(&node.citation),
            Some(&node.properties),
        );
        outcome.entities_added += 1;
        for entity in &extraction.entities {
            graph.add_entity(
                &entity.id,
                &entity.name,
                &entity.entity_type,
                Some(&entity.citation),
                Some(&entity.properties),
            );
            outcome.entities_added += 1;
        }
        relations.extend(extraction.relations);
        relations.extend(parse::file_edges(
            &node.id,
            &file.path,
            &extraction.entities,
        ));
        outcome.documents_processed += 1;
        if outcome.documents_processed.is_multiple_of(10) {
            context.thinking(format!(
                "[progress] 📄 Processed {} files | 📊 {} entities",
                outcome.documents_processed, outcome.entities_added
            ));
        }
    }
    Ok(relations)
}

/// Step 4 of the run, over a checked-out tree: update `graph` with the
/// files of `root` that `source` selects, given the hashes of the last
/// completed run.
///
/// In the Python pipeline's order: every selected file is read and
/// compared first ([`prepare`]); the files to read are parsed and each
/// adds its file node and its entities ([`assemble`]); and only when every
/// file is in the graph are the relations added — a relation names its
/// ends, which may be declared in a later file or an earlier run.
///
/// # Errors
///
/// The tree cannot be listed, or a stop was requested.
pub fn ingest_tree(
    graph: &mut Graph,
    source: &Source,
    root: &Path,
    previous: &BTreeMap<String, String>,
    context: &Context,
) -> Result<Outcome, EngineError> {
    let (mut outcome, to_read) = prepare(graph, source, root, previous, context)?;
    let relations = assemble(graph, source, root, &to_read, &mut outcome, context)?;
    if !relations.is_empty() {
        context.thinking(format!(
            "[relations] 🔗 Adding {} parser-extracted relationships...",
            relations.len()
        ));
    }
    outcome.relations_added += parse::add_relations(graph, &relations, &source.name);
    Ok(outcome)
}

/// Where one run clones, removed when the run ends.
struct JobScratch(PathBuf);

impl Drop for JobScratch {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

fn store_error(error: &StoreError) -> EngineError {
    EngineError::new(
        ErrorType::Runtime,
        format!("the graph store failed: {error}"),
    )
}

/// One whole run (steps 1–5) of `source` into the graph `key`.
///
/// # Errors
///
/// Another run holds the graph; the clone is refused or fails; the store
/// fails; or a stop was requested. Any error after step 2 is also
/// recorded as the source's `error` status.
pub async fn run(
    pool: &PgPool,
    key: GraphKey,
    source: &Source,
    settings: &IngestSettings,
    context: &Context,
) -> Result<Outcome, EngineError> {
    let Some(_lease) = source_store::lease(pool, key)
        .await
        .map_err(|e| store_error(&e))?
    else {
        return Err(EngineError::new(
            ErrorType::Runtime,
            "another ingestion of this Inventory toolkit is running; wait for it to finish",
        ));
    };
    let status = SourceStatus {
        toolkit_id: source.status_key(),
        toolkit_name: source.name.clone(),
        toolkit_type: source.kind.name().to_owned(),
        branch: Some(source.active_branch()),
    };
    source_store::start(pool, key, &status)
        .await
        .map_err(|e| store_error(&e))?;
    let outcome = run_started(pool, key, source, settings, context).await;
    if let Err(error) = &outcome {
        // The run's own error is what the caller sees; a failure to record
        // it as well is only logged.
        if let Err(store) = source_store::fail(pool, key, &status.toolkit_id, &error.message).await
        {
            tracing::warn!(%store, "the failed ingestion's status was not recorded");
        }
    }
    outcome
}

async fn run_started(
    pool: &PgPool,
    key: GraphKey,
    source: &Source,
    settings: &IngestSettings,
    context: &Context,
) -> Result<Outcome, EngineError> {
    let repo_config = source.repo_config()?;
    context.thinking(format!(
        "Ingesting from {} source {}",
        source.kind.name(),
        source.name
    ));
    let graph = store::load(pool, key)
        .await
        .map_err(|e| store_error(&e))?
        .map(|(graph, _)| graph)
        .unwrap_or_default();
    let previous = source_store::file_hashes(pool, key, &source.name)
        .await
        .map_err(|e| store_error(&e))?;

    let scratch = JobScratch(settings.scratch_path.join(format!(
        "inventory-{}-{}-{}",
        key.project_id,
        key.application_id,
        uuid_like()
    )));
    std::fs::create_dir_all(&scratch.0).map_err(|error| {
        EngineError::new(
            ErrorType::Runtime,
            format!("the scratch directory cannot be created: {error}"),
        )
    })?;
    let cancel = Arc::new(AtomicBool::new(false));
    let watcher = {
        let cancel = Arc::clone(&cancel);
        let stop = context.stop_signal();
        tokio::spawn(async move {
            stop.stopped().await;
            cancel.store(true, Ordering::Release);
        })
    };
    context.thinking(format!("[fetch] Cloning {}", source.active_branch()));
    let cloned = elitea_repo_ingest::ingest(&repo_config, settings, &scratch.0, cancel).await;
    watcher.abort();
    let cloned = cloned?;
    context.checkpoint()?;

    let tree = cloned.path.clone();
    let source_for_tree = source.clone();
    let context_for_tree = context.clone();
    let (graph, outcome) = tokio::task::spawn_blocking(move || {
        let mut graph = graph;
        let outcome = ingest_tree(
            &mut graph,
            &source_for_tree,
            &tree,
            &previous,
            &context_for_tree,
        );
        (graph, outcome)
    })
    .await
    .map_err(|join| {
        EngineError::new(
            ErrorType::Runtime,
            format!("the ingestion task ended abnormally ({join})"),
        )
    })?;
    let outcome = outcome?;
    context.checkpoint()?;

    // What sources_status.json recorded: this run's additions.
    let counts = RunCounts {
        entities: i64::try_from(outcome.entities_added).unwrap_or(i64::MAX),
        relations: i64::try_from(outcome.relations_added).unwrap_or(i64::MAX),
        documents: i64::try_from(outcome.documents_processed).unwrap_or(i64::MAX),
    };
    source_store::complete(
        pool,
        key,
        &graph,
        &Completion {
            toolkit_id: &source.status_key(),
            source_name: &source.name,
            hashes: &outcome.hashes,
            counts,
            commit_sha: Some(cloned.identity.commit()),
        },
    )
    .await
    .map_err(|e| store_error(&e))?;
    context.thinking(format!(
        "[complete] {} files read, {} unchanged, {} removed",
        outcome.documents_processed, outcome.unchanged, outcome.removed_files
    ));
    Ok(outcome)
}

/// A process-unique suffix for a scratch directory.
fn uuid_like() -> String {
    static NEXT: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);
    format!(
        "{}-{}",
        std::process::id(),
        NEXT.fetch_add(1, Ordering::Relaxed)
    )
}
