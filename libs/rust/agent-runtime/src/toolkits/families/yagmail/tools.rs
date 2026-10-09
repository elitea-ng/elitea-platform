use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{
    MAX_CC_RECIPIENTS, MAX_MAILBOX_BYTES, MAX_MESSAGE_BYTES, MAX_MESSAGE_CHARS, MAX_SUBJECT_CHARS,
    YagmailApi, YagmailClient, YagmailClientError, validate_mailbox, validate_message,
};
use super::config::{YagmailConfigError, YagmailToolkitConfig};

const SEND_GMAIL_MESSAGE: &str = "send_gmail_message";
const MAX_ARGUMENT_BYTES: usize = 256 * 1_024;
const MAX_ARGUMENT_NODES: usize = 1_024;
const MAX_ARGUMENT_DEPTH: usize = 8;
const MAX_DESCRIPTION_BYTES: usize = 1_000;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum YagmailToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedSelection,
    Client,
    InvalidDefinition,
}

/// Safe construction failure for the complete one-tool Yagmail family.
pub(crate) struct YagmailToolsetError {
    code: YagmailToolsetErrorCode,
}

impl YagmailToolsetError {
    #[must_use]
    pub(crate) const fn code(&self) -> YagmailToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for YagmailToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("YagmailToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for YagmailToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            YagmailToolsetErrorCode::InvalidConfiguration => {
                "the Yagmail toolkit configuration is invalid"
            }
            YagmailToolsetErrorCode::ResourceExhausted => {
                "the Yagmail toolkit configuration exceeds its approved limit"
            }
            YagmailToolsetErrorCode::UnsupportedSelection => {
                "the selected Yagmail tool profile is not supported"
            }
            YagmailToolsetErrorCode::Client => "the Yagmail client could not be created",
            YagmailToolsetErrorCode::InvalidDefinition => {
                "the Yagmail ADK tool definition is invalid"
            }
        })
    }
}

impl std::error::Error for YagmailToolsetError {}

impl From<YagmailConfigError> for YagmailToolsetError {
    fn from(source: YagmailConfigError) -> Self {
        Self {
            code: match source.code() {
                super::config::YagmailConfigErrorCode::InvalidConfiguration => {
                    YagmailToolsetErrorCode::InvalidConfiguration
                }
                super::config::YagmailConfigErrorCode::ResourceExhausted => {
                    YagmailToolsetErrorCode::ResourceExhausted
                }
            },
        }
    }
}

impl From<YagmailClientError> for YagmailToolsetError {
    fn from(_: YagmailClientError) -> Self {
        Self {
            code: YagmailToolsetErrorCode::Client,
        }
    }
}

impl From<MaterializedToolsetError> for YagmailToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: YagmailToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// Build the complete, capability-disabled Yagmail toolset.
///
/// The SDK's `write` group remains ordinary operation metadata. Independent
/// policy may still require approval for this send effect.
pub(crate) fn build_yagmail_toolset(
    toolkit_name: &str,
    config: YagmailToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, YagmailToolsetError> {
    validate_selection(config.selected_tools())?;
    let selected = config
        .selected_tools()
        .iter()
        .map(ToString::to_string)
        .collect::<Vec<_>>();
    let client: Arc<dyn YagmailApi> = Arc::new(YagmailClient::new(config)?);
    build_with_api(toolkit_name, &selected, policy, &client)
}

fn validate_selection(selected: &[Box<str>]) -> Result<(), YagmailToolsetError> {
    if selected
        .iter()
        .any(|name| name.as_ref() != SEND_GMAIL_MESSAGE)
    {
        return Err(YagmailToolsetError {
            code: YagmailToolsetErrorCode::UnsupportedSelection,
        });
    }
    Ok(())
}

fn build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn YagmailApi>,
) -> Result<BasicToolset, YagmailToolsetError> {
    let include = selected.is_empty() || selected.iter().any(|name| name == SEND_GMAIL_MESSAGE);
    let tools: Vec<Arc<dyn Tool>> = if include {
        vec![Arc::new(YagmailTool::new(toolkit_name, Arc::clone(client)))]
    } else {
        Vec::new()
    };
    admit_materialized_toolset(toolkit_name, "yagmail", policy, tools).map_err(Into::into)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn YagmailApi>,
) -> Result<BasicToolset, YagmailToolsetError> {
    if selected.iter().any(|name| name != SEND_GMAIL_MESSAGE) {
        return Err(YagmailToolsetError {
            code: YagmailToolsetErrorCode::UnsupportedSelection,
        });
    }
    build_with_api(toolkit_name, selected, policy, client)
}

struct YagmailTool {
    client: Arc<dyn YagmailApi>,
    description: Box<str>,
}

impl YagmailTool {
    fn new(toolkit_name: &str, client: Arc<dyn YagmailApi>) -> Self {
        const ACTION: &str = "Send one email through the configured implicit-TLS SMTP authority. receiver is one envelope recipient, message is literal UTF-8 text (never a file path or attachment), subject may be empty, and cc is an optional list of copy recipients. The result is an empty object when every recipient is accepted, or a bounded sent receipt listing refused recipients by address and SMTP code when delivery proceeds to others. This is a remote effect and may independently require approval under sensitivity policy. It makes exactly one send attempt with no automatic retry; if the outcome becomes unknown after SMTP DATA, reconcile delivery before retrying because a retry can duplicate the email. Production activation also requires a durable interrupt/effect identity; the invocation call ID only seeds this disabled slice's Message-ID.";
        let prefix_bytes = "Toolkit: \n".len();
        let name_budget = MAX_DESCRIPTION_BYTES.saturating_sub(prefix_bytes + ACTION.len());
        let bounded_name = truncate_utf8(toolkit_name, name_budget);
        let description = format!("Toolkit: {bounded_name}\n{ACTION}");
        Self {
            client,
            description: description.into_boxed_str(),
        }
    }
}

fn truncate_utf8(value: &str, max_bytes: usize) -> &str {
    if value.len() <= max_bytes {
        return value;
    }
    let mut end = max_bytes;
    while end != 0 && !value.is_char_boundary(end) {
        end -= 1;
    }
    &value[..end]
}

#[async_trait]
impl Tool for YagmailTool {
    fn name(&self) -> &str {
        SEND_GMAIL_MESSAGE
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn is_read_only(&self) -> bool {
        false
    }

    fn is_concurrency_safe(&self) -> bool {
        false
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(send_schema())
    }

    async fn execute(
        &self,
        context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        validate_json(&arguments)?;
        let arguments = arguments.as_object().ok_or_else(invalid_arguments)?;
        reject_unknown_keys(arguments, &["receiver", "message", "subject", "cc"])?;
        let receiver = required_string(arguments, "receiver")?;
        let message = required_string(arguments, "message")?;
        let subject = required_string(arguments, "subject")?;
        let cc = optional_cc(arguments)?;
        validate_message(receiver, message, subject, &cc).map_err(YagmailClientError::into_adk)?;
        self.client
            .send_gmail_message(context.function_call_id(), receiver, message, subject, &cc)
            .await
            .map_err(YagmailClientError::into_adk)
    }
}

fn send_schema() -> Value {
    json!({
        "title": "GmailSendMessageStep",
        "type": "object",
        "properties": {
            "receiver": {
                "type": "string",
                "format": "email",
                "minLength": 1,
                "maxLength": MAX_MAILBOX_BYTES,
                "description": "One ASCII envelope recipient in addr-spec form, for example person@example.com. The encoded value is limited to 254 bytes."
            },
            "message": {
                "type": "string",
                "maxLength": MAX_MESSAGE_CHARS,
                "description": "Literal UTF-8 message text, with an empty string allowed. It is sent as escaped text/plain and text/html alternatives and is never interpreted as a local path or attachment. The value is limited to 12288 Unicode characters and 49152 UTF-8 bytes."
            },
            "subject": {
                "type": "string",
                "maxLength": MAX_SUBJECT_CHARS,
                "description": "Email subject text; an empty string omits the Subject header. CR/LF and other control characters are rejected. The value is limited to 249 Unicode characters and 998 UTF-8 bytes."
            },
            "cc": {
                "anyOf": [{
                    "type": "array",
                    "maxItems": MAX_CC_RECIPIENTS,
                    "items": {
                        "type": "string",
                        "format": "email",
                        "minLength": 1,
                        "maxLength": MAX_MAILBOX_BYTES
                    }
                }, {"type": "null"}],
                "default": null,
                "description": "Optional copy recipients in source order, at most 100 ASCII addr-spec addresses; null or omission sends no copies."
            }
        },
        "required": ["receiver", "message", "subject"],
        "additionalProperties": false
    })
}

fn required_string<'a>(arguments: &'a Map<String, Value>, name: &str) -> adk_core::Result<&'a str> {
    arguments
        .get(name)
        .and_then(Value::as_str)
        .ok_or_else(invalid_arguments)
}

fn optional_cc(arguments: &Map<String, Value>) -> adk_core::Result<Vec<String>> {
    let Some(value) = arguments.get("cc") else {
        return Ok(Vec::new());
    };
    if value.is_null() {
        return Ok(Vec::new());
    }
    let values = value.as_array().ok_or_else(invalid_arguments)?;
    if values.len() > MAX_CC_RECIPIENTS {
        return Err(resource_exhausted());
    }
    let mut cc = Vec::with_capacity(values.len());
    for value in values {
        let value = value.as_str().ok_or_else(invalid_arguments)?;
        validate_mailbox(value).map_err(YagmailClientError::into_adk)?;
        cc.push(value.to_owned());
    }
    Ok(cc)
}

fn reject_unknown_keys(arguments: &Map<String, Value>, allowed: &[&str]) -> adk_core::Result<()> {
    if arguments.keys().any(|key| !allowed.contains(&key.as_str())) {
        return Err(invalid_arguments());
    }
    Ok(())
}

fn validate_json(value: &Value) -> adk_core::Result<()> {
    if serde_json::to_vec(value)
        .map_err(|_| invalid_arguments())?
        .len()
        > MAX_ARGUMENT_BYTES
    {
        return Err(resource_exhausted());
    }
    let mut nodes = 0usize;
    let mut stack = vec![(value, 0usize)];
    while let Some((node, depth)) = stack.pop() {
        nodes = nodes.checked_add(1).ok_or_else(resource_exhausted)?;
        if nodes > MAX_ARGUMENT_NODES || depth > MAX_ARGUMENT_DEPTH {
            return Err(resource_exhausted());
        }
        match node {
            Value::String(value) if value.len() > MAX_MESSAGE_BYTES.max(MAX_MAILBOX_BYTES) => {
                return Err(resource_exhausted());
            }
            Value::Array(values) => {
                stack.extend(values.iter().map(|value| (value, depth + 1)));
            }
            Value::Object(values) => {
                stack.extend(values.values().map(|value| (value, depth + 1)));
            }
            Value::Null | Value::Bool(_) | Value::Number(_) | Value::String(_) => {}
        }
    }
    Ok(())
}

fn invalid_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "yagmail.arguments.invalid",
        "the Yagmail tool arguments are invalid",
    )
}

fn resource_exhausted() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "yagmail.arguments.resource_exhausted",
        "the Yagmail tool arguments exceed the approved limit",
    )
}
