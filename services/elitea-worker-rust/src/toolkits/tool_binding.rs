//! Deterministic provider-visible names for composed toolkit operations.
//!
//! Elitea persists selections as `(toolkit alias, operation name)`. ADK models,
//! however, receive one flat function namespace. This boundary keeps unique
//! operation names unchanged and qualifies every external member of a real
//! collision while retaining the exact original `Tool` as the execution target.

use std::collections::{BTreeMap, BTreeSet};
use std::fmt;
use std::sync::Arc;
use std::time::Duration;

use adk_rust::tool::{BasicToolset, SimpleToolContext};
use adk_rust::{ReadonlyContext, Tool, ToolContext, Toolset};
use async_trait::async_trait;
use ring::digest;
use serde_json::Value;

const MAX_PROVIDER_TOOL_NAME_LENGTH: usize = 128;
const MAX_TOOLSETS: usize = 1_024;
const MAX_TOOLS: usize = 4_096;
const TOOL_ENUMERATION_TIMEOUT: Duration = Duration::from_secs(1);

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ToolBindingError {
    InvalidConfiguration,
    ResourceExhausted,
    DependencyUnavailable,
}

impl fmt::Display for ToolBindingError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self {
            Self::InvalidConfiguration => "the runtime tool namespace is invalid",
            Self::ResourceExhausted => "the runtime tool namespace exceeds its approved limit",
            Self::DependencyUnavailable => "the runtime tool namespace could not be enumerated",
        })
    }
}

impl std::error::Error for ToolBindingError {}

/// One invocation-frozen toolkit and its exact admitted tools.
#[derive(Clone)]
pub(crate) struct FrozenToolset {
    name: String,
    tools: Vec<Arc<dyn Tool>>,
}

impl FrozenToolset {
    #[must_use]
    pub(crate) fn name(&self) -> &str {
        &self.name
    }

    #[must_use]
    pub(crate) fn tools(&self) -> &[Arc<dyn Tool>] {
        &self.tools
    }

    pub(crate) fn select(&self, selected: &[String]) -> Result<Self, ToolBindingError> {
        let mut available = BTreeMap::new();
        for tool in &self.tools {
            if available.insert(tool.name(), Arc::clone(tool)).is_some() {
                return Err(ToolBindingError::InvalidConfiguration);
            }
        }
        let tools = selected
            .iter()
            .map(|name| {
                available
                    .get(name.as_str())
                    .cloned()
                    .ok_or(ToolBindingError::InvalidConfiguration)
            })
            .collect::<Result<Vec<_>, _>>()?;
        Ok(Self {
            name: self.name.clone(),
            tools,
        })
    }
}

#[derive(Clone, Debug, Eq, Ord, PartialEq, PartialOrd)]
struct ToolIdentity {
    toolset_name: String,
    tool_name: String,
}

type OriginalTools = Vec<(ToolIdentity, Arc<dyn Tool>)>;
type ToolNameCounts = BTreeMap<String, usize>;

/// Provider alias lookup plus the static toolsets handed to ADK.
pub(crate) struct ToolBindingPlan {
    toolsets: Vec<Arc<dyn Toolset>>,
    provider_names: BTreeMap<ToolIdentity, String>,
}

impl ToolBindingPlan {
    pub(crate) fn into_toolsets(self) -> Vec<Arc<dyn Toolset>> {
        self.toolsets
    }

    #[must_use]
    pub(crate) fn provider_name(&self, toolset_name: &str, tool_name: &str) -> Option<&str> {
        self.provider_names
            .get(&ToolIdentity {
                toolset_name: toolset_name.to_owned(),
                tool_name: tool_name.to_owned(),
            })
            .map(String::as_str)
    }

    pub(crate) fn bindings(&self) -> impl Iterator<Item = (&str, &str, &str)> {
        self.provider_names.iter().map(|(identity, provider_name)| {
            (
                identity.toolset_name.as_str(),
                identity.tool_name.as_str(),
                provider_name.as_str(),
            )
        })
    }
}

/// Resolve each already-materialized toolset once inside the invocation.
pub(crate) async fn freeze_toolsets(
    toolsets: Vec<Arc<dyn Toolset>>,
    context_label: &str,
) -> Result<Vec<FrozenToolset>, ToolBindingError> {
    if toolsets.len() > MAX_TOOLSETS {
        return Err(ToolBindingError::ResourceExhausted);
    }
    let context: Arc<dyn ReadonlyContext> = Arc::new(SimpleToolContext::new(context_label));
    let mut frozen = Vec::with_capacity(toolsets.len());
    let mut total = 0_usize;
    for toolset in toolsets {
        let name = toolset.name().to_owned();
        if !valid_identity(&name) {
            return Err(ToolBindingError::InvalidConfiguration);
        }
        let tools = tokio::time::timeout(
            TOOL_ENUMERATION_TIMEOUT,
            toolset.tools(Arc::clone(&context)),
        )
        .await
        .map_err(|_| ToolBindingError::DependencyUnavailable)?
        .map_err(|_| ToolBindingError::DependencyUnavailable)?;
        total = total
            .checked_add(tools.len())
            .ok_or(ToolBindingError::ResourceExhausted)?;
        if total > MAX_TOOLS {
            return Err(ToolBindingError::ResourceExhausted);
        }
        frozen.push(FrozenToolset { name, tools });
    }
    Ok(frozen)
}

/// Freeze and bind a complete model-callable namespace.
pub(crate) async fn bind_toolsets(
    toolsets: Vec<Arc<dyn Toolset>>,
    reserved_toolsets: &BTreeSet<String>,
    context_label: &str,
) -> Result<ToolBindingPlan, ToolBindingError> {
    let frozen = freeze_toolsets(toolsets, context_label).await?;
    bind_frozen_toolsets(&frozen, reserved_toolsets)
}

/// Build one deterministic flat namespace over a selected static toolkit set.
pub(crate) fn bind_frozen_toolsets(
    toolsets: &[FrozenToolset],
    reserved_toolsets: &BTreeSet<String>,
) -> Result<ToolBindingPlan, ToolBindingError> {
    let (originals, name_counts) = collect_originals(toolsets, reserved_toolsets)?;
    let aliases = build_aliases(&originals, &name_counts, reserved_toolsets)?;

    let originals_by_identity = originals.into_iter().collect::<BTreeMap<_, _>>();
    let mut bound_toolsets = Vec::with_capacity(toolsets.len());
    for toolset in toolsets {
        let mut bound = Vec::with_capacity(toolset.tools().len());
        for tool in toolset.tools() {
            let identity = ToolIdentity {
                toolset_name: toolset.name().to_owned(),
                tool_name: tool.name().to_owned(),
            };
            let provider_name = aliases
                .get(&identity)
                .ok_or(ToolBindingError::InvalidConfiguration)?;
            let original = originals_by_identity
                .get(&identity)
                .cloned()
                .ok_or(ToolBindingError::InvalidConfiguration)?;
            if provider_name == original.name() {
                bound.push(original);
            } else {
                bound.push(Arc::new(ProviderAliasedTool::new(
                    original,
                    provider_name.clone(),
                    toolset.name(),
                )) as Arc<dyn Tool>);
            }
        }
        bound_toolsets.push(Arc::new(BasicToolset::new(toolset.name(), bound)) as Arc<dyn Toolset>);
    }
    Ok(ToolBindingPlan {
        toolsets: bound_toolsets,
        provider_names: aliases,
    })
}

fn collect_originals(
    toolsets: &[FrozenToolset],
    reserved_toolsets: &BTreeSet<String>,
) -> Result<(OriginalTools, ToolNameCounts), ToolBindingError> {
    if toolsets.len() > MAX_TOOLSETS {
        return Err(ToolBindingError::ResourceExhausted);
    }
    let mut originals = Vec::new();
    let mut identities = BTreeSet::new();
    let mut name_counts = BTreeMap::<String, usize>::new();
    let mut reserved_counts = BTreeMap::<String, usize>::new();
    for toolset in toolsets {
        if !valid_identity(toolset.name()) {
            return Err(ToolBindingError::InvalidConfiguration);
        }
        for tool in toolset.tools() {
            if !valid_identity(tool.name()) {
                return Err(ToolBindingError::InvalidConfiguration);
            }
            let identity = ToolIdentity {
                toolset_name: toolset.name().to_owned(),
                tool_name: tool.name().to_owned(),
            };
            if !identities.insert(identity.clone()) {
                return Err(ToolBindingError::InvalidConfiguration);
            }
            *name_counts.entry(tool.name().to_owned()).or_default() += 1;
            if reserved_toolsets.contains(toolset.name()) {
                *reserved_counts.entry(tool.name().to_owned()).or_default() += 1;
            }
            originals.push((identity, Arc::clone(tool)));
        }
    }
    if originals.len() > MAX_TOOLS {
        return Err(ToolBindingError::ResourceExhausted);
    }
    if reserved_counts.values().any(|count| *count > 1) {
        return Err(ToolBindingError::InvalidConfiguration);
    }
    Ok((originals, name_counts))
}

fn build_aliases(
    originals: &[(ToolIdentity, Arc<dyn Tool>)],
    name_counts: &BTreeMap<String, usize>,
    reserved_toolsets: &BTreeSet<String>,
) -> Result<BTreeMap<ToolIdentity, String>, ToolBindingError> {
    let reserved_names = originals
        .iter()
        .filter_map(|(identity, _)| {
            let count = name_counts.get(&identity.tool_name).copied().unwrap_or(0);
            (count == 1 || reserved_toolsets.contains(&identity.toolset_name))
                .then_some(identity.tool_name.clone())
        })
        .collect::<BTreeSet<_>>();
    let mut aliases = originals
        .iter()
        .map(|(identity, _)| {
            let count = name_counts.get(&identity.tool_name).copied().unwrap_or(0);
            let name = if count == 1 || reserved_toolsets.contains(&identity.toolset_name) {
                Ok(identity.tool_name.clone())
            } else {
                collision_alias(identity, false)
            }?;
            Ok((identity.clone(), name))
        })
        .collect::<Result<BTreeMap<_, _>, ToolBindingError>>()?;
    let mut alias_counts = BTreeMap::<String, usize>::new();
    for alias in aliases.values() {
        *alias_counts.entry(alias.clone()).or_default() += 1;
    }
    for (identity, provider_name) in &mut aliases {
        if name_counts.get(&identity.tool_name).copied().unwrap_or(0) <= 1
            || reserved_toolsets.contains(&identity.toolset_name)
        {
            continue;
        }
        if alias_counts.get(provider_name).copied().unwrap_or(0) > 1
            || reserved_names.contains(provider_name)
        {
            *provider_name = collision_alias(identity, true)?;
        }
    }
    if aliases.values().collect::<BTreeSet<_>>().len() != aliases.len() {
        return Err(ToolBindingError::InvalidConfiguration);
    }
    Ok(aliases)
}

fn collision_alias(identity: &ToolIdentity, force_hash: bool) -> Result<String, ToolBindingError> {
    let qualifier = sanitize_name(&identity.toolset_name);
    let logical = sanitize_name(&identity.tool_name);
    if qualifier.is_empty() || logical.is_empty() {
        return Err(ToolBindingError::InvalidConfiguration);
    }
    let name = format!("{qualifier}__{logical}");
    if !force_hash && name.len() <= MAX_PROVIDER_TOOL_NAME_LENGTH {
        return Ok(name);
    }
    let digest_input = format!("{}\u{1f}{}", identity.toolset_name, identity.tool_name);
    let digest = digest::digest(&digest::SHA256, digest_input.as_bytes());
    let bytes = digest.as_ref();
    let suffix = format!(
        "{:02x}{:02x}{:02x}{:02x}",
        bytes[0], bytes[1], bytes[2], bytes[3]
    );
    let prefix_len = MAX_PROVIDER_TOOL_NAME_LENGTH - suffix.len() - 2;
    Ok(format!("{}__{suffix}", &name[..name.len().min(prefix_len)]))
}

fn sanitize_name(value: &str) -> String {
    let mut output = String::with_capacity(value.len());
    let mut unsafe_run = false;
    for character in value.chars() {
        if character.is_ascii_alphanumeric() || matches!(character, '_' | '-') {
            output.push(character);
            unsafe_run = false;
        } else if !unsafe_run {
            output.push('_');
            unsafe_run = true;
        }
    }
    output.trim_matches(['_', '-']).to_owned()
}

fn valid_identity(value: &str) -> bool {
    !value.is_empty() && value.len() <= 1_024 && !value.chars().any(char::is_control)
}

struct ProviderAliasedTool {
    inner: Arc<dyn Tool>,
    name: String,
    description: String,
}

impl ProviderAliasedTool {
    fn new(inner: Arc<dyn Tool>, name: String, toolset_name: &str) -> Self {
        let description = format!("[Toolkit: {toolset_name}] {}", inner.description());
        Self {
            inner,
            name,
            description,
        }
    }
}

#[async_trait]
impl Tool for ProviderAliasedTool {
    fn name(&self) -> &str {
        &self.name
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn declaration(&self) -> Value {
        let mut declaration = self.inner.declaration();
        if let Some(object) = declaration.as_object_mut() {
            object.insert("name".to_owned(), Value::String(self.name.clone()));
            object.insert(
                "description".to_owned(),
                Value::String(self.description.clone()),
            );
        }
        declaration
    }

    fn enhanced_description(&self) -> String {
        self.description.clone()
    }

    fn is_long_running(&self) -> bool {
        self.inner.is_long_running()
    }

    fn is_builtin(&self) -> bool {
        self.inner.is_builtin()
    }

    fn parameters_schema(&self) -> Option<Value> {
        self.inner.parameters_schema()
    }

    fn response_schema(&self) -> Option<Value> {
        self.inner.response_schema()
    }

    fn required_scopes(&self) -> &[&str] {
        self.inner.required_scopes()
    }

    fn is_read_only(&self) -> bool {
        self.inner.is_read_only()
    }

    fn is_concurrency_safe(&self) -> bool {
        self.inner.is_concurrency_safe()
    }

    async fn execute(
        &self,
        context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_rust::Result<Value> {
        self.inner.execute(context, arguments).await
    }
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeSet;
    use std::sync::Arc;

    use adk_rust::tool::{FunctionTool, SimpleToolContext};
    use adk_rust::{ReadonlyContext, ToolContext};
    use serde_json::json;

    use super::{FrozenToolset, bind_frozen_toolsets};

    fn tool(name: &str, result: &'static str) -> Arc<dyn adk_rust::Tool> {
        Arc::new(FunctionTool::new(
            name,
            format!("Run {name}"),
            move |_context, _arguments| async move { Ok(json!(result)) },
        ))
    }

    #[tokio::test]
    async fn qualifies_collisions_and_delegates_to_exact_originals() {
        let attachments = tool("list_indexes", "bucket indexes");
        let configurations = tool("list_indexes", "repo indexes");
        let plan = bind_frozen_toolsets(
            &[
                FrozenToolset {
                    name: "attachments".to_owned(),
                    tools: vec![attachments],
                },
                FrozenToolset {
                    name: "configurations".to_owned(),
                    tools: vec![configurations],
                },
            ],
            &BTreeSet::default(),
        )
        .expect("binding plan");
        assert_eq!(
            plan.provider_name("attachments", "list_indexes"),
            Some("attachments__list_indexes")
        );
        assert_eq!(
            plan.provider_name("configurations", "list_indexes"),
            Some("configurations__list_indexes")
        );
        let context: Arc<dyn ReadonlyContext> = Arc::new(SimpleToolContext::new("binding_test"));
        let toolsets = plan.into_toolsets();
        let first = toolsets[0]
            .tools(Arc::clone(&context))
            .await
            .expect("tools");
        let second = toolsets[1].tools(context).await.expect("tools");
        assert_eq!(first[0].name(), "attachments__list_indexes");
        assert_eq!(second[0].name(), "configurations__list_indexes");
        let first_context: Arc<dyn ToolContext> =
            Arc::new(SimpleToolContext::new("binding_execution_test"));
        let second_context: Arc<dyn ToolContext> =
            Arc::new(SimpleToolContext::new("binding_execution_test"));
        assert_eq!(
            first[0]
                .execute(first_context, json!({}))
                .await
                .expect("first original result"),
            json!("bucket indexes")
        );
        assert_eq!(
            second[0]
                .execute(second_context, json!({}))
                .await
                .expect("second original result"),
            json!("repo indexes")
        );
    }

    #[test]
    fn keeps_unique_names_and_is_order_independent() {
        let first = FrozenToolset {
            name: "attachments".to_owned(),
            tools: vec![tool("list_indexes", "bucket")],
        };
        let second = FrozenToolset {
            name: "configurations".to_owned(),
            tools: vec![
                tool("list_indexes", "repo"),
                tool("get_branches", "branches"),
            ],
        };
        let forward = bind_frozen_toolsets(&[first.clone(), second.clone()], &BTreeSet::default())
            .expect("forward");
        let reverse =
            bind_frozen_toolsets(&[second, first], &BTreeSet::default()).expect("reverse");
        assert_eq!(
            forward.bindings().collect::<BTreeSet<_>>(),
            reverse.bindings().collect::<BTreeSet<_>>()
        );
        assert_eq!(
            forward.provider_name("configurations", "get_branches"),
            Some("get_branches")
        );
    }

    #[test]
    fn reserved_runtime_tool_keeps_its_name() {
        let plan = bind_frozen_toolsets(
            &[
                FrozenToolset {
                    name: "ask_user".to_owned(),
                    tools: vec![tool("ask_user", "question")],
                },
                FrozenToolset {
                    name: "custom".to_owned(),
                    tools: vec![tool("ask_user", "custom")],
                },
            ],
            &BTreeSet::from(["ask_user".to_owned()]),
        )
        .expect("binding plan");
        assert_eq!(plan.provider_name("ask_user", "ask_user"), Some("ask_user"));
        assert_eq!(
            plan.provider_name("custom", "ask_user"),
            Some("custom__ask_user")
        );
    }

    #[test]
    fn generated_collision_matrix_is_stable_unique_and_provider_bounded() {
        let toolkit_names = [
            "Alpha Toolkit".to_owned(),
            "alpha/toolkit".to_owned(),
            "alpha_toolkit".to_owned(),
            "customer-api.v2".to_owned(),
            "x".repeat(180),
        ];
        let toolsets = toolkit_names
            .iter()
            .enumerate()
            .map(|(index, name)| {
                let unique_name = format!("unique_{index}");
                FrozenToolset {
                    name: name.clone(),
                    tools: vec![tool("lookup", "collision"), tool(&unique_name, "unique")],
                }
            })
            .collect::<Vec<_>>();
        let forward =
            bind_frozen_toolsets(&toolsets, &BTreeSet::default()).expect("forward binding");
        let mut reversed = toolsets.clone();
        reversed.reverse();
        let reverse =
            bind_frozen_toolsets(&reversed, &BTreeSet::default()).expect("reverse binding");
        let forward_bindings = forward
            .bindings()
            .map(|(toolset, tool, provider)| {
                (toolset.to_owned(), tool.to_owned(), provider.to_owned())
            })
            .collect::<BTreeSet<_>>();
        let reverse_bindings = reverse
            .bindings()
            .map(|(toolset, tool, provider)| {
                (toolset.to_owned(), tool.to_owned(), provider.to_owned())
            })
            .collect::<BTreeSet<_>>();
        assert_eq!(forward_bindings, reverse_bindings);
        let provider_names = forward_bindings
            .iter()
            .map(|(_, _, provider)| provider)
            .collect::<BTreeSet<_>>();
        assert_eq!(provider_names.len(), forward_bindings.len());
        assert!(provider_names.iter().all(|name| {
            !name.is_empty()
                && name.len() <= super::MAX_PROVIDER_TOOL_NAME_LENGTH
                && name
                    .bytes()
                    .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-'))
        }));
        for (index, toolkit_name) in toolkit_names.iter().enumerate() {
            let unique_name = format!("unique_{index}");
            assert_eq!(
                forward.provider_name(toolkit_name, &unique_name),
                Some(unique_name.as_str())
            );
        }
    }
}
