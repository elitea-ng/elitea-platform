//! From the `repo_config` the Go host sends to a credential-free clone
//! target.
//!
//! A port of `engine/repo_providers` (`models.py`, `factory.py`,
//! `providers.py`) as `tool_operations.generate_wiki` drives it: the
//! provider type, its configuration, the repository, the branch and the
//! project come from `repo_config` (the Go host's `ExtractRepoConfig(..)
//! .Map()`: `provider_type`, `provider_config`, `repository`, `branch`,
//! `project`, `is_cloud`), and `RepoProviderFactory.from_toolkit_config`
//! normalises them. The precedence and the repository normalisation
//! (`normalize_repository`, `_strip_git_suffix`, SSH forms, the GitHub
//! Enterprise API URL, the Bitbucket Server `scm/{project}/{repo}` path,
//! Azure DevOps organisation URLs) are Python's.
//!
//! # The credential never goes into a URL
//!
//! Python built `https://{token}@host/…` and handed it to `git` on the
//! command line, so the token reached `argv`, `.git/config` and stderr.
//! Here the URL is always credential-free, and the credential becomes the
//! `Authorization` header value git would have sent for that userinfo:
//! `Basic base64("user:password")`, with an absent password sent as empty
//! (`https://TOKEN@host` makes git/libcurl send `Basic base64("TOKEN:")`;
//! verified against git 2.x). So GitHub `{token}@` is `token:`, GitHub
//! `{user}:{token}@` is `user:token`, GitLab is `oauth2:{token}`, Bitbucket
//! is `user:password` (or `x-token-auth:{token}`), and an Azure DevOps PAT
//! is `token:`.
//!
//! # Deliberate differences from the Python engine
//!
//! * A configured base URL without a scheme (`ghe.example.com`) is refused.
//!   Python's `urlparse` found no host in it and fell back to the PUBLIC
//!   default (`github.com`), sending an Enterprise token there.
//! * Userinfo inside a configured base URL is dropped from the host.
//! * Hosts are lower-cased (DNS names are case-insensitive).
//! * A repository path that would be unsafe in a URL is refused: an empty,
//!   `.` or `..` segment, or a character outside `A-Z a-z 0-9 . _ ~ -`
//!   (Azure DevOps names are percent-encoded, as in Python, instead).
//!   Python put such text into the URL verbatim.
//! * An Azure DevOps organisation that is not a plain name is refused.
//! * A missing or blank branch is `main`; a branch is trimmed and must be a
//!   valid git ref name. Python passed `None` through as the literal
//!   branch `None`.
//! * A missing repository is a `ValueError` (Python crashed with an
//!   `AttributeError` and fell back to a degraded path).
//! * A missing or null `provider_type` is `github`, the Go host's default.
//! * Bitbucket Server without a `project` keeps the `scm/` prefix of a
//!   `PROJECT/repo` path (Python dropped it and built a URL the server does
//!   not serve).
//! * Usernames are trimmed, as Python already trimmed secrets.

use super::secret::Secret;
use crate::errors::{EngineError, ErrorType};
use crate::source::{py_repr, py_str, py_truthy};
use base64::Engine as _;
use regex::Regex;
use serde_json::{Map, Value};
use std::sync::LazyLock;

/// `ProviderType`.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ProviderType {
    GitHub,
    GitLab,
    Bitbucket,
    AzureDevOps,
}

impl ProviderType {
    /// `ProviderType.value`.
    #[must_use]
    pub fn value(self) -> &'static str {
        match self {
            Self::GitHub => "github",
            Self::GitLab => "gitlab",
            Self::Bitbucket => "bitbucket",
            Self::AzureDevOps => "ado",
        }
    }

    /// `ProviderType.from_string`, with its aliases.
    ///
    /// # Errors
    ///
    /// A `ValueError` for an unknown name.
    pub fn from_name(value: &str) -> Result<Self, EngineError> {
        match value.trim().to_lowercase().as_str() {
            "github" | "gh" => Ok(Self::GitHub),
            "gitlab" | "gl" => Ok(Self::GitLab),
            "bitbucket" | "bb" => Ok(Self::Bitbucket),
            "ado" | "ado_repos" | "azure" | "azure_devops" | "azuredevops" | "vsts" => {
                Ok(Self::AzureDevOps)
            }
            _ => Err(value_error(format!("Unknown provider type: {value}"))),
        }
    }
}

/// Everything a clone needs, and nothing that can leak.
///
/// `host` is parsed from `url` by the constructor, so the host the
/// allowlist checks is the host the transport connects to.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CloneTarget {
    provider: ProviderType,
    url: String,
    host: String,
    repo_identifier: String,
    branch: String,
    auth_method: &'static str,
    authorization: Option<Secret>,
}

impl CloneTarget {
    /// A target for `url`, which must be `http(s)://authority/path` with no
    /// userinfo. Derivation always builds `https`; `http` exists for tests
    /// against a loopback server.
    ///
    /// # Errors
    ///
    /// A `ValueError` for a URL with userinfo, another scheme, or no host.
    pub fn new(
        provider: ProviderType,
        url: String,
        repo_identifier: String,
        branch: String,
        authorization: Option<Secret>,
        auth_method: &'static str,
    ) -> Result<Self, EngineError> {
        let parts = split_url(&url)
            .filter(|parts| matches!(parts.scheme, "http" | "https"))
            .ok_or_else(|| value_error("A clone URL must be http(s)://host/path".to_owned()))?;
        if parts.authority.contains('@') {
            return Err(value_error(
                "A clone URL must not carry credentials".to_owned(),
            ));
        }
        let host = validated_authority(parts.authority)
            .ok_or_else(|| value_error("A clone URL must name a usable host".to_owned()))?;
        Ok(Self {
            provider,
            url,
            host,
            repo_identifier,
            branch,
            auth_method,
            authorization,
        })
    }

    #[must_use]
    pub fn provider(&self) -> ProviderType {
        self.provider
    }

    /// The credential-free clone URL.
    #[must_use]
    pub fn url(&self) -> &str {
        &self.url
    }

    /// `host[:port]`, lower-cased.
    #[must_use]
    pub fn host(&self) -> &str {
        &self.host
    }

    /// `GitCloneConfig.repo_identifier`: `owner/repo`, `group/sub/project`,
    /// `PROJECT/repo`, `org/project/repo`.
    #[must_use]
    pub fn repo_identifier(&self) -> &str {
        &self.repo_identifier
    }

    #[must_use]
    pub fn branch(&self) -> &str {
        &self.branch
    }

    /// `token`, `password` or `anonymous`.
    #[must_use]
    pub fn auth_method(&self) -> &'static str {
        self.auth_method
    }

    /// The `Authorization` header value, when the target authenticates.
    #[must_use]
    pub fn authorization(&self) -> Option<&Secret> {
        self.authorization.as_ref()
    }
}

/// `ProviderConfig` after `_normalize_config`.
struct Normalised {
    api_url: String,
    repository: String,
    branch: String,
    token: Option<Secret>,
    username: Option<String>,
    password: Option<Secret>,
    project: Option<String>,
}

fn value_error(message: String) -> EngineError {
    EngineError::new(ErrorType::Value, message)
}

/// Derive the clone target from a `repo_config`.
///
/// # Errors
///
/// A `ValueError` for an unknown provider, a missing repository, an
/// unusable host or path, or an Azure DevOps configuration without an
/// organisation or a project — the cases Python raised for, plus the
/// refusals listed in the module documentation.
pub fn clone_target(repo_config: &Value) -> Result<CloneTarget, EngineError> {
    let empty = Map::new();
    let config = repo_config.as_object().unwrap_or(&empty);
    let provider_name = match config.get("provider_type") {
        Some(value) if py_truthy(value) => py_str(value),
        _ => "github".to_owned(),
    };
    let provider = ProviderType::from_name(&provider_name)?;
    let provider_config = match config.get("provider_config") {
        Some(Value::Object(map)) => map,
        _ => &empty,
    };
    let repository = match config.get("repository") {
        Some(value) if py_truthy(value) => py_str(value),
        _ => {
            return Err(value_error(
                "repository is required in repo_config".to_owned(),
            ));
        }
    };
    let branch = branch_of(config.get("branch"))?;
    let project = text_of(config.get("project"));
    let normalised = normalise(provider, provider_config, repository, branch, project);
    match provider {
        ProviderType::GitHub => github(normalised),
        ProviderType::GitLab => gitlab(normalised),
        ProviderType::Bitbucket => bitbucket(normalised),
        ProviderType::AzureDevOps => azure_devops(normalised),
    }
}

/// A non-empty string form of a JSON value (`str(v)` when truthy).
fn text_of(value: Option<&Value>) -> Option<String> {
    value.filter(|v| py_truthy(v)).map(py_str)
}

/// `get_secret`: `str(val).strip() if val else None`.
fn secret_of(config: &Map<String, Value>, key: &str) -> Option<Secret> {
    text_of(config.get(key)).and_then(|text| Secret::new(text.trim().to_owned()))
}

/// `config.get(key, default)`, a null value counting as absent.
fn url_of(config: &Map<String, Value>, key: &str, default: &str) -> String {
    match config.get(key) {
        None | Some(Value::Null) => default.to_owned(),
        Some(value) => py_str(value),
    }
}

fn branch_of(value: Option<&Value>) -> Result<String, EngineError> {
    let branch = text_of(value)
        .map(|b| b.trim().to_owned())
        .unwrap_or_default();
    if branch.is_empty() {
        return Ok("main".to_owned());
    }
    let valid = !branch.starts_with('-')
        && gix::validate::reference::name_partial(branch.as_str().into()).is_ok();
    if valid {
        Ok(branch)
    } else {
        Err(value_error(format!(
            "The branch {} is not a valid git branch name",
            py_repr(&branch)
        )))
    }
}

/// `RepoProviderFactory._normalize_config`.
fn normalise(
    provider: ProviderType,
    config: &Map<String, Value>,
    repository: String,
    branch: String,
    project: Option<String>,
) -> Normalised {
    let username = text_of(config.get("username")).map(|u| u.trim().to_owned());
    match provider {
        ProviderType::GitHub => Normalised {
            api_url: url_of(config, "base_url", "https://api.github.com"),
            repository,
            branch,
            token: secret_of(config, "access_token"),
            username: username.filter(|u| !u.is_empty()),
            password: secret_of(config, "password"),
            project: None,
        },
        ProviderType::GitLab => Normalised {
            api_url: url_of(config, "url", "https://gitlab.com"),
            repository,
            branch,
            token: secret_of(config, "private_token"),
            username: None,
            password: None,
            project: None,
        },
        ProviderType::Bitbucket => Normalised {
            api_url: url_of(config, "url", "https://bitbucket.org"),
            repository,
            branch,
            token: None,
            username: username.filter(|u| !u.is_empty()),
            password: secret_of(config, "password"),
            project: project.or_else(|| text_of(config.get("project"))),
        },
        ProviderType::AzureDevOps => Normalised {
            api_url: url_of(config, "organization_url", ""),
            repository,
            branch,
            token: secret_of(config, "token"),
            username: None,
            password: None,
            project: project.or_else(|| text_of(config.get("project"))),
        },
    }
}

/// The `Authorization` value for a URL userinfo of `user[:password]`.
fn basic(user: &str, password: Option<&Secret>) -> Option<Secret> {
    let mut pair = String::with_capacity(user.len() + 1 + password.map_or(0, |p| p.expose().len()));
    pair.push_str(user);
    pair.push(':');
    if let Some(password) = password {
        pair.push_str(password.expose());
    }
    // Both buffers hold the credential: zero them, not just free them.
    let pair = zeroize::Zeroizing::new(pair);
    let encoded =
        zeroize::Zeroizing::new(base64::engine::general_purpose::STANDARD.encode(pair.as_bytes()));
    Secret::new(format!("Basic {}", encoded.as_str()))
}

fn github(config: Normalised) -> Result<CloneTarget, EngineError> {
    let repo = github_normalize(&config.repository);
    let host = configured_host(
        "GitHub base_url",
        &config.api_url,
        "github.com",
        Some(("api.github.com", "github.com")),
    )?;
    let (authorization, auth_method) = if let Some(token) = &config.token {
        match &config.username {
            Some(user) => (basic(user, Some(token)), "token"),
            None => (basic(token.expose(), None), "token"),
        }
    } else if let (Some(user), Some(password)) = (&config.username, &config.password) {
        (basic(user, Some(password)), "password")
    } else {
        (None, "anonymous")
    };
    let path = safe_path(&repo)?;
    CloneTarget::new(
        ProviderType::GitHub,
        format!("https://{host}/{path}.git"),
        repo,
        config.branch,
        authorization,
        auth_method,
    )
}

fn gitlab(config: Normalised) -> Result<CloneTarget, EngineError> {
    let repo = gitlab_normalize(&config.repository);
    let host = configured_host("GitLab url", &config.api_url, "gitlab.com", None)?;
    let (authorization, auth_method) = if let Some(token) = &config.token {
        (basic("oauth2", Some(token)), "token")
    } else if let (Some(user), Some(password)) = (&config.username, &config.password) {
        (basic(user, Some(password)), "password")
    } else {
        (None, "anonymous")
    };
    let path = safe_path(&repo)?;
    CloneTarget::new(
        ProviderType::GitLab,
        format!("https://{host}/{path}.git"),
        repo,
        config.branch,
        authorization,
        auth_method,
    )
}

fn bitbucket(config: Normalised) -> Result<CloneTarget, EngineError> {
    let host = configured_host(
        "Bitbucket url",
        &config.api_url,
        "bitbucket.org",
        Some(("api.bitbucket.org", "bitbucket.org")),
    )?;
    let is_cloud = host.contains("bitbucket.org");
    let repo = bitbucket_normalize(&config.repository);
    let repo_path = match (&config.project, is_cloud) {
        (Some(project), false) => format!("scm/{project}/{repo}"),
        // Deliberate: Python dropped `scm/` here (see the module docs).
        (None, false) if repo.contains('/') => format!("scm/{repo}"),
        (Some(project), true) if !repo.contains('/') => format!("{project}/{repo}"),
        _ => repo,
    };
    let (authorization, auth_method) =
        if let (Some(user), Some(password)) = (&config.username, &config.password) {
            (basic(user, Some(password)), "password")
        } else if let Some(token) = &config.token {
            (basic("x-token-auth", Some(token)), "token")
        } else {
            (None, "anonymous")
        };
    let identifier = if is_cloud {
        repo_path.clone()
    } else {
        repo_path.replace("scm/", "")
    };
    let path = safe_path(&repo_path)?;
    CloneTarget::new(
        ProviderType::Bitbucket,
        format!("https://{host}/{path}.git"),
        identifier,
        config.branch,
        authorization,
        auth_method,
    )
}

static ADO_DEV: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"(?i)^https?://dev\.azure\.com/([^/]+)/?").unwrap_or_else(|_| unreachable!())
});
static ADO_VS: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"(?i)^https?://([^/.]+)\.visualstudio\.com/?").unwrap_or_else(|_| unreachable!())
});
static ADO_GIT: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"/_git/([^/?]+)").unwrap_or_else(|_| unreachable!()));

fn azure_devops(config: Normalised) -> Result<CloneTarget, EngineError> {
    let organization_url = config.api_url.trim_end_matches('/');
    let (org, legacy) = if let Some(found) = ADO_DEV.captures(organization_url) {
        (found.get(1).map_or("", |m| m.as_str()), false)
    } else if let Some(found) = ADO_VS.captures(organization_url) {
        (found.get(1).map_or("", |m| m.as_str()), true)
    } else {
        return Err(value_error(format!(
            "Invalid Azure DevOps organization URL: {}",
            config.api_url
        )));
    };
    if org.is_empty()
        || !org
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '-' | '_' | '.'))
    {
        return Err(value_error(format!(
            "Invalid Azure DevOps organization URL: {}",
            config.api_url
        )));
    }
    let Some(project) = config.project.as_deref() else {
        return Err(value_error("Azure DevOps requires project name".to_owned()));
    };
    let repo = ado_normalize(&config.repository);
    if repo.is_empty() {
        return Err(value_error(
            "repository is required in repo_config".to_owned(),
        ));
    }
    let host = if legacy {
        format!("{}.visualstudio.com", org.to_lowercase())
    } else {
        "dev.azure.com".to_owned()
    };
    let base = if legacy {
        format!("https://{host}")
    } else {
        format!("https://{host}/{org}")
    };
    let (authorization, auth_method) = match &config.token {
        Some(token) => (basic(token.expose(), None), "token"),
        None => (None, "anonymous"),
    };
    CloneTarget::new(
        ProviderType::AzureDevOps,
        format!("{base}/{}/_git/{}", quote(project), quote(&repo)),
        format!("{org}/{project}/{repo}"),
        config.branch,
        authorization,
        auth_method,
    )
}

/// Python's `urllib.parse.quote(text, safe='')`.
fn quote(text: &str) -> String {
    let mut out = String::with_capacity(text.len());
    for byte in text.bytes() {
        if byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'.' | b'-' | b'~') {
            out.push(char::from(byte));
        } else {
            const HEX: &[u8; 16] = b"0123456789ABCDEF";
            out.push('%');
            out.push(char::from(HEX[usize::from(byte >> 4)]));
            out.push(char::from(HEX[usize::from(byte & 0x0F)]));
        }
    }
    out
}

/// A repository path that is safe to put into a URL as is.
fn safe_path(path: &str) -> Result<&str, EngineError> {
    let ok = !path.is_empty()
        && path.split('/').all(|segment| {
            !matches!(segment, "" | "." | "..")
                && segment
                    .chars()
                    .all(|c| c.is_ascii_alphanumeric() || matches!(c, '.' | '_' | '~' | '-'))
        });
    if ok {
        Ok(path)
    } else {
        Err(value_error(format!(
            "The repository path {} cannot be cloned: it must be segments of letters, digits, '.', '_', '~' or '-'",
            py_repr(path)
        )))
    }
}

/// The parts of `scheme://authority/path?query#fragment` that matter here.
pub(crate) struct UrlParts<'a> {
    pub scheme: &'a str,
    pub authority: &'a str,
    pub path: &'a str,
}

/// Split a URL the way `urlparse` does for the `scheme://` forms.
pub(crate) fn split_url(text: &str) -> Option<UrlParts<'_>> {
    let (scheme, rest) = text.split_once("://")?;
    if scheme.is_empty()
        || !scheme
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '+' | '-' | '.'))
    {
        return None;
    }
    let authority_end = rest.find(['/', '?', '#']).unwrap_or(rest.len());
    let (authority, tail) = rest.split_at(authority_end);
    let path_end = tail.find(['?', '#']).unwrap_or(tail.len());
    Some(UrlParts {
        scheme,
        authority,
        path: &tail[..path_end],
    })
}

/// `host[:port]` lower-cased, if it is a plausible network authority:
/// a DNS-ish name or a bracketed IPv6 literal, and an optional numeric
/// port. Anything else (spaces, `@`, `%`, a path) is refused.
fn validated_authority(authority: &str) -> Option<String> {
    let lowered = authority.to_lowercase();
    let port = if let Some(rest) = lowered.strip_prefix('[') {
        let (inside, after) = rest.split_once(']')?;
        if inside.is_empty()
            || !inside
                .chars()
                .all(|c| c.is_ascii_hexdigit() || matches!(c, ':' | '.'))
        {
            return None;
        }
        if after.is_empty() {
            None
        } else {
            Some(after.strip_prefix(':')?)
        }
    } else {
        let (host, port) = match lowered.split_once(':') {
            Some((host, port)) => (host, Some(port)),
            None => (lowered.as_str(), None),
        };
        let host_ok = !host.is_empty()
            && host
                .chars()
                .all(|c| c.is_ascii_alphanumeric() || matches!(c, '.' | '-' | '_'));
        if !host_ok {
            return None;
        }
        port
    };
    if let Some(port) = port
        && (port.is_empty() || port.len() > 5 || !port.chars().all(|c| c.is_ascii_digit()))
    {
        return None;
    }
    Some(lowered)
}

/// The host a provider clones from: its configured URL's host, else its
/// public default.
fn configured_host(
    what: &str,
    api_url: &str,
    default_host: &str,
    alias: Option<(&str, &str)>,
) -> Result<String, EngineError> {
    let api_url = api_url.trim();
    if api_url.is_empty() {
        return Ok(default_host.to_owned());
    }
    let refuse = || {
        value_error(format!(
            "The {what} {} is not a URL with a host (expected https://host[/path]), so the clone host cannot be derived from it",
            py_repr(api_url)
        ))
    };
    let parts = split_url(api_url).ok_or_else(refuse)?;
    let authority = parts
        .authority
        .rsplit_once('@')
        .map_or(parts.authority, |(_, host)| host);
    let host = validated_authority(authority).ok_or_else(refuse)?;
    Ok(match alias {
        Some((from, to)) if host == from => to.to_owned(),
        _ => host,
    })
}

static GITHUB_SSH: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"^git@[^:]+:([^/]+/[^/]+?)(?:\.git)?$").unwrap_or_else(|_| unreachable!())
});
static ANY_SSH: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"^git@[^:]+:(.+?)(?:\.git)?$").unwrap_or_else(|_| unreachable!()));

fn is_http_url(text: &str) -> bool {
    text.starts_with("http://") || text.starts_with("https://")
}

/// `urlparse(url).path.strip('/')` without a literal `.git`.
fn url_repo_path(url: &str) -> String {
    let path = split_url(url)
        .map_or("", |parts| parts.path)
        .trim_matches('/');
    strip_git_suffix(path).to_owned()
}

/// `_strip_git_suffix`: one literal `.git`, nothing else.
#[must_use]
pub fn strip_git_suffix(repository: &str) -> &str {
    repository.strip_suffix(".git").unwrap_or(repository)
}

fn ssh_capture<'a>(pattern: &Regex, repo: &'a str) -> Option<&'a str> {
    pattern
        .captures(repo)
        .and_then(|found| found.get(1))
        .map(|m| m.as_str())
}

/// `GitHubProvider.normalize_repository`.
#[must_use]
pub fn github_normalize(repository: &str) -> String {
    let repo = repository.trim();
    if is_http_url(repo) {
        return url_repo_path(repo);
    }
    if repo.starts_with("git@")
        && let Some(path) = ssh_capture(&GITHUB_SSH, repo)
    {
        return path.to_owned();
    }
    strip_git_suffix(repo).trim_matches('/').to_owned()
}

/// `GitLabProvider.normalize_repository`.
#[must_use]
pub fn gitlab_normalize(repository: &str) -> String {
    let repo = repository.trim();
    if is_http_url(repo) {
        let path = url_repo_path(repo);
        return match path.split_once("/-/") {
            Some((head, _)) => head.to_owned(),
            None => path,
        };
    }
    if repo.starts_with("git@")
        && let Some(path) = ssh_capture(&ANY_SSH, repo)
    {
        return path.to_owned();
    }
    strip_git_suffix(repo).trim_matches('/').to_owned()
}

/// `BitbucketProvider.normalize_repository`.
#[must_use]
pub fn bitbucket_normalize(repository: &str) -> String {
    let repo = repository.trim();
    if is_http_url(repo) {
        let path = url_repo_path(repo);
        return path
            .strip_prefix("scm/")
            .map_or(path.clone(), str::to_owned);
    }
    if repo.starts_with("git@")
        && let Some(path) = ssh_capture(&ANY_SSH, repo)
    {
        return path.strip_prefix("scm/").unwrap_or(path).to_owned();
    }
    strip_git_suffix(repo).trim_matches('/').to_owned()
}

/// `AzureDevOpsProvider.normalize_repository`.
#[must_use]
pub fn ado_normalize(repository: &str) -> String {
    let repo = repository.trim();
    if repo.contains("/_git/")
        && let Some(name) = ssh_capture(&ADO_GIT, repo)
    {
        return name.to_owned();
    }
    strip_git_suffix(repo).trim_matches('/').to_owned()
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn target(config: &Value) -> CloneTarget {
        clone_target(config).unwrap_or_else(|e| panic!("{config}: {e}"))
    }

    fn refusal(config: &Value) -> EngineError {
        match clone_target(config) {
            Ok(t) => panic!("{config}: derived {t:?}"),
            Err(e) => e,
        }
    }

    #[test]
    fn a_schemeless_base_url_is_refused_not_sent_to_the_public_host() {
        let config = json!({"provider_type": "github", "provider_config": {"base_url": "ghe.example.com", "access_token": "t"}, "repository": "o/r"});
        let error = refusal(&config);
        assert_eq!(error.error_type, ErrorType::Value);
        assert!(error.message.contains("ghe.example.com"), "{error}");
        let gitlab = json!({"provider_type": "gitlab", "provider_config": {"url": "gitlab.internal"}, "repository": "g/p"});
        assert_eq!(refusal(&gitlab).error_type, ErrorType::Value);
    }

    #[test]
    fn userinfo_in_a_base_url_never_reaches_the_target() {
        let config = json!({"provider_type": "github", "provider_config": {"base_url": "https://u:pw@GHE.example.com/api/v3"}, "repository": "o/r"});
        let found = target(&config);
        assert_eq!(found.host(), "ghe.example.com");
        assert_eq!(found.url(), "https://ghe.example.com/o/r.git");
    }

    #[test]
    fn unsafe_repository_paths_are_refused() {
        for repo in ["o/../r", "o/r?x=1", "o r/x", "o/%2e%2e/r", "./r"] {
            let config = json!({"provider_type": "github", "repository": repo});
            let error = refusal(&config);
            assert_eq!(error.error_type, ErrorType::Value, "{repo}");
        }
    }

    #[test]
    fn branches_are_trimmed_defaulted_and_validated() {
        let base = |branch: Value| json!({"repository": "o/r", "branch": branch});
        assert_eq!(target(&base(Value::Null)).branch(), "main");
        assert_eq!(target(&base(json!("  "))).branch(), "main");
        assert_eq!(target(&base(json!(" dev "))).branch(), "dev");
        assert_eq!(target(&base(json!("feature/x"))).branch(), "feature/x");
        for bad in ["-x", "a..b", "a b", "x.lock", "a~b", "a:b"] {
            assert_eq!(
                refusal(&base(json!(bad))).error_type,
                ErrorType::Value,
                "{bad}"
            );
        }
    }

    #[test]
    fn a_missing_repository_is_a_value_error() {
        let error = refusal(&json!({"provider_type": "github"}));
        assert_eq!(error.error_type, ErrorType::Value);
        assert_eq!(refusal(&json!({})).error_type, ErrorType::Value);
    }

    #[test]
    fn the_go_default_provider_is_github() {
        let found = target(&json!({"provider_type": null, "repository": "o/r"}));
        assert_eq!(found.provider(), ProviderType::GitHub);
    }

    #[test]
    fn bitbucket_server_keeps_its_scm_prefix_without_a_project() {
        let config = json!({"provider_type": "bitbucket", "provider_config": {"url": "https://bitbucket.example.com/"}, "repository": "https://bitbucket.example.com/scm/PROJ/repo.git"});
        let found = target(&config);
        assert_eq!(
            found.url(),
            "https://bitbucket.example.com/scm/PROJ/repo.git"
        );
        assert_eq!(found.repo_identifier(), "PROJ/repo");
    }

    #[test]
    fn an_ado_organisation_must_be_a_plain_name() {
        let config = json!({"provider_type": "ado", "provider_config": {"organization_url": "https://dev.azure.com/evil@host:1"}, "repository": "r", "project": "P"});
        assert_eq!(refusal(&config).error_type, ErrorType::Value);
    }

    #[test]
    fn a_target_url_with_credentials_is_refused() {
        let made = CloneTarget::new(
            ProviderType::GitHub,
            "https://tok@github.com/o/r.git".to_owned(),
            "o/r".to_owned(),
            "main".to_owned(),
            None,
            "anonymous",
        );
        assert!(made.is_err());
        let ssh = CloneTarget::new(
            ProviderType::GitHub,
            "ssh://github.com/o/r.git".to_owned(),
            "o/r".to_owned(),
            "main".to_owned(),
            None,
            "anonymous",
        );
        assert!(ssh.is_err());
    }

    #[test]
    fn the_debug_form_of_a_target_redacts_the_header() {
        let config = json!({"provider_type": "github", "provider_config": {"access_token": "ghp_verysecret"}, "repository": "o/r"});
        let found = target(&config);
        let shown = format!("{found:?}");
        assert!(!shown.contains("ghp_verysecret"), "{shown}");
        let encoded = base64::engine::general_purpose::STANDARD.encode("ghp_verysecret:");
        assert!(!shown.contains(&encoded), "{shown}");
    }

    #[test]
    fn quote_is_pythons() {
        assert_eq!(quote("My Project/ü~"), "My%20Project%2F%C3%BC~");
    }
}
