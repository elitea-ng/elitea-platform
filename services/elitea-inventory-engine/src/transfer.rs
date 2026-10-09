//! `graph.json` in and out of the store: the operator's `import-graph`
//! (issue #1129, a Python engine's graph moved into PostgreSQL once, when a
//! deployment switches to this engine) and `export-graph` (ADR-0027 §3:
//! `graph.json` stays an export, produced on demand).
//!
//! The document is the Python engine's node-link `graph.json`
//! ([`Graph::from_node_link`], [`Graph::to_json_text`]). An import keeps
//! everything the document holds (nodes in order, edges, `graph`,
//! `_metadata` including the embedding stamp, `_schema`), so an export of
//! an imported graph is the imported document again, except:
//!
//! * `_metadata.last_saved` is the export's time (Python stamped every save);
//! * `_indices` are derived from the nodes, as Python's `_rebuild_indices`
//!   derives them (a document whose stored indices drifted exports the
//!   corrected ones), their ids in node order (Python wrote `list(set)`, in
//!   string-hash order, so its own saves of one graph differ there too);
//! * relation types are lowercase (Python's load lowercased them too).
//!
//! What an import refuses, before anything is written: a document that is
//! not JSON, an undirected graph, a multigraph, a node or link without a
//! string id, text holding a NUL character (PostgreSQL `jsonb` cannot store
//! it; the location is named), and — unless asked to replace it — a graph
//! that already has native ingestion state (see [`store::import`]).
//!
//! Edge provenance is NOT recoverable: networkx overwrote every edge's own
//! `source` attribute (`parser`, `llm`) with the source node id when Python
//! saved, so an imported edge has no `source` attribute of its own (the
//! document's `source` is read as the edge's endpoint), and an export
//! writes the endpoint there again.

use crate::graph::Graph;
use crate::store::{self, GraphKey, Imported, sources};
use serde_json::Value;
use sqlx::postgres::PgPool;

/// Why an import or export did not happen. The message is the operator's.
#[derive(Debug, thiserror::Error)]
pub enum TransferError {
    /// The document cannot be imported as it is.
    #[error("the graph document cannot be imported: {0}")]
    Document(String),
    /// The graph is busy or has state an import would contradict.
    #[error("{0}")]
    Refused(String),
    /// There is no stored graph to export.
    #[error("no graph is stored for project {project_id}, toolkit {application_id}")]
    NotFound {
        project_id: i64,
        application_id: i64,
    },
    #[error(transparent)]
    Store(#[from] store::StoreError),
}

/// What an import stored.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ImportReport {
    pub entities: usize,
    pub relations: usize,
    pub revision: i64,
    /// The graph's embedding stamp (`_metadata.embeddings_model`), if any:
    /// semantic search needs the same model configured.
    pub embeddings_model: Option<String>,
}

/// Where in `value` a NUL character is, as a JSON-pointer-like path, or
/// `None`.
#[must_use]
pub fn nul_location(value: &Value) -> Option<String> {
    fn walk(value: &Value, path: &mut Vec<String>) -> bool {
        match value {
            Value::String(text) => text.contains('\0'),
            Value::Array(items) => items.iter().enumerate().any(|(index, item)| {
                path.push(index.to_string());
                let found = walk(item, path);
                if !found {
                    path.pop();
                }
                found
            }),
            Value::Object(fields) => fields.iter().any(|(key, item)| {
                path.push(key.replace('\0', "\\u0000"));
                let found = key.contains('\0') || walk(item, path);
                if !found {
                    path.pop();
                }
                found
            }),
            _ => false,
        }
    }
    let mut path = Vec::new();
    walk(value, &mut path).then(|| format!("/{}", path.join("/")))
}

/// Parse a `graph.json` text into a graph, refusing what cannot be stored.
///
/// # Errors
///
/// [`TransferError::Document`].
pub fn parse_document(text: &str) -> Result<Graph, TransferError> {
    let document: Value = serde_json::from_str(text)
        .map_err(|e| TransferError::Document(format!("it is not JSON ({e})")))?;
    if let Some(location) = nul_location(&document) {
        return Err(TransferError::Document(format!(
            "it holds a NUL character at {location}, which PostgreSQL cannot store; remove it from the document and import again"
        )));
    }
    Graph::from_node_link(&document).map_err(|e| TransferError::Document(e.0))
}

/// `import-graph`: store `text` as the graph `key`, replacing any stored
/// graph (idempotent: a re-run replaces it again and bumps its revision).
/// Under the ingestion lease, so it cannot interleave with a run.
///
/// # Errors
///
/// The document is refused, an ingestion holds the lease, the graph has
/// native ingestion state and `replace_state` is false, or the store fails.
pub async fn import_graph(
    pool: &PgPool,
    key: GraphKey,
    text: &str,
    replace_state: bool,
) -> Result<ImportReport, TransferError> {
    let graph = parse_document(text)?;
    let Some(_lease) = sources::lease(pool, key).await? else {
        return Err(TransferError::Refused(
            "an ingestion of this Inventory toolkit is running; import when it finishes".to_owned(),
        ));
    };
    match store::import(pool, key, &graph, replace_state).await? {
        Imported::Saved { revision } => Ok(ImportReport {
            entities: graph.node_count(),
            relations: graph.edge_count(),
            revision,
            embeddings_model: graph
                .metadata
                .get("embeddings_model")
                .and_then(Value::as_str)
                .map(str::to_owned),
        }),
        Imported::HasIngestionState { sources, documents } => Err(TransferError::Refused(format!(
            "project {}, toolkit {} already has native ingestion state ({sources} source(s), {documents} document version(s)); importing over it would leave versions and ACLs that do not describe the imported graph. Re-run with --replace-ingestion-state (the import_graph tool: replace_ingestion_state=true) to delete that state with the old graph",
            key.project_id, key.application_id
        ))),
    }
}

/// `export-graph`: the stored graph `key` as Python-compatible `graph.json`
/// text (`json.dump(indent=2)`), stamped `saved_at`.
///
/// # Errors
///
/// No graph is stored, or the store fails.
pub async fn export_graph(
    pool: &PgPool,
    key: GraphKey,
    saved_at: &str,
) -> Result<String, TransferError> {
    export_report(pool, key, saved_at)
        .await
        .map(|report| report.document)
}

/// What an export produced: the document and its counts.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ExportReport {
    pub document: String,
    pub entities: usize,
    pub relations: usize,
    /// The stored graph's revision the document was read at.
    pub revision: i64,
}

/// The key `export_graph` writes in the toolkit's bucket.
pub const EXPORT_ARTIFACT: &str = "graph.json";

/// The bucket the host uploads an export to: the toolkit's `bucket` (or
/// the UI's `toolkit_configuration_bucket`), `graphs` when it names none —
/// the host's `run.ResolveBucket`.
#[must_use]
pub fn export_bucket(params: &serde_json::Map<String, Value>) -> String {
    ["bucket", "toolkit_configuration_bucket"]
        .iter()
        .find_map(|key| {
            params
                .get(*key)
                .and_then(Value::as_str)
                .map(str::trim)
                .filter(|bucket| !bucket.is_empty())
        })
        .unwrap_or("graphs")
        .to_owned()
}

/// The `export_graph` answer: a summary of what went to the bucket. The
/// document itself rides only as the artifact the host uploads; the
/// answer never repeats it (a 50-file repository's graph is 43 MB, and an
/// answer is what a poll returns and a chat shows).
#[must_use]
pub fn export_summary(
    bucket: &str,
    entities: usize,
    relations: usize,
    revision: Option<i64>,
    size_bytes: usize,
) -> (Value, String) {
    let document = serde_json::json!({
        "artifact": EXPORT_ARTIFACT,
        "bucket": bucket,
        "entities": entities,
        "relations": relations,
        "revision": revision,
        "size_bytes": size_bytes,
    });
    let at = revision.map_or_else(String::new, |revision| format!(" (revision {revision})"));
    let text = format!(
        "Exported {entities} entities and {relations} relations{at} to {EXPORT_ARTIFACT} in bucket {bucket} ({size_bytes} bytes)."
    );
    (document, text)
}

/// [`export_graph`] with the counts the `export_graph` tool reports.
///
/// # Errors
///
/// No graph is stored, or the store fails.
pub async fn export_report(
    pool: &PgPool,
    key: GraphKey,
    saved_at: &str,
) -> Result<ExportReport, TransferError> {
    let (graph, revision) = store::load(pool, key)
        .await?
        .ok_or(TransferError::NotFound {
            project_id: key.project_id,
            application_id: key.application_id,
        })?;
    Ok(ExportReport {
        document: graph.to_json_text(saved_at),
        entities: graph.node_count(),
        relations: graph.edge_count(),
        revision,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn an_export_answers_a_summary_and_never_the_document() {
        let params = json!({"toolkit_configuration_bucket": " legacy "});
        assert_eq!(
            export_bucket(params.as_object().unwrap_or(&serde_json::Map::new())),
            "legacy"
        );
        assert_eq!(export_bucket(&serde_json::Map::new()), "graphs");
        let (document, text) = export_summary("graphs", 761, 1324, Some(12), 43_297_981);
        assert_eq!(
            document,
            json!({"artifact": "graph.json", "bucket": "graphs", "entities": 761, "relations": 1324, "revision": 12, "size_bytes": 43_297_981})
        );
        assert_eq!(
            text,
            "Exported 761 entities and 1324 relations (revision 12) to graph.json in bucket graphs (43297981 bytes)."
        );
        let (document, text) = export_summary("graphs", 6, 5, None, 10);
        assert_eq!(document["revision"], Value::Null);
        assert_eq!(
            text,
            "Exported 6 entities and 5 relations to graph.json in bucket graphs (10 bytes)."
        );
    }

    #[test]
    fn a_nul_is_located() {
        assert_eq!(nul_location(&json!({"nodes": [{"id": "a"}]})), None);
        assert_eq!(
            nul_location(&json!({"nodes": [{"id": "a"}, {"id": "b", "name": "x\u{0}y"}]})),
            Some("/nodes/1/name".to_owned())
        );
        assert!(
            parse_document(r#"{"nodes": [{"id": "a\u0000"}]}"#)
                .is_err_and(|e| e.to_string().contains("NUL character at /nodes/0/id"))
        );
    }

    #[test]
    fn what_the_store_cannot_hold_is_refused_with_its_reason() {
        for (text, needle) in [
            ("not json", "it is not JSON"),
            (r#"{"directed": false}"#, "the graph is undirected"),
            (r#"{"multigraph": true}"#, "the graph is a multigraph"),
            (r#"{"nodes": [{"id": 1}]}"#, "non-string `id`"),
        ] {
            let refused = parse_document(text);
            assert!(
                refused
                    .as_ref()
                    .is_err_and(|e| e.to_string().contains(needle)
                        && e.to_string()
                            .starts_with("the graph document cannot be imported")),
                "{text}: {refused:?}"
            );
        }
    }
}
