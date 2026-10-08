//! Sign-in, token custody and sign-out (ADR-0025 decision 3, ADR-0029
//! decision 9).
//!
//! The flow is RFC 8252 for a native public client: the system browser (never
//! an embedded web view) opens the deployment's authorize URL with a PKCE S256
//! challenge and a `state`; the deployment redirects to
//! `http://127.0.0.1:<ephemeral port>/callback`, a one-shot listener in this
//! process; the code is exchanged with the verifier. The refresh token goes to
//! the OS keychain; the webview only ever receives a short-lived access token.

use std::sync::Arc;
use std::time::{Duration, Instant};

use serde::Serialize;
use serde_json::Value;
use tokio::sync::Mutex;
use url::Url;

use crate::discovery;
use crate::error::HostError;
use crate::loopback::LoopbackListener;
use crate::pkce;
use crate::settings::{Settings, SettingsFiles, resolve_client_id};
use crate::store::{SecretStore, StoredSession, load_session, save_session};
use crate::tokens::{CodeExchange, TokenEndpoint, TokenOutcome, TokenSet};

/// How long the person has to finish signing in in the browser.
const SIGN_IN_DEADLINE: Duration = Duration::from_secs(5 * 60);
/// A cached access token is renewed once it has less than this left.
const ACCESS_TOKEN_SKEW: Duration = Duration::from_secs(30);

/// Opens a URL in the system browser. A trait so tests can play the browser.
pub trait BrowserOpener: Send + Sync {
    fn open(&self, url: &str) -> Result<(), HostError>;
}

#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct DeploymentInfo {
    pub origin: String,
    pub display_name: String,
    pub deployment_kind: String,
}

/// What the webview learns about this install. Mirrors `HostState` in
/// `apps/elitea-web/src/shared/desktop/hostBridge.ts`.
#[derive(Debug, Clone, Serialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct HostState {
    pub configured: bool,
    pub origin: Option<String>,
    pub display_name: Option<String>,
    pub signed_in: bool,
    pub client_version: String,
    pub client_id: String,
    pub policy: Option<Value>,
}

#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct AccessToken {
    pub token: String,
    pub expires_in: u64,
}

/// How a refresh ended, for the webview's transport (see `RefreshOutcome` in
/// `nativeTransport.ts`).
#[derive(Debug, Clone, Copy, Serialize, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum RefreshResult {
    Refreshed,
    /// The server refused the refresh token: the session is over.
    Ended,
    /// Network down or server busy: keep the session, try again later.
    Unavailable,
}

struct Cached {
    token: String,
    expires_at: Instant,
}

pub struct AuthService {
    store: Arc<dyn SecretStore>,
    files: SettingsFiles,
    tokens: TokenEndpoint,
    opener: Arc<dyn BrowserOpener>,
    build_client_id: Option<&'static str>,
    runtime_client_id: Option<String>,
    cached: Mutex<Option<Cached>>,
    /// Serialises refreshes: refresh tokens rotate, so two at once would burn the family.
    refresh_gate: Mutex<()>,
    sign_in_deadline: Duration,
}

pub struct AuthConfig {
    pub store: Arc<dyn SecretStore>,
    pub files: SettingsFiles,
    pub tokens: TokenEndpoint,
    pub opener: Arc<dyn BrowserOpener>,
    pub build_client_id: Option<&'static str>,
    pub runtime_client_id: Option<String>,
}

impl AuthService {
    pub fn new(config: AuthConfig) -> Self {
        Self {
            store: config.store,
            files: config.files,
            tokens: config.tokens,
            opener: config.opener,
            build_client_id: config.build_client_id,
            runtime_client_id: config.runtime_client_id,
            cached: Mutex::new(None),
            refresh_gate: Mutex::new(()),
            sign_in_deadline: SIGN_IN_DEADLINE,
        }
    }

    #[cfg(test)]
    fn with_deadline(mut self, deadline: Duration) -> Self {
        self.sign_in_deadline = deadline;
        self
    }

    fn client_id(&self, settings: &Settings) -> String {
        resolve_client_id(
            self.runtime_client_id.as_deref(),
            settings.client_id.as_deref(),
            self.build_client_id,
        )
    }

    // ---- state ---------------------------------------------------------

    pub fn state(&self) -> Result<HostState, HostError> {
        let settings = self.files.settings()?;
        let session = load_session(self.store.as_ref())?;
        // A session only counts for the deployment it was issued by.
        let signed_in =
            matches!((&session, &settings.origin), (Some(s), Some(o)) if &s.origin == o);
        Ok(HostState {
            configured: settings.origin.is_some(),
            client_id: self.client_id(&settings),
            origin: settings.origin,
            display_name: settings.display_name,
            signed_in,
            client_version: self.tokens.client_version().to_owned(),
            policy: if signed_in {
                self.files.policy()?
            } else {
                None
            },
        })
    }

    // ---- connect -------------------------------------------------------

    /// Normalise the address, fetch and validate discovery, remember the
    /// deployment. Choosing a different deployment ends any session on the old one.
    pub async fn connect(&self, input: &str) -> Result<DeploymentInfo, HostError> {
        let origin = discovery::normalize_origin(input)?;
        let document = discovery::fetch_discovery(self.tokens.http(), &origin).await?;
        let origin_text = origin_string(&origin);

        let mut settings = self.files.settings()?;
        if settings
            .origin
            .as_deref()
            .is_some_and(|previous| previous != origin_text)
        {
            self.sign_out().await?;
        }
        settings.origin = Some(origin_text.clone());
        settings.display_name = Some(document.display_name.clone());
        self.files.save_settings(&settings)?;
        Ok(DeploymentInfo {
            origin: origin_text,
            display_name: document.display_name,
            deployment_kind: document.deployment_kind,
        })
    }

    // ---- sign in -------------------------------------------------------

    pub async fn sign_in(&self) -> Result<HostState, HostError> {
        let settings = self.files.settings()?;
        let origin_text = settings.origin.clone().ok_or(HostError::NotConnected)?;
        let origin = Url::parse(&origin_text).map_err(|e| HostError::Internal(e.to_string()))?;
        // Fresh discovery, not a cached one: endpoints are validated against the
        // origin the person confirmed, every time a credential is about to move.
        let document = discovery::fetch_discovery(self.tokens.http(), &origin).await?;
        let auth = document.native_auth()?.clone();
        let client_id = self.client_id(&settings);

        let pkce = pkce::create_pkce();
        let state = pkce::create_state();
        let listener = LoopbackListener::bind().await?;
        let redirect_uri = listener.redirect_uri();

        let url = authorize_url(
            &auth.authorization_endpoint,
            &AuthorizeParams {
                client_id: &client_id,
                redirect_uri: &redirect_uri,
                challenge: &pkce.challenge,
                state: &state,
                device_name: &device_name(),
                platform: platform(),
                client_version: self.tokens.client_version(),
            },
        )?;
        self.opener.open(&url)?;

        let params = listener.wait_for_callback(self.sign_in_deadline).await?;
        let code = verify_callback(&params, &state, &auth.issuer)?;

        let outcome = self
            .tokens
            .exchange_code(
                &auth.token_endpoint,
                &CodeExchange {
                    code: &code,
                    verifier: &pkce.verifier,
                    redirect_uri: &redirect_uri,
                    client_id: &client_id,
                },
            )
            .await;
        match outcome {
            TokenOutcome::Ok(tokens) => {
                self.adopt(&origin_text, &client_id, tokens, None).await?;
                self.state()
            }
            other => Err(map_exchange_failure(&other, &client_id)),
        }
    }

    /// Persist a token response: the rotated refresh token first (so a crash
    /// after the exchange never loses the session), then the access token.
    async fn adopt(
        &self,
        origin: &str,
        client_id: &str,
        tokens: TokenSet,
        previous: Option<&StoredSession>,
    ) -> Result<(), HostError> {
        let device_id = if tokens.device_id.is_empty() {
            previous.map(|p| p.device_id.clone()).unwrap_or_default()
        } else {
            tokens.device_id.clone()
        };
        save_session(
            self.store.as_ref(),
            &StoredSession {
                origin: origin.to_owned(),
                client_id: client_id.to_owned(),
                device_id,
                refresh_token: tokens.refresh_token.clone(),
            },
        )?;
        if let Some(policy) = &tokens.client_policy {
            self.files.save_policy(policy)?;
        }
        *self.cached.lock().await = Some(Cached {
            token: tokens.access_token,
            expires_at: Instant::now() + Duration::from_secs(tokens.expires_in),
        });
        Ok(())
    }

    // ---- access token --------------------------------------------------

    /// A usable access token, refreshing first when the cached one is near its
    /// end. `Ok(None)` means there is no session.
    pub async fn access_token(&self) -> Result<Option<AccessToken>, HostError> {
        if let Some(token) = self.cached_token().await {
            return Ok(Some(token));
        }
        match self.refresh_inner(None).await? {
            RefreshResult::Refreshed => Ok(self.cached_token().await),
            RefreshResult::Ended => Ok(None),
            RefreshResult::Unavailable => Err(HostError::Unavailable),
        }
    }

    async fn cached_token(&self) -> Option<AccessToken> {
        let cached = self.cached.lock().await;
        let entry = cached.as_ref()?;
        let left = entry.expires_at.checked_duration_since(Instant::now())?;
        (left > ACCESS_TOKEN_SKEW).then(|| AccessToken {
            token: entry.token.clone(),
            expires_in: left.as_secs(),
        })
    }

    /// Exchange the refresh token now (the webview saw a 401).
    pub async fn refresh(&self) -> Result<RefreshResult, HostError> {
        let stale = self.cached.lock().await.as_ref().map(|c| c.token.clone());
        self.refresh_inner(stale).await
    }

    async fn refresh_inner(&self, stale: Option<String>) -> Result<RefreshResult, HostError> {
        let _gate = self.refresh_gate.lock().await;
        // A refresh that finished while this call waited already replaced the
        // token the caller saw fail: do not rotate the refresh token twice.
        if let Some(stale) = stale {
            let current = self.cached.lock().await.as_ref().map(|c| c.token.clone());
            if current.is_some_and(|c| c != stale) && self.cached_token().await.is_some() {
                return Ok(RefreshResult::Refreshed);
            }
        }
        let settings = self.files.settings()?;
        let Some(session) = load_session(self.store.as_ref())? else {
            return Ok(RefreshResult::Ended);
        };
        if settings.origin.as_deref() != Some(session.origin.as_str()) {
            return Ok(RefreshResult::Ended);
        }
        let origin = Url::parse(&session.origin).map_err(|e| HostError::Internal(e.to_string()))?;
        let document = match discovery::fetch_discovery(self.tokens.http(), &origin).await {
            Ok(document) => document,
            Err(HostError::Unreachable | HostError::Unavailable) => {
                return Ok(RefreshResult::Unavailable);
            }
            Err(e) => return Err(e),
        };
        let auth = document.native_auth()?;

        match self
            .tokens
            .refresh(
                &auth.token_endpoint,
                &session.refresh_token,
                &session.client_id,
            )
            .await
        {
            TokenOutcome::Ok(tokens) => {
                self.adopt(&session.origin, &session.client_id, tokens, Some(&session))
                    .await?;
                Ok(RefreshResult::Refreshed)
            }
            // The session is gone for good: forget it.
            TokenOutcome::DeviceRevoked
            | TokenOutcome::InvalidGrant
            | TokenOutcome::InvalidClient => {
                self.wipe().await?;
                Ok(RefreshResult::Ended)
            }
            TokenOutcome::UpgradeRequired => Err(HostError::UpgradeRequired),
            // Nothing was consumed, or the response was lost: keep the token and retry later.
            TokenOutcome::Unavailable | TokenOutcome::Network | TokenOutcome::Failed => {
                Ok(RefreshResult::Unavailable)
            }
        }
    }

    // ---- sign out ------------------------------------------------------

    /// Revoke the device session on the server (best effort), then forget it.
    pub async fn sign_out(&self) -> Result<(), HostError> {
        if let Some(session) = load_session(self.store.as_ref())?
            && let Ok(origin) = Url::parse(&session.origin)
            && let Ok(document) = discovery::fetch_discovery(self.tokens.http(), &origin).await
            && let Ok(auth) = document.native_auth()
        {
            self.tokens
                .revoke(
                    &auth.revocation_endpoint,
                    &session.refresh_token,
                    &session.client_id,
                )
                .await;
        }
        self.wipe().await
    }

    /// Forget the session and everything derived from it, without a server call.
    /// The connected deployment is kept so the person only has to sign in again.
    pub async fn wipe(&self) -> Result<(), HostError> {
        *self.cached.lock().await = None;
        self.store.clear()?;
        self.files.clear_policy()
    }
}

fn origin_string(origin: &Url) -> String {
    origin.as_str().trim_end_matches('/').to_owned()
}

struct AuthorizeParams<'a> {
    client_id: &'a str,
    redirect_uri: &'a str,
    challenge: &'a str,
    state: &'a str,
    device_name: &'a str,
    platform: &'a str,
    client_version: &'a str,
}

fn authorize_url(endpoint: &str, p: &AuthorizeParams<'_>) -> Result<String, HostError> {
    let mut url = Url::parse(endpoint).map_err(|e| HostError::Internal(e.to_string()))?;
    url.query_pairs_mut()
        .append_pair("response_type", "code")
        .append_pair("client_id", p.client_id)
        .append_pair("redirect_uri", p.redirect_uri)
        .append_pair("code_challenge", p.challenge)
        .append_pair("code_challenge_method", "S256")
        .append_pair("state", p.state)
        .append_pair("device_name", p.device_name)
        .append_pair("platform", p.platform)
        .append_pair("client_version", p.client_version);
    Ok(url.into())
}

/// Validate an authorization response for THIS attempt and return its code.
/// Order matters: the `state` first (anything else is not our response), then
/// the RFC 9207 `iss`, then an error from the server, then the code.
fn verify_callback(
    params: &std::collections::HashMap<String, String>,
    state: &str,
    issuer: &str,
) -> Result<String, HostError> {
    let got_state = params.get("state").ok_or(HostError::SignInRejected)?;
    if !pkce::constant_time_eq(got_state, state) {
        return Err(HostError::SignInRejected);
    }
    // The deployment always sends `iss`; a missing or different one means the
    // response is not from the server this attempt was started with.
    let same_issuer = params
        .get("iss")
        .is_some_and(|iss| iss.trim_end_matches('/') == issuer.trim_end_matches('/'));
    if !same_issuer {
        return Err(HostError::SignInRejected);
    }
    if let Some(error) = params.get("error") {
        return Err(if error == "access_denied" {
            HostError::SignInDenied
        } else {
            HostError::SignInRejected
        });
    }
    match params.get("code") {
        Some(code) if !code.is_empty() && code.len() <= 1024 => Ok(code.clone()),
        _ => Err(HostError::SignInRejected),
    }
}

fn map_exchange_failure(outcome: &TokenOutcome, client_id: &str) -> HostError {
    match outcome {
        TokenOutcome::InvalidClient => HostError::ClientNotRegistered(client_id.to_owned()),
        TokenOutcome::UpgradeRequired => HostError::UpgradeRequired,
        TokenOutcome::DeviceRevoked => HostError::DeviceRevoked,
        TokenOutcome::Unavailable => HostError::Unavailable,
        TokenOutcome::Network => HostError::Unreachable,
        TokenOutcome::InvalidGrant | TokenOutcome::Failed | TokenOutcome::Ok(_) => {
            HostError::SignInRejected
        }
    }
}

/// What the app tells the deployment about this device: coarse on purpose
/// (the server shows it in the device list), no hardware identifiers.
fn device_name() -> String {
    let os = match std::env::consts::OS {
        "macos" => "macOS",
        "windows" => "Windows",
        "linux" => "Linux",
        other => other,
    };
    format!("Elitea Desktop on {os}")
}

fn platform() -> &'static str {
    match std::env::consts::OS {
        "macos" => "macos",
        "windows" => "windows",
        _ => "other",
    }
}

#[cfg(test)]
mod tests;
