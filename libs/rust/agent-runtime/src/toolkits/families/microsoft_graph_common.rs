//! What the Microsoft delegated toolkit families share (ADR-0057 decision 7):
//! Outlook, Teams, Microsoft Graph and Dataverse, and `SharePoint` for the
//! pieces it already had.
//!
//! * **Token lookup.** A run's delegated access token rides in the
//!   `delegated_tokens` map of the claim, keyed by credential id and
//!   authority. The worker receives an access token only: it never holds a
//!   refresh token or a client secret (Main withholds both from the claim,
//!   `microsoftauth.WithholdFromWorker`) and it calls no token endpoint.
//! * **The interrupt.** [`authorization_requirement`] builds the
//!   [`DelegatedAuthorizationRequirement`] a family raises when it has no
//!   token, or when Graph rejects the one it has.
//! * **The client.** [`GraphClient`] sends a request as the signed-in person
//!   to one endpoint ([`Endpoint::graph`] or [`Endpoint::dataverse`]) and
//!   nowhere else. A 401 asks Main for another token once through a
//!   [`TokenRefresher`] and retries once, a 429 or 503 waits for
//!   `Retry-After` (at most [`MAX_RETRY_AFTER`]) and retries, paging follows
//!   `@odata.nextLink` only on the same host, and `$batch` is bounded to 20
//!   requests. A redirect to another host is refused: a download from another
//!   Microsoft host is not followed (decision 10).
//! * **The request rules.** [`RequestRules`] narrows which methods and path
//!   prefixes a toolkit may send, as upstream's `ms_graph/rules.py` does.
//!
//! Ported from `elitea_sdk/tools/ms_graph/{graph_client,rules}.py` and the
//! Outlook and Teams `graph_wrapper.py` of `EliteaAI/elitea-sdk` main. Where
//! upstream refreshes the token itself with a stored refresh token, this
//! client asks Main, which redeems the same sources again.

use std::collections::BTreeMap;
use std::fmt;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use percent_encoding::percent_decode_str;
use reqwest::header::{
    AUTHORIZATION, CONTENT_LENGTH, CONTENT_TYPE, HeaderMap, HeaderName, HeaderValue, LOCATION,
    RETRY_AFTER,
};
use reqwest::{Method, Request, StatusCode, Url};
use serde_json::{Map, Value, json};
use zeroize::Zeroizing;

use crate::egress::BuildForRun as _;
use crate::toolkits::DelegatedAuthorizationRequirement;
use crate::toolkits::delegated_auth::delegated_authorization_error;

/// The one host a Graph token is sent to.
pub(crate) const GRAPH_HOST: &str = "graph.microsoft.com";
/// Pinned against the egress table by `TestImplicitHostsMatchTheRustClients`.
pub(crate) const GRAPH_ORIGIN: &str = "https://graph.microsoft.com";

const MAX_TOKEN_BYTES: usize = 64 * 1_024;
const MAX_URL_BYTES: usize = 8 * 1_024;
const MAX_LINK_BYTES: usize = 16 * 1_024;
const MAX_ERROR_TEXT: usize = 500;
const MAX_RETRIES: u32 = 3;
/// The longest a `Retry-After` is honoured, as upstream's `_MAX_RETRY_AFTER`.
pub(crate) const MAX_RETRY_AFTER: Duration = Duration::from_secs(30);
const MAX_REDIRECTS: u32 = 5;
const MAX_PAGES: usize = 100;
const MAX_BATCH: usize = 20;
const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(60);
const DEFAULT_MAX_RESPONSE_BYTES: usize = 50 * 1_024 * 1_024;
const USER_AGENT_VALUE: &str = "elitea-worker-rust/0.1";

/// Headers the caller may not set: authentication and transport are the
/// client's job.
const RESERVED_HEADERS: [&str; 5] = [
    "authorization",
    "host",
    "content-length",
    "transfer-encoding",
    "cookie",
];
/// Response headers worth showing to the model.
const RESPONSE_HEADERS: [&str; 6] = [
    "content-type",
    "location",
    "request-id",
    "retry-after",
    "etag",
    "odata-version",
];

// ---------------------------------------------------------------------------
// Token lookup and the interrupt
// ---------------------------------------------------------------------------

/// Why a claim's token entry could not be used.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum TokenLookupError {
    /// The entry is not a token, or not a usable header value.
    Invalid,
    /// The entry is over its approved size.
    TooLarge,
}

/// Append `key` unless it is already listed, keeping the first position.
pub(crate) fn push_unique_key(keys: &mut Vec<String>, key: String) {
    if !keys.contains(&key) {
        keys.push(key);
    }
}

/// The claim keys a Graph credential's token is looked up under, most
/// specific first: the credential id and the authority as configured, the
/// authority alone, then both again in canonical spelling.
#[must_use]
pub(crate) fn graph_token_keys(
    configuration_uuid: Option<&str>,
    configured_authority: &str,
    canonical_authority: &str,
) -> Vec<String> {
    let mut keys = Vec::with_capacity(4);
    for authority in [configured_authority, canonical_authority] {
        if let Some(uuid) = configuration_uuid {
            push_unique_key(&mut keys, format!("{uuid}:{authority}"));
        }
        push_unique_key(&mut keys, authority.to_owned());
    }
    keys
}

/// The first of `keys` the claim's token map holds, as an access token. The
/// entry is a string or an object with `access_token` (or `token`). Nothing
/// but an access token is ever read from it.
pub(crate) fn lookup_access_token(
    tokens: &Map<String, Value>,
    keys: &[String],
) -> Result<Option<Zeroizing<String>>, TokenLookupError> {
    let Some(value) = keys.iter().find_map(|key| tokens.get(key)) else {
        return Ok(None);
    };
    let token = match value {
        Value::String(token) => token.as_str(),
        Value::Object(object) => object
            .get("access_token")
            .or_else(|| object.get("token"))
            .and_then(Value::as_str)
            .ok_or(TokenLookupError::Invalid)?,
        _ => return Err(TokenLookupError::Invalid),
    };
    if token.len() > MAX_TOKEN_BYTES {
        return Err(TokenLookupError::TooLarge);
    }
    if token.trim().is_empty() || token.chars().any(char::is_control) {
        return Err(TokenLookupError::Invalid);
    }
    HeaderValue::from_str(token).map_err(|_| TokenLookupError::Invalid)?;
    Ok(Some(Zeroizing::new(token.to_owned())))
}

/// Why a requirement could not be built.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum RequirementError {
    Invalid,
    TooLarge,
}

/// The requirement a family raises when its credential has no usable token:
/// the browser opens the Entra sign-in named by `discovery_key` (the
/// canonical authority, `https://login.microsoftonline.com/<tenant>`) for the
/// credential's app. Only the client id and whether a secret exists are
/// carried; the secret itself never is.
pub(crate) fn authorization_requirement(
    toolkit_name: &str,
    toolkit_type: &str,
    resource_key: String,
    discovery_key: &str,
    configuration: &Map<String, Value>,
) -> Result<DelegatedAuthorizationRequirement, RequirementError> {
    let resource_metadata_url = format!("{discovery_key}/v2.0/.well-known/openid-configuration");
    let authorization_url = format!("{discovery_key}/oauth2/v2.0/authorize");
    if resource_metadata_url.len() > MAX_URL_BYTES || authorization_url.len() > MAX_URL_BYTES {
        return Err(RequirementError::TooLarge);
    }
    DelegatedAuthorizationRequirement::new(
        toolkit_name.to_owned(),
        toolkit_type.to_owned(),
        resource_key,
        Some(resource_metadata_url.clone()),
        Some(format!(
            "Bearer error=\"unauthorized_client\", resource_metadata=\"{resource_metadata_url}\", authorization_uri=\"{authorization_url}\""
        )),
    )
    .and_then(|requirement| requirement.with_configured_oauth(discovery_key, configuration))
    .ok_or(RequirementError::Invalid)
}

/// Another access token for the credential, from Main.
///
/// A run that outlives its token asks Main over the existing worker-to-Main
/// broker call (no new internal REST hop); Main redeems the same sources the
/// claim used. The worker holds nothing that could mint a
/// token itself. `None` means Main has none to give.
#[async_trait]
pub(crate) trait TokenRefresher: Send + Sync {
    async fn refresh(&self) -> Option<Zeroizing<String>>;
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum GraphErrorKind {
    /// The request itself is wrong (a path, a rule, an argument).
    InvalidInput,
    /// A host or a redirect the client will not send the token to.
    Refused,
    /// 401 after the one refresh: the person must sign in again.
    AuthorizationRequired,
    /// 403: a permission is missing.
    Forbidden,
    NotFound,
    /// 409 or 412.
    Conflict,
    /// 429 after the retries.
    Throttled,
    /// 5xx, or 503 after the retries.
    Unavailable,
    Timeout,
    /// The request may have reached Graph and the answer was lost.
    ConnectionLost,
    InvalidResponse,
    ResourceExhausted,
}

/// A failed Graph call. The message names Graph's own error code and text
/// (bounded, control characters removed) and never a token.
#[derive(Clone, Debug)]
pub(crate) struct GraphError {
    kind: GraphErrorKind,
    status: Option<u16>,
    detail: String,
    hint: String,
    retry_after: Option<Duration>,
}

impl GraphError {
    #[must_use]
    pub(crate) fn new(kind: GraphErrorKind, detail: impl Into<String>) -> Self {
        Self {
            kind,
            status: None,
            detail: detail.into(),
            hint: String::new(),
            retry_after: None,
        }
    }

    #[must_use]
    pub(crate) const fn kind(&self) -> GraphErrorKind {
        self.kind
    }

    #[must_use]
    pub(crate) const fn status(&self) -> Option<u16> {
        self.status
    }

    #[must_use]
    pub(crate) const fn retry_after(&self) -> Option<Duration> {
        self.retry_after
    }

    #[must_use]
    pub(crate) fn hint(&self) -> &str {
        &self.hint
    }

    #[must_use]
    pub(crate) const fn retryable(&self) -> bool {
        matches!(
            self.kind,
            GraphErrorKind::Throttled
                | GraphErrorKind::Unavailable
                | GraphErrorKind::Timeout
                | GraphErrorKind::ConnectionLost
        )
    }

    /// Replace the hint of a 403 with the permission the tool needs, as the
    /// Outlook and Teams wrappers do (`missing_permission_hint`).
    #[must_use]
    pub(crate) fn with_permission(mut self, needed: Option<&str>) -> Self {
        if self.kind == GraphErrorKind::Forbidden {
            self.hint = missing_permission_hint(needed);
        }
        self
    }

    /// Replace the hint of a 404 (the toolkit knows which tool to look the id
    /// up with).
    #[must_use]
    pub(crate) fn with_not_found_hint(mut self, hint: &str) -> Self {
        if self.kind == GraphErrorKind::NotFound {
            hint.clone_into(&mut self.hint);
        }
        self
    }

    /// The error as the tool failure the model sees. An authorization failure
    /// with a `requirement` is the interrupt the client turns into a sign-in.
    #[must_use]
    pub(crate) fn into_adk(
        self,
        requirement: Option<&DelegatedAuthorizationRequirement>,
    ) -> AdkError {
        if self.kind == GraphErrorKind::AuthorizationRequired
            && let Some(requirement) = requirement
        {
            return delegated_authorization_error(requirement);
        }
        let (category, code) = match self.kind {
            GraphErrorKind::InvalidInput => {
                (ErrorCategory::InvalidInput, "microsoft_graph.invalid_input")
            }
            GraphErrorKind::Refused => (ErrorCategory::InvalidInput, "microsoft_graph.refused"),
            GraphErrorKind::AuthorizationRequired => (
                ErrorCategory::Unauthorized,
                "microsoft_graph.authorization_required",
            ),
            GraphErrorKind::Forbidden => (ErrorCategory::Forbidden, "microsoft_graph.forbidden"),
            GraphErrorKind::NotFound => (ErrorCategory::NotFound, "microsoft_graph.not_found"),
            GraphErrorKind::Conflict => (ErrorCategory::InvalidInput, "microsoft_graph.conflict"),
            GraphErrorKind::Throttled => {
                (ErrorCategory::RateLimited, "microsoft_graph.rate_limited")
            }
            GraphErrorKind::Unavailable => {
                (ErrorCategory::Unavailable, "microsoft_graph.unavailable")
            }
            GraphErrorKind::Timeout => (ErrorCategory::Timeout, "microsoft_graph.timeout"),
            GraphErrorKind::ConnectionLost => (
                ErrorCategory::Unavailable,
                "microsoft_graph.connection_lost",
            ),
            GraphErrorKind::InvalidResponse => {
                (ErrorCategory::Internal, "microsoft_graph.invalid_response")
            }
            GraphErrorKind::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "microsoft_graph.resource_exhausted",
            ),
        };
        let retry = RetryHint {
            should_retry: self.retryable(),
            retry_after_ms: self
                .retry_after
                .and_then(|delay| u64::try_from(delay.as_millis()).ok()),
            max_attempts: None,
        };
        AdkError::new(ErrorComponent::Tool, category, code, self.to_string()).with_retry(retry)
    }
}

impl fmt::Display for GraphError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        if self.hint.is_empty() {
            formatter.write_str(&self.detail)
        } else {
            write!(
                formatter,
                "{}.{}",
                self.detail.trim_end_matches('.'),
                self.hint
            )
        }
    }
}

impl std::error::Error for GraphError {}

/// The 403 hint naming the permission a tool needs.
#[must_use]
pub(crate) fn missing_permission_hint(needed: Option<&str>) -> String {
    match needed {
        None => " The signed-in account lacks the Microsoft Graph permission this tool needs: tick it in the credential scopes and sign in again (an Entra admin may need to grant it).".to_owned(),
        Some(needed) => format!(
            " This tool needs the Microsoft Graph permission {needed}: tick it in the credential scopes and sign in again (an Entra admin may need to grant it)."
        ),
    }
}

fn invalid_input(detail: &str) -> GraphError {
    GraphError::new(GraphErrorKind::InvalidInput, detail)
}

fn refused(detail: &str) -> GraphError {
    GraphError::new(GraphErrorKind::Refused, detail)
}

fn invalid_response() -> GraphError {
    GraphError::new(
        GraphErrorKind::InvalidResponse,
        "Microsoft Graph returned a response this client cannot read",
    )
}

fn clean_text(text: &str) -> String {
    let mut cleaned: String = text.chars().filter(|c| !c.is_control()).collect();
    if cleaned.len() > MAX_ERROR_TEXT {
        let mut end = MAX_ERROR_TEXT;
        while !cleaned.is_char_boundary(end) {
            end -= 1;
        }
        cleaned.truncate(end);
    }
    cleaned.trim().to_owned()
}

/// Graph's `error.code` and `error.message`, as upstream reads them.
fn graph_error_text(body: &[u8]) -> String {
    let parsed: Option<Value> = serde_json::from_slice(body).ok();
    let error = parsed.as_ref().and_then(|value| value.get("error"));
    let field = |name: &str| {
        error
            .and_then(|error| error.get(name))
            .and_then(Value::as_str)
            .map(clean_text)
            .filter(|text| !text.is_empty())
    };
    match (field("code"), field("message")) {
        (Some(code), Some(message)) => format!("{code}: {message}"),
        (None, Some(text)) | (Some(text), None) => text,
        (None, None) => "no details".to_owned(),
    }
}

fn error_from_status(status: StatusCode, body: &[u8]) -> GraphError {
    let code = status.as_u16();
    let text = graph_error_text(body);
    let (kind, hint) = match code {
        400 => (GraphErrorKind::InvalidInput, String::new()),
        401 => (GraphErrorKind::AuthorizationRequired, String::new()),
        403 => (GraphErrorKind::Forbidden, missing_permission_hint(None)),
        404 => (
            GraphErrorKind::NotFound,
            " The resource does not exist or is not visible to the signed-in user; do not retry with the same ID.".to_owned(),
        ),
        409 | 412 => (GraphErrorKind::Conflict, String::new()),
        429 => (GraphErrorKind::Throttled, String::new()),
        500..=599 => (GraphErrorKind::Unavailable, String::new()),
        _ => (GraphErrorKind::InvalidInput, String::new()),
    };
    let detail = if kind == GraphErrorKind::AuthorizationRequired {
        format!("Microsoft Graph access token is invalid or expired: {text}. Please re-authorize.")
    } else {
        format!("Microsoft Graph returned HTTP {code}: {text}")
    };
    GraphError {
        kind,
        status: Some(code),
        detail,
        hint,
        retry_after: None,
    }
}

// ---------------------------------------------------------------------------
// Endpoints and path rules
// ---------------------------------------------------------------------------

/// The one origin a client sends its token to.
#[derive(Clone, Debug)]
pub(crate) struct Endpoint {
    host: Box<str>,
    prefixes: &'static [&'static str],
}

impl Endpoint {
    /// `https://graph.microsoft.com`, API versions `v1.0` and `beta`.
    #[must_use]
    pub(crate) fn graph() -> Self {
        Self {
            host: GRAPH_HOST.into(),
            prefixes: &["/v1.0/", "/beta/"],
        }
    }

    /// A Dataverse environment's own origin, from the credential's `url`:
    /// https, port 443, no path, no user info, a `.dynamics.com` host with a
    /// label before it. Sovereign clouds (`.dynamics.cn`,
    /// `.microsoftdynamics.us`) are refused, as ADR-0057 decision 10 says.
    pub(crate) fn dataverse(org_url: &str) -> Result<Self, GraphError> {
        let not_an_org = || {
            refused(
                "the Dataverse URL must look like https://<org>.crm.dynamics.com; sovereign clouds are not supported",
            )
        };
        let url = Url::parse(org_url.trim().trim_end_matches('/')).map_err(|_| not_an_org())?;
        let host = url.host_str().ok_or_else(not_an_org)?;
        if url.scheme() != "https"
            || url.port().is_some()
            || !url.username().is_empty()
            || url.password().is_some()
            || url.path() != "/"
            || url.query().is_some()
            || url.fragment().is_some()
            || !host.ends_with(".dynamics.com")
            || host.len() == ".dynamics.com".len()
            || host.contains("..")
        {
            return Err(not_an_org());
        }
        Ok(Self {
            host: host.into(),
            prefixes: &["/api/data/"],
        })
    }

    #[must_use]
    pub(crate) fn host(&self) -> &str {
        &self.host
    }

    /// Whether `url` is an address this endpoint's token may be sent to.
    #[must_use]
    pub(crate) fn admits(&self, url: &Url) -> bool {
        url.scheme() == "https"
            && url.host_str() == Some(&*self.host)
            && url.port().is_none()
            && url.username().is_empty()
            && url.password().is_none()
            && url.fragment().is_none()
            && self
                .prefixes
                .iter()
                .any(|prefix| url.path().starts_with(prefix))
    }
}

fn collapse_slashes(text: &str) -> String {
    let mut out = String::with_capacity(text.len());
    let mut previous_slash = false;
    for c in text.chars() {
        if c == '/' {
            if !previous_slash {
                out.push(c);
            }
            previous_slash = true;
        } else {
            out.push(c);
            previous_slash = false;
        }
    }
    out
}

/// The API version (if the path names one) and the path without host, version
/// and query, for a relative path or a Graph URL. A URL on any other host is
/// refused: the token is sent only to Microsoft Graph.
pub(crate) fn split_graph_url(path: &str) -> Result<(Option<&'static str>, String), GraphError> {
    let text = path.trim();
    if text.is_empty() {
        return Err(invalid_input("path is required, e.g. /me/messages"));
    }
    let mut text = if text.contains("://") || text.starts_with("//") {
        let url = Url::parse(text).ok().filter(|url| {
            url.scheme() == "https"
                && url.host_str() == Some(GRAPH_HOST)
                && url.username().is_empty()
                && url.port().is_none()
        });
        let Some(url) = url else {
            return Err(refused(&format!("Only {GRAPH_ORIGIN} URLs are allowed")));
        };
        url.path().to_owned()
    } else {
        text.split(['?', '#']).next().unwrap_or_default().to_owned()
    };
    if !text.starts_with('/') {
        text.insert(0, '/');
    }
    let text = collapse_slashes(&text);
    let (head, rest) = text[1..].split_once('/').unwrap_or((&text[1..], ""));
    let version = ["v1.0", "beta"]
        .into_iter()
        .find(|version| head.eq_ignore_ascii_case(version));
    match version {
        Some(version) => Ok((Some(version), format!("/{rest}"))),
        None => Ok((None, text)),
    }
}

fn decode(text: &str) -> String {
    percent_decode_str(text).decode_utf8_lossy().into_owned()
}

/// Canonical path for rule matching: decoded once, lowercase, no trailing
/// slash, no dot segments.
pub(crate) fn normalize_path(path: &str) -> Result<String, GraphError> {
    let (_, text) = split_graph_url(path)?;
    let decoded = decode(&text);
    if decoded.contains('%') && decode(&decoded) != decoded {
        return Err(invalid_input("Double-encoded paths are not allowed"));
    }
    let segments: Vec<&str> = decoded.split('/').filter(|s| !s.is_empty()).collect();
    if segments.iter().any(|s| *s == "." || *s == "..") {
        return Err(invalid_input(
            "Paths with '.' or '..' segments are not allowed",
        ));
    }
    let lowered: Vec<String> = segments.iter().map(|s| s.to_lowercase()).collect();
    Ok(format!("/{}", lowered.join("/")))
}

fn prefix_segments(prefix: &str) -> Result<Vec<String>, GraphError> {
    let text = prefix.trim();
    if text.is_empty() {
        return Ok(Vec::new());
    }
    let (_, text) = split_graph_url(text)?;
    Ok(decode(&text)
        .split('/')
        .filter(|s| !s.is_empty())
        .map(str::to_lowercase)
        .collect())
}

fn prefix_matches(path: &[&str], prefix: &[String]) -> bool {
    !prefix.is_empty()
        && prefix.len() <= path.len()
        && prefix
            .iter()
            .zip(path)
            .all(|(wanted, segment)| wanted == "*" || wanted == segment)
}

const METHODS: [&str; 5] = ["GET", "POST", "PATCH", "PUT", "DELETE"];

/// Allowed methods plus allowed and denied path prefixes of one toolkit.
///
/// A prefix matches whole segments (`/me/messages` matches
/// `/me/messages/123`, not `/me/messagesX`) and `*` matches exactly one
/// segment. Denied prefixes win; an empty allow list allows every path. The
/// rules narrow what the model can call; the token's Graph permissions stay
/// the real limit.
#[derive(Clone, Debug)]
pub(crate) struct RequestRules {
    allowed_methods: Vec<&'static str>,
    allowed_paths: Vec<String>,
    denied_paths: Vec<String>,
    allowed: Vec<Vec<String>>,
    denied: Vec<Vec<String>>,
}

impl RequestRules {
    /// `allowed_methods` of `None` is `["GET"]`.
    pub(crate) fn new(
        allowed_methods: Option<&[String]>,
        allowed_paths: &[String],
        denied_paths: &[String],
    ) -> Result<Self, GraphError> {
        let wanted: Vec<String> = allowed_methods.map_or_else(
            || vec!["GET".to_owned()],
            |methods| methods.iter().map(|m| m.trim().to_uppercase()).collect(),
        );
        let allowed_methods = METHODS
            .into_iter()
            .filter(|method| wanted.iter().any(|w| w == method))
            .collect();
        let clean = |paths: &[String]| -> Vec<String> {
            paths
                .iter()
                .map(|p| p.trim().to_owned())
                .filter(|p| !p.is_empty())
                .collect()
        };
        let allowed_paths = clean(allowed_paths);
        let denied_paths = clean(denied_paths);
        let segments = |paths: &[String]| -> Result<Vec<Vec<String>>, GraphError> {
            let mut out = Vec::with_capacity(paths.len());
            for path in paths {
                let parsed = prefix_segments(path)?;
                if !parsed.is_empty() {
                    out.push(parsed);
                }
            }
            Ok(out)
        };
        Ok(Self {
            allowed: segments(&allowed_paths)?,
            denied: segments(&denied_paths)?,
            allowed_methods,
            allowed_paths,
            denied_paths,
        })
    }

    /// Refuse the request, naming the rule, when it is not allowed.
    pub(crate) fn check(&self, method: &str, path: &str) -> Result<(), GraphError> {
        let method = if method.trim().is_empty() {
            "GET".to_owned()
        } else {
            method.trim().to_uppercase()
        };
        if !METHODS.iter().any(|known| *known == method) {
            return Err(refused(&format!(
                "Unsupported HTTP method {method}; use one of {}",
                METHODS.join(", ")
            )));
        }
        if !self
            .allowed_methods
            .iter()
            .any(|allowed| *allowed == method)
        {
            let allowed = if self.allowed_methods.is_empty() {
                "none".to_owned()
            } else {
                self.allowed_methods.join(", ")
            };
            return Err(refused(&format!(
                "{method} is not allowed by this toolkit (allowed: {allowed})"
            )));
        }
        let normalized = normalize_path(path)?;
        let segments: Vec<&str> = normalized.split('/').filter(|s| !s.is_empty()).collect();
        for (rule, prefix) in self.denied_paths.iter().zip(&self.denied) {
            if prefix_matches(&segments, prefix) {
                return Err(refused(&format!(
                    "Path {path} is denied by this toolkit (rule: {rule})"
                )));
            }
        }
        if !self.allowed.is_empty()
            && !self
                .allowed
                .iter()
                .any(|prefix| prefix_matches(&segments, prefix))
        {
            return Err(refused(&format!(
                "Path {path} is not in the toolkit's allowed paths: {}",
                self.allowed_paths.join(", ")
            )));
        }
        Ok(())
    }

    /// One line for a tool description, so the model does not try requests
    /// that will be refused.
    #[must_use]
    pub(crate) fn describe(&self) -> String {
        let methods = if self.allowed_methods.is_empty() {
            "none".to_owned()
        } else {
            self.allowed_methods.join(", ")
        };
        let mut parts = vec![format!("Allowed methods: {methods}.")];
        if !self.allowed_paths.is_empty() {
            parts.push(format!("Allowed paths: {}.", self.allowed_paths.join(", ")));
        }
        if !self.denied_paths.is_empty() {
            parts.push(format!("Denied paths: {}.", self.denied_paths.join(", ")));
        }
        parts.join(" ")
    }
}

/// `https://graph.microsoft.com/<version><path>?<query>` for a relative path
/// or a Graph URL (whose own version wins).
pub(crate) fn build_graph_url(
    path: &str,
    api_version: &str,
    query: &[(String, String)],
) -> Result<Url, GraphError> {
    let (version, clean) = split_graph_url(path)?;
    let fallback = ["v1.0", "beta"]
        .into_iter()
        .find(|v| *v == api_version)
        .ok_or_else(|| invalid_input("api_version must be v1.0 or beta"))?;
    let mut url = Url::parse(&format!(
        "{GRAPH_ORIGIN}/{}{clean}",
        version.unwrap_or(fallback)
    ))
    .map_err(|_| invalid_input("the path is not a valid Graph path"))?;
    if !query.is_empty() {
        let mut pairs = url.query_pairs_mut();
        for (name, value) in query {
            pairs.append_pair(name, value);
        }
    }
    Ok(url)
}

/// A tool's `query` object as query pairs: strings as they are, numbers and
/// booleans in their JSON text, anything else refused.
pub(crate) fn query_pairs(query: &Map<String, Value>) -> Result<Vec<(String, String)>, GraphError> {
    query
        .iter()
        .map(|(name, value)| match value {
            Value::String(text) => Ok((name.clone(), text.clone())),
            Value::Number(_) | Value::Bool(_) => Ok((name.clone(), value.to_string())),
            _ => Err(invalid_input(
                "query values must be strings, numbers or booleans",
            )),
        })
        .collect()
}

// ---------------------------------------------------------------------------
// Transport
// ---------------------------------------------------------------------------

/// One answer, its body bounded.
pub(crate) struct RawResponse {
    pub(crate) status: StatusCode,
    pub(crate) headers: HeaderMap,
    pub(crate) body: Vec<u8>,
    /// The answer was larger than the cap; `body` is empty.
    pub(crate) oversize: bool,
}

#[async_trait]
pub(crate) trait GraphTransport: Send + Sync {
    /// One request. The body is read up to `max_body` bytes.
    async fn execute(&self, request: Request, max_body: usize) -> Result<RawResponse, GraphError>;
}

/// The production transport: bound to the run's egress policy, no redirect
/// followed, no protocol-level retry (a change is sent exactly once).
pub(crate) struct ReqwestGraphTransport {
    http: crate::egress::HttpClient,
}

impl ReqwestGraphTransport {
    pub(crate) fn new() -> Result<Self, GraphError> {
        let http = crate::egress::client_builder()
            .redirect(reqwest::redirect::Policy::none())
            .retry(reqwest::retry::never())
            .connect_timeout(CONNECT_TIMEOUT)
            .timeout(REQUEST_TIMEOUT)
            .user_agent(USER_AGENT_VALUE)
            .https_only(true)
            .build_for_run()
            .map_err(|_| invalid_input("the Microsoft Graph client could not be built"))?;
        Ok(Self { http })
    }
}

fn map_reqwest_error(error: &reqwest::Error) -> GraphError {
    if error.is_connect() {
        GraphError::new(
            GraphErrorKind::Unavailable,
            "Microsoft Graph could not be reached",
        )
    } else if error.is_timeout() {
        GraphError::new(
            GraphErrorKind::Timeout,
            "the Microsoft Graph request timed out",
        )
    } else if error.is_request() || error.is_body() {
        GraphError::new(
            GraphErrorKind::ConnectionLost,
            "the connection to Microsoft Graph was lost before it answered",
        )
    } else {
        invalid_response()
    }
}

#[async_trait]
impl GraphTransport for ReqwestGraphTransport {
    async fn execute(&self, request: Request, max_body: usize) -> Result<RawResponse, GraphError> {
        let mut response = self
            .http
            .execute(request)
            .await
            .map_err(|error| map_reqwest_error(&error))?;
        let status = response.status();
        let headers = response.headers().clone();
        let declared_oversize = headers
            .get(CONTENT_LENGTH)
            .and_then(|value| value.to_str().ok())
            .and_then(|value| value.parse::<usize>().ok())
            .is_some_and(|length| length > max_body);
        let oversize = |status, headers| RawResponse {
            status,
            headers,
            body: Vec::new(),
            oversize: true,
        };
        if declared_oversize {
            return Ok(oversize(status, headers));
        }
        let mut body = Vec::new();
        loop {
            match response.chunk().await {
                Ok(Some(chunk)) => {
                    if body.len().saturating_add(chunk.len()) > max_body {
                        return Ok(oversize(status, headers));
                    }
                    body.extend_from_slice(&chunk);
                }
                Ok(None) => break,
                Err(error) => return Err(map_reqwest_error(&error)),
            }
        }
        Ok(RawResponse {
            status,
            headers,
            body,
            oversize: false,
        })
    }
}

// ---------------------------------------------------------------------------
// The client
// ---------------------------------------------------------------------------

/// A request body.
#[derive(Clone, Debug)]
pub(crate) enum GraphBody {
    Empty,
    Json(Value),
    Bytes {
        content: Vec<u8>,
        content_type: String,
    },
}

/// One request to the client's endpoint.
#[derive(Clone, Debug)]
pub(crate) struct GraphRequest {
    pub(crate) method: Method,
    pub(crate) url: Url,
    /// Caller headers; reserved ones (authorization, host, ...) are ignored.
    pub(crate) headers: Vec<(String, String)>,
    pub(crate) body: GraphBody,
}

impl GraphRequest {
    #[must_use]
    pub(crate) fn get(url: Url) -> Self {
        Self {
            method: Method::GET,
            url,
            headers: Vec::new(),
            body: GraphBody::Empty,
        }
    }
}

/// A successful answer: its status, the headers worth showing, and the body.
#[derive(Clone, Debug)]
pub(crate) struct GraphResponse {
    pub(crate) status: u16,
    pub(crate) headers: BTreeMap<String, String>,
    pub(crate) body: Vec<u8>,
    pub(crate) url: Url,
}

impl GraphResponse {
    #[must_use]
    pub(crate) fn content_type(&self) -> String {
        self.headers
            .get("content-type")
            .and_then(|value| value.split(';').next())
            .unwrap_or_default()
            .trim()
            .to_lowercase()
    }

    #[must_use]
    pub(crate) fn is_json(&self) -> bool {
        let content_type = self.content_type();
        content_type == "application/json" || content_type.ends_with("+json")
    }

    #[must_use]
    pub(crate) fn is_text(&self) -> bool {
        let content_type = self.content_type();
        content_type.starts_with("text/")
            || content_type == "application/xml"
            || content_type == "application/xhtml+xml"
            || content_type.ends_with("+xml")
    }

    #[must_use]
    pub(crate) fn text(&self) -> String {
        String::from_utf8_lossy(&self.body).into_owned()
    }

    /// The body as JSON; an empty body is `None`.
    pub(crate) fn json(&self) -> Result<Option<Value>, GraphError> {
        if self.body.is_empty() {
            return Ok(None);
        }
        serde_json::from_slice(&self.body)
            .map(Some)
            .map_err(|_| invalid_response())
    }

    /// The body as a JSON object (a Graph collection page, a `$batch` answer).
    pub(crate) fn json_object(&self) -> Result<Map<String, Value>, GraphError> {
        match self.json()? {
            Some(Value::Object(object)) => Ok(object),
            _ => Err(invalid_response()),
        }
    }
}

/// How a client behaves beyond its endpoint and token.
#[derive(Clone, Debug)]
pub(crate) struct GraphClientOptions {
    /// The largest response body read.
    pub(crate) max_response_bytes: usize,
    /// Headers sent on every request before the caller's (Outlook sends
    /// `Prefer: IdType="ImmutableId"`).
    pub(crate) default_headers: Vec<(String, String)>,
}

impl Default for GraphClientOptions {
    fn default() -> Self {
        Self {
            max_response_bytes: DEFAULT_MAX_RESPONSE_BYTES,
            default_headers: Vec::new(),
        }
    }
}

/// Set a header, replacing an earlier one of the name; a reserved header
/// (authorization, host, ...) is ignored, because auth and transport are the
/// client's job.
fn put_header(
    headers: &mut Vec<(HeaderName, HeaderValue)>,
    name: &str,
    value: &str,
) -> Result<(), GraphError> {
    let name = HeaderName::try_from(name.trim())
        .map_err(|_| invalid_input("a request header name is not valid"))?;
    if RESERVED_HEADERS.contains(&name.as_str()) {
        return Ok(());
    }
    let value = HeaderValue::from_str(value)
        .map_err(|_| invalid_input("a request header value is not valid"))?;
    headers.retain(|(existing, _)| *existing != name);
    headers.push((name, value));
    Ok(())
}

/// What the loop does after one answer.
enum Step {
    Done(RawResponse),
    Again,
}

struct Attempts {
    refreshed: bool,
    retries: u32,
    redirects: u32,
}

/// Sends requests as the signed-in person, to one endpoint.
pub(crate) struct GraphClient {
    endpoint: Endpoint,
    token: Mutex<Zeroizing<String>>,
    refresher: Option<Arc<dyn TokenRefresher>>,
    transport: Arc<dyn GraphTransport>,
    options: GraphClientOptions,
}

impl GraphClient {
    pub(crate) fn new(
        endpoint: Endpoint,
        token: Zeroizing<String>,
        refresher: Option<Arc<dyn TokenRefresher>>,
        options: GraphClientOptions,
    ) -> Result<Self, GraphError> {
        Ok(Self::with_transport(
            endpoint,
            token,
            refresher,
            Arc::new(ReqwestGraphTransport::new()?),
            options,
        ))
    }

    pub(crate) fn with_transport(
        endpoint: Endpoint,
        token: Zeroizing<String>,
        refresher: Option<Arc<dyn TokenRefresher>>,
        transport: Arc<dyn GraphTransport>,
        options: GraphClientOptions,
    ) -> Self {
        Self {
            endpoint,
            token: Mutex::new(token),
            refresher,
            transport,
            options,
        }
    }

    #[must_use]
    pub(crate) const fn endpoint(&self) -> &Endpoint {
        &self.endpoint
    }

    /// One request, with the 401 handler, the throttling backoff and the
    /// redirect rule applied.
    pub(crate) async fn request(&self, request: GraphRequest) -> Result<GraphResponse, GraphError> {
        if !self.endpoint.admits(&request.url) {
            return Err(refused(&format!(
                "Only https://{} URLs are allowed",
                self.endpoint.host()
            )));
        }
        let headers = self.build_headers(&request)?;
        let mut method = request.method;
        let mut url = request.url;
        let mut body = request.body;
        let mut attempts = Attempts {
            refreshed: false,
            retries: 0,
            redirects: 0,
        };
        loop {
            let raw = self.send_once(&method, &url, &headers, &body).await?;
            match self
                .next_step(raw, &mut attempts, &mut method, &mut url, &mut body)
                .await?
            {
                Step::Again => {}
                Step::Done(raw) => return Ok(self.finish(raw, url)),
            }
        }
    }

    fn build_headers(
        &self,
        request: &GraphRequest,
    ) -> Result<Vec<(HeaderName, HeaderValue)>, GraphError> {
        let mut headers: Vec<(HeaderName, HeaderValue)> = Vec::new();
        put_header(&mut headers, "accept", "application/json")?;
        for (name, value) in &self.options.default_headers {
            put_header(&mut headers, name, value)?;
        }
        for (name, value) in &request.headers {
            put_header(&mut headers, name, value)?;
        }
        match &request.body {
            GraphBody::Empty => {}
            GraphBody::Json(_) => {
                if !headers.iter().any(|(name, _)| name == CONTENT_TYPE) {
                    put_header(&mut headers, "content-type", "application/json")?;
                }
            }
            GraphBody::Bytes { content_type, .. } => {
                put_header(&mut headers, "content-type", content_type)?;
            }
        }
        Ok(headers)
    }

    fn bearer(&self) -> Result<HeaderValue, GraphError> {
        let token = self
            .token
            .lock()
            .map_err(|_| invalid_input("the access token is unavailable"))?;
        let bearer = Zeroizing::new(format!("Bearer {}", token.as_str()));
        let mut value = HeaderValue::from_str(&bearer)
            .map_err(|_| invalid_input("the access token is not a valid header value"))?;
        value.set_sensitive(true);
        Ok(value)
    }

    async fn send_once(
        &self,
        method: &Method,
        url: &Url,
        headers: &[(HeaderName, HeaderValue)],
        body: &GraphBody,
    ) -> Result<RawResponse, GraphError> {
        let mut request = Request::new(method.clone(), url.clone());
        for (name, value) in headers {
            request.headers_mut().insert(name.clone(), value.clone());
        }
        request.headers_mut().insert(AUTHORIZATION, self.bearer()?);
        match body {
            GraphBody::Empty => {}
            GraphBody::Json(value) => {
                let bytes = serde_json::to_vec(value)
                    .map_err(|_| invalid_input("the request body is not valid JSON"))?;
                *request.body_mut() = Some(bytes.into());
            }
            GraphBody::Bytes { content, .. } => {
                *request.body_mut() = Some(content.clone().into());
            }
        }
        self.transport
            .execute(request, self.options.max_response_bytes)
            .await
    }

    async fn next_step(
        &self,
        raw: RawResponse,
        attempts: &mut Attempts,
        method: &mut Method,
        url: &mut Url,
        body: &mut GraphBody,
    ) -> Result<Step, GraphError> {
        let status = raw.status;
        if status == StatusCode::UNAUTHORIZED && !attempts.refreshed {
            attempts.refreshed = true;
            if self.refresh_token().await {
                return Ok(Step::Again);
            }
        }
        if (status == StatusCode::TOO_MANY_REQUESTS || status == StatusCode::SERVICE_UNAVAILABLE)
            && attempts.retries < MAX_RETRIES
        {
            let delay = retry_delay(&raw.headers, attempts.retries);
            attempts.retries += 1;
            tokio::time::sleep(delay).await;
            return Ok(Step::Again);
        }
        if status.is_redirection() && raw.headers.contains_key(LOCATION) {
            attempts.redirects += 1;
            let target = self.redirect_target(&raw, url, attempts.redirects)?;
            if status == StatusCode::SEE_OTHER
                || (matches!(status, StatusCode::MOVED_PERMANENTLY | StatusCode::FOUND)
                    && *method != Method::GET)
            {
                *method = Method::GET;
                *body = GraphBody::Empty;
            }
            *url = target;
            return Ok(Step::Again);
        }
        if status.is_success() {
            return if raw.oversize {
                Err(GraphError::new(
                    GraphErrorKind::ResourceExhausted,
                    format!(
                        "Response is larger than the toolkit limit of {} bytes",
                        self.options.max_response_bytes
                    ),
                ))
            } else {
                Ok(Step::Done(raw))
            };
        }
        let mut error = error_from_status(status, &raw.body);
        if matches!(
            status,
            StatusCode::TOO_MANY_REQUESTS | StatusCode::SERVICE_UNAVAILABLE
        ) {
            error.retry_after = Some(retry_delay(&raw.headers, 0));
        }
        Err(error)
    }

    /// The address a redirect names, only when it is on this endpoint: a
    /// pre-authenticated storage URL on another host is not followed.
    fn redirect_target(
        &self,
        raw: &RawResponse,
        current: &Url,
        redirects: u32,
    ) -> Result<Url, GraphError> {
        if redirects > MAX_REDIRECTS {
            return Err(refused("Too many redirects"));
        }
        let target = raw
            .headers
            .get(LOCATION)
            .and_then(|value| value.to_str().ok())
            .filter(|value| value.len() <= MAX_LINK_BYTES)
            .and_then(|value| current.join(value).ok())
            .ok_or_else(invalid_response)?;
        if self.endpoint.admits(&target) {
            Ok(target)
        } else {
            Err(refused(&format!(
                "Microsoft answered with a redirect to another host; only https://{} is followed",
                self.endpoint.host()
            )))
        }
    }

    /// One request to Main for another token. Whether it got one.
    async fn refresh_token(&self) -> bool {
        let Some(refresher) = &self.refresher else {
            return false;
        };
        let Some(fresh) = refresher.refresh().await else {
            return false;
        };
        match self.token.lock() {
            Ok(mut token) => {
                *token = fresh;
                true
            }
            Err(_) => false,
        }
    }

    fn finish(&self, raw: RawResponse, url: Url) -> GraphResponse {
        let headers = RESPONSE_HEADERS
            .iter()
            .filter_map(|name| {
                raw.headers
                    .get(*name)
                    .and_then(|value| value.to_str().ok())
                    .map(|value| ((*name).to_owned(), value.to_owned()))
            })
            .collect();
        GraphResponse {
            status: raw.status.as_u16(),
            headers,
            body: raw.body,
            url,
        }
    }

    // -- paging -------------------------------------------------------------

    /// `GET url` as a JSON object.
    pub(crate) async fn get_object(&self, url: Url) -> Result<Map<String, Value>, GraphError> {
        self.request(GraphRequest::get(url)).await?.json_object()
    }

    /// The page's `@odata.nextLink`, only when it is on this endpoint's host.
    pub(crate) fn next_link(&self, page: &Map<String, Value>) -> Result<Option<Url>, GraphError> {
        let Some(raw) = page.get("@odata.nextLink").and_then(Value::as_str) else {
            return Ok(None);
        };
        if raw.len() > MAX_LINK_BYTES || raw.contains('\\') {
            return Err(invalid_response());
        }
        let url = Url::parse(raw).map_err(|_| invalid_response())?;
        if self.endpoint.admits(&url) {
            Ok(Some(url))
        } else {
            Err(refused(
                "Microsoft Graph returned a paging link to another host; it is not followed",
            ))
        }
    }

    /// Up to `max_items` items of a collection that match `predicate`,
    /// scanning at most `max_scan`. `has_more` says the scan stopped early.
    pub(crate) async fn collect(
        &self,
        url: Url,
        max_items: usize,
        max_scan: usize,
        predicate: Option<&(dyn Fn(&Value) -> bool + Send + Sync)>,
    ) -> Result<Collected, GraphError> {
        let mut items = Vec::new();
        let mut scanned = 0_usize;
        let mut next = Some(url);
        for _ in 0..MAX_PAGES {
            let Some(page_url) = next.take() else {
                return Ok(Collected {
                    items,
                    has_more: false,
                });
            };
            let page = self.get_object(page_url).await?;
            next = self.next_link(&page)?;
            let values: &[Value] = page
                .get("value")
                .and_then(Value::as_array)
                .map(Vec::as_slice)
                .unwrap_or_default();
            for (position, item) in values.iter().enumerate() {
                scanned += 1;
                if predicate.is_none_or(|accepts| accepts(item)) {
                    items.push(item.clone());
                }
                if items.len() >= max_items || scanned >= max_scan {
                    return Ok(Collected {
                        items,
                        has_more: position + 1 < values.len() || next.is_some(),
                    });
                }
            }
        }
        Ok(Collected {
            items,
            has_more: next.is_some(),
        })
    }

    /// The first page with up to `pages` more appended to its `value`, stopping
    /// early when the items' JSON text passes `char_limit`. `check` sees each
    /// link before it is followed (a toolkit's own path rules). The merged
    /// object keeps the first page's other fields and the last unfollowed
    /// `@odata.nextLink`.
    pub(crate) async fn follow_pages(
        &self,
        first: Map<String, Value>,
        pages: usize,
        char_limit: usize,
        check: Option<&(dyn Fn(&Url) -> Result<(), GraphError> + Send + Sync)>,
    ) -> Result<Map<String, Value>, GraphError> {
        let mut items = first
            .get("value")
            .and_then(Value::as_array)
            .cloned()
            .unwrap_or_default();
        let mut next = self.next_link(&first)?;
        for _ in 0..pages {
            let Some(url) = next.take() else {
                break;
            };
            if let Some(check) = check {
                check(&url)?;
            }
            let page = self.get_object(url).await?;
            if let Some(values) = page.get("value").and_then(Value::as_array) {
                items.extend(values.iter().cloned());
            }
            next = self.next_link(&page)?;
            if serde_json::to_string(&items).map_or(0, |text| text.len()) > char_limit {
                break;
            }
        }
        let mut merged: Map<String, Value> = first
            .into_iter()
            .filter(|(key, _)| key != "value" && key != "@odata.nextLink")
            .collect();
        merged.insert("value".to_owned(), Value::Array(items));
        if let Some(url) = next {
            merged.insert("@odata.nextLink".to_owned(), Value::String(url.to_string()));
        }
        Ok(merged)
    }

    // -- batch --------------------------------------------------------------

    /// Up to 20 requests in one `$batch` call. Each is checked against
    /// `rules` first; each answer comes back with its own status and body.
    pub(crate) async fn batch(
        &self,
        items: &[BatchItem],
        api_version: &str,
        rules: Option<&RequestRules>,
    ) -> Result<Vec<BatchOutcome>, GraphError> {
        let requests = build_batch_requests(items, rules)?;
        let url = build_graph_url("/$batch", api_version, &[])?;
        let response = self
            .request(GraphRequest {
                method: Method::POST,
                url,
                headers: Vec::new(),
                body: GraphBody::Json(json!({ "requests": requests })),
            })
            .await?;
        parse_batch_response(&response.json_object()?)
    }
}

/// The items a collection scan kept.
#[derive(Clone, Debug)]
pub(crate) struct Collected {
    pub(crate) items: Vec<Value>,
    pub(crate) has_more: bool,
}

fn retry_delay(headers: &HeaderMap, attempt: u32) -> Duration {
    let seconds = headers
        .get(RETRY_AFTER)
        .and_then(|value| value.to_str().ok())
        .and_then(|value| value.trim().parse::<f64>().ok())
        .filter(|seconds| seconds.is_finite())
        .unwrap_or_else(|| f64::from(1_u32 << attempt.min(8)));
    Duration::from_secs_f64(seconds.clamp(0.0, MAX_RETRY_AFTER.as_secs_f64()))
}

// ---------------------------------------------------------------------------
// $batch
// ---------------------------------------------------------------------------

/// One request of a batch.
#[derive(Clone, Debug)]
pub(crate) struct BatchItem {
    pub(crate) id: String,
    pub(crate) method: String,
    /// Relative to the API version, e.g. `/me/messages`.
    pub(crate) url: String,
    pub(crate) body: Option<Value>,
    pub(crate) headers: BTreeMap<String, String>,
    pub(crate) depends_on: Vec<String>,
}

/// One answer of a batch.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct BatchOutcome {
    pub(crate) id: String,
    pub(crate) status: u16,
    pub(crate) body: Value,
}

fn build_batch_requests(
    items: &[BatchItem],
    rules: Option<&RequestRules>,
) -> Result<Vec<Value>, GraphError> {
    if items.is_empty() || items.len() > MAX_BATCH {
        return Err(invalid_input("graph_batch takes 1 to 20 requests"));
    }
    let mut seen = std::collections::BTreeSet::new();
    if !items.iter().all(|item| seen.insert(item.id.as_str())) {
        return Err(invalid_input("Request ids in a batch must be unique"));
    }
    let mut requests = Vec::with_capacity(items.len());
    for item in items {
        let method = if item.method.trim().is_empty() {
            "GET".to_owned()
        } else {
            item.method.trim().to_uppercase()
        };
        if item.url.contains("://") {
            return Err(invalid_input(
                "Batch request urls are relative to the API version, e.g. /me/messages",
            ));
        }
        if normalize_path(&item.url)? == "/$batch" {
            return Err(invalid_input("A batch cannot contain a $batch request"));
        }
        if let Some(rules) = rules {
            rules.check(&method, &item.url)?;
        }
        let mut entry = json!({
            "id": item.id,
            "method": method,
            "url": format!("/{}", item.url.trim_start_matches('/')),
        });
        let mut headers = item.headers.clone();
        if let Some(body) = &item.body {
            entry["body"] = body.clone();
            headers
                .entry("Content-Type".to_owned())
                .or_insert_with(|| "application/json".to_owned());
        }
        if !headers.is_empty() {
            entry["headers"] = json!(headers);
        }
        if !item.depends_on.is_empty() {
            entry["dependsOn"] = json!(item.depends_on);
        }
        requests.push(entry);
    }
    Ok(requests)
}

fn parse_batch_response(data: &Map<String, Value>) -> Result<Vec<BatchOutcome>, GraphError> {
    let responses = data
        .get("responses")
        .and_then(Value::as_array)
        .ok_or_else(invalid_response)?;
    responses
        .iter()
        .map(|response| {
            let id = match response.get("id") {
                Some(Value::String(id)) => id.clone(),
                Some(Value::Number(id)) => id.to_string(),
                _ => return Err(invalid_response()),
            };
            let status = response
                .get("status")
                .and_then(Value::as_u64)
                .and_then(|status| u16::try_from(status).ok())
                .ok_or_else(invalid_response)?;
            Ok(BatchOutcome {
                id,
                status,
                body: response.get("body").cloned().unwrap_or(Value::Null),
            })
        })
        .collect()
}
