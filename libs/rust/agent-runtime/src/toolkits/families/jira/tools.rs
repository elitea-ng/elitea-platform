use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::families::UnsupportedSetting;
use crate::toolkits::families::attachment_host::AttachmentHost;
use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{JiraApi, JiraClient, JiraClientError, JiraOperation};
use super::config::{JiraConfigError, JiraConfigErrorCode, JiraToolkitConfig};

const MAX_ARGUMENT_BYTES: usize = 256 * 1_024;
const MAX_DESCRIPTION_BYTES: usize = 1_000;
const MAX_LABELS: usize = 64;

/// SDK tools this runtime does not serve through this family (`SOURCE_PARITY.md`):
/// the six index tools need the indexing plane, which does not exist in the
/// Jira family. The five attachment tools ([`JiraToolKind::needs_attachments`])
/// are served only when the host lends the run's artifact store and model.
const UNSERVED_TOOLS: [&str; 6] = [
    "index_data",
    "search_index",
    "stepback_search_index",
    "stepback_summary_index",
    "list_indexes",
    "remove_index",
];

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum JiraToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedCapability(UnsupportedSetting),
    UnsupportedSelection,
    Client,
    InvalidDefinition,
}

pub(crate) struct JiraToolsetError {
    code: JiraToolsetErrorCode,
}

impl JiraToolsetError {
    #[must_use]
    pub(crate) const fn code(&self) -> JiraToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for JiraToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("JiraToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for JiraToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            JiraToolsetErrorCode::InvalidConfiguration => {
                "the Jira toolkit configuration is invalid"
            }
            JiraToolsetErrorCode::ResourceExhausted => {
                "the Jira toolkit configuration exceeds its approved limit"
            }
            JiraToolsetErrorCode::UnsupportedCapability(_) => {
                "the Jira toolkit configuration requires a capability this runtime does not provide"
            }
            JiraToolsetErrorCode::UnsupportedSelection => {
                "the selected Jira tool profile is not supported"
            }
            JiraToolsetErrorCode::Client => "the Jira client could not be created",
            JiraToolsetErrorCode::InvalidDefinition => "the Jira ADK tool definition is invalid",
        })
    }
}

impl std::error::Error for JiraToolsetError {}

impl From<JiraConfigError> for JiraToolsetError {
    fn from(source: JiraConfigError) -> Self {
        Self {
            code: match source.code() {
                JiraConfigErrorCode::InvalidConfiguration => {
                    JiraToolsetErrorCode::InvalidConfiguration
                }
                JiraConfigErrorCode::ResourceExhausted => JiraToolsetErrorCode::ResourceExhausted,
                JiraConfigErrorCode::UnsupportedCapability(setting) => {
                    JiraToolsetErrorCode::UnsupportedCapability(setting)
                }
            },
        }
    }
}

impl From<JiraClientError> for JiraToolsetError {
    fn from(_: JiraClientError) -> Self {
        Self {
            code: JiraToolsetErrorCode::Client,
        }
    }
}

impl From<MaterializedToolsetError> for JiraToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: JiraToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// Build the Jira family: the SDK's twelve REST tools.
///
/// Empty selection means every tool this runtime serves. An explicit
/// selection keeps the served tools and drops the SDK tools listed in
/// [`UNSERVED_TOOLS`] with one warning; a name the SDK does not have, or a
/// selection with nothing served left, is an unsupported selection.
pub(crate) fn build_jira_toolset(
    toolkit_name: &str,
    config: JiraToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
    attachments: Option<AttachmentHost>,
) -> Result<BasicToolset, JiraToolsetError> {
    let kinds = served_selection(config.selected_tools(), attachments.is_some())?;
    let omitted = config.selected_tools().len().saturating_sub(kinds.len());
    if attachments.is_none()
        && (config.selected_tools().is_empty()
            || config.selected_tools().iter().any(|name| {
                JiraToolKind::ALL
                    .iter()
                    .any(|kind| kind.needs_attachments() && kind.name() == name.as_ref())
            }))
    {
        tracing::warn!(
            event = "agent_toolkit_tools_skipped",
            reason_code = "attachment_host_unavailable",
            toolkit_type = "jira",
            toolkit_name,
            "Jira attachment tools were omitted: this run lends no artifact store or model"
        );
    }
    if !config.selected_tools().is_empty() && omitted > 0 {
        tracing::warn!(
            event = "agent_toolkit_tools_skipped",
            reason_code = "unsupported_tool_selection",
            toolkit_type = "jira",
            toolkit_name,
            selected_count = config.selected_tools().len(),
            materialized_count = kinds.len(),
            omitted_count = omitted,
            "Jira tools this runtime does not serve were omitted from the native toolset"
        );
    }
    let client: Arc<dyn JiraApi> = Arc::new(JiraClient::new(config)?.with_attachments(attachments));
    admit_kinds(toolkit_name, &kinds, policy, &client)
}

fn served_selection(
    selected: &[Box<str>],
    has_attachments: bool,
) -> Result<Vec<JiraToolKind>, JiraToolsetError> {
    if selected.iter().any(|name| {
        !UNSERVED_TOOLS.contains(&name.as_ref())
            && !JiraToolKind::ALL
                .iter()
                .any(|kind| kind.name() == name.as_ref())
    }) {
        return Err(unsupported_selection());
    }
    let kinds = JiraToolKind::ALL
        .into_iter()
        .filter(|kind| {
            (has_attachments || !kind.needs_attachments())
                && (selected.is_empty() || selected.iter().any(|name| name.as_ref() == kind.name()))
        })
        .collect::<Vec<_>>();
    if kinds.is_empty() {
        return Err(unsupported_selection());
    }
    Ok(kinds)
}

fn admit_kinds(
    toolkit_name: &str,
    kinds: &[JiraToolKind],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn JiraApi>,
) -> Result<BasicToolset, JiraToolsetError> {
    let tools = kinds
        .iter()
        .map(|kind| {
            Arc::new(JiraTool::new(*kind, toolkit_name, Arc::clone(client))) as Arc<dyn Tool>
        })
        .collect();
    admit_materialized_toolset(toolkit_name, "jira", policy, tools).map_err(Into::into)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_build_with_api(
    toolkit_name: &str,
    selected: &[&str],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn JiraApi>,
) -> Result<BasicToolset, JiraToolsetError> {
    let selected = selected
        .iter()
        .map(|name| (*name).into())
        .collect::<Vec<Box<str>>>();
    admit_kinds(
        toolkit_name,
        &served_selection(&selected, true)?,
        policy,
        client,
    )
}

/// [`test_build_with_api`] for a run whose host lends no attachment authority.
#[cfg(test)]
pub(in crate::toolkits) fn test_build_without_attachments(
    toolkit_name: &str,
    selected: &[&str],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn JiraApi>,
) -> Result<BasicToolset, JiraToolsetError> {
    let selected = selected
        .iter()
        .map(|name| (*name).into())
        .collect::<Vec<Box<str>>>();
    admit_kinds(
        toolkit_name,
        &served_selection(&selected, false)?,
        policy,
        client,
    )
}

#[cfg(test)]
pub(in crate::toolkits) fn test_catalog() -> Vec<(&'static str, bool)> {
    JiraToolKind::ALL
        .into_iter()
        .map(|kind| (kind.name(), kind.is_read_only()))
        .collect()
}

#[derive(Clone, Copy)]
enum JiraToolKind {
    SearchUsingJql,
    CreateIssue,
    UpdateIssue,
    ModifyLabels,
    ListComments,
    AddComments,
    ListProjects,
    SetIssueStatus,
    GetSpecificFieldInfo,
    GetRemoteLinks,
    LinkIssues,
    ExecuteGenericRq,
    GetFieldWithImageDescriptions,
    GetCommentsWithImageDescriptions,
    GetAttachmentsContent,
    AddFileToIssueDescription,
    UpdateCommentWithFile,
}

impl JiraToolKind {
    /// The SDK's `get_available_tools` order, without the unserved tools.
    const ALL: [Self; 17] = [
        Self::SearchUsingJql,
        Self::CreateIssue,
        Self::UpdateIssue,
        Self::ModifyLabels,
        Self::ListComments,
        Self::AddComments,
        Self::ListProjects,
        Self::SetIssueStatus,
        Self::GetSpecificFieldInfo,
        Self::GetRemoteLinks,
        Self::LinkIssues,
        Self::ExecuteGenericRq,
        Self::GetFieldWithImageDescriptions,
        Self::GetCommentsWithImageDescriptions,
        Self::GetAttachmentsContent,
        Self::AddFileToIssueDescription,
        Self::UpdateCommentWithFile,
    ];

    /// Whether the tool reads the artifact store or calls the run's model.
    const fn needs_attachments(self) -> bool {
        matches!(
            self,
            Self::GetFieldWithImageDescriptions
                | Self::GetCommentsWithImageDescriptions
                | Self::GetAttachmentsContent
                | Self::AddFileToIssueDescription
                | Self::UpdateCommentWithFile
        )
    }

    const fn name(self) -> &'static str {
        match self {
            Self::SearchUsingJql => "search_using_jql",
            Self::CreateIssue => "create_issue",
            Self::UpdateIssue => "update_issue",
            Self::ModifyLabels => "modify_labels",
            Self::ListComments => "list_comments",
            Self::AddComments => "add_comments",
            Self::ListProjects => "list_projects",
            Self::SetIssueStatus => "set_issue_status",
            Self::GetSpecificFieldInfo => "get_specific_field_info",
            Self::GetRemoteLinks => "get_remote_links",
            Self::LinkIssues => "link_issues",
            Self::ExecuteGenericRq => "execute_generic_rq",
            Self::GetFieldWithImageDescriptions => "get_field_with_image_descriptions",
            Self::GetCommentsWithImageDescriptions => "get_comments_with_image_descriptions",
            Self::GetAttachmentsContent => "get_attachments_content",
            Self::AddFileToIssueDescription => "add_file_to_issue_description",
            Self::UpdateCommentWithFile => "update_comment_with_file",
        }
    }

    const fn is_read_only(self) -> bool {
        matches!(
            self,
            Self::SearchUsingJql
                | Self::ListComments
                | Self::ListProjects
                | Self::GetSpecificFieldInfo
                | Self::GetRemoteLinks
                | Self::GetFieldWithImageDescriptions
                | Self::GetCommentsWithImageDescriptions
                | Self::GetAttachmentsContent
        )
    }

    const fn description(self) -> &'static str {
        match self {
            Self::SearchUsingJql => {
                "Search for Jira issues using JQL. Pagination is handled internally, so limit may exceed Jira's 100-per-request cap; without limit the toolkit's default limit applies, and 0 means the 1000-issue maximum. Returns key, id, projectId, summary, description, created, assignee, priority, status, updated, duedate, url and related_issues for each issue."
            }
            Self::CreateIssue => {
                "Create an issue in Jira from issue_json, a JSON string with a 'fields' object holding at least project (key), summary and issuetype (name). The toolkit's default labels are added afterwards. This remote write can create duplicates: an unknown outcome must be reconciled, not retried automatically."
            }
            Self::UpdateIssue => {
                "Update an issue in Jira. issue_json must contain 'key' and at least one of 'fields' or 'update'. The toolkit's default labels are added afterwards. This remote write must be reconciled, not retried automatically, after an unknown outcome."
            }
            Self::ModifyLabels => {
                "Add and/or remove labels on one Jira issue; give at least one of add_labels or remove_labels. This remote write must be reconciled, not retried automatically, after an unknown outcome."
            }
            Self::ListComments => {
                "Extract the comments of one Jira issue: author, comment body, id and url for each."
            }
            Self::AddComments => {
                "Add a comment to a Jira issue (sent as Atlassian Document Format on REST v3). The toolkit's default labels are added afterwards. This remote write can duplicate the comment: an unknown outcome must be reconciled, not retried automatically."
            }
            Self::ListProjects => "List all Jira projects visible to the configured account.",
            Self::SetIssueStatus => {
                "Set a new status for a Jira issue by moving it through the workflow transition whose target status matches status_name (case-insensitive). mandatory_fields_json is a JSON object whose optional 'fields' and 'update' blocks are sent with the transition. This remote write must be reconciled, not retried automatically, after an unknown outcome."
            }
            Self::GetSpecificFieldInfo => {
                "Get one field of a Jira issue by issue key and field name, such as description, summary, priority or customfield_10300. When the field is empty, lists the fields the issue does have."
            }
            Self::GetRemoteLinks => "Get the remote links of a Jira issue by its key.",
            Self::LinkIssues => {
                "Link two Jira issues with an issue link type such as Test, Relates or Blocks; with Test, the test is the inward issue and the story is the outward issue. A comment noting the link is added. This remote write can duplicate the link: an unknown outcome must be reconciled, not retried automatically."
            }
            Self::ExecuteGenericRq => {
                "Execute a generic Jira REST request on this Jira instance. relative_url starts with '/rest/api/2/...' or '/rest/api/3/...' and carries no query string; params is a JSON object string sent as query parameters for GET and as the JSON body otherwise. For searches always request key, summary, status, assignee and issuetype and set maxResults. Non-GET methods are remote writes that must be reconciled, not retried automatically, after an unknown outcome."
            }
            Self::GetFieldWithImageDescriptions => {
                "Get a field of a Jira issue (for example description) and replace each image in it with a text description from the run's model, using the surrounding text as context. process_images=false returns the raw field text and needs no model. At most 20 pictures of up to 5 MiB are described per call; pictures are read only from this Jira's own attachments."
            }
            Self::GetCommentsWithImageDescriptions => {
                "Get all comments of a Jira issue and replace each image in them with a text description from the run's model, using the surrounding text as context. process_images=false returns the raw comments and needs no model. At most 20 pictures of up to 5 MiB are described per call; pictures are read only from this Jira's own attachments."
            }
            Self::GetAttachmentsContent => {
                "Extract the content of the attachments of a Jira issue, optionally only those whose file name matches attachment_pattern (a regular expression). Only parsable attachments are considered: plain text, YAML, CSV and pictures (described by the run's model); other file types are skipped."
            }
            Self::AddFileToIssueDescription => {
                "Upload a file from artifact storage (filepath /{bucket}/{filename}) to a Jira issue as an attachment and reference it in the description: images and videos inline, other files as a link, at the end (append) or the start (prepend). A same-named attachment is not replaced; the new one is added beside it. If the description update fails after the upload, the error says the file was uploaded. This remote write must be reconciled, not retried automatically, after an unknown outcome."
            }
            Self::UpdateCommentWithFile => {
                "Upload a file from artifact storage (filepath /{bucket}/{filename}) to a Jira issue as an attachment and reference it in an existing comment: images and videos inline, other files as a link, at the end (append) or the start (prepend). A same-named attachment is not replaced; the new one is added beside it. If the comment update fails after the upload, the error says the file was uploaded. This remote write must be reconciled, not retried automatically, after an unknown outcome."
            }
        }
    }
}

struct JiraTool {
    kind: JiraToolKind,
    client: Arc<dyn JiraApi>,
    description: Box<str>,
}

impl JiraTool {
    fn new(kind: JiraToolKind, toolkit_name: &str, client: Arc<dyn JiraApi>) -> Self {
        let description = format!("Toolkit: {toolkit_name}\n{}", kind.description());
        Self {
            kind,
            client,
            description: description
                .chars()
                .take(MAX_DESCRIPTION_BYTES)
                .collect::<String>()
                .into_boxed_str(),
        }
    }
}

#[async_trait]
impl Tool for JiraTool {
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
        Some(schema_for(self.kind))
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        validate_argument_size(&arguments)?;
        let arguments = arguments.as_object().ok_or_else(invalid_arguments)?;
        reject_unknown_keys(arguments, &schema_for(self.kind))?;
        let operation = operation(self.kind, arguments)?;
        self.client
            .execute(operation)
            .await
            .map_err(JiraClientError::into_adk)
    }
}

fn operation(
    kind: JiraToolKind,
    arguments: &Map<String, Value>,
) -> adk_core::Result<JiraOperation<'_>> {
    Ok(match kind {
        JiraToolKind::SearchUsingJql => JiraOperation::SearchUsingJql {
            jql: required_text(arguments, "jql")?,
            limit: optional_integer(arguments, "limit")?,
        },
        JiraToolKind::CreateIssue => JiraOperation::CreateIssue {
            issue_json: required_text(arguments, "issue_json")?,
        },
        JiraToolKind::UpdateIssue => JiraOperation::UpdateIssue {
            issue_json: required_text(arguments, "issue_json")?,
        },
        JiraToolKind::ModifyLabels => JiraOperation::ModifyLabels {
            issue_key: required_text(arguments, "issue_key")?,
            add_labels: optional_strings(arguments, "add_labels")?,
            remove_labels: optional_strings(arguments, "remove_labels")?,
        },
        JiraToolKind::ListComments => JiraOperation::ListComments {
            issue_key: required_text(arguments, "issue_key")?,
        },
        JiraToolKind::AddComments => JiraOperation::AddComments {
            issue_key: required_text(arguments, "issue_key")?,
            comment: required_text(arguments, "comment")?,
        },
        JiraToolKind::ListProjects => JiraOperation::ListProjects,
        JiraToolKind::SetIssueStatus => JiraOperation::SetIssueStatus {
            issue_key: required_text(arguments, "issue_key")?,
            status_name: required_text(arguments, "status_name")?,
            mandatory_fields_json: required_text(arguments, "mandatory_fields_json")?,
        },
        JiraToolKind::GetSpecificFieldInfo => JiraOperation::GetSpecificFieldInfo {
            issue_key: required_text(arguments, "jira_issue_key")?,
            field_name: required_text(arguments, "field_name")?,
        },
        JiraToolKind::GetRemoteLinks => JiraOperation::GetRemoteLinks {
            issue_key: required_text(arguments, "jira_issue_key")?,
        },
        JiraToolKind::LinkIssues => JiraOperation::LinkIssues {
            inward_issue_key: required_text(arguments, "inward_issue_key")?,
            outward_issue_key: required_text(arguments, "outward_issue_key")?,
            linktype: required_text(arguments, "linktype")?,
        },
        JiraToolKind::ExecuteGenericRq => JiraOperation::ExecuteGenericRq {
            method: required_text(arguments, "method")?,
            relative_url: required_text(arguments, "relative_url")?,
            params: optional_text(arguments, "params")?,
        },
        JiraToolKind::GetFieldWithImageDescriptions => {
            JiraOperation::GetFieldWithImageDescriptions {
                issue_key: required_text(arguments, "jira_issue_key")?,
                field_name: required_text(arguments, "field_name")?,
                prompt: optional_text(arguments, "prompt")?,
                context_radius: coerced_integer(arguments, "context_radius")?.unwrap_or(500),
                process_images: optional_flag(arguments, "process_images")?.unwrap_or(true),
            }
        }
        JiraToolKind::GetCommentsWithImageDescriptions => {
            JiraOperation::GetCommentsWithImageDescriptions {
                issue_key: required_text(arguments, "jira_issue_key")?,
                prompt: optional_text(arguments, "prompt")?,
                context_radius: coerced_integer(arguments, "context_radius")?.unwrap_or(500),
                process_images: optional_flag(arguments, "process_images")?.unwrap_or(true),
            }
        }
        JiraToolKind::GetAttachmentsContent => JiraOperation::GetAttachmentsContent {
            issue_key: required_text(arguments, "jira_issue_key")?,
            attachment_pattern: optional_text(arguments, "attachment_pattern")?,
        },
        JiraToolKind::AddFileToIssueDescription => JiraOperation::AddFileToIssueDescription {
            issue_key: required_text(arguments, "issue_key")?,
            filepath: required_text(arguments, "filepath")?,
            filename: optional_text(arguments, "filename")?,
            prepend: position(arguments)?,
        },
        JiraToolKind::UpdateCommentWithFile => JiraOperation::UpdateCommentWithFile {
            issue_key: required_text(arguments, "issue_key")?,
            comment_id: required_text(arguments, "comment_id")?,
            filepath: required_text(arguments, "filepath")?,
            filename: optional_text(arguments, "filename")?,
            prepend: position(arguments)?,
        },
    })
}

#[allow(clippy::too_many_lines)] // Keeps the twelve SDK argument schemas auditable in one place.
fn schema_for(kind: JiraToolKind) -> Value {
    let (properties, required): (Vec<(&str, Value)>, &[&str]) = match kind {
        JiraToolKind::SearchUsingJql => (
            vec![
                ("jql", text("Jira Query Language (JQL) query string")),
                (
                    "limit",
                    nullable(
                        json!({"type":"integer","description":"Maximum number of issues to return. Overrides the toolkit's default limit; pagination is handled internally, so values above 100 retrieve several pages. At most 1000; 0 means 1000."}),
                        Value::Null,
                    ),
                ),
            ],
            &["jql"],
        ),
        JiraToolKind::CreateIssue => (
            vec![(
                "issue_json",
                text(
                    "Pure JSON string to create a Jira issue. Must contain a 'fields' object with at minimum 'project' (key), 'summary' and 'issuetype' (name), and optionally 'description', 'priority', etc. For 'description' use Atlassian Document Format when the instance uses REST v3, e.g. {\"type\": \"doc\", \"version\": 1, \"content\": [{\"type\": \"paragraph\", \"content\": [{\"type\": \"text\", \"text\": \"your text\"}]}]}, otherwise a plain string. Example: {\"fields\": {\"project\": {\"key\": \"PROJ\"}, \"summary\": \"Issue title\", \"issuetype\": {\"name\": \"Task\"}, \"priority\": {\"name\": \"Major\"}}}. All JSON keys must be double-quoted, without surrounding ``` fences.",
                ),
            )],
            &["issue_json"],
        ),
        JiraToolKind::UpdateIssue => (
            vec![(
                "issue_json",
                text(
                    "JSON string to update a Jira issue. Must contain 'key' (issue key) and at least one of 'fields' or 'update' objects. The update is sent through REST v2, so rich-text fields take plain strings or wiki markup. Example: {\"key\": \"PROJ-123\", \"fields\": {\"summary\": \"Updated title\"}, \"update\": {\"labels\": [{\"add\": \"new-label\"}]}}. All JSON keys must be double-quoted.",
                ),
            )],
            &["issue_json"],
        ),
        JiraToolKind::ModifyLabels => (
            vec![
                (
                    "issue_key",
                    text("The issue key of the Jira issue whose labels change, e.g. 'TEST-123'."),
                ),
                ("add_labels", nullable_strings("List of labels to be added")),
                (
                    "remove_labels",
                    nullable_strings("List of labels to be removed"),
                ),
            ],
            &["issue_key"],
        ),
        JiraToolKind::ListComments => (
            vec![(
                "issue_key",
                text(
                    "The issue key of the Jira issue from which comments will be extracted, e.g. 'TEST-123'.",
                ),
            )],
            &["issue_key"],
        ),
        JiraToolKind::AddComments => (
            vec![
                (
                    "issue_key",
                    text(
                        "The issue key of the Jira issue to which the comment is to be added, e.g. 'TEST-123'.",
                    ),
                ),
                (
                    "comment",
                    text(
                        "The comment to be added to the Jira issue, e.g. 'This is a test comment.'",
                    ),
                ),
            ],
            &["issue_key", "comment"],
        ),
        JiraToolKind::ListProjects => (Vec::new(), &[]),
        JiraToolKind::SetIssueStatus => (
            vec![
                (
                    "issue_key",
                    text(
                        "The issue key of the Jira issue whose status changes, e.g. \"TEST-123\".",
                    ),
                ),
                (
                    "status_name",
                    text("Jira issue status name, e.g. \"Close\", \"In progress\"."),
                ),
                (
                    "mandatory_fields_json",
                    text(
                        "JSON object string with the mandatory fields the transition requires: set fields through a 'fields' object and screen properties that 'fields' cannot set through an 'update' object. Use \"{}\" when the transition needs none.",
                    ),
                ),
            ],
            &["issue_key", "status_name", "mandatory_fields_json"],
        ),
        JiraToolKind::GetSpecificFieldInfo => (
            vec![
                (
                    "jira_issue_key",
                    text(
                        "Jira issue key specific information will be extracted from, in the format TEST-1234",
                    ),
                ),
                (
                    "field_name",
                    text(
                        "Field name data from which will be taken. It should be either 'description', 'summary', 'priority' etc or a custom field name in the format 'customfield_10300'",
                    ),
                ),
            ],
            &["jira_issue_key", "field_name"],
        ),
        JiraToolKind::GetRemoteLinks => (
            vec![(
                "jira_issue_key",
                text("Jira issue key from which remote links will be extracted, e.g. TEST-1234"),
            )],
            &["jira_issue_key"],
        ),
        JiraToolKind::LinkIssues => (
            vec![
                (
                    "inward_issue_key",
                    text(
                        "The key of the inward issue. With the \"Test\" link type the test is the inward issue.",
                    ),
                ),
                (
                    "outward_issue_key",
                    text(
                        "The key of the outward issue. With the \"Test\" link type the story or other issue is the outward issue.",
                    ),
                ),
                (
                    "linktype",
                    text("The issue link type, e.g. \"Test\", \"Relates\", \"Blocks\"."),
                ),
            ],
            &["inward_issue_key", "outward_issue_key", "linktype"],
        ),
        JiraToolKind::ExecuteGenericRq => (
            vec![
                (
                    "method",
                    text("The HTTP method to use for the request (GET, POST, PUT, PATCH, DELETE)."),
                ),
                (
                    "relative_url",
                    text(
                        "The relative URI for the Jira REST API. It must start with a forward slash and use '/rest/api/2/...' or '/rest/api/3/...'. Do not include query parameters; provide them in 'params'.",
                    ),
                ),
                (
                    "params",
                    nullable(
                        json!({"type":"string","description":"Optional JSON object string of parameters, sent as query parameters for GET and as the JSON request body otherwise."}),
                        Value::String(String::new()),
                    ),
                ),
            ],
            &["method", "relative_url"],
        ),
        JiraToolKind::GetFieldWithImageDescriptions => (
            vec![
                (
                    "jira_issue_key",
                    text(
                        "Jira issue key from which field with images will be extracted, e.g. TEST-1234",
                    ),
                ),
                (
                    "field_name",
                    text(
                        "Field name containing images to be processed. Common values are 'description', 'comment', or custom fields like 'customfield_10300'",
                    ),
                ),
                (
                    "prompt",
                    optional_string(
                        "Custom prompt to use for image description generation. If not provided, a default prompt will be used",
                    ),
                ),
                ("context_radius", context_radius()),
                (
                    "process_images",
                    process_images(
                        "Whether to process images with LLM and replace references with descriptions. Set to False to return raw field content without image processing. Default is True",
                    ),
                ),
            ],
            &["jira_issue_key", "field_name"],
        ),
        JiraToolKind::GetCommentsWithImageDescriptions => (
            vec![
                (
                    "jira_issue_key",
                    text(
                        "Jira issue key from which comments with images will be extracted, e.g. TEST-1234",
                    ),
                ),
                (
                    "prompt",
                    optional_string(
                        "Custom prompt to use for image description generation. If not provided, a default prompt will be used",
                    ),
                ),
                ("context_radius", context_radius()),
                (
                    "process_images",
                    process_images(
                        "Whether to process images with LLM and replace references with descriptions. Set to False to return raw comments without image processing. Default is True",
                    ),
                ),
            ],
            &["jira_issue_key"],
        ),
        JiraToolKind::GetAttachmentsContent => (
            vec![
                (
                    "jira_issue_key",
                    text("Jira issue key from which the attachments will be read, e.g. TEST-1234"),
                ),
                (
                    "attachment_pattern",
                    optional_string(
                        "Regex pattern to filter attachment filenames. If not provided, all attachments will be processed",
                    ),
                ),
            ],
            &["jira_issue_key"],
        ),
        JiraToolKind::AddFileToIssueDescription => (
            vec![
                (
                    "issue_key",
                    text(
                        "Jira issue key where file will be added, e.g. 'PROJ-123'. The file will be uploaded as attachment and referenced in the description.",
                    ),
                ),
                (
                    "filepath",
                    text(
                        "File path in format /{bucket}/{filename} containing the file to attach. Artifact can be any file type (image, PDF, video, document, etc.). Get this from the file/image generation tool's response.",
                    ),
                ),
                (
                    "filename",
                    optional_string(
                        "Filename for the attachment, e.g. 'diagram.png', 'report.pdf'. If not provided, uses the original filename from artifact. Should include file extension.",
                    ),
                ),
                (
                    "position",
                    position_schema(
                        "Where to place the file reference in description: 'append' (end) or 'prepend' (beginning).",
                    ),
                ),
            ],
            &["issue_key", "filepath"],
        ),
        JiraToolKind::UpdateCommentWithFile => (
            vec![
                ("issue_key", text("Jira issue key, e.g. 'PROJ-123'.")),
                (
                    "comment_id",
                    text("ID of the existing comment to update. Get from list_comments tool."),
                ),
                (
                    "filepath",
                    text(
                        "File path in format /{bucket}/{filename} containing the file. Get this from the file/image generation tool's response.",
                    ),
                ),
                (
                    "filename",
                    optional_string(
                        "Filename for the attachment, e.g. 'screenshot.png', 'report.pdf'. If not provided, uses the original filename from artifact.",
                    ),
                ),
                (
                    "position",
                    position_schema(
                        "Where to place file: 'append' (end) or 'prepend' (beginning) of existing comment.",
                    ),
                ),
            ],
            &["issue_key", "comment_id", "filepath"],
        ),
    };
    json!({
        "type": "object",
        "properties": properties.into_iter().map(|(name, schema)| (name.to_owned(), schema)).collect::<Map<_, _>>(),
        "required": required,
        "additionalProperties": false,
    })
}

fn optional_string(description: &str) -> Value {
    nullable(
        json!({"type":"string","description":description}),
        Value::Null,
    )
}

fn context_radius() -> Value {
    nullable(
        json!({"type":"integer","minimum":0,"description":"Number of characters to include before and after each image for context. Default is 500"}),
        json!(500),
    )
}

fn process_images(description: &str) -> Value {
    nullable(
        json!({"type":"boolean","description":description}),
        Value::Bool(true),
    )
}

fn position_schema(description: &str) -> Value {
    nullable(
        json!({"type":"string","enum":["append","prepend"],"description":description}),
        Value::String("append".to_owned()),
    )
}

fn text(description: &str) -> Value {
    json!({"type":"string","description":description})
}

fn nullable_strings(description: &str) -> Value {
    nullable(
        json!({"type":"array","items":{"type":"string"},"maxItems":MAX_LABELS,"description":description}),
        Value::Null,
    )
}

fn nullable(value: Value, default: Value) -> Value {
    Value::Object(Map::from_iter([
        (
            "anyOf".to_owned(),
            Value::Array(vec![value, json!({"type":"null"})]),
        ),
        ("default".to_owned(), default),
    ]))
}

fn validate_argument_size(arguments: &Value) -> adk_core::Result<()> {
    if serde_json::to_vec(arguments)
        .map_err(|_| invalid_arguments())?
        .len()
        > MAX_ARGUMENT_BYTES
    {
        return Err(AdkError::new(
            ErrorComponent::Tool,
            ErrorCategory::InvalidInput,
            "jira.arguments.resource_exhausted",
            "the Jira tool arguments exceed the approved limit",
        ));
    }
    Ok(())
}

fn reject_unknown_keys(arguments: &Map<String, Value>, schema: &Value) -> adk_core::Result<()> {
    let allowed = schema
        .get("properties")
        .and_then(Value::as_object)
        .ok_or_else(invalid_arguments)?;
    if arguments.keys().any(|key| !allowed.contains_key(key)) {
        return Err(invalid_arguments());
    }
    Ok(())
}

fn required_text<'a>(arguments: &'a Map<String, Value>, name: &str) -> adk_core::Result<&'a str> {
    arguments
        .get(name)
        .and_then(Value::as_str)
        .filter(|value| !value.is_empty())
        .ok_or_else(invalid_arguments)
}

/// The SDK drops explicit nulls before dispatch, so null means "absent".
fn optional_text<'a>(
    arguments: &'a Map<String, Value>,
    name: &str,
) -> adk_core::Result<Option<&'a str>> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value)) => Ok(Some(value)),
        _ => Err(invalid_arguments()),
    }
}

fn optional_integer(arguments: &Map<String, Value>, name: &str) -> adk_core::Result<Option<i64>> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::Number(value)) => value.as_i64().map(Some).ok_or_else(invalid_arguments),
        _ => Err(invalid_arguments()),
    }
}

/// An SDK `int`: a JSON integer, an integral float, or a decimal string
/// (pydantic's coercion). Null means absent.
#[allow(clippy::cast_possible_truncation)] // The float is integral and under 9e15.
fn coerced_integer(arguments: &Map<String, Value>, name: &str) -> adk_core::Result<Option<i64>> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::Number(value)) => value
            .as_i64()
            .or_else(|| {
                value
                    .as_f64()
                    .filter(|float| float.fract() == 0.0 && float.abs() < 9.0e15)
                    .map(|float| float as i64)
            })
            .map(Some)
            .ok_or_else(invalid_arguments),
        Some(Value::String(value)) => value
            .trim()
            .parse::<i64>()
            .map(Some)
            .map_err(|_| invalid_arguments()),
        _ => Err(invalid_arguments()),
    }
}

/// A boolean argument (only booleans); null means absent.
fn optional_flag(arguments: &Map<String, Value>, name: &str) -> adk_core::Result<Option<bool>> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::Bool(value)) => Ok(Some(*value)),
        _ => Err(invalid_arguments()),
    }
}

/// `position`: `append` (the default) or `prepend`; `true` when prepending.
fn position(arguments: &Map<String, Value>) -> adk_core::Result<bool> {
    match optional_text(arguments, "position")? {
        None | Some("append") => Ok(false),
        Some("prepend") => Ok(true),
        Some(_) => Err(invalid_arguments()),
    }
}

fn optional_strings<'a>(
    arguments: &'a Map<String, Value>,
    name: &str,
) -> adk_core::Result<Option<Vec<&'a str>>> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::Array(values)) if values.len() <= MAX_LABELS => values
            .iter()
            .map(|value| value.as_str().ok_or_else(invalid_arguments))
            .collect::<adk_core::Result<Vec<_>>>()
            .map(Some),
        _ => Err(invalid_arguments()),
    }
}

fn invalid_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "jira.arguments.invalid",
        "the Jira tool arguments are invalid",
    )
}

const fn unsupported_selection() -> JiraToolsetError {
    JiraToolsetError {
        code: JiraToolsetErrorCode::UnsupportedSelection,
    }
}
