//! Classify the exact Code boundary. Do not parse display messages.

use super::{
    CodeInvocation, RemoteCodeRuntime, SandboxCallError, SandboxOutcome, observation_deadline,
    validate_invocation,
};
use crate::agents::graph::code_runtime::{CodeAttemptFailure, CodeAttemptPhase};
use crate::agents::graph::code_timing;
use crate::agents::graph::code_trace::{CodePhase, observe_typed};
use crate::agents::graph::node_recovery::{NodeFailureClass, ReplaySafety};
use crate::agents::graph::node_recovery_runtime::NodeAttemptAuthority;

impl RemoteCodeRuntime {
    #[allow(
        clippy::items_after_statements,
        reason = "Keep local protocol types beside their exact validation checks."
    )]
    #[allow(
        clippy::large_futures,
        reason = "Keep the bounded owner future lexical without adding allocations."
    )]
    #[allow(
        clippy::manual_let_else,
        reason = "Keep explicit typed owner outcomes beside state checks."
    )]
    #[allow(
        clippy::match_same_arms,
        reason = "Keep distinct receipt and authority outcomes explicit."
    )]
    #[allow(
        clippy::too_many_lines,
        reason = "Keep ordered authority checks and durable phases in one owner operation."
    )]
    pub(super) async fn execute_recovery_attempt(
        &self,
        invocation: CodeInvocation<'_>,
        authority: &NodeAttemptAuthority,
    ) -> Result<Vec<u8>, CodeAttemptFailure> {
        // Durable proof decides whether a previous attempt may have started the
        // user's code. The error kind never does.
        let prior_dispatch = prior_dispatch_possible(
            &self.journal,
            self.authority.dispatch_scope().ok().as_ref(),
            &authority.dispatch_activation(),
            authority.recovering_started(),
        )
        .await;
        let before = |phase, class| {
            classify(
                phase,
                class,
                authority.dispatch_activation(),
                prior_dispatch,
            )
        };
        if invocation.activation != authority.dispatch_activation() {
            return Err(before(
                CodeAttemptPhase::Admission,
                NodeFailureClass::AuthorizationDenied,
            ));
        }
        validate_invocation(&invocation)
            .map_err(|_| before(CodeAttemptPhase::Admission, NodeFailureClass::InvalidInput))?;
        let mut matching = self.profiles.iter().filter(|profile| {
            profile.language == invocation.language
                && profile.platform_client.is_some() == invocation.platform_client
        });
        let profile = matching.next().ok_or_else(|| {
            before(
                CodeAttemptPhase::Admission,
                NodeFailureClass::InvalidConfiguration,
            )
        })?;
        if matching.next().is_some() {
            return Err(before(
                CodeAttemptPhase::Admission,
                NodeFailureClass::InvalidConfiguration,
            ));
        }
        let content = self.intent_content.as_deref().ok_or_else(|| {
            before(
                CodeAttemptPhase::Admission,
                NodeFailureClass::AuthorizationDenied,
            )
        })?;
        let declaration = invocation.original.as_ref().ok_or_else(|| {
            before(
                CodeAttemptPhase::Admission,
                NodeFailureClass::AuthorizationDenied,
            )
        })?;
        let declaration = crate::sandbox::code_recovery::OriginalCodeDeclarationInput {
            node_id: &declaration.node_id,
            graph_thread: &declaration.graph_thread_id,
            step: declaration.graph_step,
            configuration_json: &declaration.configuration_json,
            owning_yaml_sha256: declaration.yaml,
        };
        let original_job = super::prepare_job(&invocation, profile)
            .map_err(|_| before(CodeAttemptPhase::Input, NodeFailureClass::InvalidInput))?;
        let saved_child_scope=match &self.saved_child_scope {
            Some(provider)=>Some(provider.for_visit(declaration.graph_thread,declaration.node_id,crate::agents::pipeline::saved_child_scope_provider::SavedChildPurpose::CodeRecovery).await.map_err(|_|before(CodeAttemptPhase::Admission,NodeFailureClass::AuthorizationDenied))?),
            None=>None,
        };
        // Main being briefly unavailable is never a node failure: retry the same
        // idempotent call until the attempt's own observation deadline.
        let main_deadline = observation_deadline(profile.timeout_seconds);
        let original_visit = retry_transient_main(main_deadline, || {
            content.admit_original_code_visit(
                &self.authority,
                &declaration,
                authority,
                &original_job,
                saved_child_scope.as_ref(),
            )
        })
        .await
        .map_err(|error| {
            intent_failure(
                &error,
                CodeAttemptPhase::Admission,
                invocation.activation,
                prior_dispatch,
            )
        })?;
        let trace = invocation
            .trace
            .as_ref()
            .map(|trace| trace.bind(self.authority.trace_identity()))
            .transpose()
            .map_err(|_| before(CodeAttemptPhase::Admission, NodeFailureClass::Unknown))?;
        let original_prepared = original_job
            .to_transport()
            .map_err(|_| before(CodeAttemptPhase::Input, NodeFailureClass::InvalidInput))?;
        let original_prepared_sha256: [u8; 32] =
            ring::digest::digest(&ring::digest::SHA256, &original_prepared)
                .as_ref()
                .try_into()
                .map_err(|_| before(CodeAttemptPhase::Input, NodeFailureClass::InvalidInput))?;
        super::super::code_debug::export(
            &invocation,
            &original_visit,
            authority,
            &original_prepared_sha256,
            &self.authority,
            self.debug_sink.as_deref(),
            trace.as_ref(),
        )
        .await;
        let (mut job, bundle) = observe_typed(
            trace.as_ref(),
            CodePhase::Preparation,
            async {
                self.prepare_execution_job_diagnosed(&invocation, profile)
                    .await
                    .map_err(|diagnostic| {
                        before(CodeAttemptPhase::Preparation, NodeFailureClass::Unknown)
                            .with_preparation_failure(diagnostic)
                    })
            },
            |_| before(CodeAttemptPhase::Preparation, NodeFailureClass::Unknown),
        )
        .await?;
        // Original visit/debug used the exact revision1 base. Capability joins
        // the final dependency/workspace-selected bytes before intent signing.
        if let Some(binding) = &profile.platform_client {
            job = job.with_platform_client(binding.clone()).map_err(|_| {
                before(
                    CodeAttemptPhase::Admission,
                    NodeFailureClass::InvalidConfiguration,
                )
            })?;
        }
        let (job, workspace) = self
            .acquire_execution_workspace(&invocation, &original_visit, job)
            .await
            .map_err(|error| {
                workspace_attempt_failure(
                    error,
                    CodeAttemptPhase::Preparation,
                    invocation.activation,
                    prior_dispatch,
                )
            })?;
        let selected = self
            .select_compiled_snapshot_typed(
                &invocation,
                profile,
                &job,
                bundle.as_ref(),
                Some(&original_visit),
            )
            .await
            .map_err(|error| before(error.phase(), error.class()))?;
        let scope = self.authority.dispatch_scope().map_err(|_| {
            before(
                CodeAttemptPhase::Admission,
                NodeFailureClass::AuthorizationDenied,
            )
        })?;
        let request_digest = if let Some(selected) = &selected {
            selected
                .control
                .intent_digest(crate::sandbox::compiled_snapshot::Purpose::Execute)
                .map_err(|_| before(CodeAttemptPhase::Admission, NodeFailureClass::InvalidInput))?
        } else {
            job.fingerprint()
                .map_err(|_| before(CodeAttemptPhase::Input, NodeFailureClass::InvalidInput))?
        };
        // Reserve the activation for a stop before hydration, but do not say a
        // submission was sent: that marker is written just before the first
        // submission, so a claim lost during hydration proves "no submission".
        self.journal
            .register_unsubmitted(
                &scope,
                &invocation.activation,
                &request_digest,
                profile.client.audience(),
            )
            .await
            .map_err(|_| {
                before(
                    CodeAttemptPhase::Preparation,
                    NodeFailureClass::DependencyUnavailable,
                )
            })?;
        if let Some(selected) = &selected {
            self.journal
                .record_descriptor(
                    &scope,
                    &invocation.activation,
                    &request_digest,
                    profile.client.audience(),
                    &selected.descriptor_bytes,
                )
                .await
                .map_err(|_| {
                    before(
                        CodeAttemptPhase::Preparation,
                        NodeFailureClass::DependencyUnavailable,
                    )
                })?;
        }
        // Freeze the exact Execute intent before any Execute-mode content
        // grant/hydration. Later refresh returns the same immutable identity.
        let descriptor = selected
            .as_ref()
            .and_then(|value| value.control.descriptor_sha256.as_ref())
            .map(crate::sandbox::compiled_snapshot::ContentSha256::as_str);
        retry_transient_main(main_deadline, || {
            content.finalize_original_code_intent(
                &self.authority,
                &original_visit,
                invocation.activation,
                request_digest,
                profile.client.audience(),
                &job,
                selected.as_ref().map(|value| &value.control.binding),
                descriptor,
            )
        })
        .await
        .map_err(|error| {
            intent_failure(
                &error,
                CodeAttemptPhase::Admission,
                invocation.activation,
                prior_dispatch,
            )
        })?;
        if let Some(workspace) = workspace.as_ref() {
            let hydrated = observe_typed(
                trace.as_ref(),
                CodePhase::Hydration,
                async {
                    self.hydrate_execution_workspace(
                        &invocation,
                        profile,
                        &job,
                        workspace,
                        selected.as_ref(),
                        &original_visit,
                    )
                    .await
                    .map_err(|error| {
                        workspace_attempt_failure(
                            error,
                            CodeAttemptPhase::Hydration,
                            invocation.activation,
                            prior_dispatch,
                        )
                    })
                },
                |_| before(CodeAttemptPhase::Hydration, NodeFailureClass::Unknown),
            )
            .await;
            if let Err(failure) = &hydrated {
                self.release_undispatched(
                    profile,
                    &invocation.activation,
                    &request_digest,
                    failure,
                )
                .await;
            }
            hydrated?;
        }
        if selected.is_none()
            && let Some(bundle) = &bundle
        {
            let hydrated = observe_typed(
                trace.as_ref(),
                CodePhase::Hydration,
                async {
                    self.hydrate_python_dependencies(
                        profile,
                        &invocation.activation,
                        &job,
                        bundle,
                        Some(&original_visit),
                    )
                    .await
                    .map_err(|failure| {
                        before(CodeAttemptPhase::Hydration, NodeFailureClass::Unknown)
                            .with_preparation_failure(failure)
                    })
                },
                |_| before(CodeAttemptPhase::Hydration, NodeFailureClass::Unknown),
            )
            .await;
            if let Err(failure) = &hydrated {
                self.release_undispatched(
                    profile,
                    &invocation.activation,
                    &request_digest,
                    failure,
                )
                .await;
            }
            hydrated?;
        }
        let mut platform_call_effect = None;
        let mut platform_unknown_effect = false;
        let step_tracker = super::platform_drive::StepTracker::default();
        let result=observe_typed(
            trace.as_ref(),
            CodePhase::Execution,
            async {
                let deadline = observation_deadline(profile.timeout_seconds);
                // Durable before the effect: from here a submission may reach the
                // supervisor, and a recovery must read the dispatch as possible.
                self.journal
                    .mark_submission_started(
                        &scope,
                        &invocation.activation,
                        &request_digest,
                        profile.client.audience(),
                    )
                    .await
                    .map_err(|_| {
                        classify(
                            CodeAttemptPhase::Dispatch,
                            NodeFailureClass::DependencyUnavailable,
                            invocation.activation,
                            prior_dispatch,
                        )
                    })?;
                // Keep one pump alive while the exact submission reconciles and backs off.
                // A nonterminal RPC response must not cancel a pending broker step.
                let platform = async {
                    if !invocation.platform_client {
                        return std::future::pending::<CodeAttemptFailure>().await;
                    }
                    let prepared = match job.fingerprint() {
                        Ok(prepared) => prepared,
                        Err(_) => return classify(
                            CodeAttemptPhase::Observation,
                            NodeFailureClass::InvalidInput,
                            invocation.activation,
                            prior_dispatch,
                        ),
                    };
                    let mut idle = code_timing::PLATFORM_PUMP_MIN_INTERVAL;
                    loop {
                        // A step Main began may reach an effect. The tracker says
                        // whether this pump was dropped inside one.
                        let in_step = step_tracker.begin();
                        let step = match content.step_code_platform(
                            &self.authority,
                            invocation.activation,
                            prepared,
                        ).await {
                            Ok(step) => step,
                            Err(error) => return intent_failure(
                                &error,
                                CodeAttemptPhase::Observation,
                                invocation.activation,
                                true,
                            ),
                        };
                        in_step.finish();
                        use crate::transport::input_content::code_platform_content::CodePlatformStep;
                        match step {
                            CodePlatformStep::Idle => idle = code_timing::next_idle_interval(idle),
                            CodePlatformStep::Committed { call_effect } => {
                                idle = code_timing::PLATFORM_PUMP_MIN_INTERVAL;
                                platform_call_effect = Some(call_effect);
                            }
                            CodePlatformStep::Unknown { call_effect } => {
                                idle = code_timing::PLATFORM_PUMP_MIN_INTERVAL;
                                // Retain the exact call for recovery. An unknown
                                // result never grants another effect dispatch.
                                platform_call_effect = Some(call_effect);
                                platform_unknown_effect = true;
                            }
                        }
                        tokio::time::sleep(idle).await;
                    }
                };
                let submission = async {
                    let mut dispatch_possible = prior_dispatch;
                    let mut backoff = code_timing::PendingBackoff::new();
                    loop {
                        let descriptor = selected
                            .as_ref()
                            .and_then(|selected| selected.control.descriptor_sha256.as_ref())
                            .map(crate::sandbox::compiled_snapshot::ContentSha256::as_str);
                        let signed_intent = retry_transient_main(deadline, || {
                            content.finalize_original_code_intent(
                                &self.authority,
                                &original_visit,
                                invocation.activation,
                                request_digest,
                                profile.client.audience(),
                                &job,
                                selected.as_ref().map(|selected| &selected.control.binding),
                                descriptor,
                            )
                        })
                        .await
                        .map_err(|error| {
                                intent_failure(
                                    &error,
                                    CodeAttemptPhase::Admission,
                                    invocation.activation,
                                    dispatch_possible,
                                )
                            })?;
                        let attempt = async {
                            if let Some(selected) = &selected {
                                self.control
                                    .submit_compiled_whole_code(
                                        &profile.client,
                                        &self.authority,
                                        &invocation.activation,
                                        &job,
                                        selected,
                                        bundle.as_ref(),
                                        &signed_intent,
                                    )
                                    .await
                            } else {
                                self.control
                                    .submit_whole_code(
                                        &profile.client,
                                        &self.authority,
                                        &invocation.activation,
                                        &job,
                                        bundle.as_ref(),
                                        &signed_intent,
                                    )
                                    .await
                            }
                        };
                        let outcome = attempt.await;
                        match outcome {
                            Ok(outcome @ (SandboxOutcome::Completed(_)
                                | SandboxOutcome::Cancelled
                                | SandboxOutcome::Failed { .. }
                                | SandboxOutcome::Uncertain { .. })) => return Ok(outcome),
                            Ok(SandboxOutcome::Pending) => {
                                dispatch_possible = true;
                            },
                            Err(SandboxCallError::Authorization(
                                crate::transport::control_grpc::ControlGrpcError::Unavailable(_),
                            )) => {
                                return Err(classify(
                                    CodeAttemptPhase::Dispatch,
                                    NodeFailureClass::DependencyUnavailable,
                                    invocation.activation,
                                    dispatch_possible,
                                ));
                            }
                            Err(SandboxCallError::Authorization(_) | SandboxCallError::Rejected) => {
                                return Err(classify(
                                    CodeAttemptPhase::Admission,
                                    NodeFailureClass::AuthorizationDenied,
                                    invocation.activation,
                                    dispatch_possible,
                                ));
                            }
                            Err(SandboxCallError::Submission { code:
                                    tonic::Code::Unavailable
                                    | tonic::Code::DeadlineExceeded
                                    | tonic::Code::Aborted
                                    | tonic::Code::ResourceExhausted, .. }) => dispatch_possible = true,
                            Err(SandboxCallError::Submission { code, .. })
                                if submission_control_failure(code, invocation.activation)
                                    .is_some() =>
                            {
                                return Err(submission_control_failure(code, invocation.activation)
                                    .ok_or_else(|| classify(
                                        CodeAttemptPhase::Observation,
                                        NodeFailureClass::Unknown,
                                        invocation.activation,
                                        true,
                                    ))?);
                            }
                            Err(
                                SandboxCallError::Submission { .. } | SandboxCallError::InvalidReceipt,
                            ) => {
                                return Err(classify(
                                    CodeAttemptPhase::Observation,
                                    NodeFailureClass::Unknown,
                                    invocation.activation,
                                    true,
                                ));
                            }
                            Err(SandboxCallError::Invalid) => {
                                return Err(classify(
                                    CodeAttemptPhase::Admission,
                                    NodeFailureClass::InvalidInput,
                                    invocation.activation,
                                    dispatch_possible,
                                ));
                            }
                        }
                        if tokio::time::Instant::now() >= deadline {
                            return Err(classify(
                                CodeAttemptPhase::Observation,
                                NodeFailureClass::AttemptTimeout,
                                invocation.activation,
                                dispatch_possible,
                            ));
                        }
                        // This is transport reconciliation for the exact attempt identity.
                        tokio::time::sleep_until(
                            (tokio::time::Instant::now() + backoff.next(code_timing::jitter_sample()))
                                .min(deadline),
                        )
                        .await;
                    }
                };
                let outcome = super::platform_drive::drive_tracked(
                    submission,
                    platform,
                    deadline,
                    &step_tracker,
                    code_timing::PLATFORM_STEP_SETTLE_GRACE,
                )
                    .await
                    .map_err(|error| match error {
                        super::platform_drive::ObservationFailure::Deadline => classify(
                            CodeAttemptPhase::Observation,
                            NodeFailureClass::AttemptTimeout,
                            invocation.activation,
                            true,
                        ),
                        super::platform_drive::ObservationFailure::Platform(failure) => failure,
                    })??;
                // Stop the pump as soon as the terminal RPC receipt wins. Journal
                // persistence must not let a late owner refusal replace that receipt.
                match outcome {
                    SandboxOutcome::Completed(receipt) => {
                        self.journal
                            .resolve(
                                &scope,
                                &invocation.activation,
                                &request_digest,
                                profile.client.audience(),
                            )
                            .await
                            .map_err(|_| {
                                classify(
                                    CodeAttemptPhase::Observation,
                                    NodeFailureClass::DependencyUnavailable,
                                    invocation.activation,
                                    true,
                                )
                            })?;
                        Ok(receipt)
                    }
                    SandboxOutcome::Cancelled => {
                        let _resolved = self
                            .journal
                            .resolve(
                                &scope,
                                &invocation.activation,
                                &request_digest,
                                profile.client.audience(),
                            )
                            .await;
                        Err(classify(
                            CodeAttemptPhase::Observation,
                            NodeFailureClass::Cancelled,
                            invocation.activation,
                            true,
                        ))
                    }
                    SandboxOutcome::Failed { .. } => {
                        let _resolved = self
                            .journal
                            .resolve(
                                &scope,
                                &invocation.activation,
                                &request_digest,
                                profile.client.audience(),
                            )
                            .await;
                        // The runtime reached a definite terminal phase. It has no
                        // network, so only a platform-client call can have left an
                        // effect, and Main's durable platform-call journal decides,
                        // not the failure kind. A step dropped mid-flight, an
                        // unknown call seen by this pump, or any journal answer
                        // other than "nothing unresolved" keeps the effect possible.
                        let effect_possible = invocation.platform_client
                            && (platform_unknown_effect
                                || step_tracker.interrupted()
                                || match job.fingerprint() {
                                    Ok(prepared) => {
                                        platform_effects_possible(|| {
                                            content.code_platform_effects(
                                                &self.authority,
                                                invocation.activation,
                                                prepared,
                                            )
                                        })
                                        .await
                                    }
                                    Err(_) => true,
                                });
                        Err(classify(
                            CodeAttemptPhase::Observation,
                            NodeFailureClass::InvalidResult,
                            invocation.activation,
                            effect_possible,
                        ))
                    }
                    SandboxOutcome::Uncertain { .. } => {
                        Err(classify(
                            CodeAttemptPhase::Observation,
                            NodeFailureClass::Unknown,
                            invocation.activation,
                            true,
                        ))
                    }
                    SandboxOutcome::Pending => Err(classify(
                        CodeAttemptPhase::Observation,
                        NodeFailureClass::Unknown,
                        invocation.activation,
                        true,
                    )),
                }
            },
            |_| {
                classify(
                    CodeAttemptPhase::Observation,
                    NodeFailureClass::Unknown,
                    invocation.activation,
                    true,
                )
            },
        )
        .await;
        result.map_err(|failure| failure.with_platform_call_effect(platform_call_effect))
    }
}

impl RemoteCodeRuntime {
    /// A job reserved and hydrated for this attempt that failed before any
    /// dispatch holds a live runtime nothing else will delete soon. Release it
    /// with the claim-bound stop while this attempt still owns the claim. The
    /// release is best effort and idempotent: a repeat finds the terminal job,
    /// and the supervisor's retention sweep remains the backstop. A failure
    /// after a possible dispatch is never released here (reconcile owns it).
    async fn release_undispatched(
        &self,
        profile: &super::CodeRuntimeProfile,
        activation: &[u8; 32],
        digest: &[u8; 32],
        failure: &CodeAttemptFailure,
    ) {
        if failure.failure.replay != ReplaySafety::NoExternalEffect {
            return;
        }
        self.release_hydrated_job(profile, activation, digest).await;
    }

    /// The release itself, for a caller that knows no submission was sent (the
    /// raw `execute` path never reaches a submission after a hydration failure).
    pub(super) async fn release_hydrated_job(
        &self,
        profile: &super::CodeRuntimeProfile,
        activation: &[u8; 32],
        digest: &[u8; 32],
    ) {
        use crate::protocol::elitea::runtime::v1::SandboxJobStatusV1;
        let Ok(scope) = self.authority.dispatch_scope() else {
            return;
        };
        match self
            .control
            .release_undispatched_sandbox_job(&profile.client, &self.authority, activation, digest)
            .await
        {
            Ok(
                SandboxJobStatusV1::Completed
                | SandboxJobStatusV1::Failed
                | SandboxJobStatusV1::Cancelled,
            ) => {
                if self
                    .journal
                    .resolve(&scope, activation, digest, profile.client.audience())
                    .await
                    .is_err()
                {
                    tracing::warn!(
                        event = "code_job_release_unrecorded",
                        "released sandbox job could not be marked resolved"
                    );
                }
                tracing::info!(event = "code_job_released_after_failure");
            }
            Ok(_) => {
                tracing::warn!(
                    event = "code_job_release_pending",
                    "sandbox job release is pending; the supervisor retention sweep remains"
                );
            }
            Err(_) => {
                tracing::warn!(
                    event = "code_job_release_failed",
                    "sandbox job release could not be requested; the supervisor retention sweep remains"
                );
            }
        }
    }
}

/// Whether user code may already have been dispatched for this activation.
///
/// A recovered `Running` journal says only that an earlier attempt began. The
/// Worker registers the execution's dispatch row before hydration, so the row
/// alone proves nothing, and it durably marks the row just before the first
/// execute submission is sent. With no row, or a row still pending its
/// submission, no submission was ever sent and a failure before dispatch (for
/// example a lost preparation job, or a claim lost during hydration) repeats
/// nothing. A submitted row, a missing scope or a lookup error keep the
/// conservative answer: the effect may have happened.
pub(in crate::agents::graph) async fn prior_dispatch_possible(
    journal: &crate::sandbox::dispatch::DispatchJournal,
    scope: Option<&crate::sandbox::dispatch::DispatchScope>,
    activation: &[u8; 32],
    recovering_started: bool,
) -> bool {
    if !recovering_started {
        return false;
    }
    let Some(scope) = scope else {
        return true;
    };
    journal
        .submission_started(scope, activation)
        .await
        .unwrap_or(true)
}

/// Retry one idempotent Main call while it reports a transient condition
/// (unavailable, transport, timeout) until `deadline`. Any other error, and the
/// last transient one at the deadline, is returned unchanged.
pub(in crate::agents::graph) async fn retry_transient_main<T, F, Fut>(
    deadline: tokio::time::Instant,
    mut call: F,
) -> Result<T, crate::transport::InputContentError>
where
    F: FnMut() -> Fut,
    Fut: std::future::Future<Output = Result<T, crate::transport::InputContentError>>,
{
    loop {
        match call().await {
            Err(error) if error.retryable() && tokio::time::Instant::now() < deadline => {
                tracing::warn!(
                    event = "code_main_call_retrying",
                    reason = error.code(),
                    "Main was unavailable for a Code intent call; retrying"
                );
                tokio::time::sleep_until(
                    (tokio::time::Instant::now() + code_timing::CODE_RECONCILE_INTERVAL)
                        .min(deadline),
                )
                .await;
            }
            other => return other,
        }
    }
}

/// How long the platform-journal proof retries Main after a terminal runtime
/// phase before it gives up and keeps the effect possible.
const PLATFORM_PROOF_RETRY_BUDGET: std::time::Duration = std::time::Duration::from_secs(30);

/// Ask Main's platform-call journal whether any call of this activation has an
/// unknown outcome once the runtime is terminal. The answer is the read-only,
/// fenced and authorized `code-platform/effects` query. Only an explicit
/// `None` proves the outcome known: a refusal, an unreadable journal after
/// the retry budget, and a `Possible` answer all keep the effect possible.
pub(in crate::agents::graph) async fn platform_effects_possible<F, Fut>(mut proof: F) -> bool
where
    F: FnMut() -> Fut,
    Fut: std::future::Future<
            Output = Result<
                crate::transport::input_content::code_platform_content::CodePlatformEffects,
                crate::transport::InputContentError,
            >,
        >,
{
    use crate::transport::input_content::code_platform_content::CodePlatformEffects;
    let deadline = tokio::time::Instant::now() + PLATFORM_PROOF_RETRY_BUDGET;
    !matches!(
        retry_transient_main(deadline, &mut proof).await,
        Ok(CodePlatformEffects::None)
    )
}

pub(super) fn workspace_attempt_failure(
    error: super::workspace_remote::CodeWorkspaceFailure,
    phase: CodeAttemptPhase,
    activation: [u8; 32],
    dispatch_possible: bool,
) -> CodeAttemptFailure {
    classify(phase, error.class(), activation, dispatch_possible)
}

fn intent_failure(
    error: &crate::transport::InputContentError,
    phase: CodeAttemptPhase,
    activation: [u8; 32],
    dispatch_possible: bool,
) -> CodeAttemptFailure {
    let class = match error {
        crate::transport::InputContentError::AuthorizationFailed(_) => {
            NodeFailureClass::AuthorizationDenied
        }
        crate::transport::InputContentError::ResourceExhausted(_) => NodeFailureClass::InvalidInput,
        _ => NodeFailureClass::DependencyUnavailable,
    };
    classify(phase, class, activation, dispatch_possible)
}
fn submission_control_failure(
    code: tonic::Code,
    activation: [u8; 32],
) -> Option<CodeAttemptFailure> {
    let class = match code {
        tonic::Code::Unauthenticated => NodeFailureClass::AuthenticationDenied,
        tonic::Code::PermissionDenied => NodeFailureClass::AuthorizationDenied,
        tonic::Code::Cancelled => NodeFailureClass::Cancelled,
        _ => return None,
    };
    // A response cannot prove whether prior dispatch/effects occurred. The typed
    // denial/cancellation nevertheless stops the pipeline and never grants replay.
    Some(classify(
        CodeAttemptPhase::Observation,
        class,
        activation,
        true,
    ))
}
pub(in crate::agents::graph) fn classify(
    phase: CodeAttemptPhase,
    class: NodeFailureClass,
    activation: [u8; 32],
    dispatch_possible: bool,
) -> CodeAttemptFailure {
    let replay = if dispatch_possible {
        ReplaySafety::UnknownExternalEffect {
            effect_id: activation,
        }
    } else {
        ReplaySafety::NoExternalEffect
    };
    CodeAttemptFailure::new(phase, class, replay)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::transport::InputContentError;

    #[tokio::test(start_paused = true)]
    async fn transient_main_errors_retry_until_the_deadline_and_others_do_not() {
        let deadline = tokio::time::Instant::now() + std::time::Duration::from_mins(10);
        // Main back after two transient errors: the call succeeds, it never failed the node.
        let mut calls = 0;
        let result = retry_transient_main(deadline, || {
            calls += 1;
            let attempt = calls;
            async move {
                if attempt < 3 {
                    Err(InputContentError::DependencyUnavailable("main restarting"))
                } else {
                    Ok(attempt)
                }
            }
        })
        .await;
        assert_eq!(result.unwrap(), 3);
        // An authorization refusal is final at once.
        let mut calls = 0;
        let refused = retry_transient_main(deadline, || {
            calls += 1;
            async { Err::<(), _>(InputContentError::AuthorizationFailed("refused")) }
        })
        .await;
        assert!(matches!(
            refused,
            Err(InputContentError::AuthorizationFailed(_))
        ));
        assert_eq!(calls, 1);
        // A Main that never returns ends at the deadline with the last error.
        let near = tokio::time::Instant::now() + std::time::Duration::from_secs(30);
        let down = retry_transient_main(near, || async {
            Err::<(), _>(InputContentError::Timeout("main down"))
        })
        .await;
        assert!(matches!(down, Err(InputContentError::Timeout(_))));
        assert!(tokio::time::Instant::now() >= near);
    }

    #[test]
    fn broker_call_detail_never_replaces_the_whole_code_recovery_identity() {
        let failure = classify(
            CodeAttemptPhase::Observation,
            NodeFailureClass::Unknown,
            [7; 32],
            true,
        )
        .with_platform_call_effect(Some([8; 32]));
        assert_eq!(failure.platform_call_effect, Some([8; 32]));
        assert_eq!(
            failure.failure.replay,
            ReplaySafety::UnknownExternalEffect { effect_id: [7; 32] }
        );
    }
    #[test]
    fn never_settled_platform_call_hits_whole_deadline_without_node_replay() {
        use crate::agents::graph::node_recovery::{
            NodeRecoveryPolicy, RecoveryDecision, plan_after_failure,
        };
        let failure = classify(
            CodeAttemptPhase::Observation,
            NodeFailureClass::AttemptTimeout,
            [7; 32],
            true,
        )
        .with_platform_call_effect(Some([8; 32]));
        assert_eq!(failure.platform_call_effect, Some([8; 32]));
        assert_eq!(
            failure.failure.replay,
            ReplaySafety::UnknownExternalEffect { effect_id: [7; 32] }
        );
        assert_eq!(
            plan_after_failure(
                &NodeRecoveryPolicy::default(),
                [1; 32],
                1,
                100,
                101,
                failure.failure
            ),
            Ok(RecoveryDecision::Reconcile {
                effect_id: Some([7; 32]),
                completed_receipt: None,
            })
        );
    }
    #[test]
    fn platform_observation_claim_loss_and_stop_preserve_control_stop_with_call_detail() {
        use crate::agents::graph::node_recovery::{
            NodeRecoveryPolicy, RecoveryDecision, StopReason, plan_after_failure,
        };
        let lost = intent_failure(
            &crate::transport::InputContentError::AuthorizationFailed("original broker claim lost"),
            CodeAttemptPhase::Observation,
            [7; 32],
            true,
        );
        let cancelled = submission_control_failure(tonic::Code::Cancelled, [7; 32]).unwrap();
        for (failure, expected_stop) in [
            (lost, StopReason::NotRetryable),
            (cancelled, StopReason::ControlDecision),
        ] {
            let failure = failure.with_platform_call_effect(Some([8; 32]));
            assert_eq!(failure.platform_call_effect, Some([8; 32]));
            assert_eq!(
                failure.failure.replay,
                ReplaySafety::UnknownExternalEffect { effect_id: [7; 32] }
            );
            assert_eq!(
                plan_after_failure(
                    &NodeRecoveryPolicy::default(),
                    [1; 32],
                    1,
                    100,
                    101,
                    failure.failure
                ),
                Ok(RecoveryDecision::Stop(expected_stop))
            );
        }
    }
    #[test]
    fn typed_supervisor_guard_rejection_or_cancel_stops_even_after_ambiguous_dispatch() {
        use crate::agents::graph::node_recovery::{
            NodeRecoveryPolicy, RecoveryDecision, StopReason, plan_after_failure,
        };
        for (code, class, expected_stop) in [
            (
                tonic::Code::Unauthenticated,
                NodeFailureClass::AuthenticationDenied,
                StopReason::NotRetryable,
            ),
            (
                tonic::Code::PermissionDenied,
                NodeFailureClass::AuthorizationDenied,
                StopReason::NotRetryable,
            ),
            (
                tonic::Code::Cancelled,
                NodeFailureClass::Cancelled,
                StopReason::ControlDecision,
            ),
        ] {
            let failure = submission_control_failure(code, [7; 32]).unwrap();
            assert_eq!(failure.failure.class, class);
            assert_eq!(
                failure.failure.replay,
                ReplaySafety::UnknownExternalEffect { effect_id: [7; 32] }
            );
            assert_eq!(
                plan_after_failure(
                    &NodeRecoveryPolicy::default(),
                    [1; 32],
                    1,
                    100,
                    101,
                    failure.failure
                ),
                Ok(RecoveryDecision::Stop(expected_stop))
            );
        }
        assert!(submission_control_failure(tonic::Code::Unavailable, [7; 32]).is_none());
    }
    #[test]
    fn typed_code_attempt_pre_dispatch_failure_is_safe_but_recovered_started_is_unknown() {
        let fresh = classify(
            CodeAttemptPhase::Dispatch,
            NodeFailureClass::DependencyUnavailable,
            [1; 32],
            false,
        );
        let recovered = classify(
            CodeAttemptPhase::Dispatch,
            NodeFailureClass::DependencyUnavailable,
            [1; 32],
            true,
        );
        assert_eq!(fresh.failure.replay, ReplaySafety::NoExternalEffect);
        assert_eq!(
            recovered.failure.replay,
            ReplaySafety::UnknownExternalEffect { effect_id: [1; 32] }
        );
    }
    #[test]
    fn typed_code_attempt_denial_and_cancel_preserve_their_own_reason() {
        for class in [
            NodeFailureClass::AuthorizationDenied,
            NodeFailureClass::Cancelled,
        ] {
            assert_eq!(
                classify(CodeAttemptPhase::Observation, class, [1; 32], true)
                    .failure
                    .class,
                class
            );
        }
    }
}
