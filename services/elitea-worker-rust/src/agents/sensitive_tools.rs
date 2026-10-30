//! Immutable runtime-policy projection into ADK's native tool confirmation.
//!
//! Main/runtime administration owns the toolkit-keyed policy dictionaries.
//! This module joins one immutable policy generation to the already
//! materialized toolsets, producing only the concrete ADK names that require
//! confirmation. Tool groups such as `read` and `write` are never authority.

#![allow(dead_code)] // Production activation waits for the durable resume ledger.

use std::collections::{BTreeMap, BTreeSet};
use std::sync::Arc;
use std::time::Duration;

use adk_rust::tool::SimpleToolContext;
use adk_rust::{ReadonlyContext, Toolset};
use serde_json::{Map, Value};

use super::runtime::{NativeAgentAssemblyError, NativeAgentAssemblyErrorCode};
use crate::toolkits::{
    AdmittedToolSnapshot, FrozenToolKind, SensitiveToolPolicy, ToolAdmissionPolicy, ToolBindingPlan,
};

const TOOL_ENUMERATION_TIMEOUT: Duration = Duration::from_secs(1);
const MAX_CONFIRMED_TOOLS: usize = 1_024;

/// Select one immutable guardrail generation for exactly one admitted run.
///
/// Commands produced before protocol field 38 retain the startup fallback.
/// An explicit object is authoritative even when empty, so a policy change can
/// take effect on the next run without mutating process-global state.
///
/// The generation is bound to the execution's public egress policy (#1490),
/// so every tool it admits and every client built under it uses that policy,
/// and to its content controls (ADR-0043): the disclosure marker, the
/// preamble and the run's untrusted reads.
pub(crate) fn policy_for_guardrails(
    guardrails: Option<&Map<String, Value>>,
    fallback: &Arc<ToolAdmissionPolicy>,
    egress: &Arc<elitea_agent_runtime::egress::RunEgress>,
    approval: &Arc<elitea_agent_runtime::approval_policy::ApprovalPolicy>,
    run_caps: &Arc<elitea_agent_runtime::run_caps::RunCapsMeter>,
    content: &Arc<elitea_agent_runtime::preamble::ContentControls>,
) -> Result<Arc<ToolAdmissionPolicy>, NativeAgentAssemblyError> {
    let Some(guardrails) = guardrails else {
        return Ok(Arc::new(
            fallback
                .as_ref()
                .clone()
                .with_egress(Arc::clone(egress))
                .with_approval_policy(Arc::clone(approval))
                .with_run_caps(Arc::clone(run_caps))
                .with_content_controls(Arc::clone(content)),
        ));
    };
    let runtime = Map::from_iter([(
        "toolkit_security".to_owned(),
        Value::Object(guardrails.clone()),
    )]);
    ToolAdmissionPolicy::from_runtime_config(&runtime)
        .map(|policy| {
            Arc::new(
                policy
                    .with_egress(Arc::clone(egress))
                    .with_approval_policy(Arc::clone(approval))
                    .with_run_caps(Arc::clone(run_caps))
                    .with_content_controls(Arc::clone(content)),
            )
        })
        .map_err(|error| match error.code() {
            crate::toolkits::ToolAdmissionPolicyErrorCode::InvalidConfiguration => {
                invalid_configuration()
            }
            crate::toolkits::ToolAdmissionPolicyErrorCode::ResourceExhausted => {
                resource_exhausted()
            }
        })
}

/// Record the earlier untrusted reads of a continued turn (ADR-0043
/// decision 4): a resumed approval, an answered question or an output
/// continuation carries the turn's earlier tool results in its history, so
/// the "read untrusted, then act" rule treats the turn as having read
/// untrusted content from its first call.
pub(crate) fn seed_continued_turn(
    payload: &elitea_agent_runtime::request::AgentExecutionPayload,
    content: &elitea_agent_runtime::preamble::ContentControls,
) {
    if payload.should_continue
        || payload.hitl_resume
        || payload.hitl_action.is_some()
        || payload.hitl_value.is_some()
        || !payload.hitl_decisions.is_empty()
    {
        content.untrusted().record_read();
    }
}

/// Sensitive concrete tools attached to one direct `LlmAgent` invocation.
///
/// Keys are the exact model-visible ADK function names. Values contain only
/// bounded public policy presentation; raw call arguments and interrupt
/// authority do not exist until the model emits a call.
#[derive(Clone, Default)]
pub(crate) struct SensitiveToolCatalog {
    scoped_entries: BTreeMap<SensitiveToolIdentity, SensitiveToolEntry>,
    provider_entries: BTreeMap<Box<str>, SensitiveToolEntry>,
    /// Every other enumerated tool and whether it is read-only. A dispatched
    /// sibling that is read-only re-runs (no effect to repeat).
    other_tools: BTreeMap<SensitiveToolIdentity, bool>,
    /// Provider names of read-only `other_tools`, each naming exactly one tool.
    read_only_provider_names: BTreeSet<Box<str>>,
    /// Provider names of every `other_tools` entry, read-only or not.
    other_provider_names: BTreeSet<Box<str>>,
    /// The run's untrusted reads (ADR-0043 decision 4), when the catalog
    /// holds "read untrusted, then act" candidates.
    untrusted: Option<Arc<elitea_agent_runtime::untrusted::UntrustedReads>>,
    /// Test-only: how often the unqualified names were rebuilt (budget test).
    #[cfg(test)]
    rebuilds: usize,
}

#[derive(Clone, Eq, Ord, PartialEq, PartialOrd)]
struct SensitiveToolIdentity {
    toolkit_name: Box<str>,
    tool_name: Box<str>,
}

#[derive(Clone)]
struct SensitiveToolEntry {
    policy: SensitiveToolPolicy,
    read_only: bool,
    /// Confirmed only when an argument check asks (the cloud workspace's
    /// `run_command`, ADR-0034): not a name-wide confirmation, and an
    /// approved call replays, because its effect is confined to the
    /// workspace the continuation hydrates from the published commit.
    conditional: bool,
}

impl SensitiveToolEntry {
    /// A "read untrusted, then act" candidate (ADR-0043 decision 4): a
    /// conditional entry whose call has an effect outside the run, so an
    /// approved call that may have started is never run again.
    const fn is_untrusted_act(&self) -> bool {
        matches!(
            self.policy.rule(),
            elitea_agent_runtime::approval_policy::ApprovalRule::UntrustedReadThenAct { .. }
        )
    }
}

impl SensitiveToolCatalog {
    /// The provider names of the "read untrusted, then act" candidates.
    pub(crate) fn untrusted_act_tool_names(&self) -> impl Iterator<Item = &str> {
        self.provider_entries
            .iter()
            .filter(|(_, entry)| entry.is_untrusted_act())
            .map(|(name, _)| name.as_ref())
    }

    /// `next` composed with the "read untrusted, then act" gate of this
    /// catalog's candidates (ADR-0043 decision 4); `next` itself when the
    /// catalog has none.
    pub(crate) fn untrusted_act_gate(
        &self,
        next: Option<Arc<dyn adk_rust::agent::ToolCallConfirmation>>,
    ) -> Option<Arc<dyn adk_rust::agent::ToolCallConfirmation>> {
        match &self.untrusted {
            Some(reads) => super::untrusted_gate::UntrustedActGate::compose(
                self.untrusted_act_tool_names(),
                reads,
                next,
            ),
            None => next,
        }
    }

    #[must_use]
    pub(crate) fn is_empty(&self) -> bool {
        self.scoped_entries.is_empty()
    }

    /// The names confirmed on every call (conditional entries excluded).
    pub(crate) fn tool_names(&self) -> impl Iterator<Item = &str> {
        self.provider_entries
            .iter()
            .filter(|(_, entry)| !entry.conditional)
            .map(|(name, _)| name.as_ref())
    }

    /// Every name the catalog may confirm, conditional entries included: what
    /// a sub-agent, which has no way to ask, is never offered (#1226).
    pub(crate) fn every_tool_name(&self) -> impl Iterator<Item = &str> {
        self.provider_entries.keys().map(AsRef::as_ref)
    }

    /// Whether an approved call of this tool that an earlier continuation
    /// dispatched without a recorded outcome may run again: a read-only tool
    /// has no effect to repeat, and the workspace command's own run ledger
    /// (ADR-0034 decision 9) serves its earlier result instead of re-running.
    /// Every other tool is refused as "may have started".
    #[must_use]
    pub(crate) fn reruns_after_dispatch(&self, tool_name: &str) -> Option<bool> {
        self.provider_entries
            .get(tool_name)
            .map(|entry| entry.read_only || (entry.conditional && !entry.is_untrusted_act()))
            .or_else(|| {
                self.read_only_provider_names
                    .contains(tool_name)
                    .then_some(true)
            })
    }

    /// Add the cloud workspace's conditional `run_command` (ADR-0034).
    ///
    /// # Errors
    ///
    /// The catalog already holds a `local.run_command` entry.
    /// The catalog's provider names are kept as they are: it is added after
    /// `bind_provider_names`, and rebuilding the unqualified names here would
    /// drop every renamed tool's binding. A provider name already taken by
    /// another tool is ambiguous and refused.
    pub(crate) fn with_workspace_command(mut self) -> Result<Self, NativeAgentAssemblyError> {
        const TOOLKIT: &str = "local";
        const TOOL: &str = "run_command";
        let entry = SensitiveToolEntry {
            policy: SensitiveToolPolicy::workspace_command(),
            read_only: false,
            conditional: true,
        };
        let identity = SensitiveToolIdentity {
            toolkit_name: TOOLKIT.into(),
            tool_name: TOOL.into(),
        };
        if self.scoped_entries.contains_key(&identity)
            || self.provider_entries.contains_key(TOOL)
            || self.other_provider_names.contains(TOOL)
        {
            return Err(invalid_configuration());
        }
        self.scoped_entries.insert(identity, entry.clone());
        self.provider_entries.insert(TOOL.into(), entry);
        Ok(self)
    }

    /// Add the cloud executor's `open_pull_request` and `git_push` (ADR-0034
    /// decision 10, amendment 2026-10-26) as name-wide confirmations: every
    /// call is a HITL interrupt, and an approved call that an earlier
    /// continuation dispatched without a recorded outcome is never run again
    /// (a second branch or pull request), like any other writing tool.
    ///
    /// # Errors
    ///
    /// Another tool already has either name (as for the workspace command).
    pub(crate) fn with_workspace_pull_request(mut self) -> Result<Self, NativeAgentAssemblyError> {
        use elitea_agent_runtime::toolkits::workspace_pull_request::{GIT_PUSH, OPEN_PULL_REQUEST};
        const TOOLKIT: &str = "workspace";
        let tools = [
            (
                OPEN_PULL_REQUEST,
                SensitiveToolPolicy::workspace_pull_request(),
            ),
            (GIT_PUSH, SensitiveToolPolicy::workspace_git_push()),
        ];
        for (tool, _) in &tools {
            let identity = SensitiveToolIdentity {
                toolkit_name: TOOLKIT.into(),
                tool_name: (*tool).into(),
            };
            if self.scoped_entries.contains_key(&identity)
                || self.provider_entries.contains_key(*tool)
                || self.other_provider_names.contains(*tool)
            {
                return Err(invalid_configuration());
            }
        }
        for (tool, policy) in tools {
            let entry = SensitiveToolEntry {
                policy,
                read_only: false,
                conditional: false,
            };
            let identity = SensitiveToolIdentity {
                toolkit_name: TOOLKIT.into(),
                tool_name: tool.into(),
            };
            self.scoped_entries.insert(identity, entry.clone());
            self.provider_entries.insert(tool.into(), entry);
        }
        Ok(self)
    }

    #[must_use]
    pub(crate) fn policy_for(&self, tool_name: &str) -> Option<&SensitiveToolPolicy> {
        self.provider_entries
            .get(tool_name)
            .map(|entry| &entry.policy)
    }

    pub(crate) fn policy_for_scoped(
        &self,
        toolkit_name: &str,
        tool_name: &str,
    ) -> Option<&SensitiveToolPolicy> {
        self.scoped_entries
            .get(&SensitiveToolIdentity {
                toolkit_name: toolkit_name.into(),
                tool_name: tool_name.into(),
            })
            .map(|entry| &entry.policy)
    }

    /// Return whether one exact sensitive tool was admitted as read-only.
    ///
    /// This is execution metadata, not authorization.
    #[must_use]
    pub(crate) fn is_read_only(&self, tool_name: &str) -> Option<bool> {
        self.provider_entries
            .get(tool_name)
            .map(|entry| entry.read_only)
    }

    pub(crate) fn merge(&mut self, other: Self) -> Result<(), NativeAgentAssemblyError> {
        if self
            .scoped_entries
            .len()
            .checked_add(other.scoped_entries.len())
            .is_none_or(|count| count > MAX_CONFIRMED_TOOLS)
        {
            return Err(resource_exhausted());
        }
        for (identity, policy) in other.scoped_entries {
            if self.scoped_entries.insert(identity, policy).is_some() {
                return Err(invalid_configuration());
            }
        }
        for (identity, read_only) in other.other_tools {
            self.insert_other_identity(identity, read_only);
        }
        if self.untrusted.is_none() {
            self.untrusted = other.untrusted;
        }
        self.rebuild_unqualified_provider_entries();
        Ok(())
    }

    pub(crate) fn bind_provider_names(
        mut self,
        binding: &ToolBindingPlan,
    ) -> Result<Self, NativeAgentAssemblyError> {
        let mut provider_entries = BTreeMap::new();
        for (identity, entry) in &self.scoped_entries {
            let provider_name = binding
                .provider_name(&identity.toolkit_name, &identity.tool_name)
                .ok_or_else(invalid_configuration)?;
            if provider_entries
                .insert(provider_name.into(), entry.clone())
                .is_some()
            {
                return Err(invalid_configuration());
            }
        }
        self.provider_entries = provider_entries;
        let mut read_only = BTreeMap::<Box<str>, bool>::new();
        for (identity, is_read_only) in &self.other_tools {
            // A tool this binding does not offer the model cannot be a replayed call.
            let Some(provider_name) =
                binding.provider_name(&identity.toolkit_name, &identity.tool_name)
            else {
                continue;
            };
            if let Some(entry) = self.provider_entries.get_mut(provider_name) {
                // A sensitive and a non-sensitive tool behind one identity (two
                // kinds sharing a toolkit name): the name stays confirmed and,
                // being ambiguous, is never re-run after a dispatch.
                entry.read_only = false;
                continue;
            }
            // A name two tools share is ambiguous: never re-run after a dispatch.
            read_only
                .entry(provider_name.into())
                .and_modify(|shared| *shared = false)
                .or_insert(*is_read_only);
        }
        self.other_provider_names = read_only.keys().cloned().collect();
        self.read_only_provider_names = read_only
            .into_iter()
            .filter_map(|(name, is_read_only)| is_read_only.then_some(name))
            .collect();
        Ok(self)
    }

    fn insert_other(
        &mut self,
        toolkit_name: &str,
        tool_name: &str,
        read_only: bool,
    ) -> Result<(), NativeAgentAssemblyError> {
        if !valid_identity(toolkit_name) || !valid_identity(tool_name) {
            return Err(invalid_configuration());
        }
        self.insert_other_identity(
            SensitiveToolIdentity {
                toolkit_name: toolkit_name.into(),
                tool_name: tool_name.into(),
            },
            read_only,
        );
        Ok(())
    }

    /// Record one non-sensitive tool. A second tool with the same identity
    /// (one MCP server listing a name twice, or two kinds sharing a toolkit
    /// name) is not a configuration error: the identity is just ambiguous, so
    /// it is never treated as read-only.
    fn insert_other_identity(&mut self, identity: SensitiveToolIdentity, read_only: bool) {
        self.other_tools
            .entry(identity)
            .and_modify(|shared| *shared = false)
            .or_insert(read_only);
    }

    fn insert_scoped(
        &mut self,
        toolkit_name: &str,
        tool_name: &str,
        entry: SensitiveToolEntry,
    ) -> Result<(), NativeAgentAssemblyError> {
        if !valid_identity(toolkit_name) || !valid_identity(tool_name) {
            return Err(invalid_configuration());
        }
        let identity = SensitiveToolIdentity {
            toolkit_name: toolkit_name.into(),
            tool_name: tool_name.into(),
        };
        if self.scoped_entries.insert(identity, entry).is_some() {
            return Err(invalid_configuration());
        }
        Ok(())
    }

    /// Derive the unqualified provider names from every scoped and other tool.
    /// Callers insert first and rebuild once (O(N) per catalog, not per tool).
    fn rebuild_unqualified_provider_entries(&mut self) {
        #[cfg(test)]
        {
            self.rebuilds += 1;
        }
        let mut counts = BTreeMap::<&str, usize>::new();
        for identity in self.scoped_entries.keys() {
            *counts.entry(&identity.tool_name).or_default() += 1;
        }
        self.provider_entries = self
            .scoped_entries
            .iter()
            .filter(|(identity, _)| counts.get(identity.tool_name.as_ref()) == Some(&1))
            .map(|(identity, entry)| (identity.tool_name.clone(), entry.clone()))
            .collect();
        // A name shared with any other tool, sensitive or not, is ambiguous.
        for identity in self.other_tools.keys() {
            *counts.entry(&identity.tool_name).or_default() += 1;
        }
        self.other_provider_names = self
            .other_tools
            .keys()
            .map(|identity| identity.tool_name.clone())
            .collect();
        self.read_only_provider_names = self
            .other_tools
            .iter()
            .filter(|(identity, read_only)| {
                **read_only && counts.get(identity.tool_name.as_ref()) == Some(&1)
            })
            .map(|(identity, _)| identity.tool_name.clone())
            .collect();
    }

    /// Test-only: record one non-sensitive enumerated tool.
    #[cfg(test)]
    pub(crate) fn with_other_tool(
        mut self,
        toolkit_name: &str,
        tool_name: &str,
        read_only: bool,
    ) -> Self {
        self.insert_other(toolkit_name, tool_name, read_only)
            .expect("fixture tool identity");
        self.rebuild_unqualified_provider_entries();
        self
    }

    #[cfg(test)]
    pub(crate) fn fixture(
        tool_name: &str,
        policy: SensitiveToolPolicy,
        read_only: bool,
    ) -> Result<Self, NativeAgentAssemblyError> {
        if tool_name.is_empty() || tool_name.len() > 512 || tool_name.chars().any(char::is_control)
        {
            return Err(invalid_configuration());
        }
        let mut catalog = Self::default();
        catalog.insert_scoped(
            "fixture",
            tool_name,
            SensitiveToolEntry {
                policy,
                read_only,
                conditional: false,
            },
        )?;
        catalog.rebuild_unqualified_provider_entries();
        Ok(catalog)
    }
}

/// Enumerate static, already-materialized toolsets and bind runtime sensitivity.
///
/// The configured and MCP materializers both return one bounded `BasicToolset`
/// per admitted frozen reference in source order. This helper verifies that
/// invariant instead of guessing an identity from a model-visible name.
pub(crate) async fn sensitive_tools_for_kind(
    snapshot: &AdmittedToolSnapshot<'_>,
    kind: FrozenToolKind,
    toolsets: &[Arc<dyn Toolset>],
    policy: &ToolAdmissionPolicy,
) -> Result<SensitiveToolCatalog, NativeAgentAssemblyError> {
    let mut toolsets_by_name = BTreeMap::new();
    for toolset in toolsets {
        if toolsets_by_name.insert(toolset.name(), toolset).is_some() {
            return Err(invalid_configuration());
        }
    }
    let mut reference_names = BTreeSet::new();
    let context: Arc<dyn ReadonlyContext> =
        Arc::new(SimpleToolContext::new("elitea_sensitive_policy"));
    let mut catalog = SensitiveToolCatalog::default();
    for reference in snapshot.iter().filter(|reference| reference.kind() == kind) {
        if !reference_names.insert(reference.toolkit_name()) {
            return Err(invalid_configuration());
        }
        let Some(toolset) = toolsets_by_name.remove(reference.toolkit_name()) else {
            // Configured toolkit families can be intentionally unavailable in this
            // runtime. Their materializer omits them before this catalogue is built.
            // Do not shift the next materialized toolset onto the omitted identity.
            continue;
        };
        let tools = tokio::time::timeout(
            TOOL_ENUMERATION_TIMEOUT,
            toolset.tools(Arc::clone(&context)),
        )
        .await
        .map_err(|_| dependency_unavailable())?
        .map_err(|_| dependency_unavailable())?;
        if tools.len() > MAX_CONFIRMED_TOOLS {
            return Err(resource_exhausted());
        }
        for tool in tools {
            let (sensitive, conditional) = match policy.sensitive_tool_in(
                reference.tool_type(),
                reference.toolkit_name(),
                reference.settings(),
                tool.name(),
            ) {
                Some(sensitive) => (sensitive, false),
                // ADR-0043 decision 4: asks only after an untrusted read.
                None => match policy.untrusted_act_tool_in(
                    reference.tool_type(),
                    reference.toolkit_name(),
                    reference.settings(),
                    tool.name(),
                ) {
                    Some(candidate) => {
                        catalog.untrusted = Some(Arc::clone(policy.content_controls().untrusted()));
                        (candidate, true)
                    }
                    None => {
                        catalog.insert_other(
                            reference.toolkit_name(),
                            tool.name(),
                            tool.is_read_only(),
                        )?;
                        continue;
                    }
                },
            };
            catalog.insert_scoped(
                reference.toolkit_name(),
                tool.name(),
                SensitiveToolEntry {
                    policy: sensitive,
                    read_only: tool.is_read_only(),
                    conditional,
                },
            )?;
        }
    }
    if !toolsets_by_name.is_empty() {
        return Err(invalid_configuration());
    }
    catalog.rebuild_unqualified_provider_entries();
    Ok(catalog)
}

fn valid_identity(value: &str) -> bool {
    !value.is_empty() && value.len() <= 1_024 && !value.chars().any(char::is_control)
}

const fn invalid_configuration() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::InvalidConfiguration,
        "the sensitive-tool runtime policy does not match the materialized toolsets",
    )
}

const fn resource_exhausted() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::ResourceExhausted,
        "the sensitive-tool runtime policy exceeds its approved limit",
    )
}

const fn dependency_unavailable() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::DependencyUnavailable,
        "the sensitive-tool catalog could not be enumerated",
    )
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeSet;
    use std::sync::Arc;

    use adk_rust::tool::{BasicToolset, FunctionTool};
    use adk_rust::{Tool, Toolset};
    use elitea_agent_runtime::approval_policy::ApprovalPolicy;
    use elitea_agent_runtime::run_caps::{RunCapLimits, RunCapsMeter};
    use serde_json::{Map, Value, json};

    use super::{
        SensitiveToolCatalog, SensitiveToolEntry, policy_for_guardrails, sensitive_tools_for_kind,
    };
    use crate::toolkits::{
        FrozenToolKind, FrozenToolSnapshot, SensitiveToolPolicy, ToolAdmissionPolicy, bind_toolsets,
    };

    fn policy(security: Value) -> Arc<ToolAdmissionPolicy> {
        let runtime = Map::from_iter([("toolkit_security".to_owned(), security)]);
        Arc::new(ToolAdmissionPolicy::from_runtime_config(&runtime).expect("valid toolkit policy"))
    }

    /// The shipped default (`deploy/runtime/worker-toolkit-security.json`,
    /// the chart's `worker.runtime.sensitiveTools`): `delete_file` AND the
    /// indexing families' `remove_index` need a human's approval, under the
    /// wildcard toolkit so no family's index tools escape it (ADR-0030 I1,
    /// review round 1).
    #[test]
    fn the_shipped_default_marks_remove_index_sensitive_for_every_index_family() {
        let shipped: Value = serde_json::from_str(include_str!(
            "../../../../deploy/runtime/worker-toolkit-security.json"
        ))
        .expect("the shipped toolkit-security document is JSON");
        let policy = Arc::new(
            ToolAdmissionPolicy::from_runtime_config(
                shipped.as_object().expect("a document object"),
            )
            .expect("the shipped document is a valid policy"),
        );
        for family in ["artifact", "github", "gitlab", "bitbucket", "ado_repos"] {
            assert!(
                policy
                    .sensitive_tool(family, "kit", "remove_index")
                    .is_some(),
                "{family}/remove_index must need approval by default"
            );
            assert!(
                policy
                    .sensitive_tool(family, "kit", "search_index")
                    .is_none(),
                "{family}/search_index is a read and needs none"
            );
        }
        assert!(
            policy
                .sensitive_tool("artifact", "files", "delete_file")
                .is_some()
        );
    }

    #[test]
    fn absent_guardrails_keep_the_fallback_rules_but_explicit_empty_clears_them() {
        let fallback = policy(json!({"sensitive_tools": {"*": ["delete_file"]}}));

        let open = elitea_agent_runtime::egress::RunEgress::open();
        let absent = policy_for_guardrails(
            None,
            &fallback,
            &open,
            &ApprovalPolicy::none(),
            &RunCapsMeter::unlimited(),
            &elitea_agent_runtime::preamble::ContentControls::worker_default(),
        )
        .expect("absent policy");
        assert!(Arc::ptr_eq(absent.egress(), &open), "bound to the run");
        assert!(
            absent
                .sensitive_tool("artifact", "files", "delete_file")
                .is_some()
        );

        let empty = Map::new();
        let resolved = policy_for_guardrails(
            Some(&empty),
            &fallback,
            &open,
            &ApprovalPolicy::none(),
            &RunCapsMeter::unlimited(),
            &elitea_agent_runtime::preamble::ContentControls::worker_default(),
        )
        .expect("empty policy");
        assert!(!Arc::ptr_eq(&resolved, &fallback));
        assert!(
            resolved
                .sensitive_tool("artifact", "files", "delete_file")
                .is_none()
        );
    }

    #[test]
    fn command_guardrails_override_fallback_with_one_immutable_generation() {
        let fallback = policy(json!({}));
        let guardrails = json!({
            "blocked_toolkits": ["sharepoint"],
            "sensitive_tools": {"openapi": ["create_item"]}
        })
        .as_object()
        .expect("guardrails object")
        .clone();

        let egress = elitea_agent_runtime::egress::RunEgress::new(
            elitea_agent_runtime::egress::PublicPolicy::allowlist(["api.example.com"])
                .expect("policy"),
        );
        let meter = RunCapsMeter::new(RunCapLimits::worker_default());
        let resolved = policy_for_guardrails(
            Some(&guardrails),
            &fallback,
            &egress,
            &ApprovalPolicy::none(),
            &meter,
            &elitea_agent_runtime::preamble::ContentControls::worker_default(),
        )
        .expect("command policy");
        assert!(Arc::ptr_eq(resolved.run_caps(), &meter), "bound to the run");
        assert!(Arc::ptr_eq(resolved.egress(), &egress), "bound to the run");
        assert_ne!(
            resolved.toolkit_decision("sharepoint"),
            crate::toolkits::ToolAdmissionDecision::Allowed
        );
        assert!(
            resolved
                .sensitive_tool("openapi", "customer_api", "create_item")
                .is_some()
        );
    }

    #[tokio::test]
    async fn provider_aliases_retain_exact_sensitive_toolkit_identity() {
        let policy = policy(json!({
            "sensitive_tools": {"mcp": ["lookup"]},
            "sensitive_action_company_name": "Example Org"
        }));
        let mut catalog = SensitiveToolCatalog::default();
        let mut toolsets = Vec::new();
        for toolkit_name in ["release intelligence", "audit intelligence"] {
            let sensitive = policy
                .sensitive_tool("mcp", toolkit_name, "lookup")
                .expect("sensitive fixture policy");
            catalog
                .insert_scoped(
                    toolkit_name,
                    "lookup",
                    SensitiveToolEntry {
                        policy: sensitive,
                        read_only: true,
                        conditional: false,
                    },
                )
                .expect("scoped sensitive tool");
            let tool: Arc<dyn Tool> = Arc::new(FunctionTool::new(
                "lookup",
                "Read one exact source",
                |_context, _arguments| async { Ok(json!({})) },
            ));
            toolsets
                .push(Arc::new(BasicToolset::new(toolkit_name, vec![tool])) as Arc<dyn Toolset>);
        }
        let binding = bind_toolsets(toolsets, &BTreeSet::new(), "sensitive_alias_test")
            .await
            .expect("provider binding");
        let catalog = catalog
            .bind_provider_names(&binding)
            .expect("bound sensitive catalog");
        assert_eq!(
            catalog
                .policy_for("release_intelligence__lookup")
                .map(SensitiveToolPolicy::toolkit_name),
            Some("release intelligence")
        );
        assert_eq!(
            catalog
                .policy_for("audit_intelligence__lookup")
                .map(SensitiveToolPolicy::toolkit_name),
            Some("audit intelligence")
        );
        assert!(catalog.policy_for("lookup").is_none());
    }

    /// #1321 review round 2 item 4: the workspace command joins a catalog that
    /// is already bound, and must not reset the bound (renamed) provider names.
    #[tokio::test]
    async fn the_workspace_command_keeps_bound_aliased_names() {
        let policy = policy(json!({"sensitive_tools": {"mcp": ["lookup"]}}));
        let mut catalog = SensitiveToolCatalog::default();
        let mut toolsets = Vec::new();
        for toolkit_name in ["release intelligence", "audit intelligence"] {
            catalog
                .insert_scoped(
                    toolkit_name,
                    "lookup",
                    SensitiveToolEntry {
                        policy: policy
                            .sensitive_tool("mcp", toolkit_name, "lookup")
                            .expect("sensitive fixture policy"),
                        read_only: false,
                        conditional: false,
                    },
                )
                .expect("scoped sensitive tool");
            catalog
                .insert_other(toolkit_name, "search", true)
                .expect("read-only sibling");
            let tools: Vec<Arc<dyn Tool>> = ["lookup", "search"]
                .into_iter()
                .map(|name| {
                    Arc::new(FunctionTool::new(
                        name,
                        "fixture",
                        |_context, _arguments| async { Ok(json!({})) },
                    )) as Arc<dyn Tool>
                })
                .collect();
            toolsets.push(Arc::new(BasicToolset::new(toolkit_name, tools)) as Arc<dyn Toolset>);
        }
        catalog.rebuild_unqualified_provider_entries();
        let binding = bind_toolsets(toolsets, &BTreeSet::new(), "sensitive_alias_test")
            .await
            .expect("provider binding");
        let catalog = catalog
            .bind_provider_names(&binding)
            .expect("bound sensitive catalog")
            .with_workspace_command()
            .expect("workspace command");
        for aliased in ["release_intelligence__lookup", "audit_intelligence__lookup"] {
            assert!(
                catalog.policy_for(aliased).is_some(),
                "{aliased} stays sensitive"
            );
            assert_eq!(catalog.reruns_after_dispatch(aliased), Some(false));
        }
        for aliased in ["release_intelligence__search", "audit_intelligence__search"] {
            assert_eq!(
                catalog.reruns_after_dispatch(aliased),
                Some(true),
                "the aliased read-only sibling {aliased} re-runs"
            );
        }
        assert_eq!(catalog.reruns_after_dispatch("run_command"), Some(true));
        assert!(catalog.policy_for("run_command").is_some());
        assert!(
            catalog.with_workspace_command().is_err(),
            "a second workspace command is refused"
        );
        // A data-changing, non-sensitive tool already named `run_command` is not
        // taken over by the workspace's conditional entry.
        assert!(
            SensitiveToolCatalog::default()
                .with_other_tool("kit", "run_command", false)
                .with_workspace_command()
                .is_err()
        );
    }

    /// ADR-0034 decision 10 (amended 2026-10-26): `open_pull_request` is
    /// confirmed on every call, never re-run after a dispatch, and never
    /// takes over another tool's name.
    #[test]
    fn the_cloud_pull_request_is_confirmed_on_every_call() {
        let catalog = SensitiveToolCatalog::default()
            .with_workspace_command()
            .expect("workspace command")
            .with_workspace_pull_request()
            .expect("pull request");
        assert!(catalog.tool_names().any(|name| name == "open_pull_request"));
        assert!(
            !catalog.tool_names().any(|name| name == "run_command"),
            "the command stays conditional"
        );
        assert_eq!(
            catalog.reruns_after_dispatch("open_pull_request"),
            Some(false)
        );
        let policy = catalog.policy_for("open_pull_request").expect("policy");
        assert_eq!(policy.action_name(), "open_pull_request");
        assert_eq!(catalog.reruns_after_dispatch("git_push"), Some(false));
        assert_eq!(
            catalog
                .policy_for("git_push")
                .expect("policy")
                .action_name(),
            "git_push"
        );
        assert!(catalog.with_workspace_pull_request().is_err());
        assert!(
            SensitiveToolCatalog::default()
                .with_other_tool("kit", "git_push", false)
                .with_workspace_pull_request()
                .is_err()
        );
        assert!(
            SensitiveToolCatalog::default()
                .with_other_tool("kit", "open_pull_request", false)
                .with_workspace_pull_request()
                .is_err()
        );
    }

    /// #1321 code review round 3: a sensitive and a non-sensitive tool behind one
    /// identity (two kinds sharing a toolkit name) bind; the name stays confirmed
    /// and is never re-run after a dispatch.
    #[tokio::test]
    async fn a_sensitive_and_a_plain_tool_sharing_an_identity_bind_conservatively() {
        let policy = policy(json!({"sensitive_tools": {"mcp": ["lookup"]}}));
        let mut catalog = SensitiveToolCatalog::default();
        catalog
            .insert_scoped(
                "kit",
                "lookup",
                SensitiveToolEntry {
                    policy: policy
                        .sensitive_tool("mcp", "kit", "lookup")
                        .expect("sensitive fixture policy"),
                    read_only: true,
                    conditional: false,
                },
            )
            .expect("sensitive tool");
        catalog
            .insert_other("kit", "lookup", true)
            .expect("plain tool");
        catalog.rebuild_unqualified_provider_entries();
        let tool: Arc<dyn Tool> = Arc::new(FunctionTool::new(
            "lookup",
            "fixture",
            |_context, _arguments| async { Ok(json!({})) },
        ));
        let binding = bind_toolsets(
            vec![Arc::new(BasicToolset::new("kit", vec![tool])) as Arc<dyn Toolset>],
            &BTreeSet::new(),
            "shared_identity_test",
        )
        .await
        .expect("provider binding");
        let catalog = catalog
            .bind_provider_names(&binding)
            .expect("a shared identity binds");
        assert!(catalog.policy_for("lookup").is_some(), "still confirmed");
        assert_eq!(catalog.reruns_after_dispatch("lookup"), Some(false));
    }

    /// #1321 code review round 3: a non-sensitive tool listed twice (or two kinds
    /// sharing a toolkit name) is ambiguous, not a configuration error.
    #[test]
    fn a_duplicate_non_sensitive_tool_is_ambiguous_not_fatal() {
        let mut catalog = SensitiveToolCatalog::default()
            .with_other_tool("kit", "search", true)
            .with_other_tool("kit", "search", true);
        assert_eq!(catalog.reruns_after_dispatch("search"), None);
        let other = SensitiveToolCatalog::default().with_other_tool("kit", "lookup", true);
        catalog
            .merge(SensitiveToolCatalog::default().with_other_tool("kit", "lookup", true))
            .expect("first kind");
        catalog
            .merge(other)
            .expect("a second kind with the same toolkit name");
        assert_eq!(catalog.reruns_after_dispatch("lookup"), None);
    }

    /// #1321 review round 2 item 6 (budget): enumerating N tools derives the
    /// unqualified names once, not once per tool.
    #[tokio::test]
    async fn enumeration_rebuilds_the_unqualified_names_once() {
        const TOOLS: usize = 256;
        let policy = policy(json!({"sensitive_tools": {"mcp": ["tool_0"]}}));
        let version = json!({
            "tools": [{"id": 1, "type": "mcp", "toolkit_name": "kit", "settings": {}}]
        });
        let snapshot =
            FrozenToolSnapshot::from_version_details(version.as_object().expect("version object"))
                .expect("frozen tools")
                .apply_policy(policy.as_ref());
        let tools = (0..TOOLS)
            .map(|index| {
                Arc::new(FunctionTool::new(
                    format!("tool_{index}"),
                    "fixture",
                    |_context, _arguments| async { Ok(json!({})) },
                )) as Arc<dyn Tool>
            })
            .collect();
        let toolsets = vec![Arc::new(BasicToolset::new("kit", tools)) as Arc<dyn Toolset>];
        let catalog =
            sensitive_tools_for_kind(&snapshot, FrozenToolKind::Mcp, &toolsets, policy.as_ref())
                .await
                .expect("catalog");
        assert_eq!(catalog.rebuilds, 1);
        assert!(catalog.policy_for("tool_0").is_some());
        assert_eq!(catalog.other_tools.len(), TOOLS - 1);
    }

    #[tokio::test]
    async fn omitted_unsupported_toolkit_does_not_shift_sensitive_identity() {
        let policy = policy(json!({
            "sensitive_tools": {
                "artifact": ["lookup"],
                "github": ["lookup"]
            }
        }));
        let version = json!({
            "tools": [
                {"id": 1, "type": "artifact", "toolkit_name": "attachments", "settings": {}},
                {"id": 2, "type": "github", "toolkit_name": "repository", "settings": {}}
            ]
        });
        let snapshot =
            FrozenToolSnapshot::from_version_details(version.as_object().expect("version object"))
                .expect("frozen tools")
                .apply_policy(policy.as_ref());
        let tool: Arc<dyn Tool> = Arc::new(FunctionTool::new(
            "lookup",
            "Read one exact source",
            |_context, _arguments| async { Ok(json!({})) },
        ));
        let toolsets =
            vec![Arc::new(BasicToolset::new("repository", vec![tool])) as Arc<dyn Toolset>];

        let catalog = sensitive_tools_for_kind(
            &snapshot,
            FrozenToolKind::Configured,
            &toolsets,
            policy.as_ref(),
        )
        .await
        .expect("omitted unsupported toolkit");

        assert!(catalog.policy_for_scoped("attachments", "lookup").is_none());
        assert!(catalog.policy_for_scoped("repository", "lookup").is_some());
    }
}
