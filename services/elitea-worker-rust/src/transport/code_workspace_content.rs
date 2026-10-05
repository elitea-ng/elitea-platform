//! Original-visit-scoped repository snapshot acquisition. No caller URL or credential.
use super::{
    Body, CLAIM_HEADER, CONTENT_DIGEST_HEADER, CONTENT_LENGTH, CONTENT_TYPE, FENCE_HEADER,
    InputContentClient, InputContentError, Method, PATH_SEGMENT, Request, StatusCode, Version,
    sha256_header, timeout, utf8_percent_encode,
};
use crate::{
    protocol::control::ClaimBoundSandboxAuthority,
    sandbox::{
        code_recovery::OriginalCodeVisitRef,
        request::PreparedJob,
        workspace::{WorkspaceManifest, WorkspaceMode, WorkspaceSelection},
    },
};
use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use http_body_util::BodyExt as _;
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
        let uri = format!(
            "{}/executions/{}/generations/{generation}/runtime-context/code-workspace",
            self.config.origin,
            utf8_percent_encode(execution, PATH_SEGMENT)
        );
        let request = Request::builder()
            .method(Method::POST)
            .version(Version::HTTP_2)
            .uri(uri)
            .header(CLAIM_HEADER, claim_id)
            .header(FENCE_HEADER, URL_SAFE_NO_PAD.encode(fence))
            .header(CONTENT_TYPE, "application/json")
            .header(CONTENT_LENGTH, body.len())
            .body(Body::new(http_body_util::Full::new(bytes::Bytes::from(
                body,
            ))))
            .map_err(|_| invalid())?;
        timeout(self.config.deadline,async {
            let mut response=self.rpc.get(request).await.map_err(InputContentError::Transport)?;
            if matches!(response.status(),StatusCode::UNAUTHORIZED|StatusCode::FORBIDDEN|StatusCode::CONFLICT){return Err(invalid())}
            if response.status()==StatusCode::UNPROCESSABLE_ENTITY {return Err(InputContentError::InvalidInput("the saved Code repository selection exceeds operator policy or requires unsupported writable storage"))}
            if response.status()!=StatusCode::OK||response.version()!=Version::HTTP_2||response.headers().get(CONTENT_TYPE).and_then(|v|v.to_str().ok())!=Some("application/json"){return Err(InputContentError::DependencyUnavailable("Code repository acquisition is unavailable"))}
            let root=response.headers().get("x-elitea-workspace-root").and_then(|v|v.to_str().ok()).filter(|v|v.len()==64&&v.bytes().all(|c|c.is_ascii_digit()||(b'a'..=b'f').contains(&c))).ok_or_else(invalid)?.to_owned();
            let digest=sha256_header(response.headers(),&CONTENT_DIGEST_HEADER)?;
            let bytes_expected=response.headers().get(CONTENT_LENGTH).and_then(|v|v.to_str().ok()).and_then(|v|v.parse::<usize>().ok()).filter(|v|(1..=4<<20).contains(v)).ok_or_else(invalid)?;
            let mut bytes=Vec::new();
            while let Some(frame)=response.body_mut().frame().await {
                let frame=frame.map_err(|_|InputContentError::DependencyUnavailable("Code repository response was interrupted"))?;
                if frame.is_trailers(){return Err(invalid())}
                if let Ok(data)=frame.into_data(){if bytes.len().checked_add(data.len()).is_none_or(|v|v>bytes_expected){return Err(invalid())}bytes.extend_from_slice(&data)}
            }
            if bytes.len()!=bytes_expected||ring::digest::digest(&ring::digest::SHA256,&bytes).as_ref()!=digest{return Err(invalid())}
            let manifest=WorkspaceManifest::from_bound_transport(&bytes,&root).map_err(|_|invalid())?;
            if manifest.selection()!=selection{return Err(invalid())}
            Ok(manifest)
        }).await.map_err(|_|InputContentError::DependencyUnavailable("Code repository acquisition timed out; retry the same original visit"))?
    }
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
