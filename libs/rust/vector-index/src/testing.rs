//! An in-process fake of `elitea-vector`, for tests (feature `test-server`).
//!
//! It is **not** the service: it stores points in memory, scores them with
//! exact cosine similarity, and understands the same filter shape (`must`,
//! `should`, `must_not` of keyword, any, integer and boolean matches on the
//! point fields and on `metadata.<path>`). It checks the bearer token and
//! the project scope. It does not run Qdrant, BM25 statistics or quantization.
//! The crate's integration tests drive the real client against it, over a
//! real gRPC connection.

use std::collections::HashMap;
use std::net::SocketAddr;
use std::sync::{Arc, Mutex};

use serde_json::Value;
use tokio::net::TcpListener;
use tokio::task::JoinHandle;
use tonic::{Code, Request, Response, Status, Streaming};

use crate::pb;
use crate::pb::vector_service_server::{VectorService, VectorServiceServer};

/// `k` of the reciprocal rank fusion, like the service.
const RRF_K: f64 = 60.0;

#[derive(Clone)]
struct Stored {
    project_id: i64,
    source: i32,
    namespace_id: String,
    generation: String,
    space: (String, u32),
    point: pb::Point,
}

#[derive(Default)]
struct State {
    points: Vec<Stored>,
    /// Every bearer the fake saw, in order.
    bearers: Vec<String>,
    /// Errors to return for the next calls, oldest first.
    failures: Vec<Status>,
}

/// The fake service. Clone it to keep a handle on its state.
#[derive(Clone, Default)]
pub struct FakeVector {
    tokens: Arc<HashMap<String, i64>>,
    state: Arc<Mutex<State>>,
}

impl FakeVector {
    /// A fake that accepts `tokens` (bearer to project).
    #[must_use]
    pub fn new(tokens: &[(&str, i64)]) -> Self {
        Self {
            tokens: Arc::new(
                tokens
                    .iter()
                    .map(|(token, project)| ((*token).to_owned(), *project))
                    .collect(),
            ),
            state: Arc::default(),
        }
    }

    /// The next call fails with this status (before anything else).
    ///
    /// # Panics
    ///
    /// If the state lock is poisoned.
    pub fn fail_next(&self, status: Status) {
        self.state.lock().expect("lock").failures.push(status);
    }

    /// The bearers of all calls so far.
    ///
    /// # Panics
    ///
    /// If the state lock is poisoned.
    #[must_use]
    pub fn bearers(&self) -> Vec<String> {
        self.state.lock().expect("lock").bearers.clone()
    }

    /// How many points are stored.
    ///
    /// # Panics
    ///
    /// If the state lock is poisoned.
    #[must_use]
    pub fn point_count(&self) -> usize {
        self.state.lock().expect("lock").points.len()
    }

    /// Serves the fake on an ephemeral local port.
    ///
    /// # Errors
    ///
    /// If the listener cannot bind.
    pub async fn serve(self) -> std::io::Result<(SocketAddr, JoinHandle<()>)> {
        let listener = TcpListener::bind("127.0.0.1:0").await?;
        let address = listener.local_addr()?;
        let incoming = tokio_stream::wrappers::TcpListenerStream::new(listener);
        let handle = tokio::spawn(async move {
            let _ = tonic::transport::Server::builder()
                .add_service(VectorServiceServer::new(self))
                .serve_with_incoming(incoming)
                .await;
        });
        Ok((address, handle))
    }

    fn authenticate<T>(&self, request: &Request<T>) -> Result<i64, Status> {
        let header = request
            .metadata()
            .get("authorization")
            .and_then(|value| value.to_str().ok())
            .unwrap_or("");
        let mut state = self.state.lock().expect("lock");
        state.bearers.push(header.to_owned());
        if !state.failures.is_empty() {
            return Err(state.failures.remove(0));
        }
        drop(state);
        let token = header
            .strip_prefix("Bearer ")
            .ok_or_else(|| Status::unauthenticated("a project token is required"))?;
        self.tokens
            .get(token)
            .copied()
            .ok_or_else(|| Status::unauthenticated("unknown token"))
    }
}

fn space_key(space: Option<&pb::EmbeddingSpace>) -> Result<(String, u32), Status> {
    let space = space.ok_or_else(|| Status::invalid_argument("space is required"))?;
    Ok((space.model_slug.clone(), space.dimension))
}

fn field<'a>(point: &'a pb::Point, key: &str) -> Option<&'a str> {
    match key {
        "document_key" => Some(&point.document_key),
        "document_version" => Some(&point.document_version),
        "parent_id" => Some(&point.parent_id),
        "chunk_id" => Some(&point.chunk_id),
        "chunk_type" => Some(&point.chunk_type),
        _ => None,
    }
}

fn matches_condition(point: &pb::Point, condition: &pb::Condition) -> bool {
    use pb::condition::Match;
    let key = condition.key.as_str();
    if key == "acl" {
        return match &condition.r#match {
            Some(Match::Keyword(value)) => point.acl.contains(value),
            Some(Match::Any(list)) => point.acl.iter().any(|entry| list.values.contains(entry)),
            _ => false,
        };
    }
    if let Some(text) = field(point, key) {
        return match &condition.r#match {
            Some(Match::Keyword(value)) => text == value,
            Some(Match::Any(list)) => list.values.iter().any(|value| value == text),
            _ => false,
        };
    }
    let Some(path) = key.strip_prefix("metadata.") else {
        return false;
    };
    let mut current: Value = serde_json::from_str(&point.metadata_json).unwrap_or(Value::Null);
    for part in path.split('.') {
        current = match current {
            Value::Object(mut map) => map.remove(part).unwrap_or(Value::Null),
            _ => Value::Null,
        };
    }
    match (&condition.r#match, &current) {
        (Some(Match::Keyword(value)), Value::String(text)) => text == value,
        (Some(Match::Any(list)), Value::String(text)) => list.values.contains(text),
        (Some(Match::Integer(value)), Value::Number(number)) => number.as_i64() == Some(*value),
        (Some(Match::Boolean(value)), Value::Bool(flag)) => flag == value,
        _ => false,
    }
}

/// Qdrant's semantics: every `must` holds, at least one `should` holds
/// (when there are any), no `must_not` holds.
fn matches_filter(point: &pb::Point, filter: Option<&pb::Filter>) -> bool {
    let Some(filter) = filter else {
        return true;
    };
    filter.must.iter().all(|c| matches_condition(point, c))
        && (filter.should.is_empty() || filter.should.iter().any(|c| matches_condition(point, c)))
        && !filter.must_not.iter().any(|c| matches_condition(point, c))
}

fn cosine(left: &[f32], right: &[f32]) -> f32 {
    let dot: f32 = left.iter().zip(right).map(|(a, b)| a * b).sum();
    let norm = |v: &[f32]| v.iter().map(|x| x * x).sum::<f32>().sqrt();
    let denominator = norm(left) * norm(right);
    if denominator == 0.0 {
        0.0
    } else {
        dot / denominator
    }
}

fn scored(stored: &Stored, score: f32) -> pb::ScoredPoint {
    pb::ScoredPoint {
        id: String::new(),
        score,
        project_id: stored.project_id,
        source: stored.source,
        namespace_id: stored.namespace_id.clone(),
        generation: stored.generation.clone(),
        document_key: stored.point.document_key.clone(),
        document_version: stored.point.document_version.clone(),
        parent_id: stored.point.parent_id.clone(),
        chunk_id: stored.point.chunk_id.clone(),
        chunk_type: stored.point.chunk_type.clone(),
        acl: stored.point.acl.clone(),
        text: stored.point.text.clone(),
        metadata_json: stored.point.metadata_json.clone(),
    }
}

impl State {
    /// The stored points a read scope, space and filter select.
    fn select(
        &self,
        project: i64,
        scope: Option<&pb::Scope>,
        space: Option<&pb::EmbeddingSpace>,
        filter: Option<&pb::Filter>,
    ) -> Result<Vec<&Stored>, Status> {
        let scope = scope.ok_or_else(|| Status::invalid_argument("scope is required"))?;
        if scope.project_id != 0 && scope.project_id != project {
            return Err(Status::permission_denied("the scope names another project"));
        }
        let space = space.map(|s| (s.model_slug.clone(), s.dimension));
        Ok(self
            .points
            .iter()
            .filter(|stored| {
                stored.project_id == project
                    && stored.source == scope.source
                    && scope.namespace_ids.contains(&stored.namespace_id)
                    && (scope.generation.is_empty() || stored.generation == scope.generation)
                    && space.as_ref().is_none_or(|space| *space == stored.space)
                    && matches_filter(&stored.point, filter)
            })
            .collect())
    }
}

#[tonic::async_trait]
impl VectorService for FakeVector {
    async fn upsert(
        &self,
        request: Request<Streaming<pb::UpsertRequest>>,
    ) -> Result<Response<pb::UpsertResponse>, Status> {
        let project = self.authenticate(&request)?;
        let mut stream = request.into_inner();
        let Some(pb::UpsertRequest {
            message: Some(pb::upsert_request::Message::Header(header)),
        }) = stream.message().await?
        else {
            return Err(Status::invalid_argument(
                "the first Upsert message must be a header",
            ));
        };
        let space = space_key(header.space.as_ref())?;
        let namespace = header
            .namespace
            .ok_or_else(|| Status::invalid_argument("namespace is required"))?;
        let mut upserted = 0_u64;
        while let Some(message) = stream.message().await? {
            let Some(pb::upsert_request::Message::Batch(batch)) = message.message else {
                return Err(Status::invalid_argument(
                    "only the first message is a header",
                ));
            };
            if batch.points.len() > 512 {
                return Err(Status::invalid_argument("at most 512 points per batch"));
            }
            let mut state = self.state.lock().expect("lock");
            for point in batch.points {
                if point.vector.len() != space.1 as usize {
                    return Err(Status::invalid_argument(
                        "the vector has the wrong dimension",
                    ));
                }
                state.points.retain(|stored| {
                    !(stored.project_id == project
                        && stored.namespace_id == namespace.namespace_id
                        && stored.generation == namespace.generation
                        && stored.point.document_key == point.document_key
                        && stored.point.chunk_id == point.chunk_id
                        && stored.point.chunk_type == point.chunk_type)
                });
                state.points.push(Stored {
                    project_id: project,
                    source: namespace.source,
                    namespace_id: namespace.namespace_id.clone(),
                    generation: namespace.generation.clone(),
                    space: space.clone(),
                    point,
                });
                upserted += 1;
            }
        }
        Ok(Response::new(pb::UpsertResponse {
            upserted,
            collection: format!("emb_{}_{}", space.0, space.1),
        }))
    }

    async fn delete(
        &self,
        request: Request<pb::DeleteRequest>,
    ) -> Result<Response<pb::DeleteResponse>, Status> {
        let project = self.authenticate(&request)?;
        let request = request.into_inner();
        let namespace = request
            .namespace
            .ok_or_else(|| Status::invalid_argument("namespace is required"))?;
        let space = request.space.map(|s| (s.model_slug, s.dimension));
        let selector = request
            .selector
            .ok_or_else(|| Status::invalid_argument("a selector is required"))?;
        let mut state = self.state.lock().expect("lock");
        let before = state.points.len();
        state.points.retain(|stored| {
            let in_namespace = stored.project_id == project
                && stored.source == namespace.source
                && stored.namespace_id == namespace.namespace_id
                && space.as_ref().is_none_or(|space| *space == stored.space);
            let selected = match &selector {
                pb::delete_request::Selector::WholeNamespace(_) => true,
                pb::delete_request::Selector::Documents(keys) => {
                    keys.document_keys.contains(&stored.point.document_key)
                }
                pb::delete_request::Selector::Generations(list) => {
                    list.generations.contains(&stored.generation)
                }
                pb::delete_request::Selector::GenerationsExcept(keep) => {
                    stored.generation != keep.keep
                }
            };
            !(in_namespace && selected)
        });
        Ok(Response::new(pb::DeleteResponse {
            collections: u32::from(state.points.len() != before),
        }))
    }

    async fn drop_project(
        &self,
        request: Request<pb::DropProjectRequest>,
    ) -> Result<Response<pb::DropProjectResponse>, Status> {
        self.authenticate(&request)?;
        Err(Status::permission_denied(
            "only an administrator can drop a project",
        ))
    }

    async fn search(
        &self,
        request: Request<pb::SearchRequest>,
    ) -> Result<Response<pb::SearchResponse>, Status> {
        let project = self.authenticate(&request)?;
        let request = request.into_inner();
        let state = self.state.lock().expect("lock");
        let mut found: Vec<(&Stored, f32)> = state
            .select(
                project,
                request.scope.as_ref(),
                request.space.as_ref(),
                request.filter.as_ref(),
            )?
            .into_iter()
            .map(|stored| (stored, cosine(&request.vector, &stored.point.vector)))
            .filter(|(_, score)| {
                request
                    .score_threshold
                    .is_none_or(|threshold| *score >= threshold)
            })
            .collect();
        found.sort_by(|a, b| b.1.total_cmp(&a.1));
        found.truncate(request.limit as usize);
        Ok(Response::new(pb::SearchResponse {
            points: found
                .into_iter()
                .map(|(stored, score)| scored(stored, score))
                .collect(),
        }))
    }

    async fn hybrid_search(
        &self,
        request: Request<pb::HybridSearchRequest>,
    ) -> Result<Response<pb::HybridSearchResponse>, Status> {
        let project = self.authenticate(&request)?;
        let request = request.into_inner();
        let (dense_weight, text_weight) =
            if request.dense_weight == 0.0 && request.text_weight == 0.0 {
                (0.7, 0.3)
            } else {
                (
                    f64::from(request.dense_weight),
                    f64::from(request.text_weight),
                )
            };
        let state = self.state.lock().expect("lock");
        let candidates = state.select(
            project,
            request.scope.as_ref(),
            request.space.as_ref(),
            request.filter.as_ref(),
        )?;
        let mut dense: Vec<(usize, f32)> = candidates
            .iter()
            .enumerate()
            .map(|(index, stored)| (index, cosine(&request.vector, &stored.point.vector)))
            .filter(|(_, score)| {
                request
                    .score_threshold
                    .is_none_or(|threshold| *score >= threshold)
            })
            .collect();
        dense.sort_by(|a, b| b.1.total_cmp(&a.1));
        let terms: Vec<String> = request
            .text
            .to_lowercase()
            .split_whitespace()
            .map(str::to_owned)
            .collect();
        let mut lexical: Vec<(usize, usize)> = candidates
            .iter()
            .enumerate()
            .map(|(index, stored)| {
                let text = stored.point.text.to_lowercase();
                (
                    index,
                    terms
                        .iter()
                        .filter(|term| text.contains(term.as_str()))
                        .count(),
                )
            })
            .filter(|(_, hits)| *hits > 0)
            .collect();
        lexical.sort_by_key(|entry| std::cmp::Reverse(entry.1));
        let mut fused: HashMap<usize, f64> = HashMap::new();
        for (rank, (index, _)) in dense.iter().enumerate() {
            *fused.entry(*index).or_default() +=
                dense_weight / (RRF_K + f64::from(u32::try_from(rank).unwrap_or(u32::MAX)) + 1.0);
        }
        for (rank, (index, _)) in lexical.iter().enumerate() {
            *fused.entry(*index).or_default() +=
                text_weight / (RRF_K + f64::from(u32::try_from(rank).unwrap_or(u32::MAX)) + 1.0);
        }
        let mut ranked: Vec<(usize, f64)> = fused.into_iter().collect();
        ranked.sort_by(|a, b| b.1.total_cmp(&a.1).then(a.0.cmp(&b.0)));
        ranked.truncate(request.limit as usize);
        Ok(Response::new(pb::HybridSearchResponse {
            points: ranked
                .into_iter()
                .map(|(index, score)| {
                    #[allow(clippy::cast_possible_truncation, reason = "a test score")]
                    scored(candidates[index], score as f32)
                })
                .collect(),
        }))
    }

    async fn count(
        &self,
        request: Request<pb::CountRequest>,
    ) -> Result<Response<pb::CountResponse>, Status> {
        let project = self.authenticate(&request)?;
        let request = request.into_inner();
        let state = self.state.lock().expect("lock");
        let count = state
            .select(
                project,
                request.scope.as_ref(),
                request.space.as_ref(),
                request.filter.as_ref(),
            )?
            .len();
        Ok(Response::new(pb::CountResponse {
            count: count as u64,
        }))
    }

    async fn list_indexes(
        &self,
        request: Request<pb::ListIndexesRequest>,
    ) -> Result<Response<pb::ListIndexesResponse>, Status> {
        let project = self.authenticate(&request)?;
        let source = request.into_inner().source;
        let state = self.state.lock().expect("lock");
        let mut groups: Vec<((String, u32), i32, String, u64)> = Vec::new();
        for stored in state
            .points
            .iter()
            .filter(|s| s.project_id == project && (source == 0 || s.source == source))
        {
            if let Some(group) = groups
                .iter_mut()
                .find(|g| g.0 == stored.space && g.1 == stored.source && g.2 == stored.namespace_id)
            {
                group.3 += 1;
            } else {
                groups.push((
                    stored.space.clone(),
                    stored.source,
                    stored.namespace_id.clone(),
                    1,
                ));
            }
        }
        Ok(Response::new(pb::ListIndexesResponse {
            indexes: groups
                .into_iter()
                .map(
                    |((slug, dimension), source, namespace_id, point_count)| pb::IndexInfo {
                        collection: format!("emb_{slug}_{dimension}"),
                        space: Some(pb::EmbeddingSpace {
                            model_slug: slug,
                            dimension,
                        }),
                        source,
                        namespace_id,
                        point_count,
                    },
                )
                .collect(),
        }))
    }
}

/// A status with this code, for [`FakeVector::fail_next`].
#[must_use]
pub fn status(code: Code) -> Status {
    Status::new(code, "injected failure")
}
