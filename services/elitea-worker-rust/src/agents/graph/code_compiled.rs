//! Optional pre-admission snapshot lookup. A recorded selection never becomes a miss.
use super::workspace_remote::CodeWorkspaceFailure;
use super::{CodeRuntimeProfile, RemoteCodeRuntime, failed};
use crate::agents::graph::{code_runtime::CodeAttemptPhase, node_recovery::NodeFailureClass};
use crate::{
    agents::graph::{code::CodeLanguage, code_runtime::CodeInvocation},
    sandbox::{
        compiled_snapshot::{Purpose, SelectedSnapshot},
        request::PreparedJob,
    },
};
use adk_rust::graph::GraphError;

pub(super) enum CodePreparationFailure {
    Existing(GraphError),
    Workspace(CodeWorkspaceFailure),
}
impl From<GraphError> for CodePreparationFailure {
    fn from(error: GraphError) -> Self {
        Self::Existing(error)
    }
}
impl From<CodeWorkspaceFailure> for CodePreparationFailure {
    fn from(error: CodeWorkspaceFailure) -> Self {
        Self::Workspace(error)
    }
}
impl CodePreparationFailure {
    pub(super) fn class(&self) -> NodeFailureClass {
        match self {
            Self::Existing(_) => NodeFailureClass::Unknown,
            Self::Workspace(error) => error.class(),
        }
    }
    pub(super) fn phase(&self) -> CodeAttemptPhase {
        match self {
            Self::Existing(_) => CodeAttemptPhase::Preparation,
            Self::Workspace(_) => CodeAttemptPhase::Hydration,
        }
    }
    fn into_graph_error(self) -> GraphError {
        match self {
            Self::Existing(error) => error,
            Self::Workspace(_) => failed("Code repository preparation is refused or unconfirmed."),
        }
    }
}

impl RemoteCodeRuntime {
    // Preserve the legacy GraphError surface outside the typed attempt owner.
    pub(super) async fn select_compiled_snapshot(
        &self,
        invocation: &CodeInvocation<'_>,
        profile: &CodeRuntimeProfile,
        job: &PreparedJob,
        bundle: Option<&crate::sandbox::dependency_bundle::DependencyBundle>,
        original_visit: Option<&crate::sandbox::code_recovery::OriginalCodeVisitRef>,
    ) -> Result<Option<SelectedSnapshot>, GraphError> {
        self.select_compiled_snapshot_typed(invocation, profile, job, bundle, original_visit)
            .await
            .map_err(CodePreparationFailure::into_graph_error)
    }

    pub(super) async fn select_compiled_snapshot_typed(
        &self,
        invocation: &CodeInvocation<'_>,
        profile: &CodeRuntimeProfile,
        job: &PreparedJob,
        bundle: Option<&crate::sandbox::dependency_bundle::DependencyBundle>,
        original_visit: Option<&crate::sandbox::code_recovery::OriginalCodeVisitRef>,
    ) -> Result<Option<SelectedSnapshot>, CodePreparationFailure> {
        let scope = self
            .authority
            .dispatch_scope()
            .map_err(|error| failed(&error.to_string()))?;
        let recorded = self
            .journal
            .recorded(
                &scope,
                &invocation.activation,
                self.selected_compiled_profile(profile).is_some(),
            )
            .await
            .map_err(|error| failed(&error.to_string()))?;
        let base = job
            .fingerprint()
            .map_err(|_| failed("Sandbox request identity is invalid."))?;
        if recorded
            .as_ref()
            .is_some_and(|recorded| recorded.audience != profile.client.audience())
        {
            return Err(
                failed("The original supervisor is unavailable; the job cannot move.").into(),
            );
        }
        if recorded
            .as_ref()
            .is_some_and(|recorded| recorded.digest == base)
        {
            return Ok(None);
        }
        let Some(snapshot_profile) = self
            .selected_compiled_profile(profile)
            .filter(|_| invocation.language == CodeLanguage::Rust)
        else {
            return if recorded.is_some() {
                Err(failed("The original compiled profile is unavailable.").into())
            } else {
                Ok(None)
            };
        };
        let binding = self
            .authority
            .compiled_binding(snapshot_profile, job)
            .map_err(|error| failed(&error.to_string()))?;
        if let Some(recorded) = &recorded
            && let Some(canonical) = &recorded.descriptor
        {
            let mut selected = SelectedSnapshot::select(binding.clone(), canonical.clone())
                .map_err(|_| failed("The recorded compiled snapshot is corrupt."))?;
            if selected
                .control
                .intent_digest(Purpose::Execute)
                .map_err(|_| failed("The recorded compiled intent is invalid."))?
                != recorded.digest
            {
                return Err(failed("The recorded compiled intent changed.").into());
            }
            selected.recovery = true;
            return Ok(Some(selected));
        }
        let compilation_activation = compiled_activation(&invocation.activation);
        if recorded.is_none()
            && self
                .journal
                .contains_activation(&scope, &compilation_activation)
                .await
                .map_err(|error| failed(&error.to_string()))?
        {
            return self
                .prepare_compiled_snapshot(
                    invocation,
                    profile,
                    job,
                    binding,
                    bundle,
                    &compilation_activation,
                    original_visit,
                )
                .await
                .map(Some);
        }
        self.lookup_or_compile_snapshot(
            invocation,
            profile,
            job,
            binding,
            bundle,
            &compilation_activation,
            recorded.as_ref().map(|record| record.digest),
            original_visit,
        )
        .await
    }
    #[allow(clippy::too_many_arguments)] // Keep lookup and the immutable fallback plan beside exact graph activation.
    async fn lookup_or_compile_snapshot(
        &self,
        invocation: &CodeInvocation<'_>,
        profile: &CodeRuntimeProfile,
        job: &PreparedJob,
        binding: crate::sandbox::compiled_snapshot::Binding,
        bundle: Option<&crate::sandbox::dependency_bundle::DependencyBundle>,
        compilation_activation: &[u8; 32],
        recorded: Option<[u8; 32]>,
        original_visit: Option<&crate::sandbox::code_recovery::OriginalCodeVisitRef>,
    ) -> Result<Option<SelectedSnapshot>, CodePreparationFailure> {
        let selected = self
            .control
            .read_compiled_snapshot(
                &profile.client,
                &self.authority,
                &invocation.activation,
                job,
                binding.clone(),
            )
            .await
            .map_err(|error| failed(&error.to_string()))?;
        match selected {
            None if recorded.is_some() => Err(failed(
                "The selected compiled snapshot is absent; the job was not recompiled.",
            )
            .into()),
            None => self
                .prepare_compiled_snapshot(
                    invocation,
                    profile,
                    job,
                    binding,
                    bundle,
                    compilation_activation,
                    original_visit,
                )
                .await
                .map(Some),
            Some(selected) => {
                let digest = selected
                    .control
                    .intent_digest(Purpose::Execute)
                    .map_err(|_| failed("Compiled snapshot intent is invalid."))?;
                if recorded.is_some_and(|recorded| recorded != digest) {
                    return Err(failed(
                        "The compiled snapshot differs from the original dispatch; the job was not restarted.",
                    ).into());
                }
                Ok(Some(selected))
            }
        }
    }
}

fn compiled_activation(activation: &[u8; 32]) -> [u8; 32] {
    let mut hash = ring::digest::Context::new(&ring::digest::SHA256);
    hash.update(b"elitea.sandbox.compilation-activation.v1\0");
    hash.update(activation);
    let mut out = [0; 32];
    out.copy_from_slice(hash.finish().as_ref());
    out
}

impl RemoteCodeRuntime {
    #[allow(
        clippy::too_many_arguments,
        reason = "Keep the existing authenticated identity fields explicit."
    )]
    async fn prepare_compiled_snapshot(
        &self,
        _invocation: &CodeInvocation<'_>,
        profile: &CodeRuntimeProfile,
        job: &PreparedJob,
        binding: crate::sandbox::compiled_snapshot::Binding,
        bundle: Option<&crate::sandbox::dependency_bundle::DependencyBundle>,
        activation: &[u8; 32],
        original_visit: Option<&crate::sandbox::code_recovery::OriginalCodeVisitRef>,
    ) -> Result<SelectedSnapshot, CodePreparationFailure> {
        use crate::sandbox::{client::CompilationOutcome, compiled_snapshot::Control};
        let scope = self
            .authority
            .dispatch_scope()
            .map_err(|error| failed(&error.to_string()))?;
        let control = Control {
            revision: 1,
            snapshot_key_sha256: binding
                .key()
                .map_err(|_| failed("The snapshot key is invalid."))?,
            binding: binding.clone(),
            descriptor_sha256: None,
        };
        let digest = control
            .intent_digest(Purpose::Compile)
            .map_err(|_| failed("The compile intent is invalid."))?;
        self.journal
            .register(&scope, activation, &digest, profile.client.audience())
            .await
            .map_err(|error| failed(&error.to_string()))?;
        let recorded = self
            .journal
            .recorded(&scope, activation, true)
            .await
            .map_err(|error| failed(&error.to_string()))?
            .ok_or_else(|| failed("The original compile plan is missing."))?;
        if recorded.digest != digest || recorded.audience != profile.client.audience() {
            return Err(failed("The original compilation identity changed.").into());
        }
        let deadline = super::observation_deadline(profile.timeout_seconds);
        let canonical = if let Some(canonical) = recorded.descriptor {
            canonical
        } else {
            if job.workspace().is_some() {
                let original_visit =
                    original_visit.ok_or_else(CodeWorkspaceFailure::authorization)?;
                self.hydrate_compile_repository(profile, activation, job, &control, original_visit)
                    .await?;
            }
            loop {
                let result = tokio::time::timeout_at(
                    deadline,
                    self.control.compile_snapshot_from_original_visit(
                        &profile.client,
                        &self.authority,
                        activation,
                        job,
                        binding.clone(),
                        bundle,
                        original_visit,
                    ),
                )
                .await
                .map_err(|_| super::uncertain())?;
                match result {
                    Ok(CompilationOutcome::Captured { canonical, .. }) => {
                        self.journal
                            .record_descriptor(
                                &scope,
                                activation,
                                &digest,
                                profile.client.audience(),
                                &canonical,
                            )
                            .await
                            .map_err(|error| failed(&error.to_string()))?;
                        break canonical;
                    }
                    Ok(CompilationOutcome::Pending) => {}
                    Ok(CompilationOutcome::Cancelled) => {
                        return Err(failed("Compilation was cancelled.").into());
                    }
                    Ok(
                        CompilationOutcome::Failed { code }
                        | CompilationOutcome::Uncertain { code },
                    ) => return Err(failed(&code).into()),
                    Err(error) if super::preparation::retryable(&error) => {}
                    Err(error) => return Err(failed(&error.to_string()).into()),
                }
                if tokio::time::Instant::now() >= deadline {
                    return Err(super::uncertain().into());
                }
                tokio::time::sleep(std::time::Duration::from_millis(250)).await;
            }
        };
        let selected = SelectedSnapshot::select(binding.clone(), canonical.clone())
            .map_err(|_| failed("Captured snapshot metadata is invalid."))?;
        self.publish_compilation(profile, job, &binding, &canonical, activation, deadline)
            .await?;
        self.journal
            .resolve(&scope, activation, &digest, profile.client.audience())
            .await
            .map_err(|error| failed(&error.to_string()))?;
        Ok(selected)
    }
    async fn publish_compilation(
        &self,
        profile: &CodeRuntimeProfile,
        job: &PreparedJob,
        binding: &crate::sandbox::compiled_snapshot::Binding,
        canonical: &[u8],
        activation: &[u8; 32],
        deadline: tokio::time::Instant,
    ) -> Result<(), GraphError> {
        use crate::{
            protocol::elitea::runtime::v1::RustCompiledPublicationPhaseV1,
            sandbox::client::PublicationOutcome,
        };
        for phase in [
            RustCompiledPublicationPhaseV1::Release,
            RustCompiledPublicationPhaseV1::Ready,
        ] {
            loop {
                let result = tokio::time::timeout_at(
                    deadline,
                    self.control.publish_snapshot(
                        &profile.client,
                        &self.authority,
                        activation,
                        job,
                        binding,
                        canonical,
                        phase,
                    ),
                )
                .await
                .map_err(|_| super::uncertain())?;
                match result {
                    Ok(PublicationOutcome::Completed) => break,
                    Ok(PublicationOutcome::Pending) => {}
                    Ok(PublicationOutcome::Cancelled) => {
                        return Err(failed("Compilation was cancelled."));
                    }
                    Ok(
                        PublicationOutcome::Failed { code }
                        | PublicationOutcome::Uncertain { code },
                    ) => return Err(failed(&code)),
                    Err(error) if super::preparation::retryable(&error) => {}
                    Err(error) => return Err(failed(&error.to_string())),
                }
                if tokio::time::Instant::now() >= deadline {
                    return Err(super::uncertain());
                }
                tokio::time::sleep(std::time::Duration::from_millis(250)).await;
            }
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn legacy_compiled_adapter_preserves_existing_error_and_redacts_workspace_cause() {
        let existing = CodePreparationFailure::from(failed("existing compile failure"));
        assert!(
            existing
                .into_graph_error()
                .to_string()
                .contains("existing compile failure")
        );
        let workspace = CodePreparationFailure::from(CodeWorkspaceFailure::input(
            &crate::transport::InputContentError::AuthorizationFailed(
                "token=synthetic-secret https://private.invalid/repository",
            ),
        ));
        let public_error = workspace.into_graph_error().to_string();
        assert!(public_error.contains("Code repository preparation is refused or unconfirmed."));
        assert!(!public_error.contains("synthetic-secret"));
        assert!(!public_error.contains("private.invalid"));
    }
    #[test]
    fn compile_activation_is_distinct_stable_and_cannot_move_with_changed_content() {
        let activation = [7; 32];
        let compiler = compiled_activation(&activation);
        assert_ne!(compiler, activation);
        assert_eq!(compiler, compiled_activation(&activation));
        assert_ne!(compiler, compiled_activation(&[8; 32]));
        // Content belongs to the immutable request digest. The original activation alone
        // fixes the compiler identity, so conflicting content cannot create another job.
    }
}
