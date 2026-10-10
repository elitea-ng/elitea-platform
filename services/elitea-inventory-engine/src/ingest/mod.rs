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

pub mod source;

// The store-free steps (selection, comparison, parsing, the file nodes, the
// relations, the quality pass) live in the shared core (ADR-0029 decision
// 7), over any `ContentSource`; re-exported so every path in this crate
// stays the same. The functions that took the git `Source` keep that
// signature here and pass its selection on.
pub use elitea_inventory_core::ingest::{
    FileEntities, FileToRead, Outcome, SourceSelection, files, ids, ingest_documents, parse,
    quality_pass,
};

use crate::extract;
use crate::graph::Graph;
use crate::store::sources::{self as source_store, Completion, RunCounts, SourceStatus};
use crate::store::{self, GraphKey, StoreError};
use elitea_content_source::ContentSource;
use elitea_content_source::git::GitSource;
use elitea_engine_core::errors::{EngineError, ErrorType};
use elitea_engine_core::stream::Context;
use elitea_inventory_core::ingest as core;
use elitea_repo_ingest::IngestSettings;
use source::Source;
use sqlx::postgres::PgPool;
use std::collections::{BTreeMap, HashMap};
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};

/// Step 4a ([`core::prepare`]) for `source`.
///
/// # Errors
///
/// The source cannot be listed, or a stop was requested.
pub async fn prepare<S: ContentSource>(
    graph: &mut Graph,
    source: &Source,
    documents: &S,
    previous: &BTreeMap<String, String>,
    context: &Context,
) -> Result<(Outcome, Vec<FileToRead>), EngineError> {
    core::prepare(graph, &source.selection(), documents, previous, context).await
}

/// Step 4b ([`core::parse_files`]) for `source`: each file's bytes are
/// dropped once parsed.
///
/// # Errors
///
/// A stop was requested.
pub fn parse_files(
    source: &Source,
    root: &Path,
    to_read: &mut [FileToRead],
    context: &Context,
) -> Result<BTreeMap<String, parse::FileExtraction>, EngineError> {
    core::parse_files(&source.selection(), root, to_read, context)
}

/// Step 4c ([`core::assemble`]) for `source`.
///
/// # Errors
///
/// A stop was requested.
#[allow(clippy::implicit_hasher)]
pub fn assemble(
    graph: &mut Graph,
    source: &Source,
    to_read: &[FileToRead],
    parsed: &BTreeMap<String, parse::FileExtraction>,
    modelled: &HashMap<String, extract::ModelExtraction>,
    outcome: &mut Outcome,
    context: &Context,
) -> Result<(Vec<parse::PendingRelation>, FileEntities), EngineError> {
    core::assemble(
        graph,
        &source.selection(),
        to_read,
        parsed,
        modelled,
        outcome,
        context,
    )
}

/// Step 4 of the run over a checked-out tree, without a model
/// ([`core::ingest_tree`]) for `source`.
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
    core::ingest_tree(graph, &source.selection(), root, previous, context)
}

fn add_parser_relations(
    graph: &mut Graph,
    relations: &[parse::PendingRelation],
    source: &Source,
    outcome: &mut Outcome,
    context: &Context,
) {
    core::add_parser_relations(graph, relations, &source.selection(), outcome, context);
}

/// What a run does beyond reading files.
#[derive(Debug, Clone, Default)]
pub struct RunOptions {
    /// The model stage, community labels; `None` reads files and parses
    /// code only.
    pub model: Option<ModelOptions>,
    /// Entity embeddings, when the toolkit configures a model for them.
    pub embeddings: Option<elitea_model_client::embeddings::EmbeddingClient>,
    /// `full_rebuild`: re-read every document of THIS source, after
    /// forgetting everything it said ([`Graph::remove_source`]); the other
    /// sources of the graph are untouched.
    pub full_rebuild: bool,
}

/// How a run uses a model.
#[derive(Debug, Clone)]
pub struct ModelOptions {
    pub model: extract::Model,
    pub tuning: extract::Tuning,
    /// A file shorter than this many lines (or characters) is not sent to
    /// the model (`min_file_lines` 20, `min_file_chars` 300).
    pub min_file_lines: usize,
    pub min_file_chars: usize,
}

impl ModelOptions {
    /// The Python pipeline's defaults around `model`.
    #[must_use]
    pub fn new(model: extract::Model) -> Self {
        Self {
            model,
            tuning: extract::Tuning::default(),
            min_file_lines: 20,
            min_file_chars: 300,
        }
    }
}

/// The model stage over the files to read: each file not skipped
/// (`extract::skip`) is extracted, up to `parallel_files` at once.
///
/// # Errors
///
/// A stop was requested.
pub async fn model_files(
    options: &ModelOptions,
    source: &Source,
    to_read: &[FileToRead],
    parsed: &BTreeMap<String, parse::FileExtraction>,
    outcome: &mut Outcome,
    context: &Context,
) -> Result<HashMap<String, extract::ModelExtraction>, EngineError> {
    let stop = context.stop_signal();
    let semaphore = std::sync::Arc::new(tokio::sync::Semaphore::new(
        options.tuning.parallel_files.max(1),
    ));
    let mut tasks = tokio::task::JoinSet::new();
    for file in to_read {
        if let Some(reason) =
            extract::skip::reason(&file.text, options.min_file_lines, options.min_file_chars)
        {
            tracing::debug!(path = %file.path, %reason, "the model is not asked about this file");
            outcome.model_skipped += 1;
            continue;
        }
        let parser_names: std::collections::HashSet<String> = parsed
            .get(&file.path)
            .map(|p| p.entities.iter().map(|e| e.name.to_lowercase()).collect())
            .unwrap_or_default();
        let (model, tuning, stop, semaphore) = (
            options.model.clone(),
            options.tuning,
            stop.clone(),
            std::sync::Arc::clone(&semaphore),
        );
        let (path, text, hash, toolkit) = (
            file.path.clone(),
            file.text.clone(),
            file.hash.clone(),
            source.name.clone(),
        );
        tasks.spawn(async move {
            let _permit = semaphore.acquire().await;
            let input = extract::FileInput {
                path: &path,
                text: &text,
                content_hash: &hash,
                source_toolkit: &toolkit,
                parser_names: &parser_names,
            };
            let extraction = extract::extract_file(&model, &input, tuning, &stop).await;
            (path, extraction)
        });
    }
    let total = tasks.len();
    let mut done = 0usize;
    let mut results = HashMap::new();
    while let Some(joined) = tasks.join_next().await {
        let (path, extraction) = joined.map_err(|join| {
            EngineError::new(
                ErrorType::Runtime,
                format!("a model task ended abnormally ({join})"),
            )
        })?;
        results.insert(path, extraction?);
        done += 1;
        if done.is_multiple_of(10) || done == total {
            context.thinking(format!(
                "[extract] 🤖 Model extraction: {done}/{total} files"
            ));
        }
    }
    Ok(results)
}

/// The model relation step: each file with two or more entities this run
/// is read once more for the relations between them, resolved against the
/// whole graph.
///
/// # Errors
///
/// A stop was requested.
pub async fn model_relations(
    options: &ModelOptions,
    graph: &Graph,
    to_read: &[FileToRead],
    per_file: &FileEntities,
    context: &Context,
) -> Result<Vec<parse::PendingRelation>, EngineError> {
    let resolver =
        std::sync::Arc::new(extract::IdResolver::new(graph.nodes().map(|(id, node)| {
            (
                id,
                node.get("name")
                    .and_then(serde_json::Value::as_str)
                    .unwrap_or_default(),
                node.get("type")
                    .and_then(serde_json::Value::as_str)
                    .unwrap_or_default(),
            )
        })));
    let stop = context.stop_signal();
    let semaphore = std::sync::Arc::new(tokio::sync::Semaphore::new(
        options.tuning.parallel_files.max(1),
    ));
    let mut tasks = tokio::task::JoinSet::new();
    for file in to_read {
        let Some(entities) = per_file.get(&file.path).filter(|e| e.len() >= 2) else {
            continue;
        };
        let (model, tuning, stop, semaphore, resolver) = (
            options.model.clone(),
            options.tuning,
            stop.clone(),
            std::sync::Arc::clone(&semaphore),
            std::sync::Arc::clone(&resolver),
        );
        let (path, text, entities) = (file.path.clone(), file.text.clone(), entities.clone());
        tasks.spawn(async move {
            let _permit = semaphore.acquire().await;
            (
                path.clone(),
                extract::extract_relations(
                    &model, &path, &text, &entities, &resolver, tuning, &stop,
                )
                .await,
            )
        });
    }
    let mut by_file: BTreeMap<String, Vec<parse::PendingRelation>> = BTreeMap::new();
    while let Some(joined) = tasks.join_next().await {
        let (path, relations) = joined.map_err(|join| {
            EngineError::new(
                ErrorType::Runtime,
                format!("a model task ended abnormally ({join})"),
            )
        })?;
        by_file.insert(path, relations?);
    }
    Ok(by_file.into_values().flatten().collect())
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

/// Steps 4c onwards over the read files: the model stage, the graph's
/// nodes, the parser and model relations, the quality pass.
async fn build(
    graph: &mut Graph,
    source: &Source,
    to_read: &[FileToRead],
    parsed: &BTreeMap<String, parse::FileExtraction>,
    outcome: &mut Outcome,
    options: &RunOptions,
    context: &Context,
) -> Result<(), EngineError> {
    let model = options.model.as_ref();
    let modelled = match model {
        Some(options) => model_files(options, source, to_read, parsed, outcome, context).await?,
        None => HashMap::new(),
    };
    let (relations, per_file) =
        assemble(graph, source, to_read, parsed, &modelled, outcome, context)?;
    add_parser_relations(graph, &relations, source, outcome, context);
    if let Some(options) = model {
        context.thinking(format!(
            "[relations] 🔗 Extracting semantic relations from {} files...",
            per_file.values().filter(|e| e.len() >= 2).count()
        ));
        let found = model_relations(options, graph, to_read, &per_file, context).await?;
        outcome.relations_added += parse::add_relations(graph, &found, &source.name, "llm");
    }
    let pruned = quality_pass(graph);
    outcome.pruned_edges += pruned;
    if pruned > 0 {
        context.thinking(format!(
            "[quality] 🧹 Quality pass: pruned {pruned} low-quality edges"
        ));
    }
    context.checkpoint()?;
    communities(graph, model, outcome, context).await?;
    if let Some(client) = &options.embeddings {
        context.thinking("[embeddings] 🧮 Generating entity embeddings...".to_owned());
        let count = crate::embed::embed_graph(
            graph,
            client,
            &crate::clock::now_iso(),
            &context.stop_signal(),
        )
        .await?;
        outcome.embeddings_generated = count;
        context.thinking(format!(
            "[embeddings] ✅ Generated embeddings for {count} entities"
        ));
    }
    Ok(())
}

/// `_run_community_detection`: Leiden over the whole graph (10 nodes or
/// more), labelled and summarised by the model when there is one.
async fn communities(
    graph: &mut Graph,
    model: Option<&ModelOptions>,
    outcome: &mut Outcome,
    context: &Context,
) -> Result<(), EngineError> {
    context.thinking("[communities] 🔍 Detecting communities...".to_owned());
    let Some(mut data) = crate::communities::detect(graph, 1.0) else {
        return Ok(());
    };
    let count = data
        .get("num_communities")
        .and_then(serde_json::Value::as_u64)
        .unwrap_or(0);
    let modularity = data
        .get("modularity")
        .and_then(serde_json::Value::as_f64)
        .unwrap_or(0.0);
    context.thinking(format!(
        "[communities] 🏘️ Detected {count} communities (modularity={modularity:.3})"
    ));
    if let Some(options) = model.filter(|_| count > 0) {
        context.thinking(format!(
            "[communities] 🏷️ Generating labels for {count} communities..."
        ));
        crate::communities::label_and_summarize(
            &options.model,
            &mut data,
            graph,
            options.tuning,
            &context.stop_signal(),
        )
        .await?;
    }
    outcome.communities = usize::try_from(count).unwrap_or(usize::MAX);
    graph.set_community_data(data);
    Ok(())
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
    options: &RunOptions,
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
    // `full_rebuild` does NOT delete anything here: a rebuild that fails
    // or is stopped must leave the previous graph readable, as every other
    // failed run does. run_started removes the source's contributions from
    // the loaded graph in memory instead, and the commit replaces the graph
    // and the source's state in one transaction. MEASURED: deleting up
    // front, a rebuild stopped 45 s in left the toolkit with no graph at
    // all. The rebuild is scoped to the source run_ingestion names: Python
    // deleted the whole graph.json, every other source's entities,
    // relations and state with it.
    source_store::start(pool, key, &status)
        .await
        .map_err(|e| store_error(&e))?;
    let outcome = run_started(pool, key, source, settings, options, context).await;
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

/// Step 3: the shallow clone, into a scratch directory removed when the
/// returned guard drops; a stop interrupts it.
async fn clone(
    settings: &IngestSettings,
    repo_config: &serde_json::Value,
    key: GraphKey,
    source: &Source,
    context: &Context,
) -> Result<(JobScratch, elitea_repo_ingest::ClonedRepository), EngineError> {
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
    let cloned = elitea_repo_ingest::ingest(repo_config, settings, &scratch.0, cancel).await;
    watcher.abort();
    let cloned = cloned?;
    context.checkpoint()?;
    Ok((scratch, cloned))
}

async fn run_started(
    pool: &PgPool,
    key: GraphKey,
    source: &Source,
    settings: &IngestSettings,
    options: &RunOptions,
    context: &Context,
) -> Result<Outcome, EngineError> {
    let repo_config = source.repo_config()?;
    context.thinking(format!(
        "Ingesting from {} source {}",
        source.kind.name(),
        source.name
    ));
    let mut graph = store::load(pool, key)
        .await
        .map_err(|e| store_error(&e))?
        .map(|(graph, _)| graph)
        .unwrap_or_default();
    let previous = if options.full_rebuild {
        // Forget what this source said (its citations, the entities only it
        // cited, its contribution to every relation) and read every document
        // again. Nothing is committed until the run completes.
        graph.remove_source(&source.name);
        BTreeMap::new()
    } else {
        source_store::document_versions(pool, key, &source.name)
            .await
            .map_err(|e| store_error(&e))?
    };

    let (_scratch, cloned) = clone(settings, &repo_config, key, source, context).await?;

    let tree = cloned.path.clone();
    let checkout = GitSource::new(&tree);
    let (outcome, to_read) = prepare(&mut graph, source, &checkout, &previous, context).await?;
    let source_for_tree = source.clone();
    let context_for_tree = context.clone();
    let (to_read, parsed) = tokio::task::spawn_blocking(move || {
        let mut to_read = to_read;
        let parsed = parse_files(&source_for_tree, &tree, &mut to_read, &context_for_tree);
        (to_read, parsed)
    })
    .await
    .map_err(|join| {
        EngineError::new(
            ErrorType::Runtime,
            format!("the ingestion task ended abnormally ({join})"),
        )
    })?;
    let parsed = parsed?;
    let mut outcome = outcome;
    context.checkpoint()?;

    build(
        &mut graph,
        source,
        &to_read,
        &parsed,
        &mut outcome,
        options,
        context,
    )
    .await?;

    // What the source status records: what the stored graph holds of the
    // source. Python's sources_status.json recorded this run's add calls
    // (merged duplicates, re-found relations and pruned edges included),
    // so the status disagreed with get_stats on the same graph.
    (outcome.entities_stored, outcome.relations_stored) = graph.source_counts(&source.name);
    let counts = RunCounts {
        entities: i64::try_from(outcome.entities_stored).unwrap_or(i64::MAX),
        relations: i64::try_from(outcome.relations_stored).unwrap_or(i64::MAX),
        documents: i64::try_from(outcome.documents_processed).unwrap_or(i64::MAX),
    };
    let toolkit_id = source.status_key();
    let completion = Completion {
        toolkit_id: &toolkit_id,
        source_name: &source.name,
        documents: &outcome.documents,
        counts,
        commit_sha: Some(cloned.identity.commit()),
    };
    // A rebuild commits as any run does: the graph, and this source's
    // documents (all replaced) and status.
    source_store::complete(pool, key, &graph, &completion)
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
