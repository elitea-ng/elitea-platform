//! Binary debug transfer reuses the original private mTLS context channel.
use super::{
    Code, RuntimeContextClient, RuntimeContextRedemptionBinding, Status, claim_request, scope,
    validate_binding,
};
use crate::agents::graph::{
    CodeDebugAdmission, CodeDebugArtifactReference, CodeDebugArtifactSink, CodeDebugFailure,
};
use crate::protocol::control::ClaimBoundSandboxAuthority;
use crate::protocol::elitea::runtime::v1::{AdmitCodeDebugRequestV1, CommitCodeDebugRequestV1};
use async_trait::async_trait;
#[async_trait]
impl CodeDebugArtifactSink for RuntimeContextClient {
    async fn export(
        &self,
        authority: &ClaimBoundSandboxAuthority,
        intent: &CodeDebugAdmission,
        snapshot: &[u8],
    ) -> Result<CodeDebugArtifactReference, CodeDebugFailure> {
        let binding = authority.debug_content_binding();
        let admission = serde_json::to_vec(intent).map_err(|_| CodeDebugFailure::Unavailable)?;
        self.export_debug(
            &binding,
            admission,
            &intent.original_visit.visit_id,
            snapshot,
        )
        .await
    }
}
impl RuntimeContextClient {
    /// Admit, then commit, one snapshot under the original claim.
    async fn export_debug(
        &self,
        binding: &RuntimeContextRedemptionBinding<'_>,
        admission: Vec<u8>,
        visit_id: &str,
        snapshot: &[u8],
    ) -> Result<CodeDebugArtifactReference, CodeDebugFailure> {
        validate_binding(binding).map_err(|_| CodeDebugFailure::Denied)?;
        if admission.len() > 3 * 1024 * 1024 || snapshot.len() > 3 * 1024 * 1024 {
            return Err(CodeDebugFailure::Unavailable);
        }
        let mut rpc = self.rpc();
        let request = AdmitCodeDebugRequestV1 {
            scope: Some(scope(binding)),
            admission_json: admission,
        };
        rpc.admit_code_debug(debug_request(binding, request)?)
            .await
            .map_err(|status| debug_failure(&status))?;
        let request = CommitCodeDebugRequestV1 {
            scope: Some(scope(binding)),
            visit_id: visit_id.to_owned(),
            snapshot_json: snapshot.to_vec(),
        };
        let reference = rpc
            .commit_code_debug(debug_request(binding, request)?)
            .await
            .map_err(|status| debug_failure(&status))?
            .into_inner()
            .reference_json;
        if reference.is_empty() || reference.len() > 4096 {
            return Err(CodeDebugFailure::Unavailable);
        }
        serde_json::from_slice(&reference).map_err(|_| CodeDebugFailure::Unavailable)
    }
}
/// A refused claim, a missing visit or a refused snapshot is a denial; every
/// other failure (overload included) is an export that may succeed later.
fn debug_failure(status: &Status) -> CodeDebugFailure {
    if matches!(
        status.code(),
        Code::PermissionDenied | Code::Unauthenticated | Code::NotFound | Code::FailedPrecondition
    ) {
        CodeDebugFailure::Denied
    } else {
        CodeDebugFailure::Unavailable
    }
}
fn debug_request<T>(
    binding: &RuntimeContextRedemptionBinding<'_>,
    message: T,
) -> Result<tonic::Request<T>, CodeDebugFailure> {
    claim_request(message, binding.claim_id, binding.fence_token).ok_or(CodeDebugFailure::Denied)
}

#[cfg(test)]
mod tests {
    use super::super::{RuntimeContextConfig, content_scope};
    use super::*;
    use crate::protocol::elitea::runtime::v1::{
        AdmitCodeDebugResponseV1, CommitCodeDebugResponseV1,
    };
    use crate::transport::input_content::content_fake::{ContentFake, claim_of};
    use std::sync::{Arc, Mutex};
    use std::time::Duration;

    #[derive(Default)]
    struct DebugListener {
        admitted: Mutex<Option<Vec<u8>>>,
        committed: Mutex<Option<(String, Vec<u8>)>>,
        refuse_admission: Option<Code>,
    }
    #[tonic::async_trait]
    impl ContentFake for Arc<DebugListener> {
        async fn admit_code_debug(
            &self,
            request: tonic::Request<AdmitCodeDebugRequestV1>,
        ) -> Result<tonic::Response<AdmitCodeDebugResponseV1>, Status> {
            assert_eq!(claim_of(&request), ("claim-1".into(), vec![5; 32]));
            let request = request.into_inner();
            assert_eq!(request.scope, Some(content_scope("execution-one", 3)));
            *self.admitted.lock().unwrap() = Some(request.admission_json);
            match self.refuse_admission {
                Some(code) => Err(Status::new(code, "refused")),
                None => Ok(tonic::Response::new(AdmitCodeDebugResponseV1 {})),
            }
        }
        async fn commit_code_debug(
            &self,
            request: tonic::Request<CommitCodeDebugRequestV1>,
        ) -> Result<tonic::Response<CommitCodeDebugResponseV1>, Status> {
            assert_eq!(claim_of(&request), ("claim-1".into(), vec![5; 32]));
            let request = request.into_inner();
            *self.committed.lock().unwrap() = Some((request.visit_id, request.snapshot_json));
            Ok(tonic::Response::new(CommitCodeDebugResponseV1 {
                reference_json: serde_json::to_vec(&reference()).unwrap(),
            }))
        }
    }
    fn reference() -> CodeDebugArtifactReference {
        CodeDebugArtifactReference {
            schema_version: "elitea.runtime.code-debug-artifact.v1".into(),
            project_id: 7,
            bucket: "debug".into(),
            name: "visit-1.json".into(),
            media_type: "application/json".into(),
            byte_length: 22,
            sha256: "a".repeat(64),
        }
    }
    fn client(listener: Arc<DebugListener>) -> RuntimeContextClient {
        RuntimeContextClient::with_rpc(
            listener,
            RuntimeContextConfig {
                origin: "https://content.internal".into(),
                deadline: Duration::from_secs(1),
                max_response_bytes: 4096,
                max_application_response_bytes: 4096,
                max_attachment_response_bytes: 4096,
                max_artifact_response_bytes: 4096,
            },
        )
        .unwrap()
    }
    fn binding(fence: &[u8; 32]) -> RuntimeContextRedemptionBinding<'_> {
        RuntimeContextRedemptionBinding {
            execution_id: "execution-one",
            generation: 3,
            claim_id: "claim-1",
            fence_token: fence,
            resource_project_id: "7",
        }
    }
    #[tokio::test]
    async fn binary_documents_keep_exact_bytes_under_the_original_claim() {
        let listener = Arc::new(DebugListener::default());
        let admission =
            br#"{"source":"line\r\n","selected_input":{"large":9007199254740993}}"#.to_vec();
        let snapshot = b"{\"frames\":[\"\\u0000\"]}".to_vec();
        let exported = client(Arc::clone(&listener))
            .export_debug(&binding(&[5; 32]), admission.clone(), "visit-1", &snapshot)
            .await;
        assert_eq!(exported, Ok(reference()));
        assert_eq!(
            listener.admitted.lock().unwrap().as_deref(),
            Some(&admission[..])
        );
        assert_eq!(
            listener.committed.lock().unwrap().as_ref(),
            Some(&("visit-1".to_owned(), snapshot))
        );
    }
    #[tokio::test]
    async fn a_refused_admission_commits_nothing() {
        for (code, expected) in [
            (Code::PermissionDenied, CodeDebugFailure::Denied),
            (Code::FailedPrecondition, CodeDebugFailure::Denied),
            (Code::Unavailable, CodeDebugFailure::Unavailable),
        ] {
            let listener = Arc::new(DebugListener {
                refuse_admission: Some(code),
                ..DebugListener::default()
            });
            let outcome = client(Arc::clone(&listener))
                .export_debug(&binding(&[5; 32]), b"{}".to_vec(), "visit-1", b"{}")
                .await;
            assert_eq!(outcome.err(), Some(expected), "{code:?}");
            assert!(listener.committed.lock().unwrap().is_none());
        }
    }
    #[test]
    fn denied_statuses_do_not_become_artifact_references() {
        for code in [
            Code::PermissionDenied,
            Code::Unauthenticated,
            Code::NotFound,
            Code::FailedPrecondition,
        ] {
            assert_eq!(
                debug_failure(&Status::new(code, "")),
                CodeDebugFailure::Denied
            );
        }
        for code in [Code::Unavailable, Code::Internal, Code::ResourceExhausted] {
            assert_eq!(
                debug_failure(&Status::new(code, "")),
                CodeDebugFailure::Unavailable
            );
        }
    }
}
