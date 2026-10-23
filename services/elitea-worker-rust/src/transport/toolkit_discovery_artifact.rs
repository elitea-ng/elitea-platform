//! Immutable toolkit discovery results on the claim-bound content channel.

use super::*;
use prost::Message as _;

use crate::protocol::elitea::runtime::v1::{
    DigestAlgorithmV1, PutToolkitDiscoveryResultRequestV1, ToolkitAvailableToolsArtifactReferenceV1,
};

const RESULT_MEDIA_TYPE: &str = "application/vnd.elitea.toolkit-available-tools.v1+json";
const MAX_RESULT_BYTES: usize = 1024 * 1024;
const MAX_REFERENCE_BYTES: usize = 4096;

impl InputContentClient {
    /// Publish one immutable result under the live claim and admitted input.
    ///
    /// The server accepts exact retries and refuses conflicting result bytes.
    /// The caller keeps its lease monitor active and cancels this future when
    /// ownership ends. A lost response does not prove that storage failed.
    ///
    /// # Errors
    /// Returns a bounded transport, authority, integrity, or resource error.
    pub async fn publish_toolkit_discovery(
        &self,
        execution: &LeaseMonitoredAgentExecution,
        content: &[u8],
    ) -> Result<ToolkitAvailableToolsArtifactReferenceV1, InputContentError> {
        let authority =
            execution
                .input_content_authority()
                .ok_or(InputContentError::InvalidInput(
                    "the sealed input authority is malformed",
                ))?;
        self.publish_discovery_authority(authority, content).await
    }

    async fn publish_discovery_authority(
        &self,
        authority: ClaimBoundInputAuthority<'_>,
        content: &[u8],
    ) -> Result<ToolkitAvailableToolsArtifactReferenceV1, InputContentError> {
        validate_reference(&authority)?;
        if content.is_empty() || content.len() > MAX_RESULT_BYTES {
            return Err(InputContentError::ResourceExhausted(
                "the toolkit discovery result exceeds its content limit",
            ));
        }
        let expected = digest::digest(&digest::SHA256, content);
        let request = claim_request(
            PutToolkitDiscoveryResultRequestV1 {
                scope: Some(content_scope(authority.execution_id, authority.generation)),
                content_id: authority.content_id.to_owned(),
                immutable_version: authority.immutable_version.to_owned(),
                result_json: content.to_vec(),
            },
            authority.claim_id,
            authority.fence_token,
        )
        .ok_or(InputContentError::InvalidInput(
            "the result artifact request is malformed",
        ))?;
        let mut rpc = self.rpc();
        timeout(self.config.deadline, async {
            let reference = rpc
                .put_toolkit_discovery_result(request)
                .await
                .map_err(discovery_status)?
                .into_inner()
                .reference
                .ok_or(InputContentError::InvalidInput(
                    "the result artifact reference is malformed",
                ))?;
            validate_discovery_reference(reference, content.len(), expected.as_ref())
        })
        .await
        .map_err(|_| InputContentError::Timeout("the result artifact request timed out"))?
    }
}

/// A conflicting or refused result is final for these bytes; Main's other
/// failures may clear on a fresh attempt.
fn discovery_status(status: Status) -> InputContentError {
    match status.code() {
        Code::Unauthenticated | Code::PermissionDenied => {
            InputContentError::AuthorizationFailed("the result artifact claim was rejected")
        }
        Code::ResourceExhausted => {
            InputContentError::ResourceExhausted("the result artifact exceeds its content limit")
        }
        Code::Aborted | Code::InvalidArgument | Code::FailedPrecondition => {
            InputContentError::InvalidInput("the immutable result artifact was rejected")
        }
        Code::DeadlineExceeded => {
            InputContentError::Timeout("the result artifact request timed out")
        }
        _ if is_transport_status(&status) => InputContentError::transport(status),
        _ => InputContentError::DependencyUnavailable("the result artifact service is unavailable"),
    }
}

fn validate_discovery_reference(
    reference: ToolkitAvailableToolsArtifactReferenceV1,
    content_length: usize,
    expected_digest: &[u8],
) -> Result<ToolkitAvailableToolsArtifactReferenceV1, InputContentError> {
    if reference.encoded_len() > MAX_REFERENCE_BYTES {
        return Err(InputContentError::ResourceExhausted(
            "the result artifact reference exceeds its limit",
        ));
    }
    if !bounded_text(&reference.artifact_id)
        || reference.immutable_version != digest_hex(expected_digest)
        || reference.media_type != RESULT_MEDIA_TYPE
        || reference.classification != "tenant-confidential"
        || reference.byte_length != content_length as u64
        || reference.digest.as_ref().is_none_or(|value| {
            value.algorithm != DigestAlgorithmV1::Sha256 as i32
                || value.value.as_slice().ct_eq(expected_digest).unwrap_u8() != 1
        })
    {
        return Err(InputContentError::InvalidInput(
            "the result artifact reference does not bind the uploaded content",
        ));
    }
    Ok(reference)
}

fn digest_hex(value: &[u8]) -> String {
    const DIGITS: &[u8; 16] = b"0123456789abcdef";
    value
        .iter()
        .flat_map(|byte| {
            [
                char::from(DIGITS[usize::from(byte >> 4)]),
                char::from(DIGITS[usize::from(byte & 15)]),
            ]
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::protocol::elitea::runtime::v1::{DigestV1, PutToolkitDiscoveryResultResponseV1};
    use crate::transport::input_content::content_fake::{ContentFake, claim_of};
    use std::sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    };

    struct UploadRpc {
        requests: Arc<AtomicUsize>,
        hang: bool,
    }

    #[tonic::async_trait]
    impl ContentFake for UploadRpc {
        async fn put_toolkit_discovery_result(
            &self,
            request: tonic::Request<PutToolkitDiscoveryResultRequestV1>,
        ) -> Result<tonic::Response<PutToolkitDiscoveryResultResponseV1>, Status> {
            self.requests.fetch_add(1, Ordering::SeqCst);
            assert_eq!(claim_of(&request), ("claim-1".into(), vec![b'f'; 32]));
            let request = request.into_inner();
            assert_eq!(
                request.scope,
                Some(content_scope("execution-one", 2)),
                "the claimed execution"
            );
            // Identifiers travel raw: nothing is percent-encoded any more.
            assert_eq!(request.content_id, "settings id");
            assert_eq!(request.immutable_version, "v/1");
            assert_eq!(request.result_json, b"{\"tools\":[]}");
            if self.hang {
                return std::future::pending().await;
            }
            Ok(tonic::Response::new(PutToolkitDiscoveryResultResponseV1 {
                reference: Some(reference(&request.result_json)),
            }))
        }
    }

    fn authority() -> ClaimBoundInputAuthority<'static> {
        ClaimBoundInputAuthority {
            execution_id: "execution-one",
            generation: 2,
            content_id: "settings id",
            immutable_version: "v/1",
            claim_id: "claim-1",
            fence_token: &[b'f'; 32],
            expected_source_length: 2,
            expected_source_sha256: &[1; 32],
            media_type: "application/json",
        }
    }

    fn client(hang: bool, requests: Arc<AtomicUsize>) -> InputContentClient {
        InputContentClient::with_rpc(
            UploadRpc { requests, hang },
            InputContentConfig {
                origin: "https://content.internal".into(),
                deadline: Duration::from_millis(200),
                max_materialized_bytes: 1024,
            },
        )
        .unwrap()
    }

    #[tokio::test]
    async fn uploads_exact_bytes_with_claim_and_input_binding() {
        let requests = Arc::new(AtomicUsize::new(0));
        let client = client(false, Arc::clone(&requests));
        let first = client
            .publish_discovery_authority(authority(), b"{\"tools\":[]}")
            .await
            .unwrap();
        let retry = client
            .publish_discovery_authority(authority(), b"{\"tools\":[]}")
            .await
            .unwrap();
        assert_eq!(first, retry);
        assert_eq!(requests.load(Ordering::SeqCst), 2);
    }

    #[tokio::test]
    async fn bounds_upload_before_dispatch_and_does_not_retry_timeout() {
        let requests = Arc::new(AtomicUsize::new(0));
        let client = client(true, Arc::clone(&requests));
        assert!(matches!(
            client
                .publish_discovery_authority(authority(), &vec![b' '; MAX_RESULT_BYTES + 1])
                .await,
            Err(InputContentError::ResourceExhausted(_))
        ));
        assert_eq!(requests.load(Ordering::SeqCst), 0);
        assert!(matches!(
            client
                .publish_discovery_authority(authority(), b"{\"tools\":[]}")
                .await,
            Err(InputContentError::Timeout(_))
        ));
        assert_eq!(requests.load(Ordering::SeqCst), 1);
    }

    fn reference(content: &[u8]) -> ToolkitAvailableToolsArtifactReferenceV1 {
        let digest = digest::digest(&digest::SHA256, content);
        ToolkitAvailableToolsArtifactReferenceV1 {
            artifact_id: "artifact".into(),
            immutable_version: digest_hex(digest.as_ref()),
            media_type: RESULT_MEDIA_TYPE.into(),
            classification: "tenant-confidential".into(),
            byte_length: content.len() as u64,
            digest: Some(DigestV1 {
                algorithm: DigestAlgorithmV1::Sha256 as i32,
                value: digest.as_ref().to_vec(),
            }),
        }
    }

    #[test]
    fn accepts_only_reference_bound_to_exact_uploaded_bytes() {
        let content = b"{\"tools\":[]}";
        let digest = digest::digest(&digest::SHA256, content);
        let accepted =
            validate_discovery_reference(reference(content), content.len(), digest.as_ref())
                .unwrap();
        assert_eq!(accepted.byte_length, content.len() as u64);
        assert!(
            validate_discovery_reference(reference(content), content.len() + 1, digest.as_ref())
                .is_err()
        );
        assert!(validate_discovery_reference(reference(content), content.len(), &[1; 32]).is_err());
        let mut oversized = reference(content);
        oversized.artifact_id = "a".repeat(MAX_REFERENCE_BYTES);
        assert!(matches!(
            validate_discovery_reference(oversized, content.len(), digest.as_ref()),
            Err(InputContentError::ResourceExhausted(_))
        ));
    }

    #[test]
    fn refuses_stale_claim_and_conflict_and_retries_only_outages() {
        assert!(matches!(
            discovery_status(Status::permission_denied("fence")),
            InputContentError::AuthorizationFailed(_)
        ));
        assert!(matches!(
            discovery_status(Status::aborted("conflict")),
            InputContentError::InvalidInput(_)
        ));
        assert!(matches!(
            discovery_status(Status::resource_exhausted("large")),
            InputContentError::ResourceExhausted(_)
        ));
        assert!(discovery_status(Status::unavailable("overload")).retryable());
        assert!(discovery_status(Status::internal("verification")).retryable());
    }
}
