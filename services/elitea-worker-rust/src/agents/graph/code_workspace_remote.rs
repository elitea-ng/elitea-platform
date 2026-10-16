//! Original-visit acquisition and exact-job hydration. No project, URL, or credentials come from Code.
use super::{CodeRuntimeProfile, RemoteCodeRuntime, observation_deadline};
use crate::agents::graph::code_timing;
use crate::{
    agents::graph::code_runtime::CodeInvocation,
    sandbox::{
        client::{SandboxCallError, WorkspaceHydrationOutcome},
        code_recovery::OriginalCodeVisitRef,
        compiled_snapshot::{Control, Purpose, SelectedSnapshot},
        request::PreparedJob,
        workspace::{WorkspaceManifest, WorkspaceMode},
    },
};
#[path = "code_workspace_failure.rs"]
mod failure;
pub(super) use failure::CodeWorkspaceFailure;

impl RemoteCodeRuntime {
    pub(super) async fn acquire_execution_workspace(
        &self,
        invocation: &CodeInvocation<'_>,
        original_visit: &OriginalCodeVisitRef,
        base: PreparedJob,
    ) -> Result<(PreparedJob, Option<WorkspaceManifest>), CodeWorkspaceFailure> {
        let Some(workspace) = &invocation.workspace else {
            return Ok((base, None));
        };
        if workspace.selection.mode != WorkspaceMode::Read {
            return Err(CodeWorkspaceFailure::invalid_configuration());
        }
        let content = self
            .intent_content
            .as_deref()
            .ok_or_else(CodeWorkspaceFailure::invalid_configuration)?;
        let manifest = content
            .resolve_code_workspace(&self.authority, original_visit, &base, &workspace.selection)
            .await
            .map_err(|error| CodeWorkspaceFailure::input(&error))?;
        let binding = manifest
            .binding()
            .map_err(|_| CodeWorkspaceFailure::invalid_input())?;
        let job = base
            .with_workspace(binding)
            .map_err(|_| CodeWorkspaceFailure::invalid_input())?;
        Ok((job, Some(manifest)))
    }

    pub(super) async fn hydrate_execution_workspace(
        &self,
        invocation: &CodeInvocation<'_>,
        profile: &CodeRuntimeProfile,
        job: &PreparedJob,
        manifest: &WorkspaceManifest,
        selected: Option<&SelectedSnapshot>,
        original_visit: &OriginalCodeVisitRef,
    ) -> Result<(), CodeWorkspaceFailure> {
        let content = self
            .intent_content
            .as_deref()
            .ok_or_else(CodeWorkspaceFailure::invalid_configuration)?;
        let digest = match selected {
            Some(selected) => selected
                .control
                .intent_digest(Purpose::Execute)
                .map_err(|_| CodeWorkspaceFailure::invalid_input())?,
            None => job
                .fingerprint()
                .map_err(|_| CodeWorkspaceFailure::invalid_input())?,
        };
        let deadline = observation_deadline(profile.timeout_seconds);
        let mut index = 0;
        loop {
            let intent = tokio::time::timeout_at(
                deadline,
                content.finalize_original_code_intent(
                    &self.authority,
                    original_visit,
                    invocation.activation,
                    digest,
                    profile.client.audience(),
                    job,
                    selected.map(|value| &value.control.binding),
                    selected
                        .and_then(|value| value.control.descriptor_sha256.as_ref())
                        .map(crate::sandbox::compiled_snapshot::ContentSha256::as_str),
                ),
            )
            .await
            .map_err(|_| CodeWorkspaceFailure::deadline())?
            .map_err(|error| CodeWorkspaceFailure::input(&error))?;
            let step = async {
                match selected {
                    Some(selected) => {
                        self.control
                            .hydrate_cached_code_workspace(
                                &profile.client,
                                &self.authority,
                                &invocation.activation,
                                job,
                                manifest,
                                selected,
                                &intent,
                                index,
                            )
                            .await
                    }
                    None => {
                        self.control
                            .hydrate_plain_code_workspace(
                                &profile.client,
                                &self.authority,
                                &invocation.activation,
                                job,
                                manifest,
                                &intent,
                                index,
                            )
                            .await
                    }
                }
            };
            let outcome = tokio::time::timeout_at(deadline, step)
                .await
                .map_err(|_| CodeWorkspaceFailure::deadline())?;
            match outcome {
                Ok(
                    WorkspaceHydrationOutcome::Progress { ready: true, .. }
                    | WorkspaceHydrationOutcome::ObserveOriginal,
                ) => return Ok(()),
                Ok(WorkspaceHydrationOutcome::Progress {
                    next_file_index,
                    ready: false,
                }) => {
                    if next_file_index > index {
                        index = next_file_index;
                        continue;
                    }
                }
                Err(error) if workspace_retryable(&error) => {}
                Err(error) => return Err(CodeWorkspaceFailure::sandbox(&error)),
            }
            tokio::time::sleep_until(
                (tokio::time::Instant::now() + code_timing::CODE_RECONCILE_INTERVAL).min(deadline),
            )
            .await;
        }
    }

    pub(super) async fn hydrate_compile_workspace(
        &self,
        profile: &CodeRuntimeProfile,
        activation: &[u8; 32],
        job: &PreparedJob,
        manifest: &WorkspaceManifest,
        control: &Control,
        original_visit: &OriginalCodeVisitRef,
    ) -> Result<(), CodeWorkspaceFailure> {
        let deadline = observation_deadline(profile.timeout_seconds);
        let mut index = 0;
        loop {
            let attempt = self.control.hydrate_compile_code_workspace(
                &profile.client,
                &self.authority,
                activation,
                job,
                manifest,
                control,
                original_visit,
                index,
            );
            let outcome = tokio::time::timeout_at(deadline, attempt)
                .await
                .map_err(|_| CodeWorkspaceFailure::deadline())?;
            match outcome {
                Ok(
                    WorkspaceHydrationOutcome::Progress { ready: true, .. }
                    | WorkspaceHydrationOutcome::ObserveOriginal,
                ) => return Ok(()),
                Ok(WorkspaceHydrationOutcome::Progress {
                    next_file_index,
                    ready: false,
                }) => {
                    if next_file_index > index {
                        index = next_file_index;
                        continue;
                    }
                }
                Err(error) if workspace_retryable(&error) => {}
                Err(error) => return Err(CodeWorkspaceFailure::sandbox(&error)),
            }
            tokio::time::sleep_until(
                (tokio::time::Instant::now() + code_timing::CODE_RECONCILE_INTERVAL).min(deadline),
            )
            .await;
        }
    }

    pub(super) async fn hydrate_compile_repository(
        &self,
        profile: &CodeRuntimeProfile,
        activation: &[u8; 32],
        job: &PreparedJob,
        control: &Control,
        original_visit: &OriginalCodeVisitRef,
    ) -> Result<(), CodeWorkspaceFailure> {
        let binding = job
            .workspace()
            .ok_or_else(CodeWorkspaceFailure::invalid_input)?;
        let base = job
            .pre_workspace()
            .map_err(|_| CodeWorkspaceFailure::invalid_input())?;
        let content = self
            .intent_content
            .as_deref()
            .ok_or_else(CodeWorkspaceFailure::invalid_configuration)?;
        let manifest = content
            .resolve_code_workspace(&self.authority, original_visit, &base, &binding.selection)
            .await
            .map_err(|error| CodeWorkspaceFailure::input(&error))?;
        if !binding
            .matches(&manifest)
            .map_err(|_| CodeWorkspaceFailure::invalid_input())?
        {
            return Err(CodeWorkspaceFailure::invalid_input());
        }
        self.hydrate_compile_workspace(profile, activation, job, &manifest, control, original_visit)
            .await
    }
}
fn workspace_retryable(error: &SandboxCallError) -> bool {
    matches!(
        error,
        SandboxCallError::Submission {
            code: tonic::Code::Unavailable
                | tonic::Code::Aborted
                | tonic::Code::DeadlineExceeded
                | tonic::Code::ResourceExhausted
        }
    )
}

#[cfg(test)]
#[path = "code_workspace_activation_tests.rs"]
mod activation_tests;
