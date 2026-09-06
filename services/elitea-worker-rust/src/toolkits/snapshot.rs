use std::collections::HashSet;
use std::fmt;

use serde_json::{Map, Value};

use crate::agents::request::{AgentExecutionKind, AgentExecutionRequest};

use super::policy::{ToolAdmissionDecision, ToolAdmissionPolicy};

const MAX_FROZEN_TOOL_REFERENCES: usize = 1_024;
const MAX_TOOL_IDENTIFIER_BYTES: usize = 1_024;
const MAX_SELECTED_TOOLS: usize = 1_024;

/// Stable, data-free failure categories for a frozen tool snapshot.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum FrozenToolSnapshotErrorCode {
    InvalidInput,
    ResourceExhausted,
}

/// A malformed or over-limit tool snapshot.
///
/// Raw settings can contain secret references and endpoint metadata. Debug and
/// display output therefore expose only a stable category and message.
#[derive(Clone, Copy)]
pub(crate) struct FrozenToolSnapshotError {
    code: FrozenToolSnapshotErrorCode,
    message: &'static str,
}

impl FrozenToolSnapshotError {
    #[must_use]
    pub(crate) const fn code(self) -> FrozenToolSnapshotErrorCode {
        self.code
    }
}

impl fmt::Debug for FrozenToolSnapshotError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("FrozenToolSnapshotError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for FrozenToolSnapshotError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.message)
    }
}

impl std::error::Error for FrozenToolSnapshotError {}

/// The materialization family selected by Main's frozen toolkit type.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum FrozenToolKind {
    Configured,
    Mcp,
    Application,
}

/// A validated view over the immutable tool documents owned by one request.
///
/// This view borrows settings rather than copying them. It has no `Debug` or
/// `Clone` implementation and cannot outlive the admitted request.
pub(crate) struct FrozenToolSnapshot<'a> {
    references: Vec<FrozenToolReference<'a>>,
}

/// Frozen references after whole-toolkit deployment policy is applied.
///
/// Specific tool-name restrictions are evaluated again after materialization,
/// when concrete ADK tool identities exist.
pub(crate) struct AdmittedToolSnapshot<'a> {
    references: Vec<FrozenToolReference<'a>>,
}

impl<'a> FrozenToolSnapshot<'a> {
    /// Validate one exact toolkit frozen for direct external-MCP execution.
    /// Nested applications are a separate external capability and cannot be
    /// smuggled through the direct toolkit command.
    pub(crate) fn from_toolkit(toolkit: &'a Value) -> Result<Self, FrozenToolSnapshotError> {
        let reference = parse_reference(toolkit)?;
        if reference.kind() == FrozenToolKind::Application {
            return Err(invalid_input());
        }
        Ok(Self {
            references: vec![reference],
        })
    }

    /// Select and validate the authoritative tool list for this request kind.
    ///
    /// Application turns use `application.version_details.tools`; ad-hoc turns
    /// use the top-level `tools` list. Supplying tools through both paths fails
    /// closed rather than creating an ambiguous materialization source.
    pub(crate) fn from_request(
        request: &'a AgentExecutionRequest,
    ) -> Result<Self, FrozenToolSnapshotError> {
        let source = authoritative_tools(request)?;
        if source.len() > MAX_FROZEN_TOOL_REFERENCES {
            return Err(resource_exhausted());
        }

        let mut toolkit_ids = HashSet::with_capacity(source.len());
        let mut references = Vec::with_capacity(source.len());
        for value in source {
            let reference = parse_reference(value)?;
            if reference
                .tool_id()
                .is_some_and(|tool_id| !toolkit_ids.insert(tool_id))
            {
                continue;
            }
            references.push(reference);
        }
        Ok(Self { references })
    }

    /// Validate a nested application's already-frozen tool list.
    pub(crate) fn from_version_details(
        version: &'a Map<String, Value>,
    ) -> Result<Self, FrozenToolSnapshotError> {
        let source = version
            .get("tools")
            .and_then(Value::as_array)
            .ok_or_else(invalid_input)?;
        if source.len() > MAX_FROZEN_TOOL_REFERENCES {
            return Err(resource_exhausted());
        }
        let mut toolkit_ids = HashSet::with_capacity(source.len());
        let mut references = Vec::with_capacity(source.len());
        for value in source {
            let reference = parse_reference(value)?;
            if reference
                .tool_id()
                .is_some_and(|tool_id| !toolkit_ids.insert(tool_id))
            {
                continue;
            }
            references.push(reference);
        }
        Ok(Self { references })
    }

    #[must_use]
    pub(crate) fn len(&self) -> usize {
        self.references.len()
    }

    #[must_use]
    pub(crate) fn is_empty(&self) -> bool {
        self.references.is_empty()
    }

    /// Remove blocked toolkit families before credentials or clients exist.
    #[must_use]
    pub(crate) fn apply_policy(mut self, policy: &ToolAdmissionPolicy) -> AdmittedToolSnapshot<'a> {
        self.references.retain(|reference| {
            policy.toolkit_decision(reference.tool_type()) == ToolAdmissionDecision::Allowed
        });
        AdmittedToolSnapshot {
            references: self.references,
        }
    }

    pub(crate) fn iter(&self) -> impl ExactSizeIterator<Item = &FrozenToolReference<'a>> {
        self.references.iter()
    }
}

impl<'a> AdmittedToolSnapshot<'a> {
    #[must_use]
    pub(crate) fn len(&self) -> usize {
        self.references.len()
    }

    #[must_use]
    pub(crate) fn is_empty(&self) -> bool {
        self.references.is_empty()
    }

    pub(crate) fn iter(&self) -> impl ExactSizeIterator<Item = &FrozenToolReference<'a>> {
        self.references.iter()
    }

    /// Retain only toolkit aliases selected by an admitted pipeline node.
    ///
    /// This is an authority reduction: unconnected toolkit credentials never
    /// reach family materialization merely because they exist on the frozen
    /// application version.
    #[must_use]
    pub(crate) fn retain_toolkit_names(
        mut self,
        aliases: &std::collections::BTreeSet<String>,
    ) -> Self {
        self.references
            .retain(|reference| aliases.contains(reference.toolkit_name()));
        self
    }
}

/// One frozen configured, MCP, or nested-application reference.
pub(crate) enum FrozenToolReference<'a> {
    Configured(FrozenConfiguredToolReference<'a>),
    Mcp(FrozenConfiguredToolReference<'a>),
    Application(FrozenApplicationReference<'a>),
}

impl FrozenToolReference<'_> {
    #[must_use]
    pub(crate) const fn kind(&self) -> FrozenToolKind {
        match self {
            Self::Configured(_) => FrozenToolKind::Configured,
            Self::Mcp(_) => FrozenToolKind::Mcp,
            Self::Application(_) => FrozenToolKind::Application,
        }
    }

    #[must_use]
    pub(crate) const fn tool_id(&self) -> Option<u64> {
        match self {
            Self::Configured(reference) | Self::Mcp(reference) => Some(reference.tool_id),
            Self::Application(reference) => reference.tool_id,
        }
    }

    #[must_use]
    pub(crate) const fn tool_type(&self) -> &str {
        match self {
            Self::Configured(reference) | Self::Mcp(reference) => reference.tool_type,
            Self::Application(_) => "application",
        }
    }

    #[must_use]
    pub(crate) const fn toolkit_name(&self) -> &str {
        match self {
            Self::Configured(reference) | Self::Mcp(reference) => reference.toolkit_name,
            Self::Application(reference) => reference.toolkit_name,
        }
    }

    #[must_use]
    pub(crate) const fn settings(&self) -> Option<&Map<String, Value>> {
        match self {
            Self::Configured(reference) | Self::Mcp(reference) => Some(reference.settings),
            Self::Application(_) => None,
        }
    }

    #[must_use]
    pub(crate) const fn application_identity(&self) -> Option<(u64, u64)> {
        match self {
            Self::Application(reference) => {
                Some((reference.application_id, reference.application_version_id))
            }
            Self::Configured(_) | Self::Mcp(_) => None,
        }
    }

    #[must_use]
    pub(crate) const fn application_description(&self) -> Option<&str> {
        match self {
            Self::Application(reference) => reference.description,
            Self::Configured(_) | Self::Mcp(_) => None,
        }
    }

    #[must_use]
    pub(crate) const fn application_agent_type(&self) -> Option<&str> {
        match self {
            Self::Application(reference) => Some(reference.agent_type),
            Self::Configured(_) | Self::Mcp(_) => None,
        }
    }

    #[must_use]
    pub(crate) const fn application_project_id(&self) -> Option<u64> {
        match self {
            Self::Application(reference) => reference.project_id,
            Self::Configured(_) | Self::Mcp(_) => None,
        }
    }
}

pub(crate) struct FrozenConfiguredToolReference<'a> {
    tool_id: u64,
    tool_type: &'a str,
    toolkit_name: &'a str,
    settings: &'a Map<String, Value>,
}

pub(crate) struct FrozenApplicationReference<'a> {
    tool_id: Option<u64>,
    toolkit_name: &'a str,
    description: Option<&'a str>,
    agent_type: &'a str,
    application_id: u64,
    application_version_id: u64,
    participant_id: Option<u64>,
    project_id: Option<u64>,
}

fn authoritative_tools(
    request: &AgentExecutionRequest,
) -> Result<&[Value], FrozenToolSnapshotError> {
    match request.kind {
        AgentExecutionKind::Application => {
            if !request.payload.tools.is_empty() {
                return Err(invalid_input());
            }
            request
                .payload
                .application
                .get("version_details")
                .and_then(Value::as_object)
                .and_then(|version| version.get("tools"))
                .and_then(Value::as_array)
                .map(Vec::as_slice)
                .ok_or_else(invalid_input)
        }
        AgentExecutionKind::Adhoc => {
            let nested_tools = request
                .payload
                .application
                .get("version_details")
                .and_then(Value::as_object)
                .and_then(|version| version.get("tools"))
                .and_then(Value::as_array);
            if nested_tools.is_some_and(|tools| !tools.is_empty()) {
                return Err(invalid_input());
            }
            Ok(&request.payload.tools)
        }
    }
}

fn parse_reference(value: &Value) -> Result<FrozenToolReference<'_>, FrozenToolSnapshotError> {
    let tool = value.as_object().ok_or_else(invalid_input)?;
    let tool_type = required_identifier(tool, "type")?;
    if tool_type == "application" {
        return parse_application_reference(tool).map(FrozenToolReference::Application);
    }

    let tool_id = positive_integer(tool.get("id")).ok_or_else(invalid_input)?;
    let toolkit_name = required_identifier(tool, "toolkit_name")?;
    let settings = tool
        .get("settings")
        .and_then(Value::as_object)
        .ok_or_else(invalid_input)?;
    validate_optional_tool_names(settings, "selected_tools")?;
    validate_optional_tool_names(settings, "excluded_tools")?;
    let reference = FrozenConfiguredToolReference {
        tool_id,
        tool_type,
        toolkit_name,
        settings,
    };
    if is_mcp_type(tool_type) {
        Ok(FrozenToolReference::Mcp(reference))
    } else {
        Ok(FrozenToolReference::Configured(reference))
    }
}

fn parse_application_reference(
    tool: &Map<String, Value>,
) -> Result<FrozenApplicationReference<'_>, FrozenToolSnapshotError> {
    match tool.get("id") {
        Some(Value::Null) => parse_adhoc_application_reference(tool),
        Some(value) => parse_stored_application_reference(tool, value),
        None => Err(invalid_input()),
    }
}

fn parse_stored_application_reference<'a>(
    tool: &'a Map<String, Value>,
    id: &Value,
) -> Result<FrozenApplicationReference<'a>, FrozenToolSnapshotError> {
    let tool_id = positive_integer(Some(id)).ok_or_else(invalid_input)?;
    let name = required_identifier(tool, "name")?;
    let toolkit_name = required_identifier(tool, "toolkit_name")?;
    let description = tool.get("description").and_then(Value::as_str);
    let agent_type = required_identifier(tool, "agent_type")?;
    if name != toolkit_name
        || !optional_identifier(tool.get("description"))
        || positive_integer(tool.get("author_id")).is_none()
        || required_identifier(tool, "created_at").is_err()
        || !tool.get("meta").is_some_and(Value::is_object)
        || !empty_array(tool.get("variables"))
        || tool.get("is_pinned") != Some(&Value::Bool(false))
        || !null_or_absent(tool, "author")
        || !null_or_absent(tool, "online")
        || !null_or_absent(tool, "icon_meta")
        || !null_or_absent(tool, "indexes_count")
    {
        return Err(invalid_input());
    }
    let settings = tool
        .get("settings")
        .and_then(Value::as_object)
        .filter(|settings| settings.len() == 2)
        .ok_or_else(invalid_input)?;
    let application_id =
        positive_integer(settings.get("application_id")).ok_or_else(invalid_input)?;
    let application_version_id =
        positive_integer(settings.get("application_version_id")).ok_or_else(invalid_input)?;
    Ok(FrozenApplicationReference {
        tool_id: Some(tool_id),
        toolkit_name,
        description,
        agent_type,
        application_id,
        application_version_id,
        participant_id: None,
        project_id: None,
    })
}

fn parse_adhoc_application_reference(
    tool: &Map<String, Value>,
) -> Result<FrozenApplicationReference<'_>, FrozenToolSnapshotError> {
    let name = required_identifier(tool, "name")?;
    let toolkit_name = required_identifier(tool, "toolkit_name")?;
    let description = tool.get("description").and_then(Value::as_str);
    let agent_type = required_identifier(tool, "agent_type")?;
    let settings = tool
        .get("settings")
        .and_then(Value::as_object)
        .filter(|settings| settings.len() == 4)
        .ok_or_else(invalid_input)?;
    if tool.len() != 11
        || name != toolkit_name
        || !tool.get("description").is_some_and(|value| {
            value
                .as_str()
                .is_some_and(|text| valid_identifier(text, true))
        })
        || positive_integer(tool.get("author_id")).is_none()
        || required_identifier(tool, "created_at").is_err()
        || !empty_array(settings.get("variables"))
        || !empty_array(settings.get("selected_tools"))
    {
        return Err(invalid_input());
    }
    let participant_id = positive_integer(tool.get("participant_id")).ok_or_else(invalid_input)?;
    let project_id = positive_integer(tool.get("project_id")).ok_or_else(invalid_input)?;
    let application_id =
        positive_integer(settings.get("application_id")).ok_or_else(invalid_input)?;
    let application_version_id =
        positive_integer(settings.get("application_version_id")).ok_or_else(invalid_input)?;
    Ok(FrozenApplicationReference {
        tool_id: None,
        toolkit_name,
        description,
        agent_type,
        application_id,
        application_version_id,
        participant_id: Some(participant_id),
        project_id: Some(project_id),
    })
}

fn validate_optional_tool_names(
    settings: &Map<String, Value>,
    key: &str,
) -> Result<(), FrozenToolSnapshotError> {
    let Some(value) = settings.get(key) else {
        return Ok(());
    };
    let names = value.as_array().ok_or_else(invalid_input)?;
    if names.len() > MAX_SELECTED_TOOLS {
        return Err(resource_exhausted());
    }
    for name in names {
        let name = name.as_str().ok_or_else(invalid_input)?;
        if name.len() > MAX_TOOL_IDENTIFIER_BYTES {
            return Err(resource_exhausted());
        }
        if !valid_identifier(name, false) {
            return Err(invalid_input());
        }
    }
    Ok(())
}

fn required_identifier<'a>(
    object: &'a Map<String, Value>,
    key: &str,
) -> Result<&'a str, FrozenToolSnapshotError> {
    object
        .get(key)
        .and_then(Value::as_str)
        .filter(|value| valid_identifier(value, false))
        .ok_or_else(invalid_input)
}

fn optional_identifier(value: Option<&Value>) -> bool {
    match value {
        None | Some(Value::Null) => true,
        Some(Value::String(value)) => valid_identifier(value, true),
        Some(_) => false,
    }
}

fn valid_identifier(value: &str, allow_empty: bool) -> bool {
    (allow_empty || !value.is_empty())
        && value.len() <= MAX_TOOL_IDENTIFIER_BYTES
        && !value.bytes().any(|byte| matches!(byte, 0 | b'\r' | b'\n'))
}

fn positive_integer(value: Option<&Value>) -> Option<u64> {
    value
        .and_then(Value::as_u64)
        .filter(|value| (1..=i64::MAX.cast_unsigned()).contains(value))
}

fn empty_array(value: Option<&Value>) -> bool {
    value.and_then(Value::as_array).is_some_and(Vec::is_empty)
}

fn null_or_absent(object: &Map<String, Value>, key: &str) -> bool {
    object.get(key).is_none_or(Value::is_null)
}

fn is_mcp_type(tool_type: &str) -> bool {
    matches!(tool_type, "mcp" | "mcp_config") || tool_type.starts_with("mcp_")
}

const fn invalid_input() -> FrozenToolSnapshotError {
    FrozenToolSnapshotError {
        code: FrozenToolSnapshotErrorCode::InvalidInput,
        message: "the frozen agent tool snapshot is malformed",
    }
}

const fn resource_exhausted() -> FrozenToolSnapshotError {
    FrozenToolSnapshotError {
        code: FrozenToolSnapshotErrorCode::ResourceExhausted,
        message: "the frozen agent tool snapshot exceeds its approved limit",
    }
}
