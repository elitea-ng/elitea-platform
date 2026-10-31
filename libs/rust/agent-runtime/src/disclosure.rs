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
//!
//! A tool that takes its text inside a JSON document argument (Jira
//! `create_issue`, Bitbucket `create_pull_request`, Azure DevOps Boards
//! `create_work_item`) names the text with an RFC 6901 JSON pointer into
//! that argument (`issue_json#/fields/description`). The argument can be a
//! JSON string or an object. A string that is not JSON is left as it is: the
//! toolkit refuses it. An absent text field gets the marker only; an Atlassian
//! Document Format value gets the marker as a last paragraph.

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
    /// The target renders HTML (an Azure DevOps work item description): the
    /// marker is a paragraph, with the text escaped.
    Html,
}

/// One text parameter of a tool.
#[derive(Clone, Debug, Eq, PartialEq)]
enum TextPath {
    /// A top-level string argument.
    Field(Box<str>),
    /// A string field of each object in a top-level list argument
    /// (`list[].field`).
    ListField(Box<str>, Box<str>),
    /// A text field inside a JSON document argument: the argument and the
    /// unescaped reference tokens of an RFC 6901 pointer
    /// (`issue_json#/fields/description`).
    JsonPointer(Box<str>, Box<[Box<str>]>),
}

impl TextPath {
    fn parse(raw: &str) -> Option<Self> {
        let valid = |part: &str| {
            !part.is_empty()
                && part
                    .chars()
                    .all(|character| character.is_ascii_alphanumeric() || character == '_')
        };
        if let Some((argument, pointer)) = raw.split_once('#') {
            let tokens = pointer
                .strip_prefix('/')?
                .split('/')
                .map(unescape_pointer_token)
                .collect::<Option<Box<[_]>>>()?;
            return valid(argument).then(|| Self::JsonPointer(argument.into(), tokens));
        }
        match raw.split_once("[].") {
            Some((list, field)) if valid(list) && valid(field) => {
                Some(Self::ListField(list.into(), field.into()))
            }
            None if valid(raw) => Some(Self::Field(raw.into())),
            _ => None,
        }
    }
}

/// One RFC 6901 reference token, unescaped (`~1` is `/`, `~0` is `~`). An
/// empty token or a `~` that starts no escape is malformed.
fn unescape_pointer_token(token: &str) -> Option<Box<str>> {
    if token.is_empty() {
        return None;
    }
    let mut out = String::with_capacity(token.len());
    let mut characters = token.chars();
    while let Some(character) = characters.next() {
        if character == '~' {
            match characters.next() {
                Some('0') => out.push('~'),
                Some('1') => out.push('/'),
                _ => return None,
            }
        } else {
            out.push(character);
        }
    }
    Some(out.into())
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
                "html" => DisclosureFormat::Html,
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
            (DisclosureFormat::Html, _) => format!("<p>{}</p>", escape_html(&self.text)),
            _ => self.text.to_string(),
        }
    }

    /// Append the marker to each text parameter of a listed tool's call.
    /// Returns how many parameters got it. A parameter that is absent or
    /// not a string is left to the tool's own validation; a text that
    /// already ends with the marker is not changed. Inside a JSON document
    /// argument, an absent or empty text field gets the marker only.
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
                TextPath::JsonPointer(argument, tokens) => {
                    applied += usize::from(arguments.get_mut(&**argument).is_some_and(|value| {
                        append_marker_in_document(value, tokens, &marker, self.text())
                    }));
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

/// The start of the hidden run id comment of a Markdown marker.
const RUN_ID_COMMENT: &str = " <!-- elitea-run-id: ";

/// Whether `text` already ends with the marker's text, with or without the
/// run id comment of any run. An update tool (for example a new issue body
/// that keeps the old marker) then does not get a second marker.
fn already_marked(text: &str, marker: &str) -> bool {
    let marker_text = marker.split(RUN_ID_COMMENT).next().unwrap_or(marker);
    let mut end = text.trim_end();
    if end.ends_with("-->")
        && let Some(start) = end.rfind(RUN_ID_COMMENT)
    {
        end = &end[..start];
    }
    end.ends_with(marker_text)
}

fn append_marker(value: &mut Value, marker: &str) -> bool {
    let Value::String(text) = value else {
        return false;
    };
    if text.trim().is_empty() || already_marked(text, marker) {
        return false;
    }
    let trimmed = text.trim_end().len();
    text.truncate(trimmed);
    text.push_str(SEPARATOR);
    text.push_str(marker);
    true
}

/// Append the marker to the text field at `tokens` inside a JSON document
/// argument: a JSON string (re-encoded after the change) or an object. A
/// string that is not a JSON object, or a path whose parent is not an
/// object, is not changed.
fn append_marker_in_document(
    argument: &mut Value,
    tokens: &[Box<str>],
    marker: &str,
    marker_text: &str,
) -> bool {
    match argument {
        Value::String(encoded) => {
            let Ok(mut document) = serde_json::from_str::<Value>(encoded) else {
                return false;
            };
            if !document.is_object()
                || !append_at_pointer(&mut document, tokens, marker, marker_text)
            {
                return false;
            }
            match serde_json::to_string(&document) {
                Ok(changed) => {
                    *encoded = changed;
                    true
                }
                Err(_) => false,
            }
        }
        Value::Object(_) => append_at_pointer(argument, tokens, marker, marker_text),
        _ => false,
    }
}

fn append_at_pointer(
    document: &mut Value,
    tokens: &[Box<str>],
    marker: &str,
    marker_text: &str,
) -> bool {
    let Some((leaf, parents)) = tokens.split_last() else {
        return false;
    };
    let mut parent = document;
    for token in parents {
        let Some(child) = parent
            .as_object_mut()
            .and_then(|object| object.get_mut(&**token))
        else {
            return false;
        };
        parent = child;
    }
    let Some(parent) = parent.as_object_mut() else {
        return false;
    };
    match parent.get_mut(&**leaf) {
        None | Some(Value::Null) => {
            parent.insert(leaf.to_string(), Value::String(marker.to_owned()));
            true
        }
        Some(Value::String(text)) if text.trim().is_empty() => {
            marker.clone_into(text);
            true
        }
        Some(value @ Value::String(_)) => append_marker(value, marker),
        Some(Value::Object(adf)) => append_adf_paragraph(adf, marker_text),
        Some(_) => false,
    }
}

/// Append the marker as the last paragraph of an Atlassian Document Format
/// document (Jira Cloud API v3). A document whose last paragraph is the
/// marker is not changed; an object that is not a document is not changed.
fn append_adf_paragraph(document: &mut Map<String, Value>, marker_text: &str) -> bool {
    if document.get("type").and_then(Value::as_str) != Some("doc") {
        return false;
    }
    let Some(Value::Array(content)) = document.get_mut("content") else {
        return false;
    };
    let last_text = content.last().and_then(|node| {
        (node.get("type").and_then(Value::as_str) == Some("paragraph")).then(|| {
            node.get("content")
                .and_then(Value::as_array)
                .into_iter()
                .flatten()
                .filter_map(|inline| inline.get("text").and_then(Value::as_str))
                .collect::<String>()
        })
    });
    if last_text.is_some_and(|text| text.trim() == marker_text) {
        return false;
    }
    content.push(serde_json::json!({
        "type": "paragraph",
        "content": [{"type": "text", "text": marker_text}],
    }));
    true
}

fn escape_html(text: &str) -> String {
    let mut out = String::with_capacity(text.len());
    for character in text.chars() {
        match character {
            '&' => out.push_str("&amp;"),
            '<' => out.push_str("&lt;"),
            '>' => out.push_str("&gt;"),
            '"' => out.push_str("&quot;"),
            '\'' => out.push_str("&#39;"),
            other => out.push(other),
        }
    }
    out
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
    fn the_compiled_in_contract_is_well_formed_and_lists_only_send_and_write_tools() {
        let declared = parse_contract(CONTRACT).expect("the contract parses");
        assert!(declared.len() >= 10, "toolkits = {}", declared.len());
        let contract: Value = serde_json::from_str(CONTRACT).expect("json");
        for (toolkit, tools) in contract["toolkits"].as_object().expect("toolkits") {
            for tool in tools.as_object().expect("tools").keys() {
                // An outbound message is external_send; a pull request, an
                // issue, a case, a ticket or a page with a body keeps its
                // category (write), so the approval defaults do not change.
                let category = declared_category(toolkit, tool).unwrap_or(ToolCategory::Write);
                assert!(
                    matches!(category, ToolCategory::ExternalSend | ToolCategory::Write),
                    "{toolkit}.{tool} is {category}, not external_send or write"
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
            r#"{"contract":"elitea.tool-disclosure.v1","default_marker_text":"Generated with Elitea AI","toolkits":{"x":{"a":{"format":"rtf","text_parameters":["b"]}}}}"#,
            r#"{"contract":"elitea.tool-disclosure.v1","default_marker_text":"Generated with Elitea AI","toolkits":{"x":{"a":{"format":"plain","text_parameters":[]}}}}"#,
            r#"{"contract":"elitea.tool-disclosure.v1","default_marker_text":"Generated with Elitea AI","toolkits":{"x":{"a":{"format":"plain","text_parameters":["a.b"]}}}}"#,
            r#"{"contract":"elitea.tool-disclosure.v1","default_marker_text":"Generated with Elitea AI","toolkits":{"x":{"a":{"format":"plain","text_parameters":["doc#fields"]}}}}"#,
            r#"{"contract":"elitea.tool-disclosure.v1","default_marker_text":"Generated with Elitea AI","toolkits":{"x":{"a":{"format":"plain","text_parameters":["doc#/fields//x"]}}}}"#,
            r#"{"contract":"elitea.tool-disclosure.v1","default_marker_text":"Generated with Elitea AI","toolkits":{"x":{"a":{"format":"plain","text_parameters":["doc#/a~2b"]}}}}"#,
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

    /// A pull request or issue body is `write`, not `external_send`, but it
    /// is text outside the platform: it gets the marker, once, also when an
    /// update keeps the marker of an earlier run.
    #[test]
    fn pull_request_and_issue_bodies_get_the_marker_once() {
        let policy = DisclosurePolicy::new("Generated with Elitea AI", Vec::<String>::new())
            .with_run_id("exec-2");
        let mut call = arguments(&json!({"title": "Fix", "body": "Fixes the build."}));
        assert_eq!(policy.apply("github", "create_pull_request", &mut call), 1);
        assert_eq!(
            call["body"],
            "Fixes the build.\n\nGenerated with Elitea AI <!-- elitea-run-id: exec-2 -->"
        );
        let mut update = arguments(&json!({
            "issue_id": 7,
            "body": "Old text.\n\nGenerated with Elitea AI <!-- elitea-run-id: exec-1 -->"
        }));
        assert_eq!(policy.apply("github", "update_issue", &mut update), 0);
        assert_eq!(
            declared_category("github", "create_pull_request").unwrap_or(ToolCategory::Write),
            ToolCategory::Write
        );
    }

    fn document(call: &Map<String, Value>, argument: &str) -> Value {
        serde_json::from_str(call[argument].as_str().expect("a JSON string")).expect("JSON")
    }

    /// Jira `create_issue` takes its text inside `issue_json`: a wiki or
    /// plain description gets the marker, an ADF description (API v3) gets a
    /// last paragraph, an absent one gets the marker only. The rest of the
    /// document is not changed.
    #[test]
    fn jira_create_issue_writes_the_marker_inside_issue_json() {
        let policy = DisclosurePolicy::default();
        let fields = json!({"project": {"key": "EL"}, "summary": "Fix", "issuetype": {"name": "Bug"}, "customfield_1": 1.50});
        let mut plain = fields.clone();
        plain["description"] = json!("The build fails.");
        let mut call = arguments(&json!({"issue_json": json!({"fields": plain}).to_string()}));
        assert_eq!(policy.apply("jira", "create_issue", &mut call), 1);
        let created = document(&call, "issue_json");
        assert_eq!(
            created["fields"]["description"],
            "The build fails.\n\nGenerated with Elitea AI"
        );
        assert_eq!(created["fields"]["summary"], "Fix");
        assert_eq!(created["fields"]["customfield_1"], fields["customfield_1"]);
        // Applied once: a replayed call is not marked twice.
        assert_eq!(policy.apply("jira", "create_issue", &mut call), 0);

        let mut call = arguments(&json!({"issue_json": json!({"fields": fields}).to_string()}));
        assert_eq!(policy.apply("jira", "create_issue", &mut call), 1);
        assert_eq!(
            document(&call, "issue_json")["fields"]["description"],
            "Generated with Elitea AI"
        );

        let mut adf = fields.clone();
        adf["description"] = json!({"type": "doc", "version": 1, "content": [
            {"type": "paragraph", "content": [{"type": "text", "text": "The build fails."}]}
        ]});
        // An object argument is changed in place.
        let mut call = arguments(&json!({"issue_json": {"fields": adf}}));
        assert_eq!(policy.apply("jira", "create_issue", &mut call), 1);
        let content = &call["issue_json"]["fields"]["description"]["content"];
        assert_eq!(content.as_array().map(Vec::len), Some(2));
        assert_eq!(
            content[1],
            json!({"type": "paragraph", "content": [{"type": "text", "text": "Generated with Elitea AI"}]})
        );
        assert_eq!(policy.apply("jira", "create_issue", &mut call), 0);
    }

    /// A document argument that is not JSON, or has no object on the path,
    /// is left as it is: the toolkit refuses it with its own message.
    #[test]
    fn a_malformed_document_argument_is_left_as_it_is() {
        let policy = DisclosurePolicy::default();
        for raw in ["{not json", "[1, 2]", r#"{"fields": "x"}"#, r#""text""#] {
            let mut call = arguments(&json!({"issue_json": raw}));
            assert_eq!(policy.apply("jira", "create_issue", &mut call), 0, "{raw}");
            assert_eq!(call["issue_json"], raw);
        }
        // No `fields` object: no field is invented above the text.
        let mut call = arguments(&json!({"issue_json": r#"{"update": {}}"#}));
        assert_eq!(policy.apply("jira", "create_issue", &mut call), 0);
    }

    #[test]
    fn bitbucket_and_azure_devops_documents_get_their_formats() {
        let policy = DisclosurePolicy::new("AI & <us>", Vec::<String>::new()).with_run_id("exec-9");
        let mut call =
            arguments(&json!({"pr_json_data": r#"{"title":"Fix","description":"Fixes it."}"#}));
        assert_eq!(
            policy.apply("bitbucket", "create_pull_request", &mut call),
            1
        );
        assert_eq!(
            document(&call, "pr_json_data")["description"],
            "Fixes it.\n\nAI & <us> <!-- elitea-run-id: exec-9 -->"
        );
        let mut call = arguments(&json!({
            "work_item_json": r#"{"fields":{"System.Title":"Fix","System.Description":"<div>Broken</div>"}}"#,
            "wi_type": "Task"
        }));
        assert_eq!(policy.apply("ado_boards", "create_work_item", &mut call), 1);
        assert_eq!(
            document(&call, "work_item_json")["fields"]["System.Description"],
            "<div>Broken</div>\n\n<p>AI &amp; &lt;us&gt;</p>"
        );
        assert_eq!(policy.apply("ado_boards", "create_work_item", &mut call), 0);
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
