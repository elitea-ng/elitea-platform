//! The user-visible record of what a turn could NOT serve.
//!
//! Before this module the worker told nobody: an unported toolkit family, a
//! refused toolkit setting or an unserved internal tool was one `warn!` line
//! plus, on a fresh session, a model-only history event. The person who
//! enabled the capability saw an agent that quietly did less.
//!
//! The record rides the channel the worker already uses to speak to Main: a
//! `skipped_capabilities` array in the `response_metadata` of the turn's
//! `agent_start` and final `full_message` node events. No new RPC, no REST.
//! Main persists the final array on the message group (`meta`), so a reload
//! shows what the live stream showed, and the chat renders one compact notice
//! per turn.
//!
//! Each entry is `{kind, name, [type], reason_code, reason}`. The set is
//! deduplicated, ordered by content (not by the order the version listed its
//! tools), bounded, and carries no configuration value or credential: names
//! and types are the stored labels, `reason` is fixed text.

use serde_json::{Value, json};

use super::application_tools::SkippedApplicationChild;
use super::internal_tools::InternalToolCatalog;
use crate::toolkits::RefusedToolkit;

/// Most entries one turn reports. A version holds at most a few dozen tools;
/// the bound keeps a hostile row from growing the event.
pub(crate) const MAX_SKIPPED_CAPABILITIES: usize = 32;
const MAX_LABEL_CHARS: usize = 96;

pub(crate) const KIND_TOOLKIT: &str = "toolkit";
pub(crate) const KIND_INTERNAL_TOOL: &str = "internal_tool";
pub(crate) const KIND_APPLICATION: &str = "application";

const REASON_INTERNAL_TOOL: &str = "this built-in capability is not available on this worker yet, so it was turned off for this agent";
const REASON_APPLICATION: &str =
    "this attached agent type is not available as a tool on this worker yet, so it was left out";

fn label(value: &str) -> String {
    value
        .chars()
        .filter(|character| !character.is_control())
        .take(MAX_LABEL_CHARS)
        .collect()
}

/// The deduplicated, ordered `skipped_capabilities` array; empty when the turn
/// served everything it was asked for.
pub(crate) fn skipped_capabilities(
    toolkits: &[RefusedToolkit],
    internal_tools: InternalToolCatalog,
    application_children: &[SkippedApplicationChild],
) -> Vec<Value> {
    let mut entries: Vec<(String, String, String, &'static str, &'static str)> = Vec::new();
    for toolkit in toolkits {
        entries.push((
            KIND_TOOLKIT.to_owned(),
            toolkit.toolkit_name().to_owned(),
            toolkit.toolkit_type().to_owned(),
            toolkit.reason_code(),
            toolkit.reason(),
        ));
    }
    for name in internal_tools.skipped_platform_tools() {
        entries.push((
            KIND_INTERNAL_TOOL.to_owned(),
            name.to_owned(),
            String::new(),
            "internal_tool_unsupported",
            REASON_INTERNAL_TOOL,
        ));
    }
    for child in application_children {
        entries.push((
            KIND_APPLICATION.to_owned(),
            label(&child.name),
            label(&child.agent_type),
            "application_child_unsupported",
            REASON_APPLICATION,
        ));
    }
    entries.sort();
    entries.dedup();
    entries.truncate(MAX_SKIPPED_CAPABILITIES);
    entries
        .into_iter()
        .map(|(kind, name, kind_type, reason_code, reason)| {
            let mut entry = json!({
                "kind": kind,
                "name": name,
                "reason_code": reason_code,
                "reason": reason,
            });
            if !kind_type.is_empty() {
                entry["type"] = Value::String(kind_type);
            }
            entry
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn nothing_skipped_reports_nothing() {
        assert!(skipped_capabilities(&[], InternalToolCatalog::empty(), &[]).is_empty());
    }

    #[test]
    fn internal_tools_and_children_are_named_deduplicated_and_ordered() {
        let catalog =
            InternalToolCatalog::from_values(Some(&json!(["swarm", "internal_mcp", "swarm"])))
                .expect("recognized names");
        let children = vec![
            SkippedApplicationChild {
                agent_type: "predict".to_owned(),
                name: "b".to_owned(),
            },
            SkippedApplicationChild {
                agent_type: "predict".to_owned(),
                name: "b".to_owned(),
            },
        ];
        let entries = skipped_capabilities(&[], catalog, &children);
        let names: Vec<_> = entries
            .iter()
            .map(|entry| {
                (
                    entry["kind"].as_str().unwrap().to_owned(),
                    entry["name"].as_str().unwrap().to_owned(),
                )
            })
            .collect();
        assert_eq!(
            names,
            [
                ("application".to_owned(), "b".to_owned()),
                ("internal_tool".to_owned(), "internal_mcp".to_owned()),
                ("internal_tool".to_owned(), "swarm".to_owned()),
            ]
        );
        assert_eq!(entries[0]["type"], "predict");
        assert!(entries[1].get("type").is_none());
        assert!(entries.iter().all(|entry| entry["reason"].is_string()));
    }

    #[test]
    fn the_list_is_bounded() {
        let children: Vec<_> = (0..100)
            .map(|index| SkippedApplicationChild {
                agent_type: "pipeline".to_owned(),
                name: format!("child-{index:03}"),
            })
            .collect();
        assert_eq!(
            skipped_capabilities(&[], InternalToolCatalog::empty(), &children).len(),
            MAX_SKIPPED_CAPABILITIES
        );
    }
}
