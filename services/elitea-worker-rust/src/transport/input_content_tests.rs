use std::convert::Infallible;
use std::future::pending;
use std::sync::{Arc, LazyLock, Mutex};
use std::time::Duration;

use super::input_content::{
    InputContentClient, InputContentConfig, InputContentError, InputContentRpc,
    InputContentTransportError,
};
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD;
use bytes::Bytes;
use http::{HeaderValue, Request, Response, StatusCode, Version};
use http_body_util::{BodyExt as _, Full};
use ring::digest;
use tonic::body::Body;

use crate::protocol::control::{ClaimBoundInputAuthority, test_lease_monitored_input_execution};

const MEDIA_TYPE: &str = "application/vnd.elitea.agent-execution-input.v1+protobuf";
const TOOLKIT_MEDIA_TYPE: &str = "application/vnd.elitea.toolkit-execute-read-input.v1+protobuf";
const SOURCE: &[u8] = b"source {{secret}}";
static SOURCE_SHA256: LazyLock<[u8; 32]> = LazyLock::new(|| sha256(SOURCE));

struct FakeRpc {
    captured: Arc<Mutex<Vec<CapturedRequest>>>,
    response: Mutex<Option<Result<Response<Body>, InputContentTransportError>>>,
    hangs: bool,
}

#[derive(Debug, Eq, PartialEq)]
struct CapturedRequest {
    method: String,
    path: String,
    claim: String,
    fence: String,
}

#[async_trait]
impl InputContentRpc for FakeRpc {
    async fn get(
        &self,
        request: Request<Body>,
    ) -> Result<Response<Body>, InputContentTransportError> {
        self.captured
            .lock()
            .expect("captured requests")
            .push(CapturedRequest {
                method: request.method().as_str().to_owned(),
                path: request.uri().path().to_owned(),
                claim: request
                    .headers()
                    .get("x-elitea-claim-id")
                    .expect("claim header")
                    .to_str()
                    .expect("claim header text")
                    .to_owned(),
                fence: request
                    .headers()
                    .get("x-elitea-fence")
                    .expect("fence header")
                    .to_str()
                    .expect("fence header text")
                    .to_owned(),
            });
        if self.hangs {
            return pending().await;
        }
        self.response
            .lock()
            .expect("fake response")
            .take()
            .expect("one fake response")
    }
}

fn source() -> Vec<u8> {
    SOURCE.to_vec()
}

fn materialized() -> Vec<u8> {
    b"materialized-in-memory".to_vec()
}

fn sha256(value: &[u8]) -> [u8; 32] {
    digest::digest(&digest::SHA256, value)
        .as_ref()
        .try_into()
        .expect("SHA-256 length")
}

fn reference() -> ClaimBoundInputAuthority<'static> {
    ClaimBoundInputAuthority {
        execution_id: "execution/one",
        generation: 2,
        content_id: "settings id",
        immutable_version: "v/1",
        claim_id: "claim-1",
        fence_token: &[b'f'; 32],
        expected_source_length: SOURCE.len() as u64,
        expected_source_sha256: SOURCE_SHA256.as_slice(),
        media_type: MEDIA_TYPE,
    }
}

struct TestClient(InputContentClient);

impl TestClient {
    async fn fetch_materialized(
        &self,
        reference: ClaimBoundInputAuthority<'_>,
    ) -> Result<super::input_content::MaterializedInput, InputContentError> {
        self.0.fetch_test_authority(reference).await
    }
}

fn config(deadline: Duration, max_materialized_bytes: usize) -> InputContentConfig {
    InputContentConfig {
        origin: "https://content.internal".to_owned(),
        deadline,
        max_materialized_bytes,
    }
}

fn response(
    body: &[u8],
    status: StatusCode,
    version: Version,
    source_version: &str,
) -> Response<Body> {
    let source = source();
    Response::builder()
        .status(status)
        .version(version)
        .header("content-length", body.len())
        .header(
            "content-digest",
            format!("sha-256=:{}:", STANDARD.encode(sha256(body))),
        )
        .header(
            "x-elitea-source-content-digest",
            format!("sha-256=:{}:", STANDARD.encode(sha256(&source))),
        )
        .header("x-elitea-source-content-length", source.len())
        .header("x-elitea-source-immutable-version", source_version)
        .header("cache-control", "private, no-store")
        .header("content-type", MEDIA_TYPE)
        .body(Body::new(Full::new(Bytes::copy_from_slice(body))))
        .expect("content response")
}

fn fake_client(
    response: Result<Response<Body>, InputContentTransportError>,
    deadline: Duration,
    max_materialized_bytes: usize,
) -> (TestClient, Arc<Mutex<Vec<CapturedRequest>>>) {
    let captured = Arc::new(Mutex::new(Vec::new()));
    let client = InputContentClient::with_rpc(
        FakeRpc {
            captured: Arc::clone(&captured),
            response: Mutex::new(Some(response)),
            hangs: false,
        },
        config(deadline, max_materialized_bytes),
    )
    .expect("input content client");
    (TestClient(client), captured)
}

#[tokio::test(flavor = "current_thread")]
async fn claim_bound_materialization_matches_python_and_go_route() {
    let body = materialized();
    let (client, captured) = fake_client(
        Ok(response(&body, StatusCode::OK, Version::HTTP_2, "v/1")),
        Duration::from_secs(1),
        1024,
    );

    let execution = test_lease_monitored_input_execution(SOURCE.len() as u64, *SOURCE_SHA256);
    let result = client
        .0
        .fetch_materialized(&execution)
        .await
        .expect("materialized input");

    assert_eq!(result.as_bytes(), body);
    assert_eq!(
        *captured.lock().expect("captured requests"),
        [CapturedRequest {
            method: "GET".to_owned(),
            path: "/executions/execution%2Fone/generations/2/inputs/settings%20id/versions/v%2F1"
                .to_owned(),
            claim: "claim-1".to_owned(),
            fence: "ZmZmZmZmZmZmZmZmZmZmZmZmZmZmZmZmZmZmZmZmZmY".to_owned(),
        }]
    );
}

#[tokio::test(flavor = "current_thread")]
async fn source_identity_and_response_digest_are_both_required() {
    let body = materialized();
    let wrong_source = response(&body, StatusCode::OK, Version::HTTP_2, "wrong");
    let (client, _) = fake_client(Ok(wrong_source), Duration::from_secs(1), 1024);
    assert!(matches!(
        client.fetch_materialized(reference()).await,
        Err(InputContentError::AuthorizationFailed(_))
    ));

    let mut wrong_body_digest = response(&body, StatusCode::OK, Version::HTTP_2, "v/1");
    wrong_body_digest.headers_mut().insert(
        "content-digest",
        HeaderValue::from_str(&format!("sha-256=:{}:", STANDARD.encode([9_u8; 32])))
            .expect("digest header"),
    );
    let (client, _) = fake_client(Ok(wrong_body_digest), Duration::from_secs(1), 1024);
    assert!(matches!(
        client.fetch_materialized(reference()).await,
        Err(InputContentError::InvalidInput(_))
    ));

    let mut wrong_source_digest = response(&body, StatusCode::OK, Version::HTTP_2, "v/1");
    wrong_source_digest.headers_mut().insert(
        "x-elitea-source-content-digest",
        HeaderValue::from_str(&format!("sha-256=:{}:", STANDARD.encode([7_u8; 32])))
            .expect("digest header"),
    );
    let (client, _) = fake_client(Ok(wrong_source_digest), Duration::from_secs(1), 1024);
    assert!(matches!(
        client.fetch_materialized(reference()).await,
        Err(InputContentError::AuthorizationFailed(_))
    ));

    let mut wrong_source_length = response(&body, StatusCode::OK, Version::HTTP_2, "v/1");
    wrong_source_length.headers_mut().insert(
        "x-elitea-source-content-length",
        HeaderValue::from_static("999"),
    );
    let (client, _) = fake_client(Ok(wrong_source_length), Duration::from_secs(1), 1024);
    assert!(matches!(
        client.fetch_materialized(reference()).await,
        Err(InputContentError::AuthorizationFailed(_))
    ));
}

#[tokio::test(flavor = "current_thread")]
async fn protocol_status_metadata_and_body_bounds_fail_closed() {
    let body = materialized();
    for candidate in [
        response(&body, StatusCode::OK, Version::HTTP_11, "v/1"),
        response(&body, StatusCode::FOUND, Version::HTTP_2, "v/1"),
        response(&body, StatusCode::UNAUTHORIZED, Version::HTTP_2, "v/1"),
    ] {
        let (client, _) = fake_client(Ok(candidate), Duration::from_secs(1), 1024);
        assert!(client.fetch_materialized(reference()).await.is_err());
    }

    for (status, expected) in [
        (StatusCode::NOT_FOUND, "input_content.invalid_response"),
        (
            StatusCode::UNPROCESSABLE_ENTITY,
            "input_content.invalid_response",
        ),
        (
            StatusCode::PAYLOAD_TOO_LARGE,
            "input_content.resource_exhausted",
        ),
        (
            StatusCode::SERVICE_UNAVAILABLE,
            "input_content.dependency_unavailable",
        ),
    ] {
        let (client, _) = fake_client(
            Ok(response(&body, status, Version::HTTP_2, "v/1")),
            Duration::from_secs(1),
            1024,
        );
        let Err(error) = client.fetch_materialized(reference()).await else {
            panic!("non-success status must fail")
        };
        assert_eq!(error.code(), expected);
    }

    let mut duplicate_digest = response(&body, StatusCode::OK, Version::HTTP_2, "v/1");
    duplicate_digest.headers_mut().append(
        "content-digest",
        HeaderValue::from_static("sha-256=:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=:"),
    );
    let (client, _) = fake_client(Ok(duplicate_digest), Duration::from_secs(1), 1024);
    assert!(matches!(
        client.fetch_materialized(reference()).await,
        Err(InputContentError::InvalidInput(_))
    ));

    let mut wrong_cache = response(&body, StatusCode::OK, Version::HTTP_2, "v/1");
    wrong_cache.headers_mut().insert(
        "cache-control",
        HeaderValue::from_static("public, max-age=60"),
    );
    let (client, _) = fake_client(Ok(wrong_cache), Duration::from_secs(1), 1024);
    assert!(matches!(
        client.fetch_materialized(reference()).await,
        Err(InputContentError::InvalidInput(_))
    ));

    let mut wrong_media = response(&body, StatusCode::OK, Version::HTTP_2, "v/1");
    wrong_media
        .headers_mut()
        .insert("content-type", HeaderValue::from_static("application/json"));
    let (client, _) = fake_client(Ok(wrong_media), Duration::from_secs(1), 1024);
    assert!(matches!(
        client.fetch_materialized(reference()).await,
        Err(InputContentError::InvalidInput(_))
    ));

    let mut duplicate_source = response(&body, StatusCode::OK, Version::HTTP_2, "v/1");
    duplicate_source.headers_mut().append(
        "x-elitea-source-immutable-version",
        HeaderValue::from_static("v/1"),
    );
    let (client, _) = fake_client(Ok(duplicate_source), Duration::from_secs(1), 1024);
    assert!(matches!(
        client.fetch_materialized(reference()).await,
        Err(InputContentError::InvalidInput(_))
    ));
}

#[tokio::test(flavor = "current_thread")]
async fn streaming_length_trailers_and_required_metadata_fail_closed() {
    let body = materialized();
    let (client, _) = fake_client(
        Ok(response(&body, StatusCode::OK, Version::HTTP_2, "v/1")),
        Duration::from_secs(1),
        body.len() - 1,
    );
    assert!(matches!(
        client.fetch_materialized(reference()).await,
        Err(InputContentError::ResourceExhausted(_))
    ));

    let oversized_stream = vec![b'x'; SOURCE.len() + 1];
    let mut understated = response(&oversized_stream, StatusCode::OK, Version::HTTP_2, "v/1");
    understated.headers_mut().insert(
        "content-length",
        HeaderValue::from_str(&SOURCE.len().to_string()).expect("content length"),
    );
    let (client, _) = fake_client(Ok(understated), Duration::from_secs(1), SOURCE.len());
    assert!(matches!(
        client.fetch_materialized(reference()).await,
        Err(InputContentError::ResourceExhausted(_))
    ));

    let mut wrong_length = response(&body, StatusCode::OK, Version::HTTP_2, "v/1");
    wrong_length.headers_mut().insert(
        "content-length",
        HeaderValue::from_str(&(body.len() + 1).to_string()).expect("content length"),
    );
    let (client, _) = fake_client(Ok(wrong_length), Duration::from_secs(1), 1024);
    assert!(matches!(
        client.fetch_materialized(reference()).await,
        Err(InputContentError::InvalidInput(_))
    ));

    let mut trailers = response(&body, StatusCode::OK, Version::HTTP_2, "v/1");
    *trailers.body_mut() = Body::new(Full::new(Bytes::copy_from_slice(&body)).with_trailers(
        std::future::ready(Some(Ok::<_, Infallible>(http::HeaderMap::new()))),
    ));
    let (client, _) = fake_client(Ok(trailers), Duration::from_secs(1), 1024);
    assert!(matches!(
        client.fetch_materialized(reference()).await,
        Err(InputContentError::InvalidInput(_))
    ));

    let mut noncanonical_digest = response(&body, StatusCode::OK, Version::HTTP_2, "v/1");
    noncanonical_digest.headers_mut().insert(
        "content-digest",
        HeaderValue::from_static("sha-256=:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA:"),
    );
    let (client, _) = fake_client(Ok(noncanonical_digest), Duration::from_secs(1), 1024);
    assert!(matches!(
        client.fetch_materialized(reference()).await,
        Err(InputContentError::InvalidInput(_))
    ));

    for required in [
        "content-digest",
        "x-elitea-source-content-digest",
        "x-elitea-source-content-length",
        "x-elitea-source-immutable-version",
        "cache-control",
        "content-type",
        "content-length",
    ] {
        let mut missing = response(&body, StatusCode::OK, Version::HTTP_2, "v/1");
        missing.headers_mut().remove(required);
        let (client, _) = fake_client(Ok(missing), Duration::from_secs(1), 1024);
        assert!(matches!(
            client.fetch_materialized(reference()).await,
            Err(InputContentError::InvalidInput(_))
        ));
    }
}

#[tokio::test(flavor = "current_thread")]
async fn exact_body_limit_is_accepted_and_transport_failures_are_typed() {
    let body = vec![b'x'; 1024];
    let (client, _) = fake_client(
        Ok(response(&body, StatusCode::OK, Version::HTTP_2, "v/1")),
        Duration::from_secs(1),
        body.len(),
    );
    let exact_limit = client
        .fetch_materialized(reference())
        .await
        .expect("body at exact configured limit");
    assert_eq!(exact_limit.as_bytes(), body);

    let mixed_case_body = materialized();
    let mut mixed_case_media = response(&mixed_case_body, StatusCode::OK, Version::HTTP_2, "v/1");
    mixed_case_media.headers_mut().insert(
        "content-type",
        HeaderValue::from_static(
            "Application/Vnd.Elitea.Agent-Execution-Input.V1+Protobuf; charset=binary",
        ),
    );
    let (client, _) = fake_client(Ok(mixed_case_media), Duration::from_secs(1), 1024);
    assert_eq!(
        client
            .fetch_materialized(reference())
            .await
            .expect("case-insensitive media type")
            .as_bytes(),
        mixed_case_body
    );

    let (client, _) = fake_client(
        Err(InputContentTransportError::Unavailable),
        Duration::from_secs(1),
        1024,
    );
    assert!(matches!(
        client.fetch_materialized(reference()).await,
        Err(InputContentError::Transport(
            InputContentTransportError::Unavailable
        ))
    ));
}

#[tokio::test(flavor = "current_thread")]
async fn direct_toolkit_input_media_type_is_exactly_admitted() {
    let body = materialized();
    let mut toolkit_response = response(&body, StatusCode::OK, Version::HTTP_2, "v/1");
    toolkit_response
        .headers_mut()
        .insert("content-type", HeaderValue::from_static(TOOLKIT_MEDIA_TYPE));
    let (client, captured) = fake_client(Ok(toolkit_response), Duration::from_secs(1), 1024);
    let mut toolkit_reference = reference();
    toolkit_reference.media_type = TOOLKIT_MEDIA_TYPE;

    let materialized = client
        .fetch_materialized(toolkit_reference)
        .await
        .expect("direct toolkit materialized input");

    assert_eq!(materialized.as_bytes(), body);
    assert_eq!(captured.lock().expect("captured requests").len(), 1);

    let (client, captured) = fake_client(
        Ok(response(&body, StatusCode::OK, Version::HTTP_2, "v/1")),
        Duration::from_secs(1),
        1024,
    );
    let mut lookalike = reference();
    lookalike.media_type = "application/vnd.elitea.toolkit-execute-read-input.v2+protobuf";
    assert!(matches!(
        client.fetch_materialized(lookalike).await,
        Err(InputContentError::InvalidInput(_))
    ));
    assert!(captured.lock().expect("captured requests").is_empty());
}

#[tokio::test(flavor = "current_thread")]
async fn oversized_admitted_source_fails_before_request_emission() {
    let body = materialized();
    let (client, captured) = fake_client(
        Ok(response(&body, StatusCode::OK, Version::HTTP_2, "v/1")),
        Duration::from_secs(1),
        source().len() - 1,
    );

    assert!(matches!(
        client.fetch_materialized(reference()).await,
        Err(InputContentError::ResourceExhausted(_))
    ));
    assert!(captured.lock().expect("captured requests").is_empty());
}

#[test]
fn errors_expose_safe_actionable_codes_and_retry_classification() {
    let authorization = InputContentError::AuthorizationFailed(
        "the materialized input source digest does not match the admitted descriptor",
    );
    assert_eq!(authorization.code(), "input_content.authorization_failed");
    assert!(!authorization.retryable());
    assert_eq!(
        authorization.to_string(),
        "the materialized input source digest does not match the admitted descriptor"
    );

    let unavailable = InputContentError::Transport(InputContentTransportError::Unavailable);
    assert_eq!(unavailable.code(), "input_content.dependency_unavailable");
    assert!(unavailable.retryable());
    assert_eq!(
        unavailable.to_string(),
        "the input content service is unavailable"
    );
}

#[tokio::test(flavor = "current_thread")]
async fn whole_request_deadline_bounds_a_stalled_transport() {
    let captured = Arc::new(Mutex::new(Vec::new()));
    let client = TestClient(
        InputContentClient::with_rpc(
            FakeRpc {
                captured,
                response: Mutex::new(None),
                hangs: true,
            },
            config(Duration::from_millis(1), 1024),
        )
        .expect("input content client"),
    );

    assert!(matches!(
        client.fetch_materialized(reference()).await,
        Err(InputContentError::Timeout(_))
    ));

    let body = materialized();
    let mut stalled_body = response(&body, StatusCode::OK, Version::HTTP_2, "v/1");
    *stalled_body.body_mut() = Body::new(
        Full::new(Bytes::copy_from_slice(&body))
            .with_trailers(pending::<Option<Result<http::HeaderMap, Infallible>>>()),
    );
    let (client, _) = fake_client(Ok(stalled_body), Duration::from_millis(1), 1024);
    assert!(matches!(
        client.fetch_materialized(reference()).await,
        Err(InputContentError::Timeout(_))
    ));
}

#[tokio::test(flavor = "current_thread")]
async fn configuration_and_reference_bounds_reject_unsafe_authority() {
    let rpc = FakeRpc {
        captured: Arc::new(Mutex::new(Vec::new())),
        response: Mutex::new(None),
        hangs: false,
    };
    assert!(matches!(
        InputContentClient::with_rpc(
            rpc,
            InputContentConfig {
                origin: "http://content.internal".to_owned(),
                deadline: Duration::from_secs(1),
                max_materialized_bytes: 1024,
            }
        ),
        Err(InputContentError::InvalidConfiguration(_))
    ));
    let (client, captured) = fake_client(
        Ok(response(
            &materialized(),
            StatusCode::OK,
            Version::HTTP_2,
            "v/1",
        )),
        Duration::from_secs(1),
        1024,
    );
    let malformed = ClaimBoundInputAuthority {
        claim_id: "claim\nforged",
        ..reference()
    };
    assert!(matches!(
        client.fetch_materialized(malformed).await,
        Err(InputContentError::InvalidInput(_))
    ));
    assert!(captured.lock().expect("captured requests").is_empty());

    let accepted_rpc = FakeRpc {
        captured: Arc::new(Mutex::new(Vec::new())),
        response: Mutex::new(None),
        hangs: false,
    };
    assert!(
        InputContentClient::with_rpc(
            accepted_rpc,
            InputContentConfig {
                origin: "https://CONTENT.internal:8443".to_owned(),
                deadline: Duration::from_secs(1),
                max_materialized_bytes: 1024,
            },
        )
        .is_ok()
    );

    for origin in ["https://CONTENT.internal:443", "https://[::1]:8443"] {
        let rpc = FakeRpc {
            captured: Arc::new(Mutex::new(Vec::new())),
            response: Mutex::new(None),
            hangs: false,
        };
        assert!(
            InputContentClient::with_rpc(
                rpc,
                InputContentConfig {
                    origin: origin.to_owned(),
                    deadline: Duration::from_secs(1),
                    max_materialized_bytes: 1024,
                }
            )
            .is_ok()
        );
    }
}
