//! The unattended grant of a run (ADR-0045 decisions 1, 2 and 4).
//!
//! Main freezes the grant into the run's input
//! (`AgentExecutionInputV1.unattended_grant`, field 68) only when the person
//! who starts the run gives one. Main admitted every part of it before the
//! run started. The worker uses two parts of it:
//!
//! * `sensitive_tools`: at a sensitive pause for one of these
//!   `(toolkit_name, tool_name)` pairs the worker approves the call itself.
//!   It does not end the run. It emits one `agent_unattended_approval` node
//!   event, and Main writes the approval ledger row from it. Every other
//!   pause stays as it is.
//! * `continue_step_limit`: at the step limit the run ends with
//!   `STEP_LIMIT_REACHED` and the text so far, not with an error. Main then
//!   starts the next link of the chain.
//!
//! The grant never lifts a deny, a guardrail, a run cap or a budget. An
//! absent grant changes nothing. The legacy boolean
//! `auto_approve_sensitive_actions` stays refused.

use std::collections::BTreeSet;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};

use serde_json::{Value, json};

use crate::protocol::{
    ProtocolError as AgentProtocolError,
    elitea::runtime::v1::{AgentExecutionInputV1, UnattendedGrantV1},
};

/// The node event type of one in-line grant approval.
pub(crate) const UNATTENDED_APPROVAL_EVENT: &str = "agent_unattended_approval";
/// The `response_metadata` member that holds the approval.
pub(crate) const UNATTENDED_APPROVAL_METADATA_KEY: &str = "unattended_approval_v1";
/// The `full_message` metadata member of a run that ended at its step limit.
pub(crate) const STEP_LIMIT_REACHED_METADATA_KEY: &str = "step_limit_reached";

/// The most sensitive tools one grant can name.
pub(crate) const MAX_GRANTED_TOOLS: usize = 64;
/// The longest toolkit or tool name a grant can hold.
const MAX_GRANTED_NAME_BYTES: usize = 512;

/// One admitted unattended grant.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct UnattendedGrant {
    grant_id: Arc<str>,
    /// The person who gave the grant. Main checks it; the worker keeps it
    /// only as part of the admitted value.
    owner_user_id: i64,
    sensitive_tools: BTreeSet<(Box<str>, Box<str>)>,
    continue_step_limit: bool,
}

impl UnattendedGrant {
    /// The grant's id: a lowercase canonical UUID.
    #[must_use]
    pub(crate) fn grant_id(&self) -> &Arc<str> {
        &self.grant_id
    }

    /// Whether the grant names this exact `(toolkit_name, tool_name)` pair.
    #[must_use]
    pub(crate) fn names_tool(&self, toolkit_name: &str, tool_name: &str) -> bool {
        self.sensitive_tools
            .contains(&(Box::from(toolkit_name), Box::from(tool_name)))
    }

    /// Whether the grant names any sensitive tool.
    #[must_use]
    pub(crate) fn has_sensitive_tools(&self) -> bool {
        !self.sensitive_tools.is_empty()
    }

    /// Whether the run ends with `STEP_LIMIT_REACHED` at its step limit.
    #[must_use]
    pub(crate) const fn continue_step_limit(&self) -> bool {
        self.continue_step_limit
    }

    /// A grant for tests.
    #[cfg(test)]
    pub(crate) fn fixture(tools: &[(&str, &str)], continue_step_limit: bool) -> Self {
        Self {
            grant_id: Arc::from("0b9e3f4c-1d2a-4b5c-8d6e-7f8091a2b3c4"),
            owner_user_id: 42,
            sensitive_tools: tools
                .iter()
                .map(|(toolkit, tool)| (Box::from(*toolkit), Box::from(*tool)))
                .collect(),
            continue_step_limit,
        }
    }
}

/// Decode and check the input's grant. An absent grant answers `None`.
///
/// # Errors
///
/// Returns [`AgentProtocolError::InvalidInput`] for a malformed grant: an id
/// that is not a lowercase canonical UUID, an owner that is not positive,
/// more than [`MAX_GRANTED_TOOLS`] tools, an empty, long or control-character
/// name, or a pair named twice.
pub(crate) fn grant_from_input(
    message: &AgentExecutionInputV1,
) -> Result<Option<UnattendedGrant>, AgentProtocolError> {
    message
        .unattended_grant
        .as_ref()
        .map(grant_from)
        .transpose()
}

fn grant_from(grant: &UnattendedGrantV1) -> Result<UnattendedGrant, AgentProtocolError> {
    let malformed = || AgentProtocolError::InvalidInput("the unattended grant is malformed");
    if !canonical_uuid(&grant.grant_id)
        || grant.owner_user_id <= 0
        || grant.sensitive_tools.len() > MAX_GRANTED_TOOLS
        || (!grant.chain_root_execution_id.is_empty()
            && !valid_name(&grant.chain_root_execution_id))
    {
        return Err(malformed());
    }
    let mut sensitive_tools = BTreeSet::new();
    for tool in &grant.sensitive_tools {
        if !valid_name(&tool.toolkit_name) || !valid_name(&tool.tool_name) {
            return Err(malformed());
        }
        if !sensitive_tools.insert((
            Box::from(tool.toolkit_name.as_str()),
            Box::from(tool.tool_name.as_str()),
        )) {
            return Err(malformed());
        }
    }
    Ok(UnattendedGrant {
        grant_id: Arc::from(grant.grant_id.as_str()),
        owner_user_id: grant.owner_user_id,
        sensitive_tools,
        continue_step_limit: grant.continue_step_limit,
    })
}

fn valid_name(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= MAX_GRANTED_NAME_BYTES
        && !value.chars().any(char::is_control)
}

/// A lowercase canonical UUID: 8-4-4-4-12 lowercase hex digits.
fn canonical_uuid(value: &str) -> bool {
    value.len() == 36
        && value.bytes().enumerate().all(|(index, byte)| match index {
            8 | 13 | 18 | 23 => byte == b'-',
            _ => byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte),
        })
}

/// One in-line approval that a grant gave: what the approval event names.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct GrantedApproval {
    grant_id: Arc<str>,
    /// The granted pair as the grant names it, not the model-visible name.
    toolkit_name: Box<str>,
    tool_name: Box<str>,
}

impl GrantedApproval {
    #[must_use]
    pub(crate) fn new(grant_id: Arc<str>, toolkit_name: &str, tool_name: &str) -> Self {
        Self {
            grant_id,
            toolkit_name: toolkit_name.into(),
            tool_name: tool_name.into(),
        }
    }

    /// The `response_metadata` of the approval event. `interrupt_id` is the
    /// id that the pause would have used. `argument_digest` is the
    /// `sha256:<hex>` digest of the canonical JSON arguments before masking.
    #[must_use]
    pub(crate) fn event_metadata(
        &self,
        interrupt_id: &str,
        toolkit_type: &str,
        argument_digest: &str,
    ) -> Value {
        json!({
            UNATTENDED_APPROVAL_METADATA_KEY: {
                "grant_id": self.grant_id.as_ref(),
                "interrupt_id": interrupt_id,
                "toolkit_name": self.toolkit_name.as_ref(),
                "toolkit_type": toolkit_type,
                "tool_name": self.tool_name.as_ref(),
                "argument_digest": argument_digest,
            }
        })
    }
}

/// The stop of a run at its step limit, shared by the lifecycle (which sees
/// the step-limit end) and the completion selector (which then completes the
/// run with its text so far).
#[derive(Debug, Default)]
pub(crate) struct StepLimitStop {
    reached: AtomicBool,
}

impl StepLimitStop {
    /// Record that the run ended at its step limit.
    pub(crate) fn mark(&self) {
        self.reached.store(true, Ordering::Release);
    }

    /// Whether the run ended at its step limit.
    #[must_use]
    pub(crate) fn reached(&self) -> bool {
        self.reached.load(Ordering::Acquire)
    }
}

#[cfg(test)]
mod tests {
    use super::{MAX_GRANTED_TOOLS, grant_from_input};
    use crate::protocol::elitea::runtime::v1::{
        AgentExecutionInputV1, UnattendedGrantToolV1, UnattendedGrantV1,
    };

    fn tool(toolkit: &str, name: &str) -> UnattendedGrantToolV1 {
        UnattendedGrantToolV1 {
            toolkit_name: toolkit.to_owned(),
            tool_name: name.to_owned(),
        }
    }

    fn input(grant: UnattendedGrantV1) -> AgentExecutionInputV1 {
        AgentExecutionInputV1 {
            unattended_grant: Some(grant),
            ..AgentExecutionInputV1::default()
        }
    }

    fn valid() -> UnattendedGrantV1 {
        UnattendedGrantV1 {
            grant_id: "0b9e3f4c-1d2a-4b5c-8d6e-7f8091a2b3c4".to_owned(),
            owner_user_id: 7,
            sensitive_tools: vec![tool("files", "delete_file")],
            continue_step_limit: true,
            ..UnattendedGrantV1::default()
        }
    }

    #[test]
    fn absent_grant_is_none() {
        assert_eq!(
            grant_from_input(&AgentExecutionInputV1::default()).expect("absent"),
            None
        );
    }

    #[test]
    fn valid_grant_decodes() {
        let grant = grant_from_input(&input(valid()))
            .expect("valid")
            .expect("present");
        assert_eq!(grant.owner_user_id, 7);
        assert!(grant.names_tool("files", "delete_file"));
        assert!(!grant.names_tool("files", "read_file"));
        assert!(!grant.names_tool("other", "delete_file"));
        assert!(grant.continue_step_limit());
    }

    #[test]
    fn malformed_grants_are_refused() {
        type Change = Box<dyn Fn(&mut UnattendedGrantV1)>;
        let cases: Vec<Change> = vec![
            Box::new(|grant| grant.grant_id = "0B9E3F4C-1D2A-4B5C-8D6E-7F8091A2B3C4".to_owned()),
            Box::new(|grant| grant.grant_id = "not-a-uuid".to_owned()),
            Box::new(|grant| grant.owner_user_id = 0),
            Box::new(|grant| grant.sensitive_tools.push(tool("files", "delete_file"))),
            Box::new(|grant| grant.sensitive_tools.push(tool("", "x"))),
            Box::new(|grant| grant.sensitive_tools.push(tool("kit", "a\nb"))),
            Box::new(|grant| grant.sensitive_tools.push(tool("kit", &"x".repeat(513)))),
            Box::new(|grant| {
                grant.sensitive_tools = (0..=MAX_GRANTED_TOOLS)
                    .map(|index| tool("kit", &format!("tool_{index}")))
                    .collect();
            }),
        ];
        for (index, change) in cases.iter().enumerate() {
            let mut grant = valid();
            change(&mut grant);
            assert!(
                grant_from_input(&input(grant)).is_err(),
                "case {index} must be refused"
            );
        }
    }
}
