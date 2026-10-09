use std::collections::HashSet;
use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{SlackApi, SlackClient, SlackClientError};
use super::config::{SlackConfigError, SlackToolkitConfig};

const SEND_MESSAGE: &str = "send_message";
const READ_MESSAGES: &str = "read_messages";
const CREATE_CHANNEL: &str = "create_slack_channel";
const LIST_CHANNEL_USERS: &str = "list_channel_users";
const LIST_WORKSPACE_USERS: &str = "list_workspace_users";
const INVITE_TO_CONVERSATION: &str = "invite_to_conversation";
const LIST_WORKSPACE_CONVERSATIONS: &str = "list_workspace_conversations";
const DEFAULT_MESSAGE_LIMIT: usize = 10;
const MAX_MESSAGE_LIMIT: usize = 15;
const MAX_CHANNEL_ID_BYTES: usize = 255;
const MAX_MESSAGE_CHARS: usize = 40_000;
const MAX_CHANNEL_NAME_BYTES: usize = 80;
const MAX_THREAD_TS_BYTES: usize = 64;
const MAX_INVITEES: usize = 100;
const MAX_ARGUMENT_BYTES: usize = 256 * 1_024;
const MAX_DESCRIPTION_BYTES: usize = 1_000;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum SlackToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedSelection,
    Client,
    InvalidDefinition,
}

/// Stable construction failure for the complete Slack family.
pub(crate) struct SlackToolsetError {
    code: SlackToolsetErrorCode,
}

impl SlackToolsetError {
    #[must_use]
    pub(crate) const fn code(&self) -> SlackToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for SlackToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("SlackToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for SlackToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            SlackToolsetErrorCode::InvalidConfiguration => {
                "the Slack toolkit configuration is invalid"
            }
            SlackToolsetErrorCode::ResourceExhausted => {
                "the Slack toolkit configuration exceeds its approved limit"
            }
            SlackToolsetErrorCode::UnsupportedSelection => {
                "the selected Slack tool profile is not supported"
            }
            SlackToolsetErrorCode::Client => "the Slack client could not be created",
            SlackToolsetErrorCode::InvalidDefinition => "the Slack ADK tool definition is invalid",
        })
    }
}

impl std::error::Error for SlackToolsetError {}

impl From<SlackConfigError> for SlackToolsetError {
    fn from(source: SlackConfigError) -> Self {
        Self {
            code: match source.code() {
                super::config::SlackConfigErrorCode::InvalidConfiguration => {
                    SlackToolsetErrorCode::InvalidConfiguration
                }
                super::config::SlackConfigErrorCode::ResourceExhausted => {
                    SlackToolsetErrorCode::ResourceExhausted
                }
            },
        }
    }
}

impl From<SlackClientError> for SlackToolsetError {
    fn from(_: SlackClientError) -> Self {
        Self {
            code: SlackToolsetErrorCode::Client,
        }
    }
}

impl From<MaterializedToolsetError> for SlackToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: SlackToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// Build all seven capability-disabled Slack tools.
///
/// Read/write grouping describes effects to the model; it never grants
/// authority. The shared policy and future exact-interrupt wrapper decide
/// whether each admitted invocation requires human approval.
pub(crate) fn build_slack_toolset(
    toolkit_name: &str,
    config: SlackToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, SlackToolsetError> {
    validate_selection(config.selected_tools())?;
    let selected = config
        .selected_tools()
        .iter()
        .map(ToString::to_string)
        .collect::<Vec<_>>();
    let client: Arc<dyn SlackApi> = Arc::new(SlackClient::new(config)?);
    build_with_api(toolkit_name, &selected, policy, &client)
}

fn validate_selection(selected: &[Box<str>]) -> Result<(), SlackToolsetError> {
    if selected.iter().any(|name| {
        !matches!(
            name.as_ref(),
            SEND_MESSAGE
                | READ_MESSAGES
                | CREATE_CHANNEL
                | LIST_CHANNEL_USERS
                | LIST_WORKSPACE_USERS
                | INVITE_TO_CONVERSATION
                | LIST_WORKSPACE_CONVERSATIONS
        )
    }) {
        return Err(SlackToolsetError {
            code: SlackToolsetErrorCode::UnsupportedSelection,
        });
    }
    Ok(())
}

fn build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn SlackApi>,
) -> Result<BasicToolset, SlackToolsetError> {
    let include_all = selected.is_empty();
    let mut tools: Vec<Arc<dyn Tool>> = Vec::with_capacity(7);
    for kind in SlackToolKind::ALL {
        if include_all || selected.iter().any(|name| name == kind.name()) {
            tools.push(Arc::new(SlackTool::new(
                kind,
                toolkit_name,
                Arc::clone(client),
            )));
        }
    }
    admit_materialized_toolset(toolkit_name, "slack", policy, tools).map_err(Into::into)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn SlackApi>,
) -> Result<BasicToolset, SlackToolsetError> {
    build_with_api(toolkit_name, selected, policy, client)
}

#[derive(Clone, Copy)]
enum SlackToolKind {
    SendMessage,
    ReadMessages,
    CreateChannel,
    ListChannelUsers,
    ListWorkspaceUsers,
    Invite,
    ListWorkspaceConversations,
}

impl SlackToolKind {
    const ALL: [Self; 7] = [
        Self::SendMessage,
        Self::ReadMessages,
        Self::CreateChannel,
        Self::ListChannelUsers,
        Self::ListWorkspaceUsers,
        Self::Invite,
        Self::ListWorkspaceConversations,
    ];

    const fn name(self) -> &'static str {
        match self {
            Self::SendMessage => SEND_MESSAGE,
            Self::ReadMessages => READ_MESSAGES,
            Self::CreateChannel => CREATE_CHANNEL,
            Self::ListChannelUsers => LIST_CHANNEL_USERS,
            Self::ListWorkspaceUsers => LIST_WORKSPACE_USERS,
            Self::Invite => INVITE_TO_CONVERSATION,
            Self::ListWorkspaceConversations => LIST_WORKSPACE_CONVERSATIONS,
        }
    }

    const fn is_read_only(self) -> bool {
        matches!(
            self,
            Self::ReadMessages
                | Self::ListChannelUsers
                | Self::ListWorkspaceUsers
                | Self::ListWorkspaceConversations
        )
    }
}

struct SlackTool {
    kind: SlackToolKind,
    client: Arc<dyn SlackApi>,
    description: Box<str>,
}

impl SlackTool {
    fn new(kind: SlackToolKind, toolkit_name: &str, client: Arc<dyn SlackApi>) -> Self {
        let action = match kind {
            SlackToolKind::SendMessage => {
                "Post one Slack message to a channel, DM conversation, or user's App Home. Slack's default mrkdwn parsing and link unfurling apply. channel_id overrides the configured default; thread_ts replies to a parent returned by read_messages. This creates visible content and is not safe to retry after an unknown outcome. Returns success, channel_id, ts, and the supplied thread_ts for a reply."
            }
            SlackToolKind::ReadMessages => {
                "Return the newest messages from the first Slack conversation-history page, projected to ts, user, message, app_name, and optional thread_ts. channel_id defaults to the configured conversation; limit defaults to 10 and is 1 through 15. Thread replies and continuation pages are not fetched."
            }
            SlackToolKind::CreateChannel => {
                "Create one public or private Slack channel. The channel name must be a new lowercase Slack name. This changes workspace state and is not safe to retry after an unknown outcome. Returns success and channel_id."
            }
            SlackToolKind::ListChannelUsers => {
                "Return id and name for users in the first membership page of one Slack conversation. channel_id defaults to the configured conversation. One bounded user lookup is performed per member; continuation pages are not fetched."
            }
            SlackToolKind::ListWorkspaceUsers => {
                "Return the first Slack workspace-user page projected to id, name, is_bot, email, and team. Email requires the users:read.email scope. Continuation pages are not fetched."
            }
            SlackToolKind::Invite => {
                "Invite one or more Slack user IDs to a channel. channel_id overrides the configured default. This changes channel membership and is not safe to retry after an unknown outcome. Returns Slack's bounded successful invitation response."
            }
            SlackToolKind::ListWorkspaceConversations => {
                "Return the first page of public Slack channels visible to the token, projected to id, name, is_channel, and shared_team_ids. Private channels, DMs, and continuation pages are not fetched."
            }
        };
        let description = format!("Toolkit: {toolkit_name}\n{action}");
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
impl Tool for SlackTool {
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
        Some(match self.kind {
            SlackToolKind::SendMessage => send_message_schema(),
            SlackToolKind::ReadMessages => read_messages_schema(),
            SlackToolKind::CreateChannel => create_channel_schema(),
            SlackToolKind::ListChannelUsers => channel_schema(
                "Slack channel, DM, or conversation ID to inspect, for example `C12345678` or `D12345678`. Omit or pass null to use the configured default conversation.",
            ),
            SlackToolKind::ListWorkspaceUsers | SlackToolKind::ListWorkspaceConversations => {
                empty_schema()
            }
            SlackToolKind::Invite => invite_schema(),
        })
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        validate_argument_size(&arguments)?;
        let arguments = arguments.as_object().ok_or_else(invalid_arguments)?;
        let result = match self.kind {
            SlackToolKind::SendMessage => {
                reject_unknown_keys(arguments, &["channel_id", "message", "thread_ts"])?;
                self.client
                    .send_message(
                        optional_channel(arguments)?.unwrap_or(""),
                        required_message(arguments)?,
                        optional_thread_ts(arguments)?,
                    )
                    .await
            }
            SlackToolKind::ReadMessages => {
                reject_unknown_keys(arguments, &["channel_id", "limit"])?;
                self.client
                    .read_messages(
                        optional_channel(arguments)?.unwrap_or(""),
                        message_limit(arguments)?,
                    )
                    .await
            }
            SlackToolKind::CreateChannel => {
                reject_unknown_keys(arguments, &["channel_name", "is_private"])?;
                self.client
                    .create_channel(
                        channel_name(arguments)?,
                        optional_bool(arguments, "is_private", false)?,
                    )
                    .await
            }
            SlackToolKind::ListChannelUsers => {
                reject_unknown_keys(arguments, &["channel_id"])?;
                self.client
                    .list_channel_users(optional_channel(arguments)?.unwrap_or(""))
                    .await
            }
            SlackToolKind::ListWorkspaceUsers => {
                reject_unknown_keys(arguments, &[])?;
                self.client.list_workspace_users().await
            }
            SlackToolKind::Invite => {
                reject_unknown_keys(arguments, &["channel_id", "user_ids"])?;
                let user_ids = invitees(arguments)?;
                self.client
                    .invite_to_conversation(optional_channel(arguments)?.unwrap_or(""), &user_ids)
                    .await
            }
            SlackToolKind::ListWorkspaceConversations => {
                reject_unknown_keys(arguments, &[])?;
                self.client.list_workspace_conversations().await
            }
        }
        .map_err(SlackClientError::into_adk)?;
        Ok(result)
    }
}

fn send_message_schema() -> Value {
    json!({
        "type":"object",
        "properties":{
            "channel_id": channel_property("Destination Slack channel, DM, conversation, or user App Home ID, for example `C12345678`, `D12345678`, or `U12345678`. Omit or pass null to use the configured default conversation."),
            "message":{
                "type":"string", "minLength":1, "maxLength":MAX_MESSAGE_CHARS,
                "description":"Message text to post. Slack applies its default mrkdwn parsing and can unfurl links, so formatting, encoded mentions, and URLs may have visible effects. Keep messages at or below 4,000 characters for reliable reading; the hard limit is 40,000 characters."
            },
            "thread_ts":{
                "type":["string","null"], "maxLength":MAX_THREAD_TS_BYTES, "default":null,
                "description":"Parent message timestamp returned by read_messages, for example `1712345678.000100`. Omit or pass null to create a new top-level message."
            }
        },
        "required":["message"],
        "additionalProperties":false
    })
}

fn read_messages_schema() -> Value {
    json!({
        "type":"object",
        "properties":{
            "channel_id": channel_property("Slack channel, DM, or conversation ID to read, for example `C12345678` or `D12345678`. Omit or pass null to use the configured default conversation."),
            "limit":{
                "type":"integer", "minimum":1, "maximum":MAX_MESSAGE_LIMIT, "default":DEFAULT_MESSAGE_LIMIT,
                "description":"Number of newest messages to return from the first history page. Defaults to 10; accepted range is 1 through 15."
            }
        },
        "additionalProperties":false
    })
}

fn create_channel_schema() -> Value {
    json!({
        "type":"object",
        "properties":{
            "channel_name":{
                "type":"string", "minLength":1, "maxLength":MAX_CHANNEL_NAME_BYTES,
                "pattern":"^[a-z0-9_-]+$",
                "description":"New Slack channel name using 1 through 80 lowercase ASCII letters, digits, hyphens, or underscores, for example `release-status`."
            },
            "is_private":{
                "type":"boolean", "default":false,
                "description":"Set true to create a private channel; defaults to false for a public channel."
            }
        },
        "required":["channel_name"],
        "additionalProperties":false
    })
}

fn channel_schema(description: &str) -> Value {
    json!({
        "type":"object",
        "properties":{"channel_id":channel_property(description)},
        "additionalProperties":false
    })
}

fn invite_schema() -> Value {
    json!({
        "type":"object",
        "properties":{
            "channel_id":channel_property("Destination Slack channel ID, for example `C12345678`. Omit or pass null to use the configured default channel."),
            "user_ids":{
                "type":"array", "minItems":1, "maxItems":MAX_INVITEES, "uniqueItems":true,
                "items":{
                    "type":"string", "minLength":1, "maxLength":MAX_CHANNEL_ID_BYTES,
                    "pattern":"^[A-Za-z0-9]+$"
                },
                "description":"One through 100 unique Slack user IDs to invite, for example [`U12345678`, `U23456789`]."
            }
        },
        "required":["user_ids"],
        "additionalProperties":false
    })
}

fn channel_property(description: &str) -> Value {
    json!({
        "type":["string","null"], "minLength":1, "maxLength":MAX_CHANNEL_ID_BYTES,
        "default":null, "description":description
    })
}

fn empty_schema() -> Value {
    json!({"type":"object","properties":{},"additionalProperties":false})
}

fn validate_argument_size(arguments: &Value) -> Result<(), AdkError> {
    let size = serde_json::to_vec(arguments)
        .map_err(|_| invalid_arguments())?
        .len();
    if size > MAX_ARGUMENT_BYTES {
        return Err(resource_exhausted_arguments());
    }
    Ok(())
}

fn reject_unknown_keys(arguments: &Map<String, Value>, allowed: &[&str]) -> Result<(), AdkError> {
    if arguments.keys().any(|key| !allowed.contains(&key.as_str())) {
        return Err(invalid_arguments());
    }
    Ok(())
}

fn optional_channel(arguments: &Map<String, Value>) -> Result<Option<&str>, AdkError> {
    match arguments.get("channel_id") {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value)) if valid_identifier(value, MAX_CHANNEL_ID_BYTES) => {
            Ok(Some(value))
        }
        Some(_) => Err(invalid_arguments()),
    }
}

fn required_message(arguments: &Map<String, Value>) -> Result<&str, AdkError> {
    arguments
        .get("message")
        .and_then(Value::as_str)
        .filter(|value| {
            !value.is_empty() && value.chars().count() <= MAX_MESSAGE_CHARS && !value.contains('\0')
        })
        .ok_or_else(invalid_arguments)
}

fn optional_thread_ts(arguments: &Map<String, Value>) -> Result<Option<&str>, AdkError> {
    match arguments.get("thread_ts") {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value)) if valid_thread_ts(value) => Ok(Some(value)),
        Some(_) => Err(invalid_arguments()),
    }
}

fn valid_thread_ts(value: &str) -> bool {
    if value.is_empty() || value.len() > MAX_THREAD_TS_BYTES {
        return false;
    }
    let mut parts = value.split('.');
    let (Some(seconds), Some(micros), None) = (parts.next(), parts.next(), parts.next()) else {
        return false;
    };
    !seconds.is_empty()
        && !micros.is_empty()
        && seconds.bytes().all(|byte| byte.is_ascii_digit())
        && micros.bytes().all(|byte| byte.is_ascii_digit())
}

fn message_limit(arguments: &Map<String, Value>) -> Result<usize, AdkError> {
    match arguments.get("limit") {
        None | Some(Value::Null) => Ok(DEFAULT_MESSAGE_LIMIT),
        Some(value) => value
            .as_u64()
            .and_then(|value| usize::try_from(value).ok())
            .filter(|value| (1..=MAX_MESSAGE_LIMIT).contains(value))
            .ok_or_else(invalid_arguments),
    }
}

fn channel_name(arguments: &Map<String, Value>) -> Result<&str, AdkError> {
    arguments
        .get("channel_name")
        .and_then(Value::as_str)
        .filter(|value| {
            !value.is_empty()
                && value.len() <= MAX_CHANNEL_NAME_BYTES
                && value.bytes().all(|byte| {
                    byte.is_ascii_lowercase()
                        || byte.is_ascii_digit()
                        || matches!(byte, b'-' | b'_')
                })
        })
        .ok_or_else(invalid_arguments)
}

fn optional_bool(
    arguments: &Map<String, Value>,
    key: &str,
    default: bool,
) -> Result<bool, AdkError> {
    match arguments.get(key) {
        None | Some(Value::Null) => Ok(default),
        Some(Value::Bool(value)) => Ok(*value),
        Some(_) => Err(invalid_arguments()),
    }
}

fn invitees(arguments: &Map<String, Value>) -> Result<Vec<String>, AdkError> {
    let values = arguments
        .get("user_ids")
        .and_then(Value::as_array)
        .filter(|values| (1..=MAX_INVITEES).contains(&values.len()))
        .ok_or_else(invalid_arguments)?;
    let mut seen = HashSet::with_capacity(values.len());
    let mut users = Vec::with_capacity(values.len());
    for value in values {
        let value = value.as_str().ok_or_else(invalid_arguments)?;
        if !valid_identifier(value, MAX_CHANNEL_ID_BYTES) || !seen.insert(value) {
            return Err(invalid_arguments());
        }
        users.push(value.to_owned());
    }
    Ok(users)
}

fn valid_identifier(value: &str, max: usize) -> bool {
    !value.is_empty()
        && value.len() <= max
        && value.bytes().all(|byte| byte.is_ascii_alphanumeric())
}

fn invalid_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "slack.arguments.invalid",
        "the Slack tool arguments are invalid",
    )
}

fn resource_exhausted_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "slack.arguments.resource_exhausted",
        "the Slack tool arguments exceed the approved limit",
    )
}
