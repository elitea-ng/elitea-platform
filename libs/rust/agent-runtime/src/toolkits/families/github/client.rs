#[cfg(test)]
use std::time::SystemTime;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD;
use chrono::{DateTime, SecondsFormat};
use serde_json::{Map, Value, json};

// The GitHub REST client lives in `elitea-connectors` (ADR-0030 decision 3),
// shared with the GitHub connector; this module keeps the family's tool
// subset and its SDK-shaped projections.
#[cfg(test)]
use elitea_connectors::github::client::project_tree_sha;
use elitea_connectors::github::client::{
    CLIENT_POLICY, GitHubRest, MAX_RESPONSE_BYTES, invalid_configuration,
};
#[allow(unused_imports)] // The family's submodules and suites each use a subset.
pub(in crate::toolkits) use elitea_connectors::github::client::{
    GitHubClientError, GitHubClientErrorCode, GitHubRequestKind, error, invalid_input,
    invalid_response, resource_exhausted, validate_repository,
};
#[cfg(test)]
use elitea_connectors::github::client::{REQUEST_TIMEOUT, map_status};
#[cfg(test)]
use elitea_connectors::transport::{HeaderMap, Request, StatusCode};

use super::code_search::{
    MAX_CODE_SEARCH_RESPONSE_BYTES, project_code_search, scope_code_search_query,
    validate_code_search_window,
};
use super::commits::{
    COMMIT_FILES_PER_PAGE, MAX_COMMIT_FILES, MAX_COMMITS, append_commit_file_page,
    commit_response_sha, finish_commit_changes, project_commit_comparison, project_commit_list,
};
use super::config::GitHubToolkitConfig;
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
use crate::toolkits::families::connector_client::{IntoAdk, transport};

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

impl IntoAdk for GitHubClientError {
    fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code() {
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

/// One invocation-scoped GitHub family client over the shared REST client
/// and the worker's transport.
pub(crate) struct GitHubClient {
    rest: GitHubRest,
}

impl GitHubClient {
    pub(crate) fn new(config: GitHubToolkitConfig) -> Result<Self, GitHubClientError> {
        let transport = transport(&CLIENT_POLICY).map_err(|_| invalid_configuration())?;
        Ok(Self {
            rest: GitHubRest::new(config, transport)?,
        })
    }

    /// Perform the current SDK connection probe through this family client.
    pub(crate) async fn probe(&self) -> Result<(), GitHubClientError> {
        self.rest.probe().await
    }
}

/// Operations used by the first ordinary read-only GitHub tool subset.
#[async_trait]
pub(crate) trait GitHubApi: Send + Sync {
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
pub(in crate::toolkits) enum GitHubFileScope {
    BaseBranch,
    ActiveBranch,
}

/// Validated, bounded query for the current commit-list tool.
pub(in crate::toolkits) struct GitHubCommitQuery {
    pub(in crate::toolkits) repository: Option<String>,
    pub(in crate::toolkits) reference: Option<String>,
    pub(in crate::toolkits) path: Option<String>,
    pub(in crate::toolkits) since: Option<String>,
    pub(in crate::toolkits) until: Option<String>,
    pub(in crate::toolkits) author: Option<String>,
    pub(in crate::toolkits) max_count: usize,
}

/// Validated, bounded query for one GitHub code-search provider page.
pub(in crate::toolkits) struct GitHubCodeSearchQuery {
    pub(in crate::toolkits) query: String,
    pub(in crate::toolkits) sort: Option<String>,
    pub(in crate::toolkits) order: Option<String>,
    pub(in crate::toolkits) per_page: usize,
    pub(in crate::toolkits) page: usize,
}

#[async_trait]
impl GitHubApi for GitHubClient {
    async fn get_authenticated_user(&self) -> Result<Value, GitHubClientError> {
        let response = self
            .rest
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
            .rest
            .config()
            .repository()
            .split_once('/')
            .ok_or_else(invalid_configuration)?;
        let response = self
            .rest
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
        let repository = repository.unwrap_or_else(|| self.rest.config().repository());
        let (owner, repository_name) = validate_repository(repository)?;
        let branch = branch.unwrap_or_else(|| self.rest.config().active_branch());
        validate_runtime_text(branch, MAX_BRANCH_BYTES)?;
        let file_segments = validate_file_path(file_path)?;
        let mut path = Vec::with_capacity(file_segments.len().saturating_add(4));
        path.extend(["repos", owner, repository_name, "contents"]);
        path.extend(file_segments);
        let response = self
            .rest
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
        let (owner, repository) = validate_repository(self.rest.config().repository())?;
        let reference = match scope {
            GitHubFileScope::BaseBranch => self.rest.config().base_branch(),
            GitHubFileScope::ActiveBranch => self.rest.config().active_branch(),
        };
        let directory = normalize_directory_path(directory_path.unwrap_or(""))?;
        let tree_sha = self
            .rest
            .resolve_tree_sha(owner, repository, reference)
            .await?;
        let response = self
            .rest
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
        let (owner, repository) = validate_repository(self.rest.config().repository())?;
        let response = self
            .rest
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
        let repository = repository.unwrap_or_else(|| self.rest.config().repository());
        let (owner, repository) = validate_repository(repository)?;
        let issue_number = issue_number.to_string();
        let response = self
            .rest
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
        let repository = repository.unwrap_or_else(|| self.rest.config().repository());
        let (owner, repository) = validate_repository(repository)?;
        let query = format!("repo:{owner}/{repository} {search_query}");
        let response = self
            .rest
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
        let (owner, repository) = validate_repository(self.rest.config().repository())?;
        let response = self
            .rest
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
        let repository = repository.unwrap_or_else(|| self.rest.config().repository());
        let (owner, repository) = validate_repository(repository)?;
        let number = pull_request_number.to_string();
        let pull = self
            .rest
            .get_json(
                GitHubRequestKind::Repository,
                &["repos", owner, repository, "pulls", &number],
                &[],
                MAX_RESPONSE_BYTES,
            )
            .await?;
        let comments = self
            .rest
            .get_json(
                GitHubRequestKind::Repository,
                &["repos", owner, repository, "issues", &number, "comments"],
                &[("per_page", "10".to_owned()), ("page", "1".to_owned())],
                MAX_RESPONSE_BYTES,
            )
            .await?;
        let commits = self
            .rest
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
        let repository = repository.unwrap_or_else(|| self.rest.config().repository());
        let (owner, repository) = validate_repository(repository)?;
        let number = pull_request_number.to_string();
        let pull = self
            .rest
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
                .rest
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
            .unwrap_or_else(|| self.rest.config().repository());
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
            .rest
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
        let repository = repository.unwrap_or_else(|| self.rest.config().repository());
        let (owner, repository) = validate_repository(repository)?;
        let mut first_page = None;
        let mut expected_sha = None;
        let mut files = Vec::new();
        for page in 1..=(MAX_COMMIT_FILES / COMMIT_FILES_PER_PAGE + 1) {
            let response = self
                .rest
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
        let repository = repository.unwrap_or_else(|| self.rest.config().repository());
        let (owner, repository) = validate_repository(repository)?;
        let comparison_reference = format!("{base_reference}...{head_reference}");
        let comparison = self
            .rest
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
        let scoped_query = scope_code_search_query(&query.query, self.rest.config().repository())?;
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
            .rest
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
        let repository = repository.unwrap_or_else(|| self.rest.config().repository());
        let (owner, repository) = validate_repository(repository)?;
        let run_id_segment = run_id.to_string();
        let run = self
            .rest
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
            .rest
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
            .rest
            .post_graphql_json(&payload, MAX_PROJECT_RESPONSE_BYTES)
            .await?;
        project_project_issues(&response, items_count)
    }
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

fn validate_pull_request_number(value: u64) -> Result<(), GitHubClientError> {
    if value == 0 || i64::try_from(value).is_err() {
        return Err(invalid_configuration());
    }
    Ok(())
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

#[cfg(test)]
impl GitHubClient {
    pub(in crate::toolkits) fn test_request(
        &self,
        kind: GitHubRequestKind,
        path: &[&str],
        query: &[(&str, String)],
        now: SystemTime,
    ) -> Result<Request, GitHubClientError> {
        self.rest
            .build_request_at(kind, path, query, REQUEST_TIMEOUT, now)
    }

    pub(in crate::toolkits) fn test_graphql_request(
        &self,
        payload: &Value,
        now: SystemTime,
    ) -> Result<Request, GitHubClientError> {
        self.rest.build_graphql_request_at(payload, now)
    }
}

#[cfg(test)]
pub(in crate::toolkits) fn test_project_user(value: &Value) -> Result<Value, GitHubClientError> {
    project_authenticated_user(value)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_project_branches(
    value: &Value,
    max_count: usize,
) -> Result<Value, GitHubClientError> {
    project_branches(value, max_count)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_project_text_file(
    value: &Value,
) -> Result<String, GitHubClientError> {
    project_text_file(value)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_validate_file_path(value: &str) -> Result<(), GitHubClientError> {
    validate_file_path(value).map(|_| ())
}

#[cfg(test)]
pub(in crate::toolkits) fn test_project_tree_files(
    value: &Value,
    directory: &str,
) -> Result<Value, GitHubClientError> {
    project_tree_files(value, directory)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_project_tree_sha(
    value: &Value,
) -> Result<String, GitHubClientError> {
    project_tree_sha(value)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_project_issue_detail(
    value: &Value,
) -> Result<Value, GitHubClientError> {
    project_issue_detail(value)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_project_issue_list(
    value: &Value,
) -> Result<Value, GitHubClientError> {
    project_issue_list(value)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_project_issue_search(
    value: &Value,
    max_count: usize,
) -> Result<Value, GitHubClientError> {
    project_issue_search(value, max_count)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_map_status(
    status: StatusCode,
    headers: &HeaderMap,
) -> Result<(), GitHubClientError> {
    map_status(status, headers)
}
