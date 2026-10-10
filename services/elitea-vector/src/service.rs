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

use std::collections::{HashMap, HashSet};
use std::sync::Arc;

use qdrant_client::Payload;
use qdrant_client::qdrant::point_id::PointIdOptions;
use qdrant_client::qdrant::value::Kind;
use qdrant_client::qdrant::{
    Condition, CountPointsBuilder, DeletePointsBuilder, Document, FacetCountsBuilder, NamedVectors,
    PointStruct, PrefetchQueryBuilder, Query, QueryPointsBuilder, Rrf, UpsertPointsBuilder, Value,
    Vector,
};
use tokio_stream::StreamExt as _;
use tonic::{Request, Response, Status, Streaming};

use crate::auth::{Authenticator, Caller};
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
        let target = UpsertTarget {
            project_id,
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
            // The introspection cache keeps this cheap; a token that went
            // inactive, moved project or lost the source ends the stream.
            let current = self.auth.verify_token(&token).await?;
            if current.project(0)? != project_id {
                return Err(Status::permission_denied(
                    "the token's project changed during the stream",
                ));
            }
            current.require_source(namespace.source)?;
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
        let source = layout::source_keyword(namespace.source)?;
        layout::namespace_uuid(&namespace.namespace_id)?;
        let scope = Scope::project(project_id)
            .source(source)
            .namespaces(vec![namespace.namespace_id.clone()]);
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
                scope.without(Condition::matches(key::GENERATION, keep.to_owned()))
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
        let request = request.into_inner();
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
            points: response.result.into_iter().map(scored_point).collect(),
        }))
    }

    async fn hybrid_search(
        &self,
        request: Request<pb::HybridSearchRequest>,
    ) -> Result<Response<pb::HybridSearchResponse>, Status> {
        let caller = self.auth.authenticate(&request).await?;
        caller.require_token()?;
        let request = request.into_inner();
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
            .query(Query::new_nearest(request.vector))
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
        let query = QueryPointsBuilder::new(collection)
            .add_prefetch(dense)
            .add_prefetch(text)
            .query(Query::new_rrf(Rrf {
                k: Some(RRF_K),
                weights: vec![dense_weight, text_weight],
            }))
            // The scope again on the fused stage: defence in depth.
            .filter(filter)
            .limit(u64::from(limit))
            .with_payload(true);
        let response = self
            .store
            .client()
            .query(query)
            .await
            .map_err(|error| unavailable("query", &error))?;
        Ok(Response::new(pb::HybridSearchResponse {
            points: response.result.into_iter().map(scored_point).collect(),
        }))
    }

    async fn count(
        &self,
        request: Request<pb::CountRequest>,
    ) -> Result<Response<pb::CountResponse>, Status> {
        let caller = self.auth.authenticate(&request).await?;
        let request = request.into_inner();
        let read = request
            .scope
            .as_ref()
            .ok_or_else(|| Status::invalid_argument("scope is required"))?;
        let project_id = caller.project(read.project_id)?;
        caller.require_source(read.source)?;
        let scope = scope::read_scope(project_id, read, false)?;
        let narrowing = scope::translate(request.filter.as_ref())?;
        let space = request
            .space
            .as_ref()
            .map(|space| Space::from_proto(Some(space)))
            .transpose()?;
        let filter = scope.filter(narrowing);
        let mut total: u64 = 0;
        for collection in self.store.targets(space.as_ref()).await? {
            let counted = self
                .store
                .client()
                .count(
                    CountPointsBuilder::new(collection)
                        .filter(filter.clone())
                        .exact(true),
                )
                .await
                .map_err(|error| unavailable("count", &error))?;
            total += counted.result.map_or(0, |result| result.count);
        }
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
            caller.require_source(request.source)?;
            vec![layout::source_keyword(request.source)?]
        };
        let mut indexes = Vec::new();
        for (collection, space) in self.store.collections().await? {
            for source in &sources {
                let facet = FacetCountsBuilder::new(&collection, key::NAMESPACE_ID)
                    .filter(Scope::project(project_id).source(source).filter(None))
                    .limit(MAX_LISTED_NAMESPACES)
                    .exact(true);
                let response = self
                    .store
                    .client()
                    .facet(facet)
                    .await
                    .map_err(|error| unavailable("facet", &error))?;
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
                        source: layout::source_from_keyword(source) as i32,
                        namespace_id,
                        point_count: hit.count,
                    });
                }
            }
        }
        Ok(Response::new(pb::ListIndexesResponse { indexes }))
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
        let id = layout::point_id(
            self.project_id,
            self.source,
            self.namespace,
            self.generation.as_deref(),
            &point.document_key,
            &point.chunk_id,
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
    let scope = scope::read_scope(project_id, read, true)?;
    let narrowing = scope::translate(filter)?;
    let space = Space::from_proto(space)?;
    Ok((scope.filter(narrowing), space))
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

fn scored_point(point: qdrant_client::qdrant::ScoredPoint) -> pb::ScoredPoint {
    let payload = &point.payload;
    let id = match point.id.and_then(|id| id.point_id_options) {
        Some(PointIdOptions::Uuid(uuid)) => uuid,
        Some(PointIdOptions::Num(number)) => number.to_string(),
        None => String::new(),
    };
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
        score: point.score,
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
