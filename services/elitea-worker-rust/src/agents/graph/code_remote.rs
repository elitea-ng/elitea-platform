//! Invocation-bound supervisor execution; graph state never selects the runtime.

use super::{
    code::{CodeLanguage, CodeProvenance},
    code_runtime::{CodeInvocation, CodeSandboxRuntime},
};
use crate::{
    protocol::control::{AgentControlClient, ClaimBoundSandboxAuthority},
    sandbox::{
        client::{SandboxCallError, SandboxClient, SandboxOutcome},
        request::{Language, PreparedJob},
    },
    transport::control_grpc::TonicControlRpc,
};
use adk_rust::graph::GraphError;
use async_trait::async_trait;
use std::{sync::Arc, time::Duration};

pub(super) struct CodeRuntimeProfile {
    pub(super) language: CodeLanguage,
    pub(super) image_digest: String,
    pub(super) policy_revision: String,
    pub(super) timeout_seconds: u32,
    pub(super) client: SandboxClient,
}

pub(super) struct RemoteCodeRuntime {
    pub(super) control: Arc<AgentControlClient<TonicControlRpc>>,
    pub(super) authority: Arc<ClaimBoundSandboxAuthority>,
    pub(super) profiles: Arc<[CodeRuntimeProfile]>,
    journal: Arc<crate::sandbox::dispatch::DispatchJournal>,
}

pub(crate) struct CodeRuntimeFactory {
    control: Arc<AgentControlClient<TonicControlRpc>>,
    profiles: Arc<[CodeRuntimeProfile]>,
    journal: Arc<crate::sandbox::dispatch::DispatchJournal>,
}
impl CodeRuntimeFactory {
    pub(crate) fn new(
        control: Arc<AgentControlClient<TonicControlRpc>>,
        profiles: Vec<(crate::config::SandboxRuntimeConfig, SandboxClient)>,
        state: sqlx::PgPool,
    ) -> Self {
        let profiles = profiles
            .into_iter()
            .map(|(config, client)| CodeRuntimeProfile {
                language: match config.language {
                    Language::Python => CodeLanguage::Python,
                    Language::JavaScript => CodeLanguage::JavaScript,
                    Language::TypeScript => CodeLanguage::TypeScript,
                    Language::Rust => CodeLanguage::Rust,
                },
                image_digest: config.image_digest,
                policy_revision: config.policy_revision,
                timeout_seconds: config.timeout_seconds,
                client,
            })
            .collect::<Vec<_>>()
            .into();
        Self {
            control,
            profiles,
            journal: Arc::new(crate::sandbox::dispatch::DispatchJournal::new(state)),
        }
    }
    pub(crate) fn attach(
        &self,
        nodes: super::compiler::PipelineNodeRuntimes,
        authority: Arc<ClaimBoundSandboxAuthority>,
    ) -> super::compiler::PipelineNodeRuntimes {
        nodes.with_code(self.bind(authority))
    }

    pub(in crate::agents) fn bind(
        &self,
        authority: Arc<ClaimBoundSandboxAuthority>,
    ) -> Arc<dyn CodeSandboxRuntime> {
        Arc::new(RemoteCodeRuntime {
            control: self.control.clone(),
            authority,
            profiles: self.profiles.clone(),
            journal: self.journal.clone(),
        })
    }
}

#[async_trait]
impl CodeSandboxRuntime for RemoteCodeRuntime {
    async fn execute(&self, invocation: CodeInvocation<'_>) -> Result<Vec<u8>, GraphError> {
        // Dynamic code needs its own approval binding, not merely a saved YAML
        // variable reference. It remains disabled until that contract is wired.
        if invocation.provenance != CodeProvenance::SavedLiteral {
            return Err(failed(
                "State-supplied code requires sandbox approval, which is not configured.",
            ));
        }
        let mut matching = self
            .profiles
            .iter()
            .filter(|profile| profile.language == invocation.language);
        let profile = matching
            .next()
            .ok_or_else(|| failed("No sandbox runtime is configured for this Code language."))?;
        if matching.next().is_some() {
            return Err(failed(
                "Multiple sandbox runtimes are configured for this Code language.",
            ));
        }
        let job = prepare_job(&invocation, profile)?;
        let scope = self
            .authority
            .dispatch_scope()
            .map_err(|error| failed(&error.to_string()))?;
        let digest = job
            .fingerprint()
            .map_err(|_| failed("Sandbox request identity is invalid."))?;
        self.journal
            .register(
                &scope,
                &invocation.activation,
                &digest,
                profile.client.audience(),
            )
            .await
            .map_err(|error| failed(&error.to_string()))?;
        // Leave room for the supervisor's existing lease to expire during
        // recovery. This does not extend the sandbox's execution deadline.
        let deadline = tokio::time::Instant::now()
            + Duration::from_secs(u64::from(profile.timeout_seconds) + 90);
        loop {
            let attempt = self.control.submit_sandbox_job(
                &profile.client,
                &self.authority,
                &invocation.activation,
                &job,
            );
            let outcome = tokio::time::timeout_at(deadline, attempt)
                .await
                .map_err(|_| uncertain())?;
            if matches!(
                &outcome,
                Ok(SandboxOutcome::Completed(_)
                    | SandboxOutcome::Failed { .. }
                    | SandboxOutcome::Cancelled)
            ) {
                self.journal
                    .resolve(
                        &scope,
                        &invocation.activation,
                        &digest,
                        profile.client.audience(),
                    )
                    .await
                    .map_err(|error| failed(&error.to_string()))?;
            }
            match outcome {
                Ok(SandboxOutcome::Completed(receipt)) => return Ok(receipt),
                Ok(SandboxOutcome::Failed { code }) => {
                    return Err(failed(&format!(
                        "Sandbox execution failed ({code}); dependent pipeline nodes were not run."
                    )));
                }
                Ok(SandboxOutcome::Cancelled) => {
                    return Err(failed(
                        "Sandbox execution was cancelled; dependent pipeline nodes were not run.",
                    ));
                }
                Ok(SandboxOutcome::Uncertain { .. }) => return Err(uncertain()),
                Ok(SandboxOutcome::Pending)
                | Err(
                    SandboxCallError::Authorization(
                        crate::transport::control_grpc::ControlGrpcError::Unavailable(_),
                    )
                    | SandboxCallError::Submission {
                        code:
                            tonic::Code::Unavailable
                            | tonic::Code::DeadlineExceeded
                            | tonic::Code::Aborted,
                    },
                ) => {}
                Err(error) => return Err(failed(&error.to_string())),
            }
            if tokio::time::Instant::now() >= deadline {
                return Err(uncertain());
            }
            // Every retry uses a fresh Main grant and the identical prepared
            // request/activation. No replacement sandbox identity is generated.
            tokio::time::sleep_until(
                (tokio::time::Instant::now() + Duration::from_secs(1)).min(deadline),
            )
            .await;
        }
    }
}

fn failed(message: &str) -> GraphError {
    GraphError::Other(format!("graph.code.execution_failed: {message}"))
}
fn uncertain() -> GraphError {
    failed(
        "Sandbox completion could not be confirmed. The job was not restarted; reconcile its durable status before retrying the pipeline.",
    )
}

#[async_trait]
impl crate::sandbox::dispatch::SandboxStopDelivery for CodeRuntimeFactory {
    async fn stop(
        &self,
        authority: &crate::protocol::control::SandboxStopAuthority,
    ) -> Result<bool, crate::sandbox::dispatch::DispatchError> {
        use crate::protocol::elitea::runtime::v1::SandboxJobStatusV1;
        use crate::sandbox::dispatch::DispatchError;
        let scope = authority.scope()?;
        for pending in self.journal.pending(&scope).await? {
            let profile = self
                .profiles
                .iter()
                .find(|profile| profile.client.audience() == pending.audience)
                .ok_or(DispatchError::TargetUnavailable)?;
            let status = self
                .control
                .stop_sandbox_job(
                    &profile.client,
                    authority,
                    &pending.activation,
                    &pending.digest,
                )
                .await?;
            match status {
                SandboxJobStatusV1::Completed
                | SandboxJobStatusV1::Failed
                | SandboxJobStatusV1::Cancelled => {
                    self.journal
                        .resolve(
                            &scope,
                            &pending.activation,
                            &pending.digest,
                            &pending.audience,
                        )
                        .await?;
                }
                _ => return Ok(false),
            }
        }
        Ok(self.journal.pending(&scope).await?.is_empty())
    }
}

fn prepare_job(
    invocation: &CodeInvocation<'_>,
    profile: &CodeRuntimeProfile,
) -> Result<PreparedJob, GraphError> {
    let language = match invocation.language {
        CodeLanguage::Python => Language::Python,
        CodeLanguage::JavaScript => Language::JavaScript,
        CodeLanguage::TypeScript => Language::TypeScript,
        CodeLanguage::Rust => Language::Rust,
    };
    PreparedJob::new(
        language,
        invocation.source.to_owned(),
        serde_json::from_slice(&invocation.input_json)
            .map_err(|_| failed("Code input is invalid."))?,
        profile.image_digest.clone(),
        profile.policy_revision.clone(),
        profile.timeout_seconds,
    )
    .map_err(|_| {
        failed("Code input or sandbox runtime configuration exceeds its supported limits.")
    })
}
