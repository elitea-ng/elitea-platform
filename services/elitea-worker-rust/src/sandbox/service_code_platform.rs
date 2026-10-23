//! Separate owner purpose on the same mandatory mTLS listener, never Submit.
use super::{
    Deserialize, DockerSupervisor, Ed25519PublicKeyResolver, GrantVerifier, OWNER_DEADLINE,
    OWNER_REQUEST_LIMIT, SignedCodeEnvelope, Status, conflict, expired, malformed, owner_request,
    refused, valid_job_key,
};
use crate::{
    protocol::sandbox_grant::CodePlatformOwnerOperation, sandbox::code_recovery::canonical,
};
use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
/// A committed reply publication carries the reply itself.
const PUBLISH_REQUEST_LIMIT: usize = 3 * 1024 * 1024;
/// The longest base64url encoding of a committed platform reply.
const MAX_REPLY_BASE64URL: usize = 2_889_067;
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct PlatformRequest {
    schema: String,
    grant: SignedCodeEnvelope,
    #[serde(deserialize_with = "required_nullable")]
    committed_reply_base64url: Option<String>,
}
/// Answer one retained Code platform owner RPC for the authenticated `peer`.
/// Refusals carry a fixed message and never any ledger content.
pub(in crate::sandbox) async fn platform_owner<R: Ed25519PublicKeyResolver>(
    verifier: &GrantVerifier<R>,
    supervisor: &DockerSupervisor,
    peer: &str,
    job_key: &str,
    operation: CodePlatformOwnerOperation,
    request_json: &[u8],
) -> Result<Vec<u8>, Status> {
    valid_job_key(job_key)?;
    let limit = if operation == CodePlatformOwnerOperation::PublishCommittedPlatformReply {
        PUBLISH_REQUEST_LIMIT
    } else {
        OWNER_REQUEST_LIMIT
    };
    let input: PlatformRequest = owner_request(request_json, limit)?;
    if input.schema != "elitea.sandbox.code-platform-owner-request.v1" {
        return Err(malformed());
    }
    let authority = verifier
        .verify_code_platform_owner(
            &input.grant,
            peer,
            job_key,
            operation,
            chrono::Utc::now().timestamp_millis(),
        )
        .map_err(|_| refused())?;
    let reply = input
        .committed_reply_base64url
        .map(|value| {
            if value.len() > MAX_REPLY_BASE64URL {
                return Err(Status::resource_exhausted(
                    "The committed platform reply exceeds its size limit.",
                ));
            }
            let bytes = URL_SAFE_NO_PAD.decode(&value).map_err(|_| malformed())?;
            if URL_SAFE_NO_PAD.encode(&bytes) != value {
                return Err(malformed());
            }
            Ok(bytes)
        })
        .transpose()?;
    if !authority.accepts_reply(reply.as_deref()) {
        return Err(refused());
    }
    let result = tokio::time::timeout(
        OWNER_DEADLINE,
        supervisor.observe_code_platform_owner(&authority, reply.as_deref()),
    )
    .await
    .map_err(|_| expired())?
    .map_err(|_| conflict())?;
    observation_bytes(result)
}
fn observation_bytes(
    result: crate::sandbox::code_platform_owner::CodePlatformOwnerObservation,
) -> Result<Vec<u8>, Status> {
    use crate::sandbox::code_platform_owner::CodePlatformOwnerObservation;
    match result {
        CodePlatformOwnerObservation::NotReady => Ok(br#"{"schema":"elitea.sandbox.code-platform-owner-response.v1","state":"not_ready","runtime":null,"pending_call_base64url":null,"reply_published":null}"#.to_vec()),
        CodePlatformOwnerObservation::Completed => Ok(br#"{"schema":"elitea.sandbox.code-platform-owner-response.v1","state":"completed","runtime":null,"pending_call_base64url":null,"reply_published":null}"#.to_vec()),
        CodePlatformOwnerObservation::Completing => Ok(br#"{"schema":"elitea.sandbox.code-platform-owner-response.v1","state":"completing","runtime":null,"pending_call_base64url":null,"reply_published":null}"#.to_vec()),
        CodePlatformOwnerObservation::Running(result) => {
            canonical(&result).map_err(|_| conflict())
        }
    }
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
    #[tokio::test]
    async fn retained_owner_requests_refuse_recovery_bodies_selectors_and_bad_keys() {
        let (verifier, supervisor) = super::super::tests::no_io_owner_service_fixture();
        let grant = serde_json::json!({"schema":"x","key_id":"x","claims_base64url":"x","signature_base64url":"x"});
        let request = |extra: serde_json::Value| {
            let mut value = serde_json::json!({"schema":"elitea.sandbox.code-platform-owner-request.v1","grant":grant,"committed_reply_base64url":null});
            value
                .as_object_mut()
                .unwrap()
                .extend(extra.as_object().unwrap().clone());
            serde_json::to_vec(&value).unwrap()
        };
        let valid = request(serde_json::json!({}));
        let selector = request(serde_json::json!({"runtime_id":"selected"}));
        let recovery =
            request(serde_json::json!({"schema":"elitea.sandbox.node-code-recovery-request.v1"}));
        let mut absent_reply: serde_json::Value = serde_json::from_slice(&valid).unwrap();
        absent_reply
            .as_object_mut()
            .unwrap()
            .remove("committed_reply_base64url");
        let absent_reply = serde_json::to_vec(&absent_reply).unwrap();
        let oversized = vec![b' '; OWNER_REQUEST_LIMIT + 1];
        let call = |key: String, operation, body: Vec<u8>| {
            let verifier = &verifier;
            let supervisor = &supervisor;
            async move {
                platform_owner(
                    verifier,
                    supervisor,
                    "dns:main.fixture",
                    &key,
                    operation,
                    &body,
                )
                .await
                .unwrap_err()
                .code()
            }
        };
        let read = CodePlatformOwnerOperation::ReadRetainedRuntime;
        for key in ["0".repeat(64), "A".repeat(64), "1".repeat(65)] {
            assert_eq!(
                call(key, read, valid.clone()).await,
                tonic::Code::InvalidArgument
            );
        }
        let key = "1".repeat(64);
        for (body, code) in [
            (selector, tonic::Code::InvalidArgument),
            (recovery, tonic::Code::InvalidArgument),
            (absent_reply, tonic::Code::InvalidArgument),
            (oversized.clone(), tonic::Code::ResourceExhausted),
            (valid.clone(), tonic::Code::PermissionDenied),
        ] {
            assert_eq!(call(key.clone(), read, body).await, code);
        }
        // Only a reply publication may carry the larger document.
        let publish = CodePlatformOwnerOperation::PublishCommittedPlatformReply;
        assert_eq!(
            call(key.clone(), publish, oversized).await,
            tonic::Code::InvalidArgument
        );
        assert_eq!(
            call(key, publish, vec![b' '; PUBLISH_REQUEST_LIMIT + 1]).await,
            tonic::Code::ResourceExhausted
        );
    }
}
