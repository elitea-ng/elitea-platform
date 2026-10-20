//! The store operations of one thread (ADR-0034 decision 14 and the Phase 1
//! wire contract §2.2, §2.3, §2.5), whatever carries them: REST for the
//! desktop ([`crate::rest`]), the gRPC runtime plane for the worker, an
//! in-process store for tests ([`crate::memory`]).

use std::collections::BTreeMap;

use async_trait::async_trait;
use serde::{Deserialize, Serialize};

use crate::error::StoreError;

/// `POST workspace_store_upload` request.
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
pub struct UploadRequest {
    pub epoch: u64,
    pub sha256: String,
    pub bytes: u64,
}

/// Where one PUT goes.
#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
pub struct PutTarget {
    pub url: String,
    #[serde(default)]
    pub headers: BTreeMap<String, String>,
}

/// A multipart upload (segments over 64 MiB).
#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
pub struct Multipart {
    pub upload_id: String,
    pub part_size: u64,
    pub part_urls: Vec<String>,
    pub complete_url: String,
}

/// The upload grant.
#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
pub struct UploadGrant {
    /// The digest is already stored for this thread: nothing to upload.
    #[serde(default)]
    pub exists: bool,
    #[serde(default)]
    pub put: Option<PutTarget>,
    #[serde(default)]
    pub multipart: Option<Multipart>,
    #[serde(default)]
    pub expires_at: Option<String>,
}

/// One ref move of a publish (`old: None` for a new ref).
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
pub struct RefUpdate {
    pub name: String,
    pub old: Option<String>,
    pub new: String,
}

/// `POST workspace_store_publish` request.
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
pub struct PublishRequest {
    pub epoch: u64,
    pub kind: String,
    pub segment_sha256: String,
    pub bytes: u64,
    pub prerequisites: Vec<String>,
    pub refs: Vec<RefUpdate>,
    pub object_format: String,
}

/// The publish result.
#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
pub struct Published {
    pub seq: u64,
    pub refs: BTreeMap<String, String>,
    #[serde(default)]
    pub superseded: Vec<serde_json::Value>,
}

/// One segment of a fetch plan.
#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
pub struct PlanSegment {
    pub seq: u64,
    pub kind: String,
    pub sha256: String,
    pub bytes: u64,
    #[serde(default)]
    pub prerequisites: Vec<String>,
    #[serde(default)]
    pub refs: BTreeMap<String, String>,
    pub url: String,
    #[serde(default)]
    pub expires_at: Option<String>,
}

/// `GET workspace_store_fetch` answer.
#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
pub struct FetchPlan {
    pub head_seq: u64,
    #[serde(default)]
    pub refs: BTreeMap<String, String>,
    #[serde(default)]
    pub segments: Vec<PlanSegment>,
}

/// One uploaded part of a multipart upload.
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
pub struct CompletedPart {
    pub part_number: u32,
    pub etag: String,
}

/// The operations on one thread's store.
#[async_trait]
pub trait StoreTransport: Send + Sync {
    /// # Errors
    ///
    /// The server refused it or did not answer.
    async fn upload_grant(&self, request: &UploadRequest) -> Result<UploadGrant, StoreError>;

    /// PUT the whole segment.
    ///
    /// # Errors
    ///
    /// As [`Self::upload_grant`].
    async fn put(&self, target: &PutTarget, bytes: &[u8]) -> Result<(), StoreError>;

    /// PUT one part; its `ETag`.
    ///
    /// # Errors
    ///
    /// As [`Self::upload_grant`].
    async fn put_part(&self, url: &str, bytes: &[u8]) -> Result<String, StoreError>;

    /// # Errors
    ///
    /// As [`Self::upload_grant`].
    async fn complete_multipart(
        &self,
        multipart: &Multipart,
        parts: &[CompletedPart],
    ) -> Result<(), StoreError>;

    /// # Errors
    ///
    /// As [`Self::upload_grant`].
    async fn publish(&self, request: &PublishRequest) -> Result<Published, StoreError>;

    /// # Errors
    ///
    /// As [`Self::upload_grant`].
    async fn fetch_plan(&self, since_seq: u64) -> Result<FetchPlan, StoreError>;

    /// GET a segment by the plan's URL.
    ///
    /// # Errors
    ///
    /// As [`Self::upload_grant`].
    async fn get(&self, url: &str) -> Result<Vec<u8>, StoreError>;
}
