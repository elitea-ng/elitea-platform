//! The one request/response flow every non-chat `/llm` route shares
//! (embeddings, images): the caller-contract headers, one total deadline from
//! the first byte sent to the last byte read, a bounded body, and the
//! gateway's refusal classified from the status.
//!
//! Nothing here adds a transport: callers pass the model facade's
//! origin-bound mTLS channel.

use std::fmt;
use std::sync::Arc;
use std::time::Duration;

use bytes::Bytes;
use elitea_llm_wire::headers::{BEARER_PREFIX, EXECUTION_HEADER, PROJECT_HEADER};
use elitea_llm_wire::refusal::Refusal;
use http::header::{ACCEPT, AUTHORIZATION, CONTENT_LENGTH, CONTENT_TYPE};
use http::{HeaderName, HeaderValue, Method, Request, Version};
use http_body_util::{BodyExt as _, Full};
use tokio::time::timeout;
use tonic::body::Body;
use zeroize::Zeroizing;

use super::openai_compatible_facade::ModelGatewayTransport;

type BoxError = Box<dyn std::error::Error + Send + Sync + 'static>;

const PROJECT_SELECTOR: HeaderName = HeaderName::from_static(PROJECT_HEADER);
const EXECUTION_ID_HEADER: HeaderName = HeaderName::from_static(EXECUTION_HEADER);
/// How much of a refusal's body is read, to learn a 402's budget scope.
const MAX_REFUSAL_BYTES: usize = 64 * 1024;

/// Why a gateway call produced no usable body. Carries no data.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum CallError {
    /// The gateway refused the call.
    Refused(Refusal),
    /// The channel could not carry the request or the response, or the
    /// deadline passed.
    Transport,
    /// The response is over its bound.
    TooLarge,
    /// The request could not be built (a header value is unusable).
    Malformed,
}

impl fmt::Display for CallError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Refused(refusal) => {
                write!(formatter, "the model gateway refused: {}", refusal.code())
            }
            Self::Transport => formatter.write_str("the model gateway transport failed"),
            Self::TooLarge => formatter.write_str("the model gateway response exceeds its bound"),
            Self::Malformed => formatter.write_str("the model gateway request is malformed"),
        }
    }
}

/// A POST to `route` carrying `body`, under the claim's credential, project
/// and execution.
pub(crate) fn build_request(
    route: &'static str,
    content_type: &str,
    body: Bytes,
    billing_project_id: u64,
    token: &Zeroizing<String>,
    execution_id: &str,
) -> Result<Request<Body>, CallError> {
    let malformed = |_| CallError::Malformed;
    let body_length = body.len();
    let mut request = Request::builder()
        .method(Method::POST)
        .uri(route)
        .version(Version::HTTP_2)
        .body(Body::new(Full::new(body)))
        .map_err(|_| CallError::Malformed)?;
    let headers = request.headers_mut();
    headers.insert(
        CONTENT_TYPE,
        HeaderValue::from_str(content_type).map_err(malformed)?,
    );
    headers.insert(ACCEPT, HeaderValue::from_static("application/json"));
    headers.insert(
        CONTENT_LENGTH,
        HeaderValue::from_str(&body_length.to_string()).map_err(malformed)?,
    );
    headers.insert(
        PROJECT_SELECTOR,
        HeaderValue::from_str(&billing_project_id.to_string()).map_err(malformed)?,
    );
    headers.insert(
        EXECUTION_ID_HEADER,
        HeaderValue::from_str(execution_id).map_err(malformed)?,
    );
    let mut bearer = Zeroizing::new(String::with_capacity(BEARER_PREFIX.len() + token.len()));
    bearer.push_str(BEARER_PREFIX);
    bearer.push_str(token);
    let mut authorization = HeaderValue::from_str(bearer.as_str()).map_err(malformed)?;
    authorization.set_sensitive(true);
    headers.insert(AUTHORIZATION, authorization);
    Ok(request)
}

/// Send `request` and read the whole success body, all within `deadline`
/// (headers and body together: a gateway that answers its headers and then
/// stalls is cut off at the same moment as one that never answers).
///
/// # Errors
///
/// A refusal (classified from the status, with a 402's scope read from a
/// bounded body), a transport failure or deadline, or a body over
/// `max_response`.
pub(crate) async fn send(
    transport: &Arc<dyn ModelGatewayTransport>,
    request: Request<Body>,
    deadline: Duration,
    max_response: usize,
) -> Result<Bytes, CallError> {
    timeout(deadline, async {
        let response = transport
            .post(request)
            .await
            .map_err(|_| CallError::Transport)?;
        let status = response.status().as_u16();
        if (200..300).contains(&status) {
            let limited = http_body_util::Limited::new(response.into_body(), max_response);
            return match limited.collect().await {
                Ok(collected) => Ok(collected.to_bytes()),
                Err(error) => Err(classify_body_error(&error)),
            };
        }
        // Best effort: the scope of a 402 is in the body, nothing else is.
        let limited = http_body_util::Limited::new(response.into_body(), MAX_REFUSAL_BYTES);
        let body = limited
            .collect()
            .await
            .map(http_body_util::Collected::to_bytes)
            .unwrap_or_default();
        Err(CallError::Refused(
            Refusal::classify(status, &body).unwrap_or(Refusal::Rejected),
        ))
    })
    .await
    .map_err(|_| CallError::Transport)?
}

/// Only the bound's own refusal (`Limited` stopped at the maximum) is
/// `TooLarge`; every other body error (the stream reset, the connection
/// dropped mid-body) is the transport's, and another attempt may succeed.
fn classify_body_error(error: &BoxError) -> CallError {
    if error.is::<http_body_util::LengthLimitError>() {
        CallError::TooLarge
    } else {
        CallError::Transport
    }
}

#[cfg(test)]
mod tests {
    use async_trait::async_trait;
    use http::Response;

    use super::*;
    use crate::transport::openai_compatible_facade::ModelGatewayTransportError;

    const ROUTE: &str = "/llm/v1/embeddings";

    fn request() -> Request<Body> {
        let token = Zeroizing::new("pat".to_owned());
        build_request(
            ROUTE,
            "application/json",
            Bytes::from_static(b"{}"),
            7,
            &token,
            "exec-1",
        )
        .expect("ok")
    }

    #[test]
    fn the_request_carries_the_caller_contract_headers() {
        let request = request();
        assert_eq!(request.uri().path(), ROUTE);
        assert_eq!(request.version(), Version::HTTP_2);
        let headers = request.headers();
        assert_eq!(headers.get(PROJECT_SELECTOR).unwrap(), "7");
        assert_eq!(headers.get(EXECUTION_ID_HEADER).unwrap(), "exec-1");
        assert_eq!(headers.get(AUTHORIZATION).unwrap(), "Bearer pat");
        assert!(headers.get(AUTHORIZATION).unwrap().is_sensitive());
        assert_eq!(headers.get(ACCEPT).unwrap(), "application/json");
        assert_eq!(headers.get(CONTENT_TYPE).unwrap(), "application/json");
    }

    /// Only the bound's own refusal is `TooLarge` (the document is skipped);
    /// a body that fails for any other reason is the transport's, retryable.
    #[tokio::test]
    async fn only_the_length_bound_makes_a_body_error_too_large() {
        let over = http_body_util::Limited::new(Full::new(Bytes::from(vec![0_u8; 64])), 16)
            .collect()
            .await
            .expect_err("over the bound");
        assert_eq!(classify_body_error(&over), CallError::TooLarge);
        let reset: BoxError = Box::new(std::io::Error::new(
            std::io::ErrorKind::ConnectionReset,
            "stream reset",
        ));
        assert_eq!(classify_body_error(&reset), CallError::Transport);
    }

    /// Answers the headers after `header_delay`, then the body after
    /// `body_delay`.
    struct Slow {
        header_delay: Duration,
        body_delay: Duration,
        status: u16,
    }

    #[async_trait]
    impl ModelGatewayTransport for Slow {
        async fn post(
            &self,
            _: Request<Body>,
        ) -> Result<Response<Body>, ModelGatewayTransportError> {
            tokio::time::sleep(self.header_delay).await;
            let delay = self.body_delay;
            let frames = adk_rust::futures::stream::once(async move {
                tokio::time::sleep(delay).await;
                Ok::<_, std::io::Error>(http_body::Frame::data(Bytes::from_static(b"{}")))
            });
            Ok(Response::builder()
                .status(self.status)
                .body(Body::new(http_body_util::StreamBody::new(frames)))
                .expect("response"))
        }
    }

    #[tokio::test]
    async fn one_deadline_spans_the_headers_and_the_body() {
        let slow = |header_ms, body_ms| -> Arc<dyn ModelGatewayTransport> {
            Arc::new(Slow {
                header_delay: Duration::from_millis(header_ms),
                body_delay: Duration::from_millis(body_ms),
                status: 200,
            })
        };
        let deadline = Duration::from_millis(150);
        // Each phase alone is inside the deadline; together they are not.
        let outcome = send(&slow(100, 100), request(), deadline, 1024).await;
        assert_eq!(outcome, Err(CallError::Transport));
        // Within the deadline overall it succeeds.
        let body = send(&slow(20, 20), request(), deadline, 1024)
            .await
            .expect("in time");
        assert_eq!(&body[..], b"{}");
    }

    #[tokio::test]
    async fn a_refusal_is_classified_from_the_status() {
        let refused: Arc<dyn ModelGatewayTransport> = Arc::new(Slow {
            header_delay: Duration::ZERO,
            body_delay: Duration::ZERO,
            status: 429,
        });
        assert_eq!(
            send(&refused, request(), Duration::from_secs(1), 1024).await,
            Err(CallError::Refused(Refusal::RateLimited))
        );
    }
}
