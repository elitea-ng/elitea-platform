//! The prompt texts of `ask`, deep research and `resolve_wiki`, and the
//! tool definitions the model is offered (ADR-0026 decision 8).
//!
//! Every text is the Python value, byte for byte, in a data file next to
//! this module, compiled in with `include_str!`. `PROMPTS_MANIFEST.json`
//! names, per file, its source and the SHA-256 of the value:
//!
//! * the engine's own prompts (`ask_prompts.py`, `research_prompts.py`,
//!   `wiki_query.py`): `tests/ask_prompts.rs` re-derives each hash from the
//!   Python source with `python3`;
//! * the third-party texts the Python agents sent (`LangChain`'s todo list
//!   and summary prompts, the `deepagents` summary prompt), pinned by
//!   package version: the parity golden holds them to the recorded request
//!   bodies.
//!
//! `TOOLS.json` holds the tool definitions (name, description, JSON
//! Schema) as `LangChain` built them from the Python docstrings and
//! `deepagents`' schemas; the golden compares them with the recorded
//! requests.
//!
//! The system prompts take only the date and a step budget: repository
//! text never reaches a system message.

use crate::structure::prompts::{FormatError, format};

/// `ASK_WORKFLOW_INSTRUCTIONS` (`{date}`, `{max_iterations}`).
pub const ASK_WORKFLOW: &str = include_str!("ask_workflow.txt");
/// `ASK_TOOL_INSTRUCTIONS`.
pub const ASK_TOOLS: &str = include_str!("ask_tools.txt");
/// `ASK_OUTPUT_INSTRUCTIONS`.
pub const ASK_OUTPUT: &str = include_str!("ask_output.txt");
/// `RESEARCH_WORKFLOW_INSTRUCTIONS` (`{date}`).
pub const RESEARCH_WORKFLOW: &str = include_str!("research_workflow.txt");
/// `TOOL_USAGE_INSTRUCTIONS`.
pub const RESEARCH_TOOLS: &str = include_str!("research_tools.txt");
/// `STOPPING_CRITERIA`.
pub const RESEARCH_STOPPING: &str = include_str!("research_stopping.txt");
/// `OUTPUT_FORMAT_INSTRUCTIONS`.
pub const RESEARCH_OUTPUT: &str = include_str!("research_output.txt");
/// `wiki_query.RESOLUTION_PROMPT` (`{question}`, `{wiki_list_text}`).
pub const RESOLUTION: &str = include_str!("resolution.txt");
/// `langchain.agents.middleware.todo.WRITE_TODOS_SYSTEM_PROMPT`.
pub const TODO_SYSTEM: &str = include_str!("todo_system.txt");
/// `langchain.agents.middleware.summarization.DEFAULT_SUMMARY_PROMPT`.
pub const SUMMARY: &str = include_str!("summary_prompt.txt");
/// `deepagents.middleware.summarization.DEEPAGENTS_DEFAULT_SUMMARY_PROMPT`.
pub const SUMMARY_DEEPAGENTS: &str = include_str!("summary_prompt_deepagents.txt");
/// The tool definitions.
pub const TOOLS: &str = include_str!("TOOLS.json");
/// `PROMPTS_MANIFEST.json`.
pub const MANIFEST: &str = include_str!("PROMPTS_MANIFEST.json");

/// `RESEARCH_TYPE_PROMPTS`, in the Python dict's order.
pub const RESEARCH_TYPES: &[(&str, &str)] = &[
    ("general", include_str!("research_type_general.txt")),
    (
        "architecture",
        include_str!("research_type_architecture.txt"),
    ),
    (
        "implementation",
        include_str!("research_type_implementation.txt"),
    ),
    ("security", include_str!("research_type_security.txt")),
    ("integration", include_str!("research_type_integration.txt")),
    ("debugging", include_str!("research_type_debugging.txt")),
    ("feature", include_str!("research_type_feature.txt")),
];

/// Every embedded prompt by data file name, for the manifest test.
pub const ALL: &[(&str, &str)] = &[
    ("ask_workflow.txt", ASK_WORKFLOW),
    ("ask_tools.txt", ASK_TOOLS),
    ("ask_output.txt", ASK_OUTPUT),
    ("research_workflow.txt", RESEARCH_WORKFLOW),
    ("research_tools.txt", RESEARCH_TOOLS),
    ("research_stopping.txt", RESEARCH_STOPPING),
    ("research_output.txt", RESEARCH_OUTPUT),
    ("research_type_general.txt", RESEARCH_TYPES[0].1),
    ("research_type_architecture.txt", RESEARCH_TYPES[1].1),
    ("research_type_implementation.txt", RESEARCH_TYPES[2].1),
    ("research_type_security.txt", RESEARCH_TYPES[3].1),
    ("research_type_integration.txt", RESEARCH_TYPES[4].1),
    ("research_type_debugging.txt", RESEARCH_TYPES[5].1),
    ("research_type_feature.txt", RESEARCH_TYPES[6].1),
    ("resolution.txt", RESOLUTION),
    ("todo_system.txt", TODO_SYSTEM),
    ("summary_prompt.txt", SUMMARY),
    ("summary_prompt_deepagents.txt", SUMMARY_DEEPAGENTS),
];

/// `get_ask_instructions(max_iterations)` for `date` (`%Y-%m-%d`).
///
/// # Errors
///
/// A template error (a programming error; the templates are tested).
pub fn ask_system(date: &str, max_iterations: usize) -> Result<String, FormatError> {
    let budget = max_iterations.to_string();
    let workflow = format(
        ASK_WORKFLOW,
        &[("date", date), ("max_iterations", budget.as_str())],
    )?;
    Ok([workflow.as_str(), ASK_TOOLS, ASK_OUTPUT].join("\n\n"))
}

/// `get_research_instructions()` for `date`.
///
/// # Errors
///
/// A template error.
pub fn research_system(date: &str) -> Result<String, FormatError> {
    let workflow = format(RESEARCH_WORKFLOW, &[("date", date)])?;
    Ok([
        workflow.as_str(),
        RESEARCH_TOOLS,
        RESEARCH_STOPPING,
        RESEARCH_OUTPUT,
    ]
    .join("\n\n"))
}

/// `get_ask_prompt(question, repo_context, chat_history)`.
#[must_use]
pub fn ask_user(question: &str, repo_context: &str, chat_history: &str) -> String {
    let mut parts = vec![format!("## Question\n\n{question}\n")];
    if !repo_context.is_empty() {
        parts.push(format!("## Repository Context\n\n{repo_context}\n"));
    }
    if !chat_history.is_empty() {
        parts.push(format!("## Conversation History\n\n{chat_history}\n"));
    }
    parts.push(
        "## Getting Started\n\nUse `search_symbols` (call multiple in parallel) to find relevant code, \
         then `get_relationships` for key symbols, then `get_code` for 2-4 critical symbols. \
         Answer directly — do not create a research report.\n"
            .to_owned(),
    );
    parts.join("\n")
}

/// `get_research_prompt(research_type, topic, context)`. An unknown type
/// is `general`, as `dict.get` with a default.
#[must_use]
pub fn research_user(research_type: &str, topic: &str, context: &str) -> String {
    let guidance = RESEARCH_TYPES
        .iter()
        .find(|(name, _)| *name == research_type)
        .map_or(RESEARCH_TYPES[0].1, |(_, text)| *text);
    let mut parts = vec![format!("## Research Question\n\n{topic}\n")];
    if !context.is_empty() {
        parts.push(format!("## Repository Context\n\n{context}\n"));
    }
    parts.push(format!("## Research Focus\n\n{guidance}\n"));
    parts.push(
        "## Getting Started\n\n\
         1. Search strategically using `search_codebase` (call multiple in parallel)\n\
         2. Use `get_symbol_relationships` for code connections\n\
         3. Use `think` briefly after searches to reflect\n\
         4. Return the comprehensive answer directly in your final message\n"
            .to_owned(),
    );
    parts.join("\n")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_templates_format() {
        let system = ask_system("2026-01-02", 8).expect("format");
        assert!(system.contains("Today's date is 2026-01-02."));
        assert!(system.contains("maximum of **8 tool calls**"));
        let research = research_system("2026-01-02").expect("format");
        assert!(research.starts_with("# Deep Research Workflow"));
        assert!(research_user("nope", "q", "").contains("holistically"));
    }
}
