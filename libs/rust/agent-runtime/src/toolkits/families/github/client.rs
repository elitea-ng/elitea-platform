use std::fmt;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::{STANDARD, URL_SAFE_NO_PAD};
use chrono::{DateTime, SecondsFormat};
use reqwest::header::{
    ACCEPT, AUTHORIZATION, CONTENT_LENGTH, CONTENT_TYPE, HeaderMap, HeaderValue, RETRY_AFTER,
};
use reqwest::{Method, StatusCode, Url};
use ring::rand::SystemRandom;
use ring::signature::{RSA_PKCS1_SHA256, RsaKeyPair};
use serde_json::{Map, Value, json};
use zeroize::Zeroizing;

use super::code_search::{
    MAX_CODE_SEARCH_RESPONSE_BYTES, project_code_search, scope_code_search_query,
    validate_code_search_window,
};
use super::commits::{
    COMMIT_FILES_PER_PAGE, MAX_COMMIT_FILES, MAX_COMMITS, append_commit_file_page,
    commit_response_sha, finish_commit_changes, project_commit_comparison, project_commit_list,
};
use super::config::{GitHubAuthKind, GitHubToolkitConfig};
use super::projects::{
    MAX_PROJECT_ITEMS, MAX_PROJECT_RESPONSE_BYTES, project_project_issues, project_query_payload,
};
use super::pull_requests::{
    MAX_PULL_REQUEST_FILES, MAX_PULL_REQUESTS, PULL_REQUEST_FILES_PER_PAGE,
    append_pull_request_file_page, finish_pull_request_files, project_pull_request_detail,
    project_pull_request_list, pull_request_file_count,
};
use super::workflow_runs::{
    MAX_WORKFLOW_JOBS, MAX_WORKFLOW_JOBS_RESPONSE_BYTES, project_workflow_status,
};

const CONNECT_TIMEOUT: Duration = Duration::from_secs(10);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(30);
const PROBE_TIMEOUT: Duration = Duration::from_secs(10);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_secs(30);
const MAX_IDLE_PER_HOST: usize = 8;
const MAX_RESPONSE_BYTES: usize = 512 * 1_024;
const MAX_FILE_RESPONSE_BYTES: usize = 2 * 1_024 * 1_024;
const MAX_FILE_BYTES: usize = 1_024 * 1_024;
const MAX_TREE_RESPONSE_BYTES: usize = 8 * 1_024 * 1_024;
const MAX_TREE_ENTRIES: usize = 100_000;
const MAX_PROJECTED_FILES: usize = 10_000;
const MAX_PROJECTED_FILE_CHARS: usize = 200_000;
const MAX_ISSUES: usize = 100;
const MAX_ISSUE_OUTPUT_CHARS: usize = 200_000;
const MAX_ISSUE_TITLE_BYTES: usize = 16 * 1_024;
const MAX_ISSUE_BODY_BYTES: usize = 128 * 1_024;
const MAX_ISSUE_URL_BYTES: usize = 4 * 1_024;
const MAX_ISSUE_METADATA_BYTES: usize = 1_024;
const MAX_ISSUE_COLLECTION_ITEMS: usize = 100;
const MAX_SEARCH_QUERY_BYTES: usize = 4 * 1_024;
const MAX_PULL_REQUEST_FILE_PAGE_BYTES: usize = 2 * 1_024 * 1_024;
const MAX_COMMIT_PAGE_BYTES: usize = 2 * 1_024 * 1_024;
const MAX_COMMIT_REF_BYTES: usize = 1_024;
const MAX_BRANCHES: usize = 100;
const MAX_BRANCH_BYTES: usize = 1_024;
const MAX_FILE_PATH_BYTES: usize = 4 * 1_024;
const MAX_FILE_PATH_SEGMENTS: usize = 128;
const GITHUB_ACCEPT: &str = "application/vnd.github+json";
const GITHUB_API_VERSION: &str = "2022-11-28";
const USER_AGENT: &str = "elitea-worker-rust/0.1";

/// Stable, data-free failure categories for GitHub transport and response use.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum GitHubClientErrorCode {
    InvalidConfiguration,
    InvalidInput,
    UnsupportedAuthentication,
    Authentication,
    Authorization,
    NotFound,
    RateLimited,
    Timeout,
    DependencyUnavailable,
    InvalidResponse,
    ResourceExhausted,
}

/// A safe GitHub client failure.
///
/// Upstream bodies, URLs, repositories and credential material are never
/// retained as error sources or rendered through Debug/Display.
pub struct GitHubClientError {
    code: GitHubClientErrorCode,
}

impl GitHubClientError {
    #[must_use]
    pub const fn code(&self) -> GitHubClientErrorCode {
        self.code
    }

    #[must_use]
    pub const fn retryable(&self) -> bool {
        matches!(
            self.code,
            GitHubClientErrorCode::RateLimited
                | GitHubClientErrorCode::Timeout
                | GitHubClientErrorCode::DependencyUnavailable
        )
    }

    pub fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code {
            GitHubClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "github.configuration.invalid",
                "the GitHub toolkit configuration is invalid",
            ),
            GitHubClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "github.request.invalid",
                "the GitHub request is invalid",
            ),
            GitHubClientErrorCode::UnsupportedAuthentication => (
                ErrorCategory::Unsupported,
                "github.authentication.unsupported",
                "this GitHub authentication mode is not available for the requested operation",
            ),
            GitHubClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "github.authentication.failed",
                "GitHub authentication failed",
            ),
            GitHubClientErrorCode::Authorization => (
                ErrorCategory::Forbidden,
                "github.authorization.failed",
                "GitHub did not authorize the requested operation",
            ),
            GitHubClientErrorCode::NotFound => (
                ErrorCategory::NotFound,
                "github.resource.not_found",
                "the requested GitHub resource was not found",
            ),
            GitHubClientErrorCode::RateLimited => (
                ErrorCategory::RateLimited,
                "github.rate_limited",
                "GitHub rate limited the request",
            ),
            GitHubClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "github.timeout",
                "the GitHub request timed out",
            ),
            GitHubClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "github.unavailable",
                "GitHub is unavailable",
            ),
            GitHubClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "github.response.invalid",
                "GitHub returned an invalid response",
            ),
            GitHubClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "github.response.resource_exhausted",
                "the GitHub response exceeds the approved limit",
            ),
        };
        AdkError::new(ErrorComponent::Tool, category, code, message).with_retry(RetryHint {
            should_retry: self.retryable(),
            retry_after_ms: None,
            max_attempts: None,
        })
    }
}

impl fmt::Debug for GitHubClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("GitHubClientError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for GitHubClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            GitHubClientErrorCode::InvalidConfiguration => {
                "the GitHub client configuration is invalid"
            }
            GitHubClientErrorCode::InvalidInput => "the GitHub request is invalid",
            GitHubClientErrorCode::UnsupportedAuthentication => {
                "the GitHub authentication mode is not supported for this operation"
            }
            GitHubClientErrorCode::Authentication => "GitHub authentication failed",
            GitHubClientErrorCode::Authorization => "GitHub authorization failed",
            GitHubClientErrorCode::NotFound => "the GitHub resource was not found",
            GitHubClientErrorCode::RateLimited => "GitHub rate limited the request",
            GitHubClientErrorCode::Timeout => "the GitHub request timed out",
            GitHubClientErrorCode::DependencyUnavailable => "GitHub is unavailable",
            GitHubClientErrorCode::InvalidResponse => "GitHub returned an invalid response",
            GitHubClientErrorCode::ResourceExhausted => {
                "the GitHub response exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for GitHubClientError {}

/// Operations used by the first ordinary read-only GitHub tool subset.
#[async_trait]
pub trait GitHubApi: Send + Sync {
    async fn get_authenticated_user(&self) -> Result<Value, GitHubClientError>;

    async fn list_branches(&self, max_count: usize) -> Result<Value, GitHubClientError>;

    async fn read_text_file(
        &self,
        file_path: &str,
        branch: Option<&str>,
        repository: Option<&str>,
    ) -> Result<String, GitHubClientError>;

    async fn list_repository_files(
        &self,
        scope: GitHubFileScope,
        directory_path: Option<&str>,
    ) -> Result<Value, GitHubClientError>;

    async fn list_open_issues(&self) -> Result<Value, GitHubClientError>;

    async fn get_issue(
        &self,
        issue_number: u64,
        repository: Option<&str>,
    ) -> Result<Value, GitHubClientError>;

    async fn search_issues(
        &self,
        search_query: &str,
        repository: Option<&str>,
        max_count: usize,
    ) -> Result<Value, GitHubClientError>;

    async fn list_open_pull_requests(&self, max_count: usize) -> Result<Value, GitHubClientError>;

    async fn get_pull_request(
        &self,
        pull_request_number: u64,
        repository: Option<&str>,
    ) -> Result<Value, GitHubClientError>;

    async fn list_pull_request_files(
        &self,
        pull_request_number: u64,
        repository: Option<&str>,
    ) -> Result<Value, GitHubClientError>;

    async fn list_commits(&self, query: GitHubCommitQuery) -> Result<Value, GitHubClientError>;

    async fn get_commit_changes(
        &self,
        reference: &str,
        repository: Option<&str>,
    ) -> Result<Value, GitHubClientError>;

    async fn compare_commits(
        &self,
        base_reference: &str,
        head_reference: &str,
        repository: Option<&str>,
    ) -> Result<Value, GitHubClientError>;

    async fn search_code(&self, query: GitHubCodeSearchQuery) -> Result<Value, GitHubClientError>;

    async fn get_workflow_status(
        &self,
        run_id: u64,
        repository: Option<&str>,
    ) -> Result<Value, GitHubClientError>;

    async fn list_project_issues(
        &self,
        board_repository: &str,
        project_number: u32,
        items_count: usize,
    ) -> Result<Value, GitHubClientError>;
}

/// Selects one of the two immutable branches admitted with the toolkit.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum GitHubFileScope {
    BaseBranch,
    ActiveBranch,
}

/// Validated, bounded query for the current commit-list tool.
pub struct GitHubCommitQuery {
    pub repository: Option<String>,
    pub reference: Option<String>,
    pub path: Option<String>,
    pub since: Option<String>,
    pub until: Option<String>,
    pub author: Option<String>,
    pub max_count: usize,
}

/// Validated, bounded query for one GitHub code-search provider page.
pub struct GitHubCodeSearchQuery {
    pub query: String,
    pub sort: Option<String>,
    pub order: Option<String>,
    pub per_page: usize,
    pub page: usize,
}

/// One invocation-scoped, pooled and origin-bound GitHub client.
///
/// The `reqwest::Client` is pooled across calls from the same toolkit but is
/// never placed in a process-global credential registry. Redirects are
/// disabled and request paths are appended to the admitted base URL, so tool
/// arguments cannot select another origin.
pub struct GitHubClient {
    http: reqwest::Client,
    config: GitHubToolkitConfig,
}

impl GitHubClient {
    pub fn new(config: GitHubToolkitConfig) -> Result<Self, GitHubClientError> {
        if config.auth_kind() == GitHubAuthKind::App {
            let (_, key) = config.auth().app().ok_or_else(invalid_configuration)?;
            let _ = parse_rsa_key(key)?;
        }
        let http = reqwest::Client::builder()
            .https_only(true)
            .redirect(reqwest::redirect::Policy::none())
            .connect_timeout(CONNECT_TIMEOUT)
            .timeout(REQUEST_TIMEOUT)
            .pool_idle_timeout(POOL_IDLE_TIMEOUT)
            .pool_max_idle_per_host(MAX_IDLE_PER_HOST)
            .user_agent(USER_AGENT)
            .build()
            .map_err(|_| invalid_configuration())?;
        Ok(Self { http, config })
    }

    /// Perform the current SDK connection probe through this family client.
    ///
    /// Anonymous configuration remains a validation-only success, matching the
    /// current SDK. Token/basic credentials use `/user`; GitHub App credentials
    /// use `/app` and deliberately do not require an installation.
    pub async fn probe(&self) -> Result<(), GitHubClientError> {
        if self.config.auth_kind() == GitHubAuthKind::Anonymous {
            return Ok(());
        }
        let request = self.build_request_at(
            GitHubRequestKind::Probe,
            &[],
            &[],
            PROBE_TIMEOUT,
            SystemTime::now(),
        )?;
        let response = self
            .http
            .execute(request)
            .await
            .map_err(|source| map_reqwest_error(&source))?;
        map_status(response.status(), response.headers())
    }

    fn build_request_at(
        &self,
        kind: GitHubRequestKind,
        path: &[&str],
        query: &[(&str, String)],
        timeout: Duration,
        now: SystemTime,
    ) -> Result<reqwest::Request, GitHubClientError> {
        let endpoint = match kind {
            GitHubRequestKind::Probe if self.config.auth_kind() == GitHubAuthKind::App => {
                self.endpoint(&["app"])?
            }
            GitHubRequestKind::Probe | GitHubRequestKind::AuthenticatedUser => {
                self.endpoint(&["user"])?
            }
            GitHubRequestKind::Repository => self.endpoint(path)?,
        };
        let mut endpoint = endpoint;
        if !query.is_empty() {
            endpoint
                .query_pairs_mut()
                .extend_pairs(query.iter().map(|(key, value)| (*key, value.as_str())));
        }
        let mut builder = self
            .http
            .request(Method::GET, endpoint)
            .header(ACCEPT, GITHUB_ACCEPT)
            .header("x-github-api-version", GITHUB_API_VERSION)
            .timeout(timeout);
        if let Some(authorization) = self.authorization(kind, now)? {
            builder = builder.header(AUTHORIZATION, authorization);
        }
        builder.build().map_err(|_| invalid_configuration())
    }

    fn endpoint(&self, path: &[&str]) -> Result<Url, GitHubClientError> {
        let mut endpoint = self.config.base_url().clone();
        endpoint
            .path_segments_mut()
            .map_err(|()| invalid_configuration())?
            .pop_if_empty()
            .extend(path.iter().copied());
        Ok(endpoint)
    }

    fn graphql_endpoint(&self) -> Result<Url, GitHubClientError> {
        let mut endpoint = self.config.base_url().clone();
        match endpoint.path().trim_end_matches('/') {
            "" => endpoint.set_path("/graphql"),
            "/api/v3" => endpoint.set_path("/api/graphql"),
            _ => return Err(invalid_configuration()),
        }
        Ok(endpoint)
    }

    fn build_graphql_request_at(
        &self,
        payload: &Value,
        now: SystemTime,
    ) -> Result<reqwest::Request, GitHubClientError> {
        let body = serde_json::to_vec(payload).map_err(|_| invalid_configuration())?;
        let mut builder = self
            .http
            .request(Method::POST, self.graphql_endpoint()?)
            .header(ACCEPT, "application/json")
            .header(CONTENT_TYPE, "application/json")
            .timeout(REQUEST_TIMEOUT)
            .body(body);
        if let Some(authorization) = self.authorization(GitHubRequestKind::Repository, now)? {
            builder = builder.header(AUTHORIZATION, authorization);
        }
        builder.build().map_err(|_| invalid_configuration())
    }

    fn authorization(
        &self,
        kind: GitHubRequestKind,
        now: SystemTime,
    ) -> Result<Option<HeaderValue>, GitHubClientError> {
        if let Some(token) = self.config.auth().token() {
            return secret_header("token ", token).map(Some);
        }
        if let Some((username, password)) = self.config.auth().basic() {
            let mut plaintext = Zeroizing::new(String::with_capacity(
                username
                    .len()
                    .saturating_add(password.len())
                    .saturating_add(1),
            ));
            plaintext.push_str(username);
            plaintext.push(':');
            plaintext.push_str(password);
            let encoded = Zeroizing::new(STANDARD.encode(plaintext.as_bytes()));
            return secret_header("Basic ", &encoded).map(Some);
        }
        if let Some((app_id, private_key)) = self.config.auth().app() {
            if kind != GitHubRequestKind::Probe {
                return Err(unsupported_authentication());
            }
            let jwt = github_app_jwt(app_id, private_key, now)?;
            return secret_header("Bearer ", &jwt).map(Some);
        }
        Ok(None)
    }

    async fn get_json(
        &self,
        kind: GitHubRequestKind,
        path: &[&str],
        query: &[(&str, String)],
        max_response_bytes: usize,
    ) -> Result<Value, GitHubClientError> {
        let request =
            self.build_request_at(kind, path, query, REQUEST_TIMEOUT, SystemTime::now())?;
        self.execute_json_request(request, max_response_bytes).await
    }

    async fn post_graphql_json(
        &self,
        payload: &Value,
        max_response_bytes: usize,
    ) -> Result<Value, GitHubClientError> {
        let request = self.build_graphql_request_at(payload, SystemTime::now())?;
        self.execute_json_request(request, max_response_bytes).await
    }

    async fn execute_json_request(
        &self,
        request: reqwest::Request,
        max_response_bytes: usize,
    ) -> Result<Value, GitHubClientError> {
        let mut response = self
            .http
            .execute(request)
            .await
            .map_err(|source| map_reqwest_error(&source))?;
        map_status(response.status(), response.headers())?;
        if response
            .headers()
            .get(CONTENT_LENGTH)
            .and_then(|value| value.to_str().ok())
            .and_then(|value| value.parse::<usize>().ok())
            .is_some_and(|length| length > max_response_bytes)
        {
            return Err(resource_exhausted());
        }
        let mut body = Vec::new();
        while let Some(chunk) = response
            .chunk()
            .await
            .map_err(|source| map_reqwest_error(&source))?
        {
            let next = body
                .len()
                .checked_add(chunk.len())
                .ok_or_else(resource_exhausted)?;
            if next > max_response_bytes {
                return Err(resource_exhausted());
            }
            body.extend_from_slice(&chunk);
        }
        serde_json::from_slice(&body).map_err(|_| invalid_response())
    }

    async fn resolve_tree_sha(
        &self,
        owner: &str,
        repository: &str,
        reference: &str,
    ) -> Result<String, GitHubClientError> {
        let branch = self
            .get_json(
                GitHubRequestKind::Repository,
                &["repos", owner, repository, "branches", reference],
                &[],
                MAX_RESPONSE_BYTES,
            )
            .await;
        let response = match branch {
            Ok(response) => response,
            Err(error) if error.code() == GitHubClientErrorCode::NotFound => {
                self.get_json(
                    GitHubRequestKind::Repository,
                    &["repos", owner, repository, "commits", reference],
                    &[],
                    MAX_RESPONSE_BYTES,
                )
                .await?
            }
            Err(error) => return Err(error),
        };
        project_tree_sha(&response)
    }
}

#[async_trait]
impl GitHubApi for GitHubClient {
    async fn get_authenticated_user(&self) -> Result<Value, GitHubClientError> {
        let response = self
            .get_json(
                GitHubRequestKind::AuthenticatedUser,
                &[],
                &[],
                MAX_RESPONSE_BYTES,
            )
            .await?;
        project_authenticated_user(&response)
    }

    async fn list_branches(&self, max_count: usize) -> Result<Value, GitHubClientError> {
        if max_count == 0 || max_count > MAX_BRANCHES {
            return Err(invalid_configuration());
        }
        let (owner, repository) = self
            .config
            .repository()
            .split_once('/')
            .ok_or_else(invalid_configuration)?;
        let response = self
            .get_json(
                GitHubRequestKind::Repository,
                &["repos", owner, repository, "branches"],
                &[("per_page", max_count.to_string())],
                MAX_RESPONSE_BYTES,
            )
            .await?;
        project_branches(&response, max_count)
    }

    async fn read_text_file(
        &self,
        file_path: &str,
        branch: Option<&str>,
        repository: Option<&str>,
    ) -> Result<String, GitHubClientError> {
        let repository = repository.unwrap_or_else(|| self.config.repository());
        let (owner, repository_name) = validate_repository(repository)?;
        let branch = branch.unwrap_or_else(|| self.config.active_branch());
        validate_runtime_text(branch, MAX_BRANCH_BYTES)?;
        let file_segments = validate_file_path(file_path)?;
        let mut path = Vec::with_capacity(file_segments.len().saturating_add(4));
        path.extend(["repos", owner, repository_name, "contents"]);
        path.extend(file_segments);
        let response = self
            .get_json(
                GitHubRequestKind::Repository,
                &path,
                &[("ref", branch.to_owned())],
                MAX_FILE_RESPONSE_BYTES,
            )
            .await?;
        project_text_file(&response)
    }

    async fn list_repository_files(
        &self,
        scope: GitHubFileScope,
        directory_path: Option<&str>,
    ) -> Result<Value, GitHubClientError> {
        let (owner, repository) = validate_repository(self.config.repository())?;
        let reference = match scope {
            GitHubFileScope::BaseBranch => self.config.base_branch(),
            GitHubFileScope::ActiveBranch => self.config.active_branch(),
        };
        let directory = normalize_directory_path(directory_path.unwrap_or(""))?;
        let tree_sha = self.resolve_tree_sha(owner, repository, reference).await?;
        let response = self
            .get_json(
                GitHubRequestKind::Repository,
                &["repos", owner, repository, "git", "trees", &tree_sha],
                &[("recursive", "1".to_owned())],
                MAX_TREE_RESPONSE_BYTES,
            )
            .await?;
        project_tree_files(&response, directory)
    }

    async fn list_open_issues(&self) -> Result<Value, GitHubClientError> {
        let (owner, repository) = validate_repository(self.config.repository())?;
        let response = self
            .get_json(
                GitHubRequestKind::Repository,
                &["repos", owner, repository, "issues"],
                &[
                    ("state", "open".to_owned()),
                    ("per_page", MAX_ISSUES.to_string()),
                ],
                MAX_RESPONSE_BYTES,
            )
            .await?;
        project_issue_list(&response)
    }

    async fn get_issue(
        &self,
        issue_number: u64,
        repository: Option<&str>,
    ) -> Result<Value, GitHubClientError> {
        if issue_number == 0 || i64::try_from(issue_number).is_err() {
            return Err(invalid_configuration());
        }
        let repository = repository.unwrap_or_else(|| self.config.repository());
        let (owner, repository) = validate_repository(repository)?;
        let issue_number = issue_number.to_string();
        let response = self
            .get_json(
                GitHubRequestKind::Repository,
                &["repos", owner, repository, "issues", &issue_number],
                &[],
                MAX_RESPONSE_BYTES,
            )
            .await?;
        project_issue_detail(&response)
    }

    async fn search_issues(
        &self,
        search_query: &str,
        repository: Option<&str>,
        max_count: usize,
    ) -> Result<Value, GitHubClientError> {
        if max_count == 0 || max_count > MAX_ISSUES {
            return Err(invalid_configuration());
        }
        validate_runtime_text(search_query, MAX_SEARCH_QUERY_BYTES)?;
        let repository = repository.unwrap_or_else(|| self.config.repository());
        let (owner, repository) = validate_repository(repository)?;
        let query = format!("repo:{owner}/{repository} {search_query}");
        let response = self
            .get_json(
                GitHubRequestKind::Repository,
                &["search", "issues"],
                &[
                    ("q", query),
                    ("per_page", max_count.to_string()),
                    ("page", "1".to_owned()),
                ],
                MAX_RESPONSE_BYTES,
            )
            .await?;
        project_issue_search(&response, max_count)
    }

    async fn list_open_pull_requests(&self, max_count: usize) -> Result<Value, GitHubClientError> {
        if max_count == 0 || max_count > MAX_PULL_REQUESTS {
            return Err(invalid_configuration());
        }
        let (owner, repository) = validate_repository(self.config.repository())?;
        let response = self
            .get_json(
                GitHubRequestKind::Repository,
                &["repos", owner, repository, "pulls"],
                &[
                    ("state", "open".to_owned()),
                    ("per_page", max_count.to_string()),
                    ("page", "1".to_owned()),
                ],
                MAX_RESPONSE_BYTES,
            )
            .await?;
        project_pull_request_list(&response, max_count)
    }

    async fn get_pull_request(
        &self,
        pull_request_number: u64,
        repository: Option<&str>,
    ) -> Result<Value, GitHubClientError> {
        validate_pull_request_number(pull_request_number)?;
        let repository = repository.unwrap_or_else(|| self.config.repository());
        let (owner, repository) = validate_repository(repository)?;
        let number = pull_request_number.to_string();
        let pull = self
            .get_json(
                GitHubRequestKind::Repository,
                &["repos", owner, repository, "pulls", &number],
                &[],
                MAX_RESPONSE_BYTES,
            )
            .await?;
        let comments = self
            .get_json(
                GitHubRequestKind::Repository,
                &["repos", owner, repository, "issues", &number, "comments"],
                &[("per_page", "10".to_owned()), ("page", "1".to_owned())],
                MAX_RESPONSE_BYTES,
            )
            .await?;
        let commits = self
            .get_json(
                GitHubRequestKind::Repository,
                &["repos", owner, repository, "pulls", &number, "commits"],
                &[("per_page", "10".to_owned()), ("page", "1".to_owned())],
                MAX_RESPONSE_BYTES,
            )
            .await?;
        project_pull_request_detail(&pull, &comments, &commits, pull_request_number)
    }

    async fn list_pull_request_files(
        &self,
        pull_request_number: u64,
        repository: Option<&str>,
    ) -> Result<Value, GitHubClientError> {
        validate_pull_request_number(pull_request_number)?;
        let repository = repository.unwrap_or_else(|| self.config.repository());
        let (owner, repository) = validate_repository(repository)?;
        let number = pull_request_number.to_string();
        let pull = self
            .get_json(
                GitHubRequestKind::Repository,
                &["repos", owner, repository, "pulls", &number],
                &[],
                MAX_RESPONSE_BYTES,
            )
            .await?;
        let expected_count = pull_request_file_count(&pull, pull_request_number)?;
        let page_count = expected_count.div_ceil(PULL_REQUEST_FILES_PER_PAGE);
        let mut files = Vec::with_capacity(expected_count.min(MAX_PULL_REQUEST_FILES));
        for page in 1..=page_count {
            let response = self
                .get_json(
                    GitHubRequestKind::Repository,
                    &["repos", owner, repository, "pulls", &number, "files"],
                    &[
                        ("per_page", PULL_REQUEST_FILES_PER_PAGE.to_string()),
                        ("page", page.to_string()),
                    ],
                    MAX_PULL_REQUEST_FILE_PAGE_BYTES,
                )
                .await?;
            append_pull_request_file_page(&response, &mut files)?;
        }
        finish_pull_request_files(files, expected_count)
    }

    async fn list_commits(&self, query: GitHubCommitQuery) -> Result<Value, GitHubClientError> {
        if query.max_count == 0 || query.max_count > MAX_COMMITS {
            return Err(invalid_configuration());
        }
        let repository = query
            .repository
            .as_deref()
            .unwrap_or_else(|| self.config.repository());
        let (owner, repository) = validate_repository(repository)?;
        let mut parameters = Vec::with_capacity(8);
        for (name, value) in [
            ("sha", query.reference),
            ("path", query.path),
            ("since", query.since),
            ("until", query.until),
            ("author", query.author),
        ] {
            if let Some(value) = value {
                validate_runtime_text(&value, MAX_FILE_PATH_BYTES)?;
                parameters.push((name, value));
            }
        }
        parameters.push(("per_page", query.max_count.to_string()));
        parameters.push(("page", "1".to_owned()));
        let response = self
            .get_json(
                GitHubRequestKind::Repository,
                &["repos", owner, repository, "commits"],
                &parameters,
                MAX_RESPONSE_BYTES,
            )
            .await?;
        project_commit_list(&response, query.max_count)
    }

    async fn get_commit_changes(
        &self,
        reference: &str,
        repository: Option<&str>,
    ) -> Result<Value, GitHubClientError> {
        validate_runtime_text(reference, MAX_COMMIT_REF_BYTES)?;
        let repository = repository.unwrap_or_else(|| self.config.repository());
        let (owner, repository) = validate_repository(repository)?;
        let mut first_page = None;
        let mut expected_sha = None;
        let mut files = Vec::new();
        for page in 1..=(MAX_COMMIT_FILES / COMMIT_FILES_PER_PAGE + 1) {
            let response = self
                .get_json(
                    GitHubRequestKind::Repository,
                    &["repos", owner, repository, "commits", reference],
                    &[
                        ("per_page", COMMIT_FILES_PER_PAGE.to_string()),
                        ("page", page.to_string()),
                    ],
                    MAX_COMMIT_PAGE_BYTES,
                )
                .await?;
            let sha = if let Some(sha) = expected_sha.as_deref() {
                sha
            } else {
                expected_sha = Some(commit_response_sha(&response)?);
                expected_sha.as_deref().ok_or_else(invalid_response)?
            };
            let page_len = append_commit_file_page(&response, sha, &mut files)?;
            if first_page.is_none() {
                first_page = Some(response);
            }
            if page_len < COMMIT_FILES_PER_PAGE {
                break;
            }
        }
        finish_commit_changes(first_page.as_ref().ok_or_else(invalid_response)?, &files)
    }

    async fn compare_commits(
        &self,
        base_reference: &str,
        head_reference: &str,
        repository: Option<&str>,
    ) -> Result<Value, GitHubClientError> {
        validate_runtime_text(base_reference, MAX_COMMIT_REF_BYTES)?;
        validate_runtime_text(head_reference, MAX_COMMIT_REF_BYTES)?;
        let repository = repository.unwrap_or_else(|| self.config.repository());
        let (owner, repository) = validate_repository(repository)?;
        let comparison_reference = format!("{base_reference}...{head_reference}");
        let comparison = self
            .get_json(
                GitHubRequestKind::Repository,
                &["repos", owner, repository, "compare", &comparison_reference],
                &[
                    ("per_page", MAX_COMMITS.to_string()),
                    ("page", "1".to_owned()),
                ],
                MAX_COMMIT_PAGE_BYTES,
            )
            .await?;
        project_commit_comparison(&comparison)
    }

    async fn search_code(&self, query: GitHubCodeSearchQuery) -> Result<Value, GitHubClientError> {
        validate_code_search_window(query.page, query.per_page)?;
        let scoped_query = scope_code_search_query(&query.query, self.config.repository())?;
        if query.sort.as_deref().is_some_and(|sort| sort != "indexed")
            || query
                .order
                .as_deref()
                .is_some_and(|order| !matches!(order, "asc" | "desc"))
        {
            return Err(invalid_input());
        }
        let mut parameters = Vec::with_capacity(5);
        parameters.push(("q", scoped_query));
        if let Some(sort) = query.sort {
            parameters.push(("sort", sort));
        }
        if let Some(order) = query.order {
            parameters.push(("order", order));
        }
        parameters.push(("per_page", query.per_page.to_string()));
        parameters.push(("page", query.page.to_string()));
        let response = self
            .get_json(
                GitHubRequestKind::Repository,
                &["search", "code"],
                &parameters,
                MAX_CODE_SEARCH_RESPONSE_BYTES,
            )
            .await?;
        project_code_search(&response, query.page, query.per_page)
    }

    async fn get_workflow_status(
        &self,
        run_id: u64,
        repository: Option<&str>,
    ) -> Result<Value, GitHubClientError> {
        if run_id == 0 || i64::try_from(run_id).is_err() {
            return Err(invalid_input());
        }
        let repository = repository.unwrap_or_else(|| self.config.repository());
        let (owner, repository) = validate_repository(repository)?;
        let run_id_segment = run_id.to_string();
        let run = self
            .get_json(
                GitHubRequestKind::Repository,
                &[
                    "repos",
                    owner,
                    repository,
                    "actions",
                    "runs",
                    &run_id_segment,
                ],
                &[],
                MAX_RESPONSE_BYTES,
            )
            .await?;
        let jobs = self
            .get_json(
                GitHubRequestKind::Repository,
                &[
                    "repos",
                    owner,
                    repository,
                    "actions",
                    "runs",
                    &run_id_segment,
                    "jobs",
                ],
                &[
                    ("per_page", MAX_WORKFLOW_JOBS.to_string()),
                    ("page", "1".to_owned()),
                ],
                MAX_WORKFLOW_JOBS_RESPONSE_BYTES,
            )
            .await?;
        project_workflow_status(&run, &jobs, run_id)
    }

    async fn list_project_issues(
        &self,
        board_repository: &str,
        project_number: u32,
        items_count: usize,
    ) -> Result<Value, GitHubClientError> {
        if items_count == 0 || items_count > MAX_PROJECT_ITEMS {
            return Err(invalid_input());
        }
        let (owner, repository) = validate_repository(board_repository)?;
        let payload = project_query_payload(owner, repository, project_number, items_count);
        let response = self
            .post_graphql_json(&payload, MAX_PROJECT_RESPONSE_BYTES)
            .await?;
        project_project_issues(&response, items_count)
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum GitHubRequestKind {
    Probe,
    AuthenticatedUser,
    Repository,
}

fn project_authenticated_user(value: &Value) -> Result<Value, GitHubClientError> {
    const FIELDS: &[&str] = &[
        "login",
        "id",
        "name",
        "email",
        "bio",
        "company",
        "location",
        "blog",
        "twitter_username",
        "public_repos",
        "public_gists",
        "followers",
        "following",
        "created_at",
        "updated_at",
        "html_url",
        "avatar_url",
        "type",
        "hireable",
        "private_gists",
        "total_private_repos",
        "owned_private_repos",
    ];
    let object = value.as_object().ok_or_else(invalid_response)?;
    if !object
        .get("login")
        .and_then(Value::as_str)
        .is_some_and(|login| !login.is_empty() && login.len() <= 256)
    {
        return Err(invalid_response());
    }
    let mut projected = Map::new();
    for field in FIELDS {
        if let Some(value) = object.get(*field).filter(|value| !value.is_null()) {
            projected.insert((*field).to_owned(), value.clone());
        }
    }
    Ok(Value::Object(projected))
}

fn project_branches(value: &Value, max_count: usize) -> Result<Value, GitHubClientError> {
    let branches = value.as_array().ok_or_else(invalid_response)?;
    if branches.len() > max_count || branches.len() > MAX_BRANCHES {
        return Err(resource_exhausted());
    }
    let mut projected = Vec::with_capacity(branches.len());
    for branch in branches {
        let branch = branch.as_object().ok_or_else(invalid_response)?;
        let name = branch
            .get("name")
            .and_then(Value::as_str)
            .filter(|name| {
                !name.is_empty()
                    && name.len() <= 1_024
                    && !name.chars().any(|character| character.is_ascii_control())
            })
            .ok_or_else(invalid_response)?;
        let protected_flag = branch
            .get("protected")
            .and_then(Value::as_bool)
            .ok_or_else(invalid_response)?;
        projected.push(json!({"name": name, "protected": protected_flag}));
    }
    Ok(Value::Array(projected))
}

fn project_text_file(value: &Value) -> Result<String, GitHubClientError> {
    let object = value.as_object().ok_or_else(invalid_response)?;
    if object.get("type").and_then(Value::as_str) != Some("file")
        || object.get("encoding").and_then(Value::as_str) != Some("base64")
    {
        return Err(invalid_response());
    }
    let declared_size = object
        .get("size")
        .and_then(Value::as_u64)
        .and_then(|size| usize::try_from(size).ok())
        .ok_or_else(invalid_response)?;
    if declared_size > MAX_FILE_BYTES {
        return Err(resource_exhausted());
    }
    let encoded = object
        .get("content")
        .and_then(Value::as_str)
        .ok_or_else(invalid_response)?;
    if encoded.len() > MAX_FILE_RESPONSE_BYTES {
        return Err(resource_exhausted());
    }
    let compact = encoded
        .bytes()
        .filter(|byte| !byte.is_ascii_whitespace())
        .collect::<Vec<_>>();
    let decoded = STANDARD.decode(compact).map_err(|_| invalid_response())?;
    if decoded.len() > MAX_FILE_BYTES {
        return Err(resource_exhausted());
    }
    if decoded.len() != declared_size {
        return Err(invalid_response());
    }
    String::from_utf8(decoded).map_err(|_| invalid_response())
}

fn project_tree_sha(value: &Value) -> Result<String, GitHubClientError> {
    let sha = value
        .get("commit")
        .and_then(|commit| commit.get("commit"))
        .and_then(|commit| commit.get("tree"))
        .and_then(|tree| tree.get("sha"))
        .and_then(Value::as_str)
        .filter(|sha| {
            matches!(sha.len(), 40 | 64) && sha.bytes().all(|byte| byte.is_ascii_hexdigit())
        })
        .ok_or_else(invalid_response)?;
    Ok(sha.to_ascii_lowercase())
}

fn project_tree_files(value: &Value, directory: &str) -> Result<Value, GitHubClientError> {
    let object = value.as_object().ok_or_else(invalid_response)?;
    match object.get("truncated").and_then(Value::as_bool) {
        Some(false) => {}
        Some(true) => return Err(resource_exhausted()),
        None => return Err(invalid_response()),
    }
    let tree = object
        .get("tree")
        .and_then(Value::as_array)
        .ok_or_else(invalid_response)?;
    if tree.len() > MAX_TREE_ENTRIES {
        return Err(resource_exhausted());
    }
    let prefix = if directory.is_empty() {
        String::new()
    } else {
        format!("{directory}/")
    };
    let mut files = Vec::new();
    let mut projected_chars = 2_usize;
    for entry in tree {
        let entry = entry.as_object().ok_or_else(invalid_response)?;
        let entry_type = entry
            .get("type")
            .and_then(Value::as_str)
            .filter(|entry_type| matches!(*entry_type, "blob" | "tree" | "commit"))
            .ok_or_else(invalid_response)?;
        let path = entry
            .get("path")
            .and_then(Value::as_str)
            .ok_or_else(invalid_response)?;
        validate_response_path(path)?;
        if entry_type == "blob" && (prefix.is_empty() || path.starts_with(&prefix)) {
            if files.len() >= MAX_PROJECTED_FILES {
                return Err(resource_exhausted());
            }
            let encoded_chars = serde_json::to_string(path)
                .map_err(|_| invalid_response())?
                .chars()
                .count();
            projected_chars = projected_chars
                .checked_add(encoded_chars)
                .and_then(|value| value.checked_add(usize::from(!files.is_empty())))
                .ok_or_else(resource_exhausted)?;
            if projected_chars > MAX_PROJECTED_FILE_CHARS {
                return Err(resource_exhausted());
            }
            files.push(Value::String(path.to_owned()));
        }
    }
    Ok(Value::Array(files))
}

fn project_issue_detail(value: &Value) -> Result<Value, GitHubClientError> {
    let issue = value.as_object().ok_or_else(invalid_response)?;
    let mut projected = Map::new();
    projected.insert("number".to_owned(), Value::from(issue_number(issue)?));
    projected.insert(
        "title".to_owned(),
        Value::String(required_issue_text(issue, "title", MAX_ISSUE_TITLE_BYTES)?.to_owned()),
    );
    projected.insert("body".to_owned(), issue_body(issue)?);
    projected.insert(
        "state".to_owned(),
        Value::String(issue_state(issue)?.to_owned()),
    );
    projected.insert(
        "url".to_owned(),
        Value::String(required_issue_text(issue, "html_url", MAX_ISSUE_URL_BYTES)?.to_owned()),
    );
    project_issue_timestamps(issue, &mut projected)?;
    projected.insert(
        "comments".to_owned(),
        Value::from(
            issue
                .get("comments")
                .and_then(Value::as_u64)
                .filter(|comments| i64::try_from(*comments).is_ok())
                .ok_or_else(invalid_response)?,
        ),
    );
    project_issue_people(issue, &mut projected)?;
    bounded_issue_output(Value::Object(projected))
}

fn project_issue_list(value: &Value) -> Result<Value, GitHubClientError> {
    let issues = value.as_array().ok_or_else(invalid_response)?;
    if issues.len() > MAX_ISSUES {
        return Err(resource_exhausted());
    }
    let projected = issues
        .iter()
        .map(project_issue_summary)
        .collect::<Result<Vec<_>, _>>()?;
    bounded_issue_output(Value::Array(projected))
}

fn project_issue_summary(value: &Value) -> Result<Value, GitHubClientError> {
    let issue = value.as_object().ok_or_else(invalid_response)?;
    let mut projected = Map::new();
    projected.insert("number".to_owned(), Value::from(issue_number(issue)?));
    projected.insert(
        "title".to_owned(),
        Value::String(required_issue_text(issue, "title", MAX_ISSUE_TITLE_BYTES)?.to_owned()),
    );
    projected.insert(
        "state".to_owned(),
        Value::String(issue_state(issue)?.to_owned()),
    );
    projected.insert(
        "url".to_owned(),
        Value::String(required_issue_text(issue, "html_url", MAX_ISSUE_URL_BYTES)?.to_owned()),
    );
    project_issue_timestamps(issue, &mut projected)?;
    project_issue_people(issue, &mut projected)?;
    Ok(Value::Object(projected))
}

fn project_issue_search(value: &Value, max_count: usize) -> Result<Value, GitHubClientError> {
    if max_count == 0 || max_count > MAX_ISSUES {
        return Err(invalid_configuration());
    }
    let result = value.as_object().ok_or_else(invalid_response)?;
    let total_count = result
        .get("total_count")
        .and_then(Value::as_u64)
        .ok_or_else(invalid_response)?;
    if total_count == 0 {
        return Ok(Value::String(
            "No issues or PRs found matching your query.".to_owned(),
        ));
    }
    let items = result
        .get("items")
        .and_then(Value::as_array)
        .ok_or_else(invalid_response)?;
    let count = max_count.min(usize::try_from(total_count).unwrap_or(usize::MAX));
    let projected = items
        .iter()
        .take(count)
        .map(project_issue_search_item)
        .collect::<Result<Vec<_>, _>>()?;
    bounded_issue_output(Value::Array(projected))
}

fn project_issue_search_item(value: &Value) -> Result<Value, GitHubClientError> {
    let issue = value.as_object().ok_or_else(invalid_response)?;
    let entity_type = match issue.get("pull_request") {
        None | Some(Value::Null) => "Issue",
        Some(Value::Object(_)) => "PR",
        Some(_) => return Err(invalid_response()),
    };
    Ok(json!({
        "id": issue_number(issue)?,
        "title": required_issue_text(issue, "title", MAX_ISSUE_TITLE_BYTES)?,
        "description": issue_body(issue)?,
        "status": issue_state(issue)?,
        "url": required_issue_text(issue, "html_url", MAX_ISSUE_URL_BYTES)?,
        "entity_type": entity_type,
    }))
}

fn issue_number(issue: &Map<String, Value>) -> Result<u64, GitHubClientError> {
    issue
        .get("number")
        .and_then(Value::as_u64)
        .filter(|number| *number > 0 && i64::try_from(*number).is_ok())
        .ok_or_else(invalid_response)
}

fn issue_state(issue: &Map<String, Value>) -> Result<&str, GitHubClientError> {
    required_issue_text(issue, "state", MAX_ISSUE_METADATA_BYTES).and_then(|state| {
        matches!(state, "open" | "closed")
            .then_some(state)
            .ok_or_else(invalid_response)
    })
}

fn issue_body(issue: &Map<String, Value>) -> Result<Value, GitHubClientError> {
    match issue.get("body") {
        Some(Value::Null) => Ok(Value::Null),
        Some(Value::String(body)) if body.len() <= MAX_ISSUE_BODY_BYTES => {
            Ok(Value::String(body.clone()))
        }
        _ => Err(
            if issue
                .get("body")
                .and_then(Value::as_str)
                .is_some_and(|body| body.len() > MAX_ISSUE_BODY_BYTES)
            {
                resource_exhausted()
            } else {
                invalid_response()
            },
        ),
    }
}

fn project_issue_timestamps(
    issue: &Map<String, Value>,
    projected: &mut Map<String, Value>,
) -> Result<(), GitHubClientError> {
    for field in ["created_at", "updated_at"] {
        let value = required_issue_text(issue, field, MAX_ISSUE_METADATA_BYTES)?;
        let timestamp = DateTime::parse_from_rfc3339(value)
            .map_err(|_| invalid_response())?
            .to_rfc3339_opts(SecondsFormat::AutoSi, false);
        projected.insert(field.to_owned(), Value::String(timestamp));
    }
    Ok(())
}

fn project_issue_people(
    issue: &Map<String, Value>,
    projected: &mut Map<String, Value>,
) -> Result<(), GitHubClientError> {
    projected.insert(
        "labels".to_owned(),
        project_issue_names(issue, "labels", "name")?,
    );
    projected.insert(
        "assignees".to_owned(),
        project_issue_names(issue, "assignees", "login")?,
    );
    Ok(())
}

fn project_issue_names(
    issue: &Map<String, Value>,
    collection: &str,
    field: &str,
) -> Result<Value, GitHubClientError> {
    let values = issue
        .get(collection)
        .and_then(Value::as_array)
        .ok_or_else(invalid_response)?;
    if values.len() > MAX_ISSUE_COLLECTION_ITEMS {
        return Err(resource_exhausted());
    }
    values
        .iter()
        .map(|value| {
            let object = value.as_object().ok_or_else(invalid_response)?;
            required_issue_text(object, field, MAX_ISSUE_METADATA_BYTES)
                .map(|value| Value::String(value.to_owned()))
        })
        .collect::<Result<Vec<_>, _>>()
        .map(Value::Array)
}

fn required_issue_text<'a>(
    object: &'a Map<String, Value>,
    field: &str,
    max_bytes: usize,
) -> Result<&'a str, GitHubClientError> {
    let value = object
        .get(field)
        .and_then(Value::as_str)
        .ok_or_else(invalid_response)?;
    if value.len() > max_bytes {
        return Err(resource_exhausted());
    }
    Ok(value)
}

fn bounded_issue_output(value: Value) -> Result<Value, GitHubClientError> {
    let characters = serde_json::to_string(&value)
        .map_err(|_| invalid_response())?
        .chars()
        .count();
    if characters > MAX_ISSUE_OUTPUT_CHARS {
        return Err(resource_exhausted());
    }
    Ok(value)
}

pub fn validate_repository(value: &str) -> Result<(&str, &str), GitHubClientError> {
    let (owner, repository) = value.split_once('/').ok_or_else(invalid_configuration)?;
    if repository.contains('/')
        || !valid_repository_segment(owner)
        || !valid_repository_segment(repository)
    {
        return Err(invalid_configuration());
    }
    Ok((owner, repository))
}

fn validate_pull_request_number(value: u64) -> Result<(), GitHubClientError> {
    if value == 0 || i64::try_from(value).is_err() {
        return Err(invalid_configuration());
    }
    Ok(())
}

fn valid_repository_segment(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 256
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.'))
}

fn validate_runtime_text(value: &str, max_bytes: usize) -> Result<(), GitHubClientError> {
    if value.is_empty()
        || value.len() > max_bytes
        || value.chars().any(|character| character.is_ascii_control())
    {
        return Err(if value.len() > max_bytes {
            resource_exhausted()
        } else {
            invalid_configuration()
        });
    }
    Ok(())
}

fn validate_file_path(value: &str) -> Result<Vec<&str>, GitHubClientError> {
    validate_runtime_text(value, MAX_FILE_PATH_BYTES)?;
    if value.starts_with('/') || value.ends_with('/') {
        return Err(invalid_configuration());
    }
    let segments = value.split('/').collect::<Vec<_>>();
    if segments.len() > MAX_FILE_PATH_SEGMENTS
        || segments
            .iter()
            .any(|segment| segment.is_empty() || matches!(*segment, "." | ".."))
    {
        return Err(if segments.len() > MAX_FILE_PATH_SEGMENTS {
            resource_exhausted()
        } else {
            invalid_configuration()
        });
    }
    Ok(segments)
}

fn normalize_directory_path(value: &str) -> Result<&str, GitHubClientError> {
    let normalized = value.trim_matches('/');
    if normalized.is_empty() {
        return Ok("");
    }
    let _ = validate_file_path(normalized)?;
    Ok(normalized)
}

fn validate_response_path(value: &str) -> Result<(), GitHubClientError> {
    validate_file_path(value).map(|_| ()).map_err(|error| {
        if error.code() == GitHubClientErrorCode::ResourceExhausted {
            error
        } else {
            invalid_response()
        }
    })
}

fn map_status(status: StatusCode, headers: &HeaderMap) -> Result<(), GitHubClientError> {
    match status {
        StatusCode::OK => Ok(()),
        StatusCode::UNAUTHORIZED => Err(error(GitHubClientErrorCode::Authentication)),
        StatusCode::FORBIDDEN if github_rate_limited(headers) => {
            Err(error(GitHubClientErrorCode::RateLimited))
        }
        StatusCode::FORBIDDEN => Err(error(GitHubClientErrorCode::Authorization)),
        StatusCode::NOT_FOUND => Err(error(GitHubClientErrorCode::NotFound)),
        StatusCode::UNPROCESSABLE_ENTITY => Err(invalid_input()),
        StatusCode::TOO_MANY_REQUESTS => Err(error(GitHubClientErrorCode::RateLimited)),
        status if status.is_server_error() => {
            Err(error(GitHubClientErrorCode::DependencyUnavailable))
        }
        _ => Err(invalid_response()),
    }
}

fn github_rate_limited(headers: &HeaderMap) -> bool {
    headers
        .get("x-ratelimit-remaining")
        .and_then(|value| value.to_str().ok())
        .is_some_and(|value| value.trim() == "0")
        || headers.contains_key(RETRY_AFTER)
}

fn map_reqwest_error(source: &reqwest::Error) -> GitHubClientError {
    if source.is_timeout() {
        return error_code(GitHubClientErrorCode::Timeout);
    }
    if source.is_connect() || source.is_request() || source.is_body() {
        return error_code(GitHubClientErrorCode::DependencyUnavailable);
    }
    invalid_response()
}

fn github_app_jwt(
    app_id: &str,
    private_key: &str,
    now: SystemTime,
) -> Result<Zeroizing<String>, GitHubClientError> {
    let issued_at = now
        .duration_since(UNIX_EPOCH)
        .map_err(|_| invalid_configuration())?
        .as_secs();
    let expires_at = issued_at
        .checked_add(600)
        .ok_or_else(invalid_configuration)?;
    let header = URL_SAFE_NO_PAD.encode(br#"{"alg":"RS256","typ":"JWT"}"#);
    let payload = Zeroizing::new(
        serde_json::to_vec(&json!({"iat": issued_at, "exp": expires_at, "iss": app_id}))
            .map_err(|_| invalid_configuration())?,
    );
    let encoded_payload = Zeroizing::new(URL_SAFE_NO_PAD.encode(payload.as_slice()));
    let mut signing_input = Zeroizing::new(format!("{header}.{}", encoded_payload.as_str()));
    let key_pair = parse_rsa_key(private_key)?;
    let random = SystemRandom::new();
    let mut signature = Zeroizing::new(vec![0_u8; key_pair.public().modulus_len()]);
    key_pair
        .sign(
            &RSA_PKCS1_SHA256,
            &random,
            signing_input.as_bytes(),
            &mut signature,
        )
        .map_err(|_| invalid_configuration())?;
    let encoded_signature = Zeroizing::new(URL_SAFE_NO_PAD.encode(signature.as_slice()));
    signing_input.push('.');
    signing_input.push_str(&encoded_signature);
    Ok(signing_input)
}

// The decoded DER and all JWT buffers are zeroized by this module. `ring` does
// not promise zeroization of `RsaKeyPair`'s internal key schedule, so complete
// erasure remains a process-isolation and process-termination property.
fn parse_rsa_key(private_key: &str) -> Result<RsaKeyPair, GitHubClientError> {
    const PKCS1_BEGIN: &str = "-----BEGIN RSA PRIVATE KEY-----";
    const PKCS1_END: &str = "-----END RSA PRIVATE KEY-----";
    const PKCS8_BEGIN: &str = "-----BEGIN PRIVATE KEY-----";
    const PKCS8_END: &str = "-----END PRIVATE KEY-----";

    let (kind, body) = if private_key.contains(PKCS1_BEGIN) {
        (
            PemKind::Pkcs1,
            remove_pem_markers(private_key, PKCS1_BEGIN, PKCS1_END)?,
        )
    } else if private_key.contains(PKCS8_BEGIN) {
        (
            PemKind::Pkcs8,
            remove_pem_markers(private_key, PKCS8_BEGIN, PKCS8_END)?,
        )
    } else if private_key.contains(PKCS1_END) || private_key.contains(PKCS8_END) {
        return Err(invalid_configuration());
    } else {
        (PemKind::Pkcs1, private_key)
    };
    let compact = Zeroizing::new(
        body.chars()
            .filter(|character| !character.is_ascii_whitespace())
            .collect::<String>(),
    );
    if compact.is_empty() || compact.len() > 128 * 1_024 {
        return Err(invalid_configuration());
    }
    let der = Zeroizing::new(
        STANDARD
            .decode(compact.as_bytes())
            .map_err(|_| invalid_configuration())?,
    );
    match kind {
        PemKind::Pkcs1 => RsaKeyPair::from_der(&der),
        PemKind::Pkcs8 => RsaKeyPair::from_pkcs8(&der),
    }
    .map_err(|_| invalid_configuration())
}

fn remove_pem_markers<'a>(
    value: &'a str,
    begin: &str,
    end: &str,
) -> Result<&'a str, GitHubClientError> {
    let (_, after_begin) = value.split_once(begin).ok_or_else(invalid_configuration)?;
    let (body, after_end) = after_begin
        .split_once(end)
        .ok_or_else(invalid_configuration)?;
    if !after_end.trim().is_empty() {
        return Err(invalid_configuration());
    }
    Ok(body)
}

#[derive(Clone, Copy)]
enum PemKind {
    Pkcs1,
    Pkcs8,
}

fn secret_header(prefix: &str, secret: &str) -> Result<HeaderValue, GitHubClientError> {
    let mut value = Zeroizing::new(String::with_capacity(
        prefix.len().saturating_add(secret.len()),
    ));
    value.push_str(prefix);
    value.push_str(secret);
    HeaderValue::from_str(&value).map_err(|_| invalid_configuration())
}

pub const fn error(code: GitHubClientErrorCode) -> GitHubClientError {
    GitHubClientError { code }
}

const fn error_code(code: GitHubClientErrorCode) -> GitHubClientError {
    error(code)
}

const fn invalid_configuration() -> GitHubClientError {
    error(GitHubClientErrorCode::InvalidConfiguration)
}

pub const fn invalid_input() -> GitHubClientError {
    error(GitHubClientErrorCode::InvalidInput)
}

const fn unsupported_authentication() -> GitHubClientError {
    error(GitHubClientErrorCode::UnsupportedAuthentication)
}

pub const fn invalid_response() -> GitHubClientError {
    error(GitHubClientErrorCode::InvalidResponse)
}

pub const fn resource_exhausted() -> GitHubClientError {
    error(GitHubClientErrorCode::ResourceExhausted)
}

#[cfg(any(test, feature = "test-support"))]
impl GitHubClient {
    pub fn test_request(
        &self,
        kind: GitHubRequestKind,
        path: &[&str],
        query: &[(&str, String)],
        now: SystemTime,
    ) -> Result<reqwest::Request, GitHubClientError> {
        self.build_request_at(kind, path, query, REQUEST_TIMEOUT, now)
    }

    pub fn test_graphql_request(
        &self,
        payload: &Value,
        now: SystemTime,
    ) -> Result<reqwest::Request, GitHubClientError> {
        self.build_graphql_request_at(payload, now)
    }
}

#[cfg(any(test, feature = "test-support"))]
pub fn test_project_user(value: &Value) -> Result<Value, GitHubClientError> {
    project_authenticated_user(value)
}

#[cfg(any(test, feature = "test-support"))]
pub fn test_project_branches(value: &Value, max_count: usize) -> Result<Value, GitHubClientError> {
    project_branches(value, max_count)
}

#[cfg(any(test, feature = "test-support"))]
pub fn test_project_text_file(value: &Value) -> Result<String, GitHubClientError> {
    project_text_file(value)
}

#[cfg(any(test, feature = "test-support"))]
pub fn test_validate_file_path(value: &str) -> Result<(), GitHubClientError> {
    validate_file_path(value).map(|_| ())
}

#[cfg(any(test, feature = "test-support"))]
pub fn test_project_tree_files(value: &Value, directory: &str) -> Result<Value, GitHubClientError> {
    project_tree_files(value, directory)
}

#[cfg(any(test, feature = "test-support"))]
pub fn test_project_tree_sha(value: &Value) -> Result<String, GitHubClientError> {
    project_tree_sha(value)
}

#[cfg(any(test, feature = "test-support"))]
pub fn test_project_issue_detail(value: &Value) -> Result<Value, GitHubClientError> {
    project_issue_detail(value)
}

#[cfg(any(test, feature = "test-support"))]
pub fn test_project_issue_list(value: &Value) -> Result<Value, GitHubClientError> {
    project_issue_list(value)
}

#[cfg(any(test, feature = "test-support"))]
pub fn test_project_issue_search(
    value: &Value,
    max_count: usize,
) -> Result<Value, GitHubClientError> {
    project_issue_search(value, max_count)
}

#[cfg(any(test, feature = "test-support"))]
pub fn test_map_status(status: StatusCode, headers: &HeaderMap) -> Result<(), GitHubClientError> {
    map_status(status, headers)
}
