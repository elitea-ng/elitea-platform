//! The per-agent tool envelope (ADR-0050 decision 3, #1613).
//!
//! Main freezes the root agent's `meta.tool_envelope` and the platform's
//! `agents` ceilings into the run input; a host enforces them here:
//!
//! - [`ToolEnvelope::limit_for`]: the most calls of one tool in a turn, the
//!   lowest of every matching `tool_limits` entry and the platform's
//!   `max_tool_limit`. [`limit_toolsets`] wraps each limited tool in a
//!   counter that refuses the call past it ([`TOOL_LIMIT_REACHED`]).
//! - [`ToolEnvelope::admits_unattended`]: whether a turn nobody waits on (an
//!   agent-to-agent turn) may bind the tool. Without `unattended_tools`, the
//!   read-only tools. With it, the tools it names, and a tool that writes
//!   only when the platform's allowlist names it too.
//! - [`ToolEnvelope::admits_subagent`]: whether a sub-agent may be given the
//!   tool. Without `subagent_tools`, the read-only tools (the rule before
//!   the envelope). With it, the tools it names, writes included.
//!
//! Every control only narrows: each one starts from the tools the turn
//! already bound, after the toolkit policy, the guardrails and the
//! approvals, and keeps or counts some of them. None adds a tool, and a
//! tool kept here still asks for every approval it asked for before.
//!
//! A tool key is `<toolkit>/<tool>`: the toolkit by its name in the agent or
//! by its type, and `*` for every tool of the toolkit. Matching ignores case
//! and surrounding space.

use std::collections::BTreeMap;
use std::sync::Arc;
use std::sync::atomic::{AtomicU32, Ordering};

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use async_trait::async_trait;
use serde_json::Value;

use crate::toolkits::FrozenToolset;

/// The most `tool_limits` entries, and the most entries of each list.
pub const MAX_ENTRIES: usize = 256;
/// The longest tool key, in bytes.
pub const MAX_KEY_BYTES: usize = 512;
/// The highest per-tool limit, and the highest platform ceiling.
pub const MAX_CALLS: u32 = 10_000;
/// The tool part that matches every tool of a toolkit.
pub const WILDCARD: &str = "*";
/// The code of a call past its tool's limit.
pub const TOOL_LIMIT_REACHED: &str = "tool_limit_reached";
/// The refusal of a sub-agent tool outside the parent's grant.
pub const SUBAGENT_TOOL_NOT_GRANTED: &str = "SUBAGENT_TOOL_NOT_GRANTED";

/// A frozen envelope that is not well formed.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ToolEnvelopeError {
    /// A malformed key, a limit out of range, or a list without its flag.
    Invalid(&'static str),
    /// More than [`MAX_ENTRIES`] entries.
    TooLarge,
}

/// One parsed tool key.
#[derive(Clone, Debug, Eq, PartialEq)]
struct ToolKey {
    toolkit: String,
    /// `None` is the wildcard.
    tool: Option<String>,
}

impl ToolKey {
    fn parse(raw: &str) -> Result<Self, ToolEnvelopeError> {
        if raw.is_empty() || raw.len() > MAX_KEY_BYTES || raw.chars().any(char::is_control) {
            return Err(ToolEnvelopeError::Invalid("a tool key is malformed"));
        }
        let Some((toolkit, tool)) = raw.split_once('/') else {
            return Err(ToolEnvelopeError::Invalid(
                "a tool key is not \"<toolkit>/<tool>\"",
            ));
        };
        let (toolkit, tool) = (toolkit.trim(), tool.trim());
        if toolkit.is_empty()
            || tool.is_empty()
            || tool.contains('/')
            || (tool != WILDCARD && tool.contains('*'))
        {
            return Err(ToolEnvelopeError::Invalid(
                "a tool key is not \"<toolkit>/<tool>\"",
            ));
        }
        Ok(Self {
            toolkit: toolkit.to_lowercase(),
            tool: (tool != WILDCARD).then(|| tool.to_lowercase()),
        })
    }

    fn canonical(&self) -> String {
        format!(
            "{}/{}",
            self.toolkit,
            self.tool.as_deref().unwrap_or(WILDCARD)
        )
    }

    fn matches(&self, tool: &ToolRef<'_>) -> bool {
        let toolkit_matches = self.toolkit == tool.toolkit_name.trim().to_lowercase()
            || tool
                .toolkit_type
                .is_some_and(|kind| self.toolkit == kind.trim().to_lowercase());
        toolkit_matches
            && self
                .tool
                .as_ref()
                .is_none_or(|name| *name == tool.tool_name.trim().to_lowercase())
    }
}

fn parse_keys(raw: &[String]) -> Result<Vec<ToolKey>, ToolEnvelopeError> {
    if raw.len() > MAX_ENTRIES {
        return Err(ToolEnvelopeError::TooLarge);
    }
    raw.iter().map(|key| ToolKey::parse(key)).collect()
}

/// One tool as the envelope matches it.
#[derive(Clone, Copy, Debug)]
pub struct ToolRef<'a> {
    /// The toolkit's name in the agent (the toolset name).
    pub toolkit_name: &'a str,
    /// The toolkit's type, when the tool is a configured toolkit's.
    pub toolkit_type: Option<&'a str>,
    /// The tool's own name, before any provider alias.
    pub tool_name: &'a str,
}

/// The envelope's fields as the run input carries them
/// (`AgentToolEnvelopeV1`).
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct ToolEnvelopeInput {
    pub tool_limits: Vec<(String, u32)>,
    pub max_tool_limit: u32,
    pub unattended_tools: Vec<String>,
    pub unattended_tools_set: bool,
    pub unattended_tools_allowlist: Vec<String>,
    pub subagent_tools: Vec<String>,
    pub subagent_tools_set: bool,
}

/// One run's validated envelope.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct ToolEnvelope {
    limits: Vec<(ToolKey, u32)>,
    max_tool_limit: u32,
    unattended: Option<Vec<ToolKey>>,
    unattended_allowlist: Vec<ToolKey>,
    subagent: Option<Vec<ToolKey>>,
}

impl ToolEnvelope {
    /// The envelope of a run whose input carries none: no limit, unattended
    /// turns and sub-agents get the read-only tools.
    #[must_use]
    pub fn none() -> Self {
        Self::default()
    }

    /// Validate the frozen fields.
    ///
    /// # Errors
    ///
    /// A malformed key, a limit of 0 or above [`MAX_CALLS`], a list without
    /// its `_set` flag, or more than [`MAX_ENTRIES`] entries.
    pub fn from_input(input: &ToolEnvelopeInput) -> Result<Self, ToolEnvelopeError> {
        if input.tool_limits.len() > MAX_ENTRIES {
            return Err(ToolEnvelopeError::TooLarge);
        }
        if input.max_tool_limit > MAX_CALLS {
            return Err(ToolEnvelopeError::Invalid(
                "the platform tool limit is out of range",
            ));
        }
        let limits = input
            .tool_limits
            .iter()
            .map(|(key, calls)| {
                if *calls == 0 || *calls > MAX_CALLS {
                    return Err(ToolEnvelopeError::Invalid("a tool limit is out of range"));
                }
                Ok((ToolKey::parse(key)?, *calls))
            })
            .collect::<Result<Vec<_>, _>>()?;
        if (!input.unattended_tools_set && !input.unattended_tools.is_empty())
            || (!input.subagent_tools_set && !input.subagent_tools.is_empty())
        {
            return Err(ToolEnvelopeError::Invalid(
                "a tool list is present without its flag",
            ));
        }
        Ok(Self {
            limits,
            max_tool_limit: input.max_tool_limit,
            unattended: input
                .unattended_tools_set
                .then(|| parse_keys(&input.unattended_tools))
                .transpose()?,
            unattended_allowlist: parse_keys(&input.unattended_tools_allowlist)?,
            subagent: input
                .subagent_tools_set
                .then(|| parse_keys(&input.subagent_tools))
                .transpose()?,
        })
    }

    /// The most calls of `tool` in one turn: the lowest matching limit and
    /// the platform's ceiling. `None` is no per-tool limit.
    #[must_use]
    pub fn limit_for(&self, tool: &ToolRef<'_>) -> Option<u32> {
        let own = self
            .limits
            .iter()
            .filter(|(key, _)| key.matches(tool))
            .map(|(_, calls)| *calls)
            .min();
        let ceiling = (self.max_tool_limit > 0).then_some(self.max_tool_limit);
        match (own, ceiling) {
            (Some(own), Some(ceiling)) => Some(own.min(ceiling)),
            (own, ceiling) => own.or(ceiling),
        }
    }

    /// Whether a turn nobody waits on may bind `tool`.
    #[must_use]
    pub fn admits_unattended(&self, tool: &ToolRef<'_>, read_only: bool) -> bool {
        match &self.unattended {
            None => read_only,
            Some(keys) => {
                keys.iter().any(|key| key.matches(tool))
                    && (read_only
                        || self
                            .unattended_allowlist
                            .iter()
                            .any(|key| key.matches(tool)))
            }
        }
    }

    /// Whether `subagent_tools` is set: the sub-agent grant then decides,
    /// writes included.
    #[must_use]
    pub const fn grants_subagent_tools(&self) -> bool {
        self.subagent.is_some()
    }

    /// Whether a sub-agent may be given `tool`.
    #[must_use]
    pub fn admits_subagent(&self, tool: &ToolRef<'_>, read_only: bool) -> bool {
        match &self.subagent {
            None => read_only,
            Some(keys) => keys.iter().any(|key| key.matches(tool)),
        }
    }
}

/// The toolkit type of each configured toolkit, by its name in the agent.
pub type ToolkitKinds = BTreeMap<String, String>;

/// What a sub-agent of this run may be given (decision 4): the envelope's
/// `subagent_tools` over the run's toolkits. A host lends it to the
/// sub-agent runtime ([`crate::subagents::SubAgentConfig::with_grant`]).
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct SubAgentGrant {
    envelope: ToolEnvelope,
    kinds: ToolkitKinds,
}

impl SubAgentGrant {
    #[must_use]
    pub const fn new(envelope: ToolEnvelope, kinds: ToolkitKinds) -> Self {
        Self { envelope, kinds }
    }

    /// Whether a child may be given `tool` of the toolset `toolset`.
    /// Without `subagent_tools`, a read-only tool; with it, a tool it
    /// names, writes included.
    #[must_use]
    pub fn admits(&self, toolset: &str, tool: &dyn Tool) -> bool {
        self.envelope
            .admits_subagent(&tool_ref(toolset, tool, &self.kinds), tool.is_read_only())
    }

    /// Whether `subagent_tools` is set (a child may then get writes).
    #[must_use]
    pub const fn grants_writes(&self) -> bool {
        self.envelope.grants_subagent_tools()
    }

    /// The keys of `subagent_tools`, sorted, for the delegation evidence.
    #[must_use]
    pub fn granted_keys(&self) -> Vec<String> {
        let mut keys: Vec<String> = self
            .envelope
            .subagent
            .iter()
            .flatten()
            .map(ToolKey::canonical)
            .collect();
        keys.sort();
        keys.dedup();
        keys
    }

    /// Whether `requested` (a key of `spawn_agent`'s `tools`) names `tool`.
    #[must_use]
    pub fn names(&self, requested: &RequestedTool, toolset: &str, tool: &dyn Tool) -> bool {
        requested.0.matches(&tool_ref(toolset, tool, &self.kinds))
    }
}

/// One tool key a `spawn_agent` call asks for.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RequestedTool(ToolKey);

impl RequestedTool {
    /// # Errors
    ///
    /// A key that is not `<toolkit>/<tool>`.
    pub fn parse(raw: &str) -> Result<Self, ToolEnvelopeError> {
        ToolKey::parse(raw).map(Self)
    }

    /// The key, lower-cased, as the refusal names it.
    #[must_use]
    pub fn canonical(&self) -> String {
        self.0.canonical()
    }
}

fn tool_ref<'a>(toolset: &'a str, tool: &'a dyn Tool, kinds: &'a ToolkitKinds) -> ToolRef<'a> {
    ToolRef {
        toolkit_name: toolset,
        toolkit_type: kinds.get(toolset).map(String::as_str),
        tool_name: tool.name(),
    }
}

/// `toolsets` with each limited tool wrapped in a counter of its own: the
/// call past its limit does not run and answers [`TOOL_LIMIT_REACHED`].
/// Every tool keeps its name, schema and flags.
#[must_use]
pub fn limit_toolsets(
    toolsets: Vec<FrozenToolset>,
    envelope: &ToolEnvelope,
    kinds: &ToolkitKinds,
) -> (Vec<FrozenToolset>, usize) {
    let mut limited = 0;
    let toolsets = toolsets
        .into_iter()
        .map(|toolset| {
            let tools = toolset
                .tools()
                .iter()
                .map(|tool| {
                    match envelope.limit_for(&tool_ref(toolset.name(), tool.as_ref(), kinds)) {
                        Some(max_calls) => {
                            limited += 1;
                            Arc::new(LimitedTool {
                                inner: Arc::clone(tool),
                                max_calls,
                                used: AtomicU32::new(0),
                            }) as Arc<dyn Tool>
                        }
                        None => Arc::clone(tool),
                    }
                })
                .collect();
            FrozenToolset::new(toolset.name(), tools)
        })
        .collect();
    (toolsets, limited)
}

/// The tools of `toolsets` a turn nobody waits on may bind, and how many
/// were kept and dropped. A toolset left with no tool is dropped.
#[must_use]
pub fn retain_unattended(
    toolsets: Vec<FrozenToolset>,
    envelope: &ToolEnvelope,
    kinds: &ToolkitKinds,
) -> (Vec<FrozenToolset>, usize, usize) {
    let (mut kept, mut dropped) = (0, 0);
    let mut retained = Vec::with_capacity(toolsets.len());
    for toolset in toolsets {
        let tools: Vec<Arc<dyn Tool>> = toolset
            .tools()
            .iter()
            .filter(|tool| {
                envelope.admits_unattended(
                    &tool_ref(toolset.name(), tool.as_ref(), kinds),
                    tool.is_read_only(),
                )
            })
            .cloned()
            .collect();
        kept += tools.len();
        dropped += toolset.tools().len() - tools.len();
        if !tools.is_empty() {
            retained.push(FrozenToolset::new(toolset.name(), tools));
        }
    }
    (retained, kept, dropped)
}

/// A tool counted against its envelope limit.
struct LimitedTool {
    inner: Arc<dyn Tool>,
    max_calls: u32,
    used: AtomicU32,
}

impl LimitedTool {
    fn charge(&self) -> bool {
        self.used
            .try_update(Ordering::AcqRel, Ordering::Acquire, |used| {
                (used < self.max_calls).then(|| used.saturating_add(1))
            })
            .is_ok()
    }
}

fn limit_error(max_calls: u32) -> AdkError {
    AdkError::new(
        ErrorComponent::Guardrail,
        ErrorCategory::Forbidden,
        TOOL_LIMIT_REACHED,
        format!(
            "this tool may be called at most {max_calls} times in a turn; the call does not run"
        ),
    )
}

#[async_trait]
impl Tool for LimitedTool {
    fn name(&self) -> &str {
        self.inner.name()
    }

    fn description(&self) -> &str {
        self.inner.description()
    }

    fn declaration(&self) -> Value {
        self.inner.declaration()
    }

    fn enhanced_description(&self) -> String {
        self.inner.enhanced_description()
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

    fn is_agent_delegation(&self) -> bool {
        self.inner.is_agent_delegation()
    }

    async fn execute(
        &self,
        context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        if !self.charge() {
            return Err(limit_error(self.max_calls));
        }
        self.inner.execute(context, arguments).await
    }
}

#[cfg(test)]
#[path = "tool_envelope_tests.rs"]
mod tests;
