//! Tool categories (ADR-0041 decision 5, #1498).
//!
//! Every tool is in one category: `read`, `write`, `delete`,
//! `external_send` (an email, a chat post, a ticket comment that people
//! outside see), `payment` or `code_execution`. The approval policy
//! ([`crate::approval_policy`]) names, per risk tier, the categories whose
//! tools need a human approval.
//!
//! The categories of the built-in tools come from one contract,
//! `libs/proto/contracts/tool-categories-v1.json`, compiled in here and read
//! by elitea-main for the pipeline hitl rule. An MCP server's tools take the
//! category the administrator gave each pinned tool when they approved the
//! server (`pinned_tools[].category`, ADR-0044 decision 1). A tool that is
//! not declared is `write`: safe, but it asks more often than a declared
//! `read` tool would under a policy that names `write`.

use std::collections::HashMap;
use std::fmt;
use std::sync::LazyLock;

use serde_json::{Map, Value};

/// The category contract every built-in tool is declared in.
const CONTRACT: &str = include_str!("../../../proto/contracts/tool-categories-v1.json");

/// The contract's identity; a different one is not read.
const CONTRACT_NAME: &str = "elitea.tool-categories.v1";

/// The setting an approved MCP server's pins are in (see
/// `toolkits::mcp_pin`).
const PINNED_TOOLS_SETTING: &str = "pinned_tools";

/// The category of one tool.
#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub enum ToolCategory {
    /// Reads data and changes nothing.
    Read,
    /// Creates or changes data. A tool that is not declared is `write`.
    Write,
    /// Deletes data.
    Delete,
    /// Sends something people outside the run see: an email, a chat post, a
    /// visible ticket or pull request comment.
    ExternalSend,
    /// Moves money.
    Payment,
    /// Runs code or a query the model wrote.
    CodeExecution,
}

impl ToolCategory {
    /// Every category, in the contract's order.
    pub const ALL: [Self; 6] = [
        Self::Read,
        Self::Write,
        Self::Delete,
        Self::ExternalSend,
        Self::Payment,
        Self::CodeExecution,
    ];

    /// The category of a tool that is not declared.
    pub const DEFAULT: Self = Self::Write;

    /// The wire name.
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Read => "read",
            Self::Write => "write",
            Self::Delete => "delete",
            Self::ExternalSend => "external_send",
            Self::Payment => "payment",
            Self::CodeExecution => "code_execution",
        }
    }

    /// The category a wire name names, exactly.
    #[must_use]
    pub fn parse(value: &str) -> Option<Self> {
        Self::ALL
            .into_iter()
            .find(|category| category.as_str() == value)
    }
}

impl fmt::Display for ToolCategory {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.as_str())
    }
}

/// Toolkit type key → tool key → category.
type Declarations = HashMap<String, HashMap<String, ToolCategory>>;

static DECLARED: LazyLock<Declarations> =
    LazyLock::new(|| parse_contract(CONTRACT).unwrap_or_default());

/// A stored toolkit type the contract lists under another name.
const STORED_TYPES: [(&str, &str); 1] = [("k8s", "kubernetes")];

/// Read one contract. `None` for a malformed one (a test holds the
/// compiled-in contract well formed; were it not, every tool would be
/// `write`).
fn parse_contract(raw: &str) -> Option<Declarations> {
    let contract: Value = serde_json::from_str(raw).ok()?;
    if contract.get("contract").and_then(Value::as_str) != Some(CONTRACT_NAME)
        || contract.get("default_category").and_then(Value::as_str)
            != Some(ToolCategory::DEFAULT.as_str())
    {
        return None;
    }
    let mut declared = Declarations::new();
    for (toolkit, tools) in contract.get("toolkits")?.as_object()? {
        let toolkit_key = canonical_key(toolkit)?;
        let mut entries = HashMap::new();
        for (tool, category) in tools.as_object()? {
            let category = ToolCategory::parse(category.as_str()?)?;
            if entries.insert(canonical_key(tool)?, category).is_some() {
                return None;
            }
        }
        if declared.insert(toolkit_key, entries).is_some() {
            return None;
        }
    }
    Some(declared)
}

/// The guardrails match key: lower case, ASCII letters and digits only. The
/// sensitive tool rules compare toolkit types and tool names on it.
fn canonical_key(value: &str) -> Option<String> {
    let key = value
        .trim()
        .chars()
        .flat_map(char::to_lowercase)
        .filter(char::is_ascii_alphanumeric)
        .collect::<String>();
    (!key.is_empty()).then_some(key)
}

/// The declared category of a built-in tool, if the contract lists it. A
/// tool name with a routing prefix (`toolkit___tool`, `server:tool`) is
/// matched as the sensitive tool rules match it: the whole name first, then
/// the name with each prefix removed.
#[must_use]
pub fn declared_category(toolkit_type: &str, tool_name: &str) -> Option<ToolCategory> {
    let mut key = canonical_key(toolkit_type)?;
    if let Some((_, listed)) = STORED_TYPES.iter().find(|(stored, _)| *stored == key) {
        (*listed).clone_into(&mut key);
    }
    let tools = DECLARED.get(&key)?;
    let mut current = tool_name.trim().to_lowercase();
    while !current.is_empty() {
        if let Some(category) = canonical_key(&current).and_then(|tool| tools.get(&tool)) {
            return Some(*category);
        }
        let mut reduced = current.as_str();
        if let Some((_, suffix)) = reduced.split_once("___") {
            reduced = suffix.trim();
        }
        if let Some((_, suffix)) = reduced.split_once(':') {
            reduced = suffix.trim();
        }
        if reduced.is_empty() || reduced == current {
            break;
        }
        current = reduced.to_owned();
    }
    None
}

/// The category an approved MCP server's pin gives the tool, if the
/// toolkit's settings pin it with one.
#[must_use]
pub fn pinned_category(
    settings: Option<&Map<String, Value>>,
    tool_name: &str,
) -> Option<ToolCategory> {
    settings?
        .get(PINNED_TOOLS_SETTING)?
        .as_array()?
        .iter()
        .filter_map(Value::as_object)
        .find(|pin| pin.get("name").and_then(Value::as_str) == Some(tool_name))?
        .get("category")
        .and_then(Value::as_str)
        .and_then(ToolCategory::parse)
}

/// The category of one tool: the contract's declaration first, then an
/// approved MCP server's pin, else [`ToolCategory::DEFAULT`].
///
/// The contract wins over the toolkit's settings: a setting cannot move a
/// declared tool to a category the policy does not name.
#[must_use]
pub fn tool_category(
    toolkit_type: &str,
    settings: Option<&Map<String, Value>>,
    tool_name: &str,
) -> ToolCategory {
    declared_category(toolkit_type, tool_name)
        .or_else(|| pinned_category(settings, tool_name))
        .unwrap_or(ToolCategory::DEFAULT)
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use super::*;

    #[test]
    fn the_compiled_in_contract_is_well_formed() {
        let declared = parse_contract(CONTRACT).expect("the contract parses");
        assert!(declared.len() > 40, "toolkits = {}", declared.len());
        let all = declared
            .values()
            .flat_map(HashMap::values)
            .collect::<Vec<_>>();
        for category in [
            ToolCategory::Read,
            ToolCategory::Delete,
            ToolCategory::ExternalSend,
            ToolCategory::CodeExecution,
        ] {
            assert!(all.contains(&&category), "no tool is {category}");
        }
    }

    #[test]
    fn a_malformed_contract_is_not_read() {
        for raw in [
            "{",
            r#"{"contract":"other","default_category":"write","toolkits":{}}"#,
            r#"{"contract":"elitea.tool-categories.v1","default_category":"read","toolkits":{}}"#,
            r#"{"contract":"elitea.tool-categories.v1","default_category":"write","toolkits":{"x":{"a":"admin"}}}"#,
            r#"{"contract":"elitea.tool-categories.v1","default_category":"write","toolkits":{"x":{"a":"read","A":"write"}}}"#,
        ] {
            assert!(parse_contract(raw).is_none(), "{raw}");
        }
    }

    #[test]
    fn declared_tools_have_their_category_and_others_are_write() {
        for (toolkit, tool, want) in [
            ("github", "get_issue", ToolCategory::Read),
            ("github", "comment_on_issue", ToolCategory::ExternalSend),
            ("slack", "send_message", ToolCategory::ExternalSend),
            ("Slack", "slack___send_message", ToolCategory::ExternalSend),
            ("jira", "add_comments", ToolCategory::ExternalSend),
            ("sql", "execute_sql", ToolCategory::CodeExecution),
            (
                "k8s",
                "kubernetes_integration_healthcheck",
                ToolCategory::Read,
            ),
            ("github", "create_issue", ToolCategory::Write),
            ("unknown_kit", "anything", ToolCategory::Write),
        ] {
            assert_eq!(tool_category(toolkit, None, tool), want, "{toolkit}.{tool}");
        }
        assert!(declared_category("github", "create_issue").is_none());
        assert_eq!(ToolCategory::DEFAULT, ToolCategory::Write);
    }

    #[test]
    fn an_mcp_pin_gives_the_category_and_a_setting_cannot_move_a_declared_tool() {
        let settings = json!({"pinned_tools": [
            {"name": "charge", "digest": "0".repeat(64), "category": "payment"},
            {"name": "lookup", "digest": "1".repeat(64)},
            {"name": "send_message", "digest": "2".repeat(64), "category": "read"},
        ]});
        let settings = settings.as_object();
        assert_eq!(
            tool_category("mcp", settings, "charge"),
            ToolCategory::Payment
        );
        assert_eq!(
            tool_category("mcp", settings, "lookup"),
            ToolCategory::Write
        );
        assert_eq!(
            tool_category("mcp", settings, "missing"),
            ToolCategory::Write
        );
        // A declared tool keeps the contract's category whatever a pin says.
        assert_eq!(
            tool_category("slack", settings, "send_message"),
            ToolCategory::ExternalSend
        );
    }

    #[test]
    fn category_names_round_trip() {
        for category in ToolCategory::ALL {
            assert_eq!(ToolCategory::parse(category.as_str()), Some(category));
        }
        assert_eq!(ToolCategory::parse("Delete"), None);
        assert_eq!(ToolCategory::parse(""), None);
    }
}
