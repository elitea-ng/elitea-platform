//! The GitHub REST client the `github` family and the GitHub connector
//! share (moved from agent-runtime, ADR-0030 decision 3): the origin-bound
//! request builder, token / Basic / GitHub App (RS256 JWT) authentication,
//! the bounded JSON reader and the status mapping.

use std::fmt;
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use base64::Engine as _;
use base64::engine::general_purpose::{STANDARD, URL_SAFE_NO_PAD};
use ring::rand::SystemRandom;
use ring::signature::{RSA_PKCS1_SHA256, RsaKeyPair};
use serde_json::{Value, json};
use zeroize::Zeroizing;

use super::config::{GitHubAuthKind, GitHubToolkitConfig};
use crate::git_id::valid_git_object_id;
use crate::reqwest_adapter::ClientPolicy;
use crate::transport::header::{
    ACCEPT, AUTHORIZATION, CONTENT_TYPE, HeaderMap, HeaderName, HeaderValue, RETRY_AFTER,
};
use crate::transport::{Method, Request, StatusCode, Transport, TransportError, Url};

const CONNECT_TIMEOUT: Duration = Duration::from_secs(10);
/// The default per-request timeout.
pub const REQUEST_TIMEOUT: Duration = Duration::from_secs(30);
const PROBE_TIMEOUT: Duration = Duration::from_secs(10);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_secs(30);
const MAX_IDLE_PER_HOST: usize = 8;
/// One ordinary JSON response.
pub const MAX_RESPONSE_BYTES: usize = 512 * 1_024;
const GITHUB_ACCEPT: &str = "application/vnd.github+json";
const GITHUB_API_VERSION: &str = "2022-11-28";
const USER_AGENT: &str = "elitea-worker-rust/0.1";

/// The client the family builds: HTTPS only, no redirect, these timeouts.
/// reqwest's own protocol-level retries stay at their default, as the
/// family always had them.
pub const CLIENT_POLICY: ClientPolicy = ClientPolicy {
    connect_timeout: CONNECT_TIMEOUT,
    request_timeout: REQUEST_TIMEOUT,
    pool_idle_timeout: POOL_IDLE_TIMEOUT,
    max_idle_per_host: MAX_IDLE_PER_HOST,
    user_agent: USER_AGENT,
    disable_retries: false,
};

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
    /// The host is not on the egress allowlist; nothing was sent.
    EgressRefused,
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
            GitHubClientErrorCode::EgressRefused => {
                "the GitHub host is not on the egress allowlist"
            }
        })
    }
}

impl std::error::Error for GitHubClientError {}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum GitHubRequestKind {
    Probe,
    AuthenticatedUser,
    Repository,
}

/// One invocation-scoped, pooled and origin-bound GitHub REST client.
///
/// The host's transport is pooled across calls from the same toolkit but is
/// never placed in a process-global credential registry. Redirects are
/// disabled and request paths are appended to the admitted base URL, so tool
/// arguments cannot select another origin.
pub struct GitHubRest {
    transport: Arc<dyn Transport>,
    config: GitHubToolkitConfig,
}

impl GitHubRest {
    /// One client over the host's transport (built with [`CLIENT_POLICY`]).
    /// A GitHub App key is parsed now, so a bad key fails here.
    pub fn new(
        config: GitHubToolkitConfig,
        transport: Arc<dyn Transport>,
    ) -> Result<Self, GitHubClientError> {
        if config.auth_kind() == GitHubAuthKind::App {
            let (_, key) = config.auth().app().ok_or_else(invalid_configuration)?;
            let _ = parse_rsa_key(key)?;
        }
        Ok(Self { transport, config })
    }

    pub fn config(&self) -> &GitHubToolkitConfig {
        &self.config
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
            .transport
            .execute(request)
            .await
            .map_err(map_transport_error)?;
        map_status(response.status(), response.headers())
    }

    pub fn build_request_at(
        &self,
        kind: GitHubRequestKind,
        path: &[&str],
        query: &[(&str, String)],
        timeout: Duration,
        now: SystemTime,
    ) -> Result<Request, GitHubClientError> {
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
        let mut request = Request::new(Method::GET, endpoint);
        let headers = request.headers_mut();
        headers.insert(ACCEPT, HeaderValue::from_static(GITHUB_ACCEPT));
        headers.insert(
            HeaderName::from_static("x-github-api-version"),
            HeaderValue::from_static(GITHUB_API_VERSION),
        );
        if let Some(authorization) = self.authorization(kind, now)? {
            headers.insert(AUTHORIZATION, authorization);
        }
        *request.timeout_mut() = Some(timeout);
        Ok(request)
    }

    pub fn endpoint(&self, path: &[&str]) -> Result<Url, GitHubClientError> {
        let mut endpoint = self.config.base_url().clone();
        endpoint
            .path_segments_mut()
            .map_err(|()| invalid_configuration())?
            .pop_if_empty()
            .extend(path.iter().copied());
        Ok(endpoint)
    }

    pub fn graphql_endpoint(&self) -> Result<Url, GitHubClientError> {
        let mut endpoint = self.config.base_url().clone();
        match endpoint.path().trim_end_matches('/') {
            "" => endpoint.set_path("/graphql"),
            "/api/v3" => endpoint.set_path("/api/graphql"),
            _ => return Err(invalid_configuration()),
        }
        Ok(endpoint)
    }

    pub fn build_graphql_request_at(
        &self,
        payload: &Value,
        now: SystemTime,
    ) -> Result<Request, GitHubClientError> {
        let body = serde_json::to_vec(payload).map_err(|_| invalid_configuration())?;
        let mut request = Request::new(Method::POST, self.graphql_endpoint()?);
        let headers = request.headers_mut();
        headers.insert(ACCEPT, HeaderValue::from_static("application/json"));
        headers.insert(CONTENT_TYPE, HeaderValue::from_static("application/json"));
        if let Some(authorization) = self.authorization(GitHubRequestKind::Repository, now)? {
            headers.insert(AUTHORIZATION, authorization);
        }
        *request.timeout_mut() = Some(REQUEST_TIMEOUT);
        *request.body_mut() = Some(body.into());
        Ok(request)
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

    pub async fn get_json(
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

    pub async fn post_graphql_json(
        &self,
        payload: &Value,
        max_response_bytes: usize,
    ) -> Result<Value, GitHubClientError> {
        let request = self.build_graphql_request_at(payload, SystemTime::now())?;
        self.execute_json_request(request, max_response_bytes).await
    }

    pub async fn execute_json_request(
        &self,
        request: Request,
        max_response_bytes: usize,
    ) -> Result<Value, GitHubClientError> {
        let mut response = self
            .transport
            .execute(request)
            .await
            .map_err(map_transport_error)?;
        map_status(response.status(), response.headers())?;
        if response.declares_more_than(max_response_bytes) {
            return Err(resource_exhausted());
        }
        let body = response
            .bytes_within(max_response_bytes)
            .await
            .map_err(map_transport_error)?
            .ok_or_else(resource_exhausted)?;
        serde_json::from_slice(&body).map_err(|_| invalid_response())
    }

    pub async fn resolve_tree_sha(
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
        let (response, from_branch) = match branch {
            Ok(response) => (response, true),
            Err(error) if error.code() == GitHubClientErrorCode::NotFound => (
                self.get_json(
                    GitHubRequestKind::Repository,
                    &["repos", owner, repository, "commits", reference],
                    &[],
                    MAX_RESPONSE_BYTES,
                )
                .await?,
                false,
            ),
            Err(error) => return Err(error),
        };
        if from_branch {
            project_tree_sha(&response)
        } else {
            project_commit_tree_sha(&response)
        }
    }
}

/// The tree sha of a `branches/{b}` response (`commit.commit.tree.sha`: the
/// branch object nests the commit object once).
pub fn project_tree_sha(value: &Value) -> Result<String, GitHubClientError> {
    tree_sha_of(value.get("commit").and_then(|commit| commit.get("commit")))
}

/// The tree sha of a `commits/{ref}` response (`commit.tree.sha`: the answer
/// is the commit object itself), which is how a tag or a sha resolves.
pub fn project_commit_tree_sha(value: &Value) -> Result<String, GitHubClientError> {
    tree_sha_of(value.get("commit"))
}

fn tree_sha_of(commit: Option<&Value>) -> Result<String, GitHubClientError> {
    commit
        .and_then(|commit| commit.get("tree"))
        .and_then(|tree| tree.get("sha"))
        .and_then(Value::as_str)
        .filter(|sha| valid_git_object_id(sha))
        .map(str::to_ascii_lowercase)
        .ok_or_else(invalid_response)
}

/// `owner/repository`, each segment GitHub's characters.
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

fn valid_repository_segment(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 256
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.'))
}

pub fn map_status(status: StatusCode, headers: &HeaderMap) -> Result<(), GitHubClientError> {
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

fn map_transport_error(source: TransportError) -> GitHubClientError {
    if source.is_refused() {
        return error_code(GitHubClientErrorCode::EgressRefused);
    }
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
    let mut value = HeaderValue::from_str(&value).map_err(|_| invalid_configuration())?;
    value.set_sensitive(true);
    Ok(value)
}

#[must_use]
pub const fn error(code: GitHubClientErrorCode) -> GitHubClientError {
    GitHubClientError { code }
}

#[must_use]
pub const fn error_code(code: GitHubClientErrorCode) -> GitHubClientError {
    error(code)
}

#[must_use]
pub const fn invalid_configuration() -> GitHubClientError {
    error(GitHubClientErrorCode::InvalidConfiguration)
}

#[must_use]
pub const fn invalid_input() -> GitHubClientError {
    error(GitHubClientErrorCode::InvalidInput)
}

#[must_use]
pub const fn unsupported_authentication() -> GitHubClientError {
    error(GitHubClientErrorCode::UnsupportedAuthentication)
}

#[must_use]
pub const fn invalid_response() -> GitHubClientError {
    error(GitHubClientErrorCode::InvalidResponse)
}

#[must_use]
pub const fn resource_exhausted() -> GitHubClientError {
    error(GitHubClientErrorCode::ResourceExhausted)
}
