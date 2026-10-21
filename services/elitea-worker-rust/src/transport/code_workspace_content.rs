//! Original-visit-scoped repository snapshot acquisition. No caller URL or credential.
use super::{
    Code, InputContentClient, InputContentError, Status, claim_request, content_scope,
    is_transport_status, timeout,
};
use crate::{
    protocol::{
        control::ClaimBoundSandboxAuthority, elitea::runtime::v1::ResolveCodeWorkspaceRequestV1,
    },
    sandbox::{
        code_recovery::OriginalCodeVisitRef,
        request::PreparedJob,
        workspace::{WorkspaceManifest, WorkspaceMode, WorkspaceSelection},
    },
};
use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use serde::Serialize;
#[derive(Serialize)]
struct Resolve<'a> {
    original_visit: &'a OriginalCodeVisitRef,
    selected_pre_workspace_prepared_job_json_base64url: String,
}
impl InputContentClient {
    /// Main derives the saved selection from the already-admitted original declaration.
    /// Acquisition is not execution authority; a final grant binds the returned manifest.
    pub(crate) async fn resolve_code_workspace(
        &self,
        claim: &ClaimBoundSandboxAuthority,
        visit: &OriginalCodeVisitRef,
        base: &PreparedJob,
        selection: &WorkspaceSelection,
    ) -> Result<WorkspaceManifest, InputContentError> {
        if !visit.valid()
            || selection.mode != WorkspaceMode::Read
            || selection.validate_declaration().is_err()
            || base.workspace().is_some()
        {
            return Err(invalid());
        }
        let body = serde_json::to_vec(&Resolve {
            original_visit: visit,
            selected_pre_workspace_prepared_job_json_base64url: URL_SAFE_NO_PAD
                .encode(base.to_transport().map_err(|_| invalid())?),
        })
        .map_err(|_| invalid())?;
        if body.len() > 2 << 20 {
            return Err(invalid());
        }
        let (execution, generation, claim_id, fence) = claim.intent_content_binding();
        let request = claim_request(
            ResolveCodeWorkspaceRequestV1 {
                scope: Some(content_scope(execution, generation)),
                request_json: body,
            },
            claim_id,
            fence,
        )
        .ok_or_else(invalid)?;
        let mut rpc = self.rpc();
        timeout(self.config.deadline, async {
            let response = rpc
                .resolve_code_workspace(request)
                .await
                .map_err(workspace_status)?
                .into_inner();
            let root = response.workspace_root;
            if root.len() != 64
                || !root
                    .bytes()
                    .all(|c| c.is_ascii_digit() || (b'a'..=b'f').contains(&c))
                || !(1..=4 << 20).contains(&response.manifest_json.len())
            {
                return Err(invalid());
            }
            let manifest = WorkspaceManifest::from_bound_transport(&response.manifest_json, &root)
                .map_err(|_| invalid())?;
            if manifest.selection() != selection {
                return Err(invalid());
            }
            Ok(manifest)
        })
        .await
        .map_err(|_| timed_out())?
    }
}
/// A refused bound is the selection's problem, not an outage; a stale claim
/// is a refusal; Main's other answers are an acquisition that may succeed on
/// a retry of the same visit.
fn workspace_status(status: Status) -> InputContentError {
    match status.code() {
        Code::Unauthenticated | Code::PermissionDenied | Code::Aborted => invalid(),
        Code::FailedPrecondition => InputContentError::InvalidInput(
            "the saved Code repository selection exceeds operator policy or requires unsupported writable storage",
        ),
        Code::DeadlineExceeded => timed_out(),
        _ if is_transport_status(&status) => InputContentError::transport(status),
        _ => InputContentError::DependencyUnavailable("Code repository acquisition is unavailable"),
    }
}
fn timed_out() -> InputContentError {
    InputContentError::DependencyUnavailable(
        "Code repository acquisition timed out; retry the same original visit",
    )
}
fn invalid() -> InputContentError {
    InputContentError::AuthorizationFailed("the original Code repository binding is invalid")
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn acquisition_request_carries_only_original_visit_and_exact_selected_base() {
        let visit = OriginalCodeVisitRef {
            visit_id: "a".repeat(64),
            revision: 1,
            digest_sha256: "b".repeat(64),
        };
        let request = Resolve {
            original_visit: &visit,
            selected_pre_workspace_prepared_job_json_base64url: URL_SAFE_NO_PAD
                .encode(b"exact base"),
        };
        let value = serde_json::to_value(request).unwrap();
        let object = value.as_object().unwrap();
        assert_eq!(object.len(), 2);
        assert!(!object.contains_key("selection"));
        assert!(!object.contains_key("toolkit_id"));
        assert_eq!(
            URL_SAFE_NO_PAD
                .decode(
                    value["selected_pre_workspace_prepared_job_json_base64url"]
                        .as_str()
                        .unwrap()
                )
                .unwrap(),
            b"exact base"
        );
    }
}
