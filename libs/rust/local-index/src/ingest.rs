//! A build's ingestion in bounded batches: what `inventory-core`'s
//! `ingest_documents` does, without holding every file's text, every
//! parse and every chunk at once.
//!
//! `ingest_documents` reads every changed file, parses all of them, then
//! adds them to the graph: its peak is the whole folder's text plus every
//! parse result plus the graph (5–6 GB for a 14k-file repository). Here the
//! files are taken one batch at a time — all the files of one parsed
//! language together (its parser links symbols across those files, as
//! `parse_tree` already parses one language at a time), the files no parser
//! reads in batches of [`PLAIN_BATCH`] — and each batch is read, staged as
//! chunks (handed to `stage`, which writes them to the store's staging
//! table), parsed, and added to the graph file by file, dropping each
//! file's text and parse as soon as it is in. The parser relations are
//! added once every batch is in, over the whole graph, as before, and the
//! quality pass runs last. Before the relations, the nodes the batches
//! added (and the relations) are put back in listing order, the order a
//! single pass adds them in: names resolve to the last node of a name, so
//! the relations resolve as `ingest_documents`' do. One difference stays:
//! an entity id two files of different languages both declare (ids are
//! made from the name and type, not the path) is merged in batch order, so
//! its first citation and name casing come from the earlier language's
//! file — on a 13.8k-file repository, 3 nodes and 17 of 512k relations.
//!
//! **Dependents.** Removing a changed file from the graph removes the nodes
//! only it cites, and with them every edge into them — including the edges
//! unchanged files contributed (a call from another file). Parsing the file
//! again brings its nodes back, not those edges: the unchanged files are
//! skipped. So before anything is removed, the unchanged files with an
//! edge into a node a changed or deleted file cites (`dependents`) are
//! read again too, up to [`MAX_DEPENDENTS`]; past that none is, and the
//! refresh says so ([`Expansion::capped`]) for a full rebuild to follow.

use crate::source::LocalFolderSource;
use elitea_content_source::{ContentSource, Document, DocumentRef, SourceError};
use elitea_engine_core::errors::{EngineError, ErrorType};
use elitea_engine_core::stream::Context;
use elitea_inventory_core::graph::{Graph, relation_provenance};
use elitea_inventory_core::ingest::{
    Outcome, SourceSelection, add_parser_relations, assemble, parse, parse_files, prepare,
    quality_pass,
};
use serde_json::{Map, Value};
use std::borrow::Cow;
use std::collections::{BTreeMap, BTreeSet, HashMap, HashSet};
use std::path::Path;

/// Files no parser reads, per batch.
pub const PLAIN_BATCH: usize = 512;

/// The most unchanged files one refresh reads again for their edges into
/// changed files' nodes; past it, none is (a full rebuild restores them).
pub const MAX_DEPENDENTS: usize = 2_000;

/// What the dependents expansion of one ingestion did.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct Expansion {
    /// Unchanged files read again for their edges.
    pub files: usize,
    /// More than [`MAX_DEPENDENTS`] were needed: none was read, and edges
    /// from unchanged files into the changed files' nodes may be lost
    /// until a full rebuild.
    pub capped: bool,
}

/// The files `source` cites `node` in.
fn cited_in<'a>(node: &'a Map<String, Value>, source: &'a str) -> impl Iterator<Item = &'a str> {
    node.get("citations")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
        .filter(move |citation| {
            citation.get("source_toolkit").and_then(Value::as_str) == Some(source)
        })
        .filter_map(|citation| citation.get("file_path").and_then(Value::as_str))
}

/// The files other than `changed` that hold an edge into a node one of
/// `changed` cites: the files citing the edge's source node, and the files
/// the edge was discovered in. Removing `changed` from the graph removes
/// such edges; reading these files again restores them.
fn dependents(graph: &Graph, source: &str, changed: &HashSet<&str>) -> BTreeSet<String> {
    if changed.is_empty() {
        return BTreeSet::new();
    }
    let defined: HashSet<&str> = graph
        .nodes()
        .filter(|(_, node)| cited_in(node, source).any(|file| changed.contains(file)))
        .map(|(id, _)| id)
        .collect();
    let mut out = BTreeSet::new();
    if defined.is_empty() {
        return out;
    }
    for (from, to, edge) in graph.edges() {
        if !defined.contains(to) {
            continue;
        }
        if let Some(node) = graph.node(from) {
            out.extend(cited_in(node, source).map(str::to_owned));
        }
        for entry in relation_provenance(edge) {
            if entry.get("source_toolkit").and_then(Value::as_str) == Some(source)
                && let Some(file) = entry.get("discovered_in_file").and_then(Value::as_str)
            {
                out.insert(file.to_owned());
            }
        }
    }
    out.retain(|file| !changed.contains(file.as_str()));
    out
}

/// Some of the listing: what `prepare` sees for one batch.
struct Subset<'a> {
    inner: &'a LocalFolderSource,
    listed: Vec<DocumentRef>,
}

impl ContentSource for Subset<'_> {
    async fn list(&self) -> Result<Vec<DocumentRef>, SourceError> {
        Ok(self.listed.clone())
    }

    async fn fetch(&self, key: &str) -> Result<Document, SourceError> {
        self.inner.fetch_now(key)
    }
}

/// The batches of a listing: each parsed language's files together, the
/// others [`PLAIN_BATCH`] at a time, in listing order within each.
fn batches(listed: Vec<DocumentRef>) -> Vec<Vec<DocumentRef>> {
    let mut by_language: BTreeMap<&'static str, Vec<DocumentRef>> = BTreeMap::new();
    let mut plain = Vec::new();
    for reference in listed {
        match parse::language_of(&reference.key) {
            Some(language) => by_language.entry(language).or_default().push(reference),
            None => plain.push(reference),
        }
    }
    let mut out: Vec<Vec<DocumentRef>> = by_language.into_values().collect();
    let mut rest = plain.into_iter().peekable();
    while rest.peek().is_some() {
        out.push(rest.by_ref().take(PLAIN_BATCH).collect());
    }
    out
}

fn merge(total: &mut Outcome, batch: Outcome) {
    total.unchanged += batch.unchanged;
    total.removed_files += batch.removed_files;
    total.skipped_whitelist += batch.skipped_whitelist;
    total.skipped_blacklist += batch.skipped_blacklist;
    total.skipped_unsupported += batch.skipped_unsupported;
    total.skipped_empty += batch.skipped_empty;
    total.skipped_unreadable += batch.skipped_unreadable;
    total.hashes.extend(batch.hashes);
    total.documents.extend(batch.documents);
}

/// `previous` with the dependents of what changed or went since the last
/// build at a version no read matches, so each is fetched, removed from the
/// graph and parsed again as a changed file is (unchanged when there are
/// none, or more than [`MAX_DEPENDENTS`]).
fn expand<'a>(
    graph: &Graph,
    source: &str,
    listed: &[DocumentRef],
    previous: &'a BTreeMap<String, String>,
) -> (Cow<'a, BTreeMap<String, String>>, Expansion) {
    let versions: HashMap<&str, &str> = listed
        .iter()
        .map(|reference| (reference.key.as_str(), reference.version.as_str()))
        .collect();
    let changed: HashSet<&str> = previous
        .iter()
        .filter(|(key, version)| versions.get(key.as_str()) != Some(&version.as_str()))
        .map(|(key, _)| key.as_str())
        .collect();
    let found = dependents(graph, source, &changed);
    if found.is_empty() {
        return (Cow::Borrowed(previous), Expansion::default());
    }
    if found.len() > MAX_DEPENDENTS {
        let capped = Expansion {
            files: 0,
            capped: true,
        };
        return (Cow::Borrowed(previous), capped);
    }
    let mut forced = previous.clone();
    let mut files = 0;
    for file in &found {
        if let Some(version) = forced.get_mut(file) {
            version.clear();
            files += 1;
        }
    }
    let expansion = Expansion {
        files,
        capped: false,
    };
    (Cow::Owned(forced), expansion)
}

/// Ingest `source` into `graph` batch by batch; `stage` is called after
/// each batch is read (to move the chunks the reads staged into the store).
/// The unchanged files with edges into what changed are read again too
/// (see the module doc).
///
/// # Errors
///
/// The folder cannot be listed, a stop was requested, or `stage` failed.
pub async fn ingest(
    graph: &mut Graph,
    selection: &SourceSelection,
    source: &LocalFolderSource,
    root: &Path,
    previous: &BTreeMap<String, String>,
    context: &Context,
    mut stage: impl FnMut() -> Result<(), EngineError>,
) -> Result<(Outcome, Expansion), EngineError> {
    let listed = source
        .list_now()
        .map_err(|error| EngineError::new(ErrorType::Runtime, error.to_string()))?;
    let (previous, expansion) = expand(graph, &selection.name, &listed, previous);
    let previous: &BTreeMap<String, String> = &previous;
    let mut total = Outcome::default();
    // Each listed file's place in the listing: the order a single pass
    // adds files in (restored at the end).
    let order: HashMap<String, usize> = listed
        .iter()
        .enumerate()
        .map(|(rank, reference)| (reference.key.clone(), rank))
        .collect();
    // The nodes this run added, by the rank of the file that added them.
    let mut added: HashMap<String, usize> = HashMap::new();
    // Gone from the folder: out of the graph (a file still listed but no
    // longer readable is removed by its batch's `prepare`).
    let keys: HashSet<&str> = listed
        .iter()
        .map(|reference| reference.key.as_str())
        .collect();
    for gone in previous.keys().filter(|key| !keys.contains(key.as_str())) {
        graph.remove_file(&selection.name, gone);
        total.removed_files += 1;
    }
    drop(keys);
    let to_read = listed
        .iter()
        .filter(|reference| previous.get(&reference.key) != Some(&reference.version))
        .count();
    if to_read > 0 {
        context.thinking(format!("[extract] Parsing {to_read} files"));
    }
    // The parse's own "[extract]" line would count one batch: it goes
    // nowhere (the stop is shared).
    let quiet = {
        let (lines, _) = tokio::sync::mpsc::unbounded_channel();
        Context::new(lines, context.stop_signal())
    };
    let mut relations = Vec::new();
    for listed in batches(listed) {
        context.checkpoint()?;
        let batch_previous: BTreeMap<String, String> = listed
            .iter()
            .filter_map(|reference| {
                previous
                    .get(&reference.key)
                    .map(|version| (reference.key.clone(), version.clone()))
            })
            .collect();
        let subset = Subset {
            inner: source,
            listed,
        };
        let (outcome, mut files) =
            prepare(graph, selection, &subset, &batch_previous, context).await?;
        merge(&mut total, outcome);
        stage()?;
        let mut parsed = parse_files(selection, root, &mut files, &quiet)?;
        for file in files {
            let before = graph.node_count();
            let rank = order.get(&file.path).copied().unwrap_or(usize::MAX);
            let one: BTreeMap<_, _> = parsed
                .remove(&file.path)
                .map(|extraction| (file.path.clone(), extraction))
                .into_iter()
                .collect();
            let (found, entities) = assemble(
                graph,
                selection,
                std::slice::from_ref(&file),
                &one,
                &HashMap::new(),
                &mut total,
                context,
            )?;
            relations.extend(found.into_iter().map(|relation| (rank, relation)));
            // A node this file added again (another file added it first)
            // sits where the file listed first put it.
            for ids in entities.values() {
                for (id, _, _) in ids {
                    if let Some(first) = added.get_mut(id) {
                        *first = (*first).min(rank);
                    }
                }
            }
            for id in graph.ids_from(before) {
                added.insert(id.to_owned(), rank);
            }
        }
    }
    // Back in single-pass order: the batches added languages one after
    // another, and the parser relations resolve names over node order.
    graph.sort_ranked_last(&added);
    drop(added);
    relations.sort_by_key(|(rank, _)| *rank);
    let relations: Vec<_> = relations
        .into_iter()
        .map(|(_, relation)| relation)
        .collect();
    add_parser_relations(graph, &relations, selection, &mut total, context);
    drop(relations);
    total.pruned_edges += quality_pass(graph);
    Ok((total, expansion))
}

#[cfg(test)]
mod tests {
    use super::*;
    use elitea_content_source::Acl;

    fn reference(key: &str) -> DocumentRef {
        DocumentRef {
            key: key.into(),
            version: "v".into(),
            mime: String::new(),
            size: 1,
            acl: Acl::Project,
        }
    }

    #[test]
    fn each_language_is_one_batch_and_plain_files_are_bounded() {
        let mut listed = vec![reference("a.py"), reference("b.rs"), reference("c.py")];
        listed.extend((0..PLAIN_BATCH + 3).map(|n| reference(&format!("notes/{n}.txt"))));
        let out = batches(listed);
        let keys: Vec<Vec<&str>> = out
            .iter()
            .map(|batch| batch.iter().map(|r| r.key.as_str()).collect())
            .collect();
        assert!(keys.contains(&vec!["a.py", "c.py"]), "{keys:?}");
        assert!(keys.contains(&vec!["b.rs"]), "{keys:?}");
        let plain: Vec<usize> = out
            .iter()
            .filter(|batch| batch[0].key.starts_with("notes/"))
            .map(Vec::len)
            .collect();
        assert_eq!(plain, [PLAIN_BATCH, 3]);
        assert_eq!(out.iter().map(Vec::len).sum::<usize>(), PLAIN_BATCH + 6);
    }
}
