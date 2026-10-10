//! The `fixture` runner: canned results with paced progress.
//!
//! A port of `elitea_deepwiki.fixture_runner`. The Go host carries a third
//! copy (`internal/apps/deepwiki/run/fixture.go`): the E2E stack runs that
//! one, the standalone stack runs a sidecar, and the browser journeys must
//! pass on both. So the steps, the research plan, the streamed-answer table,
//! the canned wiki and the resolver's scoring are transcribed, not designed.
//!
//! The canned wiki is deterministic and derived from the request, so a test
//! can predict the keys that land: `{owner}--{repo}--{branch}/…`. One page
//! carries a mermaid block that does not parse, on purpose — it is what the
//! quick-fix journey repairs.

use super::{Context, prepare_arguments};
use crate::errors::{EngineError, ErrorType};
use crate::pyjson::{dumps, dumps_indent2};
use crate::source::{display_repository_for, py_str, py_truthy, wiki_id_for};
use serde_json::{Map, Value, json};
use std::time::Duration;

/// Progress each tool emits before it answers, in order.
const GENERATE_STEPS: [&str; 5] = [
    "Cloning the repository",
    "Indexing 12 files",
    "Planning the wiki structure",
    "Writing 3 pages",
    "Assembling the manifest",
];
const ASK_STEPS: [&str; 2] = ["Searching the wiki index", "Composing the answer"];
/// `None` is the structured research-plan event (see [`todo_update_event`]).
const RESEARCH_STEPS: [Option<&str>; 4] = [
    Some("Planning the research"),
    None,
    Some("Reading the relevant pages"),
    Some("Writing the report"),
];

/// How many fragments one fixture answer is cut into.
pub const STREAM_FRAGMENTS: usize = 3;

/// The deliberately broken page.
pub const BROKEN_MERMAID_PAGE: &str = "# Request flow\n\nThe diagram below is deliberately broken: the fixture exists so the quick fix\nhas something to repair.\n\n```mermaid\ngraph TD\n  A[Client] -->\n```\n\nAfter the diagram.\n";

/// The fixture runner.
#[derive(Debug, Clone, Default)]
pub struct FixtureRunner {
    /// The pause between progress steps; zero in unit tests.
    pub step: Duration,
}

/// The plan a fixture `deep_research` run publishes, as the browser's
/// research panel reads it.
#[must_use]
pub fn research_todos() -> Value {
    json!([
        {"id": 1, "title": "Plan the research", "description": "", "status": "completed"},
        {"id": 2, "title": "Read the relevant pages", "description": "", "status": "in_progress"},
        {"id": 3, "title": "Write the report", "description": "", "status": "pending"},
    ])
}

/// The `{event, data}` envelope the chat reducer routes to the research panel.
#[must_use]
pub fn todo_update_event() -> String {
    dumps(&json!({"event": "todo_update", "data": {"items": research_todos()}}))
}

/// The progress lines of `tool`, with the structured step resolved.
#[must_use]
pub fn steps_for(tool: &str) -> Vec<String> {
    match tool {
        "generate_wiki" => GENERATE_STEPS.iter().map(|s| (*s).to_owned()).collect(),
        "ask" => ASK_STEPS.iter().map(|s| (*s).to_owned()).collect(),
        "deep_research" => RESEARCH_STEPS
            .iter()
            .map(|s| s.map_or_else(todo_update_event, str::to_owned))
            .collect(),
        _ => Vec::new(),
    }
}

/// Which key of a tool's result holds the ANSWER the fixture streams.
/// `generate_wiki` is absent on purpose: it produces a wiki, not an answer.
#[must_use]
pub fn streamed_answer_key(tool: &str) -> Option<&'static str> {
    match tool {
        "ask" => Some("answer"),
        "deep_research" => Some("report"),
        _ => None,
    }
}

/// Cut one answer into `count` fragments that join back to it exactly.
/// Sizes count CHARACTERS (Python `len`), never bytes.
#[must_use]
pub fn answer_fragments(answer: &str, count: usize) -> Vec<String> {
    let characters: Vec<char> = answer.chars().collect();
    if characters.is_empty() {
        return Vec::new();
    }
    let size = characters.len().div_ceil(count.max(1)).max(1);
    characters
        .chunks(size)
        .map(|chunk| chunk.iter().collect())
        .collect()
}

impl FixtureRunner {
    /// Run one tool: progress, the canned result, then the streamed answer.
    ///
    /// # Errors
    ///
    /// A refused argument set, a tool failure, or the stop line.
    pub async fn run(
        &self,
        tool: &str,
        arguments: Map<String, Value>,
        context: &Context,
    ) -> Result<Value, EngineError> {
        let arguments = prepare_arguments(tool, arguments)?;
        for step in steps_for(tool) {
            // The checkpoint is what makes Stop work mid-run.
            context.checkpoint()?;
            context.thinking(step);
            context.pause(self.step).await;
        }
        let result = match tool {
            "generate_wiki" => generate_wiki(&arguments)?,
            "ask" => ask(&arguments)?,
            "deep_research" => deep_research(&arguments)?,
            "resolve_wiki" => resolve_wiki(&arguments)?,
            // The fixture holds no index, so there is nothing to delete.
            "delete_wiki_index" => serde_json::json!({
                "success": true,
                "wiki_id": required(tool, &arguments, "wiki_id")?,
                "deleted": false,
            }),
            "delete_project_wikis" => serde_json::json!({"success": true, "wikis": []}),
            other => {
                return Err(EngineError::new(
                    ErrorType::Value,
                    format!("Unknown tool: {other}"),
                ));
            }
        };
        if let Some(key) = streamed_answer_key(tool)
            && let Some(Value::String(answer)) = result.get(key)
        {
            for fragment in answer_fragments(answer, STREAM_FRAGMENTS) {
                context.checkpoint()?;
                context.token(fragment);
                context.pause(self.step).await;
            }
        }
        Ok(result)
    }
}

/// A required keyword-only argument, as Python's call would demand it.
fn required<'a>(
    tool: &str,
    arguments: &'a Map<String, Value>,
    name: &str,
) -> Result<&'a Value, EngineError> {
    arguments.get(name).ok_or_else(|| {
        EngineError::new(
            ErrorType::Type,
            format!("{tool}() missing 1 required keyword-only argument: '{name}'"),
        )
    })
}

/// A JSON value a Python attribute call would fail on, reported as Python
/// reports it.
fn not_a_mapping(value: &Value) -> EngineError {
    let kind = match value {
        Value::Bool(_) => "bool",
        Value::Number(number) if number.is_f64() => "float",
        Value::Number(_) => "int",
        Value::String(_) => "str",
        Value::Array(_) => "list",
        Value::Null | Value::Object(_) => "NoneType",
    };
    EngineError::new(
        ErrorType::Generic,
        format!("'{kind}' object has no attribute 'get'"),
    )
}

/// `active_branch` as the Python tool used it: `(branch or 'main').strip()`,
/// so any falsy value is the default branch and `str.strip` fails for a
/// truthy non-string.
fn branch_of(value: &Value) -> Result<Option<&str>, EngineError> {
    match value {
        Value::String(text) => Ok(Some(text.as_str())),
        other if !py_truthy(other) => Ok(None),
        other => Err(EngineError::new(
            ErrorType::Generic,
            format!(
                "'{}' object has no attribute 'strip'",
                if other.is_number() { "int" } else { "object" }
            ),
        )),
    }
}

/// `(repo_config or {}).get("provider_type") or "github"`.
fn provider_type_of(repo_config: Option<&Value>) -> Result<Value, EngineError> {
    Ok(match repo_config {
        None => json!("github"),
        Some(Value::Object(config)) => config
            .get("provider_type")
            .filter(|v| py_truthy(v))
            .cloned()
            .unwrap_or_else(|| json!("github")),
        Some(other) if !py_truthy(other) => json!("github"),
        Some(other) => return Err(not_a_mapping(other)),
    })
}

/// The canned generation: manifest, structure, three pages, context.
fn generate_wiki(arguments: &Map<String, Value>) -> Result<Value, EngineError> {
    let query = required("generate_wiki", arguments, "query")?;
    let repo_config = arguments.get("repo_config").filter(|v| !v.is_null());
    let branch_value = arguments
        .get("active_branch")
        .cloned()
        .unwrap_or_else(|| json!("main"));
    let branch = branch_of(&branch_value)?;
    let wiki_id = wiki_id_for(repo_config, branch)?;
    let mut repository = display_repository_for(repo_config)?;
    if repository.is_empty() {
        "fixture/repository".clone_into(&mut repository);
    }
    let provider_type = provider_type_of(repo_config)?;
    let pages: [(&str, String); 3] = [
        (
            "wiki_pages/overview/getting-started.md",
            format!(
                "# Getting started\n\nGenerated by the fixture runner for `{repository}` in answer to: {}\n",
                py_str(query)
            ),
        ),
        (
            "wiki_pages/architecture/request-flow.md",
            BROKEN_MERMAID_PAGE.to_owned(),
        ),
        (
            "wiki_pages/components/storage.md",
            "# Storage\n\nObjects live in the `wiki-artifacts` bucket, one key per page.\n"
                .to_owned(),
        ),
    ];
    let wiki_title = format!("{} wiki", repository.rsplit('/').next().unwrap_or_default());
    let manifest_branch = if py_truthy(&branch_value) {
        branch_value.clone()
    } else {
        json!("main")
    };
    let manifest = json!({
        "schema_version": 1,
        "wiki_id": wiki_id,
        "wiki_title": wiki_title,
        "description": "Generated by the DeepWiki fixture runner.",
        "wiki_version_id": "fixture-1",
        "created_at": "2026-01-01T00:00:00Z",
        "canonical_repo_identifier": repository,
        "repository": repository,
        "branch": manifest_branch,
        "provider_type": provider_type,
        "pages": pages.iter().map(|(key, _)| *key).collect::<Vec<_>>(),
    });
    let structure = json!({
        "title": wiki_title,
        "sections": [
            {"title": "Overview", "pages": ["wiki_pages/overview/getting-started.md"]},
            {"title": "Architecture", "pages": ["wiki_pages/architecture/request-flow.md"]},
            {"title": "Components", "pages": ["wiki_pages/components/storage.md"]},
        ],
    });
    let mut artifacts = vec![json!({
        "name": format!("{wiki_id}/analysis/wiki_structure_fixture.json"),
        "type": "application/json",
        "data": dumps_indent2(&structure),
    })];
    for (key, body) in &pages {
        artifacts.push(
            json!({"name": format!("{wiki_id}/{key}"), "type": "text/markdown", "data": body}),
        );
    }
    artifacts.push(json!({
        "name": format!("{wiki_id}/wiki_manifest_fixture-1.json"),
        "type": "application/json",
        "data": dumps_indent2(&manifest),
    }));
    Ok(json!({
        "success": true,
        "result": format!("Wiki generated: {} pages", pages.len()),
        "wiki_id": wiki_id,
        "artifacts": artifacts,
        "repository_context": format!(
            "repository: {repository}\nbranch: {}\nfiles: 12\n",
            py_str(&branch_value)
        ),
    }))
}

fn ask(arguments: &Map<String, Value>) -> Result<Value, EngineError> {
    let question = required("ask", arguments, "question")?;
    Ok(json!({
        "success": true,
        "answer": format!("Fixture answer to: {}", py_str(question)),
        "sources": [
            {"source": "wiki_pages/overview/getting-started.md"},
            {"source": "wiki_pages/components/storage.md"},
        ],
    }))
}

fn deep_research(arguments: &Map<String, Value>) -> Result<Value, EngineError> {
    let question = required("deep_research", arguments, "question")?;
    let research_type = arguments
        .get("research_type")
        .map_or_else(|| "general".to_owned(), py_str);
    Ok(json!({
        "success": true,
        "report": format!(
            "# Research report ({research_type})\n\nQuestion: {}\n\nFindings: fixture.\n",
            py_str(question)
        ),
    }))
}

/// The wiki resolver, without a model: word overlap between the question
/// and each candidate's id and title. The Go host's fixture
/// (`fixtureResolveWiki`) scores identically.
fn resolve_wiki(arguments: &Map<String, Value>) -> Result<Value, EngineError> {
    let question = required("resolve_wiki", arguments, "question")?;
    let lowered = match question {
        Value::String(text) => text.to_lowercase(),
        other if !py_truthy(other) => String::new(),
        other => {
            return Err(EngineError::new(
                ErrorType::Generic,
                format!(
                    "'{}' object has no attribute 'lower'",
                    if other.is_number() { "int" } else { "object" }
                ),
            ));
        }
    };
    let candidates: &[Value] = match arguments.get("wikis") {
        Some(Value::Array(items)) => items,
        _ => &[],
    };
    let (mut best, mut best_score) = (String::new(), 0usize);
    for candidate in candidates {
        let Value::Object(fields) = candidate else {
            continue;
        };
        let wiki_id = fields
            .get("wiki_id")
            .filter(|v| py_truthy(v))
            .map(py_str)
            .unwrap_or_default();
        let title = fields
            .get("wiki_title")
            .filter(|v| py_truthy(v))
            .map(py_str)
            .unwrap_or_default();
        let text = format!("{wiki_id} {title}")
            .to_lowercase()
            .replace(['-', '_', '/', '.'], " ");
        let score = text
            .split_whitespace()
            .filter(|word| word.chars().count() >= 3 && lowered.contains(word))
            .count();
        if score > best_score {
            best = wiki_id;
            best_score = score;
        }
    }
    if best.is_empty()
        && let [Value::Object(fields)] = candidates
    {
        best = fields
            .get("wiki_id")
            .filter(|v| py_truthy(v))
            .map(py_str)
            .unwrap_or_default();
    }
    Ok(json!({"success": true, "wiki_id": if best.is_empty() { "NONE".to_owned() } else { best }}))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn args(value: &Value) -> Map<String, Value> {
        value.as_object().cloned().unwrap_or_default()
    }

    #[test]
    fn the_fragments_of_an_answer_join_back_to_it() {
        for answer in ["", "a", "ab", "abc", "abcd", "Fixture answer to: x", "é😀ü"] {
            let fragments = answer_fragments(answer, STREAM_FRAGMENTS);
            assert_eq!(fragments.concat(), answer);
            assert!(fragments.len() <= STREAM_FRAGMENTS);
            assert!(fragments.iter().all(|f| !f.is_empty()));
        }
        assert_eq!(answer_fragments("abcd", 3), ["ab", "cd"]);
    }

    #[test]
    fn deep_research_publishes_its_plan_before_the_work() {
        let steps = steps_for("deep_research");
        assert_eq!(steps[0], "Planning the research");
        let event: Value = serde_json::from_str(&steps[1]).unwrap_or_default();
        assert_eq!(event["event"], "todo_update");
        assert_eq!(event["data"]["items"], research_todos());
        assert_eq!(
            steps[1],
            r#"{"event": "todo_update", "data": {"items": [{"id": 1, "title": "Plan the research", "description": "", "status": "completed"}, {"id": 2, "title": "Read the relevant pages", "description": "", "status": "in_progress"}, {"id": 3, "title": "Write the report", "description": "", "status": "pending"}]}}"#
        );
    }

    #[test]
    fn other_tools_carry_no_structured_step() {
        for tool in ["generate_wiki", "ask", "resolve_wiki"] {
            assert!(steps_for(tool).iter().all(|s| !s.contains("todo_update")));
        }
    }

    #[test]
    fn a_generation_lands_under_the_canonical_wiki_id() {
        let result = generate_wiki(&args(&json!({
            "query": "q",
            "repo_config": {"repository": "acme/notes"},
            "active_branch": "dev",
        })));
        let result = result.unwrap_or_default();
        assert_eq!(result["wiki_id"], "acme--notes--dev");
        let names: Vec<&str> = result["artifacts"]
            .as_array()
            .map(|a| a.iter().filter_map(|x| x["name"].as_str()).collect())
            .unwrap_or_default();
        assert_eq!(
            names,
            [
                "acme--notes--dev/analysis/wiki_structure_fixture.json",
                "acme--notes--dev/wiki_pages/overview/getting-started.md",
                "acme--notes--dev/wiki_pages/architecture/request-flow.md",
                "acme--notes--dev/wiki_pages/components/storage.md",
                "acme--notes--dev/wiki_manifest_fixture-1.json",
            ]
        );
        assert_eq!(
            result["repository_context"],
            "repository: acme/notes\nbranch: dev\nfiles: 12\n"
        );
    }

    #[test]
    fn a_falsy_branch_is_the_default_branch() {
        // python: wiki_id_for(cfg, False) == "acme--notes--main"; the context
        // line prints the value as Python would.
        for falsy in [json!(false), json!(0), json!(null), json!("")] {
            let result = generate_wiki(&args(&json!({
                "query": "q", "repo_config": {"repository": "acme/notes"}, "active_branch": falsy,
            })))
            .unwrap_or_default();
            assert_eq!(result["wiki_id"], "acme--notes--main", "{falsy}");
        }
        let truthy = generate_wiki(&args(&json!({"query": "q", "active_branch": 7})));
        assert_eq!(truthy.err().map(|e| e.error_type), Some(ErrorType::Generic));
    }

    #[test]
    fn a_missing_query_is_a_type_error() {
        let error = generate_wiki(&Map::new()).err();
        assert_eq!(error.map(|e| e.error_type), Some(ErrorType::Type));
    }

    #[test]
    fn the_resolver_scores_word_overlap() {
        let wikis = json!([
            {"wiki_id": "acme--billing--main", "wiki_title": "Billing"},
            {"wiki_id": "acme--notes--main", "wiki_title": "Notes service"},
        ]);
        let picked = resolve_wiki(&args(
            &json!({"question": "How are notes stored?", "wikis": wikis}),
        ));
        assert_eq!(picked.unwrap_or_default()["wiki_id"], "acme--notes--main");
        let none = resolve_wiki(&args(&json!({"question": "zzz", "wikis": wikis})));
        assert_eq!(none.unwrap_or_default()["wiki_id"], "NONE");
        let only = resolve_wiki(&args(
            &json!({"question": "zzz", "wikis": [{"wiki_id": "x--y--main"}]}),
        ));
        assert_eq!(only.unwrap_or_default()["wiki_id"], "x--y--main");
    }
}
