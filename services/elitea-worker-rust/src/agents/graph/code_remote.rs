//! Invocation-bound supervisor execution; graph state never selects the runtime.

use super::code_trace::{CodePhase, observe};
use super::{
    code::{CodeLanguage, CodeProvenance},
    code_runtime::{CodeInvocation, CodeSandboxRuntime},
};
use crate::agents::graph::code_timing;
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
use elitea_agent_runtime::graph::fanout::admission::{
    CodeDeadlineRefusal, CodeRefusalCause, CodeSlot, CodeSlots, StartedSlotRefusal,
    code_attempt_scope, required_window,
};
use std::{collections::BTreeMap, sync::Arc, time::Duration};
#[path = "code_attempt_remote.rs"]
pub(in crate::agents::graph) mod attempt_remote;
#[path = "code_compiled.rs"]
mod compiled;
use elitea_agent_runtime::graph::code_platform_drive as platform_drive;
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
    /// W3-1: the slots of this profile's Supervisor target, shared with every
    /// profile on the same normalised target.
    pub(super) slots: CodeSlots,
    /// The normalised Supervisor target `slots` belongs to, for the lines an
    /// operator reads.
    pub(super) slot_target: Arc<str>,
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
    /// The run's deadline (the signed command's), which every fresh Code
    /// attempt is admitted against (W3-1).
    pub(super) execution_deadline: tokio::time::Instant,
}

pub(crate) struct CodeRuntimeFactory {
    intent_content: Option<Arc<crate::transport::InputContentClient>>,
    control: Arc<AgentControlClient<TonicControlRpc>>,
    profiles: Arc<[CodeRuntimeProfile]>,
    journal: Arc<crate::sandbox::dispatch::DispatchJournal>,
    compiled_profile: Option<Arc<crate::sandbox::compiled_snapshot::SnapshotProfile>>,
    debug_sink: Option<Arc<dyn super::code_debug::CodeDebugArtifactSink>>,
    /// One set of Code slots per normalised Supervisor target (W3-1, C4).
    code_slots: BTreeMap<String, CodeSlots>,
}

/// The slots of `target`: the existing set of its normalised Supervisor, or
/// a new one of `capacity`. `RuntimeDeployConfig::validate` has already
/// refused a target that does not normalise and profiles on one target that
/// disagree, so the first capacity is every profile's.
fn slots_for(table: &mut BTreeMap<String, CodeSlots>, target: &str, capacity: u32) -> CodeSlots {
    table
        .entry(slot_key(target).to_string())
        .or_insert_with(|| CodeSlots::new(capacity))
        .clone()
}

/// The normalised Supervisor target (`crate::config::code_slot_target`).
fn slot_key(target: &str) -> Arc<str> {
    crate::config::code_slot_target(target)
        .unwrap_or_else(|_| target.to_owned())
        .into()
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
        let mut code_slots = BTreeMap::new();
        let profiles = profiles
            .into_iter()
            .map(|(config, client, preparation_client)| CodeRuntimeProfile {
                slots: slots_for(
                    &mut code_slots,
                    &config.target,
                    config.effective_code_slots(),
                ),
                slot_target: slot_key(&config.target),
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
            code_slots,
        }
    }

    /// One startup INFO line per Supervisor target: the Code slots in force
    /// (D2: the default is the shipped Supervisor concurrency, so an operator
    /// sees what to match).
    pub(crate) fn log_code_slots(&self) {
        for (target, slots) in &self.code_slots {
            tracing::info!(
                event = "code_slots_configured",
                supervisor_target = target.as_str(),
                code_slots = slots.capacity(),
            );
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
            slots: slots_for(
                &mut self.code_slots,
                &config.target,
                config.effective_code_slots(),
            ),
            slot_target: slot_key(&config.target),
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
    /// `execution_deadline` is the run's (`OrdinaryNativeAgentPlan::execution_deadline`):
    /// every fresh Code attempt of this runtime is admitted against it.
    pub(crate) fn attach(
        &self,
        nodes: super::compiler::PipelineNodeRuntimes,
        authority: Arc<ClaimBoundSandboxAuthority>,
        execution_deadline: tokio::time::Instant,
    ) -> super::compiler::PipelineNodeRuntimes {
        nodes.with_code(self.bind(authority, execution_deadline))
    }

    pub(in crate::agents) fn bind(
        &self,
        authority: Arc<ClaimBoundSandboxAuthority>,
        execution_deadline: tokio::time::Instant,
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
            execution_deadline,
        })
    }
}

impl RemoteCodeRuntime {
    /// Hydrate the prepared dependencies for the raw `execute` path. A failure
    /// happens before any submission, so the runtime this attempt hydrated is
    /// released with the claim-bound stop instead of waiting out the
    /// supervisor's retention sweep.
    async fn hydrate_or_release(
        &self,
        profile: &CodeRuntimeProfile,
        activation: &[u8; 32],
        digest: &[u8; 32],
        job: &crate::sandbox::request::PreparedJob,
        bundle: &crate::sandbox::dependency_bundle::DependencyBundle,
    ) -> Result<(), GraphError> {
        let hydrated = self
            .hydrate_python_dependencies(profile, activation, job, bundle, None)
            .await
            .map_err(super::code_runtime::CodePreparationFailure::graph_error);
        if hydrated.is_err() {
            self.release_hydrated_job(profile, activation, digest).await;
        }
        hydrated
    }

    /// W3-1: admit a fresh attempt (nothing of it started) on `profile`'s
    /// target. The slot is held until the attempt's terminal receipt or typed
    /// failure; a refusal holds nothing and is logged as one WARN fault line.
    pub(super) async fn admit_fresh(
        &self,
        profile: &CodeRuntimeProfile,
        node_id: Option<&str>,
    ) -> Result<CodeSlot, CodeDeadlineRefusal> {
        let required = required_window(
            1,
            1,
            profile.slots.capacity(),
            Duration::from_secs(u64::from(profile.timeout_seconds)),
        );
        let admitted = profile
            .slots
            .admit_fresh(self.execution_deadline, required)
            .await;
        match &admitted {
            Ok(slot) => self.note_slot_wait(slot, profile),
            Err(refusal) => self.log_deadline_refusal(refusal, profile, node_id),
        }
        admitted
    }

    /// A slot for an attempt that may already have started (C2): no deadline
    /// admission, bounded by the run's deadline.
    pub(super) async fn admit_started(
        &self,
        profile: &CodeRuntimeProfile,
    ) -> Result<CodeSlot, StartedSlotRefusal> {
        let admitted = profile.slots.admit_started(self.execution_deadline).await;
        match &admitted {
            Ok(slot) => self.note_slot_wait(slot, profile),
            Err(StartedSlotRefusal::NestedSlot) => self.log_nested_slot(profile),
            Err(StartedSlotRefusal::Expired(_)) => {}
        }
        admitted
    }

    /// N3 (W3-2): a fresh attempt is re-admitted after its preparation and
    /// hydration, before its first submission. Never applied to an attempt
    /// that may already have started (C2): the caller checks that first.
    pub(super) fn recheck_after_preparation(
        &self,
        profile: &CodeRuntimeProfile,
        node_id: Option<&str>,
    ) -> Result<(), CodeDeadlineRefusal> {
        let required = required_window(
            1,
            1,
            profile.slots.capacity(),
            Duration::from_secs(u64::from(profile.timeout_seconds)),
        );
        let checked = elitea_agent_runtime::graph::fanout::admission::recheck_after_preparation(
            self.execution_deadline,
            required,
            tokio::time::Instant::now(),
        );
        if let Err(refusal) = &checked {
            self.log_deadline_refusal(refusal, profile, node_id);
        }
        checked
    }

    /// Q2 and Q3 (W3-2): the Code time of one fan-out child, the sum of the
    /// timeouts of its Code nodes (each node once), and the smallest
    /// `code_slots` among the Supervisor targets they use. `None` when no
    /// node has a profile here: each such attempt then fails typed itself.
    pub(super) fn child_code_window(
        &self,
        nodes: &[(CodeLanguage, bool)],
    ) -> Option<(Duration, u32)> {
        let mut time = Duration::ZERO;
        let mut slots: Option<u32> = None;
        for (language, platform_client) in nodes {
            let Some(profile) = self.profiles.iter().find(|profile| {
                profile.language == *language
                    && profile.platform_client.is_some() == *platform_client
            }) else {
                continue;
            };
            time = time.saturating_add(Duration::from_secs(u64::from(profile.timeout_seconds)));
            slots = Some(slots.map_or(profile.slots.capacity(), |slots| {
                slots.min(profile.slots.capacity())
            }));
        }
        slots.map(|slots| (time, slots))
    }

    /// R1: one ERROR line, operator only, when an attempt holding a Code slot
    /// asked for a second one. Nothing user-supplied.
    fn log_nested_slot(&self, profile: &CodeRuntimeProfile) {
        let (support_ref, _) = self.authority.trace_identity();
        elitea_faults::fault!(
            tracing::Level::ERROR,
            code = elitea_faults::codes::PIPELINE_CODE_NESTED_SLOT,
            event = "code_nested_slot",
            slots_capacity = profile.slots.capacity(),
            supervisor_target = &*profile.slot_target,
            support_ref = support_ref
        );
    }

    /// One INFO line when a slot wait passed [`SLOT_WAIT_NOTICE_AFTER`]: the
    /// Worker was full, an operator signal. An uncontended acquire logs
    /// nothing. Nothing user-supplied.
    fn note_slot_wait(&self, slot: &CodeSlot, profile: &CodeRuntimeProfile) {
        if slot.waited() <= SLOT_WAIT_NOTICE_AFTER {
            return;
        }
        let (execution_id, _) = self.authority.trace_identity();
        elitea_faults::fault!(
            tracing::Level::INFO,
            code = elitea_faults::codes::PIPELINE_CODE_SLOT_WAITED,
            event = "code_slot_waited",
            slot_wait_ms = u64::try_from(slot.waited().as_millis()).unwrap_or(u64::MAX),
            slots_capacity = profile.slots.capacity(),
            supervisor_target = &*profile.slot_target,
            execution_id = execution_id
        );
    }

    fn log_deadline_refusal(
        &self,
        refusal: &CodeDeadlineRefusal,
        profile: &CodeRuntimeProfile,
        node_id: Option<&str>,
    ) {
        let millis = |duration: Duration| u64::try_from(duration.as_millis()).unwrap_or(u64::MAX);
        let (support_ref, _) = self.authority.trace_identity();
        let code = elitea_faults::codes::PIPELINE_CODE_DEADLINE_INSUFFICIENT;
        match refusal.cause {
            CodeRefusalCause::NestedSlot => self.log_nested_slot(profile),
            CodeRefusalCause::Preparation => elitea_faults::fault!(
                tracing::Level::WARN,
                code = code,
                event = "code_deadline_insufficient",
                node_id = node_id.unwrap_or_default(),
                required_ms = millis(refusal.required),
                remaining_ms = millis(refusal.remaining),
                slots = profile.slots.capacity(),
                support_ref = support_ref,
                cause = "preparation"
            ),
            CodeRefusalCause::Clock => elitea_faults::fault!(
                tracing::Level::WARN,
                code = code,
                event = "code_deadline_insufficient",
                node_id = node_id.unwrap_or_default(),
                required_ms = millis(refusal.required),
                remaining_ms = millis(refusal.remaining),
                slots = profile.slots.capacity(),
                support_ref = support_ref
            ),
            CodeRefusalCause::SlotWait => elitea_faults::fault!(
                tracing::Level::WARN,
                code = code,
                event = "code_deadline_insufficient",
                node_id = node_id.unwrap_or_default(),
                required_ms = millis(refusal.required),
                remaining_ms = millis(refusal.remaining),
                slots = profile.slots.capacity(),
                support_ref = support_ref,
                cause = "slot_wait",
                slot_wait_ms = millis(refusal.slot_wait)
            ),
        }
    }

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

impl RemoteCodeRuntime {
    /// The raw invocation. `report`, when given, receives the typed cause of
    /// a failure the caller (a tool) wants as a value.
    #[allow(clippy::too_many_lines)] // Keep ordered authority checks and durable phase fences visible together.
    async fn execute_reporting(
        &self,
        invocation: CodeInvocation<'_>,
        report: Option<&std::sync::Mutex<Option<super::code_runtime::ToolCodeError>>>,
    ) -> Result<Vec<u8>, GraphError> {
        let note = |cause: super::code_runtime::ToolCodeError| {
            if let Some(slot) = report
                && let Ok(mut slot) = slot.lock()
            {
                *slot = Some(cause);
            }
        };
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
        let profile = matching.next().ok_or_else(|| {
            note(super::code_runtime::ToolCodeError::Unavailable);
            failed("No sandbox runtime is configured for this Code language.")
        })?;
        if matching.next().is_some() {
            return Err(failed(
                "Multiple sandbox runtimes are configured for this Code language.",
            ));
        }
        // W3-1: one slot for the whole attempt, taken before its first effect
        // (preparation, journal registration, submission); refused typed
        // when the run's time left cannot hold the timeout plus the margin.
        let _slot = self.admit_fresh(profile, None).await.map_err(|refusal| {
            if refusal.cause == CodeRefusalCause::NestedSlot {
                // R1: an internal fault, never a deadline the user can fix.
                note(super::code_runtime::ToolCodeError::Refused);
                return failed(NESTED_SLOT_MESSAGE);
            }
            note(super::code_runtime::ToolCodeError::DeadlineInsufficient);
            deadline_insufficient()
        })?;
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
                self.hydrate_or_release(profile, &invocation.activation, &digest, &job, bundle),
            )
            .await?;
        }
        // Hydration remains inert and has its own fixed observation budget.
        // Execution observation starts only after readiness. The recovery
        // allowance does not change the supervisor's execution deadline.
        observe(trace.as_ref(), CodePhase::Execution, async {
        let deadline = observation_deadline(profile.timeout_seconds);
        let mut busy = SupervisorBusyNotice::default();
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
                .map_err(|_| {
                    note(super::code_runtime::ToolCodeError::Uncertain);
                    uncertain()
                })?;
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
                    note(super::code_runtime::ToolCodeError::Sandbox { code: code.clone() });
                    return Err(failed(&format!(
                        "Sandbox execution failed ({code}); dependent pipeline nodes were not run."
                    )));
                }
                Ok(SandboxOutcome::Cancelled) => {
                    note(super::code_runtime::ToolCodeError::Cancelled);
                    return Err(failed(
                        "Sandbox execution was cancelled; dependent pipeline nodes were not run.",
                    ));
                }
                Ok(SandboxOutcome::Uncertain { .. }) => {
                    note(super::code_runtime::ToolCodeError::Uncertain);
                    return Err(uncertain());
                }
                Err(SandboxCallError::Submission {
                    code: code @ tonic::Code::ResourceExhausted,
                    ..
                }) => busy.note(code, self, profile),
                Ok(SandboxOutcome::Pending)
                | Err(
                    SandboxCallError::Authorization(
                        crate::transport::control_grpc::ControlGrpcError::Unavailable(_),
                    )
                    | SandboxCallError::Submission { code:
                            tonic::Code::Unavailable
                            | tonic::Code::DeadlineExceeded
                            | tonic::Code::Aborted, .. },
                ) => {}
                Err(error) => {
                    note(match &error {
                        SandboxCallError::Invalid
                        | SandboxCallError::Rejected
                        | SandboxCallError::Authorization(_) => {
                            super::code_runtime::ToolCodeError::Refused
                        }
                        SandboxCallError::InvalidReceipt => {
                            super::code_runtime::ToolCodeError::Uncertain
                        }
                        SandboxCallError::Submission { .. } => {
                            super::code_runtime::ToolCodeError::Submission
                        }
                    });
                    return Err(failed(&error.to_string()));
                }
            }
            if tokio::time::Instant::now() >= deadline {
                note(super::code_runtime::ToolCodeError::Uncertain);
                return Err(uncertain());
            }
            // Every retry uses a fresh Main grant and the identical prepared
            // request/activation. No replacement sandbox identity is generated.
            tokio::time::sleep_until(
                (tokio::time::Instant::now() + code_timing::CODE_RECONCILE_INTERVAL).min(deadline),
            )
            .await;
        }
        }).await
    }
}

#[async_trait]
impl CodeSandboxRuntime for RemoteCodeRuntime {
    /// W3-2 (D3): `items` children with Code need
    /// `ceil(items / min(concurrency, slots)) × item time + margin` before the
    /// run's deadline. The item time is the longest child's Code time (Q2)
    /// and `slots` the smallest among every target used (Q3). A refusal is
    /// one WARN fault line; nothing user-supplied.
    fn admit_fanout(
        &self,
        request: &super::code_runtime::FanoutCodeAdmission<'_>,
    ) -> Result<(), elitea_agent_runtime::graph::fanout::admission::FanoutDeadlineInsufficient>
    {
        let mut items = 0_u32;
        let mut item_time = Duration::ZERO;
        let mut slots = u32::MAX;
        for child in &request.children {
            if let Some((time, child_slots)) = self.child_code_window(child) {
                items = items.saturating_add(1);
                item_time = item_time.max(time);
                slots = slots.min(child_slots);
            }
        }
        if items == 0 {
            return Ok(());
        }
        let admitted = elitea_agent_runtime::graph::fanout::admission::admit_fanout_window(
            items,
            request.concurrency,
            slots,
            item_time,
            self.execution_deadline,
            tokio::time::Instant::now(),
        );
        if let Err(refusal) = &admitted {
            let millis =
                |duration: Duration| u64::try_from(duration.as_millis()).unwrap_or(u64::MAX);
            let (support_ref, _) = self.authority.trace_identity();
            elitea_faults::fault!(
                tracing::Level::WARN,
                code = elitea_faults::codes::GRAPH_FANOUT_DEADLINE_INSUFFICIENT,
                event = "fanout_deadline_insufficient",
                node_id = request.node_id,
                items = items,
                concurrency = request.concurrency,
                slots = slots,
                required_ms = millis(refusal.required),
                remaining_ms = millis(refusal.remaining),
                support_ref = support_ref
            );
        }
        admitted
    }

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
            execution_deadline: self.execution_deadline,
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
        // R1: the marker wraps this one attempt only (C3).
        code_attempt_scope(self.execute_recovery_attempt(invocation, authority)).await
    }

    async fn execute_tool(
        &self,
        invocation: CodeInvocation<'_>,
    ) -> Result<Vec<u8>, super::code_runtime::ToolCodeError> {
        let report = std::sync::Mutex::new(None);
        match Box::pin(code_attempt_scope(
            self.execute_reporting(invocation, Some(&report)),
        ))
        .await
        {
            Ok(receipt) => Ok(receipt),
            Err(error) => Err(report
                .lock()
                .ok()
                .and_then(|mut slot| slot.take())
                .unwrap_or_else(|| super::code_runtime::ToolCodeError::Untyped(error.to_string()))),
        }
    }

    async fn execute(&self, invocation: CodeInvocation<'_>) -> Result<Vec<u8>, GraphError> {
        Box::pin(code_attempt_scope(self.execute_reporting(invocation, None))).await
    }

    async fn execute_node(
        &self,
        invocation: CodeInvocation<'_>,
    ) -> Result<Vec<u8>, (GraphError, Option<&'static str>)> {
        let report = std::sync::Mutex::new(None);
        Box::pin(code_attempt_scope(
            self.execute_reporting(invocation, Some(&report)),
        ))
        .await
        .map_err(|error| {
            let refused = matches!(
                report.lock().ok().and_then(|mut slot| slot.take()),
                Some(super::code_runtime::ToolCodeError::DeadlineInsufficient)
            );
            (
                error,
                refused
                    .then_some(crate::protocol::output::CODE_DEADLINE_INSUFFICIENT_UPSTREAM_CODE),
            )
        })
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
/// The raw path's refusal: the registered user sentence, nothing else.
fn deadline_insufficient() -> GraphError {
    failed(crate::protocol::output::CODE_DEADLINE_INSUFFICIENT_MESSAGE)
}

/// R1: an attempt that already holds a Code slot asked for another.
const NESTED_SLOT_MESSAGE: &str = "A Code attempt cannot take a second Code slot.";

/// A slot wait longer than this gets one INFO line.
const SLOT_WAIT_NOTICE_AFTER: Duration = Duration::from_secs(1);

/// Supervisor Busy (`RESOURCE_EXHAUSTED`) on a Code submission: one INFO
/// line per attempt, however many retries follow, so "no Busy" is provable
/// from the log. The attempt keeps its slot and its existing backoff.
#[derive(Default)]
pub(super) struct SupervisorBusyNotice {
    logged: bool,
}

impl SupervisorBusyNotice {
    pub(super) fn note(
        &mut self,
        code: tonic::Code,
        runtime: &RemoteCodeRuntime,
        profile: &CodeRuntimeProfile,
    ) {
        if code != tonic::Code::ResourceExhausted || self.logged {
            return;
        }
        self.logged = true;
        let (execution_id, _) = runtime.authority.trace_identity();
        elitea_faults::fault!(
            tracing::Level::INFO,
            code = elitea_faults::codes::PIPELINE_CODE_SUPERVISOR_BUSY,
            event = "code_supervisor_busy",
            slots_capacity = profile.slots.capacity(),
            supervisor_target = &*profile.slot_target,
            execution_id = execution_id
        );
    }
}

fn uncertain() -> GraphError {
    failed(
        "Sandbox completion could not be confirmed. The job was not restarted; reconcile its durable status before retrying the pipeline.",
    )
}

fn observation_deadline(timeout_seconds: u32) -> tokio::time::Instant {
    tokio::time::Instant::now()
        + Duration::from_secs(u64::from(timeout_seconds))
        + code_timing::OBSERVATION_MARGIN
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
        slots: CodeSlots::new(1),
        slot_target: "supervisor.fixture:443".into(),
    };
    prepare_job(invocation, &profile)
}

#[cfg(test)]
#[path = "code_remote_authority_tests.rs"]
mod authority_tests;

#[cfg(test)]
#[path = "code_slots_tests.rs"]
mod slots_tests;

#[cfg(test)]
impl CodeRuntimeFactory {
    /// The Code slots of `target`'s normalised Supervisor (W3-1 tests).
    pub(in crate::agents) fn code_slots_for_tests(&self, target: &str) -> Option<CodeSlots> {
        self.code_slots
            .get(&crate::config::code_slot_target(target).ok()?)
            .cloned()
    }

    /// Drive the raw-path hydration step with a bundle and no preparation
    /// profile, so hydration fails before any submission (review L6).
    pub(in crate::agents) async fn raw_hydration_failure_fixture(
        &self,
        authority: Arc<ClaimBoundSandboxAuthority>,
        activation: [u8; 32],
        digest: [u8; 32],
    ) -> Result<(), GraphError> {
        let runtime = RemoteCodeRuntime {
            saved_child_scope: None,
            intent_content: self.intent_content.clone(),
            control: self.control.clone(),
            authority,
            profiles: self.profiles.clone(),
            journal: self.journal.clone(),
            compiled_profile: self.compiled_profile.clone(),
            debug_sink: self.debug_sink.clone(),
            execution_deadline: tokio::time::Instant::now() + Duration::from_secs(3600),
        };
        let profile = runtime
            .profiles
            .iter()
            .find(|profile| profile.platform_client.is_none())
            .expect("a pure profile");
        let job = original_prepared_job_fixture(&CodeInvocation {
            activation,
            language: profile.language,
            platform_client: false,
            source: "return 41",
            dependencies_toml: None,
            provenance: super::code::CodeProvenance::SavedLiteral,
            input_json: b"{}".to_vec(),
            trace: None,
            original: None,
            debug: None,
            workspace: None,
        })?;
        let bundle = crate::sandbox::preparation::frozen_lookup_tests::fixture().1;
        runtime
            .hydrate_or_release(profile, &activation, &digest, &job, &bundle)
            .await
    }
}

#[path = "code_workspace_remote.rs"]
mod workspace_remote;

#[cfg(test)]
#[path = "code_platform_startup_tests.rs"]
mod platform_startup_tests;
