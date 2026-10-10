//! A graph store, without the store: the [`GraphStore`] contract the
//! Inventory engine's PostgreSQL store and the desktop's local index both
//! implement, and the data either keeps beside the graph.
//!
//! The trait's methods return futures (`impl Future + Send`, as
//! `ContentSource` does), so it needs no `dyn` and no `async-trait`: a
//! caller is generic over the store.
//!
//! The `test-support` feature adds `store_conformance`, the suite every
//! implementation runs against itself, so two stores cannot drift apart
//! unnoticed (ADR-0029, "two retrieval stores must answer the same
//! retrieval goldens").

use crate::graph::Graph;
use elitea_content_source::Acl;
use serde_json::{Map, Value};
use std::collections::BTreeMap;
use std::future::Future;

/// Which graph: the platform project and the Inventory toolkit
/// (`application_id`), as the sub-application host sends them.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, PartialOrd, Ord)]
pub struct GraphKey {
    pub project_id: i64,
    pub application_id: i64,
}

/// A graph address that is not a pair of positive integers; the message
/// says which.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct InvalidKey(pub String);

impl std::fmt::Display for InvalidKey {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::error::Error for InvalidKey {}

impl GraphKey {
    /// The one graph of a store that holds a single graph (the desktop's
    /// index of one workspace folder).
    pub const LOCAL: Self = Self {
        project_id: 1,
        application_id: 1,
    };

    /// A key from two positive ids.
    ///
    /// # Errors
    ///
    /// [`InvalidKey`] for an id below 1.
    pub fn new(project_id: i64, application_id: i64) -> Result<Self, InvalidKey> {
        if project_id < 1 || application_id < 1 {
            return Err(InvalidKey(format!(
                "a graph is addressed by a positive project id and toolkit id, got {project_id} and {application_id}"
            )));
        }
        Ok(Self {
            project_id,
            application_id,
        })
    }

    /// The key of a call's `project_id` and `application_id` arguments:
    /// integers, or strings of one (the host forwards what the request
    /// carried).
    ///
    /// # Errors
    ///
    /// [`InvalidKey`] naming the missing or malformed argument.
    pub fn from_arguments(arguments: &Map<String, Value>) -> Result<Self, InvalidKey> {
        let id = |name: &str| -> Result<i64, InvalidKey> {
            let parsed = match arguments.get(name) {
                Some(Value::Number(number)) => number.as_i64(),
                Some(Value::String(text)) => text.trim().parse().ok(),
                _ => None,
            };
            parsed.ok_or_else(|| {
                InvalidKey(format!(
                    "the call carries no integer {name}, so it names no graph"
                ))
            })
        };
        Self::new(id("project_id")?, id("application_id")?)
    }
}

/// What one source's status row says.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct SourceStatus {
    /// The status key: the source toolkit's id, as text.
    pub toolkit_id: String,
    pub toolkit_name: String,
    pub toolkit_type: String,
    pub branch: Option<String>,
}

/// The counts a finished run records.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct RunCounts {
    pub entities: i64,
    pub relations: i64,
    pub documents: i64,
}

/// What the store keeps of one document of a source (ADR-0028): its
/// version, media type and readers.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DocumentState {
    pub version: String,
    pub mime: String,
    pub acl: Acl,
}

/// What a completed run commits.
#[derive(Debug, Clone)]
pub struct Completion<'a> {
    pub toolkit_id: &'a str,
    pub source_name: &'a str,
    /// Every document the source now has.
    pub documents: &'a BTreeMap<String, DocumentState>,
    pub counts: RunCounts,
    pub commit_sha: Option<&'a str>,
}

/// A similarity ranking: `(entity id, cosine similarity)` best first, or
/// numpy's message when an entity's vector has another width than the
/// query's (Python's `semantic_search` raised it, the wrapper printed it).
pub type Ranking = std::result::Result<Vec<(String, f64)>, String>;

/// Where graphs are kept, by [`GraphKey`], with the ingestion state of
/// their sources.
///
/// Every method is one atomic step; [`GraphStore::complete`] is the one
/// that commits a run, and commits all of it or nothing.
pub trait GraphStore: Send + Sync {
    /// A storage failure.
    type Error: std::error::Error + Send + Sync + 'static;
    /// The right to ingest into one graph, held until dropped.
    type Lease: Send;

    /// Take the ingestion lease of `key`, or `None` while another holder
    /// has it: two runs on one graph would each replace it with their own
    /// copy, the later erasing the earlier.
    fn lease(
        &self,
        key: GraphKey,
    ) -> impl Future<Output = Result<Option<Self::Lease>, Self::Error>> + Send;

    /// The stored graph and its revision, or `None` when there is none.
    /// Nodes and edges come back in the order they were stored.
    fn load(
        &self,
        key: GraphKey,
    ) -> impl Future<Output = Result<Option<(Graph, i64)>, Self::Error>> + Send;

    /// The stored graph's revision, or `None` when there is no graph: a
    /// cheap check of whether a cached copy is still current.
    fn revision(
        &self,
        key: GraphKey,
    ) -> impl Future<Output = Result<Option<i64>, Self::Error>> + Send;

    /// The version of every document the source's last completed run read,
    /// by key.
    fn document_versions(
        &self,
        key: GraphKey,
        source_name: &str,
    ) -> impl Future<Output = Result<BTreeMap<String, String>, Self::Error>> + Send;

    /// Every restricted document of the graph: `(source name, key, acl)`.
    fn restricted_documents(
        &self,
        key: GraphKey,
    ) -> impl Future<Output = Result<Vec<(String, String, Acl)>, Self::Error>> + Send;

    /// The source is `in_progress` from now; its previous counts stay until
    /// the run finishes.
    fn start(
        &self,
        key: GraphKey,
        status: &SourceStatus,
    ) -> impl Future<Output = Result<(), Self::Error>> + Send;

    /// The run ended in `error`, saying why.
    fn fail(
        &self,
        key: GraphKey,
        toolkit_id: &str,
        error: &str,
    ) -> impl Future<Output = Result<(), Self::Error>> + Send;

    /// Commit a completed run in ONE transaction: the graph, the source's
    /// document versions, and its `completed` status. A failure leaves the
    /// previous graph, versions and status in place. Returns the graph's
    /// new revision.
    fn complete(
        &self,
        key: GraphKey,
        graph: &Graph,
        completion: &Completion<'_>,
    ) -> impl Future<Output = Result<i64, Self::Error>> + Send;

    /// The `sources_status.json` document of the graph:
    /// `{sources: {toolkit_id: {...}}, last_modified}`.
    fn status_document(
        &self,
        key: GraphKey,
    ) -> impl Future<Output = Result<Value, Self::Error>> + Send;

    /// The graph's entities whose vector is at least `min_score` similar to
    /// `vector`, best first, ties by entity id. A zero query vector ranks
    /// nothing; a vector of another width than the graph's is numpy's
    /// message.
    fn rank(
        &self,
        key: GraphKey,
        vector: &[f64],
        min_score: f64,
    ) -> impl Future<Output = Result<Ranking, Self::Error>> + Send;

    /// Delete the graph and its sources' state; `true` when there was a
    /// graph.
    fn delete(&self, key: GraphKey) -> impl Future<Output = Result<bool, Self::Error>> + Send;
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn keys_are_positive_integers_from_numbers_or_digit_strings() {
        let key = |arguments: Value| {
            GraphKey::from_arguments(arguments.as_object().unwrap_or(&Map::new()))
        };
        assert_eq!(
            key(json!({"project_id": 3, "application_id": "17"})).ok(),
            GraphKey::new(3, 17).ok()
        );
        for refused in [
            json!({"project_id": 3}),
            json!({"project_id": "x", "application_id": 1}),
            json!({"project_id": 0, "application_id": 1}),
            json!({"project_id": 1.5, "application_id": 1}),
        ] {
            assert!(key(refused.clone()).is_err(), "{refused}");
        }
        assert_eq!(GraphKey::new(1, 1).ok(), Some(GraphKey::LOCAL));
    }
}
