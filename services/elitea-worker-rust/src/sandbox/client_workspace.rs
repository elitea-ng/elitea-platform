//! One indexed repository RPC. The caller renews actual owning grants between calls.
use super::{SandboxCallError, SandboxClient, submission_code};
use crate::{
    protocol::elitea::runtime::v1::{
        HydrateSandboxWorkspaceRequestV1, HydrateSandboxWorkspaceResponseV1, SandboxJobStatusV1,
        SignedSandboxJobGrantV1,
    },
    sandbox::{
        compiled_snapshot::{Control, Descriptor, Purpose},
        request::PreparedJob,
        workspace::WorkspaceManifest,
    },
};
use tonic::{Code, Request};

pub(crate) enum WorkspaceHydrationMode<'a> {
    Plain {
        intent: &'a [u8],
    },
    Compile {
        control: &'a Control,
    },
    CachedExecute {
        control: &'a Control,
        descriptor: &'a Descriptor,
        canonical: &'a [u8],
        read_grant: &'a SignedSandboxJobGrantV1,
        intent: &'a [u8],
    },
}
pub(crate) enum WorkspaceHydrationOutcome {
    Progress { next_file_index: u32, ready: bool },
    // No cursor permits no import or replay. Observe the exact original job.
    ObserveOriginal,
}
impl SandboxClient {
    pub(crate) async fn hydrate_workspace(
        &self,
        grant: &SignedSandboxJobGrantV1,
        job: &PreparedJob,
        manifest: &WorkspaceManifest,
        mode: WorkspaceHydrationMode<'_>,
        file_index: u32,
    ) -> Result<WorkspaceHydrationOutcome, SandboxCallError> {
        if !job
            .workspace()
            .is_some_and(|binding| binding.matches(manifest).unwrap_or(false))
            || file_index as usize > manifest.files().len()
        {
            return Err(SandboxCallError::Invalid);
        }
        let mut input = HydrateSandboxWorkspaceRequestV1 {
            grant: Some(grant.clone()),
            prepared_job_json: job.to_transport().map_err(|_| SandboxCallError::Invalid)?,
            workspace_manifest_json: manifest
                .to_transport()
                .map_err(|_| SandboxCallError::Invalid)?,
            file_index,
            ..Default::default()
        };
        match mode {
            WorkspaceHydrationMode::Plain { intent } => {
                input.code_execution_intent_json = Some(intent_bytes(intent)?);
            }
            WorkspaceHydrationMode::Compile { control } => {
                control
                    .binding
                    .validate_job(job)
                    .map_err(|_| SandboxCallError::Invalid)?;
                input.control_json = control
                    .bytes(Purpose::Compile)
                    .map_err(|_| SandboxCallError::Invalid)?;
            }
            WorkspaceHydrationMode::CachedExecute {
                control,
                descriptor,
                canonical,
                read_grant,
                intent,
            } => {
                control
                    .binding
                    .validate_job(job)
                    .map_err(|_| SandboxCallError::Invalid)?;
                descriptor
                    .validate(control)
                    .map_err(|_| SandboxCallError::Invalid)?;
                if descriptor.bytes().map_err(|_| SandboxCallError::Invalid)? != canonical {
                    return Err(SandboxCallError::Invalid);
                }
                input.control_json = control
                    .bytes(Purpose::Execute)
                    .map_err(|_| SandboxCallError::Invalid)?;
                input.descriptor_json = canonical.to_vec();
                input.read_grant = Some(read_grant.clone());
                input.code_execution_intent_json = Some(intent_bytes(intent)?);
            }
        }
        let mut request = Request::new(input);
        request.set_timeout(self.deadline);
        let output = tokio::time::timeout(
            self.deadline,
            self.rpc.clone().hydrate_sandbox_workspace(request),
        )
        .await
        .map_err(|_| SandboxCallError::Submission {
            code: Code::DeadlineExceeded,
        })?
        .map_err(|status| SandboxCallError::Submission {
            code: submission_code(&status),
        })?
        .into_inner();
        decode_workspace_cursor(output, manifest, file_index)
    }
}
fn intent_bytes(value: &[u8]) -> Result<Vec<u8>, SandboxCallError> {
    if value.is_empty() || value.len() > 16384 {
        return Err(SandboxCallError::Invalid);
    }
    Ok(value.to_vec())
}
fn decode_workspace_cursor(
    response: HydrateSandboxWorkspaceResponseV1,
    manifest: &WorkspaceManifest,
    requested: u32,
) -> Result<WorkspaceHydrationOutcome, SandboxCallError> {
    let status = SandboxJobStatusV1::try_from(response.status)
        .map_err(|_| SandboxCallError::InvalidReceipt)?;
    let Some(cursor) = response.cursor else {
        return match status {
            SandboxJobStatusV1::Pending
            | SandboxJobStatusV1::Completed
            | SandboxJobStatusV1::Failed
            | SandboxJobStatusV1::Cancelled
            | SandboxJobStatusV1::Uncertain => Ok(WorkspaceHydrationOutcome::ObserveOriginal),
            SandboxJobStatusV1::Unspecified => Err(SandboxCallError::InvalidReceipt),
        };
    };
    let count =
        u32::try_from(manifest.files().len()).map_err(|_| SandboxCallError::InvalidReceipt)?;
    if status != SandboxJobStatusV1::Pending
        || cursor.revision != 1
        || cursor.manifest_sha256.len() != 32
        || crate::sandbox::code_recovery::hex(&cursor.manifest_sha256) != manifest.root()
        || cursor.file_count != count
        || cursor.next_file_index > count
        || cursor.next_file_index < requested
        || cursor.ready && cursor.next_file_index != count
    {
        return Err(SandboxCallError::InvalidReceipt);
    }
    Ok(WorkspaceHydrationOutcome::Progress {
        next_file_index: cursor.next_file_index,
        ready: cursor.ready,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::protocol::elitea::runtime::v1::WorkspaceHydrationCursorV1;
    fn manifest() -> WorkspaceManifest {
        let raw = include_bytes!(
            "../../../../libs/proto/elitea/runtime/v1/code_workspace_manifest_v1.json"
        );
        let root = serde_json::from_slice::<serde_json::Value>(raw).unwrap()["root"]
            .as_str()
            .unwrap()
            .to_owned();
        WorkspaceManifest::from_bound_transport(raw, &root).unwrap()
    }
    fn response() -> HydrateSandboxWorkspaceResponseV1 {
        let manifest = manifest();
        let root = manifest
            .root()
            .as_bytes()
            .chunks_exact(2)
            .map(|part| u8::from_str_radix(std::str::from_utf8(part).unwrap(), 16).unwrap())
            .collect();
        HydrateSandboxWorkspaceResponseV1 {
            status: SandboxJobStatusV1::Pending.into(),
            cursor: Some(WorkspaceHydrationCursorV1 {
                revision: 1,
                manifest_sha256: root,
                next_file_index: 1,
                file_count: 1,
                ready: false,
            }),
        }
    }
    #[test]
    fn confirmed_last_batch_still_requires_explicit_finalization_and_original_observation() {
        let manifest = manifest();
        assert!(matches!(
            decode_workspace_cursor(response(), &manifest, 0).unwrap(),
            WorkspaceHydrationOutcome::Progress {
                next_file_index: 1,
                ready: false
            }
        ));
        let mut finalization = response();
        finalization.cursor.as_mut().unwrap().ready = true;
        assert!(matches!(
            decode_workspace_cursor(finalization, &manifest, 1).unwrap(),
            WorkspaceHydrationOutcome::Progress {
                next_file_index: 1,
                ready: true
            }
        ));
        let observed = HydrateSandboxWorkspaceResponseV1 {
            status: SandboxJobStatusV1::Pending.into(),
            cursor: None,
        };
        assert!(matches!(
            decode_workspace_cursor(observed, &manifest, 1).unwrap(),
            WorkspaceHydrationOutcome::ObserveOriginal
        ));
    }
    #[test]
    fn changed_manifest_future_progress_or_ready_prefix_is_not_an_acknowledgement() {
        let manifest = manifest();
        for kind in [
            "root",
            "count",
            "future",
            "ready_prefix",
            "revision",
            "terminal_cursor",
            "backward",
        ] {
            let mut output = response();
            let cursor = output.cursor.as_mut().unwrap();
            match kind {
                "root" => cursor.manifest_sha256[0] ^= 1,
                "count" => cursor.file_count = 2,
                "future" => cursor.next_file_index = 2,
                "ready_prefix" => {
                    cursor.next_file_index = 0;
                    cursor.ready = true;
                }
                "revision" => cursor.revision = 2,
                "terminal_cursor" => output.status = SandboxJobStatusV1::Completed.into(),
                "backward" => cursor.next_file_index = 0,
                _ => unreachable!(),
            }
            assert!(
                decode_workspace_cursor(output, &manifest, u32::from(kind == "backward")).is_err(),
                "accepted {kind}"
            );
        }
    }
}
