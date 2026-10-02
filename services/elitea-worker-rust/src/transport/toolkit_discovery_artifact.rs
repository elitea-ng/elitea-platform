//! Immutable toolkit discovery results on the claim-bound content channel.

use super::*;
use http_body_util::Full;
use prost::Message as _;

use crate::protocol::elitea::runtime::v1::{
    DigestAlgorithmV1, ToolkitAvailableToolsArtifactReferenceV1,
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
        let mut request = build_request(&authority)?;
        let uri = format!("{}/toolkit-discovery-result", request.uri());
        *request.uri_mut() = uri.parse().map_err(|_| {
            InputContentError::InvalidInput("the result artifact request is malformed")
        })?;
        *request.method_mut() = Method::PUT;
        *request.body_mut() = Body::new(Full::new(Bytes::copy_from_slice(content)));
        request
            .headers_mut()
            .insert(CONTENT_TYPE, HeaderValue::from_static(RESULT_MEDIA_TYPE));
        request
            .headers_mut()
            .insert(CONTENT_LENGTH, HeaderValue::from(content.len()));
        request.headers_mut().insert(
            CONTENT_DIGEST_HEADER,
            HeaderValue::from_str(&format!("sha-256=:{}:", STANDARD.encode(expected.as_ref())))
                .map_err(|_| InputContentError::InvalidInput("the result digest is malformed"))?,
        );
        timeout(self.config.deadline, async {
            let response = self
                .rpc
                .get(request)
                .await
                .map_err(InputContentError::Transport)?;
            validate_discovery_response(response, content.len(), expected.as_ref()).await
        })
        .await
        .map_err(|_| InputContentError::Timeout("the result artifact request timed out"))?
    }
}

async fn validate_discovery_response(
    response: Response<Body>,
    content_length: usize,
    expected_digest: &[u8],
) -> Result<ToolkitAvailableToolsArtifactReferenceV1, InputContentError> {
    if response.version() != Version::HTTP_2 {
        return Err(InputContentError::DependencyUnavailable(
            "the result service did not negotiate HTTP/2",
        ));
    }
    match response.status() {
        StatusCode::OK => {}
        StatusCode::UNAUTHORIZED | StatusCode::FORBIDDEN => {
            return Err(InputContentError::AuthorizationFailed(
                "the result artifact claim was rejected",
            ));
        }
        StatusCode::PAYLOAD_TOO_LARGE => {
            return Err(InputContentError::ResourceExhausted(
                "the result artifact exceeds its content limit",
            ));
        }
        StatusCode::CONFLICT | StatusCode::BAD_REQUEST | StatusCode::UNPROCESSABLE_ENTITY => {
            return Err(InputContentError::InvalidInput(
                "the immutable result artifact was rejected",
            ));
        }
        _ => {
            return Err(InputContentError::DependencyUnavailable(
                "the result artifact service is unavailable",
            ));
        }
    }
    if single_header(response.headers(), &CONTENT_TYPE)? != "application/protobuf"
        || single_header(response.headers(), &CACHE_CONTROL)?
            != "no-store, no-cache, must-revalidate"
    {
        return Err(InputContentError::InvalidInput(
            "the result artifact response metadata is malformed",
        ));
    }
    let length = positive_decimal_header(response.headers(), &CONTENT_LENGTH)?;
    if length > MAX_REFERENCE_BYTES as u64 {
        return Err(InputContentError::ResourceExhausted(
            "the result artifact reference exceeds its limit",
        ));
    }
    let response_digest = sha256_header(response.headers(), &CONTENT_DIGEST_HEADER)?;
    let bytes = collect_validated_body(
        response,
        ValidatedResponseHead {
            declared_length: usize::try_from(length).map_err(|_| {
                InputContentError::ResourceExhausted(
                    "the result artifact reference exceeds its limit",
                )
            })?,
            response_digest,
        },
        MAX_REFERENCE_BYTES,
    )
    .await?;
    let reference =
        ToolkitAvailableToolsArtifactReferenceV1::decode(bytes.as_bytes()).map_err(|_| {
            InputContentError::InvalidInput("the result artifact reference is malformed")
        })?;
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
    use crate::protocol::elitea::runtime::v1::DigestV1;
    use std::sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    };

    struct UploadRpc {
        requests: Arc<AtomicUsize>,
        hang: bool,
    }

    #[async_trait]
    impl InputContentRpc for UploadRpc {
        async fn get(
            &self,
            request: Request<Body>,
        ) -> Result<Response<Body>, InputContentTransportError> {
            self.requests.fetch_add(1, Ordering::SeqCst);
            assert_eq!(request.method(), Method::PUT);
            assert_eq!(
                request.uri().path(),
                "/executions/execution%2Fone/generations/2/inputs/settings%20id/versions/v%2F1/toolkit-discovery-result"
            );
            assert_eq!(request.headers()[CLAIM_HEADER], "claim-1");
            assert_eq!(
                request.headers()[FENCE_HEADER],
                URL_SAFE_NO_PAD.encode([b'f'; 32])
            );
            assert_eq!(request.headers()[CONTENT_TYPE], RESULT_MEDIA_TYPE);
            let body = request.into_body().collect().await.unwrap().to_bytes();
            assert_eq!(body.as_ref(), b"{\"tools\":[]}");
            if self.hang {
                return std::future::pending().await;
            }
            Ok(response(&body))
        }
    }

    fn authority() -> ClaimBoundInputAuthority<'static> {
        ClaimBoundInputAuthority {
            execution_id: "execution/one",
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
                deadline: Duration::from_millis(20),
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

    fn response(content: &[u8]) -> Response<Body> {
        let digest = digest::digest(&digest::SHA256, content);
        let reference = ToolkitAvailableToolsArtifactReferenceV1 {
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
        .encode_to_vec();
        Response::builder()
            .version(Version::HTTP_2)
            .status(StatusCode::OK)
            .header(CONTENT_TYPE, "application/protobuf")
            .header(CACHE_CONTROL, "no-store, no-cache, must-revalidate")
            .header(CONTENT_LENGTH, reference.len())
            .header(
                CONTENT_DIGEST_HEADER,
                format!(
                    "sha-256=:{}:",
                    STANDARD.encode(digest::digest(&digest::SHA256, &reference).as_ref())
                ),
            )
            .body(Body::new(Full::new(Bytes::from(reference))))
            .unwrap()
    }

    #[tokio::test]
    async fn accepts_only_reference_bound_to_exact_uploaded_bytes() {
        let content = b"{\"tools\":[]}";
        let digest = digest::digest(&digest::SHA256, content);
        let reference =
            validate_discovery_response(response(content), content.len(), digest.as_ref())
                .await
                .unwrap();
        assert_eq!(reference.byte_length, content.len() as u64);
        assert!(
            validate_discovery_response(response(content), content.len() + 1, digest.as_ref())
                .await
                .is_err()
        );
        assert!(
            validate_discovery_response(response(content), content.len(), &[1; 32])
                .await
                .is_err()
        );
    }

    #[tokio::test]
    async fn refuses_redirect_stale_claim_conflict_and_oversized_reference() {
        for status in [
            StatusCode::TEMPORARY_REDIRECT,
            StatusCode::FORBIDDEN,
            StatusCode::CONFLICT,
        ] {
            let mut value = response(b"{}");
            *value.status_mut() = status;
            assert!(
                validate_discovery_response(value, 2, &[0; 32])
                    .await
                    .is_err()
            );
        }
        let mut value = response(b"{}");
        value
            .headers_mut()
            .insert(CONTENT_LENGTH, HeaderValue::from(MAX_REFERENCE_BYTES + 1));
        assert!(matches!(
            validate_discovery_response(value, 2, &[0; 32]).await,
            Err(InputContentError::ResourceExhausted(_))
        ));
    }
}
