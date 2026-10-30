//! The AI disclosure marker (ADR-0043 decision 3, #1500).
//!
//! A built-in `external_send` tool that writes human-readable text outside
//! the platform (a ticket comment, a pull request comment, a chat post, an
//! email) gets the platform's disclosure marker appended to that text before
//! the call. The tools and their text parameters come from one contract,
//! `libs/proto/contracts/tool-disclosure-v1.json`, compiled in here and read
//! by elitea-main for the opt-out toolkit types.
//!
//! The marker is the administrator's text (default
//! [`DEFAULT_MARKER_TEXT`]); a project can change the text but cannot remove
//! it. Only the platform administrator can allow an opt-out per toolkit type,
//! and only then can a project opt out for that type. Main sends the result
//! as `WorkerCommandV1.content_controls` (field 71); without it the worker
//! appends the default text to every listed tool.
//!
//! Where the target renders Markdown, the marker also carries the run id in
//! an HTML comment, which the target does not show. The marker is appended
//! once: a text that already ends with it is not changed.

use std::collections::{BTreeSet, HashMap};
use std::sync::{Arc, LazyLock};

use serde_json::{Map, Value};

use crate::tool_category::{canonical_key, contract_toolkit_key, tool_keys};

/// The disclosure contract.
const CONTRACT: &str = include_str!("../../../proto/contracts/tool-disclosure-v1.json");

/// The contract's identity; a different one is not read.
const CONTRACT_NAME: &str = "elitea.tool-disclosure.v1";

/// The marker text when the administrator set none.
pub const DEFAULT_MARKER_TEXT: &str = "Generated with Elitea AI";

/// The longest marker text, in bytes. Main refuses a longer one; the worker
/// cuts a longer one at a character boundary.
pub const MAX_MARKER_TEXT_BYTES: usize = 200;

/// The longest run id the marker carries, in bytes.
const MAX_RUN_ID_BYTES: usize = 128;

/// How the target renders the text.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum DisclosureFormat {
    /// The target shows the text as written: the marker is text only.
    Plain,
    /// The target renders Markdown and hides HTML comments: the marker also
    /// carries the run id in a comment.
    Markdown,
}

/// One text parameter of a tool.
#[derive(Clone, Debug, Eq, PartialEq)]
enum TextPath {
    /// A top-level string argument.
    Field(Box<str>),
    /// A string field of each object in a top-level list argument
    /// (`list[].field`).
    ListField(Box<str>, Box<str>),
}

impl TextPath {
    fn parse(raw: &str) -> Option<Self> {
        let valid = |part: &str| {
            !part.is_empty()
                && part
                    .chars()
                    .all(|character| character.is_ascii_alphanumeric() || character == '_')
        };
        match raw.split_once("[].") {
            Some((list, field)) if valid(list) && valid(field) => {
                Some(Self::ListField(list.into(), field.into()))
            }
            None if valid(raw) => Some(Self::Field(raw.into())),
            _ => None,
        }
    }
}

/// One listed tool.
#[derive(Clone, Debug, Eq, PartialEq)]
struct DisclosedTool {
    format: DisclosureFormat,
    paths: Vec<TextPath>,
}

/// Toolkit type key → tool key → its text parameters.
type Declarations = HashMap<String, HashMap<String, DisclosedTool>>;

static DECLARED: LazyLock<Declarations> =
    LazyLock::new(|| parse_contract(CONTRACT).unwrap_or_default());

/// Read one contract. `None` for a malformed one (a test holds the
/// compiled-in contract well formed).
fn parse_contract(raw: &str) -> Option<Declarations> {
    let contract: Value = serde_json::from_str(raw).ok()?;
    if contract.get("contract").and_then(Value::as_str) != Some(CONTRACT_NAME)
        || contract.get("default_marker_text").and_then(Value::as_str) != Some(DEFAULT_MARKER_TEXT)
    {
        return None;
    }
    let mut declared = Declarations::new();
    for (toolkit, tools) in contract.get("toolkits")?.as_object()? {
        let mut entries = HashMap::new();
        for (tool, entry) in tools.as_object()? {
            let format = match entry.get("format")?.as_str()? {
                "plain" => DisclosureFormat::Plain,
                "markdown" => DisclosureFormat::Markdown,
                _ => return None,
            };
            let paths = entry
                .get("text_parameters")?
                .as_array()?
                .iter()
                .map(|path| path.as_str().and_then(TextPath::parse))
                .collect::<Option<Vec<_>>>()?;
            if paths.is_empty() {
                return None;
            }
            if entries
                .insert(canonical_key(tool)?, DisclosedTool { format, paths })
                .is_some()
            {
                return None;
            }
        }
        if declared.insert(canonical_key(toolkit)?, entries).is_some() {
            return None;
        }
    }
    Some(declared)
}

fn declared_tool(toolkit_type: &str, tool_name: &str) -> Option<&'static DisclosedTool> {
    let tools = DECLARED.get(&contract_toolkit_key(toolkit_type)?)?;
    tool_keys(tool_name).iter().find_map(|key| tools.get(key))
}

/// Whether the contract lists the tool: its calls get the marker.
#[must_use]
pub fn is_disclosed_tool(toolkit_type: &str, tool_name: &str) -> bool {
    declared_tool(toolkit_type, tool_name).is_some()
}

/// One run's disclosure settings.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct DisclosurePolicy {
    text: Box<str>,
    run_id: Option<Box<str>>,
    /// Canonical toolkit type keys whose tools get no marker.
    opt_out: BTreeSet<String>,
}

impl Default for DisclosurePolicy {
    fn default() -> Self {
        Self {
            text: DEFAULT_MARKER_TEXT.into(),
            run_id: None,
            opt_out: BTreeSet::new(),
        }
    }
}

impl DisclosurePolicy {
    /// The worker default: the default text on every listed tool.
    #[must_use]
    pub fn worker_default() -> Arc<Self> {
        Arc::new(Self::default())
    }

    /// The settings Main sent. An empty or blank text is the default text:
    /// the marker cannot be removed through the text. A text longer than
    /// [`MAX_MARKER_TEXT_BYTES`] is cut at a character boundary; control
    /// characters become spaces.
    #[must_use]
    pub fn new<I, S>(text: &str, opt_out_toolkit_types: I) -> Self
    where
        I: IntoIterator<Item = S>,
        S: AsRef<str>,
    {
        let cleaned = text
            .chars()
            .map(|character| {
                if character.is_control() {
                    ' '
                } else {
                    character
                }
            })
            .collect::<String>();
        let cleaned = cleaned.trim();
        let text = if cleaned.is_empty() {
            DEFAULT_MARKER_TEXT
        } else {
            truncate(cleaned, MAX_MARKER_TEXT_BYTES)
        };
        Self {
            text: text.into(),
            run_id: None,
            opt_out: opt_out_toolkit_types
                .into_iter()
                .filter_map(|toolkit| contract_toolkit_key(toolkit.as_ref()))
                .collect(),
        }
    }

    /// The same settings, naming the run in Markdown targets. A run id with
    /// characters other than ASCII letters, digits, `-`, `_` and `.` is not
    /// carried.
    #[must_use]
    pub fn with_run_id(mut self, run_id: &str) -> Self {
        let valid = !run_id.is_empty()
            && run_id.len() <= MAX_RUN_ID_BYTES
            && run_id.chars().all(|character| {
                character.is_ascii_alphanumeric() || matches!(character, '-' | '_' | '.')
            });
        self.run_id = valid.then(|| run_id.into());
        self
    }

    /// The marker text.
    #[must_use]
    pub fn text(&self) -> &str {
        &self.text
    }

    /// The run id the Markdown marker carries, if any.
    #[must_use]
    pub fn run_id(&self) -> Option<&str> {
        self.run_id.as_deref()
    }

    /// Whether the toolkit type opted out.
    #[must_use]
    pub fn opted_out(&self, toolkit_type: &str) -> bool {
        contract_toolkit_key(toolkit_type).is_some_and(|key| self.opt_out.contains(&key))
    }

    /// The marker for a target of `format`.
    #[must_use]
    pub fn marker(&self, format: DisclosureFormat) -> String {
        match (format, self.run_id.as_deref()) {
            (DisclosureFormat::Markdown, Some(run_id)) => {
                format!("{} <!-- elitea-run-id: {run_id} -->", self.text)
            }
            _ => self.text.to_string(),
        }
    }

    /// Append the marker to each text parameter of a listed tool's call.
    /// Returns how many parameters got it. A parameter that is absent or
    /// not a string is left to the tool's own validation; a text that
    /// already ends with the marker is not changed.
    pub fn apply(
        &self,
        toolkit_type: &str,
        tool_name: &str,
        arguments: &mut Map<String, Value>,
    ) -> usize {
        let Some(tool) = declared_tool(toolkit_type, tool_name) else {
            return 0;
        };
        if self.opted_out(toolkit_type) {
            return 0;
        }
        let marker = self.marker(tool.format);
        let mut applied = 0;
        for path in &tool.paths {
            match path {
                TextPath::Field(field) => {
                    applied += usize::from(
                        arguments
                            .get_mut(&**field)
                            .is_some_and(|value| append_marker(value, &marker)),
                    );
                }
                TextPath::ListField(list, field) => {
                    if let Some(Value::Array(items)) = arguments.get_mut(&**list) {
                        for item in items.iter_mut().filter_map(Value::as_object_mut) {
                            applied += usize::from(
                                item.get_mut(&**field)
                                    .is_some_and(|value| append_marker(value, &marker)),
                            );
                        }
                    }
                }
            }
        }
        applied
    }
}

/// The separator between the text and the marker.
const SEPARATOR: &str = "\n\n";

fn append_marker(value: &mut Value, marker: &str) -> bool {
    let Value::String(text) = value else {
        return false;
    };
    if text.trim().is_empty() || text.trim_end().ends_with(marker) {
        return false;
    }
    let trimmed = text.trim_end().len();
    text.truncate(trimmed);
    text.push_str(SEPARATOR);
    text.push_str(marker);
    true
}

fn truncate(text: &str, max_bytes: usize) -> &str {
    if text.len() <= max_bytes {
        return text;
    }
    let mut end = max_bytes;
    while !text.is_char_boundary(end) {
        end -= 1;
    }
    text[..end].trim_end()
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use super::*;
    use crate::tool_category::{ToolCategory, declared_category};

    fn arguments(value: &Value) -> Map<String, Value> {
        value.as_object().expect("an object").clone()
    }

    #[test]
    fn the_compiled_in_contract_is_well_formed_and_lists_only_external_send_tools() {
        let declared = parse_contract(CONTRACT).expect("the contract parses");
        assert!(declared.len() >= 10, "toolkits = {}", declared.len());
        let contract: Value = serde_json::from_str(CONTRACT).expect("json");
        for (toolkit, tools) in contract["toolkits"].as_object().expect("toolkits") {
            for tool in tools.as_object().expect("tools").keys() {
                assert_eq!(
                    declared_category(toolkit, tool),
                    Some(ToolCategory::ExternalSend),
                    "{toolkit}.{tool} is not external_send"
                );
            }
        }
    }

    #[test]
    fn every_external_send_tool_is_listed_or_exempt() {
        let categories: Value = serde_json::from_str(include_str!(
            "../../../proto/contracts/tool-categories-v1.json"
        ))
        .expect("json");
        let disclosure: Value = serde_json::from_str(CONTRACT).expect("json");
        for (toolkit, tools) in categories["toolkits"].as_object().expect("toolkits") {
            for (tool, category) in tools.as_object().expect("tools") {
                if category != "external_send" {
                    continue;
                }
                let listed = disclosure["toolkits"][toolkit][tool].is_object();
                let exempt = disclosure["exempt"][toolkit][tool]
                    .as_str()
                    .is_some_and(|reason| !reason.is_empty());
                assert!(
                    listed ^ exempt,
                    "{toolkit}.{tool}: list it or exempt it, once"
                );
            }
        }
    }

    #[test]
    fn a_malformed_contract_is_not_read() {
        for raw in [
            "{",
            r#"{"contract":"other","default_marker_text":"Generated with Elitea AI","toolkits":{}}"#,
            r#"{"contract":"elitea.tool-disclosure.v1","default_marker_text":"x","toolkits":{}}"#,
            r#"{"contract":"elitea.tool-disclosure.v1","default_marker_text":"Generated with Elitea AI","toolkits":{"x":{"a":{"format":"html","text_parameters":["b"]}}}}"#,
            r#"{"contract":"elitea.tool-disclosure.v1","default_marker_text":"Generated with Elitea AI","toolkits":{"x":{"a":{"format":"plain","text_parameters":[]}}}}"#,
            r#"{"contract":"elitea.tool-disclosure.v1","default_marker_text":"Generated with Elitea AI","toolkits":{"x":{"a":{"format":"plain","text_parameters":["a.b"]}}}}"#,
        ] {
            assert!(parse_contract(raw).is_none(), "{raw}");
        }
    }

    /// ADR-0043 verification: a Jira comment tool call writes the marker.
    #[test]
    fn a_jira_comment_gets_the_default_marker() {
        let policy = DisclosurePolicy::worker_default();
        let mut call = arguments(&json!({"issue_key": "EL-1", "comment": "Fixed in 1.2.  "}));
        assert_eq!(policy.apply("jira", "add_comments", &mut call), 1);
        assert_eq!(call["comment"], "Fixed in 1.2.\n\nGenerated with Elitea AI");
        assert_eq!(call["issue_key"], "EL-1");
        // Applied once: a replayed call is not marked twice.
        assert_eq!(policy.apply("jira", "add_comments", &mut call), 0);
        assert_eq!(call["comment"], "Fixed in 1.2.\n\nGenerated with Elitea AI");
    }

    #[test]
    fn markdown_targets_carry_the_run_id_in_a_hidden_comment() {
        let policy = DisclosurePolicy::new("Written by our AI assistant", Vec::<String>::new())
            .with_run_id("exec-42");
        let mut call =
            arguments(&json!({"repo_name": "o/r", "issue_number": 7, "comment": "LGTM"}));
        assert_eq!(
            policy.apply("github", "github___comment_on_issue", &mut call),
            1
        );
        assert_eq!(
            call["comment"],
            "LGTM\n\nWritten by our AI assistant <!-- elitea-run-id: exec-42 -->"
        );
        // A plain target gets the text only.
        let mut post = arguments(&json!({"channel_id": "C1", "message": "Done"}));
        assert_eq!(policy.apply("slack", "send_message", &mut post), 1);
        assert_eq!(post["message"], "Done\n\nWritten by our AI assistant");
    }

    #[test]
    fn list_fields_get_the_marker_on_each_item() {
        let policy = DisclosurePolicy::worker_default();
        let mut call = arguments(&json!({
            "comment_query": "12\n\nPlease rebase.",
            "inline_comments": [
                {"file_path": "a.rs", "comment_text": "Rename this."},
                {"file_path": "b.rs", "comment_text": 3},
                "not an object",
            ],
        }));
        assert_eq!(
            policy.apply("ado_repos", "comment_on_pull_request", &mut call),
            2
        );
        assert_eq!(
            call["comment_query"],
            "12\n\nPlease rebase.\n\nGenerated with Elitea AI"
        );
        assert_eq!(
            call["inline_comments"][0]["comment_text"],
            "Rename this.\n\nGenerated with Elitea AI"
        );
        assert_eq!(call["inline_comments"][1]["comment_text"], 3);
    }

    #[test]
    fn a_blank_text_is_the_default_and_the_text_is_bounded() {
        assert_eq!(
            DisclosurePolicy::new("  \n", Vec::<String>::new()).text(),
            DEFAULT_MARKER_TEXT
        );
        let long = "é".repeat(150);
        let policy = DisclosurePolicy::new(&long, Vec::<String>::new());
        assert!(policy.text().len() <= MAX_MARKER_TEXT_BYTES);
        assert_eq!(
            DisclosurePolicy::new("AI\u{7}text", Vec::<String>::new()).text(),
            "AI text"
        );
    }

    #[test]
    fn an_opted_out_toolkit_type_and_other_tools_are_not_changed() {
        let policy = DisclosurePolicy::new("", ["Slack"]);
        let mut post = arguments(&json!({"channel_id": "C1", "message": "Done"}));
        assert_eq!(policy.apply("slack", "send_message", &mut post), 0);
        assert_eq!(post["message"], "Done");
        let mut comment = arguments(&json!({"issue_key": "EL-1", "comment": "x"}));
        assert_eq!(policy.apply("jira", "add_comments", &mut comment), 1);
        // A read tool, an exempt tool and an unknown toolkit are not listed.
        let mut read = arguments(&json!({"jql": "x"}));
        assert_eq!(policy.apply("jira", "search_using_jql", &mut read), 0);
        assert!(!is_disclosed_tool("jira", "update_comment_with_file"));
        assert!(!is_disclosed_tool("unknown", "send_message"));
        assert!(is_disclosed_tool("yagmail", "send_gmail_message"));
    }

    #[test]
    fn an_unsafe_run_id_is_not_carried() {
        let policy = DisclosurePolicy::worker_default()
            .as_ref()
            .clone()
            .with_run_id("x --> <script>");
        assert_eq!(policy.run_id(), None);
        assert_eq!(
            policy.marker(DisclosureFormat::Markdown),
            DEFAULT_MARKER_TEXT
        );
    }
}
