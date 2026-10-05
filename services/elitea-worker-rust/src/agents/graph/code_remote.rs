//! Invocation-bound supervisor execution; graph state never selects the runtime.

use super::code_trace::{CodePhase, observe};
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
#[path = "code_attempt_remote.rs"]
mod attempt_remote;
#[path = "code_compiled.rs"]
mod compiled;
#[path = "code_platform_drive.rs"]
mod platform_drive;
#[path = "code_preparation.rs"]
mod preparation;

#[derive(Clone)]
pub(super) struct CodeRuntimeProfile {
    pub(super) language: CodeLanguage,
    pub(super) platform_client:
        Option<crate::sandbox::platform_client_binding::PlatformClientBinding>,
    pub(super) image_digest: String,
    pub(super) policy_revision: String,
    pub(super) timeout_seconds: u32,
    pub(super) client: SandboxClient,
    preparation: Option<crate::config::PythonPreparationConfig>,
    preparation_client: Option<SandboxClient>,
    compiled_profile: Option<Arc<crate::sandbox::compiled_snapshot::SnapshotProfile>>,
}

pub(super) struct RemoteCodeRuntime {
    saved_child_scope: Option<
        Arc<dyn crate::agents::pipeline::saved_child_scope_provider::SavedChildScopeProvider>,
    >,
    pub(super) intent_content: Option<Arc<crate::transport::InputContentClient>>,
    pub(super) control: Arc<AgentControlClient<TonicControlRpc>>,
    pub(super) authority: Arc<ClaimBoundSandboxAuthority>,
    pub(super) profiles: Arc<[CodeRuntimeProfile]>,
    journal: Arc<crate::sandbox::dispatch::DispatchJournal>,
    compiled_profile: Option<Arc<crate::sandbox::compiled_snapshot::SnapshotProfile>>,
    debug_sink: Option<Arc<dyn super::code_debug::CodeDebugArtifactSink>>,
}

pub(crate) struct CodeRuntimeFactory {
    intent_content: Option<Arc<crate::transport::InputContentClient>>,
    control: Arc<AgentControlClient<TonicControlRpc>>,
    profiles: Arc<[CodeRuntimeProfile]>,
    journal: Arc<crate::sandbox::dispatch::DispatchJournal>,
    compiled_profile: Option<Arc<crate::sandbox::compiled_snapshot::SnapshotProfile>>,
    debug_sink: Option<Arc<dyn super::code_debug::CodeDebugArtifactSink>>,
}
impl CodeRuntimeFactory {
    pub(crate) fn new(
        control: Arc<AgentControlClient<TonicControlRpc>>,
        profiles: Vec<(
            crate::config::SandboxRuntimeConfig,
            SandboxClient,
            Option<SandboxClient>,
        )>,
        state: sqlx::PgPool,
    ) -> Self {
        let profiles = profiles
            .into_iter()
            .map(|(config, client, preparation_client)| CodeRuntimeProfile {
                language: match config.language {
                    Language::Python => CodeLanguage::Python,
                    Language::JavaScript => CodeLanguage::JavaScript,
                    Language::TypeScript => CodeLanguage::TypeScript,
                    Language::Rust => CodeLanguage::Rust,
                },
                platform_client: None,
                image_digest: config.image_digest,
                policy_revision: config.policy_revision,
                timeout_seconds: config.timeout_seconds,
                client,
                preparation: config.preparation,
                preparation_client,
                compiled_profile: None,
            })
            .collect::<Vec<_>>()
            .into();
        Self {
            intent_content: None,
            control,
            profiles,
            journal: Arc::new(crate::sandbox::dispatch::DispatchJournal::new(state)),
            compiled_profile: None,
            debug_sink: None,
        }
    }
    /// Add one exact operator/image broker profile. Existing pure profiles stay
    /// unchanged; saved YAML selects only the capability boolean.
    #[allow(
        clippy::needless_pass_by_value,
        reason = "Preserve existing ownership and caller contracts."
    )]
    pub(crate) fn with_platform_client(
        mut self,
        config: crate::config::SandboxRuntimeConfig,
        client: SandboxClient,
        preparation_client: Option<SandboxClient>,
        policy: crate::sandbox::platform_client_binding::PlatformClientPolicy,
        compiled_profile: Option<Arc<crate::sandbox::compiled_snapshot::SnapshotProfile>>,
    ) -> Result<Self, GraphError> {
        let language = match config.language {
            Language::Python => CodeLanguage::Python,
            Language::JavaScript => CodeLanguage::JavaScript,
            Language::TypeScript => CodeLanguage::TypeScript,
            Language::Rust => CodeLanguage::Rust,
        };
        if compiled_profile.is_some() && language != CodeLanguage::Rust {
            return Err(failed("Compiled broker profiles require Rust execution."));
        }
        if language == CodeLanguage::Rust && config.policy_revision != "cargo-broker-execute-v1" {
            return Err(failed(
                "Rust broker execution requires its fixed measured profile.",
            ));
        }
        if self
            .profiles
            .iter()
            .any(|profile| profile.language == language && profile.platform_client.is_some())
        {
            return Err(failed("The broker profile binding is ambiguous."));
        }
        let binding = policy
            .binding()
            .map_err(|_| failed("The broker operator policy is invalid."))?;
        let validation_job = PreparedJob::new(
            config.language,
            "validate".into(),
            std::collections::BTreeMap::new(),
            config.image_digest.clone(),
            config.policy_revision.clone(),
            config.timeout_seconds,
        )
        .map_err(|_| failed("The broker execution profile is invalid."))?;
        if let Some(profile) = &compiled_profile {
            profile
                .binding(&validation_job, "startup-validation", 1)
                .map_err(|_| {
                    failed("The broker compiled attestation does not match its execution profile.")
                })?;
        }
        let mut profiles = self.profiles.iter().cloned().collect::<Vec<_>>();
        profiles.push(CodeRuntimeProfile {
            language,
            platform_client: Some(binding),
            image_digest: config.image_digest,
            policy_revision: config.policy_revision,
            timeout_seconds: config.timeout_seconds,
            client,
            preparation: config.preparation,
            preparation_client,
            compiled_profile,
        });
        self.profiles = profiles.into();
        Ok(self)
    }
    pub(crate) fn with_code_intents(
        mut self,
        input: Arc<crate::transport::InputContentClient>,
    ) -> Self {
        self.intent_content = Some(input);
        self
    }
    pub(crate) fn with_debug_artifacts(
        mut self,
        sink: Arc<dyn super::code_debug::CodeDebugArtifactSink>,
    ) -> Self {
        self.debug_sink = Some(sink);
        self
    }
    /// Attach only after Main profile attestation and supervisor assembly are verified.
    /// Default construction leaves compiled snapshots disabled.
    #[allow(dead_code)] // Deployment assembly remains gated on verified profile producers.
    pub(crate) fn with_compiled_snapshots(
        mut self,
        profile: Arc<crate::sandbox::compiled_snapshot::SnapshotProfile>,
    ) -> Self {
        self.compiled_profile = Some(profile);
        self
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
            saved_child_scope: None,
            intent_content: self.intent_content.clone(),
            control: self.control.clone(),
            authority,
            profiles: self.profiles.clone(),
            journal: self.journal.clone(),
            compiled_profile: self.compiled_profile.clone(),
            debug_sink: self.debug_sink.clone(),
        })
    }
}

impl RemoteCodeRuntime {
    fn selected_compiled_profile<'a>(
        &'a self,
        profile: &'a CodeRuntimeProfile,
    ) -> Option<&'a Arc<crate::sandbox::compiled_snapshot::SnapshotProfile>> {
        if profile.platform_client.is_some() {
            profile.compiled_profile.as_ref()
        } else {
            self.compiled_profile.as_ref()
        }
    }
}

#[async_trait]
impl CodeSandboxRuntime for RemoteCodeRuntime {
    fn bind_saved_child_scope(
        &self,
        provider: Arc<
            dyn crate::agents::pipeline::saved_child_scope_provider::SavedChildScopeProvider,
        >,
    ) -> Result<Arc<dyn CodeSandboxRuntime>, GraphError> {
        Ok(Arc::new(Self {
            saved_child_scope: Some(provider),
            intent_content: self.intent_content.clone(),
            control: self.control.clone(),
            authority: self.authority.clone(),
            profiles: self.profiles.clone(),
            journal: self.journal.clone(),
            compiled_profile: self.compiled_profile.clone(),
            debug_sink: self.debug_sink.clone(),
        }))
    }
    #[allow(
        clippy::large_futures,
        reason = "Keep the bounded owner future lexical without adding allocations."
    )]
    async fn execute_attempt(
        &self,
        invocation: CodeInvocation<'_>,
        authority: &super::node_recovery_runtime::NodeAttemptAuthority,
    ) -> Result<Vec<u8>, super::code_runtime::CodeAttemptFailure> {
        self.execute_recovery_attempt(invocation, authority).await
    }

    #[allow(clippy::too_many_lines)] // Keep ordered authority checks and durable phase fences visible together.
    async fn execute(&self, invocation: CodeInvocation<'_>) -> Result<Vec<u8>, GraphError> {
        validate_invocation(&invocation)?;
        if invocation.original.is_some()
            || self.saved_child_scope.is_some()
            || invocation.debug.is_some()
            || invocation.workspace.is_some()
            || invocation.platform_client
        {
            return Err(failed(
                "Code original-visit authority is unavailable on the raw invocation path.",
            ));
        }
        let mut matching = self.profiles.iter().filter(|profile| {
            profile.language == invocation.language && profile.platform_client.is_none()
        });
        let profile = matching
            .next()
            .ok_or_else(|| failed("No sandbox runtime is configured for this Code language."))?;
        if matching.next().is_some() {
            return Err(failed(
                "Multiple sandbox runtimes are configured for this Code language.",
            ));
        }
        let trace = invocation
            .trace
            .as_ref()
            .map(|trace| trace.bind(self.authority.trace_identity()))
            .transpose()?;
        let (job, bundle) = observe(
            trace.as_ref(),
            CodePhase::Preparation,
            self.prepare_execution_job(&invocation, profile),
        )
        .await?;
        let selected = self
            .select_compiled_snapshot(&invocation, profile, &job, bundle.as_ref(), None)
            .await?;
        let scope = self
            .authority
            .dispatch_scope()
            .map_err(|error| failed(&error.to_string()))?;
        let digest = if let Some(selected) = &selected {
            selected
                .control
                .intent_digest(crate::sandbox::compiled_snapshot::Purpose::Execute)
                .map_err(|_| failed("Compiled snapshot intent is invalid."))?
        } else {
            job.fingerprint()
                .map_err(|_| failed("Sandbox request identity is invalid."))?
        };
        self.journal
            .register(
                &scope,
                &invocation.activation,
                &digest,
                profile.client.audience(),
            )
            .await
            .map_err(|error| failed(&error.to_string()))?;
        if let Some(selected) = &selected {
            self.journal
                .record_descriptor(
                    &scope,
                    &invocation.activation,
                    &digest,
                    profile.client.audience(),
                    &selected.descriptor_bytes,
                )
                .await
                .map_err(|error| failed(&error.to_string()))?;
        }
        if selected.is_none()
            && let Some(bundle) = &bundle
        {
            observe(
                trace.as_ref(),
                CodePhase::Hydration,
                self.hydrate_python_dependencies(
                    profile,
                    &invocation.activation,
                    &job,
                    bundle,
                    None,
                ),
            )
            .await?;
        }
        // Hydration remains inert and has its own fixed observation budget.
        // Execution observation starts only after readiness. The recovery
        // allowance does not change the supervisor's execution deadline.
        observe(trace.as_ref(), CodePhase::Execution, async {
        let deadline = observation_deadline(profile.timeout_seconds);
        loop {
            let attempt = async {
                if let Some(selected) = &selected {
                    self.control
                        .submit_compiled_snapshot(
                            &profile.client,
                            &self.authority,
                            &invocation.activation,
                            &job,
                            selected,
                            bundle.as_ref(),
                        )
                        .await
                } else {
                    self.submit_prepared_job(profile, &invocation.activation, &job, bundle.as_ref())
                        .await
                }
            };
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
                            | tonic::Code::Aborted
                            | tonic::Code::ResourceExhausted,
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
        }).await
    }
}

fn validate_invocation(invocation: &CodeInvocation<'_>) -> Result<(), GraphError> {
    if invocation.dependencies_toml.is_some() && invocation.language != CodeLanguage::Rust {
        return Err(failed("Cargo dependencies require the Rust Code language."));
    }
    // The authorized saved pipeline declares this resolver. Its selected state
    // remains untrusted. Main signs the actual prepared source/input fingerprint
    // and activation for every submission; provenance never supplies a grant.
    // This contract applies only to graph Code nodes, not chat Code tools.
    match invocation.provenance {
        CodeProvenance::SavedLiteral
        | CodeProvenance::StateVariable
        | CodeProvenance::StateTemplate => Ok(()),
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

fn observation_deadline(timeout_seconds: u32) -> tokio::time::Instant {
    tokio::time::Instant::now() + Duration::from_secs(u64::from(timeout_seconds) + 90)
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
            let client = self
                .profiles
                .iter()
                .find_map(|profile| {
                    if profile.client.audience() == pending.audience {
                        Some(&profile.client)
                    } else {
                        profile
                            .preparation_client
                            .as_ref()
                            .filter(|client| client.audience() == pending.audience)
                    }
                })
                .ok_or(DispatchError::TargetUnavailable)?;
            let status = self
                .control
                .stop_sandbox_job(client, authority, &pending.activation, &pending.digest)
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

#[cfg(test)]
pub(super) fn original_prepared_job_fixture(
    invocation: &CodeInvocation<'_>,
) -> Result<PreparedJob, GraphError> {
    let profile = CodeRuntimeProfile {
        platform_client: None,
        language: invocation.language,
        image_digest: format!("sha256:{}", "a".repeat(64)),
        policy_revision: "fixture-v1".into(),
        timeout_seconds: 30,
        client: SandboxClient::from_channel(
            tonic::transport::Endpoint::from_static("http://127.0.0.1:1").connect_lazy(),
            "dns:supervisor.fixture".into(),
            Duration::from_secs(30),
        )
        .map_err(|_| failed("Code fixture supervisor profile is invalid."))?,
        preparation: None,
        preparation_client: None,
        compiled_profile: None,
    };
    prepare_job(invocation, &profile)
}

#[cfg(test)]
#[path = "code_remote_authority_tests.rs"]
mod authority_tests;

#[path = "code_workspace_remote.rs"]
mod workspace_remote;

#[cfg(test)]
#[path = "code_platform_startup_tests.rs"]
mod platform_startup_tests;
