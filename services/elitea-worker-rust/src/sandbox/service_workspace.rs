//! One bounded import on the original signed job. Hydration never dispatches Code.
use super::{
    Ed25519PublicKeyResolver, PreparedJob, Request, Response, SandboxJobStatusV1, Status,
    SupervisorService, authenticated_peer, response, service_error,
};
use crate::{
    protocol::elitea::runtime::v1::{
        HydrateSandboxWorkspaceRequestV1, HydrateSandboxWorkspaceResponseV1,
        WorkspaceHydrationCursorV1,
    },
    sandbox::{
        compiled_snapshot::{Control, DESCRIPTOR_LIMIT, Descriptor, Purpose},
        docker_supervisor::{WorkspaceAdmission, WorkspaceReconciliation},
        workspace::WorkspaceManifest,
    },
};

enum WorkspaceWireMode {
    Plain,
    Compile,
    CachedExecute,
}
fn mode(input: &HydrateSandboxWorkspaceRequestV1) -> Result<WorkspaceWireMode, Status> {
    let intent = input.code_execution_intent_json.as_deref();
    if intent.is_some_and(|v| v.is_empty() || v.len() > 16384) {
        return Err(Status::invalid_argument(
            "The original Code intent is invalid.",
        ));
    }
    match (
        input.control_json.is_empty(),
        input.descriptor_json.is_empty(),
        input.read_grant.is_some(),
        intent.is_some(),
    ) {
        (true, true, false, true) => Ok(WorkspaceWireMode::Plain),
        (false, true, false, false) => Ok(WorkspaceWireMode::Compile),
        (false, false, true, true) => Ok(WorkspaceWireMode::CachedExecute),
        _ => Err(Status::invalid_argument(
            "Repository hydration must use one exact grant mode.",
        )),
    }
}
impl<R: Ed25519PublicKeyResolver + 'static> SupervisorService<R> {
    pub(super) async fn hydrate_workspace(
        &self,
        request: Request<HydrateSandboxWorkspaceRequestV1>,
    ) -> Result<Response<HydrateSandboxWorkspaceResponseV1>, Status> {
        let peer = authenticated_peer(&request)?;
        let input = request.into_inner();
        let selected_mode = mode(&input)?;
        let grant = input.grant.as_ref().ok_or_else(|| {
            Status::unauthenticated("A current repository job grant is required.")
        })?;
        let job = PreparedJob::from_transport(&input.prepared_job_json)
            .map_err(|_| Status::invalid_argument("The repository prepared request is invalid."))?;
        let root = &job
            .workspace()
            .ok_or_else(|| {
                Status::invalid_argument("The prepared request has no repository binding.")
            })?
            .manifest_sha256;
        let manifest =
            WorkspaceManifest::from_bound_transport(&input.workspace_manifest_json, root)
                .map_err(|_| Status::data_loss("The immutable repository manifest is invalid."))?;
        if input.file_index as usize > manifest.files().len() {
            return Err(Status::invalid_argument(
                "The repository cursor exceeds the immutable manifest.",
            ));
        }
        let content = self.content.as_deref().ok_or_else(|| {
            Status::failed_precondition("Repository content storage is unavailable.")
        })?;
        let now = chrono::Utc::now().timestamp_millis();
        let outcome = match selected_mode {
            WorkspaceWireMode::Plain => {
                let authority = self.verifier.verify(grant,&peer,&job,now)
                    .map_err(|_|Status::permission_denied("Execute authority does not bind this repository request."))?;
                let (execution,generation,dispatch) = authority.original_execution();
                let intent_json = input.code_execution_intent_json.as_deref().ok_or_else(||Status::invalid_argument("The original Code intent is required."))?;
                let intent = self.verifier.verify_code_intent(intent_json,&peer,authority.scope(),execution,generation,dispatch,&job,now)
                    .map_err(|_|Status::permission_denied("The original Code intent does not bind this repository request."))?;
                self.supervisor.hydrate_workspace_authorized(WorkspaceAdmission::Plain{job:&authority,intent:&intent},&job,&manifest,grant,Some(intent_json),input.file_index,content).await
            }
            WorkspaceWireMode::Compile => {
                let control = Control::from_bytes(&input.control_json,Purpose::Compile)
                    .map_err(|_|Status::invalid_argument("The repository Compile control is invalid."))?;
                let authority = self.verifier.verify_snapshot_compile(grant,&peer,&job,&control,now)
                    .map_err(|_|Status::permission_denied("Compile authority does not bind this repository request."))?;
                self.supervisor.hydrate_workspace_authorized(WorkspaceAdmission::Compile{job:&authority,control:&control},&job,&manifest,grant,None,input.file_index,content).await
            }
            WorkspaceWireMode::CachedExecute => {
                if input.descriptor_json.len() > DESCRIPTOR_LIMIT {
                    return Err(Status::invalid_argument("The selected repository executable descriptor exceeds its bound."));
                }
                let control = Control::from_bytes(&input.control_json,Purpose::Execute)
                    .map_err(|_|Status::invalid_argument("The repository Execute control is invalid."))?;
                let descriptor:Descriptor = serde_json::from_slice(&input.descriptor_json)
                    .map_err(|_|Status::invalid_argument("The selected repository executable descriptor is invalid."))?;
                if descriptor.bytes().map_err(|_|Status::invalid_argument("The selected descriptor is invalid."))? != input.descriptor_json {
                    return Err(Status::invalid_argument("The selected repository descriptor is not canonical."));
                }
                let authority = self.verifier.verify_snapshot_execute(grant,&peer,&job,&control,now)
                    .map_err(|_|Status::permission_denied("Execute authority does not bind the selected repository executable."))?;
                let read_grant = input.read_grant.as_ref().ok_or_else(||Status::unauthenticated("A separate pinned Read grant is required."))?;
                let read = self.verifier.verify_snapshot_read(read_grant,&peer,&job,&control,now)
                    .map_err(|_|Status::permission_denied("Read authority does not bind the selected repository executable."))?;
                let (execution,generation,dispatch) = authority.original_execution();
                let intent_json = input.code_execution_intent_json.as_deref().ok_or_else(||Status::invalid_argument("The original Code intent is required."))?;
                let intent = self.verifier.verify_code_intent(intent_json,&peer,authority.scope(),execution,generation,dispatch,&job,now)
                    .map_err(|_|Status::permission_denied("The original Code intent does not bind the selected repository executable."))?;
                self.supervisor.hydrate_workspace_authorized(WorkspaceAdmission::CachedExecute{job:&authority,read:&read,control:&control,descriptor:&descriptor,canonical:&input.descriptor_json,intent:&intent},&job,&manifest,grant,Some(intent_json),input.file_index,content).await
            }
        }.map_err(|error|service_error(&error))?;
        let output = match outcome {
            WorkspaceReconciliation::Progress(cursor) => HydrateSandboxWorkspaceResponseV1 {
                status: SandboxJobStatusV1::Pending.into(),
                cursor: Some(WorkspaceHydrationCursorV1 {
                    revision: 1,
                    manifest_sha256: hex_bytes(&cursor.manifest_sha256)?,
                    next_file_index: cursor.next_file_index,
                    file_count: cursor.file_count,
                    ready: cursor.ready,
                }),
            },
            WorkspaceReconciliation::AlreadyDispatched => HydrateSandboxWorkspaceResponseV1 {
                status: SandboxJobStatusV1::Pending.into(),
                cursor: None,
            },
            WorkspaceReconciliation::Outcome(outcome) => HydrateSandboxWorkspaceResponseV1 {
                status: response(outcome)?.status,
                cursor: None,
            },
        };
        Ok(Response::new(output))
    }
}
fn hex_bytes(value: &str) -> Result<Vec<u8>, Status> {
    if value.len() != 64
        || !value
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
    {
        return Err(Status::data_loss(
            "The repository cursor digest is invalid.",
        ));
    }
    value
        .as_bytes()
        .chunks_exact(2)
        .map(|part| {
            let text = std::str::from_utf8(part)
                .map_err(|_| Status::data_loss("The repository cursor digest is invalid."))?;
            u8::from_str_radix(text, 16)
                .map_err(|_| Status::data_loss("The repository cursor digest is invalid."))
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn mixed_intent_and_compiled_roles_refuse_before_any_runtime_work() {
        let plain = HydrateSandboxWorkspaceRequestV1 {
            code_execution_intent_json: Some(b"signed intent".to_vec()),
            ..Default::default()
        };
        assert!(matches!(mode(&plain).unwrap(), WorkspaceWireMode::Plain));
        let compile = HydrateSandboxWorkspaceRequestV1 {
            control_json: b"compile control".to_vec(),
            ..Default::default()
        };
        assert!(matches!(
            mode(&compile).unwrap(),
            WorkspaceWireMode::Compile
        ));
        let execute = HydrateSandboxWorkspaceRequestV1 {
            control_json: b"execute control".to_vec(),
            descriptor_json: b"selected descriptor".to_vec(),
            read_grant: Some(
                crate::protocol::elitea::runtime::v1::SignedSandboxJobGrantV1::default(),
            ),
            code_execution_intent_json: Some(b"signed intent".to_vec()),
            ..Default::default()
        };
        assert!(matches!(
            mode(&execute).unwrap(),
            WorkspaceWireMode::CachedExecute
        ));
        for refused in [
            HydrateSandboxWorkspaceRequestV1 {
                code_execution_intent_json: None,
                ..plain.clone()
            },
            HydrateSandboxWorkspaceRequestV1 {
                code_execution_intent_json: Some(Vec::new()),
                ..plain.clone()
            },
            HydrateSandboxWorkspaceRequestV1 {
                read_grant: Some(
                    crate::protocol::elitea::runtime::v1::SignedSandboxJobGrantV1::default(),
                ),
                ..plain
            },
            HydrateSandboxWorkspaceRequestV1 {
                code_execution_intent_json: Some(b"execute intent".to_vec()),
                ..compile.clone()
            },
            HydrateSandboxWorkspaceRequestV1 {
                read_grant: Some(
                    crate::protocol::elitea::runtime::v1::SignedSandboxJobGrantV1::default(),
                ),
                ..compile
            },
            HydrateSandboxWorkspaceRequestV1 {
                read_grant: None,
                ..execute.clone()
            },
            HydrateSandboxWorkspaceRequestV1 {
                code_execution_intent_json: None,
                ..execute
            },
        ] {
            assert!(mode(&refused).is_err());
        }
    }
}
