//! Maps an ADK model-call failure to the platform's node failure class.
//!
//! Only the typed [`ErrorCategory`] decides the class. The error message is
//! never read: it may carry provider text, prompts or credentials.
//!
//! Once any model output has started, no failure is retryable, because a
//! re-run would repeat output the user may already have seen. A cancellation
//! is a control stop in both phases and is never reclassified.

use adk_core::{AdkError, ErrorCategory};

use super::node_recovery::NodeFailureClass;

/// Where in the model call the failure happened.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum LlmOutputPhase {
    BeforeFirstOutput,
    AfterOutputStarted,
}

impl LlmOutputPhase {
    /// `pre_output` is true while no model output has been produced yet.
    #[must_use]
    pub const fn from_pre_output(pre_output: bool) -> Self {
        if pre_output {
            Self::BeforeFirstOutput
        } else {
            Self::AfterOutputStarted
        }
    }
}

/// Classify a model-call failure by its ADK category and output phase.
#[must_use]
pub const fn classify_llm_failure(category: ErrorCategory, pre_output: bool) -> NodeFailureClass {
    classify_in_phase(category, LlmOutputPhase::from_pre_output(pre_output))
}

/// Classify an ADK error. Reads the category only, never the message.
#[must_use]
pub fn classify_adk_error(error: &AdkError, pre_output: bool) -> NodeFailureClass {
    classify_llm_failure(error.category, pre_output)
}

const fn classify_in_phase(category: ErrorCategory, phase: LlmOutputPhase) -> NodeFailureClass {
    match (category, phase) {
        (ErrorCategory::Cancelled, _) => NodeFailureClass::Cancelled,
        (_, LlmOutputPhase::AfterOutputStarted) => NodeFailureClass::ModelOutputIncomplete,
        (ErrorCategory::RateLimited, LlmOutputPhase::BeforeFirstOutput) => {
            NodeFailureClass::RateLimited
        }
        (ErrorCategory::Timeout, LlmOutputPhase::BeforeFirstOutput) => {
            NodeFailureClass::AttemptTimeout
        }
        (ErrorCategory::Unavailable, LlmOutputPhase::BeforeFirstOutput) => {
            NodeFailureClass::DependencyUnavailable
        }
        (ErrorCategory::Unauthorized, LlmOutputPhase::BeforeFirstOutput) => {
            NodeFailureClass::AuthenticationDenied
        }
        (ErrorCategory::Forbidden, LlmOutputPhase::BeforeFirstOutput) => {
            NodeFailureClass::AuthorizationDenied
        }
        (ErrorCategory::InvalidInput, LlmOutputPhase::BeforeFirstOutput) => {
            NodeFailureClass::InvalidInput
        }
        (
            ErrorCategory::NotFound | ErrorCategory::Unsupported,
            LlmOutputPhase::BeforeFirstOutput,
        ) => NodeFailureClass::InvalidConfiguration,
        (ErrorCategory::Internal, LlmOutputPhase::BeforeFirstOutput) => NodeFailureClass::Unknown,
    }
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeSet;

    use adk_core::ErrorComponent;

    use super::super::node_recovery::{
        NodeBackoff, NodeFailure, NodeRecoveryPolicy, RecoveryDecision, ReplaySafety,
        RetryCondition, plan_after_failure,
    };
    use super::*;

    const ALL: [ErrorCategory; 10] = [
        ErrorCategory::InvalidInput,
        ErrorCategory::Unauthorized,
        ErrorCategory::Forbidden,
        ErrorCategory::NotFound,
        ErrorCategory::RateLimited,
        ErrorCategory::Timeout,
        ErrorCategory::Unavailable,
        ErrorCategory::Cancelled,
        ErrorCategory::Internal,
        ErrorCategory::Unsupported,
    ];

    /// Retries every retryable condition, so only the class decides.
    fn retrying_policy() -> NodeRecoveryPolicy {
        NodeRecoveryPolicy::admit(
            3,
            BTreeSet::from([
                RetryCondition::DependencyUnavailable,
                RetryCondition::RateLimited,
                RetryCondition::AttemptTimeout,
                RetryCondition::WorkerInterrupted,
            ]),
            Some(NodeBackoff {
                initial_ms: 10,
                maximum_ms: 100,
                multiplier: 2,
            }),
            Some(60_000),
            None,
        )
        .expect("policy admits")
    }

    fn retries(class: NodeFailureClass) -> bool {
        let decision = plan_after_failure(
            &retrying_policy(),
            [7; 32],
            1,
            1_000,
            1_000,
            NodeFailure::new(class, ReplaySafety::NoExternalEffect),
        )
        .expect("decision");
        matches!(decision, RecoveryDecision::RetryAt { .. })
    }

    #[test]
    fn every_category_maps_to_the_exact_class_in_both_phases() {
        use ErrorCategory as C;
        use NodeFailureClass as N;
        let table = [
            (C::InvalidInput, N::InvalidInput),
            (C::Unauthorized, N::AuthenticationDenied),
            (C::Forbidden, N::AuthorizationDenied),
            (C::NotFound, N::InvalidConfiguration),
            (C::RateLimited, N::RateLimited),
            (C::Timeout, N::AttemptTimeout),
            (C::Unavailable, N::DependencyUnavailable),
            (C::Cancelled, N::Cancelled),
            (C::Internal, N::Unknown),
            (C::Unsupported, N::InvalidConfiguration),
        ];
        assert_eq!(table.len(), ALL.len());
        for (category, before) in table {
            assert_eq!(classify_llm_failure(category, true), before, "{category}");
            let after = if category == C::Cancelled {
                N::Cancelled
            } else {
                N::ModelOutputIncomplete
            };
            assert_eq!(classify_llm_failure(category, false), after, "{category}");
        }
    }

    #[test]
    fn started_output_is_never_retryable() {
        for category in ALL {
            let class = classify_llm_failure(category, false);
            assert!(!retries(class), "{category} retried after output started");
            if category != ErrorCategory::Cancelled {
                assert_eq!(class, NodeFailureClass::ModelOutputIncomplete);
            }
        }
    }

    #[test]
    fn only_rate_limit_timeout_and_unavailable_retry_before_output() {
        for category in ALL {
            let expected = matches!(
                category,
                ErrorCategory::RateLimited | ErrorCategory::Timeout | ErrorCategory::Unavailable
            );
            assert_eq!(
                retries(classify_llm_failure(category, true)),
                expected,
                "{category}"
            );
        }
    }

    #[test]
    fn adk_error_is_classified_by_category_only() {
        for category in [ErrorCategory::RateLimited, ErrorCategory::Unauthorized] {
            for pre_output in [true, false] {
                let error = AdkError::new(
                    ErrorComponent::Model,
                    category,
                    "model.test",
                    "provider-message-marker-7f2c",
                )
                .with_upstream_status(500);
                assert_eq!(
                    classify_adk_error(&error, pre_output),
                    classify_llm_failure(category, pre_output)
                );
            }
        }
    }
}
