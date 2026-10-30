//! The command's approval policy (ADR-0041 decision 5, #1498).
//!
//! Main puts `WorkerCommandV1.approval_policy` (field 68) on a signed agent
//! command once an administrator saved the platform configuration section
//! `approval_policy`: the root agent's risk tier (its agent registry row) and
//! the categories whose tools need an approval at that tier. Absent is no
//! policy rule: only the platform's sensitive action tools ask, as before
//! this release. An unknown tier or category, or a category named twice,
//! fails the command closed at verification: reading it as fewer approvals
//! would weaken the policy.

use std::collections::BTreeSet;
use std::sync::Arc;

use elitea_agent_runtime::approval_policy::{ApprovalPolicy, RiskTier};
use elitea_agent_runtime::tool_category::ToolCategory;

use super::ProtocolError;
use super::elitea::runtime::v1::WorkerCommandV1;

/// The command's policy, validated.
///
/// # Errors
///
/// [`ProtocolError::InvalidInput`] for an unknown tier or category, or a
/// category named twice.
pub fn policy_from_command(
    command: &WorkerCommandV1,
) -> Result<Arc<ApprovalPolicy>, ProtocolError> {
    let Some(policy) = command.approval_policy.as_ref() else {
        return Ok(ApprovalPolicy::none());
    };
    let tier = RiskTier::parse(&policy.risk_tier).ok_or(ProtocolError::InvalidInput(
        "the worker approval policy names an unknown risk tier",
    ))?;
    let mut categories = BTreeSet::new();
    for name in &policy.approval_categories {
        let category = ToolCategory::parse(name).ok_or(ProtocolError::InvalidInput(
            "the worker approval policy names an unknown tool category",
        ))?;
        if !categories.insert(category) {
            return Err(ProtocolError::InvalidInput(
                "the worker approval policy names a tool category twice",
            ));
        }
    }
    Ok(Arc::new(ApprovalPolicy::new(tier, categories)))
}

#[cfg(test)]
mod tests {
    use super::super::elitea::runtime::v1::WorkerApprovalPolicyV1;
    use super::*;

    fn command(policy: Option<WorkerApprovalPolicyV1>) -> WorkerCommandV1 {
        WorkerCommandV1 {
            approval_policy: policy,
            ..WorkerCommandV1::default()
        }
    }

    #[test]
    fn absent_is_no_policy_rule() {
        let policy = policy_from_command(&command(None)).expect("no policy");
        assert_eq!(policy.tier(), None);
        assert!(policy.categories().is_empty());
    }

    #[test]
    fn the_policy_carries_the_tier_and_its_categories() {
        let policy = policy_from_command(&command(Some(WorkerApprovalPolicyV1 {
            risk_tier: "high".to_owned(),
            approval_categories: vec!["delete".to_owned(), "external_send".to_owned()],
        })))
        .expect("policy");
        assert_eq!(policy.tier(), Some(RiskTier::High));
        assert!(policy.rule_for(ToolCategory::Delete).is_some());
        assert!(policy.rule_for(ToolCategory::Payment).is_none());
        // A saved row that names no category at the tier asks for nothing.
        let empty = policy_from_command(&command(Some(WorkerApprovalPolicyV1 {
            risk_tier: "low".to_owned(),
            approval_categories: Vec::new(),
        })))
        .expect("empty");
        assert_eq!(empty.tier(), Some(RiskTier::Low));
        assert!(empty.categories().is_empty());
    }

    #[test]
    fn a_malformed_policy_fails_closed() {
        for (tier, categories) in [
            ("", vec![]),
            ("extreme", vec![]),
            ("High", vec![]),
            ("high", vec!["admin"]),
            ("high", vec!["delete", "delete"]),
        ] {
            assert!(
                matches!(
                    policy_from_command(&command(Some(WorkerApprovalPolicyV1 {
                        risk_tier: tier.to_owned(),
                        approval_categories: categories.into_iter().map(str::to_owned).collect(),
                    }))),
                    Err(ProtocolError::InvalidInput(_))
                ),
                "{tier}"
            );
        }
    }
}
