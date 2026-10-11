//! The one outbound seam (ADR-0030 decision 3): a request, a streamed
//! response and the [`Transport`] that carries one to the other.
//!
//! The types mirror the parts of `reqwest::Request` / `reqwest::Response`
//! the provider clients use, so a client reads the same whichever reqwest the
//! host links — and so this crate pins no TLS stack. The host builds its own
//! reqwest client (rustls with ring in the worker, aws-lc-rs in the engines)
//! and hands it to [`crate::reqwest012`] or [`crate::reqwest013`]; a test
//! hands a fixture.
//!
//! The method, status, header and URL types are the `http` and `url` crates'
//! own, which is what both reqwest lines re-export.

use std::fmt;
use std::time::Duration;

use async_trait::async_trait;
use bytes::Bytes;
pub use http::header::{self, HeaderMap, HeaderName, HeaderValue};
pub use http::{Method, StatusCode};
pub use url::Url;

/// A request body. Always in memory: every request a provider client sends
/// is bounded before it is built.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Body(Bytes);

impl Body {
    /// The bytes. `Option` only to match `reqwest::Body::as_bytes`, so a
    /// fixture reads a body the way it always has.
    #[must_use]
    pub fn as_bytes(&self) -> Option<&[u8]> {
        Some(&self.0)
    }

    /// The body, for a transport to send.
    #[must_use]
    pub fn into_bytes(self) -> Bytes {
        self.0
    }
}

impl From<Bytes> for Body {
    fn from(bytes: Bytes) -> Self {
        Self(bytes)
    }
}

impl From<Vec<u8>> for Body {
    fn from(bytes: Vec<u8>) -> Self {
        Self(Bytes::from(bytes))
    }
}

impl From<String> for Body {
    fn from(text: String) -> Self {
        Self(Bytes::from(text))
    }
}

impl From<&'static str> for Body {
    fn from(text: &'static str) -> Self {
        Self(Bytes::from_static(text.as_bytes()))
    }
}

impl From<&'static [u8]> for Body {
    fn from(bytes: &'static [u8]) -> Self {
        Self(Bytes::from_static(bytes))
    }
}

/// One outbound request: `reqwest::Request`'s shape, without its client.
///
/// `Debug` prints a sensitive header (a credential) as `Sensitive`, as
/// `http::HeaderMap` does.
#[derive(Clone, Debug)]
pub struct Request {
    method: Method,
    url: Url,
    headers: HeaderMap,
    body: Option<Body>,
    timeout: Option<Duration>,
}

impl Request {
    #[must_use]
    pub fn new(method: Method, url: Url) -> Self {
        Self {
            method,
            url,
            headers: HeaderMap::new(),
            body: None,
            timeout: None,
        }
    }

    #[must_use]
    pub fn method(&self) -> &Method {
        &self.method
    }

    pub fn method_mut(&mut self) -> &mut Method {
        &mut self.method
    }

    #[must_use]
    pub fn url(&self) -> &Url {
        &self.url
    }

    pub fn url_mut(&mut self) -> &mut Url {
        &mut self.url
    }

    #[must_use]
    pub fn headers(&self) -> &HeaderMap {
        &self.headers
    }

    pub fn headers_mut(&mut self) -> &mut HeaderMap {
        &mut self.headers
    }

    #[must_use]
    pub fn body(&self) -> Option<&Body> {
        self.body.as_ref()
    }

    pub fn body_mut(&mut self) -> &mut Option<Body> {
        &mut self.body
    }

    /// The per-request timeout, overriding the client's.
    #[must_use]
    pub fn timeout(&self) -> Option<&Duration> {
        self.timeout.as_ref()
    }

    pub fn timeout_mut(&mut self) -> &mut Option<Duration> {
        &mut self.timeout
    }

    /// The parts, for a transport to send.
    #[must_use]
    pub fn into_parts(self) -> (Method, Url, HeaderMap, Option<Body>, Option<Duration>) {
        (self.method, self.url, self.headers, self.body, self.timeout)
    }
}

/// Why a request did not produce a response, or a body stopped.
///
/// Carries only the classification `reqwest::Error` offers (its `is_*`
/// predicates), never the URL, a header or a body, so no family can leak one
/// through an error. More than one predicate can hold (a connect that timed
/// out), as with reqwest: combine them with [`TransportError::and`].
#[derive(Clone, Copy, Default, PartialEq, Eq)]
pub struct TransportError(u8);

impl TransportError {
    const TIMEOUT: u8 = 1;
    const CONNECT: u8 = 1 << 1;
    const REQUEST: u8 = 1 << 2;
    const BODY: u8 = 1 << 3;
    const DECODE: u8 = 1 << 4;
    const REFUSED: u8 = 1 << 5;

    /// Anything else (a refused redirect, a builder error, a policy refusal).
    #[must_use]
    pub const fn other() -> Self {
        Self(0)
    }

    /// The destination host is not on the egress allowlist, so nothing was
    /// sent. Carries no host: a provider error says which host class it
    /// meant and the guard's caller knows the URL.
    #[must_use]
    pub const fn egress_refused() -> Self {
        Self(Self::REFUSED)
    }

    /// The request or body timed out.
    #[must_use]
    pub const fn timeout() -> Self {
        Self(Self::TIMEOUT)
    }

    /// The connection could not be made (reqwest reports it as a request
    /// error too).
    #[must_use]
    pub const fn connect() -> Self {
        Self(Self::CONNECT | Self::REQUEST)
    }

    /// The request could not be sent.
    #[must_use]
    pub const fn request() -> Self {
        Self(Self::REQUEST)
    }

    /// The body stopped mid-stream.
    #[must_use]
    pub const fn body() -> Self {
        Self(Self::BODY)
    }

    /// The body could not be decoded.
    #[must_use]
    pub const fn decode() -> Self {
        Self(Self::DECODE)
    }

    /// Both classifications.
    #[must_use]
    pub const fn and(self, other: Self) -> Self {
        Self(self.0 | other.0)
    }

    /// `other` when `holds`, else nothing more.
    #[must_use]
    pub const fn and_if(self, holds: bool, other: Self) -> Self {
        if holds { self.and(other) } else { self }
    }

    /// Refused by the egress allowlist before any byte was sent.
    #[must_use]
    pub const fn is_refused(&self) -> bool {
        self.0 & Self::REFUSED != 0
    }

    #[must_use]
    pub const fn is_timeout(&self) -> bool {
        self.0 & Self::TIMEOUT != 0
    }

    #[must_use]
    pub const fn is_connect(&self) -> bool {
        self.0 & Self::CONNECT != 0
    }

    #[must_use]
    pub const fn is_request(&self) -> bool {
        self.0 & Self::REQUEST != 0
    }

    #[must_use]
    pub const fn is_body(&self) -> bool {
        self.0 & Self::BODY != 0
    }

    #[must_use]
    pub const fn is_decode(&self) -> bool {
        self.0 & Self::DECODE != 0
    }
}

impl fmt::Debug for TransportError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("TransportError")
            .field("timeout", &self.is_timeout())
            .field("connect", &self.is_connect())
            .field("request", &self.is_request())
            .field("body", &self.is_body())
            .field("decode", &self.is_decode())
            .field("refused", &self.is_refused())
            .finish()
    }
}

impl fmt::Display for TransportError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(if self.is_refused() {
            "the host is not on the egress allowlist"
        } else if self.is_timeout() {
            "the request timed out"
        } else if self.is_connect() {
            "the connection could not be made"
        } else if self.is_body() {
            "the response body stopped"
        } else if self.is_decode() {
            "the response could not be decoded"
        } else if self.is_request() {
            "the request failed"
        } else {
            "the request was refused"
        })
    }
}

impl std::error::Error for TransportError {}

/// A response body read as it arrives.
#[async_trait]
pub trait ResponseBody: Send {
    /// The next chunk; `None` at the end.
    async fn chunk(&mut self) -> Result<Option<Bytes>, TransportError>;
}

/// A body already in memory (fixtures, and transports that buffer).
struct BufferedBody(Option<Bytes>);

#[async_trait]
impl ResponseBody for BufferedBody {
    async fn chunk(&mut self) -> Result<Option<Bytes>, TransportError> {
        Ok(self.0.take().filter(|bytes| !bytes.is_empty()))
    }
}

/// A response: status and headers now, the body as it streams.
pub struct Response {
    status: StatusCode,
    headers: HeaderMap,
    body: Box<dyn ResponseBody>,
    idle: Option<Duration>,
}

impl fmt::Debug for Response {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("Response")
            .field("status", &self.status)
            .finish_non_exhaustive()
    }
}

impl Response {
    /// A streamed response.
    #[must_use]
    pub fn new(status: StatusCode, headers: HeaderMap, body: Box<dyn ResponseBody>) -> Self {
        Self {
            status,
            headers,
            body,
            idle: None,
        }
    }

    /// Fail a body read that gets no bytes for `idle` with a timeout
    /// ([`TransportError::timeout`] and [`TransportError::body`]), instead
    /// of leaving the body to the client's whole-request deadline: a large
    /// document on a slow link keeps streaming as long as bytes keep coming.
    #[must_use]
    pub fn with_idle_timeout(mut self, idle: Duration) -> Self {
        self.idle = Some(idle);
        self
    }

    /// A response whose whole body is `bytes` (a fixture, or a buffering
    /// transport).
    #[must_use]
    pub fn from_bytes(status: StatusCode, headers: HeaderMap, bytes: impl Into<Bytes>) -> Self {
        Self::new(status, headers, Box::new(BufferedBody(Some(bytes.into()))))
    }

    #[must_use]
    pub fn status(&self) -> StatusCode {
        self.status
    }

    #[must_use]
    pub fn headers(&self) -> &HeaderMap {
        &self.headers
    }

    /// The next body chunk; `None` at the end (`reqwest::Response::chunk`).
    ///
    /// # Errors
    ///
    /// The body stopped or timed out (no bytes for the idle timeout, when
    /// one is set).
    pub async fn chunk(&mut self) -> Result<Option<Bytes>, TransportError> {
        match self.idle {
            None => self.body.chunk().await,
            Some(idle) => tokio::time::timeout(idle, self.body.chunk())
                .await
                .unwrap_or_else(|_| Err(TransportError::timeout().and(TransportError::body()))),
        }
    }

    /// Whether the declared `Content-Length` is over `limit` (a body that
    /// declares none is checked as it streams).
    #[must_use]
    pub fn declares_more_than(&self, limit: usize) -> bool {
        self.headers
            .get(header::CONTENT_LENGTH)
            .and_then(|value| value.to_str().ok())
            .and_then(|value| value.trim().parse::<u64>().ok())
            .is_some_and(|length| u64::try_from(limit).is_ok_and(|limit| length > limit))
    }

    /// The whole body, or `Ok(None)` as soon as it passes `limit` bytes
    /// (nothing past the limit is buffered).
    ///
    /// # Errors
    ///
    /// The body stopped or timed out.
    pub async fn bytes_within(&mut self, limit: usize) -> Result<Option<Vec<u8>>, TransportError> {
        let mut body = Vec::new();
        while let Some(chunk) = self.chunk().await? {
            match body.len().checked_add(chunk.len()) {
                Some(next) if next <= limit => body.extend_from_slice(&chunk),
                _ => return Ok(None),
            }
        }
        Ok(Some(body))
    }
}

/// The one way a provider client reaches the network.
///
/// The production implementations wrap a host-built reqwest client
/// ([`crate::reqwest012`], [`crate::reqwest013`]); tests implement it with
/// recorded fixtures. A transport follows no redirect: a 3xx comes back as a
/// response.
#[async_trait]
pub trait Transport: Send + Sync {
    /// Send `request` and return the status and headers; the body streams.
    ///
    /// # Errors
    ///
    /// No response arrived.
    async fn execute(&self, request: Request) -> Result<Response, TransportError>;
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn a_buffered_body_reads_once_and_is_bounded() {
        let mut headers = HeaderMap::new();
        headers.insert(header::CONTENT_LENGTH, HeaderValue::from_static("5"));
        let mut response = Response::from_bytes(StatusCode::OK, headers, &b"hello"[..]);
        assert!(response.declares_more_than(4));
        assert!(!response.declares_more_than(5));
        assert_eq!(
            response.bytes_within(5).await.ok().flatten(),
            Some(b"hello".to_vec())
        );
        let mut response = Response::from_bytes(StatusCode::OK, HeaderMap::new(), &b"hello"[..]);
        assert_eq!(response.bytes_within(4).await.ok(), Some(None));
        let mut empty =
            Response::from_bytes(StatusCode::NO_CONTENT, HeaderMap::new(), Bytes::new());
        assert_eq!(empty.chunk().await.ok(), Some(None));
    }

    #[test]
    fn a_request_keeps_its_parts_and_hides_credentials() {
        let url = Url::parse("https://example.com/a").expect("url");
        let mut request = Request::new(Method::POST, url);
        let mut secret = HeaderValue::from_static("token s3cret");
        secret.set_sensitive(true);
        request.headers_mut().insert(header::AUTHORIZATION, secret);
        *request.body_mut() = Some(Body::from("{}"));
        *request.timeout_mut() = Some(Duration::from_secs(3));
        assert_eq!(request.body().and_then(Body::as_bytes), Some(&b"{}"[..]));
        assert_eq!(request.timeout(), Some(&Duration::from_secs(3)));
        assert!(!format!("{request:?}").contains("s3cret"));
    }

    #[test]
    fn errors_classify_like_reqwest() {
        assert!(TransportError::timeout().is_timeout());
        let connect = TransportError::connect();
        assert!(connect.is_connect() && connect.is_request() && !connect.is_timeout());
        assert!(TransportError::body().is_body());
        assert_eq!(
            TransportError::other().to_string(),
            "the request was refused"
        );
    }
}
