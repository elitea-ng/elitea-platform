use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value};
use tokio::sync::Mutex;

use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{QtestApi, QtestCall, QtestClient, QtestClientError};
use super::config::{QtestConfigError, QtestConfigErrorCode, QtestToolkitConfig};
use super::fields::FieldDefinitions;
use super::schema::{QtestToolKind, schema};

const MAX_OUTPUT_BYTES: usize = 512 * 1_024;
const MAX_ARGUMENT_BYTES: usize = 512 * 1_024;
const MAX_DESCRIPTION_CHARS: usize = 1_000;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum QtestToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedSelection,
    Client,
    InvalidDefinition,
}

/// Stable construction failure for the qTest family.
pub(crate) struct QtestToolsetError {
    code: QtestToolsetErrorCode,
}

impl QtestToolsetError {
    #[must_use]
    pub(crate) const fn code(&self) -> QtestToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for QtestToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("QtestToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for QtestToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            QtestToolsetErrorCode::InvalidConfiguration => {
                "the qTest toolkit configuration is invalid"
            }
            QtestToolsetErrorCode::ResourceExhausted => {
                "the qTest toolkit configuration exceeds its approved limit"
            }
            QtestToolsetErrorCode::UnsupportedSelection => {
                "the selected qTest tool profile is not supported"
            }
            QtestToolsetErrorCode::Client => "the qTest client could not be created",
            QtestToolsetErrorCode::InvalidDefinition => "the qTest ADK tool definition is invalid",
        })
    }
}

impl std::error::Error for QtestToolsetError {}

impl From<QtestConfigError> for QtestToolsetError {
    fn from(source: QtestConfigError) -> Self {
        Self {
            code: match source.code() {
                QtestConfigErrorCode::InvalidConfiguration => {
                    QtestToolsetErrorCode::InvalidConfiguration
                }
                QtestConfigErrorCode::ResourceExhausted => QtestToolsetErrorCode::ResourceExhausted,
            },
        }
    }
}

impl From<QtestClientError> for QtestToolsetError {
    fn from(_: QtestClientError) -> Self {
        Self {
            code: QtestToolsetErrorCode::Client,
        }
    }
}

impl From<MaterializedToolsetError> for QtestToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: QtestToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// How an operation ends without a result.
///
/// `Message` is the text of an exception the SDK raises from its own checks
/// (an unknown field, a test run that does not exist); the model reads it as
/// the tool's answer, as it reads the SDK's. The admission wrapper replaces
/// any error's text, so it is returned as output. `Adk` is a provider or
/// argument failure.
pub(super) enum Failure {
    Message(String),
    Adk(AdkError),
}

impl From<AdkError> for Failure {
    fn from(error: AdkError) -> Self {
        Self::Adk(error)
    }
}

/// The per-invocation qTest state every tool of one toolset shares: the
/// client plus the SDK's session caches (field definitions, modules).
pub(super) struct Qtest {
    pub(super) client: Arc<dyn QtestApi>,
    pub(super) project_id: u64,
    pub(super) shown_results: usize,
    pub(super) fields: Mutex<Option<FieldDefinitions>>,
    pub(super) modules: Mutex<Option<Vec<(Value, String)>>>,
}

impl Qtest {
    pub(super) async fn call(&self, call: QtestCall<'_>) -> Result<Value, AdkError> {
        self.client
            .call(call)
            .await
            .map_err(QtestClientError::into_adk)
    }
}

/// Build the seventeen served qTest tools.
///
/// `add_file_to_test_case` and `upload_attachment_to_test_run` upload raw
/// bytes from artifact storage, which this runtime's artifact authority does
/// not expose; the six index tools wait for indexing in Rust. A selection
/// that names none of the served tools skips the toolkit.
pub(crate) fn build_qtest_toolset(
    toolkit_name: &str,
    config: QtestToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, QtestToolsetError> {
    let selected = served_selection(config.selected_tools())?;
    let base_url = config.base_url().to_owned();
    let project_id = config.project_id();
    let shown_results = config.shown_results();
    let client: Arc<dyn QtestApi> = Arc::new(QtestClient::new(config)?);
    build_with_api(
        toolkit_name,
        &base_url,
        project_id,
        shown_results,
        &selected,
        policy,
        client,
    )
}

fn served_selection(selected: &[Box<str>]) -> Result<Vec<String>, QtestToolsetError> {
    let served = selected
        .iter()
        .filter(|name| {
            QtestToolKind::ALL
                .iter()
                .any(|kind| kind.name() == name.as_ref())
        })
        .map(ToString::to_string)
        .collect::<Vec<_>>();
    if !selected.is_empty() && served.is_empty() {
        return Err(QtestToolsetError {
            code: QtestToolsetErrorCode::UnsupportedSelection,
        });
    }
    Ok(served)
}

fn build_with_api(
    toolkit_name: &str,
    base_url: &str,
    project_id: u64,
    shown_results: usize,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: Arc<dyn QtestApi>,
) -> Result<BasicToolset, QtestToolsetError> {
    let state = Arc::new(Qtest {
        client,
        project_id,
        shown_results,
        fields: Mutex::new(None),
        modules: Mutex::new(None),
    });
    let include_all = selected.is_empty();
    let mut tools: Vec<Arc<dyn Tool>> = Vec::with_capacity(QtestToolKind::ALL.len());
    for kind in QtestToolKind::ALL {
        if include_all || selected.iter().any(|name| name == kind.name()) {
            // `f"{description}\nUrl: {base_url}. Project id: {id}"`, then the
            // toolkit, then the 1000-character cut.
            let description = format!(
                "{}\nUrl: {base_url}. Project id: {project_id}\nToolkit: {toolkit_name}",
                kind.description()
            );
            tools.push(Arc::new(QtestTool {
                kind,
                state: Arc::clone(&state),
                description: description
                    .chars()
                    .take(MAX_DESCRIPTION_CHARS)
                    .collect::<String>()
                    .into_boxed_str(),
            }));
        }
    }
    admit_materialized_toolset(toolkit_name, "qtest", policy, tools).map_err(Into::into)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_build_with_api(
    toolkit_name: &str,
    project_id: u64,
    selected: &[&str],
    policy: &Arc<ToolAdmissionPolicy>,
    client: Arc<dyn QtestApi>,
) -> Result<BasicToolset, QtestToolsetError> {
    let selected = served_selection(
        &selected
            .iter()
            .map(|name| (*name).into())
            .collect::<Vec<Box<str>>>(),
    )?;
    build_with_api(
        toolkit_name,
        "https://qtest.example.test",
        project_id,
        10,
        &selected,
        policy,
        client,
    )
}

struct QtestTool {
    kind: QtestToolKind,
    state: Arc<Qtest>,
    description: Box<str>,
}

#[async_trait]
impl Tool for QtestTool {
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
        let output = match self.state.run(self.kind, arguments).await {
            Ok(output) => output,
            Err(Failure::Message(message)) => Value::String(message),
            Err(Failure::Adk(error)) => return Err(error),
        };
        let size = match &output {
            Value::String(text) => text.len(),
            other => serde_json::to_vec(other).map_or(usize::MAX, |bytes| bytes.len()),
        };
        if size > MAX_OUTPUT_BYTES {
            return Err(super::client::resource_exhausted(false).into_adk());
        }
        Ok(output)
    }
}

impl Qtest {
    async fn run(
        &self,
        kind: QtestToolKind,
        arguments: &Map<String, Value>,
    ) -> Result<Value, Failure> {
        match kind {
            QtestToolKind::SearchByDql
            | QtestToolKind::FindTestCaseById
            | QtestToolKind::GetModules
            | QtestToolKind::GetAllTestCasesFieldsForProject
            | QtestToolKind::SearchEntitiesByDql
            | QtestToolKind::FindEntityById => self.read(kind, arguments).await,
            QtestToolKind::FindTestCasesByRequirementId
            | QtestToolKind::FindRequirementsByTestCaseId
            | QtestToolKind::FindTestRunsByTestCaseId
            | QtestToolKind::FindDefectsByTestRunId
            | QtestToolKind::GetTestCaseVersions => self.relation(kind, arguments).await,
            _ => self.effect(kind, arguments).await,
        }
    }
}

pub(super) fn required_text<'a>(
    arguments: &'a Map<String, Value>,
    name: &str,
) -> Result<&'a str, Failure> {
    arguments
        .get(name)
        .and_then(Value::as_str)
        .filter(|text| !text.is_empty())
        .ok_or_else(|| Failure::Adk(invalid_arguments()))
}

pub(super) fn optional_text<'a>(
    arguments: &'a Map<String, Value>,
    name: &str,
) -> Result<Option<&'a str>, Failure> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(text)) => Ok(Some(text)),
        Some(_) => Err(Failure::Adk(invalid_arguments())),
    }
}

pub(super) fn optional_bool(arguments: &Map<String, Value>, name: &str) -> Result<bool, Failure> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(false),
        Some(Value::Bool(flag)) => Ok(*flag),
        Some(_) => Err(Failure::Adk(invalid_arguments())),
    }
}

pub(super) fn optional_integer(
    arguments: &Map<String, Value>,
    name: &str,
) -> Result<Option<i64>, Failure> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::Number(number)) => number
            .as_i64()
            .map(Some)
            .ok_or_else(|| Failure::Adk(invalid_arguments())),
        Some(_) => Err(Failure::Adk(invalid_arguments())),
    }
}

pub(super) fn invalid_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "qtest.arguments.invalid",
        "the qTest tool arguments are invalid",
    )
}
