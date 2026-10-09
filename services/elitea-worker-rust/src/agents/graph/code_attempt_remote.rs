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
        let before = |phase, class| {
            classify(
                phase,
                class,
                authority.dispatch_activation(),
                authority.recovering_started(),
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
        let original_visit = content
            .admit_original_code_visit(
                &self.authority,
                &declaration,
                authority,
                &original_job,
                saved_child_scope.as_ref(),
            )
            .await
            .map_err(|error| {
                intent_failure(
                    &error,
                    CodeAttemptPhase::Admission,
                    invocation.activation,
                    authority.recovering_started(),
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
                    authority.recovering_started(),
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
        self.journal
            .register(
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
        content
            .finalize_original_code_intent(
                &self.authority,
                &original_visit,
                invocation.activation,
                request_digest,
                profile.client.audience(),
                &job,
                selected.as_ref().map(|value| &value.control.binding),
                descriptor,
            )
            .await
            .map_err(|error| {
                intent_failure(
                    &error,
                    CodeAttemptPhase::Admission,
                    invocation.activation,
                    authority.recovering_started(),
                )
            })?;
        if let Some(workspace) = workspace.as_ref() {
            observe_typed(
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
                            authority.recovering_started(),
                        )
                    })
                },
                |_| before(CodeAttemptPhase::Hydration, NodeFailureClass::Unknown),
            )
            .await?;
        }
        if selected.is_none()
            && let Some(bundle) = &bundle
        {
            observe_typed(
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
                    .map_err(|_| before(CodeAttemptPhase::Hydration, NodeFailureClass::Unknown))
                },
                |_| before(CodeAttemptPhase::Hydration, NodeFailureClass::Unknown),
            )
            .await?;
        }
        let mut platform_call_effect = None;
        let result=observe_typed(
            trace.as_ref(),
            CodePhase::Execution,
            async {
                let deadline = observation_deadline(profile.timeout_seconds);
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
                            authority.recovering_started(),
                        ),
                    };
                    let mut idle = code_timing::PLATFORM_PUMP_MIN_INTERVAL;
                    loop {
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
                        use crate::transport::input_content::code_platform_content::CodePlatformStep;
                        match step {
                            CodePlatformStep::Idle => idle = code_timing::next_idle_interval(idle),
                            CodePlatformStep::Committed { call_effect }
                            | CodePlatformStep::Unknown { call_effect } => {
                                idle = code_timing::PLATFORM_PUMP_MIN_INTERVAL;
                                // Retain the exact call for recovery. An unknown
                                // result never grants another effect dispatch.
                                platform_call_effect = Some(call_effect);
                            }
                        }
                        tokio::time::sleep(idle).await;
                    }
                };
                let submission = async {
                    let mut dispatch_possible = authority.recovering_started();
                    let mut backoff = code_timing::PendingBackoff::new();
                    loop {
                        let descriptor = selected
                            .as_ref()
                            .and_then(|selected| selected.control.descriptor_sha256.as_ref())
                            .map(crate::sandbox::compiled_snapshot::ContentSha256::as_str);
                        let signed_intent = content
                            .finalize_original_code_intent(
                                &self.authority,
                                &original_visit,
                                invocation.activation,
                                request_digest,
                                profile.client.audience(),
                                &job,
                                selected.as_ref().map(|selected| &selected.control.binding),
                                descriptor,
                            )
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
                            Err(SandboxCallError::Submission {
                                code:
                                    tonic::Code::Unavailable
                                    | tonic::Code::DeadlineExceeded
                                    | tonic::Code::Aborted
                                    | tonic::Code::ResourceExhausted,
                            }) => dispatch_possible = true,
                            Err(SandboxCallError::Submission { code })
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
                let outcome = super::platform_drive::drive(submission, platform, deadline)
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
                        Err(classify(
                            CodeAttemptPhase::Observation,
                            NodeFailureClass::InvalidResult,
                            invocation.activation,
                            true,
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
fn classify(
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
