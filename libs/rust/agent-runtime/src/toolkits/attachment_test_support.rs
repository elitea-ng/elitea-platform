//! The in-memory platform the tracker families' attachment tests run against:
//! a bucket in a map, a scripted chat model and per-step failure injection.
//! Unlike `office_test_support` it needs no Office engine, so it builds
//! without `toolkit-office`.

use std::collections::{BTreeMap, VecDeque};
use std::sync::{Arc, Mutex};

use async_trait::async_trait;

use super::families::attachment_host::AttachmentHost;
use super::families::office_host::{
    OfficeBucketCreated, OfficeFailure, OfficeHostError, OfficeModelRequest, OfficeObjectWritten,
    OfficeToolHost,
};

/// A real, tiny, valid 1x1 PNG.
pub(super) const PNG: &[u8] = &[
    0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
    0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
    0x89, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9C, 0x63, 0xF8, 0xFF, 0xFF, 0x3F,
    0x00, 0x05, 0xFE, 0x02, 0xFE, 0xA7, 0x35, 0x81, 0x84, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4E,
    0x44, 0xAE, 0x42, 0x60, 0x82,
];

/// A bucket named `bucket`, a scripted model and failure injection.
#[derive(Default)]
pub(super) struct FakePlatform {
    pub(super) objects: Mutex<BTreeMap<(String, String), Vec<u8>>>,
    pub(super) replies: Mutex<VecDeque<Result<String, OfficeHostError>>>,
    pub(super) requests: Mutex<Vec<OfficeModelRequest>>,
    /// Every write: bucket, key, media type and whether it asked to overwrite.
    pub(super) writes: Mutex<Vec<(String, String, String, bool)>>,
    pub(super) read_failure: Mutex<Option<OfficeFailure>>,
    pub(super) write_failure: Mutex<Option<OfficeFailure>>,
    /// Reads attempted (the size probe is not a read).
    pub(super) reads: Mutex<u32>,
    /// `Some`: only these buckets exist (a write elsewhere is `NotFound` until
    /// `create_bucket`); `None`: every bucket exists.
    pub(super) buckets: Mutex<Option<std::collections::BTreeSet<String>>>,
    /// Buckets created through the host, in order.
    pub(super) created: Mutex<Vec<String>>,
    pub(super) create_failure: Mutex<Option<OfficeFailure>>,
}

impl FakePlatform {
    pub(super) fn new() -> Arc<Self> {
        Arc::new(Self::default())
    }

    pub(super) fn host(self: &Arc<Self>) -> AttachmentHost {
        AttachmentHost::for_tests(Arc::clone(self) as Arc<dyn OfficeToolHost>)
    }

    pub(super) fn put(&self, bucket: &str, key: &str, bytes: &[u8]) {
        self.objects
            .lock()
            .expect("objects")
            .insert((bucket.to_owned(), key.to_owned()), bytes.to_vec());
    }

    pub(super) fn get(&self, bucket: &str, key: &str) -> Option<Vec<u8>> {
        self.objects
            .lock()
            .expect("objects")
            .get(&(bucket.to_owned(), key.to_owned()))
            .cloned()
    }

    /// A fresh project: no bucket exists until one is created.
    pub(super) fn no_buckets(&self) {
        *self.buckets.lock().expect("buckets") = Some(std::collections::BTreeSet::new());
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

    pub(super) fn fail_reads(&self, kind: OfficeFailure) {
        *self.read_failure.lock().expect("read failure") = Some(kind);
    }

    pub(super) fn fail_create(&self, kind: OfficeFailure) {
        *self.create_failure.lock().expect("create failure") = Some(kind);
    }

    pub(super) fn fail_writes(&self, kind: OfficeFailure) {
        *self.write_failure.lock().expect("write failure") = Some(kind);
    }

    pub(super) fn model_calls(&self) -> Vec<OfficeModelRequest> {
        self.requests.lock().expect("requests").clone()
    }

    pub(super) fn read_count(&self) -> u32 {
        *self.reads.lock().expect("reads")
    }
}

#[async_trait]
impl OfficeToolHost for FakePlatform {
    async fn read_object(
        &self,
        bucket: &str,
        name: &str,
        max_bytes: u64,
    ) -> Result<Vec<u8>, OfficeHostError> {
        *self.reads.lock().expect("reads") += 1;
        if let Some(kind) = *self.read_failure.lock().expect("read failure") {
            return Err(OfficeHostError::new(kind, "scripted"));
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
        self.writes.lock().expect("writes").push((
            bucket.to_owned(),
            name.to_owned(),
            media_type.to_owned(),
            overwrite,
        ));
        if let Some(kind) = *self.write_failure.lock().expect("write failure") {
            return Err(OfficeHostError::new(kind, "scripted"));
        }
        if let Some(buckets) = self.buckets.lock().expect("buckets").as_ref()
            && !buckets.contains(bucket)
        {
            return Err(OfficeHostError::new(OfficeFailure::NotFound, "no bucket"));
        }
        let byte_length = bytes.len() as u64;
        let mut objects = self.objects.lock().expect("objects");
        let slot = (bucket.to_owned(), name.to_owned());
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

    async fn create_bucket(
        &self,
        name: &str,
        _retention_days: Option<u32>,
    ) -> Result<OfficeBucketCreated, OfficeHostError> {
        if let Some(kind) = *self.create_failure.lock().expect("create failure") {
            return Err(OfficeHostError::new(kind, "scripted"));
        }
        let mut buckets = self.buckets.lock().expect("buckets");
        if let Some(set) = buckets.as_mut()
            && !set.insert(name.to_owned())
        {
            return Err(OfficeHostError::new(OfficeFailure::AlreadyExists, "taken"));
        }
        self.created.lock().expect("created").push(name.to_owned());
        Ok(OfficeBucketCreated {
            name: name.to_owned(),
            retention_days: None,
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

    async fn object_size(&self, bucket: &str, name: &str) -> Result<Option<u64>, OfficeHostError> {
        Ok(self
            .objects
            .lock()
            .expect("objects")
            .get(&(bucket.to_owned(), name.to_owned()))
            .map(|bytes| bytes.len() as u64))
    }
}
