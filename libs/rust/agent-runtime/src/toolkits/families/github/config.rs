use std::fmt;

use reqwest::Url;
use serde_json::{Map, Value};
use zeroize::Zeroizing;

const DEFAULT_BRANCH: &str = "main";
const MAX_ORIGIN_BYTES: usize = 2_048;
const MAX_REPOSITORY_BYTES: usize = 512;
const MAX_BRANCH_BYTES: usize = 1_024;
const MAX_CREDENTIAL_BYTES: usize = 64 * 1_024;
const MAX_APP_PRIVATE_KEY_BYTES: usize = 128 * 1_024;
const MAX_SELECTED_TOOLS: usize = 1_024;
const MAX_TOOL_NAME_BYTES: usize = 1_024;

/// Stable authentication selector for safe control flow and diagnostics.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum GitHubAuthKind {
    Anonymous,
    Token,
    Basic,
    App,
}

/// A malformed or unsupported materialized GitHub toolkit configuration.
///
/// Configuration values can include passwords, tokens, private keys and
/// private Enterprise hosts. Debug and display intentionally expose neither a
/// field path nor a source value.
pub(crate) struct GitHubToolkitConfigError {
    message: &'static str,
}

impl fmt::Debug for GitHubToolkitConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("GitHubToolkitConfigError")
            .finish_non_exhaustive()
    }
}

impl fmt::Display for GitHubToolkitConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.message)
    }
}

impl std::error::Error for GitHubToolkitConfigError {}

/// Claim-materialized GitHub settings for one authorized invocation.
///
/// This value is intentionally neither `Clone` nor `Debug`. Long-lived secret
/// storage uses `Zeroizing`; HTTP authorization headers are created only for a
/// concrete request. The unavoidable TLS/client copies are bounded by process
/// lifetime and are never logged or persisted by this family.
pub(crate) struct GitHubToolkitConfig {
    base_url: Url,
    repository: Box<str>,
    active_branch: Box<str>,
    base_branch: Box<str>,
    selected_tools: Vec<Box<str>>,
    auth: GitHubAuth,
}

pub(super) enum GitHubAuth {
    Anonymous,
    Token(Zeroizing<String>),
    Basic {
        username: Box<str>,
        password: Zeroizing<String>,
    },
    App {
        app_id: Box<str>,
        private_key: Zeroizing<String>,
    },
}

impl GitHubToolkitConfig {
    /// Parse the exact materialized settings shape produced by Main for the
    /// current SDK GitHub toolkit.
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, GitHubToolkitConfigError> {
        let configuration = settings
            .get("github_configuration")
            .and_then(Value::as_object)
            .ok_or_else(invalid_configuration)?;
        let base_url = parse_base_url(required_text(configuration, "base_url")?)?;
        let repository = parse_repository(required_text(settings, "repository")?)?;
        let active_branch = optional_text(settings, "active_branch")?
            .unwrap_or(DEFAULT_BRANCH)
            .to_owned()
            .into_boxed_str();
        let base_branch = optional_text(settings, "base_branch")?
            .unwrap_or(DEFAULT_BRANCH)
            .to_owned()
            .into_boxed_str();
        validate_runtime_name(&active_branch, MAX_BRANCH_BYTES)?;
        validate_runtime_name(&base_branch, MAX_BRANCH_BYTES)?;
        let selected_tools = selected_tools(settings)?;
        let auth = parse_auth(configuration)?;

        Ok(Self {
            base_url,
            repository,
            active_branch,
            base_branch,
            selected_tools,
            auth,
        })
    }

    #[must_use]
    pub(crate) const fn auth_kind(&self) -> GitHubAuthKind {
        match &self.auth {
            GitHubAuth::Anonymous => GitHubAuthKind::Anonymous,
            GitHubAuth::Token(_) => GitHubAuthKind::Token,
            GitHubAuth::Basic { .. } => GitHubAuthKind::Basic,
            GitHubAuth::App { .. } => GitHubAuthKind::App,
        }
    }

    #[must_use]
    pub(crate) fn base_url(&self) -> &Url {
        &self.base_url
    }

    #[must_use]
    pub(crate) fn repository(&self) -> &str {
        &self.repository
    }

    #[must_use]
    pub(crate) fn active_branch(&self) -> &str {
        &self.active_branch
    }

    #[must_use]
    pub(crate) fn base_branch(&self) -> &str {
        &self.base_branch
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        &self.selected_tools
    }

    pub(super) const fn auth(&self) -> &GitHubAuth {
        &self.auth
    }
}

impl GitHubAuth {
    pub(super) fn token(&self) -> Option<&str> {
        match self {
            Self::Token(value) => Some(value),
            Self::Anonymous | Self::Basic { .. } | Self::App { .. } => None,
        }
    }

    pub(super) fn basic(&self) -> Option<(&str, &str)> {
        match self {
            Self::Basic { username, password } => Some((username, password)),
            Self::Anonymous | Self::Token(_) | Self::App { .. } => None,
        }
    }

    pub(super) fn app(&self) -> Option<(&str, &str)> {
        match self {
            Self::App {
                app_id,
                private_key,
            } => Some((app_id, private_key)),
            Self::Anonymous | Self::Token(_) | Self::Basic { .. } => None,
        }
    }
}

fn parse_auth(configuration: &Map<String, Value>) -> Result<GitHubAuth, GitHubToolkitConfigError> {
    let token = optional_owned_secret(configuration, "access_token", MAX_CREDENTIAL_BYTES)?;
    let username = optional_text(configuration, "username")?;
    let password = optional_owned_secret(configuration, "password", MAX_CREDENTIAL_BYTES)?;
    let app_id = optional_text(configuration, "app_id")?;
    let private_key =
        optional_owned_secret(configuration, "app_private_key", MAX_APP_PRIVATE_KEY_BYTES)?;

    if username.is_some() != password.is_some() || app_id.is_some() != private_key.is_some() {
        return Err(invalid_authentication());
    }

    // Preserve the current SDK's precedence when more than one complete auth
    // section is present: token, then username/password, then GitHub App.
    if let Some(token) = token {
        validate_secret_text(&token, false)?;
        return Ok(GitHubAuth::Token(token));
    }
    if let (Some(username), Some(password)) = (username, password) {
        validate_runtime_name(username, MAX_CREDENTIAL_BYTES)?;
        validate_secret_text(&password, false)?;
        return Ok(GitHubAuth::Basic {
            username: username.to_owned().into_boxed_str(),
            password,
        });
    }
    if let (Some(app_id), Some(private_key)) = (app_id, private_key) {
        validate_runtime_name(app_id, MAX_CREDENTIAL_BYTES)?;
        validate_secret_text(&private_key, true)?;
        return Ok(GitHubAuth::App {
            app_id: app_id.to_owned().into_boxed_str(),
            private_key,
        });
    }
    Ok(GitHubAuth::Anonymous)
}

fn parse_base_url(value: &str) -> Result<Url, GitHubToolkitConfigError> {
    if value.len() > MAX_ORIGIN_BYTES {
        return Err(resource_exhausted());
    }
    let mut url = Url::parse(value).map_err(|_| invalid_configuration())?;
    if url.scheme() != "https"
        || !url.username().is_empty()
        || url.password().is_some()
        || url.host_str().is_none()
        || url.query().is_some()
        || url.fragment().is_some()
    {
        return Err(invalid_configuration());
    }
    let path = url.path().trim_end_matches('/').to_owned();
    url.set_path(if path.is_empty() { "/" } else { &path });
    Ok(url)
}

fn parse_repository(value: &str) -> Result<Box<str>, GitHubToolkitConfigError> {
    if value.is_empty() || value.len() > MAX_REPOSITORY_BYTES || value.contains('\0') {
        return Err(if value.len() > MAX_REPOSITORY_BYTES {
            resource_exhausted()
        } else {
            invalid_configuration()
        });
    }
    let repository = if value.starts_with("http://") || value.starts_with("https://") {
        let url = Url::parse(value).map_err(|_| invalid_configuration())?;
        let segments = url
            .path_segments()
            .ok_or_else(invalid_configuration)?
            .filter(|segment| !segment.is_empty())
            .collect::<Vec<_>>();
        if segments.len() != 2 || url.query().is_some() || url.fragment().is_some() {
            return Err(invalid_configuration());
        }
        format!("{}/{}", segments[0], trim_git_suffix(segments[1]))
    } else if let Some((host, path)) = value
        .strip_prefix("git@")
        .and_then(|value| value.split_once(':'))
    {
        if host.is_empty() {
            return Err(invalid_configuration());
        }
        trim_git_suffix(path).to_owned()
    } else {
        trim_git_suffix(value).to_owned()
    };
    let mut segments = repository.split('/');
    let owner = segments.next().unwrap_or_default();
    let name = segments.next().unwrap_or_default();
    if segments.next().is_some()
        || !valid_repository_segment(owner)
        || !valid_repository_segment(name)
    {
        return Err(invalid_configuration());
    }
    Ok(format!("{owner}/{name}").into_boxed_str())
}

fn valid_repository_segment(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 256
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.'))
}

fn trim_git_suffix(value: &str) -> &str {
    value.strip_suffix(".git").unwrap_or(value)
}

fn selected_tools(
    settings: &Map<String, Value>,
) -> Result<Vec<Box<str>>, GitHubToolkitConfigError> {
    let Some(value) = settings.get("selected_tools") else {
        return Ok(Vec::new());
    };
    let tools = value.as_array().ok_or_else(invalid_configuration)?;
    if tools.len() > MAX_SELECTED_TOOLS {
        return Err(resource_exhausted());
    }
    let mut selected: Vec<Box<str>> = Vec::with_capacity(tools.len());
    for tool in tools {
        let name = tool.as_str().ok_or_else(invalid_configuration)?;
        validate_runtime_name(name, MAX_TOOL_NAME_BYTES)?;
        if !selected.iter().any(|selected| selected.as_ref() == name) {
            selected.push(name.to_owned().into_boxed_str());
        }
    }
    Ok(selected)
}

fn required_text<'a>(
    values: &'a Map<String, Value>,
    key: &str,
) -> Result<&'a str, GitHubToolkitConfigError> {
    optional_text(values, key)?.ok_or_else(invalid_configuration)
}

fn optional_text<'a>(
    values: &'a Map<String, Value>,
    key: &str,
) -> Result<Option<&'a str>, GitHubToolkitConfigError> {
    match values.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value)) if value.trim().is_empty() => Ok(None),
        Some(Value::String(value)) => Ok(Some(value.as_str())),
        Some(_) => Err(invalid_configuration()),
    }
}

fn optional_owned_secret(
    values: &Map<String, Value>,
    key: &str,
    max_bytes: usize,
) -> Result<Option<Zeroizing<String>>, GitHubToolkitConfigError> {
    let Some(value) = optional_text(values, key)? else {
        return Ok(None);
    };
    if value.len() > max_bytes {
        return Err(resource_exhausted());
    }
    Ok(Some(Zeroizing::new(value.to_owned())))
}

fn validate_runtime_name(value: &str, max_bytes: usize) -> Result<(), GitHubToolkitConfigError> {
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

fn validate_secret_text(
    value: &str,
    allow_pem_lines: bool,
) -> Result<(), GitHubToolkitConfigError> {
    if value.is_empty()
        || value.contains('\0')
        || value.chars().any(|character| {
            character.is_ascii_control()
                && !(allow_pem_lines && matches!(character, '\r' | '\n' | '\t'))
        })
    {
        return Err(invalid_authentication());
    }
    Ok(())
}

const fn invalid_configuration() -> GitHubToolkitConfigError {
    GitHubToolkitConfigError {
        message: "the materialized GitHub toolkit configuration is invalid",
    }
}

const fn invalid_authentication() -> GitHubToolkitConfigError {
    GitHubToolkitConfigError {
        message: "the materialized GitHub authentication configuration is invalid",
    }
}

const fn resource_exhausted() -> GitHubToolkitConfigError {
    GitHubToolkitConfigError {
        message: "the materialized GitHub toolkit configuration exceeds its approved limit",
    }
}
