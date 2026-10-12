use std::fmt;
use std::fmt::Write as _;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD as BASE64_STANDARD;
use serde_json::{Map, Value, json};

use crate::toolkits::families::python_repr::repr;
use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{XrayApi, XrayClient, XrayClientError, invalid_response, resource_exhausted};
use super::config::{XrayConfigError, XrayConfigErrorCode, XrayToolkitConfig};

const MAX_OUTPUT_BYTES: usize = 512 * 1_024;
const MAX_ARGUMENT_BYTES: usize = 1_024 * 1_024;
const MAX_GRAPHQL_BYTES: usize = 128 * 1_024;
const MAX_MUTATIONS: usize = 50;
const MAX_FILEDATA_BYTES: usize = 1_024 * 1_024;
const MAX_TEST_PAGES: u64 = 100;
const MAX_DESCRIPTION_CHARS: usize = 1_000;

const GET_TESTS_QUERY: &str = r#"query GetTests($jql: String!, $limit:Int!, $start: Int)
{
    getTests(jql: $jql, limit: $limit, start: $start) {
        total
        start
        limit
        results {
            issueId
            jira(fields: ["key", "summary", "description", "created", "updated", "assignee.displayName", "reporter.displayName"])
            projectId
            testType {
                name
                kind
            }
            steps {
                id
                data
                action
                result
                attachments {
                    id
                    filename
                    downloadLink
                }
            }
            preconditions(limit: $limit) {
                total
                start
                limit
                results {
                    issueId
                    jira(fields: ["key"])
                    projectId
                }
            }
            unstructured
            gherkin
        }
    }
}
"#;

const UPDATE_TEST_STEP_MUTATION: &str = r"
            mutation UpdateTestStep($stepId: String!, $step: UpdateStepInput!) {
                updateTestStep(stepId: $stepId, step: $step) {
                    warnings
                }
            }
            ";

const GET_TEST_QUERY: &str = r#"
            query GetTest($issueId: String!) {
                getTest(issueId: $issueId) {
                    issueId
                    jira(fields: ["key"])
                    steps {
                        id
                        action
                        attachments {
                            id
                            filename
                            downloadLink
                        }
                    }
                }
            }
            "#;

const MUTATION_DESCRIPTION: &str = r#"Xray GraphQL mutation to create new test:
     Mutation createTest {
# Mutation used to create a new Test.
#
# Arguments
# testType: the Test Type of the Test.
# steps: the Step definition of the test.
# unstructured: the unstructured definition of the Test.
# gherkin: the gherkin definition of the Test.
# preconditionIssueIds: the Precondition ids that be associated with the Test.
# folderPath: the Test repository folder for the Test.
# jira: the Jira object that will be used to create the Test.
# Examples:
1. Create a new Test with type Manual:
mutation { createTest( testType: { name: "Manual" }, steps: [ { action: "Create first example step", result: "First step was created" }, { action: "Create second example step with data", data: "Data for the step", result: "Second step was created with data" } ], jira: { fields: { summary:"Exploratory Test", project: {key: "CALC"} } } ) { test { issueId testType { name } steps { id action data result } jira(fields: ["key"]) } warnings } }
createTest(testType: UpdateTestTypeInput, steps: [CreateStepInput], unstructured: String, gherkin: String, preconditionIssueIds: [String], folderPath: String, jira: JSON!): CreateTestResult
}
2. Create a new Test with type Generic:
mutation { createTest( testType: { name: "Generic" }, unstructured: "Perform exploratory tests on calculator.", jira: { fields: { summary:"Exploratory Test", project: {key: "CALC"} } } ) { test { issueId testType { name } unstructured jira(fields: ["key"]) } warnings } }
"#;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum XrayToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedSelection,
    Client,
    InvalidDefinition,
}

/// Stable construction failure for the Xray Cloud family.
pub(crate) struct XrayToolsetError {
    code: XrayToolsetErrorCode,
}

impl XrayToolsetError {
    #[must_use]
    pub(crate) const fn code(&self) -> XrayToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for XrayToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("XrayToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for XrayToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            XrayToolsetErrorCode::InvalidConfiguration => {
                "the Xray Cloud toolkit configuration is invalid"
            }
            XrayToolsetErrorCode::ResourceExhausted => {
                "the Xray Cloud toolkit configuration exceeds its approved limit"
            }
            XrayToolsetErrorCode::UnsupportedSelection => {
                "the selected Xray Cloud tool profile is not supported"
            }
            XrayToolsetErrorCode::Client => "the Xray Cloud client could not be created",
            XrayToolsetErrorCode::InvalidDefinition => {
                "the Xray Cloud ADK tool definition is invalid"
            }
        })
    }
}

impl std::error::Error for XrayToolsetError {}

impl From<XrayConfigError> for XrayToolsetError {
    fn from(source: XrayConfigError) -> Self {
        Self {
            code: match source.code() {
                XrayConfigErrorCode::InvalidConfiguration => {
                    XrayToolsetErrorCode::InvalidConfiguration
                }
                XrayConfigErrorCode::ResourceExhausted => XrayToolsetErrorCode::ResourceExhausted,
            },
        }
    }
}

impl From<XrayClientError> for XrayToolsetError {
    fn from(_: XrayClientError) -> Self {
        Self {
            code: XrayToolsetErrorCode::Client,
        }
    }
}

impl From<MaterializedToolsetError> for XrayToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: XrayToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// Build the six Xray Cloud business tools. The six inherited index tools
/// wait for indexing in Rust; a selection that names only those skips the
/// toolkit.
pub(crate) fn build_xray_cloud_toolset(
    toolkit_name: &str,
    config: XrayToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, XrayToolsetError> {
    let selected = served_selection(config.selected_tools())?;
    let instance = config.base_url().to_owned();
    let limit = config.limit();
    let client: Arc<dyn XrayApi> = Arc::new(XrayClient::new(config)?);
    build_with_api(toolkit_name, &instance, limit, &selected, policy, &client)
}

fn served_selection(selected: &[Box<str>]) -> Result<Vec<String>, XrayToolsetError> {
    let served = selected
        .iter()
        .filter(|name| {
            XrayToolKind::ALL
                .iter()
                .any(|kind| kind.name() == name.as_ref())
        })
        .map(ToString::to_string)
        .collect::<Vec<_>>();
    if !selected.is_empty() && served.is_empty() {
        return Err(XrayToolsetError {
            code: XrayToolsetErrorCode::UnsupportedSelection,
        });
    }
    Ok(served)
}

fn build_with_api(
    toolkit_name: &str,
    instance: &str,
    limit: u64,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn XrayApi>,
) -> Result<BasicToolset, XrayToolsetError> {
    let include_all = selected.is_empty();
    let mut tools: Vec<Arc<dyn Tool>> = Vec::with_capacity(XrayToolKind::ALL.len());
    for kind in XrayToolKind::ALL {
        if include_all || selected.iter().any(|name| name == kind.name()) {
            let description = format!(
                "Toolkit: {toolkit_name}\n{}\nXray instance: {instance}",
                kind.description()
            );
            tools.push(Arc::new(XrayTool {
                kind,
                limit,
                client: Arc::clone(client),
                description: description
                    .chars()
                    .take(MAX_DESCRIPTION_CHARS)
                    .collect::<String>()
                    .into_boxed_str(),
            }));
        }
    }
    admit_materialized_toolset(toolkit_name, "xray_cloud", policy, tools).map_err(Into::into)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_build_with_api(
    toolkit_name: &str,
    limit: u64,
    selected: &[&str],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn XrayApi>,
) -> Result<BasicToolset, XrayToolsetError> {
    let selected = served_selection(
        &selected
            .iter()
            .map(|name| (*name).into())
            .collect::<Vec<Box<str>>>(),
    )?;
    build_with_api(
        toolkit_name,
        super::config::DEFAULT_BASE_URL,
        limit,
        &selected,
        policy,
        client,
    )
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum XrayToolKind {
    GetTests,
    CreateTest,
    CreateTests,
    ExecuteGraphql,
    AddAttachmentToTestStep,
    GetTestStepAttachments,
}

impl XrayToolKind {
    const ALL: [Self; 6] = [
        Self::GetTests,
        Self::CreateTest,
        Self::CreateTests,
        Self::ExecuteGraphql,
        Self::AddAttachmentToTestStep,
        Self::GetTestStepAttachments,
    ];

    const fn name(self) -> &'static str {
        match self {
            Self::GetTests => "get_tests",
            Self::CreateTest => "create_test",
            Self::CreateTests => "create_tests",
            Self::ExecuteGraphql => "execute_graphql",
            Self::AddAttachmentToTestStep => "add_attachment_to_test_step",
            Self::GetTestStepAttachments => "get_test_step_attachments",
        }
    }

    const fn is_read_only(self) -> bool {
        matches!(self, Self::GetTests | Self::GetTestStepAttachments)
    }

    const fn description(self) -> &'static str {
        match self {
            Self::GetTests => "get all tests",
            Self::CreateTest => "Create new test in XRAY per defined XRAY graphql mutation",
            Self::CreateTests => "Create new tests in XRAY per defined XRAY graphql mutations",
            Self::ExecuteGraphql => "Executes custom graphql query or mutation",
            Self::AddAttachmentToTestStep => {
                "Add an attachment to an existing test step using GraphQL mutation. The attachment is uploaded to Xray and then associated with the test step. Pass the attachment as text in filedata with a filename. Artifact-storage filepaths are not readable as raw bytes in this runtime and are refused."
            }
            Self::GetTestStepAttachments => {
                "Get attachments for a test or specific test step. Retrieves attachment metadata including ID, filename, and download link. Can filter to a specific step or get all attachments for all steps."
            }
        }
    }
}

struct XrayTool {
    kind: XrayToolKind,
    limit: u64,
    client: Arc<dyn XrayApi>,
    description: Box<str>,
}

#[async_trait]
impl Tool for XrayTool {
    fn name(&self) -> &str {
        self.kind.name()
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn is_read_only(&self) -> bool {
        self.kind.is_read_only()
    }

    fn is_concurrency_safe(&self) -> bool {
        self.kind.is_read_only()
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(schema(self.kind))
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        if serde_json::to_vec(&arguments)
            .map_err(|_| invalid_arguments())?
            .len()
            > MAX_ARGUMENT_BYTES
        {
            return Err(invalid_arguments());
        }
        let arguments = arguments.as_object().ok_or_else(invalid_arguments)?;
        let schema = schema(self.kind);
        let allowed = schema["properties"]
            .as_object()
            .ok_or_else(invalid_arguments)?;
        if arguments.keys().any(|key| !allowed.contains_key(key)) {
            return Err(invalid_arguments());
        }
        let output = match self.kind {
            XrayToolKind::GetTests => {
                let jql = required_text(arguments, "jql", MAX_GRAPHQL_BYTES)?;
                let tests = self.tests(jql).await?;
                Value::String(format!(
                    "Extracted tests ({}):\n{}",
                    tests.len(),
                    repr(&Value::Array(tests))
                ))
            }
            XrayToolKind::CreateTest => {
                let mutation = required_text(arguments, "graphql_mutation", MAX_GRAPHQL_BYTES)?;
                Value::String(self.create_test(mutation).await?)
            }
            XrayToolKind::CreateTests => {
                let mutations = arguments
                    .get("graphql_mutations")
                    .and_then(Value::as_array)
                    .filter(|mutations| mutations.len() <= MAX_MUTATIONS)
                    .ok_or_else(invalid_arguments)?
                    .iter()
                    .map(|mutation| {
                        mutation
                            .as_str()
                            .filter(|mutation| mutation.len() <= MAX_GRAPHQL_BYTES)
                            .ok_or_else(invalid_arguments)
                    })
                    .collect::<Result<Vec<_>, _>>()?;
                let mut created = Vec::with_capacity(mutations.len());
                for mutation in mutations {
                    created.push(Value::String(self.create_test(mutation).await?));
                }
                Value::Array(created)
            }
            XrayToolKind::ExecuteGraphql => {
                let graphql = required_text(arguments, "graphql", MAX_GRAPHQL_BYTES)?;
                let document = self.graphql(graphql, None, true).await?;
                Value::String(format!(
                    "Result of graphql execution:\n{}",
                    repr(&Value::Object(document))
                ))
            }
            XrayToolKind::AddAttachmentToTestStep => self.add_attachment(arguments).await?,
            XrayToolKind::GetTestStepAttachments => self.attachments(arguments).await?,
        };
        let size = match &output {
            Value::String(text) => text.len(),
            other => serde_json::to_vec(other).map_or(usize::MAX, |bytes| bytes.len()),
        };
        if size > MAX_OUTPUT_BYTES {
            return Err(resource_exhausted().into_adk());
        }
        Ok(output)
    }
}

impl XrayTool {
    async fn graphql(
        &self,
        query: &str,
        variables: Option<Value>,
        effect: bool,
    ) -> Result<Map<String, Value>, AdkError> {
        self.client
            .graphql(query, variables, effect)
            .await
            .map_err(XrayClientError::into_adk)
    }

    async fn create_test(&self, mutation: &str) -> Result<String, AdkError> {
        let document = self.graphql(mutation, None, true).await?;
        Ok(format!(
            "Created test case:\n{}",
            repr(&Value::Object(document))
        ))
    }

    /// `get_tests`: `getTests` pages of the configured `limit` from start 0
    /// until the collected count equals `total`; a test whose preconditions
    /// total is zero loses the `preconditions` member.
    async fn tests(&self, jql: &str) -> Result<Vec<Value>, AdkError> {
        let mut tests = Vec::new();
        let mut start = 0;
        for _ in 0..MAX_TEST_PAGES {
            let variables = json!({"jql": jql, "start": start, "limit": self.limit});
            let document = self
                .graphql(GET_TESTS_QUERY, Some(variables), false)
                .await?;
            let page = document
                .get("data")
                .and_then(Value::as_object)
                .ok_or_else(|| invalid_response().into_adk())?
                .get("getTests")
                .and_then(Value::as_object)
                // `getTests` is null when the JQL is invalid.
                .ok_or_else(invalid_jql)?;
            let total = page
                .get("total")
                .and_then(Value::as_u64)
                .ok_or_else(|| invalid_response().into_adk())?;
            let results = page
                .get("results")
                .and_then(Value::as_array)
                .ok_or_else(|| invalid_response().into_adk())?;
            let returned = results.len();
            tests.extend(results.iter().cloned().map(without_empty_preconditions));
            if tests.len() as u64 == total || returned == 0 {
                return Ok(tests);
            }
            start += self.limit;
        }
        Err(resource_exhausted().into_adk())
    }

    async fn add_attachment(&self, arguments: &Map<String, Value>) -> Result<Value, AdkError> {
        let step_id = optional_text(arguments, "step_id")?.unwrap_or_default();
        let filepath = optional_text(arguments, "filepath")?;
        let filedata = optional_text(arguments, "filedata")?;
        let filename = optional_text(arguments, "filename")?;
        // The SDK raises these as ToolException text the model reads.
        let refusal = match (filepath, filedata) {
            (None, None) => Some("Either filepath or filedata must be provided".to_owned()),
            (Some(_), Some(_)) => Some("Cannot specify both filepath and filedata".to_owned()),
            (None, Some(_)) if filename.is_none() => {
                Some("filename is required when using filedata".to_owned())
            }
            _ if step_id.len() < 10 => Some(format!(
                "Invalid step_id '{step_id}'. Step ID must be a UUID (e.g., 'a1b2c3d4-...'), not a test issue key. Use get_tests tool to retrieve step IDs from test details."
            )),
            (Some(_), None) => Some(
                "filepath attachments are not available in this runtime: artifact storage is not readable as raw bytes here. Pass the attachment content as filedata with a filename instead.".to_owned(),
            ),
            _ => None,
        };
        if let Some(refusal) = refusal {
            return Ok(Value::String(refusal));
        }
        let (Some(filedata), Some(filename)) = (filedata, filename) else {
            return Err(invalid_arguments());
        };
        if filedata.len() > MAX_FILEDATA_BYTES {
            return Err(invalid_arguments());
        }
        let mime_type = mime_type(filename);
        let variables = json!({
            "stepId": step_id,
            "step": {"attachments": {"add": [{
                "filename": filename,
                "mimeType": mime_type,
                "data": BASE64_STANDARD.encode(filedata.as_bytes()),
            }]}}
        });
        let document = self
            .graphql(UPDATE_TEST_STEP_MUTATION, Some(variables), true)
            .await?;
        if let Some(errors) = document.get("errors") {
            return Ok(Value::String(format!("GraphQL errors: {}", repr(errors))));
        }
        let mut message = format!(
            "Successfully added attachment '{filename}' to step {step_id}\nFile size: {} bytes\nMIME type: {mime_type}",
            filedata.len()
        );
        let warnings = document
            .get("data")
            .and_then(|data| data.get("updateTestStep"))
            .and_then(|result| result.get("warnings"))
            .filter(|warnings| truthy(warnings));
        if let Some(warnings) = warnings {
            let _ = write!(message, "\nWarnings: {}", repr(warnings));
        }
        Ok(Value::String(message))
    }

    async fn attachments(&self, arguments: &Map<String, Value>) -> Result<Value, AdkError> {
        let issue_id = required_text(arguments, "issue_id", 256)?;
        let step_id = optional_text(arguments, "step_id")?;
        let numeric_id = if issue_id.contains('-') {
            // The key goes into JQL; only a Jira key shape is admitted.
            if !issue_id
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_'))
            {
                return Err(invalid_arguments());
            }
            let variables = json!({"jql": format!("key = \"{issue_id}\""), "start": 0, "limit": 1});
            let document = self
                .graphql(GET_TESTS_QUERY, Some(variables), false)
                .await?;
            let found = document
                .get("data")
                .and_then(|data| data.get("getTests"))
                .and_then(|tests| tests.get("results"))
                .and_then(Value::as_array)
                .and_then(|results| results.first())
                .cloned();
            let Some(found) = found else {
                return Ok(Value::String(format!(
                    "Test not found with key: {issue_id}"
                )));
            };
            match found.get("issueId") {
                Some(Value::String(id)) if !id.is_empty() => Value::String(id.clone()),
                Some(Value::Number(id)) => Value::Number(id.clone()),
                _ => {
                    return Ok(Value::String(format!(
                        "Could not retrieve numeric issue ID for key: {issue_id}"
                    )));
                }
            }
        } else {
            Value::String(issue_id.to_owned())
        };
        let document = self
            .graphql(
                GET_TEST_QUERY,
                Some(json!({"issueId": numeric_id.clone()})),
                false,
            )
            .await?;
        if let Some(errors) = document.get("errors") {
            return Ok(Value::String(format!("GraphQL errors: {}", repr(errors))));
        }
        let Some(test) = document
            .get("data")
            .and_then(|data| data.get("getTest"))
            .filter(|test| truthy(test))
        else {
            return Ok(Value::String(format!("Test not found with ID: {issue_id}")));
        };
        let key = test
            .get("jira")
            .and_then(|jira| jira.get("key"))
            .cloned()
            .unwrap_or_else(|| Value::String(issue_id.to_owned()));
        let attachments = collect_attachments(test, step_id)?;
        let key_text = match &key {
            Value::String(key) => key.clone(),
            other => repr(other),
        };
        if attachments.is_empty() {
            return Ok(Value::String(match step_id {
                Some(step) => format!("No attachments found for test {key_text}, step {step}"),
                None => format!("No attachments found for test {key_text}"),
            }));
        }
        let total = Value::from(attachments.len());
        let attachments = Pretty::Array(attachments);
        Ok(Value::String(
            Pretty::Object(vec![
                ("test", Pretty::Scalar(key)),
                ("issue_id", Pretty::Scalar(numeric_id)),
                ("total_attachments", Pretty::Scalar(total)),
                ("attachments", attachments),
            ])
            .dumps(),
        ))
    }
}

/// Every step attachment with its step context, optionally for one step.
fn collect_attachments(test: &Value, step_id: Option<&str>) -> Result<Vec<Pretty>, AdkError> {
    let mut attachments = Vec::new();
    let steps = test
        .get("steps")
        .and_then(Value::as_array)
        .map_or(&[][..], Vec::as_slice);
    for step in steps {
        let Some(items) = step.get("attachments").and_then(Value::as_array) else {
            continue;
        };
        let step_ref = step.get("id").cloned().unwrap_or(Value::Null);
        if step_id.is_some_and(|wanted| step_ref.as_str() != Some(wanted)) {
            continue;
        }
        for attachment in items {
            let member = |name: &str| {
                attachment
                    .get(name)
                    .cloned()
                    .ok_or_else(|| invalid_response().into_adk())
            };
            attachments.push(Pretty::Object(vec![
                ("id", Pretty::Scalar(member("id")?)),
                ("filename", Pretty::Scalar(member("filename")?)),
                ("downloadLink", Pretty::Scalar(member("downloadLink")?)),
                ("step_id", Pretty::Scalar(step_ref.clone())),
                (
                    "step_action",
                    Pretty::Scalar(
                        step.get("action")
                            .cloned()
                            .unwrap_or_else(|| Value::String(String::new())),
                    ),
                ),
            ]));
        }
    }
    Ok(attachments)
}

/// An ordered JSON tree, rendered as `json.dumps(value, indent=2)` with its
/// default `ensure_ascii=True`.
enum Pretty {
    Scalar(Value),
    Array(Vec<Pretty>),
    Object(Vec<(&'static str, Pretty)>),
}

impl Pretty {
    fn dumps(&self) -> String {
        let mut output = String::new();
        self.write(&mut output, 0);
        output
    }

    fn write(&self, output: &mut String, depth: usize) {
        let indent = |output: &mut String, depth: usize| {
            output.push('\n');
            output.push_str(&"  ".repeat(depth));
        };
        match self {
            Self::Scalar(value) => output.push_str(&ascii_json(value)),
            Self::Array(items) if items.is_empty() => output.push_str("[]"),
            Self::Array(items) => {
                output.push('[');
                for (index, item) in items.iter().enumerate() {
                    if index > 0 {
                        output.push(',');
                    }
                    indent(output, depth + 1);
                    item.write(output, depth + 1);
                }
                indent(output, depth);
                output.push(']');
            }
            Self::Object(members) => {
                output.push('{');
                for (index, (key, value)) in members.iter().enumerate() {
                    if index > 0 {
                        output.push(',');
                    }
                    indent(output, depth + 1);
                    output.push_str(&ascii_json(&Value::String((*key).to_owned())));
                    output.push_str(": ");
                    value.write(output, depth + 1);
                }
                indent(output, depth);
                output.push('}');
            }
        }
    }
}

/// Compact JSON with every non-ASCII character escaped, as Python's
/// `ensure_ascii` writes it.
fn ascii_json(value: &Value) -> String {
    let compact = serde_json::to_string(value).unwrap_or_else(|_| "null".to_owned());
    let mut output = String::with_capacity(compact.len());
    for character in compact.chars() {
        if character.is_ascii() {
            output.push(character);
        } else {
            let mut units = [0_u16; 2];
            for unit in character.encode_utf16(&mut units) {
                let _ = write!(output, "\\u{unit:04x}");
            }
        }
    }
    output
}

fn without_empty_preconditions(mut test: Value) -> Value {
    if let Some(object) = test.as_object_mut()
        && object
            .get("preconditions")
            .and_then(|preconditions| preconditions.get("total"))
            .and_then(Value::as_u64)
            == Some(0)
    {
        object.remove("preconditions");
    }
    test
}

/// Python truthiness for a JSON value.
fn truthy(value: &Value) -> bool {
    match value {
        Value::Null => false,
        Value::Bool(flag) => *flag,
        Value::Number(number) => number.as_f64().is_some_and(|number| number != 0.0),
        Value::String(text) => !text.is_empty(),
        Value::Array(items) => !items.is_empty(),
        Value::Object(object) => !object.is_empty(),
    }
}

/// `detect_mime_type`'s extension fallback; text supplied as `filedata`
/// carries no binary signature for its `filetype.guess` step to match.
fn mime_type(filename: &str) -> &'static str {
    let lower = filename.to_ascii_lowercase();
    let table: [(&[&str], &str); 21] = [
        (&[".png"], "image/png"),
        (&[".jpg", ".jpeg"], "image/jpeg"),
        (&[".gif"], "image/gif"),
        (&[".bmp"], "image/bmp"),
        (&[".webp"], "image/webp"),
        (&[".svg"], "image/svg+xml"),
        (&[".pdf"], "application/pdf"),
        (&[".doc", ".docx"], "application/msword"),
        (&[".xls", ".xlsx"], "application/vnd.ms-excel"),
        (&[".ppt", ".pptx"], "application/vnd.ms-powerpoint"),
        (&[".txt"], "text/plain"),
        (&[".csv"], "text/csv"),
        (&[".html"], "text/html"),
        (&[".json"], "application/json"),
        (&[".xml"], "application/xml"),
        (&[".zip"], "application/zip"),
        (&[".tar"], "application/x-tar"),
        (&[".gz"], "application/gzip"),
        (&[".mp4", ".m4v"], "video/mp4"),
        (&[".avi"], "video/x-msvideo"),
        (&[".mov"], "video/quicktime"),
    ];
    table
        .iter()
        .find(|(suffixes, _)| suffixes.iter().any(|suffix| lower.ends_with(suffix)))
        .map_or("application/octet-stream", |(_, mime)| mime)
}

fn schema(kind: XrayToolKind) -> Value {
    let text = |description: &str| json!({"type":"string","minLength":1,"maxLength":MAX_GRAPHQL_BYTES,"description":description});
    let optional = |description: &str| json!({"type":["string","null"],"default":null,"description":description});
    let (properties, required): (Value, &[&str]) = match kind {
        XrayToolKind::GetTests => (
            json!({"jql":text("the jql that defines the search")}),
            &["jql"],
        ),
        XrayToolKind::CreateTest => (
            json!({"graphql_mutation":text(MUTATION_DESCRIPTION)}),
            &["graphql_mutation"],
        ),
        XrayToolKind::CreateTests => (
            json!({"graphql_mutations":{
                "type":"array", "items":{"type":"string","maxLength":MAX_GRAPHQL_BYTES},
                "maxItems":MAX_MUTATIONS,
                "description":format!("list of GraphQL mutations:\n{MUTATION_DESCRIPTION}")
            }}),
            &["graphql_mutations"],
        ),
        XrayToolKind::ExecuteGraphql => (
            json!({"graphql":text("Custom XRAY GraphQL query for execution")}),
            &["graphql"],
        ),
        XrayToolKind::AddAttachmentToTestStep => (
            json!({
                "step_id":text("The ID of the test step to add the attachment to"),
                "filepath":optional("File path in format /{bucket}/{filename} from artifact storage. Not readable as raw bytes in this runtime: use filedata."),
                "filedata":optional("String content to attach as a file. Either filepath or filedata must be provided."),
                "filename":optional("Attachment filename. Required when using filedata, optional when using filepath (uses original filename if not specified).")
            }),
            &["step_id"],
        ),
        XrayToolKind::GetTestStepAttachments => (
            json!({
                "issue_id":text("The test issue ID. Accepts either Jira key (e.g., 'PROJ-123') or numeric issue ID (e.g., '12345')"),
                "step_id":optional("Optional: filter to specific step ID. If not provided, returns attachments for all steps in the test.")
            }),
            &["issue_id"],
        ),
    };
    json!({
        "type":"object",
        "properties":properties,
        "required":required,
        "additionalProperties":false
    })
}

fn required_text<'a>(
    arguments: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> Result<&'a str, AdkError> {
    arguments
        .get(name)
        .and_then(Value::as_str)
        .filter(|text| !text.trim().is_empty() && text.len() <= limit)
        .ok_or_else(invalid_arguments)
}

fn optional_text<'a>(
    arguments: &'a Map<String, Value>,
    name: &str,
) -> Result<Option<&'a str>, AdkError> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(text)) if text.is_empty() => Ok(None),
        Some(Value::String(text)) if text.len() <= MAX_FILEDATA_BYTES => Ok(Some(text)),
        Some(_) => Err(invalid_arguments()),
    }
}

fn invalid_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "xray.arguments.invalid",
        "the Xray Cloud tool arguments are invalid",
    )
}

fn invalid_jql() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "xray.jql.invalid",
        "Unable to get tests. The JQL query may be invalid or malformed. Please verify the JQL syntax and try again.",
    )
}
