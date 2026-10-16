//! The in-memory Office host the family tests run their tools against: a
//! bucket in a map and a scripted model, plus the production limits with room
//! for parallel test threads.

use std::collections::{BTreeMap, VecDeque};
use std::sync::{Arc, Mutex};

use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use elitea_office_core::Limits;
use serde_json::{Map, Value, json};

use super::families::office_host::{
    OfficeFailure, OfficeHostError, OfficeModelRequest, OfficeObjectWritten, OfficeToolHost,
};
use super::policy::ToolAdmissionPolicy;
use crate::host::{HostError, HostErrorCode, PlatformWriter};
use crate::platform::{
    ArtifactDeleteOutcome, ArtifactDeleteRequest, ArtifactListOutcome, ArtifactListRequest,
    ArtifactReadOutcome, ArtifactReadRequest, ArtifactWriteOutcome, ArtifactWriteRequest,
    ProjectContextWriteOutcome, ProjectContextWriteRequest, SkillWriteOutcome, SkillWriteRequest,
};

/// A real, tiny, valid 1x1 PNG.
pub(super) const TINY_PNG: &[u8] = &[
    0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
    0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
    0x89, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9C, 0x63, 0xF8, 0xFF, 0xFF, 0x3F,
    0x00, 0x05, 0xFE, 0x02, 0xFE, 0xA7, 0x35, 0x81, 0x84, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4E,
    0x44, 0xAE, 0x42, 0x60, 0x82,
];

/// An in-memory bucket and a scripted model.
#[derive(Default)]
pub(super) struct FakeHost {
    pub(super) objects: Mutex<BTreeMap<(String, String), Vec<u8>>>,
    pub(super) replies: Mutex<VecDeque<Result<String, OfficeHostError>>>,
    pub(super) requests: Mutex<Vec<OfficeModelRequest>>,
    /// Every write: the name and whether it asked to overwrite.
    pub(super) writes: Mutex<Vec<(String, bool)>>,
    pub(super) unreachable: bool,
}

impl FakeHost {
    pub(super) fn put(&self, name: &str, bytes: Vec<u8>) {
        self.objects
            .lock()
            .expect("objects")
            .insert(("bucket".to_owned(), name.to_owned()), bytes);
    }

    pub(super) fn get(&self, name: &str) -> Vec<u8> {
        self.objects
            .lock()
            .expect("objects")
            .get(&("bucket".to_owned(), name.to_owned()))
            .cloned()
            .unwrap_or_else(|| panic!("no object {name}"))
    }

    pub(super) fn has(&self, name: &str) -> bool {
        self.objects
            .lock()
            .expect("objects")
            .contains_key(&("bucket".to_owned(), name.to_owned()))
    }

    pub(super) fn reply(&self, text: &str) {
        self.replies
            .lock()
            .expect("replies")
            .push_back(Ok(text.to_owned()));
    }

    pub(super) fn fail_model(&self, kind: OfficeFailure) {
        self.replies
            .lock()
            .expect("replies")
            .push_back(Err(OfficeHostError::new(kind, "scripted")));
    }

    pub(super) fn model_calls(&self) -> Vec<OfficeModelRequest> {
        self.requests.lock().expect("requests").clone()
    }
}

#[async_trait]
impl OfficeToolHost for FakeHost {
    async fn read_object(
        &self,
        bucket: &str,
        name: &str,
        max_bytes: u64,
    ) -> Result<Vec<u8>, OfficeHostError> {
        if self.unreachable {
            return Err(OfficeHostError::new(OfficeFailure::Unreachable, "down"));
        }
        let objects = self.objects.lock().expect("objects");
        let bytes = objects
            .get(&(bucket.to_owned(), name.to_owned()))
            .ok_or_else(|| OfficeHostError::new(OfficeFailure::NotFound, "missing"))?;
        if bytes.len() as u64 > max_bytes {
            return Err(OfficeHostError::new(OfficeFailure::TooLarge, "big"));
        }
        Ok(bytes.clone())
    }

    async fn write_object(
        &self,
        bucket: &str,
        name: &str,
        media_type: &str,
        bytes: Vec<u8>,
        overwrite: bool,
    ) -> Result<OfficeObjectWritten, OfficeHostError> {
        let byte_length = bytes.len() as u64;
        let mut objects = self.objects.lock().expect("objects");
        let slot = (bucket.to_owned(), name.to_owned());
        self.writes
            .lock()
            .expect("writes")
            .push((name.to_owned(), overwrite));
        if !overwrite && objects.contains_key(&slot) {
            return Err(OfficeHostError::new(OfficeFailure::AlreadyExists, "taken"));
        }
        objects.insert(slot, bytes);
        Ok(OfficeObjectWritten {
            bucket: bucket.to_owned(),
            name: name.to_owned(),
            media_type: media_type.to_owned(),
            byte_length,
        })
    }

    async fn complete(&self, request: OfficeModelRequest) -> Result<String, OfficeHostError> {
        self.requests.lock().expect("requests").push(request);
        self.replies
            .lock()
            .expect("replies")
            .pop_front()
            .unwrap_or_else(|| Err(OfficeHostError::new(OfficeFailure::Unreachable, "no reply")))
    }
}

/// The production limits with room for the test threads: the engine's
/// permit gate is process-wide and the tests run in parallel.
pub(super) fn test_limits() -> Limits {
    Limits {
        max_concurrent: 64,
        ..Limits::default()
    }
}

pub(super) fn policy() -> Arc<ToolAdmissionPolicy> {
    Arc::new(ToolAdmissionPolicy::new(&[], &BTreeMap::new()).expect("policy"))
}

pub(super) fn settings(selected: Option<Value>) -> Map<String, Value> {
    let mut s = json!({"bucket_name": "bucket"});
    if let Some(selected) = selected {
        s["selected_tools"] = selected;
    }
    s.as_object().expect("settings").clone()
}

pub(super) fn context() -> Arc<SimpleToolContext> {
    Arc::new(SimpleToolContext::new("pptx-test"))
}

/// A host whose every call fails the same way, for the sentence of each cause.
pub(super) struct FailHost(pub(super) OfficeFailure);

#[async_trait]
impl OfficeToolHost for FailHost {
    async fn read_object(
        &self,
        _bucket: &str,
        _name: &str,
        _max_bytes: u64,
    ) -> Result<Vec<u8>, OfficeHostError> {
        Err(OfficeHostError::new(self.0, "scripted"))
    }

    async fn write_object(
        &self,
        _bucket: &str,
        _name: &str,
        _media_type: &str,
        _bytes: Vec<u8>,
        _overwrite: bool,
    ) -> Result<OfficeObjectWritten, OfficeHostError> {
        Err(OfficeHostError::new(self.0, "scripted"))
    }

    async fn complete(&self, _request: OfficeModelRequest) -> Result<String, OfficeHostError> {
        Err(OfficeHostError::new(self.0, "scripted"))
    }
}

/// A platform writer that has nothing: the authority a materialization test
/// lends an Office host on.
pub(super) struct NoPlatform;

#[async_trait]
impl PlatformWriter for NoPlatform {
    async fn list_artifacts(
        &self,
        _: &ArtifactListRequest,
    ) -> Result<ArtifactListOutcome, HostError> {
        Err(HostError::new(HostErrorCode::NotFound, "none"))
    }
    async fn read_artifact(
        &self,
        _: &ArtifactReadRequest,
    ) -> Result<ArtifactReadOutcome, HostError> {
        Err(HostError::new(HostErrorCode::NotFound, "none"))
    }
    async fn write_artifact(
        &self,
        _: &ArtifactWriteRequest,
    ) -> Result<ArtifactWriteOutcome, HostError> {
        Err(HostError::new(HostErrorCode::NotFound, "none"))
    }
    async fn delete_artifact(
        &self,
        _: &ArtifactDeleteRequest,
    ) -> Result<ArtifactDeleteOutcome, HostError> {
        Err(HostError::new(HostErrorCode::NotFound, "none"))
    }
    async fn write_skill(&self, _: &SkillWriteRequest) -> Result<SkillWriteOutcome, HostError> {
        Err(HostError::new(HostErrorCode::NotFound, "none"))
    }
    async fn write_project_context(
        &self,
        _: &ProjectContextWriteRequest,
    ) -> Result<ProjectContextWriteOutcome, HostError> {
        Err(HostError::new(HostErrorCode::NotFound, "none"))
    }
}
