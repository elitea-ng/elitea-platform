//! The `lazy_tools_mode` internal tool, "Smart Tools Selection": instead of
//! binding every toolkit tool to the model (which costs tens of thousands of
//! tokens with many toolkits), bind two meta-tools and a compact index of what
//! exists (Python SDK `elitea_sdk/runtime/tools/lazy_tools.py`).
//!
//! # What the SDK does, and what is the same here
//!
//! * `get_tool_schema(toolkit, tool)` returns one tool's schema (`name`,
//!   `description`, `parameters`), with the SDK's lookup fallbacks and error
//!   bodies.
//! * `invoke_tool(toolkit, tool, arguments)` runs any tool of the registry;
//!   explicit `null` arguments are dropped first, a failed call answers with
//!   the SDK's "ERROR: Invalid arguments ..." text including the expected
//!   parameters and a usage example.
//! * The system prompt gets the SDK's generated index (`## Type: ...` blocks,
//!   toolkit names, tool names) appended after a blank line.
//! * The mode switches itself off below [`MIN_TOOLS`] lazily-bindable tools,
//!   where the meta-tools would cost more than they save.
//!
//! # What is different, on purpose
//!
//! * **Results are values, not pre-rendered text.** The SDK renders each answer
//!   to a string (indented JSON for schemas, compact JSON for collections).
//!   Here a schema or a tool's result is returned as the JSON value itself and
//!   the shared model facade encodes it once, exactly as it does for a directly
//!   bound tool: pre-rendering would reach the model escaped a second time.
//!   The SDK's plain-text answers (not-found, `ERROR: Invalid arguments`)
//!   stay text.
//! * **Authorization is never routed through a meta-tool.** In the SDK each
//!   registry tool carries its own sensitive-action guard. Here confirmation
//!   is a property of the ADK call the model makes, so a sensitive tool or a
//!   tool whose toolkit needs delegated authorization stays bound directly
//!   (callers pass those through `keep_direct`): `invoke_tool` can only reach
//!   tools that run without a confirmation. The same holds for nested
//!   applications and every other internal tool, which are never candidates.
//! * **The registry is the admitted catalogue.** Tools are taken after tool
//!   admission policy, so there is no second blocklist check inside
//!   `invoke_tool` (the SDK's `is_tool_blocked`).
//! * **Toolkit type.** The SDK reads it from tool metadata; the caller supplies
//!   it from the frozen toolkit reference, falling back to the toolkit name.
//! * **Counting.** The threshold counts the lazily-bindable tools only (the SDK
//!   counts everything that is not an application tool).
//! * `select_toolkits_for_query` (keyword pre-selection) is not ported: the
//!   SDK worker never calls it.

use std::collections::BTreeSet;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, Tool, ToolContext};
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::tool_binding::FrozenToolset;

/// The toolset name of the two meta-tools (the SDK's `toolkit_name` metadata).
pub const TOOLSET_NAME: &str = "lazy_tools_mode";
pub const GET_TOOL_SCHEMA: &str = "get_tool_schema";
pub const INVOKE_TOOL: &str = "invoke_tool";

/// Below this many lazily-bindable tools the mode switches itself off
/// (`LAZY_TOOLS_MIN_THRESHOLD` in `langraph_agent.py`).
pub const MIN_TOOLS: usize = 20;

/// Which tools live in which toolkit, by name only.
///
/// It is the one place the model's `(toolkit, tool)` is resolved, and it is
/// public so the worker can attribute an `invoke_tool` call to the toolkit and
/// tool it will actually run (tool-call records and analytics).
#[derive(Clone, Debug, Default)]
pub struct Directory {
    pub toolkits: Vec<(String, Vec<String>)>,
}

impl Directory {
    /// Resolve the model's `(toolkit, tool)` to exactly one `(toolkit, tool)`.
    ///
    /// A toolkit named by the model is NEVER abandoned: an unknown name is
    /// refused (a typo must not run the tool of a different repository). The
    /// only leniency is letter case, taken only when it names one toolkit. With
    /// no toolkit given, a tool name resolves only if it is in exactly one
    /// toolkit. A refusal is the answer the model reads and runs nothing.
    ///
    /// # Errors
    ///
    /// The refusal body for the model.
    pub fn resolve(&self, toolkit: &str, tool: &str) -> Result<(String, String), Value> {
        let names: Vec<&str> = self
            .toolkits
            .iter()
            .map(|(name, _)| name.as_str())
            .collect();
        if toolkit.trim().is_empty() {
            let holders: Vec<&str> = self
                .toolkits
                .iter()
                .filter(|(_, tools)| tools.iter().any(|candidate| candidate == tool))
                .map(|(name, _)| name.as_str())
                .collect();
            return match holders.as_slice() {
                [only] => Ok(((*only).to_owned(), tool.to_owned())),
                [] => Err(json!({
                    "error": format!("Tool '{tool}' not found"),
                    "available_toolkits": names,
                })),
                many => Err(json!({
                    "error": format!("Tool '{tool}' is in more than one toolkit; say which toolkit to use"),
                    "toolkits_with_this_tool": many,
                })),
            };
        }
        let exact = self.toolkits.iter().find(|(name, _)| name == toolkit);
        let known = exact.or_else(|| {
            let mut matches = self
                .toolkits
                .iter()
                .filter(|(name, _)| name.eq_ignore_ascii_case(toolkit));
            match (matches.next(), matches.next()) {
                (Some(only), None) => Some(only),
                _ => None,
            }
        });
        let Some((name, tools)) = known else {
            return Err(json!({
                "error": format!("Toolkit '{toolkit}' not found"),
                "available_toolkits": names,
                "hint": "Use the exact toolkit name from the index; nothing was run.",
            }));
        };
        if tools.iter().any(|candidate| candidate == tool) {
            return Ok((name.clone(), tool.to_owned()));
        }
        Err(json!({
            "error": format!("Tool '{tool}' not found in toolkit '{name}'"),
            "available_tools": tools.iter().take(30).collect::<Vec<_>>(),
            "hint": "Nothing was run; pick one of the tools of this toolkit.",
        }))
    }
}

struct Toolkit {
    name: String,
    kind: String,
    tools: Vec<Arc<dyn Tool>>,
}

/// The lazily-bindable tools, grouped by toolkit in the order they arrived.
pub struct Registry {
    toolkits: Vec<Toolkit>,
}

impl Registry {
    fn toolkit(&self, name: &str) -> Option<&Toolkit> {
        self.toolkits.iter().find(|toolkit| toolkit.name == name)
    }

    fn tool(&self, toolkit: &str, tool: &str) -> Option<&Arc<dyn Tool>> {
        self.toolkit(toolkit)?
            .tools
            .iter()
            .find(|candidate| candidate.name() == tool)
    }

    fn directory(&self) -> Directory {
        Directory {
            toolkits: self
                .toolkits
                .iter()
                .map(|toolkit| {
                    (
                        toolkit.name.clone(),
                        toolkit
                            .tools
                            .iter()
                            .map(|tool| tool.name().to_owned())
                            .collect(),
                    )
                })
                .collect(),
        }
    }

    fn resolve(&self, toolkit: &str, tool: &str) -> Result<(String, &Arc<dyn Tool>), Value> {
        let (actual, name) = self.directory().resolve(toolkit, tool)?;
        let found = self.tool(&actual, &name).ok_or_else(
            || json!({"error": format!("Tool '{tool}' not found"), "hint": "Nothing was run."}),
        )?;
        Ok((actual, found))
    }

    fn tool_count(&self) -> usize {
        self.toolkits
            .iter()
            .map(|toolkit| toolkit.tools.len())
            .sum()
    }

    fn schema(toolkit_tool: &Arc<dyn Tool>) -> Value {
        json!({
            "name": toolkit_tool.name(),
            "description": toolkit_tool.description(),
            "parameters": toolkit_tool.parameters_schema().unwrap_or_else(|| json!({})),
        })
    }

    /// The SDK's `generate_index`, line for line.
    #[must_use]
    pub fn index(&self) -> String {
        // Group toolkits by type, in the order the types first appeared.
        let mut types: Vec<(&str, Vec<&Toolkit>)> = Vec::new();
        for toolkit in &self.toolkits {
            if let Some((_, group)) = types.iter_mut().find(|(kind, _)| *kind == toolkit.kind) {
                group.push(toolkit);
            } else {
                types.push((toolkit.kind.as_str(), vec![toolkit]));
            }
        }
        let (first_toolkit, first_tool) = types
            .first()
            .and_then(|(_, group)| group.first())
            .map_or(("docs", "read_file"), |toolkit| {
                (
                    toolkit.name.as_str(),
                    toolkit
                        .tools
                        .first()
                        .map_or("read_file", |tool| tool.name()),
                )
            });

        let mut lines: Vec<String> = vec![
            "# Available Tools".to_owned(),
            String::new(),
            "You have 2 meta-tools to access all functionality:".to_owned(),
            "- get_tool_schema(toolkit, tool) - Get parameter schema for a tool BEFORE calling it"
                .to_owned(),
            "- invoke_tool(toolkit, tool, arguments) - Execute a tool".to_owned(),
            String::new(),
            "IMPORTANT: Each toolkit name (e.g., 'sdk', 'docs', 'core') targets a DIFFERENT repository."
                .to_owned(),
            "Use the EXACT toolkit name to work with that specific repo. The toolkit name IS the repo selector."
                .to_owned(),
            String::new(),
            "SCHEMA EFFICIENCY: Toolkits of the same type share identical schemas.".to_owned(),
            "Get schema once per tool (e.g., get_tool_schema('sdk', 'search_code')), then reuse that knowledge"
                .to_owned(),
            "for other toolkits of same type - just change the toolkit name in invoke_tool."
                .to_owned(),
            String::new(),
            format!(
                "Example: invoke_tool(toolkit=\"{first_toolkit}\", tool=\"{first_tool}\", arguments={{...}})"
            ),
            String::new(),
        ];

        let mut sorted_types: Vec<&(&str, Vec<&Toolkit>)> = types.iter().collect();
        sorted_types.sort_by(|left, right| left.0.cmp(right.0));
        for (kind, group) in sorted_types {
            let mut toolkits: Vec<&&Toolkit> = group.iter().collect();
            toolkits.sort_by(|left, right| left.name.cmp(&right.name));
            // The union of the tools of every toolkit of this type: same-type
            // toolkits may carry different tools (the SDK's MCP case).
            let tool_names: BTreeSet<&str> = toolkits
                .iter()
                .flat_map(|toolkit| toolkit.tools.iter().map(|tool| tool.name()))
                .collect();
            lines.push(format!("## Type: {kind}"));
            if let [only] = toolkits.as_slice() {
                lines.push(format!("  Toolkit name: \"{}\"", only.name));
                lines.push(format!("  Description: {}", describe(&only.name)));
            } else {
                lines.push("  Toolkit names (use one of these exactly):".to_owned());
                for toolkit in &toolkits {
                    lines.push(format!(
                        "    - \"{}\": {}",
                        toolkit.name,
                        describe(&toolkit.name)
                    ));
                }
            }
            lines.push(format!(
                "  Tools ({}): {}",
                tool_names.len(),
                tool_names.iter().copied().collect::<Vec<_>>().join(", ")
            ));
            lines.push(String::new());
        }
        lines.push(format!(
            "Total: {} toolkits, {} tools",
            self.toolkits.len(),
            self.tool_count()
        ));
        lines.join("\n")
    }
}

/// The SDK's toolkit descriptions, keyed by toolkit NAME as `_extract_
/// toolkit_description` does; every other toolkit reads "<name> toolkit".
fn describe(toolkit_name: &str) -> String {
    match toolkit_name {
        "github" => "GitHub repository management - issues, PRs, files, branches".to_owned(),
        "jira" => "Jira project tracking - issues, sprints, boards, transitions".to_owned(),
        "confluence" => "Confluence documentation - pages, spaces, search".to_owned(),
        "slack" => "Slack messaging - channels, messages, threads".to_owned(),
        "artifact" => "File storage and retrieval - upload, download, search".to_owned(),
        "memory" => "Conversation memory and context management".to_owned(),
        "planning" => "Task planning and tracking".to_owned(),
        "default" => "General purpose tools".to_owned(),
        other => format!("{other} toolkit"),
    }
}

/// The toolsets to bind once the mode is on, and the index for the prompt.
pub struct Partition {
    pub toolsets: Vec<FrozenToolset>,
    /// `None` when the mode switched itself off: bind `toolsets` as they are
    /// and add nothing to the prompt.
    pub index: Option<String>,
    /// What `invoke_tool` can reach, for attributing its calls. Empty when
    /// the mode is off.
    pub directory: Directory,
}

/// Move the lazily-bindable tools behind the meta-tools.
///
/// * `candidate(toolset)` — whether a toolset is a configured toolkit (or MCP
///   connection); everything else is bound directly.
/// * `keep_direct(toolset, tool)` — a tool that must stay bound directly
///   (it needs a confirmation or delegated authorization).
/// * `kind_of(toolset)` — the toolkit type, when known.
#[must_use]
pub fn partition(
    frozen: Vec<FrozenToolset>,
    candidate: impl Fn(&str) -> bool,
    keep_direct: impl Fn(&str, &str) -> bool,
    kind_of: impl Fn(&str) -> Option<String>,
) -> Partition {
    let mut toolkits: Vec<Toolkit> = Vec::new();
    let mut direct: Vec<FrozenToolset> = Vec::new();
    for toolset in &frozen {
        let names: BTreeSet<&str> = toolset.tools().iter().map(|tool| tool.name()).collect();
        // A toolset that repeats a tool name is left to the binder, which
        // refuses it exactly as it would without the mode.
        let lazy_here = candidate(toolset.name()) && names.len() == toolset.tools().len();
        let (lazy, kept): (Vec<_>, Vec<_>) = toolset
            .tools()
            .iter()
            .cloned()
            .partition(|tool| lazy_here && !keep_direct(toolset.name(), tool.name()));
        if !lazy.is_empty() {
            toolkits.push(Toolkit {
                name: toolset.name().to_owned(),
                kind: kind_of(toolset.name()).unwrap_or_else(|| toolset.name().to_owned()),
                tools: lazy,
            });
        }
        if !kept.is_empty() {
            direct.push(FrozenToolset::new(toolset.name(), kept));
        }
    }
    let registry = Registry { toolkits };
    let reserved_name_taken = direct
        .iter()
        .flat_map(|toolset| toolset.tools().iter())
        .any(|tool| matches!(tool.name(), GET_TOOL_SCHEMA | INVOKE_TOOL))
        || direct.iter().any(|toolset| toolset.name() == TOOLSET_NAME);
    if registry.tool_count() < MIN_TOOLS || reserved_name_taken {
        tracing::info!(
            event = "lazy_tools_auto_disabled",
            tools = registry.tool_count(),
            threshold = MIN_TOOLS,
            "smart tools selection is off for this turn: too few tools to save tokens"
        );
        return Partition {
            toolsets: frozen,
            index: None,
            directory: Directory::default(),
        };
    }
    let index = registry.index();
    let directory = registry.directory();
    let registry = Arc::new(registry);
    let meta: Vec<Arc<dyn Tool>> = vec![
        Arc::new(GetToolSchema {
            registry: Arc::clone(&registry),
        }),
        Arc::new(InvokeTool { registry }),
    ];
    direct.push(FrozenToolset::new(TOOLSET_NAME, meta));
    Partition {
        toolsets: direct,
        index: Some(index),
        directory,
    }
}

struct GetToolSchema {
    registry: Arc<Registry>,
}

#[async_trait]
impl Tool for GetToolSchema {
    fn name(&self) -> &str {
        GET_TOOL_SCHEMA
    }

    fn description(&self) -> &'static str {
        "Get detailed schema for a single specific tool, including its description and parameter definitions. Use this when you know which tool you need, instead of get_toolkit_tools which returns all tools in a toolkit. IMPORTANT: Tool schemas are universal - they are the same for ALL targets/repos/instances. Once you retrieve a schema (e.g., for 'create_issue'), you do NOT need to call this again for different repositories. The schema shows what parameters are available; repo/target is just one parameter."
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(json!({
            "type": "object",
            "properties": {
                "toolkit": {"type": "string", "description": "Name of the toolkit containing the tool"},
                "tool": {"type": "string", "description": "Name of the specific tool to get schema for"}
            },
            "required": ["toolkit", "tool"]
        }))
    }

    fn response_schema(&self) -> Option<Value> {
        Some(json!({"type": "string"}))
    }

    fn is_read_only(&self) -> bool {
        true
    }

    fn is_concurrency_safe(&self) -> bool {
        true
    }

    async fn execute(&self, _ctx: Arc<dyn ToolContext>, args: Value) -> adk_core::Result<Value> {
        let toolkit = args.get("toolkit").and_then(Value::as_str).unwrap_or("");
        let tool = args.get("tool").and_then(Value::as_str).unwrap_or("");
        Ok(self.lookup(toolkit, tool))
    }
}

impl GetToolSchema {
    fn lookup(&self, toolkit: &str, tool: &str) -> Value {
        match self.registry.resolve(toolkit, tool) {
            Ok((actual, found)) => {
                let mut schema = Registry::schema(found);
                if let Some(object) = schema.as_object_mut() {
                    object.insert("toolkit".to_owned(), Value::String(actual.clone()));
                    if !toolkit.is_empty() && actual != toolkit {
                        object.insert(
                            "note".to_owned(),
                            Value::String(format!("Toolkit '{toolkit}' resolved to '{actual}'")),
                        );
                    }
                }
                schema
            }
            Err(refusal) => refusal,
        }
    }
}

struct InvokeTool {
    registry: Arc<Registry>,
}

#[async_trait]
impl Tool for InvokeTool {
    fn name(&self) -> &str {
        INVOKE_TOOL
    }

    fn description(&self) -> &'static str {
        "Invoke a specific tool from a toolkit with the required arguments. IMPORTANT: You MUST call get_tool_schema(toolkit, tool) first to learn the required parameters before calling invoke_tool. Arguments must be provided as a JSON object matching the tool's schema."
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(json!({
            "type": "object",
            "properties": {
                "toolkit": {"type": "string", "description": "Name of the toolkit containing the tool"},
                "tool": {"type": "string", "description": "Name of the tool to invoke"},
                "arguments": {
                    "type": "object",
                    "description": "Arguments to pass to the tool as a JSON object"
                }
            },
            "required": ["toolkit", "tool"]
        }))
    }

    fn response_schema(&self) -> Option<Value> {
        Some(json!({"type": "string"}))
    }

    // The inner tool decides what is safe; the meta-tool must not widen it.
    fn is_read_only(&self) -> bool {
        false
    }

    fn is_concurrency_safe(&self) -> bool {
        false
    }

    async fn execute(&self, ctx: Arc<dyn ToolContext>, args: Value) -> adk_core::Result<Value> {
        let toolkit = args.get("toolkit").and_then(Value::as_str).unwrap_or("");
        let tool_name = args.get("tool").and_then(Value::as_str).unwrap_or("");
        // Models often send explicit nulls for optional parameters (the
        // schemas show `"default": null`); drop them rather than loop on the
        // failures they cause.
        let arguments: Map<String, Value> = args
            .get("arguments")
            .and_then(Value::as_object)
            .map(|object| {
                object
                    .iter()
                    .filter(|(_, value)| !value.is_null())
                    .map(|(key, value)| (key.clone(), value.clone()))
                    .collect()
            })
            .unwrap_or_default();

        let (actual, tool) = match self.registry.resolve(toolkit, tool_name) {
            Ok(found) => found,
            Err(refusal) => return Ok(refusal),
        };

        match tool.execute(ctx, Value::Object(arguments.clone())).await {
            Ok(result) => Ok(json!({
                "toolkit": &actual,
                "tool": tool_name,
                "result": if result.is_null() {
                    Value::String("Tool executed successfully (no output)".to_owned())
                } else {
                    result
                },
            })),
            // Only a rejected argument set is the SDK's "Invalid arguments"
            // text. Every other failure (authorization, admission, an
            // unavailable or slow dependency, the tool's own error) reaches
            // the loop with its own category and text, as for a bound tool.
            Err(error) if error.category == ErrorCategory::InvalidInput => Ok(Value::String(
                self.invocation_error(toolkit, &actual, tool_name, &arguments, &error),
            )),
            Err(error) => Err(error),
        }
    }
}

impl InvokeTool {
    /// The SDK's `_format_invocation_error`.
    fn invocation_error(
        &self,
        requested: &str,
        actual_toolkit: &str,
        tool_name: &str,
        provided: &Map<String, Value>,
        error: &AdkError,
    ) -> String {
        let found = self.registry.tool(actual_toolkit, tool_name);
        let mut lines = vec![format!(
            "ERROR: Invalid arguments for {actual_toolkit}.{tool_name}"
        )];
        if !requested.is_empty() && actual_toolkit != requested {
            lines.push(format!(
                "(Note: toolkit '{requested}' resolved to '{actual_toolkit}')"
            ));
        }
        lines.push(String::new());
        lines.push(format!(
            "You provided: {}",
            py_dumps(&Value::Object(provided.clone()))
        ));
        lines.push(String::new());
        let Some(tool) = found else {
            lines.push(format!("Original error: {}", error.message));
            lines.push(format!(
                "(Could not retrieve schema for {actual_toolkit}.{tool_name})"
            ));
            return lines.join("\n");
        };
        let schema = tool.parameters_schema().unwrap_or_else(|| json!({}));
        let properties = schema.get("properties").and_then(Value::as_object);
        let required: Vec<&str> = schema
            .get("required")
            .and_then(Value::as_array)
            .map(|names| names.iter().filter_map(Value::as_str).collect())
            .unwrap_or_default();
        let Some(properties) = properties.filter(|properties| !properties.is_empty()) else {
            lines.push(format!("Original error: {}", error.message));
            return lines.join("\n");
        };
        if !required.is_empty() {
            lines.push("REQUIRED parameters:".to_owned());
            for name in &required {
                lines.push(parameter_line(name, properties.get(*name)));
            }
            lines.push(String::new());
        }
        let optional: Vec<&String> = properties
            .keys()
            .filter(|name| !required.contains(&name.as_str()))
            .collect();
        if !optional.is_empty() {
            lines.push("OPTIONAL parameters:".to_owned());
            for name in optional.iter().take(10) {
                lines.push(parameter_line(name, properties.get(name.as_str())));
            }
            if optional.len() > 10 {
                lines.push(format!("  ... and {} more", optional.len() - 10));
            }
            lines.push(String::new());
        }
        let mut example = Map::new();
        for name in &required {
            let kind = properties
                .get(*name)
                .and_then(|property| property.get("type"))
                .and_then(Value::as_str)
                .unwrap_or("string");
            example.insert(
                (*name).to_owned(),
                match kind {
                    "integer" => json!(123),
                    "boolean" => json!(true),
                    "array" => json!([]),
                    "object" => json!({}),
                    _ => Value::String(format!("<{name}>")),
                },
            );
        }
        lines.push("CORRECT usage:".to_owned());
        lines.push(format!(
            "invoke_tool(toolkit=\"{actual_toolkit}\", tool=\"{tool_name}\", arguments={})",
            py_dumps(&Value::Object(example))
        ));
        lines.join("\n")
    }
}

fn parameter_line(name: &str, property: Option<&Value>) -> String {
    let kind = property
        .and_then(|property| property.get("type"))
        .and_then(Value::as_str)
        .unwrap_or("any");
    match property
        .and_then(|property| property.get("description"))
        .and_then(Value::as_str)
        .filter(|description| !description.is_empty())
    {
        Some(description) => format!("  - {name} ({kind}): {description}"),
        None => format!("  - {name} ({kind})"),
    }
}

/// Python's `json.dumps` default layout (`", "` and `": "` separators), which
/// the SDK's error text embeds.
fn py_dumps(value: &Value) -> String {
    match value {
        Value::Array(items) => format!(
            "[{}]",
            items.iter().map(py_dumps).collect::<Vec<_>>().join(", ")
        ),
        Value::Object(object) => format!(
            "{{{}}}",
            object
                .iter()
                .map(|(key, value)| format!(
                    "{}: {}",
                    serde_json::to_string(key).unwrap_or_default(),
                    py_dumps(value)
                ))
                .collect::<Vec<_>>()
                .join(", ")
        ),
        other => serde_json::to_string(other).unwrap_or_default(),
    }
}

#[cfg(test)]
#[path = "lazy_tools_tests.rs"]
mod tests;
