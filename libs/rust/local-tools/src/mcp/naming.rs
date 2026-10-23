//! The names MCP tools are offered under: `mcp__<server>__<tool>`, cut to
//! the characters every provider takes (`[A-Za-z0-9_-]{1,64}`). Two tools
//! that end up with one name (after the cut, or a name the host reserves)
//! get a numeric suffix, and a [`RenamedTool`] says so.

use elitea_agent_runtime::tool_namespacing::RenamedTool;

/// Every MCP tool name starts with it; no other tool's does.
pub const PREFIX: &str = "mcp__";

/// The longest tool name providers take.
pub const MAX_NAME: usize = 64;

fn sanitise(text: &str) -> String {
    text.chars()
        .map(|c| {
            if c.is_ascii_alphanumeric() || c == '_' || c == '-' {
                c
            } else {
                '_'
            }
        })
        .collect()
}

/// The names of `tools` (`(server, tool)`, in the order given), none of
/// them in `reserved` nor equal to another, and the renames that took.
#[must_use]
pub fn assign(tools: &[(String, String)], reserved: &[String]) -> (Vec<String>, Vec<RenamedTool>) {
    let mut taken: Vec<String> = reserved.to_vec();
    let mut names = Vec::with_capacity(tools.len());
    let mut renamed = Vec::new();
    for (server, tool) in tools {
        let mut base = format!("{PREFIX}{}__{}", sanitise(server), sanitise(tool));
        base.truncate(MAX_NAME);
        let mut name = base.clone();
        let mut n = 2;
        while taken.contains(&name) {
            let suffix = format!("_{n}");
            let mut trimmed = base.clone();
            trimmed.truncate(MAX_NAME - suffix.len());
            name = format!("{trimmed}{suffix}");
            n += 1;
        }
        if name != base {
            renamed.push(RenamedTool {
                toolkit: server.clone(),
                original: tool.clone(),
                exposed: name.clone(),
            });
        }
        taken.push(name.clone());
        names.push(name);
    }
    (names, renamed)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn pair(server: &str, tool: &str) -> (String, String) {
        (server.to_owned(), tool.to_owned())
    }

    #[test]
    fn mcp_names_are_sanitised_and_collisions_renamed() {
        let long = "t".repeat(80);
        let (names, renamed) = assign(
            &[
                pair("gh", "search issues"),
                pair("gh", "search.issues"),
                pair("fs", &long),
                pair("fs", "list"),
            ],
            &["mcp__fs__list".to_owned()],
        );
        assert_eq!(names[0], "mcp__gh__search_issues");
        assert_eq!(names[1], "mcp__gh__search_issues_2");
        assert_eq!(names[2].len(), MAX_NAME);
        assert_eq!(names[3], "mcp__fs__list_2");
        for name in &names {
            assert!(name.len() <= MAX_NAME);
            assert!(
                name.chars()
                    .all(|c| c.is_ascii_alphanumeric() || c == '_' || c == '-'),
                "{name}"
            );
        }
        assert_eq!(renamed.len(), 2);
        assert_eq!(renamed[0].original, "search.issues");
        assert_eq!(renamed[0].exposed, "mcp__gh__search_issues_2");
        assert_eq!(renamed[1].toolkit, "fs");
    }
}
