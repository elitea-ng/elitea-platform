//! Owner RPCs on the original mandatory mTLS Supervisor listener (#1359 U4).
//! Main reads or seals a whole-Code job it owns; none of them can Submit.
use super::{DockerSupervisor, Ed25519PublicKeyResolver, GrantVerifier};
use crate::protocol::sandbox_grant::{CodeOwnerOperation, SignedCodeEnvelope};
use serde::Deserialize;
use std::time::Duration;
use tonic::Status;

#[path = "service_code_platform.rs"]
mod platform;
pub(super) use platform::platform_owner;
/// The request document limit for every owner RPC except a reply publication.
const OWNER_REQUEST_LIMIT: usize = 16 * 1024;
/// The bound on the ledger read or seal behind one owner RPC.
const OWNER_DEADLINE: Duration = Duration::from_secs(10);
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct OwnerRequest {
    schema: String,
    grant: SignedCodeEnvelope,
}
/// Answer one node-code recovery owner RPC for the authenticated `peer`.
/// Refusals carry a fixed message and never any ledger content.
pub(super) async fn recovery_owner<R: Ed25519PublicKeyResolver>(
    verifier: &GrantVerifier<R>,
    supervisor: &DockerSupervisor,
    peer: &str,
    job_key: &str,
    operation: CodeOwnerOperation,
    request_json: &[u8],
) -> Result<Vec<u8>, Status> {
    valid_job_key(job_key)?;
    let input: OwnerRequest = owner_request(request_json, OWNER_REQUEST_LIMIT)?;
    if input.schema != "elitea.sandbox.node-code-recovery-request.v1" {
        return Err(malformed());
    }
    let authority = verifier
        .verify_code_recovery(
            &input.grant,
            peer,
            job_key,
            operation,
            chrono::Utc::now().timestamp_millis(),
        )
        .map_err(|_| refused())?;
    // This method calls only JobLedger's immutable read or the special no-effect seal.
    let result = tokio::time::timeout(OWNER_DEADLINE, supervisor.observe_code_owner(&authority))
        .await
        .map_err(|_| expired())?
        .map_err(|_| conflict())?;
    result.canonical_bytes().map_err(|_| conflict())
}
fn valid_job_key(job_key: &str) -> Result<(), Status> {
    if crate::sandbox::code_recovery::hex_id(job_key, 64, true) {
        Ok(())
    } else {
        Err(malformed())
    }
}
fn owner_request<T: serde::de::DeserializeOwned>(
    request_json: &[u8],
    limit: usize,
) -> Result<T, Status> {
    if request_json.len() > limit {
        return Err(Status::resource_exhausted(
            "The owner request exceeds its size limit.",
        ));
    }
    serde_json::from_slice(request_json).map_err(|_| malformed())
}
fn malformed() -> Status {
    Status::invalid_argument("The owner request is malformed.")
}
fn refused() -> Status {
    Status::permission_denied("The owner grant does not authorize this job.")
}
fn expired() -> Status {
    Status::deadline_exceeded("The owner request timed out.")
}
fn conflict() -> Status {
    Status::aborted("The job cannot be observed for this owner grant.")
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
    pub(super) struct NoRuntime;
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
    /// A supervisor whose ledger never connects and whose runtime panics on use.
    pub(super) fn no_io_supervisor() -> std::sync::Arc<DockerSupervisor> {
        let pool = crate::test_pg::pool_options()
            .max_connections(1)
            .acquire_timeout(Duration::from_millis(50))
            .connect_lazy("postgres://fixture:fixture@127.0.0.1:1/never_connect")
            .unwrap();
        std::sync::Arc::new(
            DockerSupervisor::new(
                crate::sandbox::ledger::JobLedger::new(pool),
                NoRuntime,
                "fixture-owner".into(),
                1,
            )
            .unwrap(),
        )
    }
    pub(super) fn no_io_owner_service_fixture()
    -> (GrantVerifier<NoKeys>, std::sync::Arc<DockerSupervisor>) {
        let verifier = GrantVerifier::new(NoKeys, "dns:supervisor.fixture".into())
            .unwrap()
            .with_code_owner_requester("dns:main.fixture".into())
            .unwrap();
        (verifier, no_io_supervisor())
    }
    fn grant() -> serde_json::Value {
        serde_json::json!({"schema":"elitea.sandbox.node-code-recovery-signed-grant.v1","key_id":"fixture-key","claims_base64url":"e30","signature_base64url":"AA"})
    }
    #[tokio::test]
    async fn owner_requests_refuse_malformed_keys_and_bodies_before_authority_or_ledger() {
        let (verifier, supervisor) = no_io_owner_service_fixture();
        let key = "1".repeat(64);
        let valid = serde_json::to_vec(&serde_json::json!({
            "schema": "elitea.sandbox.node-code-recovery-request.v1",
            "grant": grant(),
        }))
        .unwrap();
        // Encoded identities, uppercase, all-zero and short keys never parse.
        for job_key in [
            "0".repeat(64),
            "A".repeat(64),
            "1".repeat(63),
            format!("{}/read", "1".repeat(59)),
            "%31".repeat(21),
        ] {
            let status = recovery_owner(
                &verifier,
                &supervisor,
                "dns:main.fixture",
                &job_key,
                CodeOwnerOperation::Read,
                &valid,
            )
            .await
            .unwrap_err();
            assert_eq!(status.code(), tonic::Code::InvalidArgument, "{job_key}");
        }
        let oversized = vec![b' '; OWNER_REQUEST_LIMIT + 1];
        let wrong_schema = serde_json::to_vec(&serde_json::json!({
            "schema": "elitea.sandbox.code-platform-owner-request.v1",
            "grant": grant(),
        }))
        .unwrap();
        let unknown_field = serde_json::to_vec(&serde_json::json!({
            "schema": "elitea.sandbox.node-code-recovery-request.v1",
            "grant": grant(),
            "runtime_id": "selected",
        }))
        .unwrap();
        for (body, code) in [
            (oversized.as_slice(), tonic::Code::ResourceExhausted),
            (b"not json".as_slice(), tonic::Code::InvalidArgument),
            (wrong_schema.as_slice(), tonic::Code::InvalidArgument),
            (unknown_field.as_slice(), tonic::Code::InvalidArgument),
            // Well-formed but unsigned: refused by authority, never the ledger.
            (valid.as_slice(), tonic::Code::PermissionDenied),
        ] {
            for operation in [CodeOwnerOperation::Read, CodeOwnerOperation::SealNoEffect] {
                let status = recovery_owner(
                    &verifier,
                    &supervisor,
                    "dns:main.fixture",
                    &key,
                    operation,
                    body,
                )
                .await
                .unwrap_err();
                assert_eq!(status.code(), code);
            }
        }
    }
}
#[cfg(test)]
#[path = "service_code_owner_tls_tests.rs"]
mod tls_tests;
