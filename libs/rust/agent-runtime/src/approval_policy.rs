//! The approval policy (ADR-0041 decision 5, #1498).
//!
//! A tool call needs a human approval when one of these rules names it:
//!
//! * the platform rule: the administrator's sensitive action tools
//!   (`toolkit_security.sensitive_tools`);
//! * the policy rule: the tool's category ([`crate::tool_category`]) is one
//!   the administrator's approval policy names for the run's risk tier.
//!
//! The rules only add approvals. Nothing an agent author sets removes one:
//! the policy comes from the signed command, and a toolkit setting cannot
//! move a declared tool to another category.
//!
//! Main sends the policy (command field 68) only once an administrator saved
//! the `approval_policy` section, with the root agent's risk tier. Until then
//! the run has no policy rule ([`ApprovalPolicy::none`]): the platform rule
//! is the only one, as before this release. Nested callees use the root
//! agent's tier.

use std::collections::BTreeSet;
use std::fmt;
use std::sync::Arc;

use serde_json::{Value, json};

use crate::tool_category::ToolCategory;

/// An agent's risk tier (its registry row, ADR-0040).
#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub enum RiskTier {
    Low,
    Medium,
    High,
    Critical,
}

impl RiskTier {
    /// Every tier, lowest first.
    pub const ALL: [Self; 4] = [Self::Low, Self::Medium, Self::High, Self::Critical];

    /// The wire name.
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Low => "low",
            Self::Medium => "medium",
            Self::High => "high",
            Self::Critical => "critical",
        }
    }

    /// The tier a wire name names, exactly.
    #[must_use]
    pub fn parse(value: &str) -> Option<Self> {
        Self::ALL.into_iter().find(|tier| tier.as_str() == value)
    }

    /// The ADR default: `delete`, `external_send` and `payment` need an
    /// approval at the high and critical tiers, nothing at the others.
    #[must_use]
    pub fn default_categories(self) -> BTreeSet<ToolCategory> {
        match self {
            Self::Low | Self::Medium => BTreeSet::new(),
            Self::High | Self::Critical => BTreeSet::from([
                ToolCategory::Delete,
                ToolCategory::ExternalSend,
                ToolCategory::Payment,
            ]),
        }
    }
}

impl fmt::Display for RiskTier {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.as_str())
    }
}

/// One run's approval policy: its root agent's tier and the categories
/// whose tools need an approval at that tier.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct ApprovalPolicy {
    /// `None`: the command carried no policy (the section was never saved).
    tier: Option<RiskTier>,
    categories: BTreeSet<ToolCategory>,
}

impl ApprovalPolicy {
    /// No policy rule: the command carried no approval policy.
    #[must_use]
    pub fn none() -> Arc<Self> {
        Arc::new(Self::default())
    }

    /// The policy main sent for a run of tier `tier`.
    #[must_use]
    pub fn new(tier: RiskTier, categories: impl IntoIterator<Item = ToolCategory>) -> Self {
        Self {
            tier: Some(tier),
            categories: categories.into_iter().collect(),
        }
    }

    /// The ADR default for `tier`.
    #[must_use]
    pub fn adr_default(tier: RiskTier) -> Self {
        Self::new(tier, tier.default_categories())
    }

    /// The run's tier, or `None` without a policy.
    #[must_use]
    pub const fn tier(&self) -> Option<RiskTier> {
        self.tier
    }

    /// The categories that need an approval.
    #[must_use]
    pub const fn categories(&self) -> &BTreeSet<ToolCategory> {
        &self.categories
    }

    /// The policy rule for a tool of `category`, if the policy names it.
    #[must_use]
    pub fn rule_for(&self, category: ToolCategory) -> Option<ApprovalRule> {
        let tier = self.tier?;
        self.categories
            .contains(&category)
            .then_some(ApprovalRule::Policy { tier, category })
    }
}

/// The rule that made a tool call need an approval. The interrupt payload
/// carries it as `approval_rule` (see [`ApprovalRule::to_json`]), so the
/// decision ledger and the run step record why the run paused.
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub enum ApprovalRule {
    /// The administrator's sensitive action tools named the tool.
    Platform,
    /// The approval policy names the tool's category for the run's tier.
    Policy {
        tier: RiskTier,
        category: ToolCategory,
    },
    /// A rule the runtime holds for one built-in tool (the cloud workspace's
    /// destructive commands, its push and its pull request).
    Builtin,
}

impl ApprovalRule {
    /// The wire name of the rule's source.
    #[must_use]
    pub const fn source(self) -> &'static str {
        match self {
            Self::Platform => "platform",
            Self::Policy { .. } => "policy",
            Self::Builtin => "builtin",
        }
    }

    /// The interrupt payload's `approval_rule`:
    /// `{"source":"policy","risk_tier":"high","category":"delete"}`, or
    /// `{"source":"platform"}` / `{"source":"builtin"}`.
    #[must_use]
    pub fn to_json(self) -> Value {
        match self {
            Self::Policy { tier, category } => json!({
                "source": self.source(),
                "risk_tier": tier.as_str(),
                "category": category.as_str(),
            }),
            Self::Platform | Self::Builtin => json!({"source": self.source()}),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_adr_default_asks_for_delete_send_and_payment_at_high_and_critical() {
        for tier in [RiskTier::High, RiskTier::Critical] {
            let policy = ApprovalPolicy::adr_default(tier);
            for category in [
                ToolCategory::Delete,
                ToolCategory::ExternalSend,
                ToolCategory::Payment,
            ] {
                assert_eq!(
                    policy.rule_for(category),
                    Some(ApprovalRule::Policy { tier, category }),
                    "{tier} {category}"
                );
            }
            for category in [
                ToolCategory::Read,
                ToolCategory::Write,
                ToolCategory::CodeExecution,
            ] {
                assert_eq!(policy.rule_for(category), None, "{tier} {category}");
            }
        }
        for tier in [RiskTier::Low, RiskTier::Medium] {
            let policy = ApprovalPolicy::adr_default(tier);
            assert!(
                ToolCategory::ALL
                    .into_iter()
                    .all(|category| policy.rule_for(category).is_none())
            );
        }
    }

    #[test]
    fn no_policy_names_no_category() {
        let policy = ApprovalPolicy::none();
        assert_eq!(policy.tier(), None);
        assert!(
            ToolCategory::ALL
                .into_iter()
                .all(|category| policy.rule_for(category).is_none())
        );
        // A policy without a tier names nothing even with categories.
        let tierless = ApprovalPolicy {
            tier: None,
            categories: BTreeSet::from([ToolCategory::Delete]),
        };
        assert_eq!(tierless.rule_for(ToolCategory::Delete), None);
    }

    #[test]
    fn the_rule_names_its_source_tier_and_category() {
        let rule = ApprovalRule::Policy {
            tier: RiskTier::Critical,
            category: ToolCategory::Payment,
        };
        assert_eq!(
            rule.to_json(),
            json!({"source": "policy", "risk_tier": "critical", "category": "payment"})
        );
        assert_eq!(
            ApprovalRule::Platform.to_json(),
            json!({"source": "platform"})
        );
        assert_eq!(
            ApprovalRule::Builtin.to_json(),
            json!({"source": "builtin"})
        );
    }

    #[test]
    fn tier_names_round_trip() {
        for tier in RiskTier::ALL {
            assert_eq!(RiskTier::parse(tier.as_str()), Some(tier));
        }
        assert_eq!(RiskTier::parse("High"), None);
    }
}
