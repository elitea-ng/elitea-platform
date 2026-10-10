use std::fmt;

use reqwest::Url;
use serde_json::{Map, Value};
use zeroize::Zeroizing;

const MAX_URL_BYTES: usize = 2_048;
const MAX_SECRET_BYTES: usize = 16 * 1_024;
const MAX_IDENTITY_BYTES: usize = 1_024;
const MAX_BRANCH_BYTES: usize = 255;
const MAX_SELECTED_TOOLS: usize = 1_024;
const MAX_TOOL_NAME_BYTES: usize = 64;
const CLOUD_API_ORIGIN: &str = "https://api.bitbucket.org";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum BitbucketConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Stable failure that retains no Bitbucket URL, repository, or credential.
pub(crate) struct BitbucketConfigError {
    code: BitbucketConfigErrorCode,
}

impl BitbucketConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> BitbucketConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for BitbucketConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("BitbucketConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for BitbucketConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            BitbucketConfigErrorCode::InvalidConfiguration => {
                "the Bitbucket toolkit configuration is invalid"
            }
            BitbucketConfigErrorCode::ResourceExhausted => {
                "the Bitbucket toolkit configuration exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for BitbucketConfigError {}

/// Which Bitbucket API the toolkit speaks.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum BitbucketHosting {
    /// Bitbucket Cloud REST 2.0 under `/2.0/repositories/{workspace}/{repo}`.
    Cloud,
    /// Bitbucket Server / Data Center REST 1.0 under
    /// `/rest/api/1.0/projects/{project}/repos/{repo}`.
    Server,
}

/// One invocation-scoped Bitbucket origin, Basic credential, repository and
/// protected base branch.
pub(crate) struct BitbucketToolkitConfig {
    base_url: Url,
    hosting: BitbucketHosting,
    username: Box<str>,
    password: Zeroizing<String>,
    project: Box<str>,
    repository: Box<str>,
    branch: Box<str>,
    selected_tools: Vec<Box<str>>,
}

impl BitbucketToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, BitbucketConfigError> {
        let configuration = settings
            .get("bitbucket_configuration")
            .and_then(Value::as_object)
            .ok_or_else(invalid_configuration)?;
        let url = parse_url(required_text(configuration, "url", MAX_URL_BYTES)?)?;
        let username = required_text(configuration, "username", MAX_IDENTITY_BYTES)?;
        let password = required_text(configuration, "password", MAX_SECRET_BYTES)?;
        if username.contains(':') {
            return Err(invalid_configuration());
        }
        let project = required_text(settings, "project", MAX_IDENTITY_BYTES)?.trim();
        let repository = required_text(settings, "repository", MAX_IDENTITY_BYTES)?.trim();
        for value in [project, repository] {
            if value.contains('/') || matches!(value, "." | "..") {
                return Err(invalid_configuration());
            }
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
        // The SDK resolves an unset `cloud` from a top-level `url` that the
        // toolkit never carries (the URL lives in `bitbucket_configuration`),
        // so it always fell back to Server. The evident intent, a
        // bitbucket.org host meaning Cloud, is applied here.
        let hosting = match settings.get("cloud") {
            Some(Value::Bool(true)) => BitbucketHosting::Cloud,
            Some(Value::Bool(false)) => BitbucketHosting::Server,
            None | Some(Value::Null) => {
                if is_bitbucket_org(&url) {
                    BitbucketHosting::Cloud
                } else {
                    BitbucketHosting::Server
                }
            }
            Some(_) => return Err(invalid_configuration()),
        };
        let base_url = match hosting {
            // `bitbucket.org` itself serves the website; its API is on `api.`.
            BitbucketHosting::Cloud if is_bitbucket_org(&url) => {
                Url::parse(CLOUD_API_ORIGIN).map_err(|_| invalid_configuration())?
            }
            _ => url,
        };
        Ok(Self {
            base_url,
            hosting,
            username: username.into(),
            password: Zeroizing::new(password.to_owned()),
            project: project.into(),
            repository: repository.into(),
            branch: branch.into(),
            selected_tools: selected_tools(settings)?,
        })
    }

    pub(super) const fn base_url(&self) -> &Url {
        &self.base_url
    }

    pub(crate) const fn hosting(&self) -> BitbucketHosting {
        self.hosting
    }

    pub(super) fn username(&self) -> &str {
        &self.username
    }

    pub(super) fn password(&self) -> &str {
        &self.password
    }

    pub(super) fn project(&self) -> &str {
        &self.project
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

fn is_bitbucket_org(url: &Url) -> bool {
    url.host_str().is_some_and(|host| {
        let host = host.to_ascii_lowercase();
        host == "bitbucket.org" || host.ends_with(".bitbucket.org")
    })
}

/// An HTTPS origin with an optional context path (Bitbucket Server is often
/// served under `/bitbucket`), never credentials, a query or a fragment.
fn parse_url(value: &str) -> Result<Url, BitbucketConfigError> {
    let value = value.trim().trim_end_matches('/');
    if value.contains(['%', '\\']) {
        return Err(invalid_configuration());
    }
    let url = Url::parse(value).map_err(|_| invalid_configuration())?;
    if url.scheme() != "https"
        || url.host_str().is_none()
        || !url.username().is_empty()
        || url.password().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
        || url
            .path_segments()
            .is_some_and(|mut segments| segments.any(|part| matches!(part, "." | "..")))
    {
        return Err(invalid_configuration());
    }
    Ok(url)
}

fn selected_tools(settings: &Map<String, Value>) -> Result<Vec<Box<str>>, BitbucketConfigError> {
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
) -> Result<&'a str, BitbucketConfigError> {
    let value = object
        .get(name)
        .and_then(Value::as_str)
        .ok_or_else(invalid_configuration)?;
    validate_text(value, limit)?;
    Ok(value)
}

fn validate_text(value: &str, limit: usize) -> Result<(), BitbucketConfigError> {
    if value.len() > limit {
        return Err(resource_exhausted());
    }
    if value.trim().is_empty() || value.chars().any(char::is_control) {
        return Err(invalid_configuration());
    }
    Ok(())
}

const fn invalid_configuration() -> BitbucketConfigError {
    BitbucketConfigError {
        code: BitbucketConfigErrorCode::InvalidConfiguration,
    }
}

const fn resource_exhausted() -> BitbucketConfigError {
    BitbucketConfigError {
        code: BitbucketConfigErrorCode::ResourceExhausted,
    }
}
