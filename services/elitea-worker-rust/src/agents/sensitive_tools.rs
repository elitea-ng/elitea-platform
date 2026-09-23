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
pub(crate) fn policy_for_guardrails(
    guardrails: Option<&Map<String, Value>>,
    fallback: &Arc<ToolAdmissionPolicy>,
) -> Result<Arc<ToolAdmissionPolicy>, NativeAgentAssemblyError> {
    let Some(guardrails) = guardrails else {
        return Ok(Arc::clone(fallback));
    };
    let runtime = Map::from_iter([(
        "toolkit_security".to_owned(),
        Value::Object(guardrails.clone()),
    )]);
    ToolAdmissionPolicy::from_runtime_config(&runtime)
        .map(Arc::new)
        .map_err(|error| match error.code() {
            crate::toolkits::ToolAdmissionPolicyErrorCode::InvalidConfiguration => {
                invalid_configuration()
            }
            crate::toolkits::ToolAdmissionPolicyErrorCode::ResourceExhausted => {
                resource_exhausted()
            }
        })
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
}

impl SensitiveToolCatalog {
    #[must_use]
    pub(crate) fn is_empty(&self) -> bool {
        self.scoped_entries.is_empty()
    }

    pub(crate) fn tool_names(&self) -> impl Iterator<Item = &str> {
        self.provider_entries.keys().map(AsRef::as_ref)
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
    /// This is execution metadata, not authorization. It only permits the
    /// capability-disabled direct-HITL replay seam to exclude effects while
    /// the durable effect owner is still absent.
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
        Ok(self)
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
        self.rebuild_unqualified_provider_entries();
        Ok(())
    }

    fn rebuild_unqualified_provider_entries(&mut self) {
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
            SensitiveToolEntry { policy, read_only },
        )?;
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
            let Some(sensitive) =
                policy.sensitive_tool(reference.tool_type(), reference.toolkit_name(), tool.name())
            else {
                continue;
            };
            catalog.insert_scoped(
                reference.toolkit_name(),
                tool.name(),
                SensitiveToolEntry {
                    policy: sensitive,
                    read_only: tool.is_read_only(),
                },
            )?;
        }
    }
    if !toolsets_by_name.is_empty() {
        return Err(invalid_configuration());
    }
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

    #[test]
    fn absent_guardrails_reuse_fallback_but_explicit_empty_clears_it() {
        let fallback = policy(json!({"sensitive_tools": {"*": ["delete_file"]}}));

        let absent = policy_for_guardrails(None, &fallback).expect("absent policy");
        assert!(Arc::ptr_eq(&absent, &fallback));
        assert!(
            absent
                .sensitive_tool("artifact", "files", "delete_file")
                .is_some()
        );

        let empty = Map::new();
        let resolved = policy_for_guardrails(Some(&empty), &fallback).expect("empty policy");
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

        let resolved = policy_for_guardrails(Some(&guardrails), &fallback).expect("command policy");
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
