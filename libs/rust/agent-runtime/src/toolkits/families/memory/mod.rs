//! The `memory` toolkit family (worker retirement unit M9): `manage_memory`,
//! `search_memory`, `get_memory` and `delete_memory`, the SDK's long-term
//! memory tools (`elitea_sdk/tools/memory/__init__.py`).
//!
//! Names, argument schemas, descriptions, the `selected_tools` rule (empty
//! selects all four) and the result sentences keep the SDK's. The settings
//! are the SDK's: `namespace` (the toolkit's id when absent) and
//! `selected_tools`; `pgvector_configuration` is read by nothing here, because
//! memories live in `elitea-vector`, not in a database the toolkit names.
//!
//! Like the index tools, the family has no credential and no model of its
//! own. It acts under the authority the host lends for the run (the claim's
//! vector token and the memory embedding model), through
//! [`IndexToolHost::memory`]; where the host does not serve memory (no vector
//! store, no token, no memory embedding model) the toolkit is skipped with the
//! reason logged, as an unserved family always was.
//!
//! Differences from the SDK, stated:
//!
//! * `search_memory` ranks by the query. The SDK's store is built without an
//!   index, so its search ignores the query and lists the most recently
//!   updated memories.
//! * Memories have limits (count, content, metadata, key); the SDK has none.
//! * Memories written under the Python worker are not carried over: they sit
//!   in the toolkit's pgvector database, which nothing here reads.
//! * Memory is shared by everyone who runs a toolkit with the same
//!   `namespace` in the same project, as in the SDK; it is not per user.

use std::sync::Arc;

use adk_core::{Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use super::index_tools::{
    IndexToolAuthority, IndexToolFailure, IndexToolsetError, IndexToolsetErrorCode,
    selected_tool_names,
};
use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

/// The stored toolkit type.
pub const MEMORY_TOOLKIT_TYPE: &str = "memory";
/// The most bytes of `namespace` setting.
pub const MAX_NAMESPACE_BYTES: usize = 256;

/// One of the four tools, by its SDK name.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum MemoryToolName {
    ManageMemory,
    SearchMemory,
    GetMemory,
    DeleteMemory,
}

impl MemoryToolName {
    /// Every tool, in the SDK's listing order.
    pub const ALL: [Self; 4] = [
        Self::ManageMemory,
        Self::SearchMemory,
        Self::GetMemory,
        Self::DeleteMemory,
    ];

    /// The SDK tool name.
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::ManageMemory => "manage_memory",
            Self::SearchMemory => "search_memory",
            Self::GetMemory => "get_memory",
            Self::DeleteMemory => "delete_memory",
        }
    }

    /// The tool of an SDK name.
    #[must_use]
    pub fn parse(name: &str) -> Option<Self> {
        Self::ALL.into_iter().find(|tool| tool.as_str() == name)
    }

    /// Whether the tool changes nothing.
    #[must_use]
    pub const fn is_read_only(self) -> bool {
        matches!(self, Self::SearchMemory | Self::GetMemory)
    }

    /// The SDK's description (the tool function's docstring, dedented).
    #[must_use]
    pub const fn sdk_description(self) -> &'static str {
        match self {
            Self::ManageMemory => {
                "Store information in long-term memory. Use this to remember important facts,\nuser preferences, or any information that should persist across conversations."
            }
            Self::SearchMemory => {
                "Search through stored memories using natural language. Returns memories\nthat are semantically similar to the query."
            }
            Self::GetMemory => "Retrieve a specific memory by its key.",
            Self::DeleteMemory => "Delete a specific memory by its key.",
        }
    }

    /// The SDK's argument schema (`ManageMemoryInput`, `SearchMemoryInput`,
    /// `GetMemoryInput`, `DeleteMemoryInput`) as JSON Schema.
    #[must_use]
    pub fn parameters_schema(self) -> Value {
        match self {
            Self::ManageMemory => json!({
                "type": "object",
                "properties": {
                    "content": {"type": "string", "description": "The content/information to store in memory"},
                    "key": {"type": ["string", "null"], "default": null, "description": "Optional unique key for the memory. If not provided, one will be generated."},
                    "metadata": {"type": ["object", "null"], "default": null, "description": "Optional metadata to attach to the memory"}
                },
                "required": ["content"],
                "additionalProperties": false
            }),
            Self::SearchMemory => json!({
                "type": "object",
                "properties": {
                    "query": {"type": "string", "description": "Natural language query to search memories"},
                    "limit": {"type": "integer", "default": 5, "description": "Maximum number of results to return"}
                },
                "required": ["query"],
                "additionalProperties": false
            }),
            Self::GetMemory => json!({
                "type": "object",
                "properties": {
                    "key": {"type": "string", "description": "The key of the memory to retrieve"}
                },
                "required": ["key"],
                "additionalProperties": false
            }),
            Self::DeleteMemory => json!({
                "type": "object",
                "properties": {
                    "key": {"type": "string", "description": "The key of the memory to delete"}
                },
                "required": ["key"],
                "additionalProperties": false
            }),
        }
    }
}

/// One memory tool call, as the host receives it.
pub struct MemoryToolCall<'a> {
    /// Which tool.
    pub tool: MemoryToolName,
    /// The toolkit's `namespace`.
    pub namespace: &'a str,
    /// The call's arguments, as the model sent them (an object).
    pub arguments: &'a Value,
}

/// The namespace a memory toolkit works in: its `namespace` setting, or the
/// toolkit's id when the setting is absent (the SDK's
/// `settings.get('namespace', str(id))`).
///
/// # Errors
///
/// A `namespace` that is not a non-blank, bounded string, or absent with no
/// toolkit id.
pub fn memory_namespace(
    settings: &Map<String, Value>,
    toolkit_id: Option<u64>,
) -> Result<String, MemoryToolsetError> {
    match settings.get("namespace") {
        None | Some(Value::Null) => toolkit_id
            .map(|id| id.to_string())
            .ok_or(MemoryToolsetError::Invalid),
        Some(Value::String(namespace))
            if !namespace.trim().is_empty()
                && namespace.len() <= MAX_NAMESPACE_BYTES
                && !namespace.chars().any(char::is_control) =>
        {
            Ok(namespace.clone())
        }
        Some(_) => Err(MemoryToolsetError::Invalid),
    }
}

/// Why the memory toolset could not be built. Carries no setting value.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum MemoryToolsetError {
    /// `namespace` or `selected_tools` is out of shape.
    Invalid,
    /// A bound was exceeded.
    ResourceExhausted,
}

impl From<IndexToolsetError> for MemoryToolsetError {
    fn from(error: IndexToolsetError) -> Self {
        match error.code() {
            IndexToolsetErrorCode::InvalidConfiguration => Self::Invalid,
            IndexToolsetErrorCode::ResourceExhausted => Self::ResourceExhausted,
        }
    }
}

impl From<MaterializedToolsetError> for MemoryToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self::Invalid
    }
}

/// The memory tools of one toolkit: every tool, or the selected ones when
/// `selected_tools` names any (an empty list selects all, as in the SDK).
///
/// # Errors
///
/// A malformed `namespace` or `selected_tools`, or the policy's refusal of
/// an identity.
pub fn build_memory_toolset(
    toolkit_name: &str,
    toolkit_id: Option<u64>,
    settings: &Map<String, Value>,
    policy: &Arc<ToolAdmissionPolicy>,
    authority: &IndexToolAuthority,
) -> Result<BasicToolset, MemoryToolsetError> {
    let namespace: Arc<str> = Arc::from(memory_namespace(settings, toolkit_id)?);
    let selected = selected_tool_names(settings)?;
    let include = |name: &str| selected.is_empty() || selected.iter().any(|value| value == name);
    let tools: Vec<Arc<dyn Tool>> = MemoryToolName::ALL
        .into_iter()
        .filter(|tool| include(tool.as_str()))
        .map(|tool| {
            Arc::new(MemoryTool {
                tool,
                namespace: Arc::clone(&namespace),
                authority: authority.clone(),
            }) as Arc<dyn Tool>
        })
        .collect();
    admit_materialized_toolset(toolkit_name, MEMORY_TOOLKIT_TYPE, policy, tools).map_err(Into::into)
}

/// Every tool before admission, for the schema gate.
#[cfg(any(test, feature = "test-support"))]
#[must_use]
pub fn all_memory_tools(authority: &IndexToolAuthority) -> Vec<Arc<dyn Tool>> {
    MemoryToolName::ALL
        .into_iter()
        .map(|tool| {
            Arc::new(MemoryTool {
                tool,
                namespace: Arc::from("conformance"),
                authority: authority.clone(),
            }) as Arc<dyn Tool>
        })
        .collect()
}

struct MemoryTool {
    tool: MemoryToolName,
    namespace: Arc<str>,
    authority: IndexToolAuthority,
}

#[async_trait]
impl Tool for MemoryTool {
    fn name(&self) -> &str {
        self.tool.as_str()
    }

    fn description(&self) -> &str {
        self.tool.sdk_description()
    }

    fn is_read_only(&self) -> bool {
        self.tool.is_read_only()
    }

    fn is_concurrency_safe(&self) -> bool {
        self.tool.is_read_only()
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(self.tool.parameters_schema())
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        let arguments = if arguments.is_object() {
            arguments
        } else if arguments.is_null() {
            Value::Object(Map::new())
        } else {
            return Ok(Value::String(
                "the tool arguments must be an object".to_owned(),
            ));
        };
        let call = MemoryToolCall {
            tool: self.tool,
            namespace: &self.namespace,
            arguments: &arguments,
        };
        match self.authority.host().memory(call).await {
            Ok(value) => Ok(value),
            Err(failure) => Ok(Value::String(failure.message().to_owned())),
        }
    }
}

/// The sentence a host answers for a tool it cannot run.
#[must_use]
pub fn memory_not_served() -> IndexToolFailure {
    IndexToolFailure::new("memory is not available on this worker")
}
