//! What every `ContentSource` connector here shares (ADR-0030 decision 3):
//! the egress-guarded transport, bounded 429 backoff that honours
//! `Retry-After`, the listing and fetch size caps, per-request timeouts that
//! suit a document download (connect and read-idle, not a whole-request
//! deadline), and the listing cache a fetch reads its version from.
//!
//! A connector sends through [`SourceHttp`], never through a provider
//! family's bounded JSON transport: a listing page and a fetched document
//! have their own, larger caps ([`SourceLimits`]), and every request —
//! including a provider-supplied pagination link — passes the egress
//! allowlist first ([`crate::egress::EgressGuard`], fail-closed).

#[cfg(test)]
pub(crate) mod fixture;

use std::collections::BTreeMap;
use std::future::Future;
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::Duration;

use elitea_content_source::{Acl, DocumentRef, SourceError, mime_of};
use serde_json::Value;
use tokio::sync::Mutex;

use crate::egress::{EgressGuard, HostAllowlist};
use crate::transport::header::RETRY_AFTER;
use crate::transport::{HeaderMap, Request, Response, StatusCode, Transport, TransportError, Url};

/// The default cap on one fetched document (ADR-0030: a separate, larger
/// cap than a tool read).
pub const DEFAULT_MAX_DOCUMENT_BYTES: u64 = 50 * 1_024 * 1_024;
/// The default cap on one listing page's body.
pub const DEFAULT_MAX_LISTING_PAGE_BYTES: usize = 32 * 1_024 * 1_024;
/// The default cap on the documents one source lists.
pub const DEFAULT_MAX_DOCUMENTS: usize = 200_000;
/// The default cap on the listing pages one listing follows (a server that
/// pages forever cannot hold a run). Directory reads are capped apart, by
/// [`DEFAULT_MAX_DIRECTORIES`].
pub const DEFAULT_MAX_PAGES: usize = 10_000;
/// The default cap on the directories (tree reads) one listing walks. Far
/// above the page cap: a monorepo holds tens of thousands of directories
/// and few documents per directory, and [`DEFAULT_MAX_DOCUMENTS`] stays the
/// real bound on what a listing returns.
pub const DEFAULT_MAX_DIRECTORIES: usize = 100_000;
/// How long a request may take to get a response (connection and first
/// byte), by default.
pub const DEFAULT_CONNECT_TIMEOUT: Duration = Duration::from_secs(30);
/// How long a response body may go without a byte, by default.
pub const DEFAULT_READ_IDLE_TIMEOUT: Duration = Duration::from_mins(1);
/// The whole-request deadline a source sends instead of the provider
/// family's 30 seconds: so far off that the idle timeout is what ends a
/// stalled download (reqwest has no way to say "none" per request).
const WHOLE_REQUEST_CEILING: Duration = Duration::from_hours(24);

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
    /// Listing pages in one listing (a directory's first page is a
    /// directory read, not counted here; its continuation pages are).
    pub max_pages: usize,
    /// Directories (tree reads) in one listing.
    pub max_directories: usize,
}

impl Default for SourceLimits {
    fn default() -> Self {
        Self {
            max_document_bytes: DEFAULT_MAX_DOCUMENT_BYTES,
            max_listing_page_bytes: DEFAULT_MAX_LISTING_PAGE_BYTES,
            max_documents: DEFAULT_MAX_DOCUMENTS,
            max_pages: DEFAULT_MAX_PAGES,
            max_directories: DEFAULT_MAX_DIRECTORIES,
        }
    }
}

/// The per-request timeouts one source sends with. Unlike the tool
/// families' fixed 30 s whole-request deadline, these let a large document
/// on a slow link finish: the request must get a response within `connect`,
/// and the body may then take as long as it likes provided a byte arrives
/// at least every `read_idle`.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Timeouts {
    /// Time to a response (connection and first byte).
    pub connect: Duration,
    /// Longest gap between body bytes.
    pub read_idle: Duration,
}

impl Default for Timeouts {
    fn default() -> Self {
        Self {
            connect: DEFAULT_CONNECT_TIMEOUT,
            read_idle: DEFAULT_READ_IDLE_TIMEOUT,
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
/// exhausted primary limit's reset: `x-ratelimit-reset` with
/// `x-ratelimit-remaining: 0` (epoch seconds; GitHub), else `RateLimit-Reset`
/// (epoch seconds; GitLab) with `RateLimit-Remaining` absent or `0`. A
/// `RateLimit-Reset` below 1 000 000 000 is read as seconds from now (the
/// IETF draft's form), not as a 1970 epoch.
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
        || header_text(headers, "ratelimit-remaining") == Some("0")
}

fn until_epoch(reset: i64, now: chrono::DateTime<chrono::Utc>) -> Duration {
    let delta = reset.saturating_sub(now.timestamp());
    Duration::from_secs(u64::try_from(delta).unwrap_or(0))
}

fn rate_limit_reset(headers: &HeaderMap, now: chrono::DateTime<chrono::Utc>) -> Option<Duration> {
    if header_text(headers, "x-ratelimit-remaining") == Some("0")
        && let Some(reset) =
            header_text(headers, "x-ratelimit-reset").and_then(|value| value.parse::<i64>().ok())
    {
        return Some(until_epoch(reset, now));
    }
    // GitLab: `RateLimit-Reset` (epoch seconds) with `RateLimit-Remaining`.
    let remaining = header_text(headers, "ratelimit-remaining");
    if !matches!(remaining, None | Some("0")) {
        return None;
    }
    let reset = header_text(headers, "ratelimit-reset")?
        .parse::<i64>()
        .ok()?;
    Some(if reset >= 1_000_000_000 {
        until_epoch(reset, now)
    } else {
        Duration::from_secs(u64::try_from(reset).unwrap_or(0))
    })
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
    /// A document changed since it was listed: the caller must list again.
    Changed,
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
            (Self::Changed, Some(key)) => SourceError::Changed(key.to_owned()),
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
                    Self::Changed => {
                        format!("{provider}: a document{what} changed since it was listed")
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

fn transport_failure(error: TransportError) -> Failure {
    if error.is_refused() {
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
    timeouts: Timeouts,
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
            timeouts: Timeouts::default(),
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
    pub fn with_timeouts(mut self, timeouts: Timeouts) -> Self {
        self.timeouts = timeouts;
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
    /// The request carries no provider-family deadline: it must get a
    /// response within [`Timeouts::connect`], and the response body (read by
    /// [`SourceHttp::body`]) fails only after [`Timeouts::read_idle`] with no
    /// byte.
    ///
    /// # Errors
    ///
    /// Refused by egress, no response, or still rate limited at the bound.
    pub async fn send(&self, mut request: Request) -> Result<Response, Failure> {
        *request.timeout_mut() = Some(WHOLE_REQUEST_CEILING);
        let mut attempt = 0_u32;
        loop {
            let response =
                tokio::time::timeout(self.timeouts.connect, self.guard.execute(request.clone()))
                    .await
                    .map_err(|_| transport_failure(TransportError::timeout()))?
                    .map_err(transport_failure)?
                    .with_idle_timeout(self.timeouts.read_idle);
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
        self.raw_with_headers(request).await.map(|(_, bytes)| bytes)
    }

    /// A document's raw bytes and the response headers (for a connector
    /// that verifies the version it was sent).
    ///
    /// # Errors
    ///
    /// See [`SourceHttp::raw`]; a 412 (a failed `If-Match`) is
    /// [`Failure::Changed`].
    pub async fn raw_with_headers(
        &self,
        request: Request,
    ) -> Result<(HeaderMap, Vec<u8>), Failure> {
        let mut response = self.send(request).await?;
        if response.status() == StatusCode::PRECONDITION_FAILED {
            return Err(Failure::Changed);
        }
        if !response.status().is_success() {
            return Err(Failure::Status(response.status()));
        }
        let bytes = Self::body(&mut response, self.limits.max_document_bytes).await?;
        Ok((response.headers().clone(), bytes))
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

    /// Refuse a listing that passed its page cap (continuation pages of a
    /// listing; directories are counted by
    /// [`SourceHttp::check_directories`]).
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

    /// Refuse a listing that passed its directory cap.
    ///
    /// # Errors
    ///
    /// More than `max_directories` directories.
    pub fn check_directories(&self, directories: usize) -> Result<(), Failure> {
        if directories > self.limits.max_directories {
            return Err(Failure::InvalidResponse(
                "the source holds more directories than the listing cap",
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

/// What a listed document's version is, so a fetch can pin to it.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum VersionKind {
    /// Pinned by the handle itself (a blob sha, a commit).
    #[default]
    Handle,
    /// An entity tag: the fetch sends `If-Match` and checks the response's
    /// `ETag`.
    ETag,
    /// A modification time: the fetch checks the response's
    /// `Last-Modified`.
    ModifiedAt,
}

/// One listed document and what a fetch needs to read it again.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Listed {
    pub reference: DocumentRef,
    /// The provider's handle for the bytes (a blob sha, an object key).
    pub handle: String,
    /// What `reference.version` is.
    pub kind: VersionKind,
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
            kind: VersionKind::Handle,
        }
    }

    /// Say what the version is (default: pinned by the handle).
    #[must_use]
    pub fn versioned_by(mut self, kind: VersionKind) -> Self {
        self.kind = kind;
        self
    }
}

/// A listing by key.
pub type Listing = BTreeMap<String, Listed>;

/// The references of `listing`, in key order.
#[must_use]
pub fn references(listing: &Listing) -> Vec<DocumentRef> {
    listing
        .values()
        .map(|listed| listed.reference.clone())
        .collect()
}

/// The last listing, by key, so a fetch reads the version it was listed
/// with (and a fetch before any listing lists once).
///
/// `list` and `fetch` both go through here, so a `list` racing a first
/// `fetch` (or another `list`) makes one listing and one snapshot.
#[derive(Default)]
pub struct ListingCache {
    listing: Mutex<Option<Arc<Listing>>>,
    /// Held while a listing runs, so concurrent callers share it.
    flight: Mutex<()>,
    /// Bumped each time a listing is remembered.
    generation: AtomicU64,
}

impl ListingCache {
    async fn remember(&self, listing: Vec<Listed>) -> Arc<Listing> {
        let map = Arc::new(
            listing
                .into_iter()
                .map(|listed| (listed.reference.key.clone(), listed))
                .collect::<Listing>(),
        );
        *self.listing.lock().await = Some(Arc::clone(&map));
        self.generation.fetch_add(1, Ordering::SeqCst);
        map
    }

    /// The remembered listing, or the result of `list` run once: callers
    /// that arrive while it runs wait for it and share its listing. A failed
    /// listing is not remembered, so the next call lists again.
    ///
    /// # Errors
    ///
    /// Whatever `list` fails with (to the caller that ran it; a waiter then
    /// runs its own).
    pub async fn get_or_list<E, F, Fut>(&self, list: F) -> Result<Arc<Listing>, E>
    where
        F: FnOnce() -> Fut,
        Fut: Future<Output = Result<Vec<Listed>, E>>,
    {
        if let Some(listing) = self.get().await {
            return Ok(listing);
        }
        let _flight = self.flight.lock().await;
        if let Some(listing) = self.get().await {
            return Ok(listing);
        }
        Ok(self.remember(list().await?).await)
    }

    /// A listing for a caller that wants the source as it is now (an
    /// explicit `list()`, which an indexer calls again when a fetch says a
    /// document changed): `list` run once, replacing the remembered
    /// listing. A caller that arrives while a listing is already running
    /// waits for it and shares its result instead of listing again, so
    /// concurrent `list` and `fetch` calls make one listing.
    ///
    /// # Errors
    ///
    /// Whatever `list` fails with.
    pub async fn list_shared<E, F, Fut>(&self, list: F) -> Result<Arc<Listing>, E>
    where
        F: FnOnce() -> Fut,
        Fut: Future<Output = Result<Vec<Listed>, E>>,
    {
        let seen = self.generation.load(Ordering::SeqCst);
        let _flight = self.flight.lock().await;
        if self.generation.load(Ordering::SeqCst) != seen
            && let Some(listing) = self.get().await
        {
            return Ok(listing);
        }
        Ok(self.remember(list().await?).await)
    }

    /// The remembered listing, if there is one.
    pub async fn get(&self) -> Option<Arc<Listing>> {
        self.listing.lock().await.clone()
    }
}

/// The last path segment: a document's title.
#[must_use]
pub fn title_of(key: &str) -> String {
    key.rsplit('/').next().unwrap_or(key).to_owned()
}

/// `base` with `segments` appended to its path, each percent-encoded, so a
/// path or branch holding `#`, `?`, `%` or a space cannot change the URL's
/// shape. `None` when `base` cannot carry a path.
#[must_use]
pub fn web_url<'a>(base: &Url, segments: impl IntoIterator<Item = &'a str>) -> Option<Url> {
    let mut url = base.clone();
    url.path_segments_mut()
        .ok()?
        .pop_if_empty()
        .extend(segments);
    Some(url)
}

pub use crate::git_id::{valid_git_object_id, valid_key};

#[cfg(test)]
mod tests {
    use super::*;
    use crate::transport::{HeaderValue, Method};

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
    fn rate_limit_resets_read_github_and_gitlab_headers() {
        let now = chrono::DateTime::parse_from_rfc3339("2015-10-21T07:28:00Z")
            .expect("now")
            .with_timezone(&chrono::Utc);
        let at = now.timestamp();
        let headers = |pairs: &[(&'static str, String)]| {
            let mut headers = HeaderMap::new();
            for (name, value) in pairs {
                headers.insert(
                    crate::transport::HeaderName::from_static(name),
                    HeaderValue::from_str(value).expect("header"),
                );
            }
            headers
        };
        // GitLab: epoch-seconds RateLimit-Reset, exhausted.
        let gitlab = headers(&[
            ("ratelimit-remaining", "0".to_owned()),
            ("ratelimit-reset", (at + 30).to_string()),
        ]);
        assert_eq!(retry_after(&gitlab, now), Some(Duration::from_secs(30)));
        // ... with no Remaining header at all (a 429 says enough) ...
        let bare = headers(&[("ratelimit-reset", (at + 45).to_string())]);
        assert_eq!(retry_after(&bare, now), Some(Duration::from_secs(45)));
        // ... a reset already past is "now" ...
        let past = headers(&[("ratelimit-reset", (at - 5).to_string())]);
        assert_eq!(retry_after(&past, now), Some(Duration::ZERO));
        // ... the IETF draft's delta form is seconds from now ...
        let delta = headers(&[("ratelimit-reset", "12".to_owned())]);
        assert_eq!(retry_after(&delta, now), Some(Duration::from_secs(12)));
        // ... a limit that is not exhausted says nothing ...
        let spare = headers(&[
            ("ratelimit-remaining", "7".to_owned()),
            ("ratelimit-reset", (at + 30).to_string()),
        ]);
        assert_eq!(retry_after(&spare, now), None);
        // ... GitHub's x-ratelimit pair still works, and Retry-After wins.
        let github = headers(&[
            ("x-ratelimit-remaining", "0".to_owned()),
            ("x-ratelimit-reset", (at + 9).to_string()),
            ("ratelimit-reset", (at + 30).to_string()),
        ]);
        assert_eq!(retry_after(&github, now), Some(Duration::from_secs(9)));
        let both = headers(&[
            ("retry-after", "2".to_owned()),
            ("ratelimit-reset", (at + 30).to_string()),
        ]);
        assert_eq!(retry_after(&both, now), Some(Duration::from_secs(2)));
    }

    #[tokio::test(start_paused = true)]
    async fn a_gitlab_rate_limit_waits_until_its_reset_within_the_bound() {
        use crate::source::fixture::{Reply, Scripted};
        let reset = (chrono::Utc::now().timestamp() + 3).to_string();
        let transport = Scripted::new(move |_, index| {
            if index == 0 {
                Reply::rate_limited(None)
                    .header("ratelimit-remaining", "0")
                    .header("ratelimit-reset", &reset)
            } else {
                Reply::bytes(b"ok")
            }
        });
        let http = SourceHttp::new(
            HostAllowlist::parse(Some("gitlab.example")),
            transport.clone(),
        );
        let request = Request::new(
            Method::GET,
            Url::parse("https://gitlab.example/api/v4/x").expect("url"),
        );
        let started = tokio::time::Instant::now();
        http.send(request.clone()).await.expect("after the reset");
        let waited = started.elapsed();
        assert!(
            (Duration::from_secs(2)..=Duration::from_secs(3)).contains(&waited),
            "{waited:?}"
        );

        // A reset past `max_wait` is not slept through.
        let far = (chrono::Utc::now().timestamp() + 3_600).to_string();
        let transport = Scripted::new(move |_, _| {
            Reply::rate_limited(None)
                .header("ratelimit-remaining", "0")
                .header("ratelimit-reset", &far)
        });
        let http = SourceHttp::new(
            HostAllowlist::parse(Some("gitlab.example")),
            transport.clone(),
        );
        assert_eq!(http.send(request).await.err(), Some(Failure::RateLimited));
        assert_eq!(transport.seen().len(), 1);
    }

    fn get(url: &str) -> Request {
        Request::new(Method::GET, Url::parse(url).expect("url"))
    }

    #[tokio::test(start_paused = true)]
    async fn a_slow_download_succeeds_while_bytes_keep_arriving() {
        use crate::source::fixture::Trickle;
        // 50 MiB, one MiB every five seconds: over four minutes, far past the
        // tool families' 30 s whole-request deadline.
        let transport = Trickle::new(50, 1_024 * 1_024, Duration::from_secs(5));
        let http = SourceHttp::new(
            HostAllowlist::parse(Some("host.example")),
            transport.clone(),
        );
        let mut request = get("https://host.example/blob");
        *request.timeout_mut() = Some(Duration::from_secs(30));
        let started = tokio::time::Instant::now();
        let bytes = http.raw(request).await.expect("the whole document");
        assert_eq!(bytes.len(), 50 * 1_024 * 1_024);
        assert!(started.elapsed() >= Duration::from_secs(250));
        let sent = transport.timeouts();
        assert!(
            sent.iter()
                .all(|timeout| timeout.is_some_and(|t| t > Duration::from_hours(1))),
            "the family's 30 s whole-request deadline is not inherited: {sent:?}"
        );
    }

    #[tokio::test(start_paused = true)]
    async fn a_body_that_goes_idle_times_out() {
        use crate::source::fixture::Trickle;
        let transport = Trickle::new(3, 16, Duration::from_secs(90));
        let http = SourceHttp::new(
            HostAllowlist::parse(Some("host.example")),
            transport.clone(),
        );
        let failure = http
            .raw(get("https://host.example/blob"))
            .await
            .expect_err("idle for 90 s against a 60 s idle timeout");
        assert_eq!(
            failure,
            Failure::Transport("the request timed out".to_owned())
        );

        // The idle timeout is configurable.
        let http = SourceHttp::new(HostAllowlist::parse(Some("host.example")), transport)
            .with_timeouts(Timeouts {
                read_idle: Duration::from_mins(2),
                ..Timeouts::default()
            });
        assert_eq!(
            http.raw(get("https://host.example/blob"))
                .await
                .expect("patient")
                .len(),
            48
        );
    }

    #[tokio::test(start_paused = true)]
    async fn a_server_that_never_answers_times_out_at_connect() {
        use crate::source::fixture::Stalled;
        let http = SourceHttp::new(
            HostAllowlist::parse(Some("host.example")),
            Arc::new(Stalled),
        )
        .with_timeouts(Timeouts {
            connect: Duration::from_secs(5),
            ..Timeouts::default()
        });
        let started = tokio::time::Instant::now();
        let failure = http
            .send(get("https://host.example/x"))
            .await
            .expect_err("no response");
        assert_eq!(
            failure,
            Failure::Transport("the request timed out".to_owned())
        );
        assert_eq!(started.elapsed(), Duration::from_secs(5));
    }

    #[tokio::test]
    async fn an_egress_refusal_is_classified_by_the_guard_not_recomputed() {
        use crate::source::fixture::{Reply, Scripted};
        let transport = Scripted::new(|_, _| Reply::bytes(b""));
        let http = SourceHttp::new(
            HostAllowlist::parse(Some("api.github.com")),
            transport.clone(),
        );
        assert_eq!(
            http.send(get("https://evil.example/next")).await.err(),
            Some(Failure::Refused)
        );
        assert!(transport.seen().is_empty());
        // A network failure is not a refusal, whatever the allowlist says.
        assert_eq!(
            transport_failure(TransportError::connect()),
            Failure::Transport("the connection could not be made".to_owned())
        );
    }

    #[test]
    fn keys_are_relative_paths() {
        assert!(valid_key("docs/a.md"));
        for key in ["", "/a", "a/", "a//b", "a/../b", "./a", "a\\b", "a\0b"] {
            assert!(!valid_key(key), "{key:?}");
        }
    }

    fn one(key: &str) -> Vec<Listed> {
        vec![Listed::new(
            key.to_owned(),
            "v".to_owned(),
            1,
            "h".to_owned(),
        )]
    }

    #[tokio::test]
    async fn concurrent_callers_share_one_listing() {
        let cache = ListingCache::default();
        let runs = std::sync::atomic::AtomicUsize::new(0);
        let list = || async {
            runs.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
            tokio::task::yield_now().await;
            Ok::<_, ()>(one("a.md"))
        };
        let (a, b, c, d) = tokio::join!(
            cache.get_or_list(list),
            cache.get_or_list(list),
            cache.get_or_list(list),
            cache.get_or_list(list),
        );
        for listing in [a, b, c, d] {
            assert!(listing.expect("listing").contains_key("a.md"));
        }
        assert_eq!(runs.load(std::sync::atomic::Ordering::SeqCst), 1);
    }

    #[tokio::test]
    async fn a_failed_listing_is_not_cached() {
        let cache = ListingCache::default();
        assert_eq!(
            cache
                .get_or_list(|| async { Err::<Vec<Listed>, _>("down") })
                .await
                .err(),
            Some("down")
        );
        assert!(cache.get().await.is_none());
        let listing = cache
            .get_or_list(|| async { Ok::<_, &str>(one("a.md")) })
            .await
            .expect("the next call retries");
        assert!(listing.contains_key("a.md"));
    }

    #[test]
    fn urls_encode_every_segment() {
        let base = Url::parse("https://host/prefix/").expect("url");
        let url = web_url(&base, ["a b", "c#d", "e%f", "g?h"]).expect("url");
        assert_eq!(url.as_str(), "https://host/prefix/a%20b/c%23d/e%25f/g%3Fh");
    }

    #[test]
    fn git_object_ids_are_full_hex() {
        assert!(valid_git_object_id(&"a".repeat(40)));
        assert!(valid_git_object_id(&"A".repeat(64)));
        for id in [
            "",
            "abc1234",
            &"g".repeat(40),
            &"a".repeat(41),
            &"a".repeat(39),
        ] {
            assert!(!valid_git_object_id(id), "{id:?}");
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
