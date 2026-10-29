//! The provider HTTP seam. Adapters never build a client: they get a
//! [`Transport`] pinned to their declared origins (`https_only`, no
//! redirects, the `google_places` timeouts), and tests swap in a fixture.
//!
//! Nothing here logs or keeps a URL: Shodan's key rides in the query string,
//! and a reqwest error is stripped of its URL before it is mapped.

use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use elitea_egress_guard::Allowlist;
use reqwest::header::{CONTENT_LENGTH, CONTENT_TYPE, RETRY_AFTER};
use reqwest::{Request, StatusCode};

use super::meter::FailureClass;
use crate::egress::BuildForRun as _;

const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_mins(1);

/// A provider or fetch failure. Carries no URL, key, query or body.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ProviderError {
    /// 401/403: the key was refused.
    AuthFailed,
    /// 429, with the provider's `Retry-After` in seconds when it sent one.
    RateLimited {
        retry_after_secs: Option<u64>,
    },
    Timeout,
    /// A network failure or a 5xx.
    Unavailable,
    /// 400/422: the provider refused the request shape.
    BadRequest,
    NotFound,
    /// The provider answered something this adapter cannot read.
    InvalidResponse,
    /// The answer exceeded the byte cap.
    TooLarge,
    /// The origin is not one the adapter declared, or the deployment's
    /// `ELITEA_WEB_EGRESS_ALLOWLIST` does not admit it.
    EgressRefused,
    /// A fetch URL (or a redirect hop) breaks the URL rules.
    UrlRefused,
    /// A fetch URL's host is outside the deployment's domain lists.
    DomainRefused,
    /// A fetch host resolves only to addresses egress refuses.
    AddressRefused,
    /// The host is not on this run's egress allowlist (#1490).
    HostNotAllowed,
    TooManyRedirects,
    /// A fetched page is not text, JSON or XHTML.
    UnsupportedContent,
}

impl ProviderError {
    /// The tool-result error code the model reads.
    pub(crate) const fn code(self) -> &'static str {
        match self {
            Self::AuthFailed => "auth_failed",
            Self::RateLimited { .. } => "rate_limited",
            Self::Timeout => "provider_timeout",
            Self::Unavailable => "provider_unavailable",
            Self::BadRequest => "provider_bad_request",
            Self::NotFound => "not_found",
            Self::InvalidResponse => "invalid_provider_response",
            Self::TooLarge => "response_too_large",
            Self::EgressRefused => "egress_not_allowed",
            Self::UrlRefused => "url_not_allowed",
            Self::DomainRefused => "domain_not_allowed",
            Self::AddressRefused => "address_not_allowed",
            Self::HostNotAllowed => crate::egress::EGRESS_HOST_NOT_ALLOWED,
            Self::TooManyRedirects => "too_many_redirects",
            Self::UnsupportedContent => "unsupported_content_type",
        }
    }

    pub(crate) const fn message(self) -> &'static str {
        match self {
            Self::AuthFailed => "the provider refused the configured API key",
            Self::RateLimited { .. } => "the provider rate limited the request",
            Self::Timeout => "the provider did not answer in time",
            Self::Unavailable => "the provider is unavailable",
            Self::BadRequest => "the provider refused the request",
            Self::NotFound => "the provider found nothing at that address",
            Self::InvalidResponse => "the provider returned an unreadable response",
            Self::TooLarge => "the response exceeds the size limit",
            Self::EgressRefused => "this deployment does not allow egress to the provider",
            Self::UrlRefused => {
                "only public https URLs on the default port, named by host and without credentials, may be fetched"
            }
            Self::DomainRefused => "this deployment does not allow fetching from that domain",
            Self::AddressRefused => {
                "that host resolves to a private, local or metadata address, which may not be fetched"
            }
            Self::HostNotAllowed => "the host is not on this run's egress allowlist",
            Self::TooManyRedirects => "the page redirected more than five times",
            Self::UnsupportedContent => {
                "only text, HTML, XHTML and JSON pages can be fetched (PDF is not supported)"
            }
        }
    }

    /// What the gateway's billing rule needs to know about this failure:
    /// whether the provider was reached and charges for the outcome.
    pub(crate) const fn failure_class(self) -> FailureClass {
        match self {
            Self::EgressRefused
            | Self::UrlRefused
            | Self::DomainRefused
            | Self::AddressRefused
            | Self::HostNotAllowed => FailureClass::NotReached,
            Self::AuthFailed | Self::RateLimited { .. } | Self::BadRequest => FailureClass::Refused,
            Self::NotFound => FailureClass::TargetFailed,
            Self::Timeout
            | Self::Unavailable
            | Self::InvalidResponse
            | Self::TooLarge
            | Self::TooManyRedirects
            | Self::UnsupportedContent => FailureClass::Failed,
        }
    }

    /// The error a status maps to, `None` for a success.
    pub(crate) fn from_status(status: StatusCode, retry_after_secs: Option<u64>) -> Option<Self> {
        if status.is_success() {
            return None;
        }
        Some(match status.as_u16() {
            401 | 403 => Self::AuthFailed,
            404 | 410 => Self::NotFound,
            408 | 504 => Self::Timeout,
            429 => Self::RateLimited { retry_after_secs },
            400 | 405..=499 => Self::BadRequest,
            _ => Self::Unavailable,
        })
    }
}

/// One bounded HTTP answer.
#[derive(Clone, Debug)]
pub(crate) struct HttpAnswer {
    pub(crate) status: StatusCode,
    pub(crate) content_type: Option<String>,
    pub(crate) retry_after_secs: Option<u64>,
    pub(crate) body: Vec<u8>,
}

impl HttpAnswer {
    /// The answer's media type, lower case, without parameters.
    pub(crate) fn media_type(&self) -> Option<String> {
        self.content_type.as_deref().map(|value| {
            value
                .split(';')
                .next()
                .unwrap_or_default()
                .trim()
                .to_ascii_lowercase()
        })
    }

    /// The error a non-success status maps to.
    pub(crate) fn error(&self) -> Option<ProviderError> {
        ProviderError::from_status(self.status, self.retry_after_secs)
    }

    /// The body as JSON, refusing a non-JSON answer.
    pub(crate) fn json(&self) -> Result<serde_json::Value, ProviderError> {
        if self
            .media_type()
            .is_some_and(|media| media != "application/json" && !media.ends_with("+json"))
        {
            return Err(ProviderError::InvalidResponse);
        }
        serde_json::from_slice(&self.body).map_err(|_| ProviderError::InvalidResponse)
    }
}

/// Send one request and read at most `max_bytes` of the answer.
#[async_trait]
pub(crate) trait Transport: Send + Sync {
    async fn send(&self, request: Request, max_bytes: usize) -> Result<HttpAnswer, ProviderError>;
}

/// Read a reqwest response into a bounded [`HttpAnswer`].
pub(crate) async fn read_answer(
    mut response: reqwest::Response,
    max_bytes: usize,
) -> Result<HttpAnswer, ProviderError> {
    let header = |name| {
        response
            .headers()
            .get(name)
            .and_then(|value: &reqwest::header::HeaderValue| value.to_str().ok())
            .map(str::to_owned)
    };
    let content_type = header(CONTENT_TYPE);
    let retry_after_secs = header(RETRY_AFTER).and_then(|value| value.trim().parse::<u64>().ok());
    if header(CONTENT_LENGTH)
        .and_then(|value| value.parse::<usize>().ok())
        .is_some_and(|length| length > max_bytes)
    {
        return Err(ProviderError::TooLarge);
    }
    let status = response.status();
    let mut body = Vec::new();
    while let Some(chunk) = response
        .chunk()
        .await
        .map_err(|error| map_reqwest(&error))?
    {
        if body.len().saturating_add(chunk.len()) > max_bytes {
            return Err(ProviderError::TooLarge);
        }
        body.extend_from_slice(&chunk);
    }
    Ok(HttpAnswer {
        status,
        content_type,
        retry_after_secs,
        body,
    })
}

/// Read at most `max_bytes` of an answer, keeping what fits: `true` when
/// the body was cut (a fetched page is truncated, not refused).
pub(crate) async fn read_answer_truncating(
    mut response: reqwest::Response,
    max_bytes: usize,
) -> Result<(HttpAnswer, bool), ProviderError> {
    let header = |name| {
        response
            .headers()
            .get(name)
            .and_then(|value: &reqwest::header::HeaderValue| value.to_str().ok())
            .map(str::to_owned)
    };
    let content_type = header(CONTENT_TYPE);
    let retry_after_secs = header(RETRY_AFTER).and_then(|value| value.trim().parse::<u64>().ok());
    let status = response.status();
    let mut body = Vec::new();
    let mut truncated = false;
    while let Some(chunk) = response
        .chunk()
        .await
        .map_err(|error| map_reqwest(&error))?
    {
        let room = max_bytes - body.len();
        if chunk.len() > room {
            body.extend_from_slice(&chunk[..room]);
            truncated = true;
            break;
        }
        body.extend_from_slice(&chunk);
    }
    Ok((
        HttpAnswer {
            status,
            content_type,
            retry_after_secs,
            body,
        },
        truncated,
    ))
}

pub(crate) fn map_reqwest(error: &reqwest::Error) -> ProviderError {
    if error.is_timeout() {
        ProviderError::Timeout
    } else {
        ProviderError::Unavailable
    }
}

/// The production provider transport: one pooled client per toolset,
/// refusing any request whose origin the adapter did not declare or the
/// deployment allowlist does not admit.
pub(crate) struct PinnedTransport {
    /// Bound to the run (#1490) for its ledger; see [`Transport::send`].
    http: crate::egress::HttpClient,
    origins: &'static [&'static str],
    egress: Arc<Allowlist>,
}

impl PinnedTransport {
    pub(crate) fn new(
        origins: &'static [&'static str],
        egress: Arc<Allowlist>,
        user_agent: &str,
    ) -> Result<Self, ProviderError> {
        let http = crate::egress::client_builder()
            .https_only(true)
            .connect_timeout(CONNECT_TIMEOUT)
            .pool_idle_timeout(POOL_IDLE_TIMEOUT)
            .pool_max_idle_per_host(4)
            .user_agent(user_agent)
            .build_for_run()
            .map_err(|_| ProviderError::Unavailable)?;
        Ok(Self {
            http,
            origins,
            egress,
        })
    }
}

/// Whether `url` is one of `origins` (scheme, host and port) and the
/// allowlist admits its host.
pub(crate) fn origin_admitted(url: &reqwest::Url, origins: &[&str], egress: &Allowlist) -> bool {
    let origin = url.origin().ascii_serialization();
    origins.contains(&origin.as_str())
        && url
            .host_str()
            .zip(url.port_or_known_default())
            .is_some_and(|(host, port)| egress.allows(host, port))
}

#[async_trait]
impl Transport for PinnedTransport {
    async fn send(&self, request: Request, max_bytes: usize) -> Result<HttpAnswer, ProviderError> {
        // The adapter's exact, compiled-in origin, admitted by the operator's
        // deployment allowlist: a worker-configured endpoint, not one the run
        // chose, so the run's egress allowlist (#1490) does not apply — Main
        // cannot know it. It is still recorded as reached.
        if !origin_admitted(request.url(), self.origins, &self.egress) {
            return Err(ProviderError::EgressRefused);
        }
        let response = self
            .http
            .execute_operator_origin(request)
            .await
            .map_err(|error| map_reqwest(&error.without_url()))?;
        read_answer(response, max_bytes).await
    }
}
