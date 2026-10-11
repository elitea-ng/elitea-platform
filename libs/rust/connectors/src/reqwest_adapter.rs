//! The reqwest adapters: one macro, expanded once per reqwest line, so the
//! 0.12 adapter (the worker, the desktop host) and the 0.13 adapter (the
//! engines) cannot drift.
//!
//! Neither names a TLS feature. The host's own reqwest dependency picks the
//! stack; a host that links no TLS feature gets a client that cannot reach
//! an `https://` URL, which [`ClientPolicy`]'s `https_only` then refuses.

use std::time::Duration;

/// How a hardened provider client is built. Every field is what the #1207
/// families set on their own `reqwest::Client` builders; `https_only` and "no
/// redirect" are not options.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ClientPolicy {
    pub connect_timeout: Duration,
    /// The default per-request timeout (a request may set its own).
    pub request_timeout: Duration,
    pub pool_idle_timeout: Duration,
    pub max_idle_per_host: usize,
    pub user_agent: &'static str,
    /// Turn reqwest's own protocol-level retries off (`retry::never()`).
    /// The GitHub family never did, and keeps reqwest's default.
    pub disable_retries: bool,
}

/// A hardened client could not be built (no TLS backend, or a bad user
/// agent). Carries nothing from the configuration.
#[derive(Clone, Copy, Debug, Eq, PartialEq, thiserror::Error)]
#[error("the HTTP client could not be built")]
pub struct BuildError;

#[cfg(any(feature = "reqwest-012", feature = "reqwest-013"))]
macro_rules! reqwest_adapter {
    ($reqwest:ident) => {
        use async_trait::async_trait;
        use bytes::Bytes;

        use crate::reqwest_adapter::{BuildError, ClientPolicy};
        use crate::transport::{Request, Response, ResponseBody, Transport, TransportError};

        /// A [`Transport`] over a host-built reqwest client.
        #[derive(Clone, Debug)]
        pub struct ReqwestTransport {
            client: $reqwest::Client,
        }

        impl ReqwestTransport {
            /// Wrap a client the host already built (its TLS roots, its
            /// proxy, its redirect policy are the host's).
            #[must_use]
            pub fn from_client(client: $reqwest::Client) -> Self {
                Self { client }
            }

            /// Build the provider client the #1207 families build: HTTPS
            /// only, no redirect, the policy's timeouts, pool and user agent.
            ///
            /// # Errors
            ///
            /// reqwest could not build the client.
            pub fn hardened(policy: &ClientPolicy) -> Result<Self, BuildError> {
                let mut builder = $reqwest::Client::builder()
                    .https_only(true)
                    .redirect($reqwest::redirect::Policy::none());
                if policy.disable_retries {
                    builder = builder.retry($reqwest::retry::never());
                }
                let client = builder
                    .connect_timeout(policy.connect_timeout)
                    .timeout(policy.request_timeout)
                    .pool_idle_timeout(policy.pool_idle_timeout)
                    .pool_max_idle_per_host(policy.max_idle_per_host)
                    .user_agent(policy.user_agent)
                    .build()
                    .map_err(|_| BuildError)?;
                Ok(Self { client })
            }

            /// The wrapped client.
            #[must_use]
            pub fn client(&self) -> &$reqwest::Client {
                &self.client
            }
        }

        fn classify(source: &$reqwest::Error) -> TransportError {
            TransportError::other()
                .and_if(source.is_timeout(), TransportError::timeout())
                .and_if(source.is_connect(), TransportError::connect())
                .and_if(source.is_request(), TransportError::request())
                .and_if(source.is_body(), TransportError::body())
                .and_if(source.is_decode(), TransportError::decode())
        }

        struct ReqwestBody($reqwest::Response);

        #[async_trait]
        impl ResponseBody for ReqwestBody {
            async fn chunk(&mut self) -> Result<Option<Bytes>, TransportError> {
                self.0.chunk().await.map_err(|source| classify(&source))
            }
        }

        #[async_trait]
        impl Transport for ReqwestTransport {
            async fn execute(&self, request: Request) -> Result<Response, TransportError> {
                let (method, url, headers, body, timeout) = request.into_parts();
                let mut outbound = $reqwest::Request::new(method, url);
                *outbound.headers_mut() = headers;
                *outbound.body_mut() = body.map(|body| body.into_bytes().into());
                *outbound.timeout_mut() = timeout;
                let response = self
                    .client
                    .execute(outbound)
                    .await
                    .map_err(|source| classify(&source))?;
                Ok(Response::new(
                    response.status(),
                    response.headers().clone(),
                    Box::new(ReqwestBody(response)),
                ))
            }
        }

        #[cfg(test)]
        mod tests {
            use super::*;
            use std::time::Duration;

            #[test]
            fn a_hardened_client_builds() {
                let policy = ClientPolicy {
                    connect_timeout: Duration::from_secs(1),
                    request_timeout: Duration::from_secs(1),
                    pool_idle_timeout: Duration::from_secs(1),
                    max_idle_per_host: 1,
                    user_agent: "elitea-connectors-test",
                    disable_retries: true,
                };
                assert!(ReqwestTransport::hardened(&policy).is_ok());
            }

            #[tokio::test]
            async fn a_plain_http_url_is_refused_before_any_connection() {
                let policy = ClientPolicy {
                    connect_timeout: Duration::from_secs(1),
                    request_timeout: Duration::from_secs(1),
                    pool_idle_timeout: Duration::from_secs(1),
                    max_idle_per_host: 1,
                    user_agent: "elitea-connectors-test",
                    disable_retries: false,
                };
                let transport = ReqwestTransport::hardened(&policy).expect("client");
                let url = url::Url::parse("http://127.0.0.1:9/").expect("url");
                let refused = transport
                    .execute(Request::new(http::Method::GET, url))
                    .await;
                assert!(refused.is_err());
            }
        }
    };
}

#[cfg(any(feature = "reqwest-012", feature = "reqwest-013"))]
pub(crate) use reqwest_adapter;
