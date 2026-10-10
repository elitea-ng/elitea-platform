//! The native token endpoint client (ADR-0025 decision 3): authorization-code
//! exchange, rotating refresh, and revoke. Outcomes follow the mobile client's
//! `src/auth/tokenEndpoint.ts`.
//!
//! No redirect is ever followed (a refresh token must not be forwarded to a
//! host the deployment did not name), nothing here is logged, and the token
//! types print as `<redacted>`.

use std::fmt;
use std::time::Duration;

use serde::Deserialize;
use serde_json::Value;

use crate::discovery::Discovery;
use crate::error::HostError;
use crate::net::SharedHttp;

/// `X-Client-Version` on every request: the deployment's 426 gate reads it.
pub const CLIENT_VERSION_HEADER: &str = "X-Client-Version";

/// How long one request to the deployment may take. Deliberately well under the
/// server's refresh re-delivery window (30 s, `nativeauth.DefaultRedeliveryWindow`):
/// a refresh whose answer was lost must be retried while the server will still
/// replay it, or the retry reads as token reuse and revokes the whole family.
pub const REQUEST_TIMEOUT: Duration = Duration::from_secs(10);

#[derive(Clone, Deserialize)]
pub struct TokenSet {
    pub access_token: String,
    pub refresh_token: String,
    pub expires_in: u64,
    #[serde(default)]
    pub device_id: String,
    /// The full `native_client_policy`, delivered with every token response.
    #[serde(default)]
    pub client_policy: Option<Value>,
}

impl fmt::Debug for TokenSet {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("TokenSet")
            .field("access_token", &"<redacted>")
            .field("refresh_token", &"<redacted>")
            .field("expires_in", &self.expires_in)
            .field("device_id", &self.device_id)
            .finish_non_exhaustive()
    }
}

#[derive(Debug)]
pub enum TokenOutcome {
    Ok(TokenSet),
    /// The device session is revoked, expired or deactivated: wipe.
    DeviceRevoked,
    /// The code or refresh token is invalid, expired or already used.
    InvalidGrant,
    /// This app is not (or no longer) a registered, enabled client.
    InvalidClient,
    /// 426: the app is older than the deployment's minimum.
    UpgradeRequired,
    /// 429 / 5xx: nothing was consumed; try again later.
    Unavailable,
    /// No HTTP answer: the request may or may not have arrived. A refresh
    /// token must be kept and retried (the server re-delivers within its grace window).
    Network,
    Failed,
}

pub struct CodeExchange<'a> {
    pub code: &'a str,
    pub verifier: &'a str,
    pub redirect_uri: &'a str,
    pub client_id: &'a str,
}

#[derive(Clone)]
pub struct TokenEndpoint {
    http: SharedHttp,
    timeout: Duration,
    client_version: String,
}

impl TokenEndpoint {
    /// An endpoint with its own client (tests; the app shares one, [`Self::shared`]).
    #[cfg(test)]
    pub fn new(client_version: &str) -> Result<Self, reqwest::Error> {
        Self::with_timeout(client_version, REQUEST_TIMEOUT)
    }

    #[cfg(test)]
    pub fn with_timeout(client_version: &str, timeout: Duration) -> Result<Self, reqwest::Error> {
        Ok(Self {
            http: SharedHttp::new(client_version),
            timeout,
            client_version: client_version.to_owned(),
        })
    }

    /// On the app's one client (src/net.rs): no redirects, its User-Agent,
    /// and [`REQUEST_TIMEOUT`] per request.
    #[must_use]
    pub fn shared(http: SharedHttp, client_version: &str) -> Self {
        Self {
            http,
            timeout: REQUEST_TIMEOUT,
            client_version: client_version.to_owned(),
        }
    }

    /// The deployment's discovery document, on the same client and deadline.
    pub async fn discovery(&self, origin: &url::Url) -> Result<Discovery, HostError> {
        crate::discovery::fetch_discovery(self.http.client().await, self.timeout, origin).await
    }

    pub fn client_version(&self) -> &str {
        &self.client_version
    }

    pub async fn exchange_code(&self, endpoint: &str, request: &CodeExchange<'_>) -> TokenOutcome {
        self.post_token(
            endpoint,
            &[
                ("grant_type", "authorization_code"),
                ("code", request.code),
                ("redirect_uri", request.redirect_uri),
                ("client_id", request.client_id),
                ("code_verifier", request.verifier),
            ],
        )
        .await
    }

    /// The server rotates the refresh token on every success.
    pub async fn refresh(
        &self,
        endpoint: &str,
        refresh_token: &str,
        client_id: &str,
    ) -> TokenOutcome {
        self.post_token(
            endpoint,
            &[
                ("grant_type", "refresh_token"),
                ("refresh_token", refresh_token),
                ("client_id", client_id),
                ("client_version", &self.client_version),
            ],
        )
        .await
    }

    /// RFC 7009 revoke of the whole device session. True on 200.
    pub async fn revoke(&self, endpoint: &str, refresh_token: &str, client_id: &str) -> bool {
        let sent = self
            .http
            .client()
            .await
            .post(endpoint)
            .timeout(self.timeout)
            .header(CLIENT_VERSION_HEADER, &self.client_version)
            .form(&[
                ("token", refresh_token),
                ("token_type_hint", "refresh_token"),
                ("client_id", client_id),
            ])
            .send()
            .await;
        sent.is_ok_and(|r| r.status().as_u16() == 200)
    }

    async fn post_token(&self, endpoint: &str, form: &[(&str, &str)]) -> TokenOutcome {
        let Ok(response) = self
            .http
            .client()
            .await
            .post(endpoint)
            .timeout(self.timeout)
            .header(CLIENT_VERSION_HEADER, &self.client_version)
            .form(form)
            .send()
            .await
        else {
            return TokenOutcome::Network;
        };
        let status = response.status().as_u16();
        let body: Option<Value> = response.json().await.ok();
        classify(status, body)
    }
}

/// Map a token endpoint answer to an outcome.
fn classify(status: u16, body: Option<Value>) -> TokenOutcome {
    if status == 200 {
        return body
            .and_then(|b| serde_json::from_value::<TokenSet>(b).ok())
            .filter(|t| !t.access_token.is_empty() && !t.refresh_token.is_empty())
            .map_or(TokenOutcome::Failed, TokenOutcome::Ok);
    }
    let error = body
        .as_ref()
        .and_then(|b| b.get("error"))
        .and_then(Value::as_str);
    match (status, error) {
        (401, Some("device_revoked")) => TokenOutcome::DeviceRevoked,
        (401, Some("invalid_client")) | (404, Some("not_found")) => TokenOutcome::InvalidClient,
        (400, Some("invalid_grant")) => TokenOutcome::InvalidGrant,
        (426, _) => TokenOutcome::UpgradeRequired,
        (429 | 500..=599, _) => TokenOutcome::Unavailable,
        _ => TokenOutcome::Failed,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testutil::{Res, serve};
    use serde_json::json;

    fn token_json() -> Value {
        json!({
            "access_token": "access-token-0123456789",
            "token_type": "bearer",
            "expires_in": 900,
            "refresh_token": "refresh-token-0123456789",
            "refresh_token_expires_in": 2_592_000,
            "device_id": "dev-1",
            "client_policy": {"idle_lock_seconds": 300}
        })
    }

    #[test]
    fn classifies_every_answer_the_mobile_client_distinguishes() {
        assert!(matches!(
            classify(200, Some(token_json())),
            TokenOutcome::Ok(_)
        ));
        assert!(matches!(
            classify(
                200,
                Some(json!({"access_token": "", "refresh_token": "x", "expires_in": 1}))
            ),
            TokenOutcome::Failed
        ));
        assert!(matches!(classify(200, None), TokenOutcome::Failed));
        assert!(matches!(
            classify(401, Some(json!({"error": "device_revoked"}))),
            TokenOutcome::DeviceRevoked
        ));
        assert!(matches!(
            classify(401, Some(json!({"error": "invalid_client"}))),
            TokenOutcome::InvalidClient
        ));
        assert!(matches!(
            classify(404, Some(json!({"error": "not_found"}))),
            TokenOutcome::InvalidClient
        ));
        assert!(matches!(
            classify(400, Some(json!({"error": "invalid_grant"}))),
            TokenOutcome::InvalidGrant
        ));
        assert!(matches!(classify(426, None), TokenOutcome::UpgradeRequired));
        assert!(matches!(classify(429, None), TokenOutcome::Unavailable));
        assert!(matches!(classify(503, None), TokenOutcome::Unavailable));
        assert!(matches!(
            classify(400, Some(json!({"error": "invalid_request"}))),
            TokenOutcome::Failed
        ));
    }

    #[test]
    fn token_set_debug_is_redacted() {
        let TokenOutcome::Ok(tokens) = classify(200, Some(token_json())) else {
            panic!("ok")
        };
        let printed = format!("{tokens:?}");
        assert!(!printed.contains("access-token-0123456789"));
        assert!(!printed.contains("refresh-token-0123456789"));
    }

    #[tokio::test]
    async fn code_exchange_posts_the_pkce_form_with_the_client_version() {
        let server = serve(|_| Res::json(200, &token_json())).await;
        let endpoint = TokenEndpoint::new("0.1.0").unwrap();
        let outcome = endpoint
            .exchange_code(
                &format!("{}/token", server.origin),
                &CodeExchange {
                    code: "the-code",
                    verifier: "the-verifier",
                    redirect_uri: "http://127.0.0.1:5555/callback",
                    client_id: "desktop",
                },
            )
            .await;
        let TokenOutcome::Ok(tokens) = outcome else {
            panic!("expected ok, got {outcome:?}")
        };
        assert_eq!(tokens.expires_in, 900);
        assert_eq!(tokens.device_id, "dev-1");
        assert!(tokens.client_policy.is_some());

        let seen = server.seen();
        assert_eq!(seen[0].method, "POST");
        assert_eq!(seen[0].headers["x-client-version"], "0.1.0");
        assert_eq!(
            seen[0].headers["content-type"],
            "application/x-www-form-urlencoded"
        );
        let form = seen[0].form();
        assert_eq!(form["grant_type"], "authorization_code");
        assert_eq!(form["code"], "the-code");
        assert_eq!(form["code_verifier"], "the-verifier");
        assert_eq!(form["redirect_uri"], "http://127.0.0.1:5555/callback");
        assert_eq!(form["client_id"], "desktop");
    }

    #[tokio::test]
    async fn refresh_sends_the_refresh_grant_and_never_follows_a_redirect() {
        let target = serve(|_| Res::json(200, &token_json())).await;
        let to = format!("{}/stolen", target.origin);
        let redirector = serve(move |_| Res {
            status: 307,
            headers: vec![("Location", to.clone())],
            body: String::new(),
        })
        .await;
        let endpoint = TokenEndpoint::new("0.1.0").unwrap();
        let outcome = endpoint
            .refresh(
                &format!("{}/token", redirector.origin),
                "old-refresh",
                "desktop",
            )
            .await;
        assert!(matches!(outcome, TokenOutcome::Failed), "{outcome:?}");
        assert!(
            target.seen().is_empty(),
            "the refresh token must not be forwarded"
        );
        let form = redirector.seen()[0].form();
        assert_eq!(form["grant_type"], "refresh_token");
        assert_eq!(form["refresh_token"], "old-refresh");
        assert_eq!(form["client_version"], "0.1.0");
    }

    #[test]
    fn the_request_timeout_leaves_room_for_a_retry_inside_the_redelivery_window() {
        // The server replays a lost rotation for 30 s; a timeout plus one immediate
        // retry must fit inside it.
        assert!(REQUEST_TIMEOUT * 2 < Duration::from_secs(30));
    }

    #[tokio::test]
    async fn a_stalled_server_ends_in_a_network_outcome_at_the_timeout() {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let port = listener.local_addr().unwrap().port();
        tokio::spawn(async move {
            // Accept and never answer.
            let _held = listener.accept().await;
            tokio::time::sleep(Duration::from_secs(30)).await;
        });
        let endpoint = TokenEndpoint::with_timeout("0.1.0", Duration::from_millis(200)).unwrap();
        let outcome = endpoint
            .refresh(&format!("http://127.0.0.1:{port}/token"), "r", "desktop")
            .await;
        assert!(matches!(outcome, TokenOutcome::Network));
    }

    #[tokio::test]
    async fn an_unreachable_server_is_a_network_outcome() {
        let endpoint = TokenEndpoint::new("0.1.0").unwrap();
        // Port 9 (discard) on loopback: nothing listens in CI or locally.
        let outcome = endpoint
            .refresh("http://127.0.0.1:9/token", "r", "desktop")
            .await;
        assert!(matches!(outcome, TokenOutcome::Network));
    }

    #[tokio::test]
    async fn revoke_is_true_on_200_only() {
        let ok = serve(|_| Res::json(200, &json!({}))).await;
        let bad = serve(|_| Res::json(500, &json!({}))).await;
        let endpoint = TokenEndpoint::new("0.1.0").unwrap();
        assert!(
            endpoint
                .revoke(&format!("{}/revoke", ok.origin), "r", "desktop")
                .await
        );
        assert!(
            !endpoint
                .revoke(&format!("{}/revoke", bad.origin), "r", "desktop")
                .await
        );
        let form = ok.seen()[0].form();
        assert_eq!(form["token_type_hint"], "refresh_token");
    }
}
