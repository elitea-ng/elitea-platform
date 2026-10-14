//! An in-process fake of `elitea-vector`, for tests (feature `test-server`).
//!
//! It is **not** the service: it stores points in memory, scores them with
//! exact cosine similarity, and understands the same filter shape (`must`,
//! `should`, `must_not` of keyword, any, integer, boolean, range, `is_empty`
//! and `is_null` conditions on the point fields and on `metadata.<path>`,
//! and nested filters), with Qdrant's semantics ([`matches_filter`]). It
//! checks the bearer token and the project scope, active generations
//! included. It does not run Qdrant, BM25 statistics or quantization. It
//! counts calls ([`FakeVector::calls`]).
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
    /// The memory owner the fake stamps, like the service (`""` for other
    /// sources).
    owner: String,
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
    /// Every call, by method name, in order.
    calls: Vec<&'static str>,
    /// Errors to return for the next calls, oldest first.
    failures: Vec<Status>,
    /// Errors to return for a later call of one method: `(method, calls of
    /// that method still to pass, status)`.
    scheduled: Vec<(&'static str, usize, Status)>,
    /// The `space` of every `list_documents` call, in order.
    listings: Vec<Option<(String, u32)>>,
}

/// The fake service. Clone it to keep a handle on its state.
#[derive(Clone, Default)]
pub struct FakeVector {
    tokens: Arc<HashMap<String, i64>>,
    /// Token to the user it was minted for and whether personal memory is
    /// allowed for its claim; a token not listed names no user.
    users: Arc<HashMap<String, (i64, bool)>>,
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
            users: Arc::default(),
            state: Arc::default(),
        }
    }

    /// The same fake where each listed token was minted for a user
    /// (`personal_memory_allowed` true), as a worker claim token is.
    #[must_use]
    pub fn with_users(mut self, users: &[(&str, i64)]) -> Self {
        self.users = Arc::new(
            users
                .iter()
                .map(|(token, user)| ((*token).to_owned(), (*user, true)))
                .collect(),
        );
        self
    }

    /// Marks `token`'s claim as running in a conversation with more than one
    /// person: personal memory is refused for it.
    #[must_use]
    pub fn with_shared_conversation(mut self, token: &str) -> Self {
        let mut users = (*self.users).clone();
        if let Some(entry) = users.get_mut(token) {
            entry.1 = false;
        }
        self.users = Arc::new(users);
        self
    }

    /// The next call fails with this status (before anything else).
    ///
    /// # Panics
    ///
    /// If the state lock is poisoned.
    pub fn fail_next(&self, status: Status) {
        self.state.lock().expect("lock").failures.push(status);
    }

    /// The `after`-th call of `method` counted from now (1 is its next call)
    /// fails with this status, before anything else; earlier calls of the
    /// method, and every other method, proceed. Several may be scheduled.
    ///
    /// # Panics
    ///
    /// If `after` is 0 or the state lock is poisoned.
    pub fn fail_call(&self, method: &'static str, after: usize, status: Status) {
        assert!(after > 0, "the next call is 1");
        self.state
            .lock()
            .expect("lock")
            .scheduled
            .push((method, after, status));
    }

    /// The `space` each `list_documents` call so far named, in order.
    ///
    /// # Panics
    ///
    /// If the state lock is poisoned.
    #[must_use]
    pub fn listed_spaces(&self) -> Vec<Option<(String, u32)>> {
        self.state.lock().expect("lock").listings.clone()
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

    /// The method names of all calls so far (`search`, `hybrid_search`, …).
    ///
    /// # Panics
    ///
    /// If the state lock is poisoned.
    #[must_use]
    pub fn calls(&self) -> Vec<&'static str> {
        self.state.lock().expect("lock").calls.clone()
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

    fn authenticate<T>(&self, method: &'static str, request: &Request<T>) -> Result<i64, Status> {
        let header = request
            .metadata()
            .get("authorization")
            .and_then(|value| value.to_str().ok())
            .unwrap_or("");
        let mut state = self.state.lock().expect("lock");
        state.bearers.push(header.to_owned());
        state.calls.push(method);
        if !state.failures.is_empty() {
            return Err(state.failures.remove(0));
        }
        let mut due = None;
        for (position, (scheduled_method, remaining, _)) in state.scheduled.iter_mut().enumerate() {
            if *scheduled_method == method {
                *remaining -= 1;
                if *remaining == 0 && due.is_none() {
                    due = Some(position);
                }
            }
        }
        if let Some(position) = due {
            let (_, _, status) = state.scheduled.remove(position);
            state.scheduled.retain(|(_, remaining, _)| *remaining > 0);
            return Err(status);
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

impl FakeVector {
    /// The bearer of a request, without the `Bearer ` prefix.
    fn token_of<T>(request: &Request<T>) -> String {
        request
            .metadata()
            .get("authorization")
            .and_then(|value| value.to_str().ok())
            .and_then(|value| value.strip_prefix("Bearer "))
            .unwrap_or("")
            .to_owned()
    }

    /// The memory owner the service would stamp, from the TOKEN only.
    ///
    /// The fake folds the toolkit id into the owner string (`user:7@41`),
    /// where the service keeps it in its own payload key; the effect, a store
    /// per (owner, toolkit), is the same.
    fn owner_for(
        &self,
        token: &str,
        source: i32,
        scope: i32,
        toolkit_id: i64,
    ) -> Result<Option<String>, Status> {
        if source != pb::Source::Memory as i32 {
            return if scope == 0 && toolkit_id == 0 {
                Ok(None)
            } else {
                Err(Status::invalid_argument(
                    "memory_scope and toolkit_id apply to the memory source only",
                ))
            };
        }
        if toolkit_id <= 0 {
            return Err(Status::invalid_argument(
                "toolkit_id is required for the memory source",
            ));
        }
        match pb::MemoryScope::try_from(scope) {
            Ok(pb::MemoryScope::Project) => Ok(Some(format!("project@{toolkit_id}"))),
            Ok(pb::MemoryScope::Personal) => {
                let Some((user, allowed)) = self.users.get(token).copied() else {
                    return Err(Status::permission_denied(
                        "memory.personal_unavailable: this token names no user, so there is no personal memory",
                    ));
                };
                if !allowed {
                    return Err(Status::permission_denied(
                        "memory.personal_shared_conversation: personal memory is not available in a conversation with more than one person",
                    ));
                }
                Ok(Some(format!("user:{user}@{toolkit_id}")))
            }
            _ => Err(Status::invalid_argument(
                "memory_scope is required for the memory source",
            )),
        }
    }

    fn scope_owner(
        &self,
        token: &str,
        scope: Option<&pb::Scope>,
    ) -> Result<Option<String>, Status> {
        let scope = scope.ok_or_else(|| Status::invalid_argument("scope is required"))?;
        self.owner_for(token, scope.source, scope.memory_scope, scope.toolkit_id)
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

/// The payload value of `key` on `point`, as Qdrant sees it: `None` when
/// missing.
fn payload_value(point: &pb::Point, key: &str) -> Option<Value> {
    if key == "acl" {
        return Some(Value::from(point.acl.clone()));
    }
    if let Some(text) = field(point, key) {
        return Some(Value::String(text.to_owned()));
    }
    let path = key.strip_prefix("metadata.")?;
    let mut current: Value = serde_json::from_str(&point.metadata_json).ok()?;
    for part in path.split('.') {
        current = match current {
            Value::Object(mut map) => map.remove(part)?,
            _ => return None,
        };
    }
    Some(current)
}

/// Whether one scalar satisfies a match. Qdrant matches an array when any
/// element does.
fn scalar_matches(r#match: &pb::condition::Match, value: &Value) -> bool {
    use pb::condition::Match;
    match (r#match, value) {
        (_, Value::Array(items)) => items.iter().any(|item| scalar_matches(r#match, item)),
        (Match::Keyword(wanted), Value::String(text)) => text == wanted,
        (Match::Any(list), Value::String(text)) => list.values.contains(text),
        (Match::Integer(wanted), Value::Number(number)) => number.as_i64() == Some(*wanted),
        (Match::Boolean(wanted), Value::Bool(flag)) => flag == wanted,
        (Match::Range(range), Value::Number(number)) => number.as_f64().is_some_and(|number| {
            range.gt.is_none_or(|bound| number > bound)
                && range.gte.is_none_or(|bound| number >= bound)
                && range.lt.is_none_or(|bound| number < bound)
                && range.lte.is_none_or(|bound| number <= bound)
        }),
        _ => false,
    }
}

fn matches_condition(point: &pb::Point, condition: &pb::Condition) -> bool {
    use pb::condition::Match;
    let Some(r#match) = &condition.r#match else {
        return false;
    };
    let value = payload_value(point, &condition.key);
    match r#match {
        Match::Filter(inner) => matches_filter(point, Some(inner)),
        Match::IsEmpty(_) => match value {
            None | Some(Value::Null) => true,
            Some(Value::Array(items)) => items.is_empty(),
            Some(_) => false,
        },
        Match::IsNull(_) => matches!(value, Some(Value::Null)),
        other => value.is_some_and(|value| scalar_matches(other, &value)),
    }
}

/// Qdrant's semantics: every `must` holds, at least one `should` holds
/// (when there are any), no `must_not` holds. Exposed so tests can check a
/// translated filter against a reference without a server.
#[must_use]
pub fn matches_filter(point: &pb::Point, filter: Option<&pb::Filter>) -> bool {
    let Some(filter) = filter else {
        return true;
    };
    filter.must.iter().all(|c| matches_condition(point, c))
        && (filter.should.is_empty() || filter.should.iter().any(|c| matches_condition(point, c)))
        && !filter.must_not.iter().any(|c| matches_condition(point, c))
}

/// Whether a stored point's generation passes the scope.
fn generation_passes(scope: &pb::Scope, stored: &Stored) -> bool {
    if !scope.generation.is_empty() {
        return stored.generation == scope.generation;
    }
    scope
        .active_generations
        .iter()
        .find(|active| active.namespace_id == stored.namespace_id)
        .is_none_or(|active| stored.generation.is_empty() || stored.generation == active.generation)
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
        dense_score: Some(score),
    }
}

impl State {
    /// The stored points a read scope, space and filter select.
    fn select(
        &self,
        project: i64,
        owner: Option<&str>,
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
                    && owner.is_none_or(|owner| stored.owner == owner)
                    && stored.source == scope.source
                    && scope.namespace_ids.contains(&stored.namespace_id)
                    && generation_passes(scope, stored)
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
        let project = self.authenticate("upsert", &request)?;
        let token = Self::token_of(&request);
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
        let owner = self
            .owner_for(
                &token,
                namespace.source,
                namespace.memory_scope,
                namespace.toolkit_id,
            )?
            .unwrap_or_default();
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
                // One collection per embedding space: a copy in another
                // space is a different point.
                state.points.retain(|stored| {
                    !(stored.space == space
                        && stored.owner == owner
                        && stored.project_id == project
                        && stored.namespace_id == namespace.namespace_id
                        && stored.generation == namespace.generation
                        && stored.point.document_key == point.document_key
                        && stored.point.chunk_id == point.chunk_id
                        && stored.point.chunk_type == point.chunk_type)
                });
                state.points.push(Stored {
                    project_id: project,
                    owner: owner.clone(),
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
        let project = self.authenticate("delete", &request)?;
        let token = Self::token_of(&request);
        let request = request.into_inner();
        let namespace = request
            .namespace
            .ok_or_else(|| Status::invalid_argument("namespace is required"))?;
        let owner = self.owner_for(
            &token,
            namespace.source,
            namespace.memory_scope,
            namespace.toolkit_id,
        )?;
        let space = request.space.map(|s| (s.model_slug, s.dimension));
        let selector = request
            .selector
            .ok_or_else(|| Status::invalid_argument("a selector is required"))?;
        let mut state = self.state.lock().expect("lock");
        let before = state.points.len();
        state.points.retain(|stored| {
            let in_namespace = stored.project_id == project
                && owner.as_deref().is_none_or(|owner| stored.owner == owner)
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
                // Only versioned points of another generation: an
                // unversioned (incremental) point is kept, as the service
                // keeps it.
                pb::delete_request::Selector::GenerationsExcept(keep) => {
                    !stored.generation.is_empty() && stored.generation != keep.keep
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
        self.authenticate("drop_project", &request)?;
        Err(Status::permission_denied(
            "only an administrator can drop a project",
        ))
    }

    async fn search(
        &self,
        request: Request<pb::SearchRequest>,
    ) -> Result<Response<pb::SearchResponse>, Status> {
        let project = self.authenticate("search", &request)?;
        let token = Self::token_of(&request);
        let request = request.into_inner();
        let owner = self.scope_owner(&token, request.scope.as_ref())?;
        let state = self.state.lock().expect("lock");
        let mut found: Vec<(&Stored, f32)> = state
            .select(
                project,
                owner.as_deref(),
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
        let project = self.authenticate("hybrid_search", &request)?;
        let token = Self::token_of(&request);
        let request = request.into_inner();
        let owner = self.scope_owner(&token, request.scope.as_ref())?;
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
            owner.as_deref(),
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
                    let mut point = scored(candidates[index], score as f32);
                    point.dense_score =
                        Some(cosine(&request.vector, &candidates[index].point.vector));
                    point
                })
                .collect(),
        }))
    }

    async fn count(
        &self,
        request: Request<pb::CountRequest>,
    ) -> Result<Response<pb::CountResponse>, Status> {
        let project = self.authenticate("count", &request)?;
        let token = Self::token_of(&request);
        let request = request.into_inner();
        let owner = self.scope_owner(&token, request.scope.as_ref())?;
        let state = self.state.lock().expect("lock");
        let count = state
            .select(
                project,
                owner.as_deref(),
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
        let project = self.authenticate("list_indexes", &request)?;
        let source = request.into_inner().source;
        if source == pb::Source::Memory as i32 {
            return Err(Status::invalid_argument(
                "memory is not an index and is never listed",
            ));
        }
        let state = self.state.lock().expect("lock");
        let mut groups: Vec<((String, u32), i32, String, u64)> = Vec::new();
        for stored in state.points.iter().filter(|s| {
            s.project_id == project
                && s.source != pb::Source::Memory as i32
                && (source == 0 || s.source == source)
        }) {
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

    type ListDocumentsStream = std::pin::Pin<
        Box<
            dyn tokio_stream::Stream<Item = Result<pb::ListDocumentsResponse, Status>>
                + Send
                + 'static,
        >,
    >;

    async fn list_documents(
        &self,
        request: Request<pb::ListDocumentsRequest>,
    ) -> Result<Response<Self::ListDocumentsStream>, Status> {
        let project = self.authenticate("list_documents", &request)?;
        let token = Self::token_of(&request);
        let request = request.into_inner();
        let namespace = request
            .namespace
            .ok_or_else(|| Status::invalid_argument("namespace is required"))?;
        let owner = self.owner_for(
            &token,
            namespace.source,
            namespace.memory_scope,
            namespace.toolkit_id,
        )?;
        let space = request.space.map(|s| (s.model_slug, s.dimension));
        let mut state = self.state.lock().expect("lock");
        state.listings.push(space.clone());
        let mut counts: std::collections::BTreeMap<(String, String), u64> =
            std::collections::BTreeMap::new();
        for stored in state.points.iter().filter(|stored| {
            stored.project_id == project
                && owner.as_deref().is_none_or(|owner| stored.owner == owner)
                && stored.source == namespace.source
                && stored.namespace_id == namespace.namespace_id
                && (namespace.generation.is_empty() || stored.generation == namespace.generation)
                && space.as_ref().is_none_or(|space| *space == stored.space)
        }) {
            *counts
                .entry((
                    stored.point.document_key.clone(),
                    stored.point.document_version.clone(),
                ))
                .or_default() += 1;
        }
        drop(state);
        // The real facade refuses before the first page once the distinct
        // documents exceed the caller's bound.
        if request.max_documents != 0 && counts.len() as u64 > request.max_documents {
            return Err(Status::resource_exhausted(format!(
                "the namespace holds more than {} documents",
                request.max_documents
            )));
        }
        let documents: Vec<pb::IndexedDocument> = counts
            .into_iter()
            .map(
                |((document_key, document_version), chunk_count)| pb::IndexedDocument {
                    document_key,
                    document_version,
                    chunk_count,
                },
            )
            .collect();
        // Two pages whenever there is more than one document, so a client
        // that stopped after the first page would be caught.
        let split = documents.len() / 2;
        let pages: Vec<Result<pb::ListDocumentsResponse, Status>> =
            [&documents[..split], &documents[split..]]
                .into_iter()
                .filter(|page| !page.is_empty())
                .map(|page| {
                    Ok(pb::ListDocumentsResponse {
                        documents: page.to_vec(),
                    })
                })
                .collect();
        Ok(Response::new(Box::pin(tokio_stream::iter(pages))))
    }

    async fn read_memory(
        &self,
        request: Request<pb::ReadMemoryRequest>,
    ) -> Result<Response<pb::ReadMemoryResponse>, Status> {
        let project = self.authenticate("read_memory", &request)?;
        let token = Self::token_of(&request);
        let request = request.into_inner();
        let scope = request
            .scope
            .as_ref()
            .ok_or_else(|| Status::invalid_argument("scope is required"))?;
        if scope.source != pb::Source::Memory as i32 || scope.namespace_ids.len() != 1 {
            return Err(Status::invalid_argument(
                "a memory read names the memory source and exactly one namespace",
            ));
        }
        let owner = self.owner_for(&token, scope.source, scope.memory_scope, scope.toolkit_id)?;
        if request.limit == 0 || request.limit > 1000 {
            return Err(Status::invalid_argument("limit is 1 to 1000"));
        }
        let state = self.state.lock().expect("lock");
        let mut points: Vec<pb::ScoredPoint> = state
            .select(
                project,
                owner.as_deref(),
                request.scope.as_ref(),
                request.space.as_ref(),
                request.filter.as_ref(),
            )?
            .into_iter()
            .map(|stored| scored(stored, 0.0))
            .collect();
        // Newest version first (valid_from, then the larger version id
        // /t/id, then the point id), like the service.
        points.sort_by_cached_key(|point| {
            let value = serde_json::from_str::<Value>(&point.metadata_json).ok();
            let valid_from = value
                .as_ref()
                .and_then(|value| value.pointer("/t/valid_from"))
                .and_then(Value::as_i64)
                .unwrap_or(0);
            let version_id = match value.as_ref().and_then(|value| value.pointer("/t/id")) {
                Some(Value::String(text)) => text.clone(),
                Some(Value::Number(number)) => number.to_string(),
                _ => String::new(),
            };
            std::cmp::Reverse((valid_from, version_id, point.id.clone()))
        });
        points.truncate(request.limit as usize);
        Ok(Response::new(pb::ReadMemoryResponse { points }))
    }

    async fn set_memory_metadata(
        &self,
        request: Request<pb::SetMemoryMetadataRequest>,
    ) -> Result<Response<pb::SetMemoryMetadataResponse>, Status> {
        let project = self.authenticate("set_memory_metadata", &request)?;
        let token = Self::token_of(&request);
        let request = request.into_inner();
        let namespace = request
            .namespace
            .ok_or_else(|| Status::invalid_argument("namespace is required"))?;
        if namespace.source != pb::Source::Memory as i32 {
            return Err(Status::invalid_argument(
                "SetMemoryMetadata applies to the memory source only",
            ));
        }
        let owner = self
            .owner_for(
                &token,
                namespace.source,
                namespace.memory_scope,
                namespace.toolkit_id,
            )?
            .unwrap_or_default();
        if request.document_key.is_empty()
            || request.chunk_id.is_empty()
            || !matches!(
                serde_json::from_str::<Value>(&request.metadata_json),
                Ok(Value::Object(_))
            )
        {
            return Err(Status::invalid_argument(
                "document_key, chunk_id and a JSON object are required",
            ));
        }
        let space = request.space.map(|s| (s.model_slug, s.dimension));
        let mut state = self.state.lock().expect("lock");
        for stored in state.points.iter_mut().filter(|stored| {
            stored.project_id == project
                && stored.owner == owner
                && stored.source == namespace.source
                && stored.namespace_id == namespace.namespace_id
                && (namespace.generation.is_empty() || stored.generation == namespace.generation)
                && stored.point.document_key == request.document_key
                && stored.point.chunk_id == request.chunk_id
                && space.as_ref().is_none_or(|space| *space == stored.space)
        }) {
            stored
                .point
                .metadata_json
                .clone_from(&request.metadata_json);
        }
        Ok(Response::new(pb::SetMemoryMetadataResponse {}))
    }

    async fn delete_memory_user(
        &self,
        request: Request<pb::DeleteMemoryUserRequest>,
    ) -> Result<Response<pb::DeleteMemoryUserResponse>, Status> {
        self.authenticate("delete_memory_user", &request)?;
        Err(Status::permission_denied(
            "only an administrator can delete a user's memories",
        ))
    }

    async fn delete_memory_toolkit(
        &self,
        request: Request<pb::DeleteMemoryToolkitRequest>,
    ) -> Result<Response<pb::DeleteMemoryToolkitResponse>, Status> {
        self.authenticate("delete_memory_toolkit", &request)?;
        Err(Status::permission_denied(
            "only an administrator can delete a memory toolkit",
        ))
    }
}

/// A status with this code, for [`FakeVector::fail_next`].
#[must_use]
pub fn status(code: Code) -> Status {
    Status::new(code, "injected failure")
}
