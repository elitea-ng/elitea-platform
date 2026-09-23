//! Report provider aliases from the canonical toolkit binding owner.

use std::collections::BTreeMap;

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
                .map(|entry| format!("'{}' for '{}'", entry.exposed, notice_label(&entry.toolkit)))
                .collect::<Vec<_>>()
                .join(", ");
            format!(
                "tool '{}' uses a toolkit-qualified name; call {pairs}",
                notice_label(original)
            )
        })
        .collect::<Vec<_>>();
    Some(lines.join("\n"))
}

fn notice_label(value: &str) -> String {
    value
        .chars()
        .filter(|character| !character.is_control())
        .take(120)
        .collect()
}
