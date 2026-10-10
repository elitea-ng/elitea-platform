use std::fmt;

use reqwest::Url;
use serde_json::{Map, Value};
use zeroize::Zeroizing;

const MAX_URL_BYTES: usize = 2_048;
const MAX_TOKEN_BYTES: usize = 16 * 1_024;
const MAX_REPOSITORY_BYTES: usize = 1_024;
const MAX_BRANCH_BYTES: usize = 255;
const MAX_SELECTED_TOOLS: usize = 1_024;
const MAX_TOOL_NAME_BYTES: usize = 64;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum GitLabConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Stable failure that retains no GitLab URL, project, or credential data.
pub(crate) struct GitLabConfigError {
    code: GitLabConfigErrorCode,
}

impl GitLabConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> GitLabConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for GitLabConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("GitLabConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for GitLabConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            GitLabConfigErrorCode::InvalidConfiguration => {
                "the GitLab toolkit configuration is invalid"
            }
            GitLabConfigErrorCode::ResourceExhausted => {
                "the GitLab toolkit configuration exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for GitLabConfigError {}

/// One invocation-scoped GitLab origin, secret, single project and base branch.
///
/// The SDK `gitlab` toolkit is bound to exactly one `repository` (a project
/// path such as `group/project`, or a numeric id) and one protected base
/// `branch` (default `main`); its tools take no repository argument.
pub(crate) struct GitLabToolkitConfig {
    base_url: Url,
    private_token: Zeroizing<String>,
    repository: Box<str>,
    branch: Box<str>,
    selected_tools: Vec<Box<str>>,
}

impl GitLabToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, GitLabConfigError> {
        let configuration = settings
            .get("gitlab_configuration")
            .and_then(Value::as_object)
            .ok_or_else(invalid_configuration)?;
        let url = required_text(configuration, "url", MAX_URL_BYTES)?;
        let private_token = required_text(configuration, "private_token", MAX_TOKEN_BYTES)?;
        if !private_token
            .bytes()
            .all(|byte| (0x21..=0x7e).contains(&byte))
        {
            return Err(invalid_configuration());
        }
        // `GitLabAPIWrapper._sanitize_url` strips trailing slashes.
        let repository = required_text(settings, "repository", MAX_REPOSITORY_BYTES)?
            .trim()
            .trim_end_matches('/');
        if repository.is_empty()
            || repository.starts_with('/')
            || repository
                .split('/')
                .any(|part| part.is_empty() || matches!(part, "." | ".."))
        {
            return Err(invalid_configuration());
        }
        let branch = match settings.get("branch") {
            None | Some(Value::Null) => "main",
            Some(Value::String(value)) if value.trim().is_empty() => "main",
            Some(Value::String(value)) => {
                validate_text(value, MAX_BRANCH_BYTES)?;
                value.as_str()
            }
            Some(_) => return Err(invalid_configuration()),
        };

        Ok(Self {
            base_url: parse_base_url(url)?,
            private_token: Zeroizing::new(private_token.to_owned()),
            repository: repository.into(),
            branch: branch.into(),
            selected_tools: selected_tools(settings)?,
        })
    }

    pub(super) const fn base_url(&self) -> &Url {
        &self.base_url
    }

    pub(super) fn private_token(&self) -> &str {
        &self.private_token
    }

    pub(super) fn repository(&self) -> &str {
        &self.repository
    }

    pub(super) fn branch(&self) -> &str {
        &self.branch
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        &self.selected_tools
    }
}

fn parse_base_url(value: &str) -> Result<Url, GitLabConfigError> {
    let value = value.trim().trim_end_matches('/');
    if value.contains(['%', '\\']) {
        return Err(invalid_configuration());
    }
    let mut url = Url::parse(value).map_err(|_| invalid_configuration())?;
    if url.scheme() != "https"
        || url.host_str().is_none()
        || !url.username().is_empty()
        || url.password().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
        || !matches!(url.path(), "" | "/")
    {
        return Err(invalid_configuration());
    }
    url.set_path("");
    Ok(url)
}

fn selected_tools(settings: &Map<String, Value>) -> Result<Vec<Box<str>>, GitLabConfigError> {
    let Some(value) = settings.get("selected_tools") else {
        return Ok(Vec::new());
    };
    if value.is_null() {
        return Ok(Vec::new());
    }
    let values = value.as_array().ok_or_else(invalid_configuration)?;
    if values.len() > MAX_SELECTED_TOOLS {
        return Err(resource_exhausted());
    }
    let mut selected: Vec<Box<str>> = Vec::with_capacity(values.len().min(32));
    for value in values {
        let name = value.as_str().ok_or_else(invalid_configuration)?;
        validate_text(name, MAX_TOOL_NAME_BYTES)?;
        if !selected.iter().any(|existing| existing.as_ref() == name) {
            selected.push(name.into());
        }
    }
    Ok(selected)
}

fn required_text<'a>(
    object: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> Result<&'a str, GitLabConfigError> {
    let value = object
        .get(name)
        .and_then(Value::as_str)
        .ok_or_else(invalid_configuration)?;
    validate_text(value, limit)?;
    Ok(value)
}

fn validate_text(value: &str, limit: usize) -> Result<(), GitLabConfigError> {
    if value.len() > limit {
        return Err(resource_exhausted());
    }
    if value.trim().is_empty() || value.chars().any(char::is_control) {
        return Err(invalid_configuration());
    }
    Ok(())
}

const fn invalid_configuration() -> GitLabConfigError {
    GitLabConfigError {
        code: GitLabConfigErrorCode::InvalidConfiguration,
    }
}

const fn resource_exhausted() -> GitLabConfigError {
    GitLabConfigError {
        code: GitLabConfigErrorCode::ResourceExhausted,
    }
}
