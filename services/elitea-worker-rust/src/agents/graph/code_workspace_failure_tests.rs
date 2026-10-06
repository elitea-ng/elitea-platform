use super::CodeWorkspaceFailure;
use crate::{
    agents::graph::{
        code_remote::{
            attempt_remote::workspace_attempt_failure, compiled::CodePreparationFailure,
        },
        code_runtime::CodeAttemptPhase,
        node_recovery::{
            NodeFailureClass, NodeRecoveryPolicy, RecoveryDecision, ReplaySafety, StopReason,
            plan_after_failure,
        },
    },
    sandbox::client::SandboxCallError,
    transport::{InputContentError, InputContentTransportError, control_grpc::ControlGrpcError},
};

const SENSITIVE_CAUSE: &str =
    "PermissionDenied token=synthetic-secret https://private.invalid/repo";

#[test]
fn workspace_content_failure_uses_variant_and_drops_sensitive_cause() {
    for (error, expected) in [
        (
            InputContentError::AuthorizationFailed(SENSITIVE_CAUSE),
            NodeFailureClass::AuthorizationDenied,
        ),
        (
            InputContentError::InvalidConfiguration(SENSITIVE_CAUSE),
            NodeFailureClass::InvalidConfiguration,
        ),
        (
            InputContentError::InvalidInput(SENSITIVE_CAUSE),
            NodeFailureClass::InvalidInput,
        ),
        (
            InputContentError::ResourceExhausted(SENSITIVE_CAUSE),
            NodeFailureClass::InvalidInput,
        ),
        (
            InputContentError::Timeout(SENSITIVE_CAUSE),
            NodeFailureClass::AttemptTimeout,
        ),
        (
            InputContentError::DependencyUnavailable(SENSITIVE_CAUSE),
            NodeFailureClass::DependencyUnavailable,
        ),
        (
            InputContentError::Transport(InputContentTransportError::Unavailable),
            NodeFailureClass::DependencyUnavailable,
        ),
    ] {
        let failure = CodeWorkspaceFailure::input(&error);
        assert_eq!(failure.class(), expected);
        assert!(!failure.to_string().contains(SENSITIVE_CAUSE));
        assert!(!format!("{failure:?}").contains("synthetic-secret"));
        assert!(std::error::Error::source(&failure).is_none());
    }
}

#[test]
fn workspace_guard_classes_use_typed_status_without_message_inference() {
    for (code, expected) in [
        (
            tonic::Code::Unauthenticated,
            NodeFailureClass::AuthenticationDenied,
        ),
        (
            tonic::Code::PermissionDenied,
            NodeFailureClass::AuthorizationDenied,
        ),
        (tonic::Code::Cancelled, NodeFailureClass::Cancelled),
        (tonic::Code::InvalidArgument, NodeFailureClass::InvalidInput),
        (tonic::Code::DataLoss, NodeFailureClass::InvalidResult),
        (
            tonic::Code::DeadlineExceeded,
            NodeFailureClass::AttemptTimeout,
        ),
        (
            tonic::Code::Unavailable,
            NodeFailureClass::DependencyUnavailable,
        ),
        (
            tonic::Code::Aborted,
            NodeFailureClass::DependencyUnavailable,
        ),
        (
            tonic::Code::ResourceExhausted,
            NodeFailureClass::RateLimited,
        ),
        (tonic::Code::Internal, NodeFailureClass::Unknown),
    ] {
        assert_eq!(
            CodeWorkspaceFailure::sandbox(&SandboxCallError::Submission { code }).class(),
            expected,
        );
    }
    for (error, expected) in [
        (SandboxCallError::Invalid, NodeFailureClass::InvalidInput),
        (
            SandboxCallError::InvalidReceipt,
            NodeFailureClass::InvalidResult,
        ),
        (
            SandboxCallError::Rejected,
            NodeFailureClass::AuthorizationDenied,
        ),
        (
            SandboxCallError::Authorization(ControlGrpcError::InvalidConfiguration(
                SENSITIVE_CAUSE,
            )),
            NodeFailureClass::InvalidConfiguration,
        ),
        (
            SandboxCallError::Authorization(ControlGrpcError::ResourceExhausted(SENSITIVE_CAUSE)),
            NodeFailureClass::InvalidInput,
        ),
        (
            SandboxCallError::Authorization(ControlGrpcError::Unavailable(SENSITIVE_CAUSE)),
            NodeFailureClass::DependencyUnavailable,
        ),
    ] {
        let failure = CodeWorkspaceFailure::sandbox(&error);
        assert_eq!(failure.class(), expected);
        assert!(!failure.to_string().contains("PermissionDenied"));
        assert!(!format!("{failure:?}").contains("synthetic-secret"));
    }
}

#[test]
fn acquisition_denial_preserves_original_started_identity_without_authorizing_replay() {
    for (started, replay, stop) in [
        (
            false,
            ReplaySafety::NoExternalEffect,
            StopReason::RetryDisabled,
        ),
        (
            true,
            ReplaySafety::UnknownExternalEffect { effect_id: [7; 32] },
            StopReason::NotRetryable,
        ),
    ] {
        let failure = workspace_attempt_failure(
            CodeWorkspaceFailure::input(&InputContentError::AuthorizationFailed(SENSITIVE_CAUSE)),
            CodeAttemptPhase::Preparation,
            [7; 32],
            started,
        );
        assert_eq!(failure.phase, CodeAttemptPhase::Preparation);
        assert_eq!(failure.failure.class, NodeFailureClass::AuthorizationDenied);
        assert_eq!(failure.failure.replay, replay);
        assert_eq!(
            plan_after_failure(
                &NodeRecoveryPolicy::default(),
                [1; 32],
                1,
                100,
                101,
                failure.failure
            ),
            Ok(RecoveryDecision::Stop(stop)),
        );
    }
}

#[test]
fn hydration_authentication_and_authorization_loss_stop_the_original_started_attempt() {
    for code in [tonic::Code::Unauthenticated, tonic::Code::PermissionDenied] {
        let failure = workspace_attempt_failure(
            CodeWorkspaceFailure::sandbox(&SandboxCallError::Submission { code }),
            CodeAttemptPhase::Hydration,
            [7; 32],
            true,
        );
        assert_eq!(failure.phase, CodeAttemptPhase::Hydration);
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
            Ok(RecoveryDecision::Stop(StopReason::NotRetryable)),
        );
    }
}

#[test]
fn workspace_stop_retains_typed_control_decision_for_fresh_and_started_attempts() {
    for started in [false, true] {
        let failure = workspace_attempt_failure(
            CodeWorkspaceFailure::sandbox(&SandboxCallError::Submission {
                code: tonic::Code::Cancelled,
            }),
            CodeAttemptPhase::Hydration,
            [7; 32],
            started,
        );
        assert_eq!(failure.failure.class, NodeFailureClass::Cancelled);
        assert_eq!(
            failure.failure.replay,
            if started {
                ReplaySafety::UnknownExternalEffect { effect_id: [7; 32] }
            } else {
                ReplaySafety::NoExternalEffect
            },
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
            Ok(RecoveryDecision::Stop(StopReason::ControlDecision)),
        );
    }
}

#[test]
fn workspace_deadline_or_unknown_response_reconciles_the_same_dispatch() {
    for error in [
        CodeWorkspaceFailure::deadline(),
        CodeWorkspaceFailure::sandbox(&SandboxCallError::Submission {
            code: tonic::Code::Internal,
        }),
        CodeWorkspaceFailure::sandbox(&SandboxCallError::InvalidReceipt),
    ] {
        let failure = workspace_attempt_failure(error, CodeAttemptPhase::Hydration, [7; 32], true)
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
                completed_receipt: None
            }),
        );
    }
}

#[test]
fn cold_compile_workspace_failure_remains_typed_without_classifying_legacy_text() {
    let legacy = CodePreparationFailure::from(super::super::super::failed(SENSITIVE_CAUSE));
    assert_eq!(legacy.class(), NodeFailureClass::Unknown);
    assert_eq!(legacy.phase(), CodeAttemptPhase::Preparation);
    for (code, expected) in [
        (
            tonic::Code::PermissionDenied,
            NodeFailureClass::AuthorizationDenied,
        ),
        (tonic::Code::Cancelled, NodeFailureClass::Cancelled),
    ] {
        let preparation = CodePreparationFailure::from(CodeWorkspaceFailure::sandbox(
            &SandboxCallError::Submission { code },
        ));
        assert_eq!(preparation.class(), expected);
        assert_eq!(preparation.phase(), CodeAttemptPhase::Hydration);
        let failure = workspace_attempt_failure(
            CodeWorkspaceFailure::sandbox(&SandboxCallError::Submission { code }),
            preparation.phase(),
            [7; 32],
            true,
        );
        assert_eq!(failure.failure.class, expected);
        assert_eq!(
            failure.failure.replay,
            ReplaySafety::UnknownExternalEffect { effect_id: [7; 32] }
        );
    }
}
