//! Separate owner purpose on the same mandatory mTLS listener, never Submit.
use super::*;
use crate::{
    protocol::sandbox_grant::CodePlatformOwnerOperation, sandbox::code_recovery::canonical,
};
use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
const PREFIX: &str = "/elitea.runtime.code-platform.v1/jobs/";
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct PlatformRequest {
    schema: String,
    grant: SignedCodeEnvelope,
    #[serde(deserialize_with = "required_nullable")]
    committed_reply_base64url: Option<String>,
}
pub(in crate::sandbox) struct CodePlatformOwnerJsonService<R> {
    verifier: Arc<GrantVerifier<R>>,
    supervisor: Arc<DockerSupervisor>,
}
impl<R> CodePlatformOwnerJsonService<R> {
    pub(in crate::sandbox) fn new(
        verifier: Arc<GrantVerifier<R>>,
        supervisor: Arc<DockerSupervisor>,
    ) -> Self {
        Self {
            verifier,
            supervisor,
        }
    }
}
impl<R> Clone for CodePlatformOwnerJsonService<R> {
    fn clone(&self) -> Self {
        Self::new(self.verifier.clone(), self.supervisor.clone())
    }
}
impl<R> tonic::server::NamedService for CodePlatformOwnerJsonService<R> {
    const NAME: &'static str = "elitea.runtime.code-platform.v1";
}
impl<R: Ed25519PublicKeyResolver + 'static> Service<Request<Body>>
    for CodePlatformOwnerJsonService<R>
{
    type Response = Response<Body>;
    type Error = Infallible;
    type Future = Pin<Box<dyn Future<Output = Result<Response<Body>, Infallible>> + Send>>;
    fn poll_ready(&mut self, _: &mut Context<'_>) -> Poll<Result<(), Infallible>> {
        Poll::Ready(Ok(()))
    }
    fn call(&mut self, request: Request<Body>) -> Self::Future {
        let verifier = self.verifier.clone();
        let supervisor = self.supervisor.clone();
        Box::pin(async move {
            Ok(match handle(verifier, supervisor, request).await {
                Ok(wire) => response(StatusCode::OK, wire),
                Err(status) => response(
                    status,
                    b"{\"error\":\"code_platform_owner_refused\"}".to_vec(),
                ),
            })
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
            .and_then(|h| h.to_str().ok())
            != Some("application/json")
    {
        return Err(StatusCode::BAD_REQUEST);
    }
    let (job, operation) = route(request.uri().path()).ok_or(StatusCode::NOT_FOUND)?;
    let limit = if operation == CodePlatformOwnerOperation::PublishCommittedPlatformReply {
        3 * 1024 * 1024
    } else {
        16 * 1024
    };
    let request = tonic::Request::from_http(request);
    let peer = authenticated_peer(&request).map_err(|_| StatusCode::UNAUTHORIZED)?;
    let body = tokio::time::timeout(
        Duration::from_secs(10),
        Limited::new(request.into_inner(), limit).collect(),
    )
    .await
    .map_err(|_| StatusCode::REQUEST_TIMEOUT)?
    .map_err(|_| StatusCode::PAYLOAD_TOO_LARGE)?
    .to_bytes();
    let input: PlatformRequest =
        serde_json::from_slice(&body).map_err(|_| StatusCode::BAD_REQUEST)?;
    if input.schema != "elitea.sandbox.code-platform-owner-request.v1" {
        return Err(StatusCode::BAD_REQUEST);
    }
    let authority = verifier
        .verify_code_platform_owner(
            &input.grant,
            &peer,
            &job,
            operation,
            chrono::Utc::now().timestamp_millis(),
        )
        .map_err(|_| StatusCode::FORBIDDEN)?;
    let reply = input
        .committed_reply_base64url
        .map(|value| {
            if value.len() > 2_889_067 {
                return Err(StatusCode::PAYLOAD_TOO_LARGE);
            }
            let bytes = URL_SAFE_NO_PAD
                .decode(&value)
                .map_err(|_| StatusCode::BAD_REQUEST)?;
            if URL_SAFE_NO_PAD.encode(&bytes) != value {
                return Err(StatusCode::BAD_REQUEST);
            }
            Ok(bytes)
        })
        .transpose()?;
    if !authority.accepts_reply(reply.as_deref()) {
        return Err(StatusCode::FORBIDDEN);
    }
    let result = tokio::time::timeout(
        Duration::from_secs(10),
        supervisor.observe_code_platform_owner(&authority, reply.as_deref()),
    )
    .await
    .map_err(|_| StatusCode::REQUEST_TIMEOUT)?
    .map_err(|_| StatusCode::CONFLICT)?;
    observation_bytes(result)
}
fn observation_bytes(
    result: crate::sandbox::code_platform_owner::CodePlatformOwnerObservation,
) -> Result<Vec<u8>, StatusCode> {
    use crate::sandbox::code_platform_owner::CodePlatformOwnerObservation;
    match result {
        CodePlatformOwnerObservation::NotReady => Ok(br#"{"schema":"elitea.sandbox.code-platform-owner-response.v1","state":"not_ready","runtime":null,"pending_call_base64url":null,"reply_published":null}"#.to_vec()),
        CodePlatformOwnerObservation::Completed => Ok(br#"{"schema":"elitea.sandbox.code-platform-owner-response.v1","state":"completed","runtime":null,"pending_call_base64url":null,"reply_published":null}"#.to_vec()),
        CodePlatformOwnerObservation::Completing => Ok(br#"{"schema":"elitea.sandbox.code-platform-owner-response.v1","state":"completing","runtime":null,"pending_call_base64url":null,"reply_published":null}"#.to_vec()),
        CodePlatformOwnerObservation::Running(result) => {
            canonical(&result).map_err(|_| StatusCode::CONFLICT)
        }
    }
}
fn route(path: &str) -> Option<(String, CodePlatformOwnerOperation)> {
    let (key, op) = path.strip_prefix(PREFIX)?.split_once('/')?;
    if !crate::sandbox::code_recovery::hex_id(key, 64, true) {
        return None;
    }
    let op = match op {
        "read-retained-runtime" => CodePlatformOwnerOperation::ReadRetainedRuntime,
        "read-pending-platform-call" => CodePlatformOwnerOperation::ReadPendingPlatformCall,
        "publish-committed-platform-reply" => {
            CodePlatformOwnerOperation::PublishCommittedPlatformReply
        }
        _ => return None,
    };
    Some((key.into(), op))
}
fn required_nullable<'de, D, T>(d: D) -> Result<Option<T>, D::Error>
where
    D: serde::Deserializer<'de>,
    T: Deserialize<'de>,
{
    Option::<T>::deserialize(d)
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn completing_wire_has_no_runtime_mailbox_reply_or_result_authority() {
        let bytes = observation_bytes(
            crate::sandbox::code_platform_owner::CodePlatformOwnerObservation::Completing,
        )
        .unwrap();
        let wire: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
        assert_eq!(wire.as_object().unwrap().len(), 5);
        assert_eq!(
            wire["schema"],
            "elitea.sandbox.code-platform-owner-response.v1"
        );
        assert_eq!(wire["state"], "completing");
        for field in ["runtime", "pending_call_base64url", "reply_published"] {
            assert!(wire[field].is_null());
        }
    }
    #[test]
    fn completed_wire_has_no_runtime_mailbox_reply_or_result_authority() {
        let bytes = observation_bytes(
            crate::sandbox::code_platform_owner::CodePlatformOwnerObservation::Completed,
        )
        .unwrap();
        let wire: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
        assert_eq!(wire.as_object().unwrap().len(), 5);
        assert_eq!(
            wire["schema"],
            "elitea.sandbox.code-platform-owner-response.v1"
        );
        assert_eq!(wire["state"], "completed");
        for field in ["runtime", "pending_call_base64url", "reply_published"] {
            assert!(wire[field].is_null());
        }
    }
    #[test]
    fn not_ready_wire_has_no_runtime_mailbox_or_publication_authority() {
        let bytes = observation_bytes(
            crate::sandbox::code_platform_owner::CodePlatformOwnerObservation::NotReady,
        )
        .unwrap();
        let wire: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
        assert_eq!(wire.as_object().unwrap().len(), 5);
        assert_eq!(
            wire["schema"],
            "elitea.sandbox.code-platform-owner-response.v1"
        );
        assert_eq!(wire["state"], "not_ready");
        for field in ["runtime", "pending_call_base64url", "reply_published"] {
            assert!(wire[field].is_null());
        }
    }
    #[test]
    fn retained_owner_routes_refuse_recovery_submit_and_runtime_selectors() {
        let key = "1".repeat(64);
        assert!(route(&format!("{PREFIX}{key}/read-retained-runtime")).is_some());
        for op in [
            "read",
            "seal-no-effect",
            "SubmitSandboxJob",
            "dispatch",
            "read-retained-runtime/cid",
            "%72ead-retained-runtime",
        ] {
            assert!(route(&format!("{PREFIX}{key}/{op}")).is_none());
        }
        let value = serde_json::json!({"schema":"elitea.sandbox.code-platform-owner-request.v1","grant":{"schema":"x","key_id":"x","claims_base64url":"x","signature_base64url":"x"},"committed_reply_base64url":null,"runtime_id":"selected"});
        assert!(serde_json::from_value::<PlatformRequest>(value).is_err());
    }
}
