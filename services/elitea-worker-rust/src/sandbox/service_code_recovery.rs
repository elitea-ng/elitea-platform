//! Versioned JSON routes on the original mandatory mTLS Supervisor listener.
use super::{DockerSupervisor, Ed25519PublicKeyResolver, GrantVerifier, authenticated_peer};
use crate::protocol::sandbox_grant::{CodeOwnerOperation, SignedCodeEnvelope};
use bytes::Bytes;
use http::{Method, Request, Response, StatusCode};
use http_body_util::{BodyExt as _, Full, Limited};
use serde::Deserialize;
use std::{
    convert::Infallible,
    future::Future,
    pin::Pin,
    sync::Arc,
    task::{Context, Poll},
    time::Duration,
};
use tonic::body::Body;
use tower::Service;

const PREFIX: &str = "/elitea.runtime.node-code-recovery.v1/jobs/";
#[path = "service_code_platform.rs"]
mod platform;
pub(super) use platform::CodePlatformOwnerJsonService;
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct OwnerRequest {
    schema: String,
    grant: SignedCodeEnvelope,
}
pub(super) struct CodeOwnerJsonService<R> {
    verifier: Arc<GrantVerifier<R>>,
    supervisor: Arc<DockerSupervisor>,
}
impl<R> CodeOwnerJsonService<R> {
    pub(super) fn new(verifier: Arc<GrantVerifier<R>>, supervisor: Arc<DockerSupervisor>) -> Self {
        Self {
            verifier,
            supervisor,
        }
    }
}
impl<R> Clone for CodeOwnerJsonService<R> {
    fn clone(&self) -> Self {
        Self {
            verifier: Arc::clone(&self.verifier),
            supervisor: Arc::clone(&self.supervisor),
        }
    }
}
impl<R> tonic::server::NamedService for CodeOwnerJsonService<R> {
    const NAME: &'static str = "elitea.runtime.node-code-recovery.v1";
}
impl<R: Ed25519PublicKeyResolver + 'static> Service<Request<Body>> for CodeOwnerJsonService<R> {
    type Response = Response<Body>;
    type Error = Infallible;
    type Future = Pin<Box<dyn Future<Output = Result<Response<Body>, Infallible>> + Send>>;
    fn poll_ready(&mut self, _: &mut Context<'_>) -> Poll<Result<(), Infallible>> {
        Poll::Ready(Ok(()))
    }
    fn call(&mut self, request: Request<Body>) -> Self::Future {
        let verifier = Arc::clone(&self.verifier);
        let supervisor = Arc::clone(&self.supervisor);
        Box::pin(async move {
            let response = match handle(verifier, supervisor, request).await {
                Ok(wire) => response(StatusCode::OK, wire),
                Err(status) => response(
                    status,
                    b"{\"error\":\"code_owner_authority_refused\"}".to_vec(),
                ),
            };
            Ok(response)
        })
    }
}
async fn handle<R: Ed25519PublicKeyResolver + 'static>(
    verifier: Arc<GrantVerifier<R>>,
    supervisor: Arc<DockerSupervisor>,
    request: Request<Body>,
) -> Result<Vec<u8>, StatusCode> {
    if request.method() != Method::POST
        || request.uri().query().is_some()
        || request
            .headers()
            .get(http::header::CONTENT_TYPE)
            .and_then(|v| v.to_str().ok())
            != Some("application/json")
    {
        return Err(StatusCode::BAD_REQUEST);
    }
    let (job_key, operation) = route(request.uri().path()).ok_or(StatusCode::NOT_FOUND)?;
    let request = tonic::Request::from_http(request);
    let peer = authenticated_peer(&request).map_err(|_| StatusCode::UNAUTHORIZED)?;
    let body = tokio::time::timeout(
        Duration::from_secs(10),
        Limited::new(request.into_inner(), 16 * 1024).collect(),
    )
    .await
    .map_err(|_| StatusCode::REQUEST_TIMEOUT)?
    .map_err(|_| StatusCode::PAYLOAD_TOO_LARGE)?
    .to_bytes();
    let input: OwnerRequest = serde_json::from_slice(&body).map_err(|_| StatusCode::BAD_REQUEST)?;
    if input.schema != "elitea.sandbox.node-code-recovery-request.v1" {
        return Err(StatusCode::BAD_REQUEST);
    }
    let authority = verifier
        .verify_code_recovery(
            &input.grant,
            &peer,
            &job_key,
            operation,
            chrono::Utc::now().timestamp_millis(),
        )
        .map_err(|_| StatusCode::FORBIDDEN)?;
    // This method calls only JobLedger's immutable read or the special no-effect seal.
    let result = tokio::time::timeout(
        Duration::from_secs(10),
        supervisor.observe_code_owner(&authority),
    )
    .await
    .map_err(|_| StatusCode::REQUEST_TIMEOUT)?
    .map_err(|_| StatusCode::CONFLICT)?;
    result.canonical_bytes().map_err(|_| StatusCode::CONFLICT)
}
fn route(path: &str) -> Option<(String, CodeOwnerOperation)> {
    let rest = path.strip_prefix(PREFIX)?;
    let (key, operation) = rest.split_once('/')?;
    if !crate::sandbox::code_recovery::hex_id(key, 64, true) {
        return None;
    }
    let operation = match operation {
        "read" => CodeOwnerOperation::Read,
        "seal-no-effect" => CodeOwnerOperation::SealNoEffect,
        _ => return None,
    };
    Some((key.to_owned(), operation))
}
fn response(status: StatusCode, wire: Vec<u8>) -> Response<Body> {
    let length = wire.len();
    let mut response = Response::new(Body::new(Full::new(Bytes::from(wire))));
    *response.status_mut() = status;
    response.headers_mut().insert(
        http::header::CONTENT_TYPE,
        http::HeaderValue::from_static("application/json"),
    );
    response.headers_mut().insert(
        http::header::CACHE_CONTROL,
        http::HeaderValue::from_static("no-store"),
    );
    response.headers_mut().insert(
        http::header::CONTENT_LENGTH,
        http::HeaderValue::from(length),
    );
    response
}
#[cfg(test)]
mod tests {
    use super::*;
    pub(super) struct NoKeys;
    impl Ed25519PublicKeyResolver for NoKeys {
        fn resolve_ed25519_public_key(&self, _: &str) -> Option<[u8; 32]> {
            None
        }
    }
    struct NoRuntime;
    #[async_trait::async_trait]
    impl crate::sandbox::runtime::CodeJobRuntime for NoRuntime {
        #[allow(
            clippy::unnecessary_literal_bound,
            reason = "Match the existing runtime trait signature in this fixture."
        )]
        fn image_digest(&self) -> &str {
            "unconfigured"
        }
        fn code_compilation_enabled(&self) -> bool {
            false
        }
        fn code_job_timeout(&self) -> Duration {
            Duration::from_secs(1)
        }
        async fn instance(
            &self,
            _: &adk_sandbox::workspace::docker::CodeJobIdentity,
        ) -> Result<Option<String>, adk_sandbox::SandboxError> {
            panic!("owner route entered runtime")
        }
        async fn exists(
            &self,
            _: &adk_sandbox::workspace::docker::CodeJobIdentity,
        ) -> Result<bool, adk_sandbox::SandboxError> {
            panic!("owner route entered runtime")
        }
        async fn prepare(
            &self,
            _: &adk_sandbox::workspace::docker::CodeJobIdentity,
            _: &adk_sandbox::workspace::Manifest,
        ) -> Result<(), adk_sandbox::SandboxError> {
            panic!("owner read prepared Code")
        }
        async fn prepared(
            &self,
            _: &adk_sandbox::workspace::docker::CodeJobIdentity,
        ) -> Result<bool, adk_sandbox::SandboxError> {
            panic!("owner route entered runtime")
        }
        async fn dispatch(
            &self,
            _: &adk_sandbox::workspace::docker::CodeJobIdentity,
        ) -> Result<(), adk_sandbox::SandboxError> {
            panic!("owner read dispatched Code")
        }
        async fn receipt(
            &self,
            _: &adk_sandbox::workspace::docker::CodeJobIdentity,
        ) -> Result<Option<Vec<u8>>, adk_sandbox::SandboxError> {
            panic!("owner route read runtime result instead of ledger")
        }
        async fn terminate(
            &self,
            _: &adk_sandbox::workspace::docker::CodeJobIdentity,
        ) -> Result<(), adk_sandbox::SandboxError> {
            panic!("owner read terminated Code")
        }
        async fn cleanup(
            &self,
            _: &adk_sandbox::workspace::docker::CodeJobIdentity,
        ) -> Result<(), adk_sandbox::SandboxError> {
            panic!("owner read cleaned Code")
        }
    }
    pub(super) fn no_io_owner_service_fixture()
    -> (Arc<GrantVerifier<NoKeys>>, Arc<DockerSupervisor>) {
        let verifier = Arc::new(
            GrantVerifier::new(NoKeys, "dns:supervisor.fixture".into())
                .unwrap()
                .with_code_owner_requester("dns:main.fixture".into())
                .unwrap(),
        );
        let pool = sqlx::postgres::PgPoolOptions::new()
            .max_connections(1)
            .acquire_timeout(Duration::from_millis(50))
            .connect_lazy("postgres://fixture:fixture@127.0.0.1:1/never_connect")
            .unwrap();
        let supervisor = Arc::new(
            DockerSupervisor::new(
                crate::sandbox::ledger::JobLedger::new(pool),
                NoRuntime,
                "fixture-owner".into(),
                1,
            )
            .unwrap(),
        );
        (verifier, supervisor)
    }
    #[tokio::test]
    async fn actual_owner_json_route_refuses_missing_mtls_before_ledger_or_runtime() {
        let (verifier, supervisor) = no_io_owner_service_fixture();
        let request = Request::builder()
            .method(Method::POST)
            .uri(format!("{PREFIX}{}/read", "1".repeat(64)))
            .header(http::header::CONTENT_TYPE, "application/json")
            .body(Body::empty())
            .unwrap();
        let response = tokio::time::timeout(
            Duration::from_millis(100),
            handle(verifier, supervisor, request),
        )
        .await
        .unwrap();
        assert_eq!(response.err(), Some(StatusCode::UNAUTHORIZED));
    }
    #[test]
    fn owner_routes_never_admit_submit_or_encoded_identity() {
        let key = "1".repeat(64);
        assert_eq!(
            route(&format!("{PREFIX}{key}/read")).unwrap().1,
            CodeOwnerOperation::Read
        );
        assert_eq!(
            route(&format!("{PREFIX}{key}/seal-no-effect")).unwrap().1,
            CodeOwnerOperation::SealNoEffect
        );
        for suffix in [
            "SubmitSandboxJob",
            "read/extra",
            "%72ead",
            "cancel",
            "dispatch",
        ] {
            assert!(route(&format!("{PREFIX}{key}/{suffix}")).is_none());
        }
        assert!(route(&format!("{PREFIX}{}/read", "0".repeat(64))).is_none());
    }
}
