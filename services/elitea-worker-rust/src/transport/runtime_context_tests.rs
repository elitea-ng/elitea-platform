use std::future::pending;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;
use bytes::Bytes;
use http::{HeaderValue, Request, Response, StatusCode, Version};
use http_body_util::{BodyExt as _, Full};
use tonic::body::Body;

use super::runtime_context::{
    ArtifactDeleteRequest, ArtifactListRequest, ArtifactReadRequest, ArtifactWriteRequest,
    RuntimeContextClient, RuntimeContextConfig, RuntimeContextError, RuntimeContextRpc,
    RuntimeContextTransportError,
};
use crate::agents::runtime::{NativeAgentAssemblyError, NativeAgentAssemblyErrorCode};
use crate::protocol::control::test_runtime_context_authority;

const TOKEN: &str = "ephemeral-fixture-token";

struct FakeRpc {
    captured: Arc<Mutex<Vec<CapturedRequest>>>,
    response: Mutex<Option<Result<Response<Body>, RuntimeContextTransportError>>>,
    hangs: bool,
}

#[derive(Debug, Eq, PartialEq)]
struct CapturedRequest {
    method: String,
    path: String,
    claim: String,
    fence: String,
    body_length: usize,
}

#[async_trait]
impl RuntimeContextRpc for FakeRpc {
    async fn post(
        &self,
        request: Request<Body>,
    ) -> Result<Response<Body>, RuntimeContextTransportError> {
        let (parts, body) = request.into_parts();
        let body = body
            .collect()
            .await
            .expect("empty runtime-context request body")
            .to_bytes();
        self.captured
            .lock()
            .expect("captured runtime-context requests")
            .push(CapturedRequest {
                method: parts.method.as_str().to_owned(),
                path: parts.uri.path().to_owned(),
                claim: parts
                    .headers
                    .get("x-elitea-claim-id")
                    .expect("claim header")
                    .to_str()
                    .expect("claim text")
                    .to_owned(),
                fence: parts
                    .headers
                    .get("x-elitea-fence")
                    .expect("fence header")
                    .to_str()
                    .expect("fence text")
                    .to_owned(),
                body_length: body.len(),
            });
        if self.hangs {
            return pending().await;
        }
        self.response
            .lock()
            .expect("runtime-context response")
            .take()
            .expect("one runtime-context response")
    }
}

fn config(deadline: Duration, max_response_bytes: usize) -> RuntimeContextConfig {
    RuntimeContextConfig {
        origin: "https://CONTENT.internal:443".to_owned(),
        deadline,
        max_response_bytes,
        max_application_response_bytes: 1024 * 1024,
        max_attachment_response_bytes: 1024 * 1024,
        max_artifact_response_bytes: 2 * 1024 * 1024,
    }
}

fn body(project_id: u64, token: &str) -> String {
    serde_json::json!({
        "schema_version": "elitea.runtime.elitea-client-token.v1",
        "project_id": project_id,
        "token": token,
    })
    .to_string()
}

fn application_body(project_id: u64, application_id: u64, version_id: u64) -> String {
    serde_json::json!({
        "schema_version": "elitea.runtime.application-version.v1",
        "project_id": project_id,
        "application_id": application_id,
        "version_id": version_id,
        "version_details": {
            "agent_type": "agent",
            "instructions": "Use the configured tools.",
            "llm_settings": {"model_name": "gpt-4o-mini"},
            "tools": []
        }
    })
    .to_string()
}

fn response(raw: &str, status: StatusCode, version: Version) -> Response<Body> {
    Response::builder()
        .status(status)
        .version(version)
        .header("content-type", "application/json; charset=utf-8")
        .header("cache-control", "private, no-cache, no-store")
        .header("pragma", "no-cache")
        .header("content-length", raw.len())
        .body(Body::new(Full::new(Bytes::copy_from_slice(raw.as_bytes()))))
        .expect("runtime-context response")
}

fn fake_client(
    response: Result<Response<Body>, RuntimeContextTransportError>,
    deadline: Duration,
    max_response_bytes: usize,
) -> (RuntimeContextClient, Arc<Mutex<Vec<CapturedRequest>>>) {
    let captured = Arc::new(Mutex::new(Vec::new()));
    let client = RuntimeContextClient::with_rpc(
        FakeRpc {
            captured: Arc::clone(&captured),
            response: Mutex::new(Some(response)),
            hangs: false,
        },
        config(deadline, max_response_bytes),
    )
    .expect("runtime-context client");
    (client, captured)
}

#[tokio::test(flavor = "current_thread")]
async fn authorized_claim_redeems_exact_python_and_go_route() {
    let raw = body(17, TOKEN);
    let (client, captured) = fake_client(
        Ok(response(&raw, StatusCode::OK, Version::HTTP_2)),
        Duration::from_secs(1),
        32 * 1_024,
    );

    let context = client
        .redeem(&test_runtime_context_authority())
        .await
        .expect("claim-scoped runtime context");

    assert_eq!(context.project_id(), 17);
    assert_eq!(context.token(), TOKEN);
    assert_eq!(
        *captured.lock().expect("captured request"),
        [CapturedRequest {
            method: "POST".to_owned(),
            path: "/executions/execution%2Fone/generations/2/runtime-context/elitea-client-token"
                .to_owned(),
            claim: "claim-1".to_owned(),
            fence: "ZmZmZmZmZmZmZmZmZmZmZmZmZmZmZmZmZmZmZmZmZmY".to_owned(),
            body_length: 0,
        }]
    );
}

#[tokio::test(flavor = "current_thread")]
async fn exact_child_identity_uses_the_claim_bound_platform_route_once() {
    let raw = application_body(17, 31, 41);
    let (client, captured) = fake_client(
        Ok(response(&raw, StatusCode::OK, Version::HTTP_2)),
        Duration::from_secs(1),
        32 * 1_024,
    );
    let authority = test_runtime_context_authority();

    let resolved = client
        .load_application_version(&authority, 31, 41)
        .await
        .expect("claim-bound child version");

    assert_eq!(resolved.application_id(), 31);
    assert_eq!(resolved.version_id(), 41);
    assert_eq!(
        resolved
            .into_version_details()
            .get("agent_type")
            .and_then(serde_json::Value::as_str),
        Some("agent")
    );
    assert_eq!(
        *captured.lock().expect("captured request"),
        [CapturedRequest {
            method: "POST".to_owned(),
            path: "/executions/execution%2Fone/generations/2/runtime-context/applications/31/versions/41"
                .to_owned(),
            claim: "claim-1".to_owned(),
            fence: "ZmZmZmZmZmZmZmZmZmZmZmZmZmZmZmZmZmZmZmZmZmY".to_owned(),
            body_length: 0,
        }]
    );
}

#[tokio::test(flavor = "current_thread")]
async fn child_identity_and_materialized_response_are_exact_and_bounded() {
    let authority = test_runtime_context_authority();
    let raw = application_body(17, 31, 41);
    for (application_id, version_id) in [(0, 41), (31, 0)] {
        let (client, captured) = fake_client(
            Ok(response(&raw, StatusCode::OK, Version::HTTP_2)),
            Duration::from_secs(1),
            32 * 1_024,
        );
        assert!(
            client
                .load_application_version(&authority, application_id, version_id)
                .await
                .is_err()
        );
        assert!(captured.lock().expect("captured request").is_empty());
    }

    for raw in [
        application_body(18, 31, 41),
        application_body(17, 32, 41),
        application_body(17, 31, 42),
        r#"{"schema_version":"elitea.runtime.application-version.v1","project_id":17,"application_id":31,"version_id":41,"version_details":{},"extra":true}"#.to_owned(),
    ] {
        let (client, _) = fake_client(
            Ok(response(&raw, StatusCode::OK, Version::HTTP_2)),
            Duration::from_secs(1),
            32 * 1_024,
        );
        assert!(
            client
                .load_application_version(&authority, 31, 41)
                .await
                .is_err()
        );
    }
}

/// A deleted child version must fail the turn once, not look transient.
///
/// Main answers 404 for an agent/version pair the claim was allowed to read
/// but that no longer exists, and keeps it distinct from the 403 it uses for a
/// rejected claim (`internal/infra/storage/content_server.go`). Collapsing 404
/// into `dependency_unavailable` made a DELETED reference retryable, so a
/// parent whose child version was removed spent its whole retry budget on a
/// failure whose reason was already known.
#[tokio::test(flavor = "current_thread")]
async fn a_deleted_nested_application_version_is_terminal_and_not_retryable() {
    let raw = application_body(17, 31, 41);
    let (client, _) = fake_client(
        Ok(response(&raw, StatusCode::NOT_FOUND, Version::HTTP_2)),
        Duration::from_secs(1),
        32 * 1_024,
    );

    let Err(error) = client
        .load_application_version(&test_runtime_context_authority(), 31, 41)
        .await
    else {
        panic!("a nested application version main no longer has must fail")
    };

    assert_eq!(error.code(), "runtime_context.not_found");
    assert!(!error.retryable());
    let mapped = NativeAgentAssemblyError::from(error);
    assert_eq!(mapped.code(), NativeAgentAssemblyErrorCode::InvalidInput);
    assert!(!mapped.retryable());
}

#[tokio::test(flavor = "current_thread")]
async fn status_protocol_cache_and_length_fail_closed() {
    let raw = body(17, TOKEN);
    for (candidate, expected_code, retryable) in [
        (
            response(&raw, StatusCode::OK, Version::HTTP_11),
            "runtime_context.dependency_unavailable",
            true,
        ),
        (
            response(&raw, StatusCode::FOUND, Version::HTTP_2),
            "runtime_context.dependency_unavailable",
            true,
        ),
        (
            response(&raw, StatusCode::UNAUTHORIZED, Version::HTTP_2),
            "runtime_context.authorization_failed",
            false,
        ),
        (
            response(&raw, StatusCode::NOT_FOUND, Version::HTTP_2),
            "runtime_context.not_found",
            false,
        ),
        (
            response(&raw, StatusCode::SERVICE_UNAVAILABLE, Version::HTTP_2),
            "runtime_context.dependency_unavailable",
            true,
        ),
    ] {
        let (client, _) = fake_client(Ok(candidate), Duration::from_secs(1), 32 * 1_024);
        let Err(error) = client.redeem(&test_runtime_context_authority()).await else {
            panic!("invalid runtime-context response must fail")
        };
        assert_eq!(error.code(), expected_code);
        assert_eq!(error.retryable(), retryable);
    }

    let mut wrong_cache = response(&raw, StatusCode::OK, Version::HTTP_2);
    wrong_cache.headers_mut().insert(
        "cache-control",
        HeaderValue::from_static("private, no-store"),
    );
    let (client, _) = fake_client(Ok(wrong_cache), Duration::from_secs(1), 32 * 1_024);
    assert!(matches!(
        client.redeem(&test_runtime_context_authority()).await,
        Err(RuntimeContextError::InvalidResponse(_))
    ));

    let mut wrong_length = response(&raw, StatusCode::OK, Version::HTTP_2);
    wrong_length.headers_mut().insert(
        "content-length",
        HeaderValue::from_str(&(raw.len() + 1).to_string()).expect("content length"),
    );
    let (client, _) = fake_client(Ok(wrong_length), Duration::from_secs(1), 32 * 1_024);
    assert!(matches!(
        client.redeem(&test_runtime_context_authority()).await,
        Err(RuntimeContextError::InvalidResponse(_))
    ));

    let (client, _) = fake_client(
        Ok(response(&raw, StatusCode::OK, Version::HTTP_2)),
        Duration::from_secs(1),
        raw.len() - 1,
    );
    assert!(matches!(
        client.redeem(&test_runtime_context_authority()).await,
        Err(RuntimeContextError::ResourceExhausted(_))
    ));
}

#[tokio::test(flavor = "current_thread")]
async fn unique_schema_project_and_secret_shape_are_authoritative() {
    for raw in [
        r#"{"schema_version":"elitea.runtime.elitea-client-token.v1","project_id":17,"project_id":17,"token":"fixture"}"#.to_owned(),
        r#"{"schema_version":"elitea.runtime.elitea-client-token.v1","project_id":17,"token":"fixture","extra":true}"#.to_owned(),
        body(18, TOKEN),
        body(17, "bad\ncredential"),
        r#"{"schema_version":"wrong","project_id":17,"token":"fixture"}"#.to_owned(),
    ] {
        let (client, _) = fake_client(
            Ok(response(&raw, StatusCode::OK, Version::HTTP_2)),
            Duration::from_secs(1),
            32 * 1_024,
        );
        assert!(
            client
                .redeem(&test_runtime_context_authority())
                .await
                .is_err()
        );
    }
}

#[tokio::test(flavor = "current_thread")]
async fn timeout_transport_and_errors_are_bounded_and_redacted() {
    let captured = Arc::new(Mutex::new(Vec::new()));
    let client = RuntimeContextClient::with_rpc(
        FakeRpc {
            captured,
            response: Mutex::new(None),
            hangs: true,
        },
        config(Duration::from_millis(1), 32 * 1_024),
    )
    .expect("runtime-context client");
    let Err(error) = client.redeem(&test_runtime_context_authority()).await else {
        panic!("stalled runtime context must time out")
    };
    assert_eq!(error.code(), "runtime_context.timeout");
    assert!(error.retryable());

    let (client, _) = fake_client(
        Err(RuntimeContextTransportError::Unavailable),
        Duration::from_secs(1),
        32 * 1_024,
    );
    let Err(error) = client.redeem(&test_runtime_context_authority()).await else {
        panic!("unavailable runtime context must fail")
    };
    assert_eq!(error.code(), "runtime_context.dependency_unavailable");
    assert!(error.retryable());
    let rendered = format!("{error:?} {error}");
    assert!(!rendered.contains(TOKEN));
    assert!(!rendered.contains("ZmZm"));
}

#[test]
fn configuration_bounds_and_origin_canonicalization_match_worker_policy() {
    for origin in [
        "http://content.internal",
        "https://user@content.internal",
        "https://content.internal/path",
        "https://content.internal?query=1",
    ] {
        let result = RuntimeContextClient::with_rpc(
            FakeRpc {
                captured: Arc::new(Mutex::new(Vec::new())),
                response: Mutex::new(None),
                hangs: false,
            },
            RuntimeContextConfig {
                origin: origin.to_owned(),
                deadline: Duration::from_secs(1),
                max_response_bytes: 32 * 1_024,
                max_application_response_bytes: 1_024 * 1_024,
                max_attachment_response_bytes: 1_024 * 1_024,
                max_artifact_response_bytes: 2 * 1_024 * 1_024,
            },
        );
        assert!(matches!(
            result,
            Err(RuntimeContextError::InvalidConfiguration(_))
        ));
    }

    for (deadline, maximum) in [
        (Duration::ZERO, 1),
        (Duration::from_secs(301), 1),
        (Duration::from_secs(1), 0),
        (Duration::from_secs(1), 32 * 1_024 + 1),
    ] {
        let result = RuntimeContextClient::with_rpc(
            FakeRpc {
                captured: Arc::new(Mutex::new(Vec::new())),
                response: Mutex::new(None),
                hangs: false,
            },
            config(deadline, maximum),
        );
        assert!(matches!(
            result,
            Err(RuntimeContextError::InvalidConfiguration(_))
        ));
    }
}

#[test]
fn runtime_context_failures_preserve_terminal_taxonomy() {
    for (error, expected) in [
        (
            RuntimeContextError::InvalidConfiguration("fixture"),
            NativeAgentAssemblyErrorCode::InvalidConfiguration,
        ),
        (
            RuntimeContextError::InvalidResponse("fixture"),
            NativeAgentAssemblyErrorCode::InvalidInput,
        ),
        (
            RuntimeContextError::ResourceExhausted("fixture"),
            NativeAgentAssemblyErrorCode::ResourceExhausted,
        ),
        (
            RuntimeContextError::AuthorizationFailed("fixture"),
            NativeAgentAssemblyErrorCode::AuthorizationFailed,
        ),
        (
            RuntimeContextError::NotFound("fixture"),
            NativeAgentAssemblyErrorCode::InvalidInput,
        ),
        (
            RuntimeContextError::Timeout("fixture"),
            NativeAgentAssemblyErrorCode::DependencyUnavailable,
        ),
    ] {
        let mapped = NativeAgentAssemblyError::from(error);
        assert_eq!(mapped.code(), expected);
    }
}

// ─────────────────────────────────────────────────────────────────────────────
// The `artifact` toolkit family's four routes (#906)
// ─────────────────────────────────────────────────────────────────────────────

fn artifact_read_body(project_id: u64, content: &str, over_limit: bool) -> String {
    serde_json::json!({
        "schema_version": "elitea.runtime.artifact-read.v1",
        "project_id": project_id,
        "bucket": "agent-artifacts",
        "name": "reports/one.txt",
        "media_type": "text/plain",
        "byte_length": content.len(),
        "char_length": if over_limit { 300_000 } else { content.chars().count() },
        "total_lines": 4_167,
        "max_chars": 200_000,
        "over_limit": over_limit,
        "content": content,
    })
    .to_string()
}

/// Each of the four operations is its own path segment under one claim
/// contract, and each carries a BODY — the bucket, the key and the document
/// do not belong in a path.
#[tokio::test(flavor = "current_thread")]
async fn artifact_operations_use_one_claim_bound_route_each() {
    let raw = serde_json::json!({
        "schema_version": "elitea.runtime.artifact-list.v1",
        "project_id": 17,
        "bucket": "agent-artifacts",
        "files": [{
            "name": "reports/one.txt",
            "byte_length": 4,
            "media_type": "text/plain",
            "modified_at": "2026-09-20T10:00:00Z",
        }],
        "truncated": false,
    })
    .to_string();
    let (client, captured) = fake_client(
        Ok(response(&raw, StatusCode::OK, Version::HTTP_2)),
        Duration::from_secs(1),
        32 * 1_024,
    );

    let listed = client
        .list_artifacts(
            &test_runtime_context_authority(),
            &ArtifactListRequest {
                bucket: "agent-artifacts".to_owned(),
                prefix: "reports/".to_owned(),
                recursive: true,
                limit: 200,
            },
        )
        .await
        .expect("claim-bound artifact listing");

    assert_eq!(listed.files.len(), 1);
    assert_eq!(listed.files[0].name, "reports/one.txt");
    let captured = captured.lock().expect("captured request");
    assert_eq!(
        captured[0].path,
        "/executions/execution%2Fone/generations/2/runtime-context/artifacts/list"
    );
    assert_eq!(captured[0].claim, "claim-1");
    assert!(captured[0].body_length > 0);
}

/// A file over the agent-path cap comes back as a MEASUREMENT with a 200, and
/// it must carry no content: a refusal that returned the file anyway would
/// have capped nothing.
#[tokio::test(flavor = "current_thread")]
async fn an_over_cap_artifact_read_returns_the_measurement_without_content() {
    let raw = artifact_read_body(17, "", true);
    let (client, _captured) = fake_client(
        Ok(response(&raw, StatusCode::OK, Version::HTTP_2)),
        Duration::from_secs(1),
        32 * 1_024,
    );

    let outcome = client
        .read_artifact(
            &test_runtime_context_authority(),
            &ArtifactReadRequest {
                bucket: "agent-artifacts".to_owned(),
                name: "reports/one.txt".to_owned(),
            },
        )
        .await
        .expect("claim-bound artifact read");

    assert!(outcome.over_limit);
    assert!(outcome.content.is_empty());
    assert_eq!(outcome.max_chars, 200_000);
    assert_eq!(outcome.char_length, 300_000);
}

/// A response that says "served" and carries nothing — or "refused" and
/// carries the file — is refused rather than believed: either shape would let
/// a refusal reach the model as an empty file, which it answers from as though
/// the file were empty.
#[tokio::test(flavor = "current_thread")]
async fn an_artifact_read_that_contradicts_its_own_verdict_is_refused() {
    for (content, over_limit) in [("", false), ("data", true)] {
        let raw = artifact_read_body(17, content, over_limit);
        let (client, _captured) = fake_client(
            Ok(response(&raw, StatusCode::OK, Version::HTTP_2)),
            Duration::from_secs(1),
            32 * 1_024,
        );
        let Err(error) = client
            .read_artifact(
                &test_runtime_context_authority(),
                &ArtifactReadRequest {
                    bucket: "agent-artifacts".to_owned(),
                    name: "reports/one.txt".to_owned(),
                },
            )
            .await
        else {
            panic!("a contradictory artifact read was accepted");
        };
        assert_eq!(error.code(), "runtime_context.invalid_response");
    }
}

/// A response naming another project is an authorization failure, not a
/// decoding quirk: two of these four routes CHANGE a bucket, so a silent
/// mismatch would be a write into a tenant this turn never had.
#[tokio::test(flavor = "current_thread")]
async fn an_artifact_write_for_another_project_is_refused() {
    let raw = serde_json::json!({
        "schema_version": "elitea.runtime.artifact-write.v1",
        "project_id": 18,
        "bucket": "agent-artifacts",
        "name": "reports/one.txt",
        "media_type": "text/plain",
        "byte_length": 4,
    })
    .to_string();
    let (client, _captured) = fake_client(
        Ok(response(&raw, StatusCode::OK, Version::HTTP_2)),
        Duration::from_secs(1),
        32 * 1_024,
    );

    let Err(error) = client
        .write_artifact(
            &test_runtime_context_authority(),
            &ArtifactWriteRequest {
                bucket: "agent-artifacts".to_owned(),
                name: "reports/one.txt".to_owned(),
                content: "data".to_owned(),
            },
        )
        .await
    else {
        panic!("a foreign-project artifact write was accepted");
    };
    assert_eq!(error.code(), "runtime_context.authorization_failed");
}

/// 422 is the DOCUMENT's refusal and it is terminal, never retryable: the tool
/// tells the model to write less instead of sending the identical body again.
#[tokio::test(flavor = "current_thread")]
async fn a_refused_artifact_document_is_terminal() {
    let (client, _captured) = fake_client(
        Ok(response(
            "{}",
            StatusCode::UNPROCESSABLE_ENTITY,
            Version::HTTP_2,
        )),
        Duration::from_secs(1),
        32 * 1_024,
    );

    let Err(error) = client
        .delete_artifact(
            &test_runtime_context_authority(),
            &ArtifactDeleteRequest {
                bucket: "agent-artifacts".to_owned(),
                name: "reports/one.txt".to_owned(),
            },
        )
        .await
    else {
        panic!("a refused artifact document was accepted");
    };
    assert_eq!(error.code(), "runtime_context.rejected");
    assert!(!error.retryable());
}
