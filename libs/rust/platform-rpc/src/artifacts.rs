//! The client of `elitea.runtime.v1.ArtifactObjectService`.
//!
//! Every call names its project ([`Caller::project_id`]) and is made under a
//! bearer; elitea-main runs it through the artifact routes' own
//! authentication, project permission, per-bucket access list and
//! system-bucket rules, so a bearer of one project cannot reach another's
//! bucket. A call made for an execution claim carries it
//! ([`Caller::with_claim`]): the system-bucket rules let a live claim reach its
//! own conversation's attachment bucket.
//!
//! Object bytes travel in chunks of at most [`CHUNK_BYTES`] either way; a
//! read is bounded by the caller's cap while it is received, never after.

use bytes::Bytes;
use tonic::transport::Channel;

use crate::pb::artifact_object_service_client::ArtifactObjectServiceClient;
use crate::{Bearer, MAX_MESSAGE_BYTES, pb, trailer};

/// The trailer that carries a refusal (`ArtifactRefusalV1`).
pub const REFUSAL_TRAILER: &str = "elitea-artifact-refusal-bin";

/// The largest chunk of object bytes one message carries.
pub const CHUNK_BYTES: usize = 1024 * 1024;

/// A refusal the artifact routes decided: the HTTP status they answered and
/// their error body's code and message.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Refusal {
    pub http_status: u16,
    pub code: String,
    pub message: String,
}

/// Why a call failed.
#[derive(Debug)]
pub enum ArtifactError {
    /// elitea-main refused the call ([`Refusal`]).
    Refused(Refusal),
    /// The hop failed: the peer was not admitted, the listener is
    /// unreachable, the call was cancelled or broke in transit. Never a
    /// refusal.
    Hop(tonic::Status),
    /// The object is larger than the caller's cap (the cap).
    TooLarge(u64),
    /// The service broke its own protocol (a stream without its info frame).
    Protocol(&'static str),
    /// The call cannot be expressed (a name that is not one segment).
    Invalid(&'static str),
}

impl ArtifactError {
    /// The HTTP status of a refusal.
    #[must_use]
    pub fn http_status(&self) -> Option<u16> {
        match self {
            Self::Refused(refusal) => Some(refusal.http_status),
            _ => None,
        }
    }

    fn from_status(status: tonic::Status) -> Self {
        match refusal(&status) {
            Some(refusal) => Self::Refused(refusal),
            None => Self::Hop(status),
        }
    }
}

impl std::fmt::Display for ArtifactError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Refused(refusal) => write!(
                f,
                "the artifact call was refused (HTTP {}): {}",
                refusal.http_status, refusal.message
            ),
            Self::Hop(status) => write!(
                f,
                "the artifact service call failed: {:?}: {}",
                status.code(),
                status.message()
            ),
            Self::TooLarge(cap) => write!(f, "the artifact object is larger than {cap} bytes"),
            Self::Protocol(reason) | Self::Invalid(reason) => f.write_str(reason),
        }
    }
}

impl std::error::Error for ArtifactError {}

/// The refusal a status carries.
#[must_use]
pub fn refusal(status: &tonic::Status) -> Option<Refusal> {
    let refusal: pb::ArtifactRefusalV1 = trailer(status, REFUSAL_TRAILER)?;
    let http_status = u16::try_from(refusal.http_status)
        .ok()
        .filter(|code| !(200..=299).contains(code) && *code >= 100)?;
    Some(Refusal {
        http_status,
        code: refusal.code,
        message: refusal.message,
    })
}

/// The execution claim a worker call is made under.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct RuntimeClaim {
    pub execution_id: String,
    pub generation: u64,
    pub claim_id: String,
    pub fence: String,
}

/// Who a call is made by and for: the bearer, the project and, for a
/// worker call made under a claim, the claim.
#[derive(Clone, Debug)]
pub struct Caller {
    bearer: Bearer,
    project_id: i64,
    claim: Option<pb::ArtifactRuntimeClaimV1>,
}

impl Caller {
    /// `bearer` acting in `project_id`.
    #[must_use]
    pub fn new(bearer: Bearer, project_id: i64) -> Self {
        Self {
            bearer,
            project_id,
            claim: None,
        }
    }

    /// `token` acting in the project `project_id` spells, or `None` when
    /// either is unusable.
    #[must_use]
    pub fn parse(token: &str, project_id: &str) -> Option<Self> {
        let project_id = project_id.trim().parse::<i64>().ok().filter(|id| *id > 0)?;
        Some(Self::new(Bearer::new(token)?, project_id))
    }

    /// This caller, naming `claim` on every request.
    #[must_use]
    pub fn with_claim(mut self, claim: RuntimeClaim) -> Self {
        self.claim = Some(pb::ArtifactRuntimeClaimV1 {
            execution_id: claim.execution_id,
            generation: claim.generation,
            claim_id: claim.claim_id,
            fence: claim.fence,
        });
        self
    }

    #[must_use]
    pub fn project_id(&self) -> i64 {
        self.project_id
    }

    fn object(&self, bucket: &str, key: &str) -> pb::ArtifactObjectRefV1 {
        pb::ArtifactObjectRefV1 {
            project_id: self.project_id,
            bucket: bucket.to_owned(),
            key: key.to_owned(),
            claim: self.claim.clone(),
        }
    }
}

/// One listing request.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct ListObjects {
    pub bucket: String,
    pub prefix: String,
    pub delimiter: String,
    /// The route's `limit`; 0 is the server's default page.
    pub page_size: i32,
    /// Resume a listing at a page's `next_cursor`.
    pub cursor: String,
    /// The pages the server streams before it stops; 0 is one page.
    pub max_pages: i32,
}

/// One object write.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct PutObject {
    pub bucket: String,
    pub key: String,
    /// Empty derives the type from the key's extension.
    pub media_type: String,
    /// Replace an existing object; false refuses it (409).
    pub overwrite: bool,
}

/// The pages of one listing, as the server streams them.
pub struct Pages {
    stream: tonic::Streaming<pb::ArtifactListObjectsResponseV1>,
}

impl Pages {
    /// The next page, `None` once the listing ended (or `max_pages` were
    /// sent: the last page's `next_cursor` then says where to resume).
    ///
    /// # Errors
    ///
    /// A refusal (the first page's: the bucket is unknown or not readable)
    /// or the stream breaking.
    pub async fn next(
        &mut self,
    ) -> Result<Option<pb::ArtifactListObjectsResponseV1>, ArtifactError> {
        self.stream
            .message()
            .await
            .map_err(ArtifactError::from_status)
    }
}

/// One object being read: its info, then its bytes.
pub struct ObjectReader {
    pub info: pb::ArtifactObjectInfoV1,
    stream: tonic::Streaming<pb::ArtifactGetObjectResponseV1>,
}

impl ObjectReader {
    /// The next chunk, `None` at the end of the object.
    ///
    /// # Errors
    ///
    /// The stream breaking, or a second info frame.
    pub async fn chunk(&mut self) -> Result<Option<Bytes>, ArtifactError> {
        use pb::artifact_get_object_response_v1::Frame;
        loop {
            match self
                .stream
                .message()
                .await
                .map_err(ArtifactError::from_status)?
            {
                None => return Ok(None),
                Some(message) => match message.frame {
                    Some(Frame::Data(data)) if !data.is_empty() => return Ok(Some(data)),
                    Some(Frame::Data(_)) | None => {}
                    Some(Frame::Info(_)) => {
                        return Err(ArtifactError::Protocol(
                            "the object stream sent a second info frame",
                        ));
                    }
                },
            }
        }
    }

    /// The whole object, refused as soon as it passes `max_bytes` (and
    /// before any byte when its declared size already does).
    ///
    /// # Errors
    ///
    /// [`ArtifactError::TooLarge`], or the stream breaking.
    pub async fn read_to_end(mut self, max_bytes: u64) -> Result<Vec<u8>, ArtifactError> {
        if u64::try_from(self.info.size_bytes).unwrap_or(0) > max_bytes {
            return Err(ArtifactError::TooLarge(max_bytes));
        }
        let mut bytes = Vec::with_capacity(
            usize::try_from(self.info.size_bytes.clamp(0, 64 * 1024 * 1024)).unwrap_or(0),
        );
        while let Some(chunk) = self.chunk().await? {
            if (bytes.len() + chunk.len()) as u64 > max_bytes {
                return Err(ArtifactError::TooLarge(max_bytes));
            }
            bytes.extend_from_slice(&chunk);
        }
        Ok(bytes)
    }
}

/// The service client over a caller-built channel. Cheap to clone.
#[derive(Clone, Debug)]
pub struct ArtifactClient {
    inner: ArtifactObjectServiceClient<Channel>,
}

impl ArtifactClient {
    /// The client over `channel` (the platform listener's).
    #[must_use]
    pub fn new(channel: Channel) -> Self {
        Self {
            inner: ArtifactObjectServiceClient::new(channel)
                .max_decoding_message_size(MAX_MESSAGE_BYTES)
                .max_encoding_message_size(MAX_MESSAGE_BYTES),
        }
    }

    /// The project's buckets.
    ///
    /// # Errors
    ///
    /// A refusal or the hop failing.
    pub async fn list_buckets(
        &self,
        caller: &Caller,
    ) -> Result<Vec<pb::ArtifactBucketV1>, ArtifactError> {
        let request = caller.bearer.request(pb::ArtifactListBucketsRequestV1 {
            project_id: caller.project_id,
        });
        self.inner
            .clone()
            .list_buckets(request)
            .await
            .map(|answer| answer.into_inner().buckets)
            .map_err(ArtifactError::from_status)
    }

    /// Create the bucket `name`.
    ///
    /// # Errors
    ///
    /// A refusal (409 taken, 400 an invalid name, 403 the policy ceiling)
    /// or the hop failing.
    pub async fn create_bucket(
        &self,
        caller: &Caller,
        name: &str,
        retention_days: Option<i32>,
    ) -> Result<pb::ArtifactBucketV1, ArtifactError> {
        let request = caller.bearer.request(pb::ArtifactCreateBucketRequestV1 {
            project_id: caller.project_id,
            name: name.to_owned(),
            retention_days,
        });
        let answer = self
            .inner
            .clone()
            .create_bucket(request)
            .await
            .map_err(ArtifactError::from_status)?
            .into_inner();
        answer
            .bucket
            .ok_or(ArtifactError::Protocol("the created bucket was not echoed"))
    }

    /// The pages of a listing.
    ///
    /// # Errors
    ///
    /// A refusal or the hop failing before the first page.
    pub async fn list_objects(
        &self,
        caller: &Caller,
        query: ListObjects,
    ) -> Result<Pages, ArtifactError> {
        let request = caller.bearer.request(pb::ArtifactListObjectsRequestV1 {
            project_id: caller.project_id,
            bucket: query.bucket,
            prefix: query.prefix,
            delimiter: query.delimiter,
            page_size: query.page_size,
            cursor: query.cursor,
            max_pages: query.max_pages,
            claim: caller.claim.clone(),
        });
        let stream = self
            .inner
            .clone()
            .list_objects(request)
            .await
            .map_err(ArtifactError::from_status)?
            .into_inner();
        Ok(Pages { stream })
    }

    /// One object's info.
    ///
    /// # Errors
    ///
    /// A refusal (404 when absent) or the hop failing.
    pub async fn stat(
        &self,
        caller: &Caller,
        bucket: &str,
        key: &str,
    ) -> Result<pb::ArtifactObjectInfoV1, ArtifactError> {
        let request = caller.bearer.request(pb::ArtifactStatRequestV1 {
            object: Some(caller.object(bucket, key)),
        });
        let answer = self
            .inner
            .clone()
            .stat(request)
            .await
            .map_err(ArtifactError::from_status)?
            .into_inner();
        answer
            .info
            .ok_or(ArtifactError::Protocol("the stat answer has no info"))
    }

    /// Open one object: its info is read here, so a refusal surfaces before
    /// any byte.
    ///
    /// # Errors
    ///
    /// A refusal or the hop failing.
    pub async fn get_object(
        &self,
        caller: &Caller,
        bucket: &str,
        key: &str,
    ) -> Result<ObjectReader, ArtifactError> {
        use pb::artifact_get_object_response_v1::Frame;
        let request = caller.bearer.request(pb::ArtifactGetObjectRequestV1 {
            object: Some(caller.object(bucket, key)),
        });
        let mut stream = self
            .inner
            .clone()
            .get_object(request)
            .await
            .map_err(ArtifactError::from_status)?
            .into_inner();
        match stream.message().await.map_err(ArtifactError::from_status)? {
            Some(pb::ArtifactGetObjectResponseV1 {
                frame: Some(Frame::Info(info)),
            }) => Ok(ObjectReader { info, stream }),
            _ => Err(ArtifactError::Protocol(
                "the object stream did not start with its info",
            )),
        }
    }

    /// Write one object: the head, then `body` in chunks.
    ///
    /// # Errors
    ///
    /// A refusal (409 taken without `overwrite`, 413 over the project's
    /// cap) or the hop failing.
    pub async fn put_object(
        &self,
        caller: &Caller,
        object: PutObject,
        body: Bytes,
    ) -> Result<pb::ArtifactObjectInfoV1, ArtifactError> {
        use pb::artifact_put_object_request_v1::Frame;
        let head = pb::ArtifactPutObjectRequestV1 {
            frame: Some(Frame::Head(pb::ArtifactPutObjectHeadV1 {
                project_id: caller.project_id,
                bucket: object.bucket,
                key: object.key,
                media_type: object.media_type,
                overwrite: object.overwrite,
                claim: caller.claim.clone(),
            })),
        };
        let chunks = (0..body.len())
            .step_by(CHUNK_BYTES)
            .map(move |start| pb::ArtifactPutObjectRequestV1 {
                frame: Some(Frame::Data(
                    body.slice(start..(start + CHUNK_BYTES).min(body.len())),
                )),
            })
            .collect::<Vec<_>>();
        let frames = futures_util::stream::iter(std::iter::once(head).chain(chunks));
        let answer = self
            .inner
            .clone()
            .put_object(caller.bearer.request(frames))
            .await
            .map_err(ArtifactError::from_status)?
            .into_inner();
        answer
            .info
            .ok_or(ArtifactError::Protocol("the written object was not echoed"))
    }

    /// Delete one object.
    ///
    /// # Errors
    ///
    /// A refusal (404 when absent) or the hop failing.
    pub async fn delete(
        &self,
        caller: &Caller,
        bucket: &str,
        key: &str,
    ) -> Result<(), ArtifactError> {
        let request = caller.bearer.request(pb::ArtifactDeleteRequestV1 {
            object: Some(caller.object(bucket, key)),
        });
        self.inner
            .clone()
            .delete(request)
            .await
            .map(|_| ())
            .map_err(ArtifactError::from_status)
    }

    /// Delete several objects in one request; each key's outcome is
    /// reported.
    ///
    /// # Errors
    ///
    /// A refusal of the whole request or the hop failing.
    pub async fn delete_batch(
        &self,
        caller: &Caller,
        bucket: &str,
        keys: Vec<String>,
    ) -> Result<pb::ArtifactDeleteBatchResponseV1, ArtifactError> {
        let request = caller.bearer.request(pb::ArtifactDeleteBatchRequestV1 {
            project_id: caller.project_id,
            bucket: bucket.to_owned(),
            keys,
            claim: caller.claim.clone(),
        });
        self.inner
            .clone()
            .delete_batch(request)
            .await
            .map(tonic::Response::into_inner)
            .map_err(ArtifactError::from_status)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use prost::Message as _;
    use tonic::metadata::{MetadataMap, MetadataValue};

    #[test]
    fn a_refusal_trailer_is_read_and_a_bare_status_is_the_hop() {
        let refusal = pb::ArtifactRefusalV1 {
            http_status: 404,
            code: "NotFound".to_owned(),
            message: "no such key".to_owned(),
        };
        let mut metadata = MetadataMap::new();
        metadata.insert_bin(
            REFUSAL_TRAILER,
            MetadataValue::from_bytes(&refusal.encode_to_vec()),
        );
        let status = tonic::Status::with_metadata(tonic::Code::NotFound, "refused", metadata);
        let error = ArtifactError::from_status(status);
        assert_eq!(error.http_status(), Some(404));
        let ArtifactError::Refused(read) = error else {
            panic!("not a refusal");
        };
        assert_eq!(read.code, "NotFound");
        assert!(matches!(
            ArtifactError::from_status(tonic::Status::unavailable("down")),
            ArtifactError::Hop(_)
        ));
    }

    #[test]
    fn a_caller_needs_a_bearer_and_a_project() {
        assert!(Caller::parse("t", "7").is_some());
        assert!(Caller::parse("", "7").is_none());
        assert!(Caller::parse("t", "seven").is_none());
        assert!(Caller::parse("t", "0").is_none());
    }
}
