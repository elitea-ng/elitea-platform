//! Immutable runtime-policy projection into ADK's native tool confirmation.
//!
//! Main/runtime administration owns the toolkit-keyed policy dictionaries.
//! This module joins one immutable policy generation to the already
//! materialized toolsets, producing only the concrete ADK names that require
//! confirmation. Tool groups such as `read` and `write` are never authority.

#![allow(dead_code)] // Production activation waits for the durable resume ledger.

use std::collections::BTreeMap;
use std::sync::Arc;
use std::time::Duration;

use adk_rust::tool::SimpleToolContext;
use adk_rust::{ReadonlyContext, Toolset};
use serde_json::{Map, Value};

use super::runtime::{NativeAgentAssemblyError, NativeAgentAssemblyErrorCode};
use crate::toolkits::{
    AdmittedToolSnapshot, FrozenToolKind, SensitiveToolPolicy, ToolAdmissionPolicy,
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
    entries: BTreeMap<Box<str>, SensitiveToolEntry>,
}

#[derive(Clone)]
struct SensitiveToolEntry {
    policy: SensitiveToolPolicy,
    read_only: bool,
}

impl SensitiveToolCatalog {
    #[must_use]
    pub(crate) fn is_empty(&self) -> bool {
        self.entries.is_empty()
    }

    pub(crate) fn tool_names(&self) -> impl Iterator<Item = &str> {
        self.entries.keys().map(AsRef::as_ref)
    }

    #[must_use]
    pub(crate) fn policy_for(&self, tool_name: &str) -> Option<&SensitiveToolPolicy> {
        self.entries.get(tool_name).map(|entry| &entry.policy)
    }

    /// Return whether one exact sensitive tool was admitted as read-only.
    ///
    /// This is execution metadata, not authorization. It only permits the
    /// capability-disabled direct-HITL replay seam to exclude effects while
    /// the durable effect owner is still absent.
    #[must_use]
    pub(crate) fn is_read_only(&self, tool_name: &str) -> Option<bool> {
        self.entries.get(tool_name).map(|entry| entry.read_only)
    }

    pub(crate) fn merge(&mut self, other: Self) -> Result<(), NativeAgentAssemblyError> {
        if self
            .entries
            .len()
            .checked_add(other.entries.len())
            .is_none_or(|count| count > MAX_CONFIRMED_TOOLS)
        {
            return Err(resource_exhausted());
        }
        for (name, policy) in other.entries {
            if self.entries.insert(name, policy).is_some() {
                return Err(invalid_configuration());
            }
        }
        Ok(())
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
        Ok(Self {
            entries: BTreeMap::from([(tool_name.into(), SensitiveToolEntry { policy, read_only })]),
        })
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
    sensitive_tools_for_kind_with_renames(snapshot, kind, toolsets, policy, &[]).await
}

/// The same, told which tools this invocation EXPOSES under another name (#983).
///
/// The policy is keyed by the name the toolkit publishes; ADK's confirmation
/// and the browser's projection are keyed by the name the model is offered.
/// When a collision renamed one of them the two are no longer the same string,
/// so a catalog built from the published name alone would silently stop
/// guarding a sensitive tool the moment a second connection published its name
/// — a security-relevant absence with no error anywhere. `renames` is
/// positional with `toolsets` (empty means "nothing was renamed", which is
/// every agent that did not collide).
pub(crate) async fn sensitive_tools_for_kind_with_renames(
    snapshot: &AdmittedToolSnapshot<'_>,
    kind: FrozenToolKind,
    toolsets: &[Arc<dyn Toolset>],
    policy: &ToolAdmissionPolicy,
    renames: &[BTreeMap<Box<str>, Box<str>>],
) -> Result<SensitiveToolCatalog, NativeAgentAssemblyError> {
    let references = snapshot
        .iter()
        .filter(|reference| reference.kind() == kind)
        .collect::<Vec<_>>();
    if references.len() != toolsets.len()
        || (!renames.is_empty() && renames.len() != toolsets.len())
    {
        return Err(invalid_configuration());
    }
    let context: Arc<dyn ReadonlyContext> =
        Arc::new(SimpleToolContext::new("elitea_sensitive_policy"));
    let mut catalog = SensitiveToolCatalog::default();
    for (index, (reference, toolset)) in references.into_iter().zip(toolsets).enumerate() {
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
            let exposed = renames
                .get(index)
                .and_then(|map| map.get(tool.name()))
                .map_or(tool.name(), AsRef::as_ref);
            if catalog
                .entries
                .insert(
                    exposed.into(),
                    SensitiveToolEntry {
                        policy: sensitive,
                        read_only: tool.is_read_only(),
                    },
                )
                .is_some()
            {
                return Err(invalid_configuration());
            }
        }
    }
    Ok(catalog)
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
    use std::sync::Arc;

    use serde_json::{Map, Value, json};

    use super::policy_for_guardrails;
    use crate::toolkits::ToolAdmissionPolicy;

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
}
