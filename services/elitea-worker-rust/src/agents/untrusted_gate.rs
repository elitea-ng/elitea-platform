//! The "read untrusted, then act" confirmation gate (ADR-0043 decision 4,
//! #1500).
//!
//! The approval policy makes a tool in `external_send`, `delete` or
//! `payment` a CONDITIONAL approval candidate when the rule is on for the
//! run's tier and no other rule names the tool
//! ([`crate::toolkits::ToolAdmissionPolicy::untrusted_act_tool_in`]). This
//! gate asks for the approval only when the call comes after the run read
//! untrusted content: a toolkit or MCP tool returned a result earlier in the
//! turn. The pause is a sensitive tool pause whose `approval_rule` is
//! `{"source":"untrusted_read_then_act","category":…}`.
//!
//! The gate composes with another argument-aware gate (the cloud
//! workspace's `run_command`): either one can ask.

use std::collections::BTreeSet;
use std::sync::Arc;

use adk_rust::agent::ToolCallConfirmation;
use async_trait::async_trait;
use elitea_agent_runtime::untrusted::UntrustedReads;

/// Asks before a candidate call after an untrusted read.
pub(crate) struct UntrustedActGate {
    names: BTreeSet<Box<str>>,
    reads: Arc<UntrustedReads>,
    next: Option<Arc<dyn ToolCallConfirmation>>,
}

impl UntrustedActGate {
    /// The gate for `names` (the provider names of the candidate tools),
    /// composed with `next`. Without a candidate it is `next` itself.
    pub(crate) fn compose<I, S>(
        names: I,
        reads: &Arc<UntrustedReads>,
        next: Option<Arc<dyn ToolCallConfirmation>>,
    ) -> Option<Arc<dyn ToolCallConfirmation>>
    where
        I: IntoIterator<Item = S>,
        S: Into<Box<str>>,
    {
        let names = names.into_iter().map(Into::into).collect::<BTreeSet<_>>();
        if names.is_empty() || !reads.act_rule() {
            return next;
        }
        Some(Arc::new(Self {
            names,
            reads: Arc::clone(reads),
            next,
        }))
    }
}

#[async_trait]
impl ToolCallConfirmation for UntrustedActGate {
    async fn requires_confirmation(&self, tool_name: &str, args: &serde_json::Value) -> bool {
        if self.names.contains(tool_name) && self.reads.has_read() {
            return true;
        }
        match &self.next {
            Some(next) => next.requires_confirmation(tool_name, args).await,
            None => false,
        }
    }
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use super::*;

    struct Always;

    #[async_trait]
    impl ToolCallConfirmation for Always {
        async fn requires_confirmation(&self, tool_name: &str, _: &serde_json::Value) -> bool {
            tool_name == "run_command"
        }
    }

    #[tokio::test]
    async fn the_gate_asks_for_candidates_only_after_a_read() {
        let reads = UntrustedReads::new(true);
        let gate = UntrustedActGate::compose(["send_message"], &reads, None).expect("a gate");
        assert!(!gate.requires_confirmation("send_message", &json!({})).await);
        reads.record_read();
        assert!(gate.requires_confirmation("send_message", &json!({})).await);
        assert!(!gate.requires_confirmation("get_issue", &json!({})).await);
    }

    #[tokio::test]
    async fn the_gate_composes_with_the_workspace_gate() {
        let reads = UntrustedReads::new(true);
        let workspace: Arc<dyn ToolCallConfirmation> = Arc::new(Always);
        let gate =
            UntrustedActGate::compose(["delete_file"], &reads, Some(workspace)).expect("a gate");
        assert!(gate.requires_confirmation("run_command", &json!({})).await);
        assert!(!gate.requires_confirmation("delete_file", &json!({})).await);
        reads.record_read();
        assert!(gate.requires_confirmation("delete_file", &json!({})).await);
    }

    #[test]
    fn no_candidate_or_no_rule_is_the_next_gate() {
        let on = UntrustedReads::new(true);
        assert!(UntrustedActGate::compose(Vec::<String>::new(), &on, None).is_none());
        let off = UntrustedReads::off();
        assert!(UntrustedActGate::compose(["send_message"], &off, None).is_none());
        let workspace: Arc<dyn ToolCallConfirmation> = Arc::new(Always);
        let kept = UntrustedActGate::compose(["send_message"], &off, Some(Arc::clone(&workspace)))
            .expect("the workspace gate");
        assert!(Arc::ptr_eq(&kept, &workspace));
    }
}
