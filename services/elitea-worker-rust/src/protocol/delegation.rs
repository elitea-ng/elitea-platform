//! The command's delegation policy (ADR-0044 decision 3, #1501).
//!
//! Main puts `WorkerCommandV1.delegation_policy` (field 67) on a signed agent
//! command when a value differs from the default: the administrator's
//! maximum delegation depth and narrowing mode (platform configuration
//! section `delegation`), and the root agent's declared data classes (its
//! agent registry row). Absent is the default: depth 4, enforced, no root
//! data class. A depth out of range, an unknown mode or a malformed class
//! fails the command closed at verification.

use elitea_agent_runtime::delegation::{
    DEFAULT_MAX_DEPTH, DelegationPolicy, DelegationPolicyError, NarrowingMode,
};

use super::ProtocolError;
use super::elitea::runtime::v1::{WorkerCommandV1, WorkerDelegationModeV1};

/// The command's policy, validated.
///
/// # Errors
///
/// [`ProtocolError::InvalidInput`] for a depth out of range, an unknown mode
/// or a malformed data class, [`ProtocolError::ResourceExhausted`] for more
/// than 32 data classes.
pub fn policy_from_command(command: &WorkerCommandV1) -> Result<DelegationPolicy, ProtocolError> {
    let Some(policy) = command.delegation_policy.as_ref() else {
        return Ok(DelegationPolicy::default());
    };
    let mode = match WorkerDelegationModeV1::try_from(policy.mode) {
        Ok(WorkerDelegationModeV1::Unspecified | WorkerDelegationModeV1::Enforce) => {
            NarrowingMode::Enforce
        }
        Ok(WorkerDelegationModeV1::Report) => NarrowingMode::Report,
        Err(_) => {
            return Err(ProtocolError::InvalidInput(
                "the worker delegation mode is not supported",
            ));
        }
    };
    let max_depth = if policy.max_depth == 0 {
        DEFAULT_MAX_DEPTH
    } else {
        policy.max_depth
    };
    DelegationPolicy::new(max_depth, mode, policy.root_data_classes.iter().cloned()).map_err(
        |error| match error {
            DelegationPolicyError::TooManyDataClasses => ProtocolError::ResourceExhausted(
                "the worker delegation policy names too many data classes",
            ),
            DelegationPolicyError::DepthOutOfRange | DelegationPolicyError::MalformedDataClass => {
                ProtocolError::InvalidInput("the worker delegation policy is malformed")
            }
        },
    )
}

#[cfg(test)]
mod tests {
    use super::super::elitea::runtime::v1::WorkerDelegationPolicyV1;
    use super::*;

    fn command(policy: Option<WorkerDelegationPolicyV1>) -> WorkerCommandV1 {
        WorkerCommandV1 {
            delegation_policy: policy,
            ..WorkerCommandV1::default()
        }
    }

    #[test]
    fn absent_is_the_default_policy() {
        let policy = policy_from_command(&command(None)).expect("default");
        assert_eq!(policy, DelegationPolicy::default());
        assert_eq!(policy.max_depth(), 4);
    }

    #[test]
    fn the_policy_carries_depth_mode_and_root_classes() {
        let policy = policy_from_command(&command(Some(WorkerDelegationPolicyV1 {
            max_depth: 2,
            mode: WorkerDelegationModeV1::Report as i32,
            root_data_classes: vec!["pii".to_owned(), "finance.restricted".to_owned()],
        })))
        .expect("policy");
        assert_eq!(policy.max_depth(), 2);
        assert_eq!(policy.mode(), NarrowingMode::Report);
        assert_eq!(policy.root_data_classes().len(), 2);
        let zero = policy_from_command(&command(Some(WorkerDelegationPolicyV1::default())))
            .expect("zero is the default depth");
        assert_eq!(zero.max_depth(), 4);
    }

    #[test]
    fn a_malformed_policy_fails_closed() {
        for policy in [
            WorkerDelegationPolicyV1 {
                max_depth: 17,
                ..WorkerDelegationPolicyV1::default()
            },
            WorkerDelegationPolicyV1 {
                mode: 9,
                ..WorkerDelegationPolicyV1::default()
            },
            WorkerDelegationPolicyV1 {
                root_data_classes: vec!["Not A Class".to_owned()],
                ..WorkerDelegationPolicyV1::default()
            },
        ] {
            assert!(matches!(
                policy_from_command(&command(Some(policy))),
                Err(ProtocolError::InvalidInput(_))
            ));
        }
        assert!(matches!(
            policy_from_command(&command(Some(WorkerDelegationPolicyV1 {
                root_data_classes: (0..33).map(|n| format!("c{n}")).collect(),
                ..WorkerDelegationPolicyV1::default()
            }))),
            Err(ProtocolError::ResourceExhausted(_))
        ));
    }
}
