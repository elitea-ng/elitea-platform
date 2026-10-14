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
//! Each entry is `{kind, name, [type], reason_code, reason}`; a `tool` entry
//! names one tool of a built toolkit as `<toolkit>.<tool>`. The set is
//! deduplicated, ordered by content (not by the order the version listed its
//! tools), bounded, and carries no configuration value or credential: names
//! and types are the stored labels, `reason` is fixed text.

use serde_json::{Value, json};

use super::application_tools::SkippedApplicationChild;
use super::internal_tools::InternalToolCatalog;
use crate::toolkits::{LeftOutToolkit, bounded_label};

/// Most entries one turn reports.
pub(crate) const MAX_SKIPPED_CAPABILITIES: usize = 32;
/// Most bytes of serialized entries one turn reports. The list rides two or
/// three size-capped node events (`agent_start`, a pause, the final message),
/// so it is bounded in BYTES, not just in entries: reasons are fixed text but
/// names come from stored rows.
pub(crate) const MAX_SKIPPED_CAPABILITIES_BYTES: usize = 4 * 1024;

pub(crate) const KIND_TOOLKIT: &str = "toolkit";
pub(crate) const KIND_INTERNAL_TOOL: &str = "internal_tool";
pub(crate) const KIND_APPLICATION: &str = "application";
/// One tool of a built toolkit that this worker does not serve. Its name is
/// `<toolkit>.<tool>`, its `type` the toolkit's type.
pub(crate) const KIND_TOOL: &str = "tool";

const REASON_INTERNAL_TOOL: &str = "this built-in capability is not available on this worker yet, so it was turned off for this agent";
const REASON_APPLICATION: &str =
    "this attached agent type is not available as a tool on this worker yet, so it was left out";

/// The `skipped_capabilities` value: `{"items": [...], "more": N}` (`more`
/// only when entries were cut), or `None` when the turn served everything.
///
/// Entries are deduplicated and ordered by content, then taken ROUND-ROBIN BY
/// KIND (toolkit, internal tool, attached agent) until the entry or byte
/// budget is reached, so one kind with many entries cannot push another out of
/// the notice. `more` says how many were not shown.
pub(crate) fn skipped_capabilities(
    toolkits: &[LeftOutToolkit],
    internal_tools: InternalToolCatalog,
    application_children: &[SkippedApplicationChild],
) -> Option<Value> {
    let mut by_kind: [Vec<Value>; 4] = [Vec::new(), Vec::new(), Vec::new(), Vec::new()];
    let entry = |kind: &str, name: &str, kind_type: &str, code: &str, reason: &str| {
        let mut entry = json!({"kind": kind, "name": name, "reason_code": code, "reason": reason});
        if !kind_type.is_empty() {
            entry["type"] = Value::String(kind_type.to_owned());
        }
        entry
    };
    for toolkit in toolkits {
        if let Some(tool) = toolkit.tool_name() {
            by_kind[3].push(entry(
                KIND_TOOL,
                &bounded_label(&format!("{}.{tool}", toolkit.toolkit_name())),
                toolkit.toolkit_type(),
                toolkit.reason_code(),
                toolkit.reason(),
            ));
            continue;
        }
        by_kind[0].push(entry(
            KIND_TOOLKIT,
            toolkit.toolkit_name(),
            toolkit.toolkit_type(),
            toolkit.reason_code(),
            toolkit.reason(),
        ));
    }
    for name in internal_tools.skipped_platform_tools() {
        by_kind[1].push(entry(
            KIND_INTERNAL_TOOL,
            name,
            "",
            "internal_tool_unsupported",
            REASON_INTERNAL_TOOL,
        ));
    }
    for child in application_children {
        by_kind[2].push(entry(
            KIND_APPLICATION,
            &bounded_label(&child.name),
            &bounded_label(&child.agent_type),
            "application_child_unsupported",
            REASON_APPLICATION,
        ));
    }
    let mut total = 0;
    for list in &mut by_kind {
        list.sort_by_cached_key(Value::to_string);
        list.dedup();
        total += list.len();
    }
    if total == 0 {
        return None;
    }
    let mut items = Vec::new();
    let mut bytes = 0;
    let mut rank = 0;
    'fill: loop {
        let mut took = false;
        for list in &by_kind {
            let Some(entry) = list.get(rank) else {
                continue;
            };
            let size = entry.to_string().len() + 1;
            if items.len() == MAX_SKIPPED_CAPABILITIES
                || bytes + size > MAX_SKIPPED_CAPABILITIES_BYTES
            {
                break 'fill;
            }
            bytes += size;
            items.push(entry.clone());
            took = true;
        }
        if !took {
            break;
        }
        rank += 1;
    }
    let more = total - items.len();
    let mut value = json!({ "items": items });
    if more > 0 {
        value["more"] = json!(more);
    }
    Some(value)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn items(value: &Value) -> &Vec<Value> {
        value["items"].as_array().expect("items")
    }

    #[test]
    fn nothing_skipped_reports_nothing() {
        assert!(skipped_capabilities(&[], InternalToolCatalog::empty(), &[]).is_none());
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
        let value = skipped_capabilities(&[], catalog, &children).expect("a list");
        let entries = items(&value);
        let names: Vec<_> = entries
            .iter()
            .map(|entry| {
                (
                    entry["kind"].as_str().unwrap().to_owned(),
                    entry["name"].as_str().unwrap().to_owned(),
                )
            })
            .collect();
        // Round-robin by kind: the internal tools come first among the kinds
        // present, then the attached agent, then the second internal tool.
        assert_eq!(
            names,
            [
                ("internal_tool".to_owned(), "internal_mcp".to_owned()),
                ("application".to_owned(), "b".to_owned()),
                ("internal_tool".to_owned(), "swarm".to_owned()),
            ]
        );
        assert!(value.get("more").is_none());
        assert_eq!(entries[1]["type"], "predict");
        assert!(entries[0].get("type").is_none());
    }

    #[test]
    fn a_long_list_is_cut_fairly_and_says_how_many_were_not_shown() {
        let children: Vec<_> = (0..100)
            .map(|index| SkippedApplicationChild {
                agent_type: "pipeline".to_owned(),
                name: format!("child-{index:03}"),
            })
            .collect();
        let catalog = InternalToolCatalog::from_values(Some(&json!(["swarm"]))).expect("names");
        let value = skipped_capabilities(&[], catalog, &children).expect("a list");
        let entries = items(&value);
        assert!(entries.len() <= MAX_SKIPPED_CAPABILITIES && entries.len() < 101);
        // The one internal tool is not pushed out by the 100 children.
        assert!(
            entries
                .iter()
                .any(|entry| entry["kind"] == KIND_INTERNAL_TOOL)
        );
        assert_eq!(value["more"], 101 - entries.len());
    }

    #[test]
    fn multi_byte_names_never_exceed_the_byte_budget() {
        let children: Vec<_> = (0..60)
            .map(|index| SkippedApplicationChild {
                agent_type: "pipeline".to_owned(),
                name: format!("{index:02}{}", "ж".repeat(200)),
            })
            .collect();
        let value =
            skipped_capabilities(&[], InternalToolCatalog::empty(), &children).expect("a list");
        let entries = items(&value);
        assert!(value.to_string().len() <= MAX_SKIPPED_CAPABILITIES_BYTES + 64);
        assert!(value["more"].as_u64().expect("more") > 0);
        for entry in entries {
            // Cut on a character boundary, in bytes.
            assert!(
                entry["name"].as_str().expect("name").len()
                    <= crate::toolkits::MAX_NOTICE_LABEL_BYTES
            );
        }
    }

    #[test]
    fn omitted_tools_are_named_per_toolkit_and_do_not_crowd_out_other_kinds() {
        let toolkits: Vec<_> = (0..100)
            .map(|index| {
                LeftOutToolkit::unserved_tool(
                    "repo",
                    "github",
                    &format!("tool_{index:03}"),
                    "this tool is not available on this worker yet, so it was left out",
                )
            })
            .collect();
        let catalog = InternalToolCatalog::from_values(Some(&json!(["swarm"]))).expect("names");
        let value = skipped_capabilities(&toolkits, catalog, &[]).expect("a list");
        let entries = items(&value);
        assert!(entries.len() <= MAX_SKIPPED_CAPABILITIES);
        assert!(value.to_string().len() <= MAX_SKIPPED_CAPABILITIES_BYTES + 64);
        // Round-robin: the internal tool is not pushed out by 100 tools.
        assert!(
            entries
                .iter()
                .any(|entry| entry["kind"] == KIND_INTERNAL_TOOL)
        );
        let tool = entries
            .iter()
            .find(|entry| entry["kind"] == KIND_TOOL)
            .expect("a tool entry");
        assert_eq!(tool["name"], "repo.tool_000");
        assert_eq!(tool["type"], "github");
        assert_eq!(tool["reason_code"], "unsupported_tool_selection");
        assert_eq!(value["more"], 101 - entries.len());
    }
}
