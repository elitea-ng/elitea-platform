//! `elitea.vector.v1.VectorService`.
//!
//! Each handler does the same four steps, in this order:
//!
//! 1. admit the caller ([`Authenticator::authenticate`]);
//! 2. resolve the project from the caller, never from the request alone
//!    ([`Caller::project`]), and refuse a source the caller's token is not
//!    admitted for ([`Caller::require_source`]);
//! 3. validate the request's shape (space, dimension, namespace, filter);
//! 4. send Qdrant a filter whose first `must` condition is that project
//!    ([`Scope`]).

use std::collections::{BTreeMap, HashMap, HashSet};
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};

use qdrant_client::Payload;
use qdrant_client::qdrant::point_id::PointIdOptions;
use qdrant_client::qdrant::value::Kind;
use qdrant_client::qdrant::{
    Condition, CountPointsBuilder, DeletePointsBuilder, Document, FacetCountsBuilder, Filter,
    NamedVectors, PayloadIncludeSelector, PointId, PointStruct, PrefetchQueryBuilder, Query,
    QueryPointsBuilder, Rrf, ScrollPointsBuilder, SetPayloadPointsBuilder, UpsertPointsBuilder,
    Value, Vector, VectorsSelector, vector_output, with_vectors_selector,
};
use tokio_stream::StreamExt as _;
use tonic::{Request, Response, Status, Streaming};

use crate::auth::{Authenticator, Caller, MemoryBinding, MemoryIntent};
use crate::fanout::fan_out;
use crate::layout::{self, BM25_MODEL, BM25_VECTOR, DENSE_VECTOR, Space, key};
use crate::pb;
use crate::scope::{self, Scope};
use crate::store::{Store, unavailable};

/// The most points in one Upsert batch.
pub const MAX_BATCH_POINTS: usize = 512;
/// The most results one search returns.
pub const MAX_LIMIT: u32 = 1000;
/// The longest `text` of a point, and of a hybrid query.
const MAX_TEXT_BYTES: usize = 1 << 20;
const MAX_QUERY_TEXT_BYTES: usize = 8 << 10;
/// The longest `metadata_json` of a point.
const MAX_METADATA_BYTES: usize = 64 << 10;
/// The longest key-like string field of a point.
const MAX_KEY_BYTES: usize = 1024;
/// The most `acl` entries of a point.
const MAX_ACL_ENTRIES: usize = 256;
/// Facet buckets read per (collection, source) by `ListIndexes`.
const MAX_LISTED_NAMESPACES: u64 = 10_000;
/// The RRF constant (ADR-0031: the `DeepWiki` k = 60).
const RRF_K: u32 = 60;
/// ADR-0031 decision 4: 0.7 dense, 0.3 text.
const DEFAULT_DENSE_WEIGHT: f32 = 0.7;
const DEFAULT_TEXT_WEIGHT: f32 = 0.3;

/// The default of [`SpaceLimits::max_dimension`]: halfvec-scale models (up
/// to 3072) with headroom.
pub const DEFAULT_MAX_DIMENSION: u32 = 4096;

/// What embedding spaces a caller may bring into existence. A space is a
/// Qdrant collection, a costly resource, and its name comes from the caller.
#[derive(Clone, Debug)]
pub struct SpaceLimits {
    /// The largest dimension of a space (the smallest is 1).
    pub max_dimension: u32,
    /// When set, the only spaces that may be created; a space whose
    /// collection already exists stays usable. `None` allows any space that
    /// passes the other checks.
    pub allowed: Option<HashSet<Space>>,
}

impl Default for SpaceLimits {
    fn default() -> Self {
        Self {
            max_dimension: DEFAULT_MAX_DIMENSION,
            allowed: None,
        }
    }
}

impl SpaceLimits {
    /// Refuses a dimension outside `1..=max_dimension`.
    ///
    /// # Errors
    /// `INVALID_ARGUMENT` for a dimension out of range.
    pub fn check_dimension(&self, space: &Space) -> Result<(), Status> {
        if space.dimension() == 0 || space.dimension() > self.max_dimension {
            return Err(Status::invalid_argument(format!(
                "space.dimension must be 1 to {}",
                self.max_dimension
            )));
        }
        Ok(())
    }

    /// Refuses creating a space outside the allowlist.
    ///
    /// # Errors
    /// `PERMISSION_DENIED` for a space the allowlist does not name.
    pub fn check_creatable(&self, space: &Space) -> Result<(), Status> {
        match &self.allowed {
            Some(allowed) if !allowed.contains(space) => Err(Status::permission_denied(
                "this embedding space is not enabled on this deployment",
            )),
            _ => Ok(()),
        }
    }
}

/// The service.
#[derive(Clone)]
pub struct VectorService {
    auth: Authenticator,
    store: Arc<Store>,
    limits: Arc<SpaceLimits>,
}

impl VectorService {
    /// For a MEMORY request that needs a memory decision (a personal-scope
    /// request, or a project-scope write), replaces the cached caller with one
    /// verified afresh: elitea-main then re-checks the conversation and, for a
    /// personal read, records it. Every other request keeps `caller`, which
    /// came from the claim cache (a toolkit-index search or upsert never asks
    /// elitea-main again within the cache lifetime).
    async fn memory_caller(
        &self,
        token: Option<&str>,
        caller: Caller,
        source_scope_toolkit: Option<(i32, i32, i64)>,
        write: bool,
    ) -> Result<Caller, Status> {
        let Some((source, memory_scope, toolkit_id)) = source_scope_toolkit else {
            return Ok(caller);
        };
        if source != pb::Source::Memory as i32 || !matches!(caller, Caller::Token(_)) {
            return Ok(caller);
        }
        let intent = MemoryIntent::of(memory_scope, write);
        if intent == MemoryIntent::None {
            return Ok(caller);
        }
        // Everything the cached grant can refuse is refused BEFORE elitea-main
        // is asked afresh: a personal-read introspection records the read, and
        // a request refused afterwards (a toolkit the run was not given, a bad
        // scope) must not mark the run as having read personal memory. The
        // memory toolkits and the sources are fixed at mint, so the cached
        // grant is authoritative for them. Only the write-after-read refusal is
        // left to the fresh answer (it is what the fresh answer decides).
        caller.precheck_memory(source, memory_scope, toolkit_id)?;
        let token = token.ok_or_else(|| Status::unauthenticated("a project token is required"))?;
        self.auth.fresh_token(token, intent).await
    }

    /// Re-verifies an Upsert stream's token before a batch. The introspection
    /// cache keeps this cheap (a memory write asks afresh); a token that went
    /// inactive, moved project, lost the source or whose memory binding
    /// changed (another owner, a project write refused after a personal read)
    /// ends the stream.
    async fn reverify_upsert(
        &self,
        token: &str,
        namespace: &pb::Namespace,
        project_id: i64,
        binding: Option<&MemoryBinding>,
    ) -> Result<(), Status> {
        let current = self.auth.verify_token(token).await?;
        let current = self
            .memory_caller(
                Some(token),
                current,
                Some((
                    namespace.source,
                    namespace.memory_scope,
                    namespace.toolkit_id,
                )),
                true,
            )
            .await?;
        if current.project(0)? != project_id {
            return Err(Status::permission_denied(
                "the token's project changed during the stream",
            ));
        }
        current.require_source(namespace.source)?;
        if current
            .memory_binding(
                namespace.source,
                namespace.memory_scope,
                namespace.toolkit_id,
                true,
            )?
            .as_ref()
            != binding
        {
            return Err(Status::permission_denied(
                "the token's memory owner changed during the stream",
            ));
        }
        Ok(())
    }

    /// A service over `store`, admitting callers through `auth`, with the
    /// default [`SpaceLimits`].
    #[must_use]
    pub fn new(auth: Authenticator, store: Arc<Store>) -> Self {
        Self {
            auth,
            store,
            limits: Arc::new(SpaceLimits::default()),
        }
    }

    /// Replaces the space limits.
    #[must_use]
    pub fn with_limits(mut self, limits: SpaceLimits) -> Self {
        self.limits = Arc::new(limits);
        self
    }
}

#[tonic::async_trait]
impl pb::vector_service_server::VectorService for VectorService {
    async fn upsert(
        &self,
        request: Request<Streaming<pb::UpsertRequest>>,
    ) -> Result<Response<pb::UpsertResponse>, Status> {
        let caller = self.auth.authenticate(&request).await?;
        caller.require_token()?;
        // Kept to re-verify the token before every batch: a stream can run
        // for minutes, and the claim that minted the token can settle or be
        // lost meanwhile.
        let token = Authenticator::bearer_token(&request)
            .ok_or_else(|| Status::unauthenticated("a project token is required"))?;
        let mut stream = request.into_inner();
        let header = match stream.next().await {
            Some(Ok(pb::UpsertRequest {
                message: Some(pb::upsert_request::Message::Header(header)),
            })) => header,
            Some(Err(status)) => return Err(status),
            _ => {
                return Err(Status::invalid_argument(
                    "the first Upsert message must be a header",
                ));
            }
        };
        let project_id = caller.project(header.project_id)?;
        let space = Space::from_proto(header.space.as_ref())?;
        let namespace = header
            .namespace
            .as_ref()
            .ok_or_else(|| Status::invalid_argument("namespace is required"))?;
        caller.require_source(namespace.source)?;
        // A memory write needs a memory decision made now, not a cached one.
        let caller = self
            .memory_caller(
                Some(&token),
                caller,
                Some((
                    namespace.source,
                    namespace.memory_scope,
                    namespace.toolkit_id,
                )),
                true,
            )
            .await?;
        // The owner of a memory point comes from the token, here and nowhere
        // else; a request cannot name one.
        let binding = caller.memory_binding(
            namespace.source,
            namespace.memory_scope,
            namespace.toolkit_id,
            true,
        )?;
        let target = UpsertTarget {
            project_id,
            binding: binding.clone(),
            source: layout::source_keyword(namespace.source)?,
            namespace_id: namespace.namespace_id.clone(),
            namespace: layout::namespace_uuid(&namespace.namespace_id)?,
            generation: layout::generation(&namespace.generation)?.map(str::to_owned),
            dimension: space.dimension(),
        };
        // Everything the caller chose is checked before Qdrant is asked to
        // make anything.
        self.limits.check_dimension(&space)?;
        if self.store.existing(&space).await?.is_none() {
            self.limits.check_creatable(&space)?;
        }
        let collection = self.store.ensure(&space).await?;
        let mut upserted: u64 = 0;
        while let Some(message) = stream.next().await {
            let Some(pb::upsert_request::Message::Batch(batch)) = message?.message else {
                return Err(Status::invalid_argument(
                    "only the first Upsert message is a header",
                ));
            };
            self.reverify_upsert(&token, namespace, project_id, binding.as_ref())
                .await?;
            if batch.points.len() > MAX_BATCH_POINTS {
                return Err(Status::invalid_argument("at most 512 points per batch"));
            }
            if batch.points.is_empty() {
                continue;
            }
            let points = batch
                .points
                .iter()
                .map(|point| target.point(point))
                .collect::<Result<Vec<_>, _>>()?;
            let count = points.len() as u64;
            self.store
                .client()
                .upsert_points(UpsertPointsBuilder::new(&collection, points).wait(true))
                .await
                .map_err(|error| unavailable("upsert_points", &error))?;
            upserted += count;
        }
        Ok(Response::new(pb::UpsertResponse {
            upserted,
            collection,
        }))
    }

    async fn delete(
        &self,
        request: Request<pb::DeleteRequest>,
    ) -> Result<Response<pb::DeleteResponse>, Status> {
        let caller = self.auth.authenticate(&request).await?;
        let token = Authenticator::bearer_token(&request);
        let request = request.into_inner();
        let project_id = caller.project(request.project_id)?;
        let space = request
            .space
            .as_ref()
            .map(|space| Space::from_proto(Some(space)))
            .transpose()?;
        let namespace = request
            .namespace
            .as_ref()
            .ok_or_else(|| Status::invalid_argument("namespace is required"))?;
        caller.require_source(namespace.source)?;
        let caller = self
            .memory_caller(
                token.as_deref(),
                caller,
                Some((
                    namespace.source,
                    namespace.memory_scope,
                    namespace.toolkit_id,
                )),
                true,
            )
            .await?;
        let source = layout::source_keyword(namespace.source)?;
        layout::namespace_uuid(&namespace.namespace_id)?;
        let binding = caller.memory_binding(
            namespace.source,
            namespace.memory_scope,
            namespace.toolkit_id,
            true,
        )?;
        let scope = Scope::project(project_id)
            .source(source)
            .namespaces(vec![namespace.namespace_id.clone()]);
        let scope = match &binding {
            Some(binding) => scope.bound_to(binding),
            None => scope,
        };
        let scope = match request.selector {
            Some(pb::delete_request::Selector::WholeNamespace(_)) => scope,
            Some(pb::delete_request::Selector::Documents(documents)) => {
                bounded_keys(&documents.document_keys, "document_keys")?;
                scope.with(Condition::matches(
                    key::DOCUMENT_KEY,
                    documents.document_keys,
                ))
            }
            Some(pb::delete_request::Selector::Generations(generations)) => {
                bounded_keys(&generations.generations, "generations")?;
                for generation in &generations.generations {
                    layout::generation(generation)?;
                }
                scope.with(Condition::matches(key::GENERATION, generations.generations))
            }
            Some(pb::delete_request::Selector::GenerationsExcept(except)) => {
                let keep = layout::generation(&except.keep)?
                    .ok_or_else(|| Status::invalid_argument("keep is required"))?;
                // Only points that HAVE a generation other than `keep`: a
                // point written without one (an incremental update) is not
                // a superseded build and stays.
                scope.with(Condition::from(Filter::must_not([
                    Condition::is_empty(key::GENERATION),
                    Condition::matches(key::GENERATION, keep.to_owned()),
                ])))
            }
            None => return Err(Status::invalid_argument("a selector is required")),
        };
        let collections = self.store.targets(space.as_ref()).await?;
        self.delete_where(&collections, &scope).await?;
        tracing::info!(
            project_id,
            source,
            caller = caller_label(&caller),
            "points deleted"
        );
        Ok(Response::new(pb::DeleteResponse {
            collections: count_u32(collections.len()),
        }))
    }

    async fn drop_project(
        &self,
        request: Request<pb::DropProjectRequest>,
    ) -> Result<Response<pb::DropProjectResponse>, Status> {
        let caller = self.auth.authenticate(&request).await?;
        let identity = caller.require_admin()?.to_owned();
        let project_id = caller.project(request.into_inner().project_id)?;
        let collections = self.store.targets(None).await?;
        self.delete_where(&collections, &Scope::project(project_id))
            .await?;
        tracing::info!(project_id, identity, "project vectors dropped");
        Ok(Response::new(pb::DropProjectResponse {
            collections: count_u32(collections.len()),
        }))
    }

    async fn search(
        &self,
        request: Request<pb::SearchRequest>,
    ) -> Result<Response<pb::SearchResponse>, Status> {
        let caller = self.auth.authenticate(&request).await?;
        caller.require_token()?;
        let token = Authenticator::bearer_token(&request);
        let request = request.into_inner();
        let caller = self
            .memory_caller(
                token.as_deref(),
                caller,
                request
                    .scope
                    .as_ref()
                    .map(|scope| (scope.source, scope.memory_scope, scope.toolkit_id)),
                false,
            )
            .await?;
        let (filter, space) = read_parts(
            &caller,
            request.scope.as_ref(),
            request.space.as_ref(),
            request.filter.as_ref(),
        )?;
        check_query_vector(&request.vector, &space)?;
        let limit = check_limit(request.limit)?;
        check_score_threshold(request.score_threshold)?;
        let Some(collection) = self.store.existing(&space).await? else {
            return Ok(Response::new(pb::SearchResponse::default()));
        };
        let mut query = QueryPointsBuilder::new(collection)
            .query(Query::new_nearest(request.vector))
            .using(DENSE_VECTOR)
            .filter(filter)
            .limit(u64::from(limit))
            .with_payload(true);
        if let Some(threshold) = request.score_threshold {
            query = query.score_threshold(threshold);
        }
        let response = self
            .store
            .client()
            .query(query)
            .await
            .map_err(|error| unavailable("query", &error))?;
        Ok(Response::new(pb::SearchResponse {
            points: response
                .result
                .into_iter()
                .map(|point| {
                    let mut scored = scored_point(point);
                    scored.dense_score = Some(scored.score);
                    scored
                })
                .collect(),
        }))
    }

    async fn hybrid_search(
        &self,
        request: Request<pb::HybridSearchRequest>,
    ) -> Result<Response<pb::HybridSearchResponse>, Status> {
        let caller = self.auth.authenticate(&request).await?;
        caller.require_token()?;
        let token = Authenticator::bearer_token(&request);
        let request = request.into_inner();
        let caller = self
            .memory_caller(
                token.as_deref(),
                caller,
                request
                    .scope
                    .as_ref()
                    .map(|scope| (scope.source, scope.memory_scope, scope.toolkit_id)),
                false,
            )
            .await?;
        let (filter, space) = read_parts(
            &caller,
            request.scope.as_ref(),
            request.space.as_ref(),
            request.filter.as_ref(),
        )?;
        check_query_vector(&request.vector, &space)?;
        let limit = check_limit(request.limit)?;
        if request.text.trim().is_empty() || request.text.len() > MAX_QUERY_TEXT_BYTES {
            return Err(Status::invalid_argument("text is 1 byte to 8 KiB"));
        }
        check_score_threshold(request.score_threshold)?;
        let (dense_weight, text_weight) = weights(request.dense_weight, request.text_weight)?;
        let prefetch_limit = match request.prefetch_limit {
            0 => limit.saturating_mul(4).min(MAX_LIMIT),
            value if value <= MAX_LIMIT => value,
            _ => return Err(Status::invalid_argument("prefetch_limit is at most 1000")),
        };
        let Some(collection) = self.store.existing(&space).await? else {
            return Ok(Response::new(pb::HybridSearchResponse::default()));
        };
        let mut dense = PrefetchQueryBuilder::default()
            .query(Query::new_nearest(request.vector.clone()))
            .using(DENSE_VECTOR)
            .filter(filter.clone())
            .limit(u64::from(prefetch_limit));
        if let Some(threshold) = request.score_threshold {
            dense = dense.score_threshold(threshold);
        }
        let text = PrefetchQueryBuilder::default()
            .query(Query::new_nearest(Document::new(request.text, BM25_MODEL)))
            .using(BM25_VECTOR)
            .filter(filter.clone())
            .limit(u64::from(prefetch_limit));
        let query = QueryPointsBuilder::new(collection.clone())
            .add_prefetch(dense)
            .add_prefetch(text)
            .query(Query::new_rrf(Rrf {
                k: Some(RRF_K),
                weights: vec![dense_weight, text_weight],
            }))
            // The scope again on the fused stage: defence in depth.
            .filter(filter)
            .limit(u64::from(limit))
            .with_payload(true)
            // The fused result carries only the fused score, and a
            // prefetch's own score cannot be read back from a fusion query
            // (Qdrant 1.19). The dense vector comes back with each point
            // instead, and its cosine against the query is computed here:
            // no second round trip. The vector is not returned to the
            // caller.
            .with_vectors(with_vectors_selector::SelectorOptions::Include(
                VectorsSelector {
                    names: vec![DENSE_VECTOR.to_owned()],
                },
            ));
        let response = self
            .store
            .client()
            .query(query)
            .await
            .map_err(|error| unavailable("query", &error))?;
        let points = response
            .result
            .into_iter()
            .filter_map(|point| {
                let Some(dense) = dense_vector(&point) else {
                    // Every point is written with a dense vector; a fused
                    // point without one is not a result the caller can cut
                    // by dense score, so it is left out and logged.
                    tracing::warn!(
                        point = point_id_text(point.id.clone()),
                        "a fused point came back without its dense vector"
                    );
                    return None;
                };
                let mut scored = scored_point(point);
                scored.dense_score = Some(cosine(&request.vector, &dense));
                Some(scored)
            })
            .collect();
        Ok(Response::new(pb::HybridSearchResponse { points }))
    }

    async fn count(
        &self,
        request: Request<pb::CountRequest>,
    ) -> Result<Response<pb::CountResponse>, Status> {
        let caller = self.auth.authenticate(&request).await?;
        let token = Authenticator::bearer_token(&request);
        let request = request.into_inner();
        let read = request
            .scope
            .as_ref()
            .ok_or_else(|| Status::invalid_argument("scope is required"))?;
        let project_id = caller.project(read.project_id)?;
        caller.require_source(read.source)?;
        let caller = self
            .memory_caller(
                token.as_deref(),
                caller,
                Some((read.source, read.memory_scope, read.toolkit_id)),
                false,
            )
            .await?;
        let scope = owned_read_scope(&caller, project_id, read, false)?;
        let narrowing = scope::translate(request.filter.as_ref())?;
        let space = request
            .space
            .as_ref()
            .map(|space| Space::from_proto(Some(space)))
            .transpose()?;
        let filter = scope.filter(narrowing);
        let counts = fan_out(self.store.targets(space.as_ref()).await?, |collection| {
            let filter = filter.clone();
            async move {
                let counted = self
                    .store
                    .client()
                    .count(
                        CountPointsBuilder::new(collection)
                            .filter(filter)
                            .exact(true),
                    )
                    .await
                    .map_err(|error| unavailable("count", &error))?;
                Ok(counted.result.map_or(0, |result| result.count))
            }
        })
        .await?;
        let total: u64 = counts.into_iter().sum();
        Ok(Response::new(pb::CountResponse { count: total }))
    }

    async fn list_indexes(
        &self,
        request: Request<pb::ListIndexesRequest>,
    ) -> Result<Response<pb::ListIndexesResponse>, Status> {
        let caller = self.auth.authenticate(&request).await?;
        let request = request.into_inner();
        let project_id = caller.project(request.project_id)?;
        // An unset source lists every served source the caller may use.
        let sources: Vec<&'static str> = if request.source == pb::Source::Unspecified as i32 {
            caller
                .permitted_sources(&[pb::Source::ToolkitIndex, pb::Source::Deepwiki])
                .into_iter()
                .map(|source| layout::source_keyword(source as i32))
                .collect::<Result<_, _>>()?
        } else {
            if request.source == pb::Source::Memory as i32 {
                // Memory is not an index: its namespaces are one per toolkit
                // and its points belong to people. There is nothing to list.
                return Err(Status::invalid_argument(
                    "memory is not an index and is never listed",
                ));
            }
            caller.require_source(request.source)?;
            vec![layout::source_keyword(request.source)?]
        };
        let queries: Vec<(String, Space, String)> = self
            .store
            .collections()
            .await?
            .into_iter()
            .flat_map(|(collection, space)| {
                sources
                    .iter()
                    .map(move |source| (collection.clone(), space.clone(), (*source).to_owned()))
            })
            .collect();
        let found = fan_out(
            queries,
            |(collection, space, source): (String, Space, String)| async move {
                let facet = FacetCountsBuilder::new(&collection, key::NAMESPACE_ID)
                    .filter(Scope::project(project_id).source(&source).filter(None))
                    .limit(MAX_LISTED_NAMESPACES)
                    .exact(true);
                let response = self
                    .store
                    .client()
                    .facet(facet)
                    .await
                    .map_err(|error| unavailable("facet", &error))?;
                let mut indexes = Vec::new();
                for hit in response.hits {
                    let Some(qdrant_client::qdrant::facet_value::Variant::StringValue(
                        namespace_id,
                    )) = hit.value.and_then(|value| value.variant)
                    else {
                        continue;
                    };
                    indexes.push(pb::IndexInfo {
                        space: Some(space.to_proto()),
                        collection: collection.clone(),
                        source: layout::source_from_keyword(&source) as i32,
                        namespace_id,
                        point_count: hit.count,
                    });
                }
                Ok(indexes)
            },
        )
        .await?;
        let mut indexes: Vec<pb::IndexInfo> = found.into_iter().flatten().collect();
        // Completion order is not deterministic; the answer is.
        indexes.sort_by(|left, right| {
            (&left.collection, left.source, &left.namespace_id).cmp(&(
                &right.collection,
                right.source,
                &right.namespace_id,
            ))
        });
        Ok(Response::new(pb::ListIndexesResponse { indexes }))
    }

    type ListDocumentsStream = ListDocumentsStream;

    async fn list_documents(
        &self,
        request: Request<pb::ListDocumentsRequest>,
    ) -> Result<Response<Self::ListDocumentsStream>, Status> {
        let caller = self.auth.authenticate(&request).await?;
        let token = Authenticator::bearer_token(&request);
        let request = request.into_inner();
        let project_id = caller.project(request.project_id)?;
        let namespace = request
            .namespace
            .as_ref()
            .ok_or_else(|| Status::invalid_argument("namespace is required"))?;
        caller.require_source(namespace.source)?;
        let caller = self
            .memory_caller(
                token.as_deref(),
                caller,
                Some((
                    namespace.source,
                    namespace.memory_scope,
                    namespace.toolkit_id,
                )),
                false,
            )
            .await?;
        let source = layout::source_keyword(namespace.source)?;
        layout::namespace_uuid(&namespace.namespace_id)?;
        let binding = caller.memory_binding(
            namespace.source,
            namespace.memory_scope,
            namespace.toolkit_id,
            false,
        )?;
        let generation = layout::generation(&namespace.generation)?;
        let space = request
            .space
            .as_ref()
            .map(|space| Space::from_proto(Some(space)))
            .transpose()?;
        let scope = Scope::project(project_id)
            .source(source)
            .namespaces(vec![namespace.namespace_id.clone()])
            .generation(generation);
        let scope = match &binding {
            Some(binding) => scope.bound_to(binding),
            None => scope,
        };
        let filter = scope.filter(None);
        // Every point of the namespace is read (payload only, two keys) and
        // folded per (document_key, document_version) before the first page
        // goes out: a document's chunk count is not known until its last
        // point was seen, and Qdrant scrolls by point id, not by key (an
        // `order_by` scroll needs a numeric payload index, and the key is
        // text), so a page cannot be final before the fold is. What is
        // bounded instead: the points read, across every collection of this
        // one call (MAX_SCROLLED_POINTS), the distinct documents folded
        // (the caller's `max_documents`, summed over the collections, so an
        // index the caller cannot handle anyway stops costing the store at
        // that bound), the fold itself (one entry per document version, each
        // a few hundred bytes at most, far below the points), and the
        // answer, which is paged lazily out of the fold rather than copied
        // into every page first.
        let scrolled = AtomicU64::new(0);
        let folded = AtomicU64::new(0);
        let bounds = ScrollBounds {
            scrolled: &scrolled,
            folded: &folded,
            max_documents: (request.max_documents != 0).then_some(request.max_documents),
        };
        let counted = fan_out(self.store.targets(space.as_ref()).await?, |collection| {
            let filter = filter.clone();
            async move { self.scroll_documents(&collection, filter, bounds).await }
        })
        .await?;
        let mut documents: BTreeMap<(String, String), u64> = BTreeMap::new();
        for partial in counted {
            for (key, count) in partial {
                *documents.entry(key).or_default() += count;
            }
        }
        tracing::debug!(
            project_id,
            source,
            caller = caller_label(&caller),
            documents = documents.len(),
            points = scrolled.load(Ordering::Relaxed),
            pages = documents.len().div_ceil(MAX_DOCUMENTS_PAGE),
            "documents listed"
        );
        let mut documents =
            documents
                .into_iter()
                .map(
                    |((document_key, document_version), chunk_count)| pb::IndexedDocument {
                        document_key,
                        document_version,
                        chunk_count,
                    },
                );
        let pages = std::iter::from_fn(move || {
            let page: Vec<pb::IndexedDocument> =
                documents.by_ref().take(MAX_DOCUMENTS_PAGE).collect();
            (!page.is_empty()).then(|| Ok(pb::ListDocumentsResponse { documents: page }))
        });
        Ok(Response::new(Box::pin(tokio_stream::iter(pages))))
    }

    async fn read_memory(
        &self,
        request: Request<pb::ReadMemoryRequest>,
    ) -> Result<Response<pb::ReadMemoryResponse>, Status> {
        let caller = self.auth.authenticate(&request).await?;
        caller.require_token()?;
        let token = Authenticator::bearer_token(&request);
        let request = request.into_inner();
        let read = request
            .scope
            .as_ref()
            .ok_or_else(|| Status::invalid_argument("scope is required"))?;
        require_memory_scope(read)?;
        let project_id = caller.project(read.project_id)?;
        caller.require_source(read.source)?;
        let caller = self
            .memory_caller(
                token.as_deref(),
                caller,
                Some((read.source, read.memory_scope, read.toolkit_id)),
                false,
            )
            .await?;
        let scope = owned_read_scope(&caller, project_id, read, true)?;
        let narrowing = scope::translate(request.filter.as_ref())?;
        let limit = check_limit(request.limit)?;
        let space = request
            .space
            .as_ref()
            .map(|space| Space::from_proto(Some(space)))
            .transpose()?;
        let filter = scope.filter(narrowing);
        let matched = AtomicU64::new(0);
        let read_points = fan_out(self.store.targets(space.as_ref()).await?, |collection| {
            let filter = filter.clone();
            let matched = &matched;
            async move {
                let mut points = Vec::new();
                let mut offset: Option<PointId> = None;
                loop {
                    let mut scroll = ScrollPointsBuilder::new(&collection)
                        .filter(filter.clone())
                        .limit(MEMORY_SCROLL_PAGE)
                        .with_payload(true)
                        .with_vectors(false);
                    if let Some(offset) = offset.take() {
                        scroll = scroll.offset(offset);
                    }
                    let response = self
                        .store
                        .client()
                        .scroll(scroll)
                        .await
                        .map_err(|error| unavailable("scroll", &error))?;
                    let read = response.result.len() as u64;
                    if matched.fetch_add(read, Ordering::Relaxed) + read > MAX_MEMORY_MATCHED {
                        return Err(Status::resource_exhausted(
                            "the read matches too many memory versions; narrow it",
                        ));
                    }
                    points.extend(response.result);
                    match response.next_page_offset {
                        Some(next) => offset = Some(next),
                        None => break,
                    }
                }
                Ok(points)
            }
        })
        .await?;
        let mut points: Vec<pb::ScoredPoint> = read_points
            .into_iter()
            .flatten()
            .map(|point| scored_from_payload(point.id, 0.0, &point.payload))
            .collect();
        // Newest version first, whatever collection holds it: the version's
        // valid_from, then the larger version id (/t/id, the tie-break the
        // client uses to pick the current version), then the point id.
        points.sort_by_cached_key(|point| {
            let (valid_from, version_id) = version_order_of(&point.metadata_json);
            std::cmp::Reverse((valid_from, version_id, point.id.clone()))
        });
        points.truncate(limit as usize);
        Ok(Response::new(pb::ReadMemoryResponse { points }))
    }

    async fn set_memory_metadata(
        &self,
        request: Request<pb::SetMemoryMetadataRequest>,
    ) -> Result<Response<pb::SetMemoryMetadataResponse>, Status> {
        let caller = self.auth.authenticate(&request).await?;
        caller.require_token()?;
        let token = Authenticator::bearer_token(&request);
        let request = request.into_inner();
        let project_id = caller.project(0)?;
        let namespace = request
            .namespace
            .as_ref()
            .ok_or_else(|| Status::invalid_argument("namespace is required"))?;
        if namespace.source != pb::Source::Memory as i32 {
            return Err(Status::invalid_argument(
                "SetMemoryMetadata applies to the memory source only",
            ));
        }
        caller.require_source(namespace.source)?;
        layout::namespace_uuid(&namespace.namespace_id)?;
        let caller = self
            .memory_caller(
                token.as_deref(),
                caller,
                Some((
                    namespace.source,
                    namespace.memory_scope,
                    namespace.toolkit_id,
                )),
                true,
            )
            .await?;
        let binding = caller
            .memory_binding(
                namespace.source,
                namespace.memory_scope,
                namespace.toolkit_id,
                true,
            )?
            .ok_or_else(|| Status::invalid_argument("memory_scope is required"))?;
        let generation = layout::generation(&namespace.generation)?;
        for (name, value) in [
            ("document_key", &request.document_key),
            ("chunk_id", &request.chunk_id),
        ] {
            if value.is_empty() || value.len() > MAX_KEY_BYTES {
                return Err(Status::invalid_argument(format!(
                    "{name} is 1 to 1024 bytes"
                )));
            }
        }
        let metadata = metadata(&request.metadata_json)?;
        let space = request
            .space
            .as_ref()
            .map(|space| Space::from_proto(Some(space)))
            .transpose()?;
        let scope = Scope::project(project_id)
            .source("memory")
            .namespaces(vec![namespace.namespace_id.clone()])
            .generation(generation)
            .bound_to(&binding)
            .with(Condition::matches(key::DOCUMENT_KEY, request.document_key))
            .with(Condition::matches(key::CHUNK_ID, request.chunk_id));
        let filter = scope.filter(None);
        let mut payload = Payload::new();
        payload.insert(key::METADATA, Value::from(metadata));
        for collection in self.store.targets(space.as_ref()).await? {
            self.store
                .client()
                .set_payload(
                    SetPayloadPointsBuilder::new(&collection, payload.clone())
                        .points_selector(filter.clone())
                        .wait(true),
                )
                .await
                .map_err(|error| unavailable("set_payload", &error))?;
        }
        Ok(Response::new(pb::SetMemoryMetadataResponse {}))
    }

    async fn delete_memory_user(
        &self,
        request: Request<pb::DeleteMemoryUserRequest>,
    ) -> Result<Response<pb::DeleteMemoryUserResponse>, Status> {
        let caller = self.auth.authenticate(&request).await?;
        let identity = caller.require_admin()?.to_owned();
        let request = request.into_inner();
        if request.user_id <= 0 {
            return Err(Status::invalid_argument("user_id must be positive"));
        }
        if request.project_id < 0 {
            return Err(Status::invalid_argument("project_id must not be negative"));
        }
        let owner = crate::auth::user_owner(request.user_id);
        // Project 0 is every project: a deleted user's personal memories go
        // wherever they were written, with no membership lookup.
        let scope = if request.project_id == 0 {
            Scope::every_project()
        } else {
            Scope::project(caller.project(request.project_id)?)
        };
        let collections = self.store.targets(None).await?;
        self.delete_where(&collections, &scope.source("memory").owned_by(&owner))
            .await?;
        tracing::info!(
            project_id = request.project_id,
            user_id = request.user_id,
            identity,
            "a user's personal memories deleted"
        );
        Ok(Response::new(pb::DeleteMemoryUserResponse {
            collections: count_u32(collections.len()),
        }))
    }

    async fn delete_memory_toolkit(
        &self,
        request: Request<pb::DeleteMemoryToolkitRequest>,
    ) -> Result<Response<pb::DeleteMemoryToolkitResponse>, Status> {
        let caller = self.auth.authenticate(&request).await?;
        let identity = caller.require_admin()?.to_owned();
        let request = request.into_inner();
        let project_id = caller.project(request.project_id)?;
        if request.toolkit_id <= 0 {
            return Err(Status::invalid_argument("toolkit_id must be positive"));
        }
        let collections = self.store.targets(None).await?;
        self.delete_where(
            &collections,
            &Scope::project(project_id)
                .source("memory")
                .toolkit(request.toolkit_id),
        )
        .await?;
        tracing::info!(project_id, identity, "a memory toolkit's points deleted");
        Ok(Response::new(pb::DeleteMemoryToolkitResponse {
            collections: count_u32(collections.len()),
        }))
    }
}

/// Points read per Qdrant scroll call by `ReadMemory`.
const MEMORY_SCROLL_PAGE: u32 = 512;
/// The most points one `ReadMemory` reads in all: a read matching more is
/// refused rather than answered from a part of what it matched.
const MAX_MEMORY_MATCHED: u64 = 5000;

/// The ordering keys of a memory point's metadata: `t.valid_from` (0 when it
/// has none) and the version id `t.id` (empty when it has none; a number is
/// read as its decimal text).
fn version_order_of(metadata_json: &str) -> (i64, String) {
    let value = serde_json::from_str::<serde_json::Value>(metadata_json).ok();
    let valid_from = value
        .as_ref()
        .and_then(|value| value.pointer("/t/valid_from"))
        .and_then(serde_json::Value::as_i64)
        .unwrap_or(0);
    let version_id = match value.as_ref().and_then(|value| value.pointer("/t/id")) {
        Some(serde_json::Value::String(text)) => text.clone(),
        Some(serde_json::Value::Number(number)) => number.to_string(),
        _ => String::new(),
    };
    (valid_from, version_id)
}

/// A memory read names the memory source, one namespace and no generation.
fn require_memory_scope(read: &pb::Scope) -> Result<(), Status> {
    if read.source != pb::Source::Memory as i32 {
        return Err(Status::invalid_argument(
            "this operation applies to the memory source only",
        ));
    }
    if read.namespace_ids.len() != 1
        || !read.generation.is_empty()
        || !read.active_generations.is_empty()
    {
        return Err(Status::invalid_argument(
            "a memory read names exactly one namespace and no generation",
        ));
    }
    Ok(())
}

/// The pages of one `ListDocuments` answer.
pub type ListDocumentsStream = std::pin::Pin<
    Box<dyn tokio_stream::Stream<Item = Result<pb::ListDocumentsResponse, Status>> + Send>,
>;

/// The most documents in one `ListDocuments` page.
pub const MAX_DOCUMENTS_PAGE: usize = 1024;
/// Points read per Qdrant scroll call by `ListDocuments`.
const SCROLL_PAGE: u32 = 2048;
/// The most points one `ListDocuments` reads, summed over every collection
/// the call scrolls. A toolkit index beyond this is refused
/// (`RESOURCE_EXHAUSTED`), not listed in part: a partial listing would make
/// an incremental indexer delete documents it did not see.
const MAX_SCROLLED_POINTS: u64 = 4_000_000;

/// The running counts one `ListDocuments` call shares between the
/// collections it scrolls concurrently, and the caller's document bound.
#[derive(Clone, Copy)]
struct ScrollBounds<'a> {
    /// Points read so far, bounded by [`MAX_SCROLLED_POINTS`].
    scrolled: &'a AtomicU64,
    /// Distinct `(document_key, document_version)` pairs folded so far, over
    /// every collection (a pair in two collections counts twice: the bound
    /// is on the server's work, and it is never exceeded by that).
    folded: &'a AtomicU64,
    /// The request's `max_documents`, when set.
    max_documents: Option<u64>,
}

impl VectorService {
    /// Folds every point `filter` selects in `collection` into
    /// `(document_key, document_version) -> chunk count`, within `bounds`:
    /// the points read and the documents folded are summed over the
    /// collections the call scrolls concurrently, and each bound is on that
    /// sum. Crossing one refuses the call with `RESOURCE_EXHAUSTED` at once,
    /// so the store does no more work for a listing the caller cannot take.
    async fn scroll_documents(
        &self,
        collection: &str,
        filter: Filter,
        bounds: ScrollBounds<'_>,
    ) -> Result<HashMap<(String, String), u64>, Status> {
        let mut counts: HashMap<(String, String), u64> = HashMap::new();
        let mut offset: Option<PointId> = None;
        loop {
            let mut scroll = ScrollPointsBuilder::new(collection)
                .filter(filter.clone())
                .limit(SCROLL_PAGE)
                .with_payload(PayloadIncludeSelector {
                    fields: vec![
                        key::DOCUMENT_KEY.to_owned(),
                        key::DOCUMENT_VERSION.to_owned(),
                    ],
                })
                .with_vectors(false);
            if let Some(offset) = offset.take() {
                scroll = scroll.offset(offset);
            }
            let response = self
                .store
                .client()
                .scroll(scroll)
                .await
                .map_err(|error| unavailable("scroll", &error))?;
            for point in &response.result {
                let text = |name: &str| -> String {
                    match point
                        .payload
                        .get(name)
                        .and_then(|value| value.kind.as_ref())
                    {
                        Some(Kind::StringValue(text)) => text.clone(),
                        _ => String::new(),
                    }
                };
                let document_key = text(key::DOCUMENT_KEY);
                if document_key.is_empty() {
                    // Every point is written with a key; a damaged one is not
                    // a document the caller can act on.
                    continue;
                }
                match counts.entry((document_key, text(key::DOCUMENT_VERSION))) {
                    std::collections::hash_map::Entry::Occupied(mut entry) => {
                        *entry.get_mut() += 1;
                    }
                    std::collections::hash_map::Entry::Vacant(entry) => {
                        entry.insert(1);
                        if let Some(max_documents) = bounds.max_documents
                            && bounds.folded.fetch_add(1, Ordering::Relaxed) + 1 > max_documents
                        {
                            return Err(Status::resource_exhausted(format!(
                                "the namespace holds more than {max_documents} documents, the most this listing takes"
                            )));
                        }
                    }
                }
            }
            let read = response.result.len() as u64;
            if bounds.scrolled.fetch_add(read, Ordering::Relaxed) + read > MAX_SCROLLED_POINTS {
                return Err(Status::resource_exhausted(
                    "the namespace holds more points than one listing reads",
                ));
            }
            match response.next_page_offset {
                Some(next) => offset = Some(next),
                None => break,
            }
        }
        Ok(counts)
    }
}

/// The point's stored dense vector, when the query asked for it.
fn dense_vector(point: &qdrant_client::qdrant::ScoredPoint) -> Option<Vec<f32>> {
    match point.vectors.as_ref()?.get_vector_by_name(DENSE_VECTOR)? {
        vector_output::Vector::Dense(dense) => Some(dense.data),
        vector_output::Vector::Sparse(_) | vector_output::Vector::MultiDense(_) => None,
    }
}

/// The cosine similarity of two vectors, each normalised here. Collections
/// use cosine distance, so Qdrant normalises what it stores (and a float16
/// round trip moves the norm a little); the query vector arrives as the
/// caller made it. Dividing by both norms is right in every case. Zero
/// vectors (refused on write and on query, so only a damaged point) score 0.
fn cosine(query: &[f32], stored: &[f32]) -> f32 {
    let dot: f32 = query.iter().zip(stored).map(|(q, s)| q * s).sum();
    let norm = |vector: &[f32]| vector.iter().map(|x| x * x).sum::<f32>().sqrt();
    let norms = norm(query) * norm(stored);
    if norms == 0.0 || !norms.is_finite() {
        0.0
    } else {
        (dot / norms).clamp(-1.0, 1.0)
    }
}

impl VectorService {
    async fn delete_where(&self, collections: &[String], scope: &Scope) -> Result<(), Status> {
        let filter = scope.filter(None);
        for collection in collections {
            self.store
                .client()
                .delete_points(
                    DeletePointsBuilder::new(collection)
                        .points(filter.clone())
                        .wait(true),
                )
                .await
                .map_err(|error| unavailable("delete_points", &error))?;
        }
        Ok(())
    }
}

/// Where one Upsert stream writes.
struct UpsertTarget {
    project_id: i64,
    /// The memory owner and toolkit stamped on every point (`None` for other
    /// sources).
    binding: Option<crate::auth::MemoryBinding>,
    source: &'static str,
    namespace_id: String,
    namespace: uuid::Uuid,
    generation: Option<String>,
    dimension: u32,
}

impl UpsertTarget {
    fn point(&self, point: &pb::Point) -> Result<PointStruct, Status> {
        check_vector(&point.vector, self.dimension)?;
        for (name, value) in [
            ("document_key", &point.document_key),
            ("chunk_id", &point.chunk_id),
        ] {
            if value.is_empty() {
                return Err(Status::invalid_argument(format!("{name} is required")));
            }
        }
        for (name, value) in [
            ("chunk_id", &point.chunk_id),
            ("chunk_type", &point.chunk_type),
        ] {
            if layout::has_control_character(value) {
                return Err(Status::invalid_argument(format!(
                    "{name} must not contain control characters"
                )));
            }
        }
        for (name, value) in [
            ("document_key", &point.document_key),
            ("document_version", &point.document_version),
            ("parent_id", &point.parent_id),
            ("chunk_id", &point.chunk_id),
            ("chunk_type", &point.chunk_type),
        ] {
            if value.len() > MAX_KEY_BYTES {
                return Err(Status::invalid_argument(format!(
                    "{name} is at most 1024 bytes"
                )));
            }
        }
        if point.acl.len() > MAX_ACL_ENTRIES
            || point.acl.iter().any(|entry| entry.len() > MAX_KEY_BYTES)
        {
            return Err(Status::invalid_argument(
                "acl holds at most 256 entries of at most 1024 bytes",
            ));
        }
        if point.text.len() > MAX_TEXT_BYTES {
            return Err(Status::invalid_argument("text is at most 1 MiB"));
        }
        let metadata = metadata(&point.metadata_json)?;

        let mut payload = Payload::new();
        // The scope keys come from the verified header, never the point.
        payload.insert(key::PROJECT_ID, self.project_id.to_string());
        payload.insert(key::SOURCE, self.source.to_owned());
        payload.insert(key::NAMESPACE_ID, self.namespace_id.clone());
        if let Some(binding) = &self.binding {
            payload.insert(key::OWNER, binding.owner.clone());
            payload.insert(key::TOOLKIT_ID, binding.toolkit_id.to_string());
        }
        if let Some(generation) = &self.generation {
            payload.insert(key::GENERATION, generation.clone());
        }
        payload.insert(key::DOCUMENT_KEY, point.document_key.clone());
        payload.insert(key::DOCUMENT_VERSION, point.document_version.clone());
        payload.insert(key::PARENT_ID, point.parent_id.clone());
        payload.insert(key::CHUNK_ID, point.chunk_id.clone());
        payload.insert(key::CHUNK_TYPE, point.chunk_type.clone());
        payload.insert(
            key::ACL,
            Value::from(serde_json::Value::from(point.acl.clone())),
        );
        payload.insert(key::TEXT, point.text.clone());
        payload.insert(key::METADATA, Value::from(metadata));

        let mut vectors = NamedVectors::default()
            .add_vector(DENSE_VECTOR, Vector::new_dense(point.vector.clone()));
        if !point.text.trim().is_empty() {
            vectors =
                vectors.add_vector(BM25_VECTOR, Document::new(point.text.clone(), BM25_MODEL));
        }
        // A memory point's identity includes its owner and its toolkit, so
        // two toolkits (or owners) writing one key never share a point.
        let identity = self
            .binding
            .as_ref()
            .map(|binding| format!("{}@{}", binding.owner, binding.toolkit_id))
            .unwrap_or_default();
        let id = layout::point_id(
            self.project_id,
            self.source,
            self.namespace,
            self.generation.as_deref(),
            &point.document_key,
            &point.chunk_id,
            &point.chunk_type,
            &identity,
        );
        Ok(PointStruct::new(id.to_string(), vectors, payload))
    }
}

fn metadata(raw: &str) -> Result<serde_json::Value, Status> {
    if raw.is_empty() {
        return Ok(serde_json::Value::Object(serde_json::Map::new()));
    }
    if raw.len() > MAX_METADATA_BYTES {
        return Err(Status::invalid_argument("metadata_json is at most 64 KiB"));
    }
    match serde_json::from_str::<serde_json::Value>(raw) {
        Ok(value @ serde_json::Value::Object(_)) => Ok(value),
        _ => Err(Status::invalid_argument(
            "metadata_json must be a JSON object",
        )),
    }
}

/// Refuses a `score_threshold` that is NaN or infinite: Qdrant would compare
/// every score against it, or reject the request in a way the caller cannot
/// tell from an outage.
fn check_score_threshold(threshold: Option<f32>) -> Result<(), Status> {
    match threshold {
        Some(value) if !value.is_finite() => Err(Status::invalid_argument(
            "score_threshold must be a finite number",
        )),
        _ => Ok(()),
    }
}

fn read_parts(
    caller: &Caller,
    read: Option<&pb::Scope>,
    space: Option<&pb::EmbeddingSpace>,
    filter: Option<&pb::Filter>,
) -> Result<(qdrant_client::qdrant::Filter, Space), Status> {
    let read = read.ok_or_else(|| Status::invalid_argument("scope is required"))?;
    let project_id = caller.project(read.project_id)?;
    caller.require_source(read.source)?;
    let scope = owned_read_scope(caller, project_id, read, true)?;
    let narrowing = scope::translate(filter)?;
    let space = Space::from_proto(space)?;
    Ok((scope.filter(narrowing), space))
}

/// [`scope::read_scope`], narrowed to the memory owner the token decides when
/// the source is memory.
fn owned_read_scope(
    caller: &Caller,
    project_id: i64,
    read: &pb::Scope,
    require_namespace: bool,
) -> Result<Scope, Status> {
    let binding = caller.memory_binding(read.source, read.memory_scope, read.toolkit_id, false)?;
    let scope = scope::read_scope(project_id, read, require_namespace)?;
    Ok(match &binding {
        Some(binding) => scope.bound_to(binding),
        None => scope,
    })
}

fn check_vector(vector: &[f32], dimension: u32) -> Result<(), Status> {
    if vector.len() != dimension as usize {
        return Err(Status::invalid_argument(
            "the vector's dimension differs from the collection's",
        ));
    }
    if vector.iter().any(|value| !value.is_finite()) || vector.iter().all(|value| *value == 0.0) {
        return Err(Status::invalid_argument(
            "the vector must be finite and not all zero",
        ));
    }
    Ok(())
}

fn check_query_vector(vector: &[f32], space: &Space) -> Result<(), Status> {
    check_vector(vector, space.dimension())
}

fn check_limit(limit: u32) -> Result<u32, Status> {
    if limit == 0 || limit > MAX_LIMIT {
        return Err(Status::invalid_argument("limit is 1 to 1000"));
    }
    Ok(limit)
}

fn weights(dense: f32, text: f32) -> Result<(f32, f32), Status> {
    if dense == 0.0 && text == 0.0 {
        return Ok((DEFAULT_DENSE_WEIGHT, DEFAULT_TEXT_WEIGHT));
    }
    if !dense.is_finite() || !text.is_finite() || dense < 0.0 || text < 0.0 {
        return Err(Status::invalid_argument(
            "weights must be finite and not negative",
        ));
    }
    Ok((dense, text))
}

fn bounded_keys(keys: &[String], name: &str) -> Result<(), Status> {
    if keys.is_empty()
        || keys.len() > 1024
        || keys
            .iter()
            .any(|value| value.is_empty() || value.len() > MAX_KEY_BYTES)
    {
        return Err(Status::invalid_argument(format!(
            "{name} holds 1 to 1024 non-empty values"
        )));
    }
    Ok(())
}

fn count_u32(value: usize) -> u32 {
    u32::try_from(value).unwrap_or(u32::MAX)
}

fn caller_label(caller: &Caller) -> String {
    match caller {
        Caller::Token(grant) => grant.principal.clone(),
        Caller::Admin(identity) => identity.clone(),
    }
}

fn string(payload: &HashMap<String, Value>, name: &str) -> String {
    match payload.get(name).and_then(|value| value.kind.as_ref()) {
        Some(Kind::StringValue(value)) => value.clone(),
        _ => String::new(),
    }
}

fn point_id_text(id: Option<PointId>) -> String {
    match id.and_then(|id| id.point_id_options) {
        Some(PointIdOptions::Uuid(uuid)) => uuid,
        Some(PointIdOptions::Num(number)) => number.to_string(),
        None => String::new(),
    }
}

fn scored_point(point: qdrant_client::qdrant::ScoredPoint) -> pb::ScoredPoint {
    scored_from_payload(point.id, point.score, &point.payload)
}

fn scored_from_payload(
    id: Option<PointId>,
    score: f32,
    payload: &HashMap<String, Value>,
) -> pb::ScoredPoint {
    let id = point_id_text(id);
    let acl = match payload.get(key::ACL).and_then(|value| value.kind.as_ref()) {
        Some(Kind::ListValue(list)) => list
            .values
            .iter()
            .filter_map(|value| match &value.kind {
                Some(Kind::StringValue(entry)) => Some(entry.clone()),
                _ => None,
            })
            .collect(),
        _ => Vec::new(),
    };
    let metadata_json = payload
        .get(key::METADATA)
        .and_then(|value| serde_json::to_string(value).ok())
        .unwrap_or_else(|| "{}".to_owned());
    pb::ScoredPoint {
        id,
        score,
        project_id: string(payload, key::PROJECT_ID).parse().unwrap_or_default(),
        source: layout::source_from_keyword(&string(payload, key::SOURCE)) as i32,
        namespace_id: string(payload, key::NAMESPACE_ID),
        generation: string(payload, key::GENERATION),
        document_key: string(payload, key::DOCUMENT_KEY),
        document_version: string(payload, key::DOCUMENT_VERSION),
        parent_id: string(payload, key::PARENT_ID),
        chunk_id: string(payload, key::CHUNK_ID),
        chunk_type: string(payload, key::CHUNK_TYPE),
        acl,
        text: string(payload, key::TEXT),
        metadata_json,
        dense_score: None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn vectors_are_checked_against_the_dimension() {
        assert!(check_vector(&[1.0, 0.0, 0.0], 3).is_ok());
        assert!(check_vector(&[1.0, 0.0], 3).is_err());
        assert!(check_vector(&[1.0, f32::NAN, 0.0], 3).is_err());
        assert!(check_vector(&[0.0, 0.0, 0.0], 3).is_err());
    }

    #[test]
    fn cosine_normalises_both_sides() {
        // Stored normalised or not, query normalised or not: the same answer.
        let query = [3.0, 0.0, 0.0];
        assert!((cosine(&query, &[1.0, 0.0, 0.0]) - 1.0).abs() < 1e-6);
        assert!((cosine(&query, &[5.0, 0.0, 0.0]) - 1.0).abs() < 1e-6);
        let half = cosine(&[2.0, 2.0, 0.0], &[0.5, 0.0, 0.0]);
        assert!((half - std::f32::consts::FRAC_1_SQRT_2).abs() < 1e-6);
        assert!((cosine(&query, &[0.0, 4.0, 0.0])).abs() < 1e-6);
        assert!((cosine(&query, &[-1.0, 0.0, 0.0]) + 1.0).abs() < 1e-6);
        assert!(cosine(&query, &[0.0, 0.0, 0.0]).abs() < f32::EPSILON);
        assert!(cosine(&query, &[f32::NAN, 0.0, 0.0]).abs() < f32::EPSILON);
    }

    #[test]
    fn weights_default_and_validate() {
        assert_eq!(weights(0.0, 0.0).expect("default"), (0.7, 0.3));
        assert_eq!(weights(1.0, 0.0).expect("dense only"), (1.0, 0.0));
        assert!(weights(-1.0, 1.0).is_err());
        assert!(weights(f32::INFINITY, 1.0).is_err());
    }

    #[test]
    fn metadata_must_be_an_object() {
        assert!(metadata("").expect("empty").is_object());
        assert!(metadata("{\"a\":1}").is_ok());
        assert!(metadata("[1]").is_err());
        assert!(metadata("nope").is_err());
    }

    #[test]
    fn upsert_payload_takes_scope_from_the_header() {
        let target = UpsertTarget {
            project_id: 7,
            binding: None,
            source: "toolkit_index",
            namespace_id: "0b0f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5b".to_owned(),
            namespace: uuid::Uuid::from_u128(1),
            generation: None,
            dimension: 2,
        };
        let point = target
            .point(&pb::Point {
                document_key: "d".to_owned(),
                chunk_id: "0".to_owned(),
                vector: vec![1.0, 0.0],
                metadata_json: "{\"project_id\":\"8\"}".to_owned(),
                ..pb::Point::default()
            })
            .expect("valid");
        let project = point.payload.get(key::PROJECT_ID).expect("project");
        assert_eq!(project.kind, Some(Kind::StringValue("7".to_owned())));
        assert!(
            target
                .point(&pb::Point {
                    document_key: "d".to_owned(),
                    chunk_id: "0".to_owned(),
                    vector: vec![1.0, 0.0, 0.0],
                    ..pb::Point::default()
                })
                .is_err()
        );
    }

    #[test]
    fn a_chunk_id_or_type_with_a_control_character_is_refused() {
        let target = UpsertTarget {
            project_id: 7,
            binding: None,
            source: "toolkit_index",
            namespace_id: "0b0f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5b".to_owned(),
            namespace: uuid::Uuid::from_u128(1),
            generation: None,
            dimension: 2,
        };
        let point = |chunk_id: &str, chunk_type: &str| {
            target.point(&pb::Point {
                document_key: "d".to_owned(),
                chunk_id: chunk_id.to_owned(),
                chunk_type: chunk_type.to_owned(),
                vector: vec![1.0, 0.0],
                ..pb::Point::default()
            })
        };
        assert!(point("3", "title").is_ok());
        for bad in [point("3\u{1f}title", "document"), point("3", "ti\ntle")] {
            assert_eq!(
                bad.expect_err("refused").code(),
                tonic::Code::InvalidArgument
            );
        }
    }

    #[test]
    fn a_non_finite_score_threshold_is_refused() {
        for bad in [f32::NAN, f32::INFINITY, f32::NEG_INFINITY] {
            assert_eq!(
                check_score_threshold(Some(bad))
                    .expect_err("refused")
                    .code(),
                tonic::Code::InvalidArgument
            );
        }
        assert!(check_score_threshold(None).is_ok());
        assert!(check_score_threshold(Some(0.5)).is_ok());
        assert!(check_score_threshold(Some(-3.0)).is_ok());
    }

    #[test]
    fn space_limits_bound_the_dimension_and_the_creatable_spaces() {
        let limits = SpaceLimits::default();
        assert!(
            limits
                .check_dimension(&Space::new("m", 1).expect("space"))
                .is_ok()
        );
        assert!(
            limits
                .check_dimension(&Space::new("m", 3072).expect("space"))
                .is_ok()
        );
        assert!(
            limits
                .check_dimension(&Space::new("m", 4096).expect("space"))
                .is_ok()
        );
        // Valid for the layout, refused for creation.
        assert_eq!(
            limits
                .check_dimension(&Space::new("m", 4097).expect("space"))
                .expect_err("too large")
                .code(),
            tonic::Code::InvalidArgument
        );
        let tight = SpaceLimits {
            max_dimension: 8,
            ..SpaceLimits::default()
        };
        assert!(
            tight
                .check_dimension(&Space::new("m", 9).expect("space"))
                .is_err()
        );
        assert!(
            limits
                .check_creatable(&Space::new("any", 7).expect("space"))
                .is_ok()
        );
        let listed = SpaceLimits {
            allowed: Some(HashSet::from([Space::new("bge-m3", 1024).expect("space")])),
            ..SpaceLimits::default()
        };
        assert!(
            listed
                .check_creatable(&Space::new("bge-m3", 1024).expect("space"))
                .is_ok()
        );
        for other in [("bge-m3", 768), ("other", 1024)] {
            assert_eq!(
                listed
                    .check_creatable(&Space::new(other.0, other.1).expect("space"))
                    .expect_err("not listed")
                    .code(),
                tonic::Code::PermissionDenied
            );
        }
    }
}
