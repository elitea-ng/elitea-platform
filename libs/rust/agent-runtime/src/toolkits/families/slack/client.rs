use std::fmt;
use std::sync::Arc;
use std::time::Duration;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use futures::{StreamExt, stream};
use reqwest::header::{
    ACCEPT, AUTHORIZATION, CONTENT_LENGTH, CONTENT_TYPE, HeaderValue, USER_AGENT,
};
use reqwest::{Method, Request, StatusCode, Url};
use serde_json::{Map, Value, json};
use zeroize::Zeroizing;

use super::config::SlackToolkitConfig;

const SLACK_API_ORIGIN: &str = "https://slack.com/api/";
const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(20);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_mins(1);
const MAX_IDLE_PER_HOST: usize = 8;
const MAX_RESPONSE_BYTES: usize = 2 * 1_024 * 1_024;
const MAX_OUTPUT_BYTES: usize = 512 * 1_024;
const MAX_REQUEST_BYTES: usize = 256 * 1_024;
const MAX_HISTORY_MESSAGES: usize = 15;
const MAX_CHANNEL_MEMBERS: usize = 100;
const MAX_WORKSPACE_USERS: usize = 100;
const MAX_WORKSPACE_CONVERSATIONS: usize = 100;
const MAX_MEMBER_LOOKUPS_IN_FLIGHT: usize = 8;
const USER_AGENT_VALUE: &str = "elitea-worker-rust/0.1";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum SlackClientErrorCode {
    InvalidConfiguration,
    InvalidInput,
    Authentication,
    Authorization,
    NotFound,
    RateLimited,
    Timeout,
    DependencyUnavailable,
    InvalidResponse,
    ResourceExhausted,
    UnknownOutcome,
}

/// One stable Slack failure without credentials, message data, URLs, or
/// provider response text.
pub(crate) struct SlackClientError {
    code: SlackClientErrorCode,
    retryable: bool,
}

impl SlackClientError {
    #[must_use]
    pub(crate) const fn code(&self) -> SlackClientErrorCode {
        self.code
    }

    #[must_use]
    pub(crate) const fn retryable(&self) -> bool {
        self.retryable
    }

    pub(crate) fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code {
            SlackClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "slack.configuration.invalid",
                "the Slack toolkit configuration is invalid",
            ),
            SlackClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "slack.request.invalid",
                "the Slack request is invalid",
            ),
            SlackClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "slack.authentication.failed",
                "Slack authentication failed",
            ),
            SlackClientErrorCode::Authorization => (
                ErrorCategory::Forbidden,
                "slack.authorization.failed",
                "Slack did not authorize the request",
            ),
            SlackClientErrorCode::NotFound => (
                ErrorCategory::NotFound,
                "slack.resource.not_found",
                "the requested Slack resource was not found",
            ),
            SlackClientErrorCode::RateLimited => (
                ErrorCategory::RateLimited,
                "slack.rate_limited",
                "Slack rate limited the request",
            ),
            SlackClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "slack.timeout",
                "the Slack request timed out",
            ),
            SlackClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "slack.unavailable",
                "Slack is unavailable",
            ),
            SlackClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "slack.response.invalid",
                "Slack returned an invalid response",
            ),
            SlackClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "slack.response.resource_exhausted",
                "the Slack response exceeds the approved limit",
            ),
            SlackClientErrorCode::UnknownOutcome => (
                ErrorCategory::Internal,
                "slack.effect.unknown_outcome",
                "Slack may have applied the requested effect; reconcile it before retrying",
            ),
        };
        AdkError::new(ErrorComponent::Tool, category, code, message).with_retry(RetryHint {
            should_retry: self.retryable,
            retry_after_ms: None,
            max_attempts: None,
        })
    }

    #[cfg(test)]
    pub(in crate::toolkits) const fn fixture(code: SlackClientErrorCode, retryable: bool) -> Self {
        Self { code, retryable }
    }
}

impl fmt::Debug for SlackClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("SlackClientError")
            .field("code", &self.code)
            .field("retryable", &self.retryable)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for SlackClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            SlackClientErrorCode::InvalidConfiguration => {
                "the Slack client configuration is invalid"
            }
            SlackClientErrorCode::InvalidInput => "the Slack request is invalid",
            SlackClientErrorCode::Authentication => "Slack authentication failed",
            SlackClientErrorCode::Authorization => "Slack authorization failed",
            SlackClientErrorCode::NotFound => "the Slack resource was not found",
            SlackClientErrorCode::RateLimited => "Slack rate limited the request",
            SlackClientErrorCode::Timeout => "the Slack request timed out",
            SlackClientErrorCode::DependencyUnavailable => "Slack is unavailable",
            SlackClientErrorCode::InvalidResponse => "Slack returned an invalid response",
            SlackClientErrorCode::ResourceExhausted => {
                "the Slack response exceeds its approved limit"
            }
            SlackClientErrorCode::UnknownOutcome => {
                "the Slack effect outcome is unknown and must be reconciled"
            }
        })
    }
}

impl std::error::Error for SlackClientError {}

#[async_trait]
pub(in crate::toolkits) trait SlackApi: Send + Sync {
    async fn send_message(
        &self,
        channel_id: &str,
        message: &str,
        thread_ts: Option<&str>,
    ) -> Result<Value, SlackClientError>;

    async fn read_messages(
        &self,
        channel_id: &str,
        limit: usize,
    ) -> Result<Value, SlackClientError>;

    async fn create_channel(
        &self,
        channel_name: &str,
        is_private: bool,
    ) -> Result<Value, SlackClientError>;

    async fn list_channel_users(&self, channel_id: &str) -> Result<Value, SlackClientError>;

    async fn list_workspace_users(&self) -> Result<Value, SlackClientError>;

    async fn invite_to_conversation(
        &self,
        channel_id: &str,
        user_ids: &[String],
    ) -> Result<Value, SlackClientError>;

    async fn list_workspace_conversations(&self) -> Result<Value, SlackClientError>;
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(in crate::toolkits) enum SlackOperation {
    SendMessage,
    ReadMessages,
    CreateChannel,
    ListChannelMembers,
    UserInfo,
    ListWorkspaceUsers,
    Invite,
    ListWorkspaceConversations,
}

impl SlackOperation {
    const fn path(self) -> &'static str {
        match self {
            Self::SendMessage => "chat.postMessage",
            Self::ReadMessages => "conversations.history",
            Self::CreateChannel => "conversations.create",
            Self::ListChannelMembers => "conversations.members",
            Self::UserInfo => "users.info",
            Self::ListWorkspaceUsers => "users.list",
            Self::Invite => "conversations.invite",
            Self::ListWorkspaceConversations => "conversations.list",
        }
    }

    const fn method(self) -> Method {
        match self {
            Self::ReadMessages
            | Self::ListChannelMembers
            | Self::UserInfo
            | Self::ListWorkspaceUsers
            | Self::ListWorkspaceConversations => Method::GET,
            Self::SendMessage | Self::CreateChannel | Self::Invite => Method::POST,
        }
    }

    const fn is_effect(self) -> bool {
        matches!(self, Self::SendMessage | Self::CreateChannel | Self::Invite)
    }
}

pub(in crate::toolkits) struct SlackHttpResponse {
    status: StatusCode,
    body: Value,
}

impl SlackHttpResponse {
    #[cfg(test)]
    pub(in crate::toolkits) const fn fixture(status: StatusCode, body: Value) -> Self {
        Self { status, body }
    }
}

#[async_trait]
pub(in crate::toolkits) trait SlackTransport: Send + Sync {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<SlackHttpResponse, SlackClientError>;
}

struct ReqwestSlackTransport {
    http: reqwest::Client,
}

#[async_trait]
impl SlackTransport for ReqwestSlackTransport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<SlackHttpResponse, SlackClientError> {
        let mut response = self
            .http
            .execute(request)
            .await
            .map_err(|source| map_reqwest_error(&source, effect))?;
        if response
            .headers()
            .get(CONTENT_LENGTH)
            .and_then(|value| value.to_str().ok())
            .and_then(|value| value.parse::<usize>().ok())
            .is_some_and(|length| length > MAX_RESPONSE_BYTES)
        {
            return Err(response_bound_failure(effect));
        }
        let json_content_type = response
            .headers()
            .get(CONTENT_TYPE)
            .and_then(|value| value.to_str().ok())
            .and_then(|value| value.split(';').next())
            .is_some_and(|value| value.trim().eq_ignore_ascii_case("application/json"));
        let mut bytes = Vec::new();
        while let Some(chunk) = response
            .chunk()
            .await
            .map_err(|source| map_reqwest_error(&source, effect))?
        {
            let next = bytes
                .len()
                .checked_add(chunk.len())
                .ok_or_else(|| response_bound_failure(effect))?;
            if next > MAX_RESPONSE_BYTES {
                return Err(response_bound_failure(effect));
            }
            bytes.extend_from_slice(&chunk);
        }
        if !json_content_type {
            return Err(response_shape_failure(effect));
        }
        let body = serde_json::from_slice(&bytes).map_err(|_| response_shape_failure(effect))?;
        Ok(SlackHttpResponse {
            status: response.status(),
            body,
        })
    }
}

/// One invocation-scoped Slack token, API authority, and HTTP pool.
pub(crate) struct SlackClient {
    config: SlackToolkitConfig,
    transport: Arc<dyn SlackTransport>,
}

impl SlackClient {
    pub(crate) fn new(config: SlackToolkitConfig) -> Result<Self, SlackClientError> {
        let http = reqwest::Client::builder()
            .https_only(true)
            .redirect(reqwest::redirect::Policy::none())
            .retry(reqwest::retry::never())
            .connect_timeout(CONNECT_TIMEOUT)
            .timeout(REQUEST_TIMEOUT)
            .pool_idle_timeout(POOL_IDLE_TIMEOUT)
            .pool_max_idle_per_host(MAX_IDLE_PER_HOST)
            .user_agent(USER_AGENT_VALUE)
            .build()
            .map_err(|_| invalid_configuration())?;
        Ok(Self {
            config,
            transport: Arc::new(ReqwestSlackTransport { http }),
        })
    }

    fn request(
        &self,
        operation: SlackOperation,
        parameters: &Map<String, Value>,
    ) -> Result<Request, SlackClientError> {
        build_request(self.config.token(), operation, parameters)
    }

    async fn call(
        &self,
        operation: SlackOperation,
        parameters: &Map<String, Value>,
    ) -> Result<Map<String, Value>, SlackClientError> {
        call_transport(
            &self.transport,
            self.request(operation, parameters)?,
            operation.is_effect(),
        )
        .await
    }

    fn configured_channel<'a>(
        &'a self,
        supplied: Option<&'a str>,
    ) -> Result<&'a str, SlackClientError> {
        supplied
            .filter(|value| !value.is_empty())
            .or(self.config.default_channel_id())
            .ok_or_else(invalid_input)
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn with_transport(
        config: SlackToolkitConfig,
        transport: Arc<dyn SlackTransport>,
    ) -> Self {
        Self { config, transport }
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn test_request(
        &self,
        operation: SlackOperation,
        parameters: &Map<String, Value>,
    ) -> Result<Request, SlackClientError> {
        self.request(operation, parameters)
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn test_configured_channel<'a>(
        &'a self,
        supplied: Option<&'a str>,
    ) -> Result<&'a str, SlackClientError> {
        self.configured_channel(supplied)
    }
}

#[async_trait]
impl SlackApi for SlackClient {
    async fn send_message(
        &self,
        channel_id: &str,
        message: &str,
        thread_ts: Option<&str>,
    ) -> Result<Value, SlackClientError> {
        let channel = self.configured_channel(Some(channel_id))?;
        let mut parameters = Map::from_iter([
            ("channel".to_owned(), Value::String(channel.to_owned())),
            ("text".to_owned(), Value::String(message.to_owned())),
        ]);
        if let Some(thread_ts) = thread_ts {
            parameters.insert("thread_ts".to_owned(), Value::String(thread_ts.to_owned()));
        }
        let response = self.call(SlackOperation::SendMessage, &parameters).await?;
        let mut sent = Map::from_iter([
            ("success".to_owned(), Value::Bool(true)),
            (
                "channel_id".to_owned(),
                Value::String(
                    optional_text(&response, "channel")
                        .map_err(|_| unknown_outcome())?
                        .unwrap_or(channel)
                        .to_owned(),
                ),
            ),
            (
                "ts".to_owned(),
                Value::String(
                    required_text(&response, "ts")
                        .map_err(|_| unknown_outcome())?
                        .to_owned(),
                ),
            ),
        ]);
        if let Some(thread_ts) = thread_ts {
            sent.insert("thread_ts".to_owned(), Value::String(thread_ts.to_owned()));
        }
        bounded_output(Value::Object(sent)).map_err(|_| unknown_outcome())
    }

    async fn read_messages(
        &self,
        channel_id: &str,
        limit: usize,
    ) -> Result<Value, SlackClientError> {
        if !(1..=MAX_HISTORY_MESSAGES).contains(&limit) {
            return Err(invalid_input());
        }
        let channel = self.configured_channel(Some(channel_id))?;
        let parameters = Map::from_iter([
            ("channel".to_owned(), Value::String(channel.to_owned())),
            (
                "limit".to_owned(),
                Value::Number(u64::try_from(limit).map_err(|_| invalid_input())?.into()),
            ),
        ]);
        let response = self.call(SlackOperation::ReadMessages, &parameters).await?;
        let messages = bounded_array(response.get("messages"), MAX_HISTORY_MESSAGES)?;
        let mut projected = Vec::with_capacity(messages.len());
        for message in messages {
            let message = message.as_object().ok_or_else(invalid_response)?;
            let mut item = Map::from_iter([
                ("ts".to_owned(), nullable_text(message.get("ts"), None)?),
                (
                    "user".to_owned(),
                    nullable_text(message.get("user"), Some("Undefined User"))?,
                ),
                (
                    "message".to_owned(),
                    nullable_text(message.get("text"), Some("No message"))?,
                ),
                ("app_name".to_owned(), bot_name(message.get("bot_profile"))?),
            ]);
            if let Some(thread_ts) = optional_value_text(message.get("thread_ts"))? {
                item.insert("thread_ts".to_owned(), Value::String(thread_ts.to_owned()));
            }
            projected.push(Value::Object(item));
        }
        bounded_output(Value::Array(projected))
    }

    async fn create_channel(
        &self,
        channel_name: &str,
        is_private: bool,
    ) -> Result<Value, SlackClientError> {
        let parameters = Map::from_iter([
            ("name".to_owned(), Value::String(channel_name.to_owned())),
            ("is_private".to_owned(), Value::Bool(is_private)),
        ]);
        let response = self
            .call(SlackOperation::CreateChannel, &parameters)
            .await?;
        let channel = response
            .get("channel")
            .and_then(Value::as_object)
            .ok_or_else(|| response_shape_failure(true))?;
        bounded_output(json!({
            "success": true,
            "channel_id": required_text(channel, "id").map_err(|_| unknown_outcome())?
        }))
        .map_err(|_| unknown_outcome())
    }

    async fn list_channel_users(&self, channel_id: &str) -> Result<Value, SlackClientError> {
        let channel = self.configured_channel(Some(channel_id))?;
        let parameters = Map::from_iter([
            ("channel".to_owned(), Value::String(channel.to_owned())),
            (
                "limit".to_owned(),
                Value::Number((MAX_CHANNEL_MEMBERS as u64).into()),
            ),
        ]);
        let response = self
            .call(SlackOperation::ListChannelMembers, &parameters)
            .await?;
        let members = bounded_array(response.get("members"), MAX_CHANNEL_MEMBERS)?;
        let mut ids = Vec::with_capacity(members.len());
        for member in members {
            let id = member.as_str().ok_or_else(invalid_response)?;
            ids.push(id.to_owned());
        }

        let mut outcomes = stream::iter(ids.into_iter().enumerate())
            .map(|(index, id)| async move {
                let parameters = Map::from_iter([("user".to_owned(), Value::String(id))]);
                let request = self.request(SlackOperation::UserInfo, &parameters)?;
                let response = call_transport(&self.transport, request, false).await?;
                let user = response
                    .get("user")
                    .and_then(Value::as_object)
                    .ok_or_else(invalid_response)?;
                let projected = json!({
                    "id": nullable_text(user.get("id"), None)?,
                    "name": nullable_text(user.get("name"), None)?
                });
                Ok::<_, SlackClientError>((index, projected))
            })
            .buffer_unordered(MAX_MEMBER_LOOKUPS_IN_FLIGHT)
            .collect::<Vec<_>>()
            .await;
        let mut projected = Vec::with_capacity(outcomes.len());
        for outcome in outcomes.drain(..) {
            projected.push(outcome?);
        }
        projected.sort_unstable_by_key(|(index, _)| *index);
        bounded_output(Value::Array(
            projected.into_iter().map(|(_, value)| value).collect(),
        ))
    }

    async fn list_workspace_users(&self) -> Result<Value, SlackClientError> {
        let parameters = Map::from_iter([(
            "limit".to_owned(),
            Value::Number((MAX_WORKSPACE_USERS as u64).into()),
        )]);
        let response = self
            .call(SlackOperation::ListWorkspaceUsers, &parameters)
            .await?;
        let members = bounded_array(response.get("members"), MAX_WORKSPACE_USERS)?;
        let mut projected = Vec::with_capacity(members.len());
        for member in members {
            let member = member.as_object().ok_or_else(invalid_response)?;
            let profile = member.get("profile").and_then(Value::as_object);
            projected.push(json!({
                "id": nullable_text(member.get("id"), None)?,
                "name": nullable_text(member.get("name"), None)?,
                "is_bot": nullable_bool(member.get("is_bot"))?,
                "email": nullable_text(profile.and_then(|value| value.get("email")), None)?,
                "team": nullable_text(profile.and_then(|value| value.get("team")), None)?
            }));
        }
        bounded_output(Value::Array(projected))
    }

    async fn invite_to_conversation(
        &self,
        channel_id: &str,
        user_ids: &[String],
    ) -> Result<Value, SlackClientError> {
        let channel = self.configured_channel(Some(channel_id))?;
        let parameters = Map::from_iter([
            ("channel".to_owned(), Value::String(channel.to_owned())),
            ("users".to_owned(), Value::String(user_ids.join(","))),
        ]);
        let response = self.call(SlackOperation::Invite, &parameters).await?;
        bounded_output(Value::Object(response)).map_err(|_| unknown_outcome())
    }

    async fn list_workspace_conversations(&self) -> Result<Value, SlackClientError> {
        let parameters = Map::from_iter([(
            "limit".to_owned(),
            Value::Number((MAX_WORKSPACE_CONVERSATIONS as u64).into()),
        )]);
        let response = self
            .call(SlackOperation::ListWorkspaceConversations, &parameters)
            .await?;
        let channels = bounded_array(response.get("channels"), MAX_WORKSPACE_CONVERSATIONS)?;
        let mut projected = Vec::with_capacity(channels.len());
        for channel in channels {
            let channel = channel.as_object().ok_or_else(invalid_response)?;
            projected.push(json!({
                "id": nullable_text(channel.get("id"), None)?,
                "name": nullable_text(channel.get("name"), None)?,
                "is_channel": nullable_bool(channel.get("is_channel"))?,
                "shared_team_ids": nullable_text_array(channel.get("shared_team_ids"))?
            }));
        }
        bounded_output(Value::Array(projected))
    }
}

fn build_request(
    token: &str,
    operation: SlackOperation,
    parameters: &Map<String, Value>,
) -> Result<Request, SlackClientError> {
    let mut endpoint = Url::parse(SLACK_API_ORIGIN).map_err(|_| invalid_configuration())?;
    endpoint.set_path(&format!("/api/{}", operation.path()));
    let authorization_text = Zeroizing::new(format!("Bearer {token}"));
    let mut authorization =
        HeaderValue::from_str(&authorization_text).map_err(|_| invalid_configuration())?;
    authorization.set_sensitive(true);
    let mut request = Request::new(operation.method(), endpoint);
    request.headers_mut().insert(
        ACCEPT,
        HeaderValue::from_static("application/json; charset=utf-8"),
    );
    request
        .headers_mut()
        .insert(USER_AGENT, HeaderValue::from_static(USER_AGENT_VALUE));
    request.headers_mut().insert(AUTHORIZATION, authorization);
    if operation.method() == Method::GET {
        {
            let mut query = request.url_mut().query_pairs_mut();
            for (name, value) in parameters {
                query.append_pair(name, &query_value(value)?);
            }
        }
    } else {
        let body = serde_json::to_vec(parameters).map_err(|_| invalid_input())?;
        if body.len() > MAX_REQUEST_BYTES {
            return Err(resource_exhausted());
        }
        request
            .headers_mut()
            .insert(CONTENT_TYPE, HeaderValue::from_static("application/json"));
        *request.body_mut() = Some(body.into());
    }
    *request.timeout_mut() = Some(REQUEST_TIMEOUT);
    Ok(request)
}

async fn call_transport(
    transport: &Arc<dyn SlackTransport>,
    request: Request,
    effect: bool,
) -> Result<Map<String, Value>, SlackClientError> {
    let response = transport.execute(request, effect).await?;
    map_http_status(response.status, effect)?;
    let body = response
        .body
        .as_object()
        .ok_or_else(|| response_shape_failure(effect))?;
    match body.get("ok").and_then(Value::as_bool) {
        Some(true) => Ok(body.clone()),
        Some(false) => Err(map_slack_error(
            body.get("error").and_then(Value::as_str),
            effect,
        )),
        None => Err(response_shape_failure(effect)),
    }
}

fn query_value(value: &Value) -> Result<String, SlackClientError> {
    match value {
        Value::String(value) => Ok(value.clone()),
        Value::Bool(value) => Ok(value.to_string()),
        Value::Number(value) => Ok(value.to_string()),
        _ => Err(invalid_input()),
    }
}

fn required_text<'a>(
    object: &'a Map<String, Value>,
    key: &str,
) -> Result<&'a str, SlackClientError> {
    object
        .get(key)
        .and_then(Value::as_str)
        .filter(|value| !value.is_empty())
        .ok_or_else(invalid_response)
}

fn optional_text<'a>(
    object: &'a Map<String, Value>,
    key: &str,
) -> Result<Option<&'a str>, SlackClientError> {
    optional_value_text(object.get(key))
}

fn optional_value_text(value: Option<&Value>) -> Result<Option<&str>, SlackClientError> {
    match value {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value)) => Ok(Some(value)),
        Some(_) => Err(invalid_response()),
    }
}

fn nullable_text(value: Option<&Value>, fallback: Option<&str>) -> Result<Value, SlackClientError> {
    match value {
        None => Ok(fallback.map_or(Value::Null, |value| Value::String(value.to_owned()))),
        Some(Value::Null) => Ok(Value::Null),
        Some(Value::String(value)) => Ok(Value::String(value.clone())),
        Some(_) => Err(invalid_response()),
    }
}

fn nullable_bool(value: Option<&Value>) -> Result<Value, SlackClientError> {
    match value {
        None | Some(Value::Null) => Ok(Value::Null),
        Some(Value::Bool(value)) => Ok(Value::Bool(*value)),
        Some(_) => Err(invalid_response()),
    }
}

fn nullable_text_array(value: Option<&Value>) -> Result<Value, SlackClientError> {
    match value {
        None | Some(Value::Null) => Ok(Value::Null),
        Some(Value::Array(values)) => {
            let mut projected = Vec::with_capacity(values.len());
            for value in values {
                let value = value.as_str().ok_or_else(invalid_response)?;
                projected.push(Value::String(value.to_owned()));
            }
            Ok(Value::Array(projected))
        }
        Some(_) => Err(invalid_response()),
    }
}

fn bot_name(value: Option<&Value>) -> Result<Value, SlackClientError> {
    match value {
        None | Some(Value::Null) => Ok(Value::String("No App Name".to_owned())),
        Some(Value::Object(profile)) => nullable_text(profile.get("name"), Some("No App Name")),
        Some(_) => Err(invalid_response()),
    }
}

fn bounded_array(value: Option<&Value>, max: usize) -> Result<&[Value], SlackClientError> {
    let values = value
        .and_then(Value::as_array)
        .ok_or_else(invalid_response)?;
    if values.len() > max {
        return Err(resource_exhausted());
    }
    Ok(values)
}

fn bounded_output(value: Value) -> Result<Value, SlackClientError> {
    let size = serde_json::to_vec(&value)
        .map_err(|_| invalid_response())?
        .len();
    if size > MAX_OUTPUT_BYTES {
        return Err(resource_exhausted());
    }
    Ok(value)
}

fn map_http_status(status: StatusCode, effect: bool) -> Result<(), SlackClientError> {
    if status.is_success() {
        return Ok(());
    }
    let code = match status {
        StatusCode::BAD_REQUEST | StatusCode::UNPROCESSABLE_ENTITY => {
            SlackClientErrorCode::InvalidInput
        }
        StatusCode::UNAUTHORIZED => SlackClientErrorCode::Authentication,
        StatusCode::FORBIDDEN => SlackClientErrorCode::Authorization,
        StatusCode::NOT_FOUND => SlackClientErrorCode::NotFound,
        StatusCode::TOO_MANY_REQUESTS if !effect => SlackClientErrorCode::RateLimited,
        StatusCode::TOO_MANY_REQUESTS => SlackClientErrorCode::RateLimited,
        status if status.is_server_error() && effect => SlackClientErrorCode::UnknownOutcome,
        status if status.is_server_error() => SlackClientErrorCode::DependencyUnavailable,
        _ if effect => SlackClientErrorCode::UnknownOutcome,
        _ => SlackClientErrorCode::InvalidResponse,
    };
    Err(error(
        code,
        !effect
            && matches!(
                code,
                SlackClientErrorCode::RateLimited | SlackClientErrorCode::DependencyUnavailable
            ),
    ))
}

fn map_slack_error(error_code: Option<&str>, effect: bool) -> SlackClientError {
    let code = match error_code {
        Some("not_authed" | "invalid_auth" | "token_revoked" | "account_inactive") => {
            SlackClientErrorCode::Authentication
        }
        Some(
            "missing_scope"
            | "no_permission"
            | "restricted_action"
            | "restricted_action_read_only_channel"
            | "org_login_required",
        ) => SlackClientErrorCode::Authorization,
        Some("channel_not_found" | "user_not_found" | "message_not_found") => {
            SlackClientErrorCode::NotFound
        }
        Some("ratelimited") => SlackClientErrorCode::RateLimited,
        Some("fatal_error" | "internal_error" | "service_unavailable") if effect => {
            SlackClientErrorCode::UnknownOutcome
        }
        Some("fatal_error" | "internal_error" | "service_unavailable") => {
            SlackClientErrorCode::DependencyUnavailable
        }
        Some(_) => SlackClientErrorCode::InvalidInput,
        None if effect => SlackClientErrorCode::UnknownOutcome,
        None => SlackClientErrorCode::InvalidResponse,
    };
    error(
        code,
        !effect
            && matches!(
                code,
                SlackClientErrorCode::RateLimited | SlackClientErrorCode::DependencyUnavailable
            ),
    )
}

fn map_reqwest_error(source: &reqwest::Error, effect: bool) -> SlackClientError {
    if effect {
        return unknown_outcome();
    }
    if source.is_timeout() {
        return error(SlackClientErrorCode::Timeout, true);
    }
    if source.is_connect() || source.is_request() || source.is_body() || source.is_decode() {
        return error(SlackClientErrorCode::DependencyUnavailable, true);
    }
    error(SlackClientErrorCode::InvalidResponse, false)
}

fn response_bound_failure(effect: bool) -> SlackClientError {
    if effect {
        unknown_outcome()
    } else {
        resource_exhausted()
    }
}

fn response_shape_failure(effect: bool) -> SlackClientError {
    if effect {
        unknown_outcome()
    } else {
        invalid_response()
    }
}

const fn error(code: SlackClientErrorCode, retryable: bool) -> SlackClientError {
    SlackClientError { code, retryable }
}

const fn invalid_configuration() -> SlackClientError {
    error(SlackClientErrorCode::InvalidConfiguration, false)
}

const fn invalid_input() -> SlackClientError {
    error(SlackClientErrorCode::InvalidInput, false)
}

const fn invalid_response() -> SlackClientError {
    error(SlackClientErrorCode::InvalidResponse, false)
}

const fn resource_exhausted() -> SlackClientError {
    error(SlackClientErrorCode::ResourceExhausted, false)
}

const fn unknown_outcome() -> SlackClientError {
    error(SlackClientErrorCode::UnknownOutcome, false)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_map_http_status(
    status: StatusCode,
    effect: bool,
) -> SlackClientError {
    map_http_status(status, effect).expect_err("non-success status must fail")
}

#[cfg(test)]
pub(in crate::toolkits) fn test_map_slack_error(
    provider_code: Option<&str>,
    effect: bool,
) -> SlackClientError {
    map_slack_error(provider_code, effect)
}
