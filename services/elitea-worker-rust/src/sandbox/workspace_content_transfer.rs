//! Supervisor data read. Main joins actual signed mode and current original visit.
use super::DependencyContentClient;
use crate::{
    protocol::elitea::runtime::v1::{
        ReadSandboxWorkspaceFileRequestV1, ReadSandboxWorkspaceManifestRequestV1,
        SignedSandboxJobGrantV1,
    },
    sandbox::{request::PreparedJob, workspace::WorkspaceManifest},
};
use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use prost::Message as _;
use ring::digest;
use serde::Serialize;
use tonic::{Code, Status};

#[derive(Debug, thiserror::Error)]
pub enum WorkspaceContentError {
    #[error(
        "Code repository content authority is invalid or expired; retry the same original visit"
    )]
    Authority,
    #[error("Code repository content differs from the immutable snapshot; execution remains inert")]
    Integrity,
    #[error("Code repository content transfer is at capacity; retry the same immutable snapshot")]
    Busy,
    #[error(
        "Code repository content transfer was interrupted; reconcile the same original runtime"
    )]
    Unavailable,
}
pub(crate) struct WorkspaceReadProof<'a> {
    pub grant: &'a SignedSandboxJobGrantV1,
    pub prepared: &'a PreparedJob,
    pub intent: Option<&'a [u8]>,
}
#[derive(Serialize)]
struct ReadRequest {
    schema: &'static str,
    grant_base64url: String,
    prepared_job_json_base64url: String,
    code_execution_intent_json_base64url: Option<String>,
}
fn body(proof: &WorkspaceReadProof<'_>) -> Result<Vec<u8>, WorkspaceContentError> {
    let raw = proof
        .prepared
        .to_transport()
        .map_err(|_| WorkspaceContentError::Integrity)?;
    let grant = proof.grant.encode_to_vec();
    if raw.len() > 1 << 20
        || grant.len() > 8192
        || proof
            .intent
            .is_some_and(|v| v.is_empty() || v.len() > 16384)
    {
        return Err(WorkspaceContentError::Authority);
    }
    let bytes = serde_json::to_vec(&ReadRequest {
        schema: "elitea.sandbox.workspace-content-read.v1",
        grant_base64url: URL_SAFE_NO_PAD.encode(grant),
        prepared_job_json_base64url: URL_SAFE_NO_PAD.encode(raw),
        code_execution_intent_json_base64url: proof
            .intent
            .map(|value| URL_SAFE_NO_PAD.encode(value)),
    })
    .map_err(|_| WorkspaceContentError::Integrity)?;
    if bytes.len() > 2 << 20 {
        return Err(WorkspaceContentError::Integrity);
    }
    Ok(bytes)
}
impl DependencyContentClient {
    pub(crate) async fn download_workspace_manifest(
        &self,
        manifest: &WorkspaceManifest,
        proof: &WorkspaceReadProof<'_>,
    ) -> Result<(), WorkspaceContentError> {
        if !proof
            .prepared
            .workspace()
            .ok_or(WorkspaceContentError::Authority)?
            .matches(manifest)
            .map_err(|_| WorkspaceContentError::Integrity)?
        {
            return Err(WorkspaceContentError::Authority);
        }
        let expected = manifest
            .to_transport()
            .map_err(|_| WorkspaceContentError::Integrity)?;
        let _slot = self
            .slots
            .try_acquire()
            .map_err(|_| WorkspaceContentError::Busy)?;
        let operation = async {
            let bytes = self
                .rpc()
                .read_sandbox_workspace_manifest(ReadSandboxWorkspaceManifestRequestV1 {
                    manifest_sha256: manifest.root().to_owned(),
                    request_json: body(proof)?,
                })
                .await
                .map_err(|status| refusal(&status))?
                .into_inner()
                .manifest_json;
            if bytes != expected {
                return Err(WorkspaceContentError::Integrity);
            }
            Ok(())
        };
        tokio::time::timeout(self.deadline, operation)
            .await
            .map_err(|_| WorkspaceContentError::Unavailable)?
    }
    pub(crate) async fn download_workspace_file(
        &self,
        manifest: &WorkspaceManifest,
        index: usize,
        proof: &WorkspaceReadProof<'_>,
    ) -> Result<Vec<u8>, WorkspaceContentError> {
        let file = manifest
            .files()
            .get(index)
            .ok_or(WorkspaceContentError::Integrity)?;
        let binding = proof
            .prepared
            .workspace()
            .ok_or(WorkspaceContentError::Authority)?;
        if !binding
            .matches(manifest)
            .map_err(|_| WorkspaceContentError::Integrity)?
        {
            return Err(WorkspaceContentError::Authority);
        }
        let _slot = self
            .slots
            .try_acquire()
            .map_err(|_| WorkspaceContentError::Busy)?;
        let request = ReadSandboxWorkspaceFileRequestV1 {
            manifest_sha256: manifest.root().to_owned(),
            content_sha256: file.sha256.clone(),
            request_json: body(proof)?,
        };
        let operation = async {
            let bytes = self
                .rpc()
                .read_sandbox_workspace_file(request)
                .await
                .map_err(|status| refusal(&status))?
                .into_inner()
                .content;
            if bytes.len() as u64 != file.bytes
                || digest::digest(&digest::SHA256, &bytes).as_ref() != hex_bytes(&file.sha256)?
            {
                return Err(WorkspaceContentError::Integrity);
            }
            Ok(bytes)
        };
        tokio::time::timeout(self.deadline, operation)
            .await
            .map_err(|_| WorkspaceContentError::Unavailable)?
    }
}
/// Main's refusal of the caller's authority (including a superseded visit)
/// is an authority failure; anything else interrupted the transfer.
fn refusal(status: &Status) -> WorkspaceContentError {
    match status.code() {
        Code::Unauthenticated | Code::PermissionDenied | Code::Aborted => {
            WorkspaceContentError::Authority
        }
        _ => WorkspaceContentError::Unavailable,
    }
}
fn hex_bytes(value: &str) -> Result<[u8; 32], WorkspaceContentError> {
    if value.len() != 64 {
        return Err(WorkspaceContentError::Integrity);
    }
    let mut out = [0u8; 32];
    for (i, chunk) in value.as_bytes().as_chunks::<2>().0.iter().enumerate() {
        out[i] = u8::from_str_radix(
            std::str::from_utf8(chunk).map_err(|_| WorkspaceContentError::Integrity)?,
            16,
        )
        .map_err(|_| WorkspaceContentError::Integrity)?;
    }
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn workspace_read_body_is_mode_specific_and_never_contains_source_paths_or_unsigned_scope() {
        let prepared = PreparedJob::from_transport(include_bytes!(
            "../../../../libs/proto/elitea/runtime/v1/code_prepared_workspace_only_v4.json"
        ))
        .unwrap();
        let grant = SignedSandboxJobGrantV1 {
            key_id: "owner".into(),
            claims_bytes: vec![1],
            signature: vec![2; 64],
        };
        let encoded = body(&WorkspaceReadProof {
            grant: &grant,
            prepared: &prepared,
            intent: None,
        })
        .unwrap();
        let value: serde_json::Value = serde_json::from_slice(&encoded).unwrap();
        assert_eq!(value.as_object().unwrap().len(), 4);
        assert!(value["code_execution_intent_json_base64url"].is_null());
        assert!(!value.as_object().unwrap().contains_key("original_visit"));
        assert_eq!(
            URL_SAFE_NO_PAD
                .decode(value["prepared_job_json_base64url"].as_str().unwrap())
                .unwrap(),
            prepared.to_transport().unwrap()
        );
    }
}
