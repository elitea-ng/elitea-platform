//! Invocation-bound supervisor execution; graph state never selects the runtime.
#![allow(dead_code)] // Deployment configuration is wired in the production composition.

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
    pub(super) profiles: Vec<CodeRuntimeProfile>,
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
        let language = match invocation.language {
            CodeLanguage::Python => Language::Python,
            CodeLanguage::JavaScript => Language::JavaScript,
            CodeLanguage::TypeScript => Language::TypeScript,
            CodeLanguage::Rust => Language::Rust,
        };
        let job = PreparedJob::new(
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
        })?;
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
                        code: tonic::Code::Unavailable | tonic::Code::DeadlineExceeded,
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
