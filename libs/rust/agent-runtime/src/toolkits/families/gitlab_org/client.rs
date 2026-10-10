use std::fmt;
use std::sync::Arc;
use std::time::Duration;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD;
use chrono::DateTime;
use reqwest::header::{ACCEPT, CONTENT_LENGTH, CONTENT_TYPE, HeaderName, HeaderValue};
use reqwest::{Method, Request, StatusCode, Url};
use serde_json::{Map, Value, json};
use tokio::sync::Mutex;
use zeroize::Zeroizing;

use super::config::GitLabOrgToolkitConfig;
use super::diff::{DiffErrorCode, discussion_position, format_changes};
use super::edit::{EditErrorCode, apply_update};
use crate::toolkits::families::python_repr::repr_str;
use crate::toolkits::families::vcs_text::python_line_ranges;

const PRIVATE_TOKEN: HeaderName = HeaderName::from_static("private-token");
const CONNECT_TIMEOUT: Duration = Duration::from_secs(10);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(30);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_mins(1);
const MAX_IDLE_PER_HOST: usize = 8;
const MAX_RESPONSE_BYTES: usize = 2 * 1_024 * 1_024;
const MAX_OUTPUT_BYTES: usize = 512 * 1_024;
const MAX_REQUEST_BYTES: usize = 2 * 1_024 * 1_024;
const MAX_FILE_BYTES: usize = 1_024 * 1_024;
const MAX_WRITABLE_FILE_BYTES: usize = 256 * 1_024;
const MAX_OUTPUT_CHARS: usize = 200_000;
const MAX_PATH_BYTES: usize = 1_024;
const MAX_BRANCH_BYTES: usize = 255;
const MAX_IDENTIFIER_BYTES: usize = 1_024;
const MAX_TEXT_BYTES: usize = 256 * 1_024;
const MAX_PAGES: usize = 10;
const MAX_ITEMS: usize = 1_000;
const MAX_COMMITS: usize = 1_000;
const USER_AGENT: &str = "elitea-worker-rust/0.1";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum GitLabOrgClientErrorCode {
    InvalidConfiguration,
    InvalidInput,
    Authentication,
    Authorization,
    NotFound,
    Conflict,
    RateLimited,
    Timeout,
    DependencyUnavailable,
    InvalidResponse,
    ResourceExhausted,
    UnknownOutcome,
}

/// Stable provider failure without origin, repository, path, body, or token.
pub(crate) struct GitLabOrgClientError {
    code: GitLabOrgClientErrorCode,
    retryable: bool,
}

impl GitLabOrgClientError {
    #[must_use]
    pub(crate) const fn code(&self) -> GitLabOrgClientErrorCode {
        self.code
    }

    #[must_use]
    pub(crate) const fn retryable(&self) -> bool {
        self.retryable
    }

    pub(crate) fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code {
            GitLabOrgClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "gitlab_org.configuration.invalid",
                "the GitLab Org toolkit configuration is invalid",
            ),
            GitLabOrgClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "gitlab_org.request.invalid",
                "the GitLab Org request is invalid",
            ),
            GitLabOrgClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "gitlab_org.authentication.failed",
                "GitLab authentication failed",
            ),
            GitLabOrgClientErrorCode::Authorization => (
                ErrorCategory::Forbidden,
                "gitlab_org.authorization.failed",
                "GitLab did not authorize the request",
            ),
            GitLabOrgClientErrorCode::NotFound => (
                ErrorCategory::NotFound,
                "gitlab_org.resource.not_found",
                "the requested GitLab resource was not found",
            ),
            GitLabOrgClientErrorCode::Conflict => (
                ErrorCategory::InvalidInput,
                "gitlab_org.resource.conflict",
                "the GitLab resource changed or already exists",
            ),
            GitLabOrgClientErrorCode::RateLimited => (
                ErrorCategory::RateLimited,
                "gitlab_org.rate_limited",
                "GitLab rate limited the request",
            ),
            GitLabOrgClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "gitlab_org.timeout",
                "the GitLab request timed out",
            ),
            GitLabOrgClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "gitlab_org.unavailable",
                "GitLab is unavailable",
            ),
            GitLabOrgClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "gitlab_org.response.invalid",
                "GitLab returned an invalid response",
            ),
            GitLabOrgClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "gitlab_org.resource_exhausted",
                "the GitLab Org request or response exceeds the approved limit",
            ),
            GitLabOrgClientErrorCode::UnknownOutcome => (
                ErrorCategory::Internal,
                "gitlab_org.effect.unknown_outcome",
                "GitLab may have applied the requested effect; reconcile it before retrying",
            ),
        };
        AdkError::new(ErrorComponent::Tool, category, code, message).with_retry(RetryHint {
            should_retry: self.retryable,
            retry_after_ms: None,
            max_attempts: None,
        })
    }

    #[cfg(test)]
    pub(in crate::toolkits) const fn fixture(
        code: GitLabOrgClientErrorCode,
        retryable: bool,
    ) -> Self {
        Self { code, retryable }
    }
}

impl fmt::Debug for GitLabOrgClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("GitLabOrgClientError")
            .field("code", &self.code)
            .field("retryable", &self.retryable)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for GitLabOrgClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            GitLabOrgClientErrorCode::InvalidConfiguration => {
                "the GitLab Org client configuration is invalid"
            }
            GitLabOrgClientErrorCode::InvalidInput => "the GitLab Org request is invalid",
            GitLabOrgClientErrorCode::Authentication => "GitLab authentication failed",
            GitLabOrgClientErrorCode::Authorization => "GitLab authorization failed",
            GitLabOrgClientErrorCode::NotFound => "the GitLab resource was not found",
            GitLabOrgClientErrorCode::Conflict => "the GitLab resource is in conflict",
            GitLabOrgClientErrorCode::RateLimited => "GitLab rate limited the request",
            GitLabOrgClientErrorCode::Timeout => "the GitLab request timed out",
            GitLabOrgClientErrorCode::DependencyUnavailable => "GitLab is unavailable",
            GitLabOrgClientErrorCode::InvalidResponse => "GitLab returned an invalid response",
            GitLabOrgClientErrorCode::ResourceExhausted => {
                "the GitLab Org request or response exceeds its approved limit"
            }
            GitLabOrgClientErrorCode::UnknownOutcome => {
                "the GitLab effect outcome is unknown and must be reconciled"
            }
        })
    }
}

impl std::error::Error for GitLabOrgClientError {}

pub(in crate::toolkits) enum GitLabOrgOperation<'a> {
    CreateBranch {
        branch_name: &'a str,
        repository: Option<&'a str>,
    },
    SetActiveBranch {
        branch: &'a str,
    },
    ListBranches {
        repository: Option<&'a str>,
        limit: usize,
        branch_wildcard: Option<&'a str>,
    },
    GetIssues {
        repository: Option<&'a str>,
    },
    GetIssue {
        issue_number: u64,
        repository: Option<&'a str>,
    },
    CreatePullRequest {
        title: &'a str,
        body: &'a str,
        branch: &'a str,
        repository: Option<&'a str>,
    },
    CommentOnIssue {
        issue_number: u64,
        comment: &'a str,
        repository: Option<&'a str>,
    },
    CreateFile {
        file_path: &'a str,
        contents: &'a str,
        branch: &'a str,
        repository: Option<&'a str>,
    },
    ReadFile {
        file_path: &'a str,
        branch: &'a str,
        repository: Option<&'a str>,
        start_line: Option<usize>,
        end_line: Option<usize>,
    },
    UpdateFile {
        file_path: &'a str,
        update_query: &'a str,
        branch: &'a str,
        repository: Option<&'a str>,
    },
    DeleteFile {
        file_path: &'a str,
        branch: &'a str,
        repository: Option<&'a str>,
    },
    GetPrChanges {
        pr_number: u64,
        repository: Option<&'a str>,
    },
    CreatePrChangeComment {
        pr_number: u64,
        file_path: &'a str,
        line_number: usize,
        comment: &'a str,
        repository: Option<&'a str>,
    },
    ListFiles {
        path: &'a str,
        recursive: bool,
        branch: Option<&'a str>,
        repository: Option<&'a str>,
    },
    ListFolders {
        path: &'a str,
        recursive: bool,
        branch: Option<&'a str>,
        repository: Option<&'a str>,
    },
    AppendFile {
        file_path: &'a str,
        content: &'a str,
        branch: &'a str,
        repository: Option<&'a str>,
    },
    GetCommits {
        sha: Option<&'a str>,
        path: Option<&'a str>,
        since: Option<&'a str>,
        until: Option<&'a str>,
        author: Option<&'a str>,
        repository: Option<&'a str>,
    },
}

#[async_trait]
pub(in crate::toolkits) trait GitLabOrgApi: Send + Sync {
    async fn execute(
        &self,
        operation: GitLabOrgOperation<'_>,
    ) -> Result<Value, GitLabOrgClientError>;
}

pub(in crate::toolkits) struct GitLabOrgHttpResponse {
    pub(in crate::toolkits) status: StatusCode,
    pub(in crate::toolkits) body: Option<Value>,
    pub(in crate::toolkits) json_content_type: bool,
    pub(in crate::toolkits) next_page: Option<Box<str>>,
}

impl GitLabOrgHttpResponse {
    #[cfg(test)]
    pub(in crate::toolkits) fn fixture(
        status: StatusCode,
        body: Option<Value>,
        next_page: Option<&str>,
    ) -> Self {
        Self {
            status,
            body,
            json_content_type: true,
            next_page: next_page.map(Into::into),
        }
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn non_json_fixture(status: StatusCode) -> Self {
        Self {
            status,
            body: None,
            json_content_type: false,
            next_page: None,
        }
    }
}

#[async_trait]
pub(in crate::toolkits) trait GitLabOrgTransport: Send + Sync {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<GitLabOrgHttpResponse, GitLabOrgClientError>;
}

struct ReqwestGitLabOrgTransport {
    http: reqwest::Client,
}

#[async_trait]
impl GitLabOrgTransport for ReqwestGitLabOrgTransport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<GitLabOrgHttpResponse, GitLabOrgClientError> {
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
        let next_page = parse_next_page(response.headers().get("x-next-page"), effect)?;
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
        let body = if bytes.is_empty() {
            None
        } else if json_content_type {
            Some(serde_json::from_slice(&bytes).map_err(|_| response_shape_failure(effect))?)
        } else {
            None
        };
        Ok(GitLabOrgHttpResponse {
            status: response.status(),
            body,
            json_content_type,
            next_page,
        })
    }
}

/// One claim-scoped GitLab client with invocation-local active-branch state.
pub(crate) struct GitLabOrgClient {
    config: GitLabOrgToolkitConfig,
    transport: Arc<dyn GitLabOrgTransport>,
    operation_gate: Mutex<()>,
    active_branch: Mutex<Box<str>>,
}

/// The production GitLab transport, shared with the single-project `gitlab`
/// family so both families keep one HTTPS, no-redirect, bounded wire policy.
pub(in crate::toolkits) fn reqwest_transport()
-> Result<Arc<dyn GitLabOrgTransport>, GitLabOrgClientError> {
    let http = reqwest::Client::builder()
        .https_only(true)
        .redirect(reqwest::redirect::Policy::none())
        .retry(reqwest::retry::never())
        .connect_timeout(CONNECT_TIMEOUT)
        .timeout(REQUEST_TIMEOUT)
        .pool_idle_timeout(POOL_IDLE_TIMEOUT)
        .pool_max_idle_per_host(MAX_IDLE_PER_HOST)
        .user_agent(USER_AGENT)
        .build()
        .map_err(|_| invalid_configuration())?;
    Ok(Arc::new(ReqwestGitLabOrgTransport { http }))
}

impl GitLabOrgClient {
    pub(crate) fn new(config: GitLabOrgToolkitConfig) -> Result<Self, GitLabOrgClientError> {
        let transport = reqwest_transport()?;
        let active_branch = Mutex::new(config.branch().into());
        Ok(Self {
            config,
            transport,
            operation_gate: Mutex::new(()),
            active_branch,
        })
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn fixture(
        config: GitLabOrgToolkitConfig,
        transport: Arc<dyn GitLabOrgTransport>,
    ) -> Self {
        let active_branch = Mutex::new(config.branch().into());
        Self {
            config,
            transport,
            operation_gate: Mutex::new(()),
            active_branch,
        }
    }

    fn resolve_repository<'a>(
        &'a self,
        supplied: Option<&'a str>,
    ) -> Result<&'a str, GitLabOrgClientError> {
        let repositories = self.config.repositories();
        let supplied = supplied.map(str::trim);
        if repositories.is_empty() {
            // This source-compatible organization-wide path remains
            // capability-disabled until the frozen empty-repository setting is
            // admitted by an explicit authority-bearing invocation claim.
            return supplied
                .filter(|value| valid_repository_path(value))
                .ok_or_else(invalid_configuration);
        }
        match supplied {
            None => Ok(&repositories[0]),
            Some(value)
                if valid_text(value, MAX_IDENTIFIER_BYTES)
                    && repositories.iter().any(|repo| repo.as_ref() == value) =>
            {
                Ok(value)
            }
            Some(_) => Err(error(GitLabOrgClientErrorCode::Authorization, false)),
        }
    }

    fn url(
        &self,
        segments: &[&str],
        query: &[(&str, String)],
    ) -> Result<Url, GitLabOrgClientError> {
        let mut url = self.config.base_url().clone();
        {
            let mut path = url
                .path_segments_mut()
                .map_err(|()| invalid_configuration())?;
            path.extend(["api", "v4"]);
            path.extend(segments.iter().copied());
        }
        if !query.is_empty() {
            let mut pairs = url.query_pairs_mut();
            for (name, value) in query {
                pairs.append_pair(name, value);
            }
        }
        Ok(url)
    }

    fn request(
        &self,
        method: Method,
        segments: &[&str],
        query: &[(&str, String)],
        body: Option<&Value>,
    ) -> Result<Request, GitLabOrgClientError> {
        let mut request = Request::new(method, self.url(segments, query)?);
        request
            .headers_mut()
            .insert(ACCEPT, HeaderValue::from_static("application/json"));
        let mut token =
            HeaderValue::from_str(&Zeroizing::new(self.config.private_token().to_owned()))
                .map_err(|_| invalid_configuration())?;
        token.set_sensitive(true);
        request.headers_mut().insert(PRIVATE_TOKEN, token);
        if let Some(body) = body {
            let encoded = serde_json::to_vec(body).map_err(|_| invalid_input())?;
            if encoded.len() > MAX_REQUEST_BYTES {
                return Err(resource_exhausted());
            }
            request
                .headers_mut()
                .insert(CONTENT_TYPE, HeaderValue::from_static("application/json"));
            *request.body_mut() = Some(encoded.into());
        }
        Ok(request)
    }

    async fn call(
        &self,
        method: Method,
        segments: &[&str],
        query: &[(&str, String)],
        body: Option<&Value>,
        effect: bool,
        allow_empty: bool,
    ) -> Result<GitLabOrgHttpResponse, GitLabOrgClientError> {
        let effect_method = method.clone();
        let request = self.request(method, segments, query, body)?;
        let response = self.transport.execute(request, effect).await?;
        map_http_status(response.status, effect)?;
        if effect {
            validate_effect_status(&effect_method, response.status)?;
        }
        if !allow_empty && (!response.json_content_type || response.body.is_none()) {
            return Err(response_shape_failure(effect));
        }
        Ok(response)
    }

    #[allow(clippy::too_many_arguments)] // Mirrors one auditable GitLab wire contract.
    async fn project_call(
        &self,
        method: Method,
        repository: &str,
        suffix: &[&str],
        query: &[(&str, String)],
        body: Option<&Value>,
        effect: bool,
        allow_empty: bool,
    ) -> Result<GitLabOrgHttpResponse, GitLabOrgClientError> {
        let mut segments = Vec::with_capacity(suffix.len() + 2);
        segments.extend(["projects", repository]);
        segments.extend_from_slice(suffix);
        self.call(method, &segments, query, body, effect, allow_empty)
            .await
    }
}

fn validate_branch(value: &str) -> Result<(), GitLabOrgClientError> {
    validate_text(value, MAX_BRANCH_BYTES)
}

fn validate_file_path(value: &str) -> Result<(), GitLabOrgClientError> {
    if !valid_path(value, false) {
        return Err(invalid_input());
    }
    Ok(())
}

fn validate_tree_path(value: &str) -> Result<(), GitLabOrgClientError> {
    if !valid_path(value, true) {
        return Err(invalid_input());
    }
    Ok(())
}

fn valid_path(value: &str, allow_empty: bool) -> bool {
    if value.is_empty() {
        return allow_empty;
    }
    value.len() <= MAX_PATH_BYTES
        && !value.starts_with('/')
        && !value.ends_with('/')
        && !value.chars().any(char::is_control)
        && !value
            .split('/')
            .any(|segment| matches!(segment, "." | "..") || segment.is_empty())
}

fn valid_repository_path(value: &str) -> bool {
    valid_text(value, MAX_IDENTIFIER_BYTES)
        && !value.starts_with('/')
        && !value.ends_with('/')
        && value.split('/').count() >= 2
        && !value
            .split('/')
            .any(|part| part.is_empty() || matches!(part, "." | ".."))
}

fn validate_text(value: &str, limit: usize) -> Result<(), GitLabOrgClientError> {
    if value.len() > limit {
        return Err(resource_exhausted());
    }
    if !valid_text(value, limit) {
        return Err(invalid_input());
    }
    Ok(())
}

fn validate_multiline_text(value: &str, limit: usize) -> Result<(), GitLabOrgClientError> {
    if value.len() > limit {
        return Err(resource_exhausted());
    }
    if value.trim().is_empty()
        || value
            .chars()
            .any(|character| character.is_control() && !matches!(character, '\n' | '\r' | '\t'))
    {
        return Err(invalid_input());
    }
    Ok(())
}

fn valid_text(value: &str, limit: usize) -> bool {
    !value.trim().is_empty() && value.len() <= limit && !value.chars().any(char::is_control)
}

fn validate_line_range(
    start: Option<usize>,
    end: Option<usize>,
) -> Result<(), GitLabOrgClientError> {
    if start == Some(0) || end == Some(0) || start.zip(end).is_some_and(|(start, end)| start > end)
    {
        return Err(invalid_input());
    }
    Ok(())
}

fn validate_rfc3339(value: &str) -> Result<(), GitLabOrgClientError> {
    validate_text(value, 128)?;
    DateTime::parse_from_rfc3339(value).map_err(|_| invalid_input())?;
    Ok(())
}

fn required_string<'a>(value: &'a Value, name: &str) -> Result<&'a str, GitLabOrgClientError> {
    value
        .get(name)
        .and_then(Value::as_str)
        .filter(|value| value.len() <= MAX_RESPONSE_BYTES)
        .ok_or_else(invalid_response)
}

fn required_string_object<'a>(
    value: &'a Map<String, Value>,
    name: &str,
) -> Result<&'a str, GitLabOrgClientError> {
    value
        .get(name)
        .and_then(Value::as_str)
        .filter(|value| value.len() <= MAX_RESPONSE_BYTES)
        .ok_or_else(invalid_response)
}

fn nullable_string(value: &Value, name: &str) -> Result<Value, GitLabOrgClientError> {
    match value.get(name) {
        None | Some(Value::Null) => Ok(Value::Null),
        Some(Value::String(value)) if value.len() <= MAX_RESPONSE_BYTES => {
            Ok(Value::String(value.clone()))
        }
        Some(_) => Err(invalid_response()),
    }
}

fn required_u64(value: &Value, name: &str) -> Result<u64, GitLabOrgClientError> {
    value
        .get(name)
        .and_then(Value::as_u64)
        .filter(|value| *value > 0)
        .ok_or_else(invalid_response)
}

fn branch_already_exists(value: &Value) -> bool {
    match value {
        Value::String(value) => value.contains("Branch already exists"),
        Value::Array(values) => values.iter().any(branch_already_exists),
        Value::Object(values) => values.values().any(branch_already_exists),
        _ => false,
    }
}

enum WildcardToken {
    Literal(char),
    Any,
    Star,
    Class {
        negated: bool,
        members: Vec<(char, char)>,
    },
}

pub(in crate::toolkits) fn wildcard_matches(pattern: &str, candidate: &str) -> bool {
    let pattern = wildcard_tokens(pattern);
    let candidate = candidate.chars().collect::<Vec<_>>();
    let mut previous = vec![false; candidate.len() + 1];
    previous[0] = true;
    for token in pattern {
        let mut current = vec![false; candidate.len() + 1];
        if matches!(token, WildcardToken::Star) {
            current[0] = previous[0];
        }
        for index in 1..=candidate.len() {
            current[index] = match &token {
                WildcardToken::Star => previous[index] || current[index - 1],
                WildcardToken::Any => previous[index - 1],
                WildcardToken::Literal(expected) => {
                    previous[index - 1] && *expected == candidate[index - 1]
                }
                WildcardToken::Class { negated, members } => {
                    let included = members
                        .iter()
                        .any(|(start, end)| (*start..=*end).contains(&candidate[index - 1]));
                    previous[index - 1] && if *negated { !included } else { included }
                }
            };
        }
        previous = current;
    }
    previous[candidate.len()]
}

fn wildcard_tokens(pattern: &str) -> Vec<WildcardToken> {
    let characters = pattern.chars().collect::<Vec<_>>();
    let mut tokens = Vec::with_capacity(characters.len());
    let mut index = 0usize;
    while index < characters.len() {
        match characters[index] {
            '*' => tokens.push(WildcardToken::Star),
            '?' => tokens.push(WildcardToken::Any),
            '[' => {
                let mut class_start = index + 1;
                let negated = characters.get(class_start) == Some(&'!');
                if negated {
                    class_start += 1;
                }
                let search_start =
                    class_start + usize::from(characters.get(class_start) == Some(&']'));
                let Some(relative_end) = characters[search_start..]
                    .iter()
                    .position(|value| *value == ']')
                else {
                    tokens.push(WildcardToken::Literal('['));
                    index += 1;
                    continue;
                };
                let end = search_start + relative_end;
                let mut member_index = class_start;
                let mut members = Vec::new();
                while member_index < end {
                    let start = characters[member_index];
                    if member_index + 2 < end && characters[member_index + 1] == '-' {
                        members.push((start, characters[member_index + 2]));
                        member_index += 3;
                    } else {
                        members.push((start, start));
                        member_index += 1;
                    }
                }
                if members.is_empty() {
                    tokens.push(WildcardToken::Literal('['));
                } else {
                    tokens.push(WildcardToken::Class { negated, members });
                    index = end;
                }
            }
            value => tokens.push(WildcardToken::Literal(value)),
        }
        index += 1;
    }
    tokens
}

pub(in crate::toolkits) fn python_issue_list(issues: &[(&str, u64)]) -> String {
    let mut output = String::from("[");
    for (index, (title, iid)) in issues.iter().enumerate() {
        if index > 0 {
            output.push_str(", ");
        }
        output.push_str("{'title': ");
        output.push_str(&repr_str(title));
        output.push_str(", 'number': ");
        output.push_str(&iid.to_string());
        output.push('}');
    }
    output.push(']');
    output
}

fn python_string_list(values: &[String]) -> String {
    let mut output = String::from("[");
    for (index, value) in values.iter().enumerate() {
        if index > 0 {
            output.push_str(", ");
        }
        output.push_str(&repr_str(value));
    }
    output.push(']');
    output
}

fn commit_payload(
    action: &str,
    file_path: &str,
    branch: &str,
    message: &str,
    content: &str,
    last_commit_id: &str,
) -> Value {
    json!({
        "branch":branch,
        "commit_message":message,
        "actions":[{
            "action":action,
            "file_path":file_path,
            "content":content,
            "last_commit_id":last_commit_id
        }]
    })
}

fn map_edit_error(error: EditErrorCode) -> GitLabOrgClientError {
    match error {
        EditErrorCode::ResourceExhausted => resource_exhausted(),
        EditErrorCode::InvalidMarkers
        | EditErrorCode::UnsupportedFile
        | EditErrorCode::Ambiguous
        | EditErrorCode::NotFound
        | EditErrorCode::NoChange => invalid_input(),
    }
}

fn map_diff_error(error: DiffErrorCode) -> GitLabOrgClientError {
    match error {
        DiffErrorCode::ResourceExhausted => resource_exhausted(),
        DiffErrorCode::InvalidIndex => invalid_input(),
        DiffErrorCode::InvalidShape => invalid_response(),
    }
}

fn slice_requested_lines(
    content: &str,
    start: Option<usize>,
    end: Option<usize>,
) -> Result<&str, GitLabOrgClientError> {
    if start.is_none() && end.is_none() {
        return Ok(content);
    }
    let ranges = python_line_ranges(content);
    if ranges.is_empty() {
        return Err(invalid_input());
    }
    let first = start.unwrap_or(1);
    let last = end.unwrap_or(ranges.len());
    if first == 0 || last == 0 || first > last || last > ranges.len() {
        return Err(invalid_input());
    }
    Ok(&content[ranges[first - 1].0..ranges[last - 1].1])
}

fn requested_range_label(start: Option<usize>, end: Option<usize>) -> String {
    match (start, end) {
        (None, None) => "full file".to_owned(),
        (start, end) => format!(
            "lines {}..{}",
            start.map_or_else(|| "1".to_owned(), |value| value.to_string()),
            end.map_or_else(|| "end".to_owned(), |value| value.to_string())
        ),
    }
}

fn guard_text_read(content: &str, file_path: &str, requested: &str, full_content: &str) -> Value {
    let actual_chars = content.chars().count();
    let serialized_bytes = serde_json::to_vec(&Value::String(content.to_owned()))
        .map_or(usize::MAX, |encoded| encoded.len());
    if actual_chars <= MAX_OUTPUT_CHARS && serialized_bytes <= MAX_OUTPUT_BYTES {
        return Value::String(content.to_owned());
    }
    let total_lines = python_line_ranges(full_content).len();
    let extension = file_path
        .rsplit('/')
        .next()
        .unwrap_or(file_path)
        .rfind('.')
        .filter(|index| *index > 0)
        .map_or("", |index| {
            &file_path[file_path.len() - file_path.rsplit('/').next().unwrap_or(file_path).len()
                + index..]
        });
    let exceeded = match (
        actual_chars > MAX_OUTPUT_CHARS,
        serialized_bytes > MAX_OUTPUT_BYTES,
    ) {
        (true, true) => format!(
            "exceeds both the {MAX_OUTPUT_CHARS}-character and {MAX_OUTPUT_BYTES}-byte serialized result limits"
        ),
        (true, false) => format!("exceeds the {MAX_OUTPUT_CHARS}-character result limit"),
        (false, true) => format!("exceeds the {MAX_OUTPUT_BYTES}-byte serialized result limit"),
        (false, false) => "exceeds the approved read limit".to_owned(),
    };
    let (first_class_params, notes) = if total_lines <= 1 {
        (
            Map::new(),
            format!(
                "This file has no usable line breaks and {exceeded}. Line slicing would return the whole file, so the full read is refused."
            ),
        )
    } else {
        (
            Map::from_iter([
                (
                    "start_line".to_owned(),
                    Value::String(format!(
                        "integer (1-indexed, inclusive); valid range 1..{total_lines}"
                    )),
                ),
                (
                    "end_line".to_owned(),
                    Value::String(format!(
                        "integer (1-indexed, inclusive); valid range 1..{total_lines}"
                    )),
                ),
            ]),
            format!(
                "The requested content {exceeded}. Use start_line/end_line to request a smaller bounded range."
            ),
        )
    };
    json!({
        "__result_status__":"content_too_large",
        "schema_version":"1.0",
        "filename":file_path,
        "type":mime_type(extension),
        "extension":extension,
        "unit":"lines",
        "total_lines":total_lines,
        "read_limits":{"max_output_chars":MAX_OUTPUT_CHARS,"max_serialized_bytes":MAX_OUTPUT_BYTES,"full_read_allowed":false},
        "instruction_for_readFile":{
            "extra_params":{},
            "first_class_params":first_class_params,
            "notes":notes
        },
        "context":{"limit_chars":MAX_OUTPUT_CHARS,"actual_chars":actual_chars,"limit_serialized_bytes":MAX_OUTPUT_BYTES,"actual_serialized_bytes":serialized_bytes,"requested":requested}
    })
}

fn mime_type(extension: &str) -> &'static str {
    match extension.to_ascii_lowercase().as_str() {
        ".py" => "text/x-python",
        ".rs" => "text/x-rust",
        ".js" => "text/javascript",
        ".ts" => "text/typescript",
        ".json" => "application/json",
        ".md" => "text/markdown",
        ".yaml" | ".yml" => "application/yaml",
        ".html" => "text/html",
        ".csv" => "text/csv",
        ".txt" | ".log" => "text/plain",
        _ => "application/octet-stream",
    }
}

fn bounded_output(value: Value) -> Result<Value, GitLabOrgClientError> {
    if serde_json::to_vec(&value)
        .map_err(|_| invalid_response())?
        .len()
        > MAX_OUTPUT_BYTES
    {
        return Err(resource_exhausted());
    }
    Ok(value)
}

fn parse_next_page(
    value: Option<&HeaderValue>,
    effect: bool,
) -> Result<Option<Box<str>>, GitLabOrgClientError> {
    let Some(value) = value else {
        return Ok(None);
    };
    let value = value.to_str().map_err(|_| response_shape_failure(effect))?;
    if value.is_empty() {
        return Ok(None);
    }
    if value.len() > 20
        || !value.bytes().all(|byte| byte.is_ascii_digit())
        || !value.parse::<u64>().is_ok_and(|page| page > 0)
    {
        return Err(response_shape_failure(effect));
    }
    Ok(Some(value.into()))
}

pub(in crate::toolkits) fn validate_effect_status(
    method: &Method,
    status: StatusCode,
) -> Result<(), GitLabOrgClientError> {
    let expected = match *method {
        Method::POST => StatusCode::CREATED,
        Method::DELETE => StatusCode::NO_CONTENT,
        _ => return Err(invalid_configuration()),
    };
    if status != expected {
        return Err(unknown_outcome());
    }
    Ok(())
}

pub(in crate::toolkits) fn map_http_status(
    status: StatusCode,
    effect: bool,
) -> Result<(), GitLabOrgClientError> {
    if status.is_success() {
        return Ok(());
    }
    let code = match status {
        StatusCode::REQUEST_TIMEOUT | StatusCode::TOO_MANY_REQUESTS if effect => {
            GitLabOrgClientErrorCode::UnknownOutcome
        }
        status if status.is_server_error() && effect => GitLabOrgClientErrorCode::UnknownOutcome,
        StatusCode::BAD_REQUEST | StatusCode::UNPROCESSABLE_ENTITY => {
            GitLabOrgClientErrorCode::InvalidInput
        }
        StatusCode::UNAUTHORIZED => GitLabOrgClientErrorCode::Authentication,
        StatusCode::FORBIDDEN => GitLabOrgClientErrorCode::Authorization,
        StatusCode::NOT_FOUND => GitLabOrgClientErrorCode::NotFound,
        StatusCode::CONFLICT => GitLabOrgClientErrorCode::Conflict,
        StatusCode::REQUEST_TIMEOUT => GitLabOrgClientErrorCode::Timeout,
        StatusCode::TOO_MANY_REQUESTS => GitLabOrgClientErrorCode::RateLimited,
        status if status.is_server_error() => GitLabOrgClientErrorCode::DependencyUnavailable,
        _ if effect => GitLabOrgClientErrorCode::UnknownOutcome,
        _ => GitLabOrgClientErrorCode::InvalidResponse,
    };
    Err(error(
        code,
        !effect
            && matches!(
                code,
                GitLabOrgClientErrorCode::Timeout
                    | GitLabOrgClientErrorCode::RateLimited
                    | GitLabOrgClientErrorCode::DependencyUnavailable
            ),
    ))
}

fn map_reqwest_error(source: &reqwest::Error, effect: bool) -> GitLabOrgClientError {
    if effect {
        return unknown_outcome();
    }
    if source.is_timeout() {
        return error(GitLabOrgClientErrorCode::Timeout, true);
    }
    if source.is_connect() || source.is_request() || source.is_body() || source.is_decode() {
        return error(GitLabOrgClientErrorCode::DependencyUnavailable, true);
    }
    invalid_response()
}

fn response_bound_failure(effect: bool) -> GitLabOrgClientError {
    if effect {
        unknown_outcome()
    } else {
        resource_exhausted()
    }
}

fn response_shape_failure(effect: bool) -> GitLabOrgClientError {
    if effect {
        unknown_outcome()
    } else {
        invalid_response()
    }
}

const fn error(code: GitLabOrgClientErrorCode, retryable: bool) -> GitLabOrgClientError {
    GitLabOrgClientError { code, retryable }
}

const fn invalid_configuration() -> GitLabOrgClientError {
    error(GitLabOrgClientErrorCode::InvalidConfiguration, false)
}

const fn invalid_input() -> GitLabOrgClientError {
    error(GitLabOrgClientErrorCode::InvalidInput, false)
}

const fn invalid_response() -> GitLabOrgClientError {
    error(GitLabOrgClientErrorCode::InvalidResponse, false)
}

const fn resource_exhausted() -> GitLabOrgClientError {
    error(GitLabOrgClientErrorCode::ResourceExhausted, false)
}

const fn unknown_outcome() -> GitLabOrgClientError {
    error(GitLabOrgClientErrorCode::UnknownOutcome, false)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_map_http_status(
    status: StatusCode,
    effect: bool,
) -> GitLabOrgClientError {
    map_http_status(status, effect).expect_err("non-success status must fail")
}

#[cfg(test)]
pub(in crate::toolkits) fn test_parse_next_page(
    value: Option<&HeaderValue>,
) -> Result<Option<Box<str>>, GitLabOrgClientError> {
    parse_next_page(value, false)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_validate_effect_status(
    method: &Method,
    status: StatusCode,
) -> Result<(), GitLabOrgClientError> {
    validate_effect_status(method, status)
}

#[async_trait]
impl GitLabOrgApi for GitLabOrgClient {
    #[allow(clippy::too_many_lines)] // One source-ordered provider ledger is easier to audit.
    async fn execute(
        &self,
        operation: GitLabOrgOperation<'_>,
    ) -> Result<Value, GitLabOrgClientError> {
        let _operation_guard = self.operation_gate.lock().await;
        match operation {
            GitLabOrgOperation::CreateBranch {
                branch_name,
                repository,
            } => {
                validate_branch(branch_name)?;
                let repository = self.resolve_repository(repository)?;
                let source = self.active_branch.lock().await.clone();
                let body = json!({"branch":branch_name,"ref":source.as_ref()});
                let request = self.request(
                    Method::POST,
                    &["projects", repository, "repository", "branches"],
                    &[],
                    Some(&body),
                )?;
                let response = self.transport.execute(request, true).await?;
                let already_exists = response.status == StatusCode::BAD_REQUEST
                    && response.body.as_ref().is_some_and(branch_already_exists);
                if !already_exists {
                    map_http_status(response.status, true)?;
                    validate_effect_status(&Method::POST, response.status)?;
                    if !response.json_content_type || response.body.is_none() {
                        return Err(unknown_outcome());
                    }
                }
                *self.active_branch.lock().await = branch_name.into();
                bounded_output(Value::String(if already_exists {
                    format!("Branch {branch_name} already exists and was set as active")
                } else {
                    format!("Branch {branch_name} created successfully and set as active")
                }))
                .map_err(|_| unknown_outcome())
            }
            GitLabOrgOperation::SetActiveBranch { branch } => {
                validate_branch(branch)?;
                *self.active_branch.lock().await = branch.into();
                bounded_output(Value::String(format!("Active branch set to {branch}")))
            }
            GitLabOrgOperation::ListBranches {
                repository,
                limit,
                branch_wildcard,
            } => {
                if !(1..=100).contains(&limit) {
                    return Err(invalid_input());
                }
                if let Some(wildcard) = branch_wildcard {
                    validate_text(wildcard, MAX_BRANCH_BYTES)?;
                }
                let repository = self.resolve_repository(repository)?;
                let branches = self
                    .paged_project_array(repository, &["repository", "branches"], &[], MAX_ITEMS)
                    .await?;
                let mut names = Vec::new();
                for branch in branches {
                    let name = required_string(&branch, "name")?;
                    if name.len() > MAX_BRANCH_BYTES {
                        return Err(resource_exhausted());
                    }
                    if branch_wildcard.is_none_or(|pattern| wildcard_matches(pattern, name)) {
                        names.push(Value::String(name.to_owned()));
                        if names.len() == limit {
                            break;
                        }
                    }
                }
                bounded_output(Value::Array(names))
            }
            GitLabOrgOperation::GetIssues { repository } => {
                let repository = self.resolve_repository(repository)?;
                let response = self
                    .project_call(
                        Method::GET,
                        repository,
                        &["issues"],
                        &[
                            ("state", "opened".to_owned()),
                            ("per_page", "20".to_owned()),
                            ("page", "1".to_owned()),
                        ],
                        None,
                        false,
                        false,
                    )
                    .await?;
                let issues = response
                    .body
                    .as_ref()
                    .and_then(Value::as_array)
                    .ok_or_else(invalid_response)?;
                if issues.len() > 20 {
                    return Err(resource_exhausted());
                }
                if issues.is_empty() {
                    return Ok(Value::String("No open issues available".to_owned()));
                }
                let mut projected = Vec::with_capacity(issues.len());
                for issue in issues {
                    projected.push((
                        required_string(issue, "title")?,
                        required_u64(issue, "iid")?,
                    ));
                }
                bounded_output(Value::String(format!(
                    "Found {} issues:\n{}",
                    projected.len(),
                    python_issue_list(&projected)
                )))
            }
            GitLabOrgOperation::GetIssue {
                issue_number,
                repository,
            } => {
                if issue_number == 0 {
                    return Err(invalid_input());
                }
                let repository = self.resolve_repository(repository)?;
                let issue = self
                    .project_json(repository, &["issues", &issue_number.to_string()], &[])
                    .await?;
                let notes_response = self
                    .project_call(
                        Method::GET,
                        repository,
                        &["issues", &issue_number.to_string(), "notes"],
                        &[("per_page", "10".to_owned()), ("page", "1".to_owned())],
                        None,
                        false,
                        false,
                    )
                    .await?;
                let notes = notes_response
                    .body
                    .as_ref()
                    .and_then(Value::as_array)
                    .ok_or_else(invalid_response)?;
                if notes.len() > 10 {
                    return Err(resource_exhausted());
                }
                let mut comments = Vec::with_capacity(notes.len());
                for note in notes {
                    let author = note
                        .get("author")
                        .and_then(Value::as_object)
                        .ok_or_else(invalid_response)?;
                    comments.push(json!({
                        "body":required_string(note,"body")?,
                        "user":required_string_object(author,"username")?
                    }));
                }
                bounded_output(json!({
                    "title":required_string(&issue,"title")?,
                    "body":nullable_string(&issue,"description")?,
                    "comments":comments
                }))
            }
            GitLabOrgOperation::CreatePullRequest {
                title,
                body,
                branch,
                repository,
            } => {
                validate_text(title, 8 * 1_024)?;
                validate_multiline_text(body, MAX_TEXT_BYTES)?;
                validate_branch(branch)?;
                let repository = self.resolve_repository(repository)?;
                let payload = json!({
                    "source_branch":branch,
                    "target_branch":self.config.branch(),
                    "title":title,
                    "description":body,
                    "labels":["created-by-agent"]
                });
                let response = self
                    .project_call(
                        Method::POST,
                        repository,
                        &["merge_requests"],
                        &[],
                        Some(&payload),
                        true,
                        false,
                    )
                    .await?;
                let iid = response
                    .body
                    .as_ref()
                    .ok_or_else(unknown_outcome)
                    .and_then(|value| required_u64(value, "iid").map_err(|_| unknown_outcome()))?;
                bounded_output(Value::String(format!(
                    "Successfully created PR number {iid}"
                )))
                .map_err(|_| unknown_outcome())
            }
            GitLabOrgOperation::CommentOnIssue {
                issue_number,
                comment,
                repository,
            } => {
                if issue_number == 0 {
                    return Err(invalid_input());
                }
                validate_multiline_text(comment, MAX_TEXT_BYTES)?;
                let repository = self.resolve_repository(repository)?;
                self.project_json(repository, &["issues", &issue_number.to_string()], &[])
                    .await?;
                let payload = json!({"body":comment});
                self.project_call(
                    Method::POST,
                    repository,
                    &["issues", &issue_number.to_string(), "notes"],
                    &[],
                    Some(&payload),
                    true,
                    false,
                )
                .await?;
                bounded_output(Value::String(format!(
                    "Comment added to issue {issue_number}"
                )))
                .map_err(|_| unknown_outcome())
            }
            GitLabOrgOperation::CreateFile {
                file_path,
                contents,
                branch,
                repository,
            } => {
                validate_file_path(file_path)?;
                validate_branch(branch)?;
                if contents.len() > MAX_WRITABLE_FILE_BYTES {
                    return Err(resource_exhausted());
                }
                let repository = self.resolve_repository(repository)?;
                let query = [("ref", branch.to_owned())];
                match self
                    .project_call(
                        Method::GET,
                        repository,
                        &["repository", "files", file_path],
                        &query,
                        None,
                        false,
                        false,
                    )
                    .await
                {
                    Ok(_) => {
                        return bounded_output(Value::String(format!(
                            "File {file_path} already exists"
                        )));
                    }
                    Err(error) if error.code() == GitLabOrgClientErrorCode::NotFound => {}
                    Err(error) => return Err(error),
                }
                let payload = json!({"branch":branch,"commit_message":format!("Create {file_path}"),"content":contents});
                self.project_call(
                    Method::POST,
                    repository,
                    &["repository", "files", file_path],
                    &[],
                    Some(&payload),
                    true,
                    false,
                )
                .await?;
                bounded_output(Value::String(format!("Created file {file_path}")))
                    .map_err(|_| unknown_outcome())
            }
            GitLabOrgOperation::ReadFile {
                file_path,
                branch,
                repository,
                start_line,
                end_line,
            } => {
                validate_file_path(file_path)?;
                validate_branch(branch)?;
                validate_line_range(start_line, end_line)?;
                let repository = self.resolve_repository(repository)?;
                let file = self
                    .read_provider_file(repository, file_path, branch)
                    .await?;
                let slice = slice_requested_lines(&file.content, start_line, end_line)?;
                let requested = requested_range_label(start_line, end_line);
                bounded_output(guard_text_read(slice, file_path, &requested, &file.content))
            }
            GitLabOrgOperation::UpdateFile {
                file_path,
                update_query,
                branch,
                repository,
            } => {
                validate_file_path(file_path)?;
                validate_branch(branch)?;
                let repository = self.resolve_repository(repository)?;
                let file = self
                    .read_provider_file(repository, file_path, branch)
                    .await?;
                let updated = match apply_update(file_path, &file.content, update_query) {
                    Ok(updated) => updated,
                    Err(EditErrorCode::Ambiguous) => {
                        return bounded_output(Value::String(
                            "Update not applied because the OLD block matched more than once"
                                .to_owned(),
                        ));
                    }
                    Err(EditErrorCode::NotFound) => {
                        return bounded_output(Value::String(
                            "Update not applied because the OLD block was not found".to_owned(),
                        ));
                    }
                    Err(EditErrorCode::NoChange) => {
                        return bounded_output(Value::String(
                            "Update not applied because it would not change the file".to_owned(),
                        ));
                    }
                    Err(error) => return Err(map_edit_error(error)),
                };
                if updated.len() > MAX_WRITABLE_FILE_BYTES {
                    return Err(resource_exhausted());
                }
                let payload = commit_payload(
                    "update",
                    file_path,
                    branch,
                    &format!("Update {file_path}"),
                    &updated,
                    &file.last_commit_id,
                );
                self.project_call(
                    Method::POST,
                    repository,
                    &["repository", "commits"],
                    &[],
                    Some(&payload),
                    true,
                    false,
                )
                .await?;
                bounded_output(Value::String(format!("Updated file {file_path}")))
                    .map_err(|_| unknown_outcome())
            }
            GitLabOrgOperation::DeleteFile {
                file_path,
                branch,
                repository,
            } => {
                validate_file_path(file_path)?;
                validate_branch(branch)?;
                let repository = self.resolve_repository(repository)?;
                let file = self
                    .read_provider_file(repository, file_path, branch)
                    .await?;
                let query = [
                    ("branch", branch.to_owned()),
                    ("commit_message", format!("Delete {file_path}")),
                    ("last_commit_id", file.last_commit_id),
                ];
                self.project_call(
                    Method::DELETE,
                    repository,
                    &["repository", "files", file_path],
                    &query,
                    None,
                    true,
                    true,
                )
                .await?;
                bounded_output(Value::String(format!("Deleted file {file_path}")))
                    .map_err(|_| unknown_outcome())
            }
            GitLabOrgOperation::GetPrChanges {
                pr_number,
                repository,
            } => {
                if pr_number == 0 {
                    return Err(invalid_input());
                }
                let repository = self.resolve_repository(repository)?;
                let mr = self
                    .project_json(repository, &["merge_requests", &pr_number.to_string()], &[])
                    .await?;
                let changes = self
                    .project_json(
                        repository,
                        &["merge_requests", &pr_number.to_string(), "changes"],
                        &[],
                    )
                    .await?;
                let formatted = format_changes(&mr, &changes).map_err(map_diff_error)?;
                bounded_output(Value::String(formatted))
            }
            GitLabOrgOperation::CreatePrChangeComment {
                pr_number,
                file_path,
                line_number,
                comment,
                repository,
            } => {
                if pr_number == 0 {
                    return Err(invalid_input());
                }
                validate_file_path(file_path)?;
                validate_multiline_text(comment, MAX_TEXT_BYTES)?;
                let repository = self.resolve_repository(repository)?;
                let mr = self
                    .project_json(repository, &["merge_requests", &pr_number.to_string()], &[])
                    .await?;
                let changes = self
                    .project_json(
                        repository,
                        &["merge_requests", &pr_number.to_string(), "changes"],
                        &[],
                    )
                    .await?;
                let position = discussion_position(&mr, &changes, file_path, line_number)
                    .map_err(map_diff_error)?;
                let payload = json!({"body":comment,"position":position});
                self.project_call(
                    Method::POST,
                    repository,
                    &["merge_requests", &pr_number.to_string(), "discussions"],
                    &[],
                    Some(&payload),
                    true,
                    false,
                )
                .await?;
                bounded_output(Value::String("Comment added".to_owned()))
                    .map_err(|_| unknown_outcome())
            }
            GitLabOrgOperation::ListFiles {
                path,
                recursive,
                branch,
                repository,
            } => {
                self.list_tree(path, recursive, branch, repository, "blob", "Files")
                    .await
            }
            GitLabOrgOperation::ListFolders {
                path,
                recursive,
                branch,
                repository,
            } => {
                self.list_tree(path, recursive, branch, repository, "tree", "Folders")
                    .await
            }
            GitLabOrgOperation::AppendFile {
                file_path,
                content,
                branch,
                repository,
            } => {
                validate_file_path(file_path)?;
                validate_branch(branch)?;
                validate_multiline_text(content, MAX_WRITABLE_FILE_BYTES)?;
                let repository = self.resolve_repository(repository)?;
                let file = self
                    .read_provider_file(repository, file_path, branch)
                    .await?;
                let length = file
                    .content
                    .len()
                    .checked_add(content.len())
                    .and_then(|value| value.checked_add(1))
                    .ok_or_else(resource_exhausted)?;
                if length > MAX_WRITABLE_FILE_BYTES {
                    return Err(resource_exhausted());
                }
                let mut updated = String::with_capacity(length);
                updated.push_str(&file.content);
                updated.push('\n');
                updated.push_str(content);
                let payload = commit_payload(
                    "update",
                    file_path,
                    branch,
                    &format!("Append {file_path}"),
                    &updated,
                    &file.last_commit_id,
                );
                self.project_call(
                    Method::POST,
                    repository,
                    &["repository", "commits"],
                    &[],
                    Some(&payload),
                    true,
                    false,
                )
                .await?;
                bounded_output(Value::String(format!("Updated file {file_path}")))
                    .map_err(|_| unknown_outcome())
            }
            GitLabOrgOperation::GetCommits {
                sha,
                path,
                since,
                until,
                author,
                repository,
            } => {
                for value in [sha, path, author].into_iter().flatten() {
                    validate_text(value, MAX_IDENTIFIER_BYTES)?;
                }
                for value in [since, until].into_iter().flatten() {
                    validate_rfc3339(value)?;
                }
                let repository = self.resolve_repository(repository)?;
                let mut query = Vec::new();
                for (name, value) in [
                    ("ref_name", sha),
                    ("path", path),
                    ("since", since),
                    ("until", until),
                    ("author", author),
                ] {
                    if let Some(value) = value {
                        query.push((name, value.to_owned()));
                    }
                }
                let commits = self
                    .paged_project_array(
                        repository,
                        &["repository", "commits"],
                        &query,
                        MAX_COMMITS,
                    )
                    .await?;
                let mut projected = Vec::with_capacity(commits.len());
                for commit in commits {
                    projected.push(json!({
                        "sha":required_string(&commit,"id")?,
                        "author":required_string(&commit,"author_name")?,
                        "createdAt":required_string(&commit,"created_at")?,
                        "message":required_string(&commit,"message")?,
                        "url":required_string(&commit,"web_url")?
                    }));
                }
                bounded_output(Value::Array(projected))
            }
        }
    }
}

struct ProviderFile {
    content: String,
    last_commit_id: String,
}

impl GitLabOrgClient {
    async fn project_json(
        &self,
        repository: &str,
        suffix: &[&str],
        query: &[(&str, String)],
    ) -> Result<Value, GitLabOrgClientError> {
        let response = self
            .project_call(Method::GET, repository, suffix, query, None, false, false)
            .await?;
        let body = response.body.ok_or_else(invalid_response)?;
        if !body.is_object() {
            return Err(invalid_response());
        }
        Ok(body)
    }

    async fn paged_project_array(
        &self,
        repository: &str,
        suffix: &[&str],
        filters: &[(&str, String)],
        max_items: usize,
    ) -> Result<Vec<Value>, GitLabOrgClientError> {
        let mut output = Vec::new();
        let mut page = 1usize;
        loop {
            if page > MAX_PAGES {
                return Err(resource_exhausted());
            }
            let mut query = filters.to_vec();
            query.push(("per_page", "100".to_owned()));
            query.push(("page", page.to_string()));
            let response = self
                .project_call(Method::GET, repository, suffix, &query, None, false, false)
                .await?;
            let values = response
                .body
                .as_ref()
                .and_then(Value::as_array)
                .ok_or_else(invalid_response)?;
            if output
                .len()
                .checked_add(values.len())
                .is_none_or(|size| size > max_items)
            {
                return Err(resource_exhausted());
            }
            output.extend(values.iter().cloned());
            let Some(next) = response.next_page.as_deref() else {
                break;
            };
            let next = next.parse::<usize>().map_err(|_| invalid_response())?;
            if next <= page {
                return Err(invalid_response());
            }
            page = next;
        }
        Ok(output)
    }

    async fn read_provider_file(
        &self,
        repository: &str,
        file_path: &str,
        branch: &str,
    ) -> Result<ProviderFile, GitLabOrgClientError> {
        let file = self
            .project_json(
                repository,
                &["repository", "files", file_path],
                &[("ref", branch.to_owned())],
            )
            .await?;
        let encoded = required_string(&file, "content")?;
        if encoded.len() > MAX_RESPONSE_BYTES {
            return Err(resource_exhausted());
        }
        let decoded = STANDARD
            .decode(encoded.as_bytes())
            .map_err(|_| invalid_response())?;
        if decoded.len() > MAX_FILE_BYTES {
            return Err(resource_exhausted());
        }
        let content = String::from_utf8(decoded).map_err(|_| invalid_response())?;
        let last_commit_id = required_string(&file, "last_commit_id")?;
        validate_text(last_commit_id, MAX_IDENTIFIER_BYTES)?;
        Ok(ProviderFile {
            content,
            last_commit_id: last_commit_id.to_owned(),
        })
    }

    async fn list_tree(
        &self,
        path: &str,
        recursive: bool,
        branch: Option<&str>,
        repository: Option<&str>,
        wanted_type: &str,
        label: &str,
    ) -> Result<Value, GitLabOrgClientError> {
        validate_tree_path(path)?;
        let resolved_branch = match branch {
            Some(branch) => {
                validate_branch(branch)?;
                branch.to_owned()
            }
            None => self.active_branch.lock().await.to_string(),
        };
        let repository = self.resolve_repository(repository)?;
        let mut filters = vec![
            ("ref", resolved_branch),
            ("recursive", recursive.to_string()),
        ];
        if !path.is_empty() {
            filters.push(("path", path.to_owned()));
        }
        let entries = self
            .paged_project_array(repository, &["repository", "tree"], &filters, MAX_ITEMS)
            .await?;
        let mut paths = Vec::new();
        for entry in entries {
            if required_string(&entry, "type")? == wanted_type {
                paths.push(required_string(&entry, "path")?.to_owned());
            }
        }
        bounded_output(Value::String(format!(
            "{label}: {}",
            python_string_list(&paths)
        )))
    }
}
