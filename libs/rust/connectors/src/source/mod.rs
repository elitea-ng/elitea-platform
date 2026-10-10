//! What every `ContentSource` connector here shares (ADR-0030 decision 3):
//! the egress-guarded transport, bounded 429 backoff that honours
//! `Retry-After`, the listing and fetch size caps, and the listing cache a
//! fetch reads its version from.
//!
//! A connector sends through [`SourceHttp`], never through a provider
//! family's bounded JSON transport: a listing page and a fetched document
//! have their own, larger caps ([`SourceLimits`]), and every request —
//! including a provider-supplied pagination link — passes the egress
//! allowlist first ([`crate::egress::EgressGuard`], fail-closed).

#[cfg(test)]
pub(crate) mod fixture;

use std::collections::BTreeMap;
use std::sync::Arc;
use std::time::Duration;

use elitea_content_source::{Acl, DocumentRef, SourceError, mime_of};
use serde_json::Value;
use tokio::sync::Mutex;

use crate::egress::{EgressGuard, HostAllowlist};
use crate::transport::header::RETRY_AFTER;
use crate::transport::{HeaderMap, Request, Response, StatusCode, Transport, TransportError};

/// The default cap on one fetched document (ADR-0030: a separate, larger
/// cap than a tool read).
pub const DEFAULT_MAX_DOCUMENT_BYTES: u64 = 50 * 1_024 * 1_024;
/// The default cap on one listing page's body.
pub const DEFAULT_MAX_LISTING_PAGE_BYTES: usize = 32 * 1_024 * 1_024;
/// The default cap on the documents one source lists.
pub const DEFAULT_MAX_DOCUMENTS: usize = 200_000;
/// The default cap on the pages one listing walks (a server that pages
/// forever cannot hold a run).
pub const DEFAULT_MAX_PAGES: usize = 10_000;

/// Size and count caps for one source.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct SourceLimits {
    /// One fetched document's bytes. A document the listing already says is
    /// larger is refused before it is requested.
    pub max_document_bytes: u64,
    /// One listing page's body.
    pub max_listing_page_bytes: usize,
    /// Documents in one listing.
    pub max_documents: usize,
    /// Pages (or directory reads) in one listing.
    pub max_pages: usize,
}

impl Default for SourceLimits {
    fn default() -> Self {
        Self {
            max_document_bytes: DEFAULT_MAX_DOCUMENT_BYTES,
            max_listing_page_bytes: DEFAULT_MAX_LISTING_PAGE_BYTES,
            max_documents: DEFAULT_MAX_DOCUMENTS,
            max_pages: DEFAULT_MAX_PAGES,
        }
    }
}

/// Bounded backoff for a rate-limited request (HTTP 429, or a 403 that
/// carries `Retry-After`, GitHub's secondary limit).
///
/// A `Retry-After` the server sends is honoured, in seconds or as an HTTP
/// date; without one the wait doubles from `initial`. A wait longer than
/// `max_wait` is not slept through: the request fails as rate limited, so
/// one source cannot hold a run for an hour.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Backoff {
    /// Tries in all, the first included.
    pub max_attempts: u32,
    pub initial: Duration,
    pub max_wait: Duration,
}

impl Default for Backoff {
    fn default() -> Self {
        Self {
            max_attempts: 5,
            initial: Duration::from_secs(1),
            max_wait: Duration::from_mins(1),
        }
    }
}

impl Backoff {
    /// The wait before try `attempt + 1` (`attempt` counts from 0), or
    /// `None` when the bound says stop.
    #[must_use]
    pub fn wait(&self, attempt: u32, retry_after: Option<Duration>) -> Option<Duration> {
        if attempt.saturating_add(1) >= self.max_attempts {
            return None;
        }
        let wait = retry_after.unwrap_or_else(|| {
            self.initial
                .saturating_mul(2_u32.saturating_pow(attempt.min(16)))
                .min(self.max_wait)
        });
        (wait <= self.max_wait).then_some(wait)
    }
}

/// How long the server asks a client to wait: `Retry-After` (delta-seconds,
/// or an HTTP date relative to `now`; a date in the past is "now"), else an
/// exhausted primary limit's `x-ratelimit-reset` (epoch seconds; GitHub,
/// GitLab).
#[must_use]
pub fn retry_after(headers: &HeaderMap, now: chrono::DateTime<chrono::Utc>) -> Option<Duration> {
    let Some(value) = headers.get(RETRY_AFTER) else {
        return rate_limit_reset(headers, now);
    };
    let value = value.to_str().ok()?.trim();
    if let Ok(seconds) = value.parse::<u64>() {
        return Some(Duration::from_secs(seconds));
    }
    let at = chrono::DateTime::parse_from_rfc2822(value).ok()?;
    let delta = at.with_timezone(&chrono::Utc) - now;
    Some(delta.to_std().unwrap_or(Duration::ZERO))
}

fn header_text<'a>(headers: &'a HeaderMap, name: &str) -> Option<&'a str> {
    headers.get(name)?.to_str().ok().map(str::trim)
}

fn primary_limit_exhausted(headers: &HeaderMap) -> bool {
    header_text(headers, "x-ratelimit-remaining") == Some("0")
}

fn rate_limit_reset(headers: &HeaderMap, now: chrono::DateTime<chrono::Utc>) -> Option<Duration> {
    if !primary_limit_exhausted(headers) {
        return None;
    }
    let reset = header_text(headers, "x-ratelimit-reset")?
        .parse::<i64>()
        .ok()?;
    let delta = reset.saturating_sub(now.timestamp());
    Some(Duration::from_secs(u64::try_from(delta).unwrap_or(0)))
}

/// A 429, or a 403 that is a rate limit (GitHub answers its primary and
/// secondary limits with 403 and `x-ratelimit-remaining: 0` or
/// `Retry-After`).
fn rate_limited(response: &Response) -> bool {
    response.status() == StatusCode::TOO_MANY_REQUESTS
        || (response.status() == StatusCode::FORBIDDEN
            && (response.headers().contains_key(RETRY_AFTER)
                || primary_limit_exhausted(response.headers())))
}

/// Why a connector request failed, before it becomes a [`SourceError`].
/// Carries no URL, header or body.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Failure {
    /// The host is not on the egress allowlist (or the list is empty).
    Refused,
    /// No response: timed out, could not connect, body stopped.
    Transport(String),
    /// Still rate limited once the backoff bound was reached.
    RateLimited,
    /// A body larger than its cap.
    TooLarge(u64),
    /// The provider answered with a status the connector cannot use.
    Status(StatusCode),
    /// The body is not what the provider documents.
    InvalidResponse(&'static str),
    /// The provider client refused to build the request (its data-free
    /// message: an invalid configuration, an unsupported credential).
    Client(String),
}

impl Failure {
    /// As the content layer reports it, naming the provider and, when one
    /// is given, the document.
    #[must_use]
    pub fn into_source_error(self, provider: &str, key: Option<&str>) -> SourceError {
        match (self, key) {
            (Self::Status(StatusCode::NOT_FOUND), Some(key)) => {
                SourceError::NotFound(key.to_owned())
            }
            (failure, key) => {
                let what = key.map_or_else(String::new, |key| format!(" for '{key}'"));
                SourceError::Unavailable(match failure {
                    Self::Refused => format!(
                        "{provider}: the host is not on the egress allowlist, so the request{what} was refused before it was sent"
                    ),
                    Self::Transport(cause) => format!("{provider}: {cause}{what}"),
                    Self::RateLimited => {
                        format!("{provider} rate limited the request{what} past the backoff bound")
                    }
                    Self::TooLarge(limit) => {
                        format!("{provider}: the response{what} is larger than {limit} bytes")
                    }
                    Self::Status(status) => {
                        format!("{provider} answered HTTP {}{what}", status.as_u16())
                    }
                    Self::InvalidResponse(why) => {
                        format!("{provider} returned an invalid response{what}: {why}")
                    }
                    Self::Client(message) => format!("{provider}: {message}{what}"),
                })
            }
        }
    }
}

fn transport_failure(error: TransportError, egress_refused: bool) -> Failure {
    if egress_refused {
        Failure::Refused
    } else {
        Failure::Transport(error.to_string())
    }
}

/// The guarded transport, the caps and the backoff one source sends with.
#[derive(Clone)]
pub struct SourceHttp {
    guard: Arc<EgressGuard>,
    limits: SourceLimits,
    backoff: Backoff,
}

impl SourceHttp {
    /// Every request goes through `allowlist` (fail-closed: an empty list
    /// refuses everything) before `transport` sees it.
    #[must_use]
    pub fn new(allowlist: HostAllowlist, transport: Arc<dyn Transport>) -> Self {
        Self {
            guard: Arc::new(EgressGuard::new(allowlist, transport)),
            limits: SourceLimits::default(),
            backoff: Backoff::default(),
        }
    }

    #[must_use]
    pub fn with_limits(mut self, limits: SourceLimits) -> Self {
        self.limits = limits;
        self
    }

    #[must_use]
    pub fn with_backoff(mut self, backoff: Backoff) -> Self {
        self.backoff = backoff;
        self
    }

    #[must_use]
    pub fn limits(&self) -> &SourceLimits {
        &self.limits
    }

    /// The guarded transport, for a provider client built over it.
    #[must_use]
    pub fn transport(&self) -> Arc<dyn Transport> {
        self.guard.clone()
    }

    /// Whether the allowlist admits `url`'s host.
    #[must_use]
    pub fn permits(&self, url: &url::Url) -> bool {
        self.guard.allowlist().permits_url(url)
    }

    /// Send `request`, backing off on a rate limit within the bound.
    ///
    /// # Errors
    ///
    /// Refused by egress, no response, or still rate limited at the bound.
    pub async fn send(&self, request: Request) -> Result<Response, Failure> {
        let refused = !self.permits(request.url());
        let mut attempt = 0_u32;
        loop {
            let response = self
                .guard
                .execute(request.clone())
                .await
                .map_err(|error| transport_failure(error, refused))?;
            if !rate_limited(&response) {
                return Ok(response);
            }
            let hint = retry_after(response.headers(), chrono::Utc::now());
            let Some(wait) = self.backoff.wait(attempt, hint) else {
                return Err(Failure::RateLimited);
            };
            drop(response);
            tokio::time::sleep(wait).await;
            attempt = attempt.saturating_add(1);
        }
    }

    /// A body of at most `limit` bytes.
    ///
    /// # Errors
    ///
    /// The body is larger (by its declared length or as it streams), or it
    /// stopped.
    pub async fn body(response: &mut Response, limit: u64) -> Result<Vec<u8>, Failure> {
        let cap = usize::try_from(limit).unwrap_or(usize::MAX);
        if response.declares_more_than(cap) {
            return Err(Failure::TooLarge(limit));
        }
        response
            .bytes_within(cap)
            .await
            .map_err(|error| Failure::Transport(error.to_string()))?
            .ok_or(Failure::TooLarge(limit))
    }

    /// A listing page: 2xx, JSON, within the listing cap.
    ///
    /// # Errors
    ///
    /// See [`SourceHttp::send`]; a non-2xx status; a body over the cap or
    /// not JSON.
    pub async fn json(&self, request: Request) -> Result<(HeaderMap, Value), Failure> {
        let mut response = self.send(request).await?;
        if !response.status().is_success() {
            return Err(Failure::Status(response.status()));
        }
        let limit = u64::try_from(self.limits.max_listing_page_bytes).unwrap_or(u64::MAX);
        let bytes = Self::body(&mut response, limit).await?;
        let value = serde_json::from_slice(&bytes)
            .map_err(|_| Failure::InvalidResponse("a listing page is not JSON"))?;
        Ok((response.headers().clone(), value))
    }

    /// A document's raw bytes: 2xx, within the document cap.
    ///
    /// # Errors
    ///
    /// See [`SourceHttp::send`]; a non-2xx status; a body over the cap.
    pub async fn raw(&self, request: Request) -> Result<Vec<u8>, Failure> {
        let mut response = self.send(request).await?;
        if !response.status().is_success() {
            return Err(Failure::Status(response.status()));
        }
        Self::body(&mut response, self.limits.max_document_bytes).await
    }

    /// Refuse a listing that passed its document cap.
    ///
    /// # Errors
    ///
    /// More than `max_documents` documents.
    pub fn check_count(&self, count: usize) -> Result<(), Failure> {
        if count > self.limits.max_documents {
            return Err(Failure::InvalidResponse(
                "the source holds more documents than the listing cap",
            ));
        }
        Ok(())
    }

    /// Refuse a listing that passed its page cap.
    ///
    /// # Errors
    ///
    /// More than `max_pages` pages.
    pub fn check_pages(&self, pages: usize) -> Result<(), Failure> {
        if pages > self.limits.max_pages {
            return Err(Failure::InvalidResponse(
                "the listing did not end within the page cap",
            ));
        }
        Ok(())
    }

    /// Refuse, before fetching, a document the listing says is too large.
    ///
    /// # Errors
    ///
    /// `size` is over the document cap.
    pub fn check_size(&self, size: u64) -> Result<(), Failure> {
        if size > self.limits.max_document_bytes {
            return Err(Failure::TooLarge(self.limits.max_document_bytes));
        }
        Ok(())
    }
}

/// One listed document and what a fetch needs to read it again.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Listed {
    pub reference: DocumentRef,
    /// The provider's handle for the bytes (a blob sha, an object key).
    pub handle: String,
}

impl Listed {
    /// A project-scoped document keyed by `key`.
    #[must_use]
    pub fn new(key: String, version: String, size: u64, handle: String) -> Self {
        Self {
            reference: DocumentRef {
                mime: mime_of(&key).to_owned(),
                key,
                version,
                size,
                acl: Acl::Project,
            },
            handle,
        }
    }
}

/// The last listing, by key, so a fetch reads the version it was listed
/// with (and a fetch before any listing lists once).
#[derive(Default)]
pub struct ListingCache(Mutex<Option<Arc<BTreeMap<String, Listed>>>>);

impl ListingCache {
    /// Remember `listing` and hand back its references in key order.
    pub async fn store(&self, listing: Vec<Listed>) -> Vec<DocumentRef> {
        let map: BTreeMap<String, Listed> = listing
            .into_iter()
            .map(|listed| (listed.reference.key.clone(), listed))
            .collect();
        let references = map
            .values()
            .map(|listed| listed.reference.clone())
            .collect();
        *self.0.lock().await = Some(Arc::new(map));
        references
    }

    /// The remembered listing, if there is one.
    pub async fn get(&self) -> Option<Arc<BTreeMap<String, Listed>>> {
        self.0.lock().await.clone()
    }
}

/// The last path segment: a document's title.
#[must_use]
pub fn title_of(key: &str) -> String {
    key.rsplit('/').next().unwrap_or(key).to_owned()
}

/// A repository-relative path a connector may key a document by: no empty,
/// `.` or `..` segment, no NUL or backslash, no leading `/`.
#[must_use]
pub fn valid_key(key: &str) -> bool {
    !key.is_empty()
        && !key.contains(['\0', '\\'])
        && key
            .split('/')
            .all(|segment| !matches!(segment, "" | "." | ".."))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::transport::HeaderValue;

    #[test]
    fn backoff_honours_retry_after_within_the_bound() {
        let backoff = Backoff {
            max_attempts: 3,
            initial: Duration::from_secs(1),
            max_wait: Duration::from_secs(10),
        };
        assert_eq!(
            backoff.wait(0, Some(Duration::from_secs(7))),
            Some(Duration::from_secs(7))
        );
        assert_eq!(backoff.wait(0, Some(Duration::from_secs(11))), None);
        assert_eq!(backoff.wait(0, None), Some(Duration::from_secs(1)));
        assert_eq!(backoff.wait(1, None), Some(Duration::from_secs(2)));
        assert_eq!(backoff.wait(2, None), None, "three tries in all");
    }

    #[test]
    fn retry_after_reads_seconds_and_dates() {
        let now = chrono::DateTime::parse_from_rfc3339("2015-10-21T07:28:00Z")
            .expect("now")
            .with_timezone(&chrono::Utc);
        let mut headers = HeaderMap::new();
        headers.insert(RETRY_AFTER, HeaderValue::from_static("120"));
        assert_eq!(retry_after(&headers, now), Some(Duration::from_mins(2)));
        headers.insert(
            RETRY_AFTER,
            HeaderValue::from_static("Wed, 21 Oct 2015 07:28:30 GMT"),
        );
        assert_eq!(retry_after(&headers, now), Some(Duration::from_secs(30)));
        headers.insert(
            RETRY_AFTER,
            HeaderValue::from_static("Wed, 21 Oct 2015 07:00:00 GMT"),
        );
        assert_eq!(retry_after(&headers, now), Some(Duration::ZERO));
        headers.insert(RETRY_AFTER, HeaderValue::from_static("soon"));
        assert_eq!(retry_after(&headers, now), None);
    }

    #[test]
    fn keys_are_relative_paths() {
        assert!(valid_key("docs/a.md"));
        for key in ["", "/a", "a/", "a//b", "a/../b", "./a", "a\\b", "a\0b"] {
            assert!(!valid_key(key), "{key:?}");
        }
    }

    #[test]
    fn failures_read_as_source_errors() {
        assert!(matches!(
            Failure::Status(StatusCode::NOT_FOUND).into_source_error("GitHub", Some("a.md")),
            SourceError::NotFound(key) if key == "a.md"
        ));
        let refused = Failure::Refused
            .into_source_error("GitHub", None)
            .to_string();
        assert!(refused.contains("egress allowlist"), "{refused}");
    }
}
