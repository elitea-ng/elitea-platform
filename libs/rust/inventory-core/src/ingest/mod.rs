//! Ingestion without a store: a source's documents into the graph.
//!
//! What one run does to the graph, over any [`ContentSource`] (ADR-0028):
//!
//! 1. select the source's documents as the SDK loader selected files
//!    ([`files`]), and compare each with the version the last completed run
//!    recorded: an unchanged document is skipped; a changed or deleted one
//!    first loses what it said ([`Graph::remove_file`]); a new or changed one
//!    is read ([`prepare`]);
//! 2. parse the documents to read ([`parse`], [`parse_files`]);
//! 3. add each document's file node and entities ([`assemble`]), then the
//!    relations once every document is in the graph, then the quality pass.
//!
//! [`ingest_documents`] is those steps over one source, without a model.
//! The Inventory engine's run (`elitea_inventory_engine::ingest::run`) adds
//! the clone, the model stage, communities, embeddings and the store's
//! transaction around the same functions; the desktop's local index adds its
//! own store around [`ingest_documents`].

pub mod files;
pub mod ids;
pub mod parse;

use crate::extract;
use crate::graph::Graph;
use crate::store::DocumentState;
use elitea_content_source::git::GitSource;
use elitea_content_source::{Acl, ContentSource};
use elitea_engine_core::errors::{EngineError, ErrorType};
use elitea_engine_core::stream::Context;
use files::{Selection, Skipped};
use std::collections::{BTreeMap, HashMap};
use std::path::Path;

/// What ingestion needs to know of a source: the name every citation
/// carries (`source_toolkit`) and which documents it selects. The engine's
/// git `Source` gives one with `Source::selection`; a source of another kind
/// builds its own.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct SourceSelection {
    /// Keys every citation (`source_toolkit`) and the source's status.
    pub name: String,
    /// `None` (or empty) selects everything.
    pub whitelist: Option<Vec<String>>,
    pub blacklist: Option<Vec<String>>,
}

impl SourceSelection {
    /// A selection of every supported document, cited as `name`.
    #[must_use]
    pub fn all(name: impl Into<String>) -> Self {
        Self {
            name: name.into(),
            whitelist: None,
            blacklist: None,
        }
    }
}

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
    source: &SourceSelection,
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
    source: &SourceSelection,
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
    source: &SourceSelection,
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

/// The run's document steps over any [`ContentSource`], without a model:
/// update `graph` with the documents of `documents` that `source` selects,
/// given the versions of the last completed run, and return what changed.
///
/// In the Python pipeline's order: every selected document is listed and
/// compared first ([`prepare`]); the documents to read are parsed and each
/// adds its file node and its entities ([`assemble`]); and only when every
/// document is in the graph are the relations added — a relation names its
/// ends, which may be declared in a later document or an earlier run.
///
/// The parsers read the documents to read from `root` by key (a tree
/// source's key is its path under `root`).
///
/// # Errors
///
/// The source cannot be listed, or a stop was requested.
pub async fn ingest_documents<S: ContentSource>(
    graph: &mut Graph,
    source: &SourceSelection,
    documents: &S,
    root: &Path,
    previous: &BTreeMap<String, String>,
    context: &Context,
) -> Result<Outcome, EngineError> {
    let (mut outcome, to_read) = prepare(graph, source, documents, previous, context).await?;
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

/// [`ingest_documents`] over the checked-out tree `root` (a
/// [`GitSource`]), as a synchronous call (its own current-thread runtime)
/// for callers outside one.
///
/// # Errors
///
/// The tree cannot be listed, or a stop was requested.
pub fn ingest_tree(
    graph: &mut Graph,
    source: &SourceSelection,
    root: &Path,
    previous: &BTreeMap<String, String>,
    context: &Context,
) -> Result<Outcome, EngineError> {
    let runtime = tokio::runtime::Builder::new_current_thread()
        .build()
        .map_err(|error| EngineError::new(ErrorType::Runtime, error.to_string()))?;
    runtime.block_on(ingest_documents(
        graph,
        source,
        &GitSource::new(root),
        root,
        previous,
        context,
    ))
}

/// Add the parser relations of [`assemble`] once every document is in the
/// graph.
pub fn add_parser_relations(
    graph: &mut Graph,
    relations: &[parse::PendingRelation],
    source: &SourceSelection,
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
