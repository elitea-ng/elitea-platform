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
    DEFAULT_MAX_DEPTH, DelegationPolicy, DelegationPolicyError, NarrowingMode, PrincipalBound,
    PrincipalToolkit,
};

use super::ProtocolError;
use super::elitea::runtime::v1::{WorkerCommandV1, WorkerDelegationModeV1, WorkerPrincipalBoundV1};

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
    let delegation =
        DelegationPolicy::new(max_depth, mode, policy.root_data_classes.iter().cloned())
            .map_err(policy_error)?;
    match policy
        .principal_bound
        .as_ref()
        .filter(|bound| bound.bounded)
    {
        Some(bound) => Ok(delegation.with_principal(principal_bound(bound)?)),
        None => Ok(delegation),
    }
}

/// The run principal's bound (ADR-0044 Amendment 1, #2157). A message that is
/// present and not `bounded` is no bound; one that is `bounded` is the whole
/// bound, so an empty list allows nothing.
fn principal_bound(bound: &WorkerPrincipalBoundV1) -> Result<PrincipalBound, ProtocolError> {
    let toolkits = bound.toolkits.iter().map(|toolkit| PrincipalToolkit {
        id: toolkit.toolkit_id,
        tools: (!toolkit.all_tools).then(|| toolkit.tool_names.iter().cloned().collect()),
    });
    let credentials = bound
        .credentials_bounded
        .then(|| bound.credential_reference_ids.clone());
    PrincipalBound::new(toolkits, credentials, bound.data_classes.iter().cloned())
        .map_err(policy_error)
}

fn policy_error(error: DelegationPolicyError) -> ProtocolError {
    match error {
        DelegationPolicyError::TooManyDataClasses => ProtocolError::ResourceExhausted(
            "the worker delegation policy names too many data classes",
        ),
        DelegationPolicyError::PrincipalBoundTooLarge => ProtocolError::ResourceExhausted(
            "the worker delegation policy's principal bound is too large",
        ),
        DelegationPolicyError::DepthOutOfRange | DelegationPolicyError::MalformedDataClass => {
            ProtocolError::InvalidInput("the worker delegation policy is malformed")
        }
    }
}

#[cfg(test)]
mod tests {
    use super::super::elitea::runtime::v1::{WorkerDelegationPolicyV1, WorkerPrincipalToolkitV1};
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
            ..WorkerDelegationPolicyV1::default()
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

    fn bound() -> WorkerPrincipalBoundV1 {
        WorkerPrincipalBoundV1 {
            bounded: true,
            toolkits: vec![
                WorkerPrincipalToolkitV1 {
                    toolkit_id: 7,
                    all_tools: true,
                    tool_names: Vec::new(),
                },
                WorkerPrincipalToolkitV1 {
                    toolkit_id: 8,
                    all_tools: false,
                    tool_names: vec!["search".to_owned()],
                },
            ],
            credentials_bounded: true,
            credential_reference_ids: vec!["ref-a".to_owned()],
            data_classes: vec!["public".to_owned()],
        }
    }

    fn with_bound(bound: WorkerPrincipalBoundV1) -> WorkerCommandV1 {
        command(Some(WorkerDelegationPolicyV1 {
            principal_bound: Some(bound),
            ..WorkerDelegationPolicyV1::default()
        }))
    }

    #[test]
    fn the_policy_carries_the_principal_bound() {
        let policy = policy_from_command(&with_bound(bound())).expect("policy");
        let principal = policy.principal().expect("a bound");
        assert!(principal.admits_toolkit(Some(7)));
        assert!(principal.admits_toolkit(Some(8)));
        assert!(!principal.admits_toolkit(Some(9)));
        assert!(principal.admits_toolkit(None));
        assert!(principal.admits_credential("ref-a"));
        assert!(!principal.admits_credential("ref-b"));
    }

    #[test]
    fn a_bound_that_is_not_bounded_is_no_bound() {
        let policy = policy_from_command(&with_bound(WorkerPrincipalBoundV1 {
            bounded: false,
            toolkits: bound().toolkits,
            ..WorkerPrincipalBoundV1::default()
        }))
        .expect("policy");
        assert!(policy.principal().is_none());
        assert!(
            policy_from_command(&command(None))
                .expect("default")
                .principal()
                .is_none()
        );
    }

    #[test]
    fn a_bounded_empty_bound_allows_no_toolkit() {
        let policy = policy_from_command(&with_bound(WorkerPrincipalBoundV1 {
            bounded: true,
            ..WorkerPrincipalBoundV1::default()
        }))
        .expect("policy");
        let principal = policy.principal().expect("a bound");
        assert!(!principal.admits_toolkit(Some(7)));
        assert!(
            principal.admits_credential("anything"),
            "no credential bound"
        );
    }

    #[test]
    fn an_oversize_or_malformed_bound_fails_closed() {
        let too_many = WorkerPrincipalBoundV1 {
            bounded: true,
            toolkits: (0..513)
                .map(|id| WorkerPrincipalToolkitV1 {
                    toolkit_id: id,
                    all_tools: true,
                    tool_names: Vec::new(),
                })
                .collect(),
            ..WorkerPrincipalBoundV1::default()
        };
        assert!(matches!(
            policy_from_command(&with_bound(too_many)),
            Err(ProtocolError::ResourceExhausted(_))
        ));
        let bad_class = WorkerPrincipalBoundV1 {
            bounded: true,
            data_classes: vec!["Not A Class".to_owned()],
            ..WorkerPrincipalBoundV1::default()
        };
        assert!(matches!(
            policy_from_command(&with_bound(bad_class)),
            Err(ProtocolError::InvalidInput(_))
        ));
    }
}
