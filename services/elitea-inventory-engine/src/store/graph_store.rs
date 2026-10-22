//! The PostgreSQL store behind the shared core's [`GraphStore`] contract
//! (ADR-0029 decision 7): a thin adapter over this module's free
//! functions, which stay what ingestion's `run` and `transfer` call.
//!
//! The entity vectors are in Qdrant behind elitea-vector (`super::vectors`):
//! a store built [`PgGraphStore::with_vectors`] (an invocation's bearer)
//! publishes them on every write and ranks through elitea-vector; one
//! without vector access writes no generation (the first ranking with access
//! backfills it) and refuses to rank a graph that has vectors.

use super::sources::{self, Completion, Lease, SourceStatus};
use super::vectors::{self, EntityVectors, Ranking};
use super::{GraphKey, Imported, StoreError};
use crate::graph::Graph;
use elitea_content_source::Acl;
use elitea_inventory_core::store::{GraphRead, GraphStamp, GraphStore};
use serde_json::Value;
use sqlx::postgres::PgPool;
use std::collections::BTreeMap;

/// The graph store over one PostgreSQL pool. Cloning shares the pool.
#[derive(Debug, Clone)]
pub struct PgGraphStore {
    pool: PgPool,
    vectors: Option<EntityVectors>,
}

impl PgGraphStore {
    /// The store over `pool`, without vector access.
    #[must_use]
    pub fn new(pool: PgPool) -> Self {
        Self {
            pool,
            vectors: None,
        }
    }

    /// The same store with (or, `None`, without) vector access: one
    /// invocation's.
    #[must_use]
    pub fn with_vectors(mut self, vectors: Option<EntityVectors>) -> Self {
        self.vectors = vectors;
        self
    }

    /// The store's vector access, if any.
    #[must_use]
    pub fn vectors(&self) -> Option<&EntityVectors> {
        self.vectors.as_ref()
    }

    /// The pool the store reads and writes.
    #[must_use]
    pub fn pool(&self) -> &PgPool {
        &self.pool
    }
}

impl GraphStore for PgGraphStore {
    type Error = StoreError;
    type Lease = Lease;

    async fn lease(&self, key: GraphKey) -> Result<Option<Lease>, StoreError> {
        sources::lease(&self.pool, key).await
    }

    async fn load(&self, key: GraphKey) -> Result<Option<(Graph, i64)>, StoreError> {
        super::load(&self.pool, key).await
    }

    async fn load_view(&self, key: GraphKey) -> Result<Option<GraphRead>, StoreError> {
        super::load_view(&self.pool, key).await
    }

    async fn revision(&self, key: GraphKey) -> Result<Option<i64>, StoreError> {
        super::revision(&self.pool, key).await
    }

    async fn stamp(&self, key: GraphKey) -> Result<Option<GraphStamp>, StoreError> {
        super::stamp(&self.pool, key).await
    }

    async fn document_versions(
        &self,
        key: GraphKey,
        source_name: &str,
    ) -> Result<BTreeMap<String, String>, StoreError> {
        sources::document_versions(&self.pool, key, source_name).await
    }

    async fn restricted_documents(
        &self,
        key: GraphKey,
    ) -> Result<Vec<(String, String, Acl)>, StoreError> {
        sources::restricted_documents(&self.pool, key).await
    }

    async fn start(&self, key: GraphKey, status: &SourceStatus) -> Result<(), StoreError> {
        sources::start(&self.pool, key, status).await
    }

    async fn fail(&self, key: GraphKey, toolkit_id: &str, error: &str) -> Result<(), StoreError> {
        sources::fail(&self.pool, key, toolkit_id, error).await
    }

    async fn complete(
        &self,
        key: GraphKey,
        graph: &Graph,
        completion: &Completion<'_>,
    ) -> Result<i64, StoreError> {
        sources::complete_with(&self.pool, key, graph, completion, self.vectors.as_ref()).await
    }

    async fn status_document(&self, key: GraphKey) -> Result<Value, StoreError> {
        sources::status_document(&self.pool, key).await
    }

    async fn rank(
        &self,
        key: GraphKey,
        vector: &[f64],
        min_score: f64,
    ) -> Result<Ranking, StoreError> {
        vectors::rank(&self.pool, key, vector, min_score, self.vectors.as_ref()).await
    }

    /// With vector access, the graph's namespace is emptied too (after the
    /// rows' commit; a failure is logged: the platform's own deletion of a
    /// toolkit or project removes the namespace again).
    async fn delete(&self, key: GraphKey) -> Result<bool, StoreError> {
        let deleted = super::delete(&self.pool, key).await?;
        if let Some(vectors) = &self.vectors
            && let Err(error) = vectors.remove_all(key).await
        {
            tracing::warn!(
                project_id = key.project_id,
                application_id = key.application_id,
                %error,
                "the deleted graph's entity vectors were not removed"
            );
        }
        Ok(deleted)
    }

    async fn save(&self, key: GraphKey, graph: &Graph) -> Result<i64, StoreError> {
        super::save_with(&self.pool, key, graph, self.vectors.as_ref()).await
    }

    async fn remove_source(
        &self,
        key: GraphKey,
        graph: &Graph,
        toolkit_id: &str,
        source_name: &str,
    ) -> Result<i64, StoreError> {
        sources::remove_with(
            &self.pool,
            key,
            graph,
            toolkit_id,
            source_name,
            self.vectors.as_ref(),
        )
        .await
    }

    async fn import(
        &self,
        key: GraphKey,
        graph: &Graph,
        replace_state: bool,
    ) -> Result<Imported, StoreError> {
        super::import_with(&self.pool, key, graph, replace_state, self.vectors.as_ref()).await
    }
}
