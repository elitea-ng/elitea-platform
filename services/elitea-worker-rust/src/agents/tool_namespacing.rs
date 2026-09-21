//! Per-toolkit namespacing for tool names two toolsets both publish (#983).
//!
//! An agent hands the model ONE flat list of function names. Two MCP
//! connections that both publish `echo` — or an MCP connection whose catalogue
//! happens to carry a name a configured toolkit also uses — therefore hand it
//! two functions with one name, and ADK refuses the whole invocation
//! (`adk-agent`'s `resolve_tools`: "duplicate tool name '…': conflict between
//! toolset '…' and toolset '…'"). That refusal arrives AFTER
//! `agent_native_assembly_completed`, as a `native_agent.event_failed` with no
//! message naming the collision or either connection: measured ~75 ms after
//! assembly, with two plain `/mcp` connections and no authorization involved.
//!
//! The policy this module implements is NAMESPACING, not first-wins and not
//! refusal:
//!
//!  * A name only ONE toolset publishes is left exactly as it is. This is the
//!    overwhelming majority of every agent, and it is why stored prompts,
//!    journals, guardrail dictionaries and transcripts are unchanged by this
//!    module existing.
//!  * A name TWO OR MORE toolsets publish is exposed once per owning toolset
//!    as `<toolkit>__<tool>`, where `<toolkit>` is the owning toolset's own
//!    name reduced to the characters a model provider accepts in a function
//!    name. Both connections keep their tool; neither is silently dropped.
//!  * The rename is recorded in ONE notice, naming every connection and every
//!    renamed name, which the session seeds into the run the same way #866 and
//!    #973 seed theirs. A model that was told `echo` in a stored prompt can
//!    read there why the function it is offered is called something else.
//!
//! Routing is by MAP, never by parsing the exposed name: the exposed tool
//! delegates `execute` to the exact `Arc<dyn Tool>` the owning toolset
//! produced, so a `__` inside a toolkit name or a tool name cannot send a call
//! to the wrong server.

use std::collections::{BTreeMap, BTreeSet};
use std::sync::Arc;
use std::time::Duration;

use adk_rust::tool::SimpleToolContext;
use adk_rust::{ReadonlyContext, Tool, ToolContext, Toolset};
use async_trait::async_trait;
use serde_json::Value;

use super::runtime::{NativeAgentAssemblyError, NativeAgentAssemblyErrorCode};

/// The same bound `sensitive_tools` uses to enumerate the same toolsets: these
/// are already-materialized, in-memory catalogues, so a slow answer is a bug
/// and not a network wait.
const TOOL_ENUMERATION_TIMEOUT: Duration = Duration::from_secs(1);

/// The tightest function-name bound among the providers this runtime speaks to
/// (`OpenAI`'s `^[a-zA-Z0-9_-]{1,64}$`). Anthropic allows 128 and the gateway
/// allows 256; the exposed name has to satisfy the strictest of them because
/// one agent's toolset list is not per-provider.
const MAX_EXPOSED_NAME_BYTES: usize = 64;

/// Toolkit labels are stored rows with no length rule of their own, and the
/// notice below is read by a person and by the model. Bound the label before
/// either sees it — the same reason `bounded_skipped_label` exists for #973.
const MAX_NOTICE_LABEL_CHARS: usize = 120;

/// Refuse to enumerate an implausible toolset rather than build a rename table
/// out of it. Matches `sensitive_tools`' own ceiling.
const MAX_TOOLSET_TOOLS: usize = 1_024;

/// One tool that had to be renamed, and who published it.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct RenamedTool {
    /// The owning toolset's name — an MCP connection's name, or a configured
    /// toolkit's.
    pub(crate) toolkit: String,
    /// The name the server published, which another toolset also published.
    pub(crate) original: String,
    /// The name the model is offered instead.
    pub(crate) exposed: String,
}

/// The rename table for one agent's toolsets, positional with the slice it was
/// planned from.
#[derive(Clone, Debug, Default)]
pub(crate) struct ToolNamespacePlan {
    /// One map per toolset, in the order the toolsets were given: published
    /// name → exposed name. An untouched toolset has an EMPTY map, which is
    /// the common case and costs nothing to apply.
    renames: Vec<BTreeMap<Box<str>, Box<str>>>,
    /// Every rename, ordered by original name then toolkit name, so the notice
    /// and the tests do not depend on toolset order.
    renamed: Vec<RenamedTool>,
}

impl ToolNamespacePlan {
    /// Whether anything at all collided. `false` for almost every agent.
    #[must_use]
    pub(crate) fn is_empty(&self) -> bool {
        self.renamed.is_empty()
    }

    /// The renames, for the notice and for tests.
    #[must_use]
    pub(crate) fn renamed(&self) -> &[RenamedTool] {
        &self.renamed
    }

    /// The rename map for one toolset position, empty when it was untouched.
    #[must_use]
    pub(crate) fn renames_for(&self, index: usize) -> Option<&BTreeMap<Box<str>, Box<str>>> {
        self.renames.get(index)
    }

    /// The map slice covering `range`, for a caller that planned over the
    /// concatenation of two kinds and now has to bind each kind's own catalog.
    #[must_use]
    pub(crate) fn renames_slice(
        &self,
        start: usize,
        end: usize,
    ) -> &[BTreeMap<Box<str>, Box<str>>] {
        self.renames.get(start..end).unwrap_or(&[])
    }
}

/// Plan the exposed name of every tool `toolsets` publishes.
///
/// Enumerates each toolset once, with the same bounded, contextless
/// enumeration `sensitive_tools_for_kind` performs on the same values, and
/// returns an empty plan when no name is published twice.
pub(crate) async fn plan_tool_namespacing(
    toolsets: &[Arc<dyn Toolset>],
) -> Result<ToolNamespacePlan, NativeAgentAssemblyError> {
    let context: Arc<dyn ReadonlyContext> = Arc::new(SimpleToolContext::new("elitea_tool_names"));
    let mut published: Vec<Vec<String>> = Vec::with_capacity(toolsets.len());
    for toolset in toolsets {
        let tools = tokio::time::timeout(
            TOOL_ENUMERATION_TIMEOUT,
            toolset.tools(Arc::clone(&context)),
        )
        .await
        .map_err(|_| dependency_unavailable())?
        .map_err(|_| dependency_unavailable())?;
        if tools.len() > MAX_TOOLSET_TOOLS {
            return Err(resource_exhausted());
        }
        published.push(tools.iter().map(|tool| tool.name().to_owned()).collect());
    }
    Ok(plan_from_published_names(
        &published,
        &toolsets
            .iter()
            .map(|toolset| toolset.name().to_owned())
            .collect::<Vec<_>>(),
    ))
}

/// The pure half of `plan_tool_namespacing`, which is what the unit tests
/// drive: enumeration is I/O-shaped and the policy is not.
pub(crate) fn plan_from_published_names(
    published: &[Vec<String>],
    toolkit_names: &[String],
) -> ToolNamespacePlan {
    let mut owners: BTreeMap<&str, Vec<usize>> = BTreeMap::new();
    for (index, names) in published.iter().enumerate() {
        // A name repeated WITHIN one toolset is not a namespacing problem —
        // `admit_materialized_toolset` and the MCP catalogue check already
        // refuse that — and counting it twice here would namespace a toolset
        // against itself.
        for name in names.iter().collect::<BTreeSet<_>>() {
            owners.entry(name.as_str()).or_default().push(index);
        }
    }
    let mut plan = ToolNamespacePlan {
        renames: vec![BTreeMap::new(); published.len()],
        renamed: Vec::new(),
    };
    // Every name that stays as published is already spoken for, so a
    // namespaced name can never be minted onto one of them.
    let mut taken: BTreeSet<String> = owners
        .iter()
        .filter(|(_, indexes)| indexes.len() < 2)
        .map(|(name, _)| (*name).to_owned())
        .collect();
    for (name, indexes) in &owners {
        if indexes.len() < 2 {
            continue;
        }
        for index in indexes {
            let toolkit = toolkit_names.get(*index).map_or("", String::as_str);
            let exposed = unique_exposed_name(toolkit, name, &taken);
            taken.insert(exposed.clone());
            plan.renames[*index].insert((*name).into(), exposed.as_str().into());
            plan.renamed.push(RenamedTool {
                toolkit: notice_label(toolkit),
                original: notice_label(name),
                exposed,
            });
        }
    }
    plan
}

/// Re-expose every toolset under the plan's names.
///
/// A toolset with no renames is returned untouched (the same `Arc`), so an
/// agent with no collision carries no wrapper at all.
#[must_use]
pub(crate) fn apply_tool_namespacing(
    plan: &ToolNamespacePlan,
    toolsets: Vec<Arc<dyn Toolset>>,
) -> Vec<Arc<dyn Toolset>> {
    toolsets
        .into_iter()
        .enumerate()
        .map(|(index, toolset)| match plan.renames_for(index) {
            Some(renames) if !renames.is_empty() => Arc::new(NamespacedToolset {
                inner: toolset,
                renames: renames.clone(),
            }) as Arc<dyn Toolset>,
            _ => toolset,
        })
        .collect()
}

/// The ONE notice a run carries when anything was renamed.
///
/// One line per COLLIDING NAME, not per rename, so a person reading it sees
/// the pairing rather than a list they have to re-assemble: which connections
/// published the name, and what each one is called now.
#[must_use]
pub(crate) fn renamed_tools_notice_text(renamed: &[RenamedTool]) -> Option<String> {
    if renamed.is_empty() {
        return None;
    }
    let mut by_original: BTreeMap<&str, Vec<&RenamedTool>> = BTreeMap::new();
    for entry in renamed {
        by_original.entry(&entry.original).or_default().push(entry);
    }
    let lines = by_original
        .into_iter()
        .map(|(original, entries)| {
            let pairs = entries
                .iter()
                .map(|entry| format!("'{}' for '{}'", entry.exposed, entry.toolkit))
                .collect::<Vec<_>>()
                .join(", ");
            format!("tool '{original}' is published by more than one connection; call {pairs}")
        })
        .collect::<Vec<_>>();
    Some(lines.join("\n"))
}

/// `<toolkit>__<tool>`, reduced to a legal function name and made unique.
fn unique_exposed_name(toolkit: &str, tool: &str, taken: &BTreeSet<String>) -> String {
    let prefix = function_name_slug(toolkit);
    let candidate = fit(&prefix, tool);
    if !taken.contains(&candidate) {
        return candidate;
    }
    // A second connection whose name reduces to the same slug (two rows both
    // called "MCP server", or two names that differ only in punctuation) would
    // otherwise mint the same exposed name twice and reproduce the very
    // collision this module exists to remove. Disambiguate deterministically.
    for suffix in 2..=u32::MAX {
        let candidate = fit(&format!("{prefix}_{suffix}"), tool);
        if !taken.contains(&candidate) {
            return candidate;
        }
    }
    candidate
}

/// Join a prefix and a tool name within `MAX_EXPOSED_NAME_BYTES`.
///
/// The PREFIX is shortened first: it is the disambiguator, but the tool name is
/// what a reader recognizes, so a long toolkit label must not erase it. When
/// even that is not enough the whole name is truncated, which is safe because
/// nothing parses the exposed name — uniqueness is enforced by the caller and
/// routing is by map.
fn fit(prefix: &str, tool: &str) -> String {
    let tool = function_name_slug(tool);
    let budget = MAX_EXPOSED_NAME_BYTES.saturating_sub(tool.len() + 2);
    let prefix = truncate_chars(prefix, budget.max(1));
    let mut name = format!("{prefix}__{tool}");
    if name.len() > MAX_EXPOSED_NAME_BYTES {
        name = truncate_chars(&name, MAX_EXPOSED_NAME_BYTES);
    }
    name
}

fn truncate_chars(value: &str, max: usize) -> String {
    value.chars().take(max).collect()
}

/// Reduce arbitrary stored text to `[a-z0-9_-]`, which every provider this
/// runtime speaks to accepts in a function name.
fn function_name_slug(value: &str) -> String {
    let mut slug = String::with_capacity(value.len());
    for character in value.chars() {
        if character.is_ascii_alphanumeric() {
            slug.push(character.to_ascii_lowercase());
        } else if character == '-' || character == '_' {
            slug.push(character);
        } else if !slug.ends_with('_') {
            slug.push('_');
        }
    }
    let trimmed = slug.trim_matches('_');
    if trimmed.is_empty() {
        "toolkit".to_owned()
    } else {
        trimmed.to_owned()
    }
}

/// Bound and de-control a label before it reaches the notice.
fn notice_label(value: &str) -> String {
    value
        .chars()
        .filter(|character| !character.is_control())
        .take(MAX_NOTICE_LABEL_CHARS)
        .collect()
}

/// One toolset re-exposed under the plan's names.
struct NamespacedToolset {
    inner: Arc<dyn Toolset>,
    renames: BTreeMap<Box<str>, Box<str>>,
}

#[async_trait]
impl Toolset for NamespacedToolset {
    fn name(&self) -> &str {
        self.inner.name()
    }

    async fn tools(&self, ctx: Arc<dyn ReadonlyContext>) -> adk_rust::Result<Vec<Arc<dyn Tool>>> {
        let tools = self.inner.tools(ctx).await?;
        Ok(tools
            .into_iter()
            .map(|tool| match self.renames.get(tool.name()) {
                Some(exposed) => {
                    Arc::new(ExposedTool::new(tool, exposed.as_ref(), self.inner.name()))
                        as Arc<dyn Tool>
                }
                None => tool,
            })
            .collect())
    }
}

/// One tool published under a different function name.
///
/// Every other question about the tool — schema, description, sensitivity,
/// execution — is answered by the tool the owning toolset produced. Only the
/// name is this wrapper's own.
struct ExposedTool {
    inner: Arc<dyn Tool>,
    name: Box<str>,
    description: Box<str>,
}

impl ExposedTool {
    fn new(inner: Arc<dyn Tool>, name: &str, toolkit: &str) -> Self {
        // The published name is in the description because the model is the
        // one that has to connect an instruction saying "call echo" to a
        // function called `conn_a__echo`, and the description is the only
        // per-tool text it reads.
        let description = format!(
            "{} (published as '{}' by connection '{}'; renamed because another connection publishes the same name)",
            inner.description(),
            notice_label(inner.name()),
            notice_label(toolkit),
        );
        Self {
            name: name.into(),
            description: description.into(),
            inner,
        }
    }
}

#[async_trait]
impl Tool for ExposedTool {
    fn name(&self) -> &str {
        &self.name
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn enhanced_description(&self) -> String {
        self.description.to_string()
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

    async fn execute(&self, ctx: Arc<dyn ToolContext>, args: Value) -> adk_rust::Result<Value> {
        self.inner.execute(ctx, args).await
    }
}

const fn resource_exhausted() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::ResourceExhausted,
        "the agent toolsets publish more tools than the runtime can name",
    )
}

const fn dependency_unavailable() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::DependencyUnavailable,
        "the agent toolsets could not be enumerated for tool naming",
    )
}
