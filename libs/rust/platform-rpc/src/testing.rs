//! Fake services for the callers' tests (`testing` feature): an in-memory
//! artifact store and a programmable internal tool server, served on a
//! loopback gRPC listener ([`serve`]). They answer as elitea-main does: a
//! refusal is a gRPC status carrying the refusal trailer, with the HTTP
//! status the route would have answered.

#![allow(clippy::missing_panics_doc)] // Test fakes: a poisoned lock is a failed test.

use std::collections::{BTreeMap, HashMap};
use std::pin::Pin;
use std::sync::{Arc, Mutex};

use bytes::Bytes;
use futures_util::{Stream, StreamExt as _};
use tonic::metadata::{MetadataMap, MetadataValue};
use tonic::transport::{Channel, Endpoint};
use tonic::{Request, Response, Status, Streaming};

pub use crate::pb::artifact_object_service_server::ArtifactObjectService;
use crate::pb::artifact_object_service_server::ArtifactObjectServiceServer;
use crate::pb::internal_tool_service_server::{InternalToolService, InternalToolServiceServer};
use crate::{code_for_http_status, pb};

type FrameStream<T> = Pin<Box<dyn Stream<Item = Result<T, Status>> + Send>>;

fn refused<M: prost::Message>(trailer: &'static str, http_status: u16, refusal: &M) -> Status {
    let mut metadata = MetadataMap::new();
    metadata.insert_bin(trailer, MetadataValue::from_bytes(&refusal.encode_to_vec()));
    Status::with_metadata(
        code_for_http_status(http_status),
        format!("refused (HTTP {http_status})"),
        metadata,
    )
}

/// The status elitea-main answers a refused artifact call with: the
/// refusal in its trailer, for a caller's own scripted service.
#[must_use]
pub fn artifact_refusal(http_status: u16, code: &str, message: &str) -> Status {
    refused(
        crate::artifacts::REFUSAL_TRAILER,
        http_status,
        &pb::ArtifactRefusalV1 {
            http_status: i32::from(http_status),
            code: code.to_owned(),
            message: message.to_owned(),
        },
    )
}

fn bearer_of(metadata: &MetadataMap) -> Option<String> {
    metadata
        .get("authorization")?
        .to_str()
        .ok()?
        .strip_prefix("Bearer ")
        .map(str::to_owned)
}

/// One stored object.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct StoredObject {
    pub bytes: Vec<u8>,
    pub media_type: String,
    pub etag: String,
    pub modified_at: String,
}

#[derive(Default)]
struct StoreState {
    /// token -> the project it may act in.
    grants: HashMap<String, i64>,
    buckets: BTreeMap<(i64, String), Option<i32>>,
    objects: BTreeMap<(i64, String, String), StoredObject>,
    refusals: HashMap<String, (u16, String, String)>,
    claims: Vec<pb::ArtifactRuntimeClaimV1>,
    calls: Vec<ArtifactCall>,
    page_size: usize,
    chunk_bytes: usize,
    writes: u64,
    any_bucket: bool,
    flat: bool,
}

impl StoreState {
    /// The object map's key: the bucket is ignored in a flat store.
    fn slot(&self, project_id: i64, bucket: &str, key: &str) -> (i64, String, String) {
        let bucket = if self.flat { "" } else { bucket };
        (project_id, bucket.to_owned(), key.to_owned())
    }
}

/// One call the store received, refused or not.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ArtifactCall {
    /// The RPC: `ListBuckets`, `CreateBucket`, `ListObjects`, `Stat`,
    /// `GetObject`, `PutObject`, `Delete`, `DeleteBatch`.
    pub op: &'static str,
    /// The bearer token the call carried.
    pub bearer: Option<String>,
    pub project_id: i64,
    /// The bucket (the new bucket's name for `CreateBucket`).
    pub bucket: String,
    pub key: String,
    /// `PutObject`'s overwrite flag.
    pub overwrite: bool,
    /// The claim id the call named.
    pub claim_id: Option<String>,
}

/// An in-memory `ArtifactObjectService`. A call must carry a granted bearer
/// ([`FakeArtifactStore::grant`]) naming that bearer's project, as
/// elitea-main's routes require; anything else is refused 401/403. Cheap to
/// clone: the clones share one store.
#[derive(Clone)]
pub struct FakeArtifactStore(Arc<Mutex<StoreState>>);

impl Default for FakeArtifactStore {
    fn default() -> Self {
        Self::new()
    }
}

impl FakeArtifactStore {
    /// An empty store: listing pages of 1000 keys, chunks of 1 MiB.
    #[must_use]
    pub fn new() -> Self {
        Self(Arc::new(Mutex::new(StoreState {
            page_size: 1000,
            chunk_bytes: crate::artifacts::CHUNK_BYTES,
            ..StoreState::default()
        })))
    }

    fn state(&self) -> std::sync::MutexGuard<'_, StoreState> {
        self.0.lock().expect("fake store lock")
    }

    /// Admit `token` in `project_id`.
    #[must_use]
    pub fn grant(self, token: &str, project_id: i64) -> Self {
        self.state().grants.insert(token.to_owned(), project_id);
        self
    }

    /// Create a bucket.
    #[must_use]
    pub fn with_bucket(self, project_id: i64, bucket: &str) -> Self {
        self.state()
            .buckets
            .insert((project_id, bucket.to_owned()), None);
        self
    }

    /// Store an object (its bucket too), etag `"<sha-ish>"` from its bytes.
    #[must_use]
    pub fn with_object(self, project_id: i64, bucket: &str, key: &str, bytes: &[u8]) -> Self {
        self.insert(project_id, bucket, key, bytes, "");
        self
    }

    /// Store an object in a shared store (its bucket too), as a test
    /// arranges it after the store is serving.
    pub fn put(&self, project_id: i64, bucket: &str, key: &str, bytes: &[u8], media_type: &str) {
        self.insert(project_id, bucket, key, bytes, media_type);
    }

    fn insert(&self, project_id: i64, bucket: &str, key: &str, bytes: &[u8], media_type: &str) {
        let mut state = self.state();
        state.writes += 1;
        let etag = format!("\"{:x}-{}\"", bytes.len(), state.writes);
        state
            .buckets
            .entry((project_id, bucket.to_owned()))
            .or_insert(None);
        let slot = state.slot(project_id, bucket, key);
        state.objects.insert(
            slot,
            StoredObject {
                bytes: bytes.to_vec(),
                media_type: if media_type.is_empty() {
                    "application/octet-stream".to_owned()
                } else {
                    media_type.to_owned()
                },
                etag,
                modified_at: "2026-10-21T10:00:00Z".to_owned(),
            },
        );
    }

    /// Answer every call naming `key` (or bucket `key` for a listing) with
    /// this refusal.
    #[must_use]
    pub fn refuse(self, key: &str, http_status: u16, code: &str, message: &str) -> Self {
        self.state().refusals.insert(
            key.to_owned(),
            (http_status, code.to_owned(), message.to_owned()),
        );
        self
    }

    /// Give a stored object this etag.
    #[must_use]
    pub fn with_etag(self, project_id: i64, bucket: &str, key: &str, etag: &str) -> Self {
        let mut state = self.state();
        let slot = state.slot(project_id, bucket, key);
        if let Some(object) = state.objects.get_mut(&slot) {
            etag.clone_into(&mut object.etag);
        }
        drop(state);
        self
    }

    /// Treat every bucket as existing (a fake for callers that never create
    /// one).
    #[must_use]
    pub fn any_bucket(self) -> Self {
        self.state().any_bucket = true;
        self
    }

    /// Key objects by key alone, whatever bucket a call names (every bucket
    /// exists): a fake for callers whose tests arrange objects by key.
    #[must_use]
    pub fn flat(self) -> Self {
        {
            let mut state = self.state();
            state.flat = true;
            state.any_bucket = true;
        }
        self
    }

    /// The calls received, in order.
    #[must_use]
    pub fn calls(&self) -> Vec<ArtifactCall> {
        self.state().calls.clone()
    }

    /// Listing pages of `size` keys.
    #[must_use]
    pub fn page_size(self, size: usize) -> Self {
        self.state().page_size = size.max(1);
        self
    }

    /// Object chunks of `bytes`.
    #[must_use]
    pub fn chunk_bytes(self, bytes: usize) -> Self {
        self.state().chunk_bytes = bytes.max(1);
        self
    }

    /// An object as stored.
    #[must_use]
    pub fn object(&self, project_id: i64, bucket: &str, key: &str) -> Option<StoredObject> {
        let state = self.state();
        state
            .objects
            .get(&state.slot(project_id, bucket, key))
            .cloned()
    }

    /// The claims the calls named, in order.
    #[must_use]
    pub fn claims(&self) -> Vec<pb::ArtifactRuntimeClaimV1> {
        self.state().claims.clone()
    }

    /// The bucket's retention, when it exists.
    #[must_use]
    pub fn bucket(&self, project_id: i64, bucket: &str) -> Option<Option<i32>> {
        self.state()
            .buckets
            .get(&(project_id, bucket.to_owned()))
            .copied()
    }

    /// The caller's project check: 401 without a granted bearer, 403 for
    /// another project.
    fn admit(&self, metadata: &MetadataMap, project_id: i64) -> Result<(), Status> {
        let Some(token) = bearer_of(metadata) else {
            return Err(artifact_refusal(
                401,
                "Unauthorized",
                "a bearer is required",
            ));
        };
        match self.state().grants.get(&token) {
            None => Err(artifact_refusal(
                401,
                "Unauthorized",
                "the bearer is not valid",
            )),
            Some(granted) if *granted != project_id => Err(artifact_refusal(
                403,
                "Forbidden",
                "the bearer may not act in this project",
            )),
            Some(_) => Ok(()),
        }
    }

    /// Record the call, then admit it: the bearer, the project, and the
    /// scripted refusals of its bucket and key.
    fn check(
        &self,
        metadata: &MetadataMap,
        call: ArtifactCall,
        claim: Option<&pb::ArtifactRuntimeClaimV1>,
    ) -> Result<(), Status> {
        let project_id = call.project_id;
        let names = [call.bucket.clone(), call.key.clone()];
        {
            let mut state = self.state();
            state.calls.push(ArtifactCall {
                bearer: bearer_of(metadata),
                claim_id: claim.map(|claim| claim.claim_id.clone()),
                ..call
            });
            if let Some(claim) = claim {
                state.claims.push(claim.clone());
            }
        }
        self.admit(metadata, project_id)?;
        let state = self.state();
        for name in names.iter().filter(|name| !name.is_empty()) {
            if let Some((status, code, message)) = state.refusals.get(name) {
                return Err(artifact_refusal(*status, code, message));
            }
        }
        Ok(())
    }

    fn call(op: &'static str, project_id: i64, bucket: &str, key: &str) -> ArtifactCall {
        ArtifactCall {
            op,
            bearer: None,
            project_id,
            bucket: bucket.to_owned(),
            key: key.to_owned(),
            overwrite: false,
            claim_id: None,
        }
    }

    fn info(key: &str, object: &StoredObject) -> pb::ArtifactObjectInfoV1 {
        pb::ArtifactObjectInfoV1 {
            key: key.to_owned(),
            size_bytes: i64::try_from(object.bytes.len()).unwrap_or(i64::MAX),
            media_type: object.media_type.clone(),
            etag: object.etag.clone(),
            modified_at: object.modified_at.clone(),
        }
    }

    fn require_bucket(&self, project_id: i64, bucket: &str) -> Result<(), Status> {
        let state = self.state();
        if state.any_bucket || state.buckets.contains_key(&(project_id, bucket.to_owned())) {
            Ok(())
        } else {
            Err(artifact_refusal(404, "NotFound", "bucket not found"))
        }
    }

    fn lookup(
        &self,
        op: &'static str,
        object: Option<&pb::ArtifactObjectRefV1>,
        metadata: &MetadataMap,
    ) -> Result<(pb::ArtifactObjectRefV1, StoredObject), Status> {
        let object = object
            .cloned()
            .ok_or_else(|| Status::invalid_argument("an object is required"))?;
        self.check(
            metadata,
            Self::call(op, object.project_id, &object.bucket, &object.key),
            object.claim.as_ref(),
        )?;
        self.require_bucket(object.project_id, &object.bucket)?;
        let state = self.state();
        let stored = state
            .objects
            .get(&state.slot(object.project_id, &object.bucket, &object.key))
            .cloned()
            .ok_or_else(|| artifact_refusal(404, "NotFound", "object not found"))?;
        drop(state);
        Ok((object, stored))
    }
}

#[tonic::async_trait]
impl ArtifactObjectService for FakeArtifactStore {
    type ListObjectsStream = FrameStream<pb::ArtifactListObjectsResponseV1>;
    type GetObjectStream = FrameStream<pb::ArtifactGetObjectResponseV1>;

    async fn list_buckets(
        &self,
        request: Request<pb::ArtifactListBucketsRequestV1>,
    ) -> Result<Response<pb::ArtifactListBucketsResponseV1>, Status> {
        let project_id = request.get_ref().project_id;
        self.check(
            request.metadata(),
            Self::call("ListBuckets", project_id, "", ""),
            None,
        )?;
        let buckets = self
            .state()
            .buckets
            .iter()
            .filter(|((project, _), _)| *project == project_id)
            .map(|((_, name), retention)| pb::ArtifactBucketV1 {
                name: name.clone(),
                retention_days: *retention,
                size_bytes: 0,
                object_count: 0,
            })
            .collect();
        Ok(Response::new(pb::ArtifactListBucketsResponseV1 { buckets }))
    }

    async fn create_bucket(
        &self,
        request: Request<pb::ArtifactCreateBucketRequestV1>,
    ) -> Result<Response<pb::ArtifactCreateBucketResponseV1>, Status> {
        let message = request.get_ref().clone();
        self.check(
            request.metadata(),
            Self::call("CreateBucket", message.project_id, &message.name, ""),
            None,
        )?;
        let mut state = self.state();
        let key = (message.project_id, message.name.clone());
        if state.buckets.contains_key(&key) {
            return Err(artifact_refusal(
                409,
                "AlreadyExists",
                "a bucket with this name already exists",
            ));
        }
        state.buckets.insert(key, message.retention_days);
        Ok(Response::new(pb::ArtifactCreateBucketResponseV1 {
            bucket: Some(pb::ArtifactBucketV1 {
                name: message.name,
                retention_days: message.retention_days,
                size_bytes: 0,
                object_count: 0,
            }),
        }))
    }

    async fn list_objects(
        &self,
        request: Request<pb::ArtifactListObjectsRequestV1>,
    ) -> Result<Response<Self::ListObjectsStream>, Status> {
        let message = request.get_ref().clone();
        self.check(
            request.metadata(),
            Self::call("ListObjects", message.project_id, &message.bucket, ""),
            message.claim.as_ref(),
        )?;
        self.require_bucket(message.project_id, &message.bucket)?;
        let state = self.state();
        let page_size = usize::try_from(message.page_size)
            .ok()
            .filter(|size| *size > 0)
            .unwrap_or(state.page_size);
        let keys = state
            .objects
            .iter()
            .filter(|((project, bucket, key), _)| {
                *project == message.project_id
                    && (state.flat || *bucket == message.bucket)
                    && key.starts_with(&message.prefix)
                    && key.as_str() > message.cursor.as_str()
            })
            .map(|((_, _, key), object)| Self::info(key, object))
            .collect::<Vec<_>>();
        drop(state);
        let max_pages = usize::try_from(message.max_pages).unwrap_or(0).max(1);
        let mut pages = Vec::new();
        for (index, chunk) in keys.chunks(page_size).enumerate() {
            let more = (index + 1) * page_size < keys.len();
            let next_cursor = if more {
                chunk
                    .last()
                    .map(|info| info.key.clone())
                    .unwrap_or_default()
            } else {
                String::new()
            };
            pages.push(Ok(pb::ArtifactListObjectsResponseV1 {
                objects: chunk.to_vec(),
                common_prefixes: Vec::new(),
                next_cursor,
            }));
            if pages.len() == max_pages {
                break;
            }
        }
        if pages.is_empty() {
            pages.push(Ok(pb::ArtifactListObjectsResponseV1::default()));
        }
        Ok(Response::new(Box::pin(futures_util::stream::iter(pages))))
    }

    async fn stat(
        &self,
        request: Request<pb::ArtifactStatRequestV1>,
    ) -> Result<Response<pb::ArtifactStatResponseV1>, Status> {
        let (object, stored) = self.lookup(
            "Stat",
            request.get_ref().object.as_ref(),
            request.metadata(),
        )?;
        Ok(Response::new(pb::ArtifactStatResponseV1 {
            info: Some(Self::info(&object.key, &stored)),
        }))
    }

    async fn get_object(
        &self,
        request: Request<pb::ArtifactGetObjectRequestV1>,
    ) -> Result<Response<Self::GetObjectStream>, Status> {
        use pb::artifact_get_object_response_v1::Frame;
        let (object, stored) = self.lookup(
            "GetObject",
            request.get_ref().object.as_ref(),
            request.metadata(),
        )?;
        let chunk_bytes = self.state().chunk_bytes;
        let bytes = Bytes::from(stored.bytes.clone());
        let mut frames = vec![Ok(pb::ArtifactGetObjectResponseV1 {
            frame: Some(Frame::Info(Self::info(&object.key, &stored))),
        })];
        for start in (0..bytes.len()).step_by(chunk_bytes) {
            frames.push(Ok(pb::ArtifactGetObjectResponseV1 {
                frame: Some(Frame::Data(
                    bytes.slice(start..(start + chunk_bytes).min(bytes.len())),
                )),
            }));
        }
        Ok(Response::new(Box::pin(futures_util::stream::iter(frames))))
    }

    async fn put_object(
        &self,
        request: Request<Streaming<pb::ArtifactPutObjectRequestV1>>,
    ) -> Result<Response<pb::ArtifactPutObjectResponseV1>, Status> {
        use pb::artifact_put_object_request_v1::Frame;
        let metadata = request.metadata().clone();
        let mut frames = request.into_inner();
        let Some(pb::ArtifactPutObjectRequestV1 {
            frame: Some(Frame::Head(head)),
        }) = frames.message().await?
        else {
            return Err(Status::invalid_argument(
                "a PutObject call starts with its head",
            ));
        };
        self.check(
            &metadata,
            ArtifactCall {
                overwrite: head.overwrite,
                ..Self::call("PutObject", head.project_id, &head.bucket, &head.key)
            },
            head.claim.as_ref(),
        )?;
        self.require_bucket(head.project_id, &head.bucket)?;
        let mut body = Vec::new();
        while let Some(frame) = frames.message().await? {
            match frame.frame {
                Some(Frame::Data(data)) => body.extend_from_slice(&data),
                Some(Frame::Head(_)) => {
                    return Err(Status::invalid_argument("a PutObject call has one head"));
                }
                None => {}
            }
        }
        if !head.overwrite
            && self
                .object(head.project_id, &head.bucket, &head.key)
                .is_some()
        {
            return Err(artifact_refusal(
                409,
                "AlreadyExists",
                "key already exists; pass overwrite=true to replace it",
            ));
        }
        self.insert(
            head.project_id,
            &head.bucket,
            &head.key,
            &body,
            &head.media_type,
        );
        let stored = self
            .object(head.project_id, &head.bucket, &head.key)
            .expect("just stored");
        Ok(Response::new(pb::ArtifactPutObjectResponseV1 {
            info: Some(Self::info(&head.key, &stored)),
        }))
    }

    async fn delete(
        &self,
        request: Request<pb::ArtifactDeleteRequestV1>,
    ) -> Result<Response<pb::ArtifactDeleteResponseV1>, Status> {
        let (object, _) = self.lookup(
            "Delete",
            request.get_ref().object.as_ref(),
            request.metadata(),
        )?;
        let mut state = self.state();
        let slot = state.slot(object.project_id, &object.bucket, &object.key);
        state.objects.remove(&slot);
        Ok(Response::new(pb::ArtifactDeleteResponseV1 {}))
    }

    async fn delete_batch(
        &self,
        request: Request<pb::ArtifactDeleteBatchRequestV1>,
    ) -> Result<Response<pb::ArtifactDeleteBatchResponseV1>, Status> {
        let message = request.get_ref().clone();
        self.check(
            request.metadata(),
            Self::call("DeleteBatch", message.project_id, &message.bucket, ""),
            message.claim.as_ref(),
        )?;
        self.require_bucket(message.project_id, &message.bucket)?;
        let mut answer = pb::ArtifactDeleteBatchResponseV1::default();
        let mut state = self.state();
        for key in message.keys {
            let slot = state.slot(message.project_id, &message.bucket, &key);
            if state.objects.remove(&slot).is_some() {
                answer.deleted.push(key);
            } else {
                answer.failed.push(pb::ArtifactDeleteFailureV1 {
                    key,
                    code: "NotFound".to_owned(),
                    message: "object not found".to_owned(),
                });
            }
        }
        Ok(Response::new(answer))
    }
}

/// One recorded internal tool call.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct RecordedCall {
    pub bearer: String,
    pub project_id: i64,
    pub scope: String,
    pub name: String,
    pub arguments: Bytes,
}

#[derive(Default)]
struct ToolState {
    token: String,
    list: Option<pb::internal_tool_list_response_v1::Outcome>,
    call_frames: Vec<pb::InternalToolCallFrameV1>,
    invoke_frames: Vec<pb::InternalToolInvokeFrameV1>,
    refusal: Option<(u16, Bytes)>,
    calls: Vec<RecordedCall>,
    hang: bool,
    released: Arc<std::sync::atomic::AtomicUsize>,
}

/// Counts the call streams the server let go of (the client cancelled).
struct ReleaseGuard(Arc<std::sync::atomic::AtomicUsize>);

impl Drop for ReleaseGuard {
    fn drop(&mut self) {
        self.0.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
    }
}

/// A programmable `InternalToolService`: one accepted bearer, one listing,
/// one scripted frame sequence for every call and every toolkit invocation,
/// or one refusal for everything.
#[derive(Clone)]
pub struct FakeInternalTools(Arc<Mutex<ToolState>>);

impl FakeInternalTools {
    /// A server that admits `token` (others are refused 401).
    #[must_use]
    pub fn new(token: &str) -> Self {
        Self(Arc::new(Mutex::new(ToolState {
            token: token.to_owned(),
            ..ToolState::default()
        })))
    }

    fn state(&self) -> std::sync::MutexGuard<'_, ToolState> {
        self.0.lock().expect("fake tools lock")
    }

    /// The `tools/list` result (`{"tools": [...]}` as JSON text).
    #[must_use]
    pub fn with_tools(self, result_json: &str) -> Self {
        self.state().list = Some(pb::internal_tool_list_response_v1::Outcome::ResultJson(
            Bytes::from(result_json.to_owned()),
        ));
        self
    }

    /// `tools/list` answers this JSON-RPC error.
    #[must_use]
    pub fn with_list_error(self, code: i32, message: &str) -> Self {
        self.state().list = Some(pb::internal_tool_list_response_v1::Outcome::Error(
            pb::InternalToolRpcErrorV1 {
                code,
                message: message.to_owned(),
            },
        ));
        self
    }

    /// Every `tools/call` streams `progress` stages, then `result_json`.
    #[must_use]
    pub fn with_call_result(self, progress: &[&str], result_json: &str) -> Self {
        use pb::internal_tool_call_frame_v1::Frame;
        let mut frames = progress
            .iter()
            .map(|stage| pb::InternalToolCallFrameV1 {
                frame: Some(Frame::Progress(pb::InternalToolProgressV1 {
                    stage: (*stage).to_owned(),
                })),
            })
            .collect::<Vec<_>>();
        frames.push(pb::InternalToolCallFrameV1 {
            frame: Some(Frame::ResultJson(Bytes::from(result_json.to_owned()))),
        });
        self.state().call_frames = frames;
        self
    }

    /// Every `tools/call` answers this JSON-RPC error.
    #[must_use]
    pub fn with_call_error(self, code: i32, message: &str) -> Self {
        self.state().call_frames = vec![pb::InternalToolCallFrameV1 {
            frame: Some(pb::internal_tool_call_frame_v1::Frame::Error(
                pb::InternalToolRpcErrorV1 {
                    code,
                    message: message.to_owned(),
                },
            )),
        }];
        self
    }

    /// Every toolkit invocation streams `progress` stages, then
    /// `result_json`.
    #[must_use]
    pub fn with_invoke_result(self, progress: &[&str], result_json: &str) -> Self {
        use pb::internal_tool_invoke_frame_v1::Frame;
        let mut frames = progress
            .iter()
            .map(|stage| pb::InternalToolInvokeFrameV1 {
                frame: Some(Frame::Progress(pb::InternalToolProgressV1 {
                    stage: (*stage).to_owned(),
                })),
            })
            .collect::<Vec<_>>();
        frames.push(pb::InternalToolInvokeFrameV1 {
            frame: Some(Frame::ResultJson(Bytes::from(result_json.to_owned()))),
        });
        self.state().invoke_frames = frames;
        self
    }

    /// Every `tools/call` streams its scripted frames and then never ends;
    /// [`FakeInternalTools::released`] counts the calls the client dropped.
    #[must_use]
    pub fn hanging(self) -> Self {
        self.state().hang = true;
        self
    }

    /// The hanging calls the server let go of.
    #[must_use]
    pub fn released(&self) -> usize {
        self.state()
            .released
            .load(std::sync::atomic::Ordering::SeqCst)
    }

    /// Every call is refused with this HTTP status and body.
    #[must_use]
    pub fn refusing(self, http_status: u16, body: &str) -> Self {
        self.state().refusal = Some((http_status, Bytes::from(body.to_owned())));
        self
    }

    /// The calls and invocations made, in order (an invocation records its
    /// toolkit id as `scope` and its tool as `name`).
    #[must_use]
    pub fn calls(&self) -> Vec<RecordedCall> {
        self.state().calls.clone()
    }

    fn admit(&self, metadata: &MetadataMap) -> Result<String, Status> {
        let state = self.state();
        if let Some((status, body)) = &state.refusal {
            return Err(refused(
                crate::internal_tools::REFUSAL_TRAILER,
                *status,
                &pb::InternalToolRefusalV1 {
                    http_status: i32::from(*status),
                    body: body.clone(),
                },
            ));
        }
        match bearer_of(metadata) {
            Some(token) if token == state.token => Ok(token),
            _ => Err(refused(
                crate::internal_tools::REFUSAL_TRAILER,
                401,
                &pb::InternalToolRefusalV1 {
                    http_status: 401,
                    body: Bytes::from_static(br#"{"error":"Unauthorized"}"#),
                },
            )),
        }
    }
}

#[tonic::async_trait]
impl InternalToolService for FakeInternalTools {
    type CallToolStream = FrameStream<pb::InternalToolCallFrameV1>;
    type InvokeToolkitStream = FrameStream<pb::InternalToolInvokeFrameV1>;

    async fn list_tools(
        &self,
        request: Request<pb::InternalToolListRequestV1>,
    ) -> Result<Response<pb::InternalToolListResponseV1>, Status> {
        self.admit(request.metadata())?;
        Ok(Response::new(pb::InternalToolListResponseV1 {
            outcome: self.state().list.clone(),
        }))
    }

    async fn call_tool(
        &self,
        request: Request<pb::InternalToolCallRequestV1>,
    ) -> Result<Response<Self::CallToolStream>, Status> {
        let bearer = self.admit(request.metadata())?;
        let message = request.into_inner();
        let scope = message.scope.unwrap_or_default();
        let mut state = self.state();
        state.calls.push(RecordedCall {
            bearer,
            project_id: scope.project_id,
            scope: scope.scope,
            name: message.name,
            arguments: message.arguments_json,
        });
        let frames = futures_util::stream::iter(state.call_frames.clone().into_iter().map(Ok));
        if !state.hang {
            return Ok(Response::new(Box::pin(frames)));
        }
        let guard = ReleaseGuard(Arc::clone(&state.released));
        let held = futures_util::stream::pending().map(move |frame| {
            let _ = &guard;
            frame
        });
        Ok(Response::new(Box::pin(frames.chain(held))))
    }

    async fn invoke_toolkit(
        &self,
        request: Request<pb::InternalToolInvokeRequestV1>,
    ) -> Result<Response<Self::InvokeToolkitStream>, Status> {
        let bearer = self.admit(request.metadata())?;
        let message = request.into_inner();
        let mut state = self.state();
        state.calls.push(RecordedCall {
            bearer,
            project_id: message.project_id,
            scope: message.toolkit_id,
            name: message.tool_name,
            arguments: message.tool_params_json,
        });
        let frames = state.invoke_frames.clone().into_iter().map(Ok);
        Ok(Response::new(Box::pin(futures_util::stream::iter(frames))))
    }
}

/// Serve `artifacts` and/or `tools` on a loopback listener and return a
/// plaintext channel to it (the listener lives as long as the runtime).
///
/// # Panics
///
/// Outside a Tokio runtime, or the loopback listener cannot be bound.
#[must_use]
pub fn serve(artifacts: Option<FakeArtifactStore>, tools: Option<FakeInternalTools>) -> Channel {
    let mut routes = tonic::service::RoutesBuilder::default();
    if let Some(artifacts) = artifacts {
        routes.add_service(ArtifactObjectServiceServer::new(artifacts));
    }
    if let Some(tools) = tools {
        routes.add_service(InternalToolServiceServer::new(tools));
    }
    serve_routes(routes.routes())
}

/// Serve a caller's own scripted `ArtifactObjectService` (a test that needs
/// answers the in-memory store never gives: a lying size, a stream that
/// hangs) as [`serve`] does.
///
/// # Panics
///
/// Outside a Tokio runtime, or the loopback listener cannot be bound.
#[must_use]
pub fn serve_artifacts<S: ArtifactObjectService>(service: S) -> Channel {
    let mut routes = tonic::service::RoutesBuilder::default();
    routes.add_service(ArtifactObjectServiceServer::new(service));
    serve_routes(routes.routes())
}

fn serve_routes(routes: tonic::service::Routes) -> Channel {
    let listener = std::net::TcpListener::bind("127.0.0.1:0").expect("bind a loopback listener");
    listener
        .set_nonblocking(true)
        .expect("a non-blocking listener");
    let listener = tokio::net::TcpListener::from_std(listener).expect("a Tokio listener");
    let address = listener.local_addr().expect("listener address");
    tokio::spawn(async move {
        let _ = tonic::transport::Server::builder()
            .add_routes(routes)
            .serve_with_incoming(tokio_stream::wrappers::TcpListenerStream::new(listener))
            .await;
    });
    Endpoint::from_shared(format!("http://{address}"))
        .expect("a loopback endpoint")
        .connect_lazy()
}
