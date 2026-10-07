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

use crate::extract;
use crate::graph::Graph;
use crate::store::sources::{self as source_store, Completion, RunCounts, SourceStatus};
use crate::store::{self, GraphKey, StoreError};
use elitea_engine_core::errors::{EngineError, ErrorType};
use elitea_engine_core::stream::Context;
use elitea_repo_ingest::IngestSettings;
use files::{Selection, Skipped};
use source::Source;
use sqlx::postgres::PgPool;
use std::collections::{BTreeMap, HashMap};
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
    /// Files the model was not asked about (small, license, barrel).
    pub model_skipped: usize,
    /// Chunks whose model extraction failed after every retry.
    pub failed_model_chunks: usize,
    /// Edges the quality pass removed.
    pub pruned_edges: usize,
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

/// Step 4b: parse the files to read, one batch per language.
#[must_use]
pub fn parse_files(
    source: &Source,
    root: &Path,
    to_read: &[FileToRead],
    context: &Context,
) -> BTreeMap<String, parse::FileExtraction> {
    if !to_read.is_empty() {
        context.thinking(format!("[extract] Parsing {} files", to_read.len()));
    }
    let paths: Vec<&str> = to_read.iter().map(|file| file.path.as_str()).collect();
    parse::parse_tree(root, &paths)
        .into_iter()
        .filter_map(|(path, result)| {
            let hash = &to_read.iter().find(|file| file.path == path)?.hash;
            let extraction = parse::extract(&path, &result, &source.name, hash);
            Some((path, extraction))
        })
        .collect()
}

/// What step 4c added for one file: the entities a model relation step
/// is offered, as `(id, name, type)`.
pub type FileEntities = BTreeMap<String, Vec<(String, String, String)>>;

/// Step 4c: add each file's file node, parser entities and model entities
/// (in that order, path order), every type through the graph's
/// normaliser (`KnowledgeGraph.add_entity`). Returns the parser relations
/// to add once every file is in the graph, and each file's entities.
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
    let mut relations = Vec::new();
    let mut per_file = FileEntities::new();
    let unparsed = parse::FileExtraction::default();
    for file in to_read {
        context.checkpoint()?;
        let extraction = parsed.get(&file.path).unwrap_or(&unparsed);
        if let Some(error) = &extraction.error {
            outcome.parse_errors.push(format!("{}: {error}", file.path));
        }
        let mut entities: Vec<&parse::ParsedEntity> = extraction.entities.iter().collect();
        if let Some(model) = modelled.get(&file.path) {
            entities.extend(model.entities.iter());
            outcome.failed_model_chunks += model.failed_chunks;
        }
        let owned: Vec<parse::ParsedEntity> = entities.iter().map(|e| (*e).clone()).collect();
        let node = files::file_node(&file.path, &file.text, &source.name, parse::counts(&owned));
        let mut offered = Vec::with_capacity(entities.len() + 1);
        let node_type = extract::types::normalize_graph(node.entity_type);
        graph.add_entity(
            &node.id,
            &node.name,
            &node_type,
            Some(&node.citation),
            Some(&node.properties),
        );
        offered.push((node.id.clone(), node.name.clone(), node_type));
        outcome.entities_added += 1;
        for entity in &entities {
            let kind = extract::types::normalize_graph(&entity.entity_type);
            graph.add_entity(
                &entity.id,
                &entity.name,
                &kind,
                Some(&entity.citation),
                Some(&entity.properties),
            );
            offered.push((entity.id.clone(), entity.name.clone(), kind));
            outcome.entities_added += 1;
        }
        relations.extend(extraction.relations.iter().cloned());
        relations.extend(parse::file_edges(&node.id, &file.path, &owned));
        per_file.insert(file.path.clone(), offered);
        outcome.documents_processed += 1;
        if outcome.documents_processed.is_multiple_of(10) {
            context.thinking(format!(
                "[progress] 📄 Processed {} files | 📊 {} entities",
                outcome.documents_processed, outcome.entities_added
            ));
        }
    }
    Ok((relations, per_file))
}

/// `_run_graph_quality_pass`: remove self-loops, `related_to` edges into a
/// node with more than 50 of them, and `related_to` edges into a short or
/// generic code-named entity. Returns how many edges went.
pub fn quality_pass(graph: &mut Graph) -> usize {
    const HUB_THRESHOLD: usize = 50;
    const GENERIC: [&str; 31] = [
        "get", "set", "context", "page", "api", "data", "test", "name", "url", "login", "user",
        "response", "request", "result", "value", "config", "error", "status", "type", "id", "key",
        "list", "item", "index", "base", "default", "main", "run", "init", "setup", "start",
    ];
    let relation = |edge: &serde_json::Map<String, serde_json::Value>| {
        edge.get("relation_type")
            .or_else(|| edge.get("type"))
            .and_then(serde_json::Value::as_str)
            .unwrap_or_default()
            .to_lowercase()
    };
    let mut pruned = graph.retain_edges(|source, target, _| source != target);
    let mut related_in: HashMap<String, usize> = HashMap::new();
    for (_, target, edge) in graph.edges() {
        if relation(edge) == "related_to" {
            *related_in.entry(target.to_owned()).or_default() += 1;
        }
    }
    let hubs: std::collections::HashSet<String> = related_in
        .into_iter()
        .filter(|(_, count)| *count > HUB_THRESHOLD)
        .map(|(id, _)| id)
        .collect();
    pruned += graph
        .retain_edges(|_, target, edge| !(hubs.contains(target) && relation(edge) == "related_to"));
    let generic_targets: std::collections::HashSet<String> = graph
        .nodes()
        .filter(|(_, node)| {
            let kind = node
                .get("type")
                .and_then(serde_json::Value::as_str)
                .unwrap_or_default()
                .to_lowercase();
            let name = node
                .get("name")
                .and_then(serde_json::Value::as_str)
                .unwrap_or_default();
            let name = elitea_engine_core::pystr::strip(&name.to_lowercase()).to_owned();
            matches!(
                kind.as_str(),
                "function" | "variable" | "constant" | "import" | "class" | "method"
            ) && (GENERIC.contains(&name.as_str())
                || (elitea_engine_core::pystr::split_whitespace(&name).count() <= 1
                    && name.chars().count() <= 10))
        })
        .map(|(id, _)| id.to_owned())
        .collect();
    pruned += graph.retain_edges(|_, target, edge| {
        !(relation(edge) == "related_to" && generic_targets.contains(target))
    });
    pruned
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
    let parsed = parse_files(source, root, &to_read, context);
    let (relations, _) = assemble(
        graph,
        source,
        &to_read,
        &parsed,
        &HashMap::new(),
        &mut outcome,
        context,
    )?;
    add_parser_relations(graph, &relations, source, &mut outcome, context);
    outcome.pruned_edges += quality_pass(graph);
    Ok(outcome)
}

fn add_parser_relations(
    graph: &mut Graph,
    relations: &[parse::PendingRelation],
    source: &Source,
    outcome: &mut Outcome,
    context: &Context,
) {
    if !relations.is_empty() {
        context.thinking(format!(
            "[relations] 🔗 Adding {} parser-extracted relationships...",
            relations.len()
        ));
    }
    outcome.relations_added += parse::add_relations(graph, relations, &source.name, "parser");
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
    model: Option<&ModelOptions>,
    context: &Context,
) -> Result<(), EngineError> {
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
    model: Option<&ModelOptions>,
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
    let outcome = run_started(pool, key, source, settings, model, context).await;
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
    model: Option<&ModelOptions>,
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

    let (_scratch, cloned) = clone(settings, &repo_config, key, source, context).await?;

    let tree = cloned.path.clone();
    let source_for_tree = source.clone();
    let context_for_tree = context.clone();
    let prepared = tokio::task::spawn_blocking(move || {
        let mut graph = graph;
        let prepared = prepare(
            &mut graph,
            &source_for_tree,
            &tree,
            &previous,
            &context_for_tree,
        )
        .map(|(outcome, to_read)| {
            let parsed = parse_files(&source_for_tree, &tree, &to_read, &context_for_tree);
            (outcome, to_read, parsed)
        });
        (graph, prepared)
    })
    .await
    .map_err(|join| {
        EngineError::new(
            ErrorType::Runtime,
            format!("the ingestion task ended abnormally ({join})"),
        )
    })?;
    let (mut graph, prepared) = prepared;
    let (mut outcome, to_read, parsed) = prepared?;
    context.checkpoint()?;

    build(
        &mut graph,
        source,
        &to_read,
        &parsed,
        &mut outcome,
        model,
        context,
    )
    .await?;

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
