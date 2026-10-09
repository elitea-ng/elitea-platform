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
use crate::store::sources::{
    self as source_store, Completion, DocumentState, RunCounts, SourceStatus,
};
use crate::store::{self, GraphKey, StoreError};
use elitea_content_source::git::GitSource;
use elitea_content_source::{Acl, ContentSource};
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
    /// `add_entity` calls (Python's `entities_added`, merges included):
    /// what this run extracted, NOT what the graph holds.
    pub entities_added: usize,
    /// `add_relation` calls that found both endpoints (Python's
    /// `relations_added`): a relation found twice, or by the parser and the
    /// model, is one edge in the graph, and the quality pass prunes some.
    pub relations_added: usize,
    /// The entities the stored graph holds of this source after the run
    /// ([`Graph::source_counts`]).
    pub entities_stored: usize,
    /// The edges the stored graph holds of this source after the run.
    pub relations_stored: usize,
    pub skipped_whitelist: usize,
    pub skipped_blacklist: usize,
    pub skipped_unsupported: usize,
    pub skipped_empty: usize,
    /// Files that are not UTF-8 text (the API read failed on them too).
    pub skipped_unreadable: usize,
    /// Every document the source has now, with its version.
    pub hashes: BTreeMap<String, String>,
    /// Every document the source has now, as the store keeps it.
    pub documents: BTreeMap<String, DocumentState>,
    /// Files a parser failed on (`path: error`); they still have their
    /// file node.
    pub parse_errors: Vec<String>,
    /// Files the model was not asked about (small, license, barrel).
    pub model_skipped: usize,
    /// Chunks whose model extraction failed after every retry.
    pub failed_model_chunks: usize,
    /// Edges the quality pass removed.
    pub pruned_edges: usize,
    /// Communities detected.
    pub communities: usize,
    /// Entities embedded.
    pub embeddings_generated: usize,
}

/// A selected file whose content changed (or is new): it is read this run.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct FileToRead {
    /// The document's key in its source (a repository path for git).
    pub path: String,
    pub text: String,
    /// The document's version (a git file's: the SHA-256 of its bytes).
    pub hash: String,
    pub mime: String,
    pub acl: Acl,
}

/// Step 4a: list the source's documents, select them as the SDK loader
/// selected files, compare each version with the last completed run's,
/// read and extract the new and changed ones, and remove what changed or
/// deleted documents said. Returns the counts so far and the documents to
/// read.
///
/// Any [`ContentSource`] (ADR-0028): a git checkout lists its files; a
/// document that is not text is extracted (`elitea-doc-extract`) when this
/// build can, and skipped as unsupported otherwise.
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
    let selection = Selection {
        whitelist: source.whitelist.clone(),
        blacklist: source.blacklist.clone(),
    };
    let listed = documents
        .list()
        .await
        .map_err(|error| EngineError::new(ErrorType::Runtime, error.to_string()))?;
    let mut outcome = Outcome::default();
    let mut to_read = Vec::new();
    for reference in &listed {
        context.checkpoint()?;
        let path = &reference.key;
        match selection.admit_document(path, &reference.mime) {
            Err(Skipped::Whitelist) => outcome.skipped_whitelist += 1,
            Err(Skipped::Blacklist) => outcome.skipped_blacklist += 1,
            Err(Skipped::UnsupportedExtension) => outcome.skipped_unsupported += 1,
            Ok(()) => {
                if reference.size == 0 {
                    outcome.skipped_empty += 1;
                    continue;
                }
                let state = DocumentState {
                    version: reference.version.clone(),
                    mime: reference.mime.clone(),
                    acl: reference.acl.clone(),
                };
                if previous.get(path) == Some(&reference.version) {
                    outcome.unchanged += 1;
                    outcome
                        .hashes
                        .insert(path.clone(), reference.version.clone());
                    outcome.documents.insert(path.clone(), state);
                    continue;
                }
                let Ok(document) = documents.fetch(path).await else {
                    outcome.skipped_unreadable += 1;
                    continue;
                };
                // Extraction is CPU work (a PDF, a spreadsheet): off the
                // runtime's threads.
                let mime = reference.mime.clone();
                let extracted = tokio::task::spawn_blocking(move || {
                    elitea_doc_extract::extract(&mime, &document.bytes)
                })
                .await
                .unwrap_or_else(|join| {
                    elitea_doc_extract::Extracted::Unreadable(format!(
                        "extraction ended abnormally ({join})"
                    ))
                });
                let text = match extracted {
                    elitea_doc_extract::Extracted::Text { text, .. } => text,
                    elitea_doc_extract::Extracted::Unsupported => {
                        outcome.skipped_unsupported += 1;
                        continue;
                    }
                    elitea_doc_extract::Extracted::Unreadable(_) => {
                        outcome.skipped_unreadable += 1;
                        continue;
                    }
                };
                if text.is_empty() {
                    outcome.skipped_empty += 1;
                    continue;
                }
                outcome
                    .hashes
                    .insert(path.clone(), reference.version.clone());
                outcome.documents.insert(path.clone(), state);
                to_read.push(FileToRead {
                    path: path.clone(),
                    text,
                    hash: reference.version.clone(),
                    mime: reference.mime.clone(),
                    acl: reference.acl.clone(),
                });
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

/// Step 4 of the run over a checked-out tree, without a model: update
/// `graph` with the files of `root` that `source` selects, given the
/// versions of the last completed run. A synchronous wrapper (its own
/// current-thread runtime) for callers outside one; the run itself uses
/// [`prepare`] on the source directly.
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
    let checkout = GitSource::new(root);
    let runtime = tokio::runtime::Builder::new_current_thread()
        .build()
        .map_err(|error| EngineError::new(ErrorType::Runtime, error.to_string()))?;
    let (mut outcome, to_read) =
        runtime.block_on(prepare(graph, source, &checkout, previous, context))?;
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

/// What a run does beyond reading files.
#[derive(Debug, Clone, Default)]
pub struct RunOptions {
    /// The model stage, community labels; `None` reads files and parses
    /// code only.
    pub model: Option<ModelOptions>,
    /// Entity embeddings, when the toolkit configures a model for them.
    pub embeddings: Option<elitea_model_client::embeddings::EmbeddingClient>,
    /// `full_rebuild`: start from an empty graph (every source of it).
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
    // `full_rebuild` does NOT delete the graph here: a rebuild that fails or
    // is stopped must leave the previous graph readable, as every other
    // failed run does. run_started starts from an empty graph instead, and
    // the commit replaces the graph and every source's state in one
    // transaction (source_store::complete_rebuild). MEASURED: deleting up
    // front, a rebuild stopped 45 s in left the toolkit with no graph at all.
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
    let (graph, previous) = if options.full_rebuild {
        (Graph::default(), BTreeMap::new())
    } else {
        let graph = store::load(pool, key)
            .await
            .map_err(|e| store_error(&e))?
            .map(|(graph, _)| graph)
            .unwrap_or_default();
        let previous = source_store::document_versions(pool, key, &source.name)
            .await
            .map_err(|e| store_error(&e))?;
        (graph, previous)
    };

    let (_scratch, cloned) = clone(settings, &repo_config, key, source, context).await?;

    let tree = cloned.path.clone();
    let checkout = GitSource::new(&tree);
    let mut graph = graph;
    let (outcome, to_read) = prepare(&mut graph, source, &checkout, &previous, context).await?;
    let source_for_tree = source.clone();
    let context_for_tree = context.clone();
    let (to_read, parsed) = tokio::task::spawn_blocking(move || {
        let parsed = parse_files(&source_for_tree, &tree, &to_read, &context_for_tree);
        (to_read, parsed)
    })
    .await
    .map_err(|join| {
        EngineError::new(
            ErrorType::Runtime,
            format!("the ingestion task ended abnormally ({join})"),
        )
    })?;
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
    if options.full_rebuild {
        source_store::complete_rebuild(pool, key, &graph, &completion).await
    } else {
        source_store::complete(pool, key, &graph, &completion).await
    }
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
