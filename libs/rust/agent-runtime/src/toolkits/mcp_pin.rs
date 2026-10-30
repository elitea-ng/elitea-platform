//! The approved tool pin of a custom MCP server (ADR-0044 decision 1,
//! #1501).
//!
//! Under the platform's `approved_only` connector catalogue, Main freezes an
//! approved custom MCP server's toolkit with `pinned_tools`: the
//! `{name, digest}` of every tool an approved connector request declared. The
//! worker keeps only the tools of the server's live listing whose name AND
//! digest are pinned, so a tool the server later adds, or changes (its
//! description or its arguments), is never offered to the model and so can
//! never be called, until a new request is approved.
//!
//! The digest is the lower-case hex SHA-256 of the canonical JSON
//! ([`crate::canonical`]: keys sorted by bytes, compact, `serde_json`'s string
//! escaping and number text) of
//! `{"description": <description>, "inputSchema": <schema or null>, "name": <name>}`,
//! the same form Main's `connectorapproval.ToolDigest` writes. The two share
//! a test vector. A settings document without the key is not pinned (the
//! open catalogue mode); a present key that does not parse refuses the
//! toolkit.

use std::collections::HashSet;
use std::fmt::Write as _;
use std::sync::Arc;

use adk_core::Tool;
use serde_json::{Map, Value, json};

/// The frozen settings key Main writes.
pub(crate) const PINNED_TOOLS_SETTING: &str = "pinned_tools";

/// The most pins one toolkit carries (Main's bound on declared tools).
const MAX_PINS: usize = 4096;

/// One pinned tool.
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub(crate) struct PinnedTool {
    pub(crate) name: String,
    pub(crate) digest: String,
}

/// The pin of one MCP tool.
#[must_use]
pub(crate) fn tool_digest(name: &str, description: &str, schema: Option<&Value>) -> String {
    let document = json!({
        "description": description,
        "inputSchema": schema.cloned().unwrap_or(Value::Null),
        "name": name,
    });
    let canonical = crate::canonical::to_vec(&document).unwrap_or_default();
    let digest = ring::digest::digest(&ring::digest::SHA256, &canonical);
    let mut hex = String::with_capacity(64);
    for byte in digest.as_ref() {
        let _ = write!(hex, "{byte:02x}");
    }
    hex
}

/// The pins in a settings document: `None` when the key is absent, `Err`
/// when it is present and malformed.
pub(crate) fn parse_pinned_tools(
    settings: &Map<String, Value>,
) -> Result<Option<HashSet<PinnedTool>>, ()> {
    let Some(raw) = settings.get(PINNED_TOOLS_SETTING) else {
        return Ok(None);
    };
    let list = raw.as_array().ok_or(())?;
    if list.len() > MAX_PINS {
        return Err(());
    }
    let mut pins = HashSet::with_capacity(list.len());
    for item in list {
        let entry = item.as_object().ok_or(())?;
        let name = entry.get("name").and_then(Value::as_str).ok_or(())?;
        let digest = entry.get("digest").and_then(Value::as_str).ok_or(())?;
        if name.is_empty()
            || digest.len() != 64
            || !digest
                .bytes()
                .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
        {
            return Err(());
        }
        pins.insert(PinnedTool {
            name: name.to_owned(),
            digest: digest.to_owned(),
        });
    }
    Ok(Some(pins))
}

/// Whether a tool's name and live digest are pinned.
#[must_use]
pub(crate) fn is_pinned(pins: &HashSet<PinnedTool>, tool: &dyn Tool) -> bool {
    let schema = tool.parameters_schema();
    let candidate = PinnedTool {
        name: tool.name().to_owned(),
        digest: tool_digest(tool.name(), tool.description(), schema.as_ref()),
    };
    pins.contains(&candidate)
}

/// Keeps the pinned tools; `None` keeps every tool.
#[must_use]
pub(crate) fn keep_pinned(
    tools: Vec<Arc<dyn Tool>>,
    pins: Option<&HashSet<PinnedTool>>,
) -> Vec<Arc<dyn Tool>> {
    let Some(pins) = pins else {
        return tools;
    };
    tools
        .into_iter()
        .filter(|tool| is_pinned(pins, tool.as_ref()))
        .collect()
}

/// Whether a tool NAME is pinned at all (a guarded tool whose listing could
/// not be read is offered only when some pin names it).
#[must_use]
pub(crate) fn name_is_pinned(pins: Option<&HashSet<PinnedTool>>, name: &str) -> bool {
    pins.is_none_or(|pins| pins.iter().any(|pin| pin.name == name))
}

#[cfg(test)]
mod tests {
    use super::*;
    use adk_core::ToolContext;
    use async_trait::async_trait;

    struct ListedTool {
        name: &'static str,
        description: &'static str,
    }

    #[async_trait]
    impl Tool for ListedTool {
        fn name(&self) -> &str {
            self.name
        }

        fn description(&self) -> &str {
            self.description
        }

        fn parameters_schema(&self) -> Option<Value> {
            Some(json!({"type": "object"}))
        }

        async fn execute(
            &self,
            _context: Arc<dyn ToolContext>,
            _arguments: Value,
        ) -> adk_core::Result<Value> {
            Ok(Value::Null)
        }
    }

    fn listed(name: &'static str, description: &'static str) -> Arc<dyn Tool> {
        Arc::new(ListedTool { name, description })
    }

    /// A tool the server added after the approval, and a tool it changed,
    /// are not offered; with no pins (open mode) everything is.
    #[test]
    fn only_pinned_unchanged_tools_are_kept() {
        let pin = PinnedTool {
            name: "search".to_owned(),
            digest: tool_digest("search", "Search.", Some(&json!({"type": "object"}))),
        };
        let pins = HashSet::from([pin]);
        let kept = keep_pinned(
            vec![
                listed("search", "Search."),
                listed("delete_all", "Delete everything."),
            ],
            Some(&pins),
        );
        assert_eq!(
            kept.iter().map(|tool| tool.name()).collect::<Vec<_>>(),
            ["search"]
        );
        assert!(
            keep_pinned(
                vec![listed("search", "Search. And exfiltrate.")],
                Some(&pins)
            )
            .is_empty()
        );
        assert_eq!(keep_pinned(vec![listed("delete_all", "")], None).len(), 1);
        assert!(name_is_pinned(Some(&pins), "search"));
        assert!(!name_is_pinned(Some(&pins), "delete_all"));
        assert!(name_is_pinned(None, "delete_all"));
    }

    /// The vector Main's `connectorapproval` digest test asserts too
    /// (services/elitea-main/internal/application/connectorapproval/digest_test.go).
    #[test]
    fn the_digest_matches_mains_vector() {
        let schema = json!({
            "type": "object",
            "properties": {"query": {"type": "string", "description": "a \"quoted\"\n<tag> & ünïcode"}},
            "required": ["query"],
            "additionalProperties": false
        });
        assert_eq!(
            tool_digest("search", "Search the docs.\tTabbed.", Some(&schema)),
            "551f3dee2f607af84054d833c4fdd55ffda6091ef0a6a8106da2a0fa018ada3b"
        );
    }

    #[test]
    fn absent_pins_keep_everything_and_malformed_pins_refuse() {
        let mut settings = Map::new();
        assert_eq!(parse_pinned_tools(&settings), Ok(None));
        settings.insert(PINNED_TOOLS_SETTING.into(), json!([{"name": "a"}]));
        assert_eq!(parse_pinned_tools(&settings), Err(()));
        settings.insert(PINNED_TOOLS_SETTING.into(), json!([]));
        assert_eq!(parse_pinned_tools(&settings), Ok(Some(HashSet::new())));
    }
}
