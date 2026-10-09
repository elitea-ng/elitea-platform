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
use tokio::sync::{Mutex, oneshot};
use url::Url;

use crate::discovery;
use crate::error::HostError;
use crate::loopback::LoopbackListener;
use crate::pkce;
use crate::settings::{Settings, SettingsFiles, resolve_client_id};
use crate::store::{
    PendingRevoke, SecretStore, StoredSession, load_pending, load_session, save_pending,
    save_session,
};
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
    /// 426: this build is older than the deployment's minimum.
    #[serde(rename = "upgrade_required")]
    UpgradeRequired,
}

struct Cached {
    token: String,
    expires_at: Instant,
}

pub struct AuthService {
    store: Arc<dyn SecretStore>,
    /// Sign-outs whose server revoke failed, retried at launch (own keychain item).
    pending_revokes: Arc<dyn SecretStore>,
    pending_gate: Mutex<()>,
    files: SettingsFiles,
    tokens: TokenEndpoint,
    opener: Arc<dyn BrowserOpener>,
    build_client_id: Option<&'static str>,
    runtime_client_id: Option<String>,
    cached: Mutex<Option<Cached>>,
    /// A rotated session the keychain refused to take. The server already
    /// consumed the old refresh token, so THIS is the only live one: it is
    /// kept in memory and written again before the next use.
    unsaved: Mutex<Option<StoredSession>>,
    /// Serialises everything that writes or forgets the session: refreshes
    /// (refresh tokens rotate, so two at once would burn the family), the
    /// sign-in's adopt, sign-out and wipe. Without it an in-flight refresh
    /// could adopt its rotated token AFTER a sign-out and resurrect the session.
    refresh_gate: Mutex<()>,
    sign_in_deadline: Duration,
    /// Ends the sign-in that is waiting for the browser (`cancel_sign_in`).
    sign_in_cancel: std::sync::Mutex<Option<oneshot::Sender<()>>>,
}

pub struct AuthConfig {
    pub store: Arc<dyn SecretStore>,
    pub pending_revokes: Arc<dyn SecretStore>,
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
            pending_revokes: config.pending_revokes,
            pending_gate: Mutex::new(()),
            files: config.files,
            tokens: config.tokens,
            opener: config.opener,
            build_client_id: config.build_client_id,
            runtime_client_id: config.runtime_client_id,
            cached: Mutex::new(None),
            unsaved: Mutex::new(None),
            refresh_gate: Mutex::new(()),
            sign_in_deadline: SIGN_IN_DEADLINE,
            sign_in_cancel: std::sync::Mutex::new(None),
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
        // Armed first, so a cancel that arrives while discovery is still
        // loading is not lost. A newer attempt replaces (and so ends) an older one.
        let (cancel_tx, cancel_rx) = oneshot::channel();
        *self
            .sign_in_cancel
            .lock()
            .unwrap_or_else(|e| e.into_inner()) = Some(cancel_tx);
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

        let params = tokio::select! {
            params = listener.wait_for_callback(self.sign_in_deadline, &state) => params?,
            // Dropping the listener closes the loopback port: a late browser
            // redirect finds nothing to deliver its code to.
            _ = cancel_rx => return Err(HostError::SignInAborted),
        };
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
                let refresh_token = tokens.refresh_token.clone();
                let gate = self.refresh_gate.lock().await;
                let adopted = self
                    .adopt(
                        &origin_text,
                        &client_id,
                        &auth.revocation_endpoint,
                        tokens,
                        None,
                    )
                    .await;
                drop(gate);
                if let Err(error) = adopted {
                    // The keychain refused the new session, so nothing here can
                    // use or revoke it later: end it on the server now (best
                    // effort) rather than leave a live device session behind.
                    self.tokens
                        .revoke(&auth.revocation_endpoint, &refresh_token, &client_id)
                        .await;
                    return Err(error);
                }
                self.state()
            }
            other => Err(map_exchange_failure(&other, &client_id)),
        }
    }

    /// End the sign-in waiting for the browser, if any. It returns
    /// `SignInAborted`; nothing is stored.
    pub fn cancel_sign_in(&self) {
        let pending = self
            .sign_in_cancel
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .take();
        if let Some(cancel) = pending {
            let _ = cancel.send(());
        }
    }

    /// Persist a token response: the rotated refresh token first (so a crash
    /// after the exchange never loses the session), then the access token.
    async fn adopt(
        &self,
        origin: &str,
        client_id: &str,
        revocation_endpoint: &str,
        tokens: TokenSet,
        previous: Option<&StoredSession>,
    ) -> Result<(), HostError> {
        let device_id = if tokens.device_id.is_empty() {
            previous.map(|p| p.device_id.clone()).unwrap_or_default()
        } else {
            tokens.device_id.clone()
        };
        let session = StoredSession {
            origin: origin.to_owned(),
            client_id: client_id.to_owned(),
            device_id,
            refresh_token: tokens.refresh_token.clone(),
            revocation_endpoint: revocation_endpoint.to_owned(),
        };
        if let Err(error) = save_session(self.store.as_ref(), &session) {
            if previous.is_none() {
                return Err(error);
            }
            // A rotation succeeded but the keychain write failed: the old
            // token is spent. Keep the new one in memory and write it later.
            *self.unsaved.lock().await = Some(session);
        } else {
            *self.unsaved.lock().await = None;
        }
        // The policy file is a cache of what the server sent; failing to write
        // it must not fail a sign-in or a refresh whose token is already stored.
        if let Some(policy) = &tokens.client_policy
            && let Err(error) = self.files.save_policy(policy)
        {
            eprintln!("elitea-desktop: could not store the client policy: {error}");
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
            RefreshResult::UpgradeRequired => Err(HostError::UpgradeRequired),
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
        let Some(session) = self.current_session().await? else {
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

        let mut outcome = self.exchange_refresh(&auth.token_endpoint, &session).await;
        if matches!(outcome, TokenOutcome::Network | TokenOutcome::Failed) {
            // The request may have arrived with only the answer lost. The server
            // replays a rotation for a short window to the SAME refresh token, so
            // retry once now, inside the gate, rather than on a later 401.
            outcome = self.exchange_refresh(&auth.token_endpoint, &session).await;
        }
        match outcome {
            TokenOutcome::Ok(tokens) => {
                self.adopt(
                    &session.origin,
                    &session.client_id,
                    &auth.revocation_endpoint,
                    tokens,
                    Some(&session),
                )
                .await?;
                Ok(RefreshResult::Refreshed)
            }
            // The session is gone for good: forget it.
            TokenOutcome::DeviceRevoked
            | TokenOutcome::InvalidGrant
            | TokenOutcome::InvalidClient => {
                self.wipe_locked().await?;
                Ok(RefreshResult::Ended)
            }
            TokenOutcome::UpgradeRequired => Ok(RefreshResult::UpgradeRequired),
            // Nothing was consumed, or the response was lost twice: keep the token and retry later.
            TokenOutcome::Unavailable | TokenOutcome::Network | TokenOutcome::Failed => {
                Ok(RefreshResult::Unavailable)
            }
        }
    }

    async fn exchange_refresh(&self, endpoint: &str, session: &StoredSession) -> TokenOutcome {
        self.tokens
            .refresh(endpoint, &session.refresh_token, &session.client_id)
            .await
    }

    /// The live session: an in-memory rotation the keychain has not taken yet
    /// (written again here), else the keychain's.
    async fn current_session(&self) -> Result<Option<StoredSession>, HostError> {
        let mut unsaved = self.unsaved.lock().await;
        if let Some(session) = unsaved.clone() {
            if save_session(self.store.as_ref(), &session).is_ok() {
                *unsaved = None;
            }
            return Ok(Some(session));
        }
        drop(unsaved);
        load_session(self.store.as_ref())
    }

    // ---- sign out ------------------------------------------------------

    /// Revoke the device session on the server, then forget it. A revoke that
    /// does not get through (offline, deployment down) is remembered in its
    /// own keychain item and retried at the next launch
    /// ([`Self::retry_pending_revokes`]); the local session is forgotten
    /// either way. Returns whether the server confirmed the revoke.
    pub async fn sign_out(&self) -> Result<bool, HostError> {
        // Held across the revoke too: a refresh finishing meanwhile would
        // otherwise rotate the token being revoked and store the new one.
        let _gate = self.refresh_gate.lock().await;
        let mut revoked = true;
        if let Some(session) = self.current_session().await? {
            let pending = PendingRevoke::from(&session);
            revoked = self.revoke(&pending).await;
            if !revoked {
                self.remember_pending(pending).await;
            }
        }
        self.wipe_locked().await?;
        Ok(revoked)
    }

    /// Revoke at the endpoint fresh discovery names, else at the one stored
    /// with the token (discovery validated it when the token was issued).
    async fn revoke(&self, pending: &PendingRevoke) -> bool {
        let fresh = match Url::parse(&pending.origin) {
            Ok(origin) => discovery::fetch_discovery(self.tokens.http(), &origin)
                .await
                .ok()
                .and_then(|d| d.native_auth().ok().map(|a| a.revocation_endpoint.clone())),
            Err(_) => None,
        };
        let Some(endpoint) = fresh.or_else(|| {
            (!pending.revocation_endpoint.is_empty()).then(|| pending.revocation_endpoint.clone())
        }) else {
            return false;
        };
        self.tokens
            .revoke(&endpoint, &pending.refresh_token, &pending.client_id)
            .await
    }

    async fn remember_pending(&self, pending: PendingRevoke) {
        let _gate = self.pending_gate.lock().await;
        let result = load_pending(self.pending_revokes.as_ref()).and_then(|mut list| {
            list.push(pending);
            save_pending(self.pending_revokes.as_ref(), &list)
        });
        if let Err(error) = result {
            eprintln!("elitea-desktop: could not keep a failed revoke for retry: {error}");
        }
    }

    /// Retry the revokes earlier sign-outs could not deliver; called once at
    /// launch. Returns how many are still waiting.
    pub async fn retry_pending_revokes(&self) -> usize {
        let _gate = self.pending_gate.lock().await;
        let Ok(list) = load_pending(self.pending_revokes.as_ref()) else {
            return 0;
        };
        let mut waiting = Vec::new();
        for pending in list {
            if !self.revoke(&pending).await {
                waiting.push(pending);
            }
        }
        if let Err(error) = save_pending(self.pending_revokes.as_ref(), &waiting) {
            eprintln!("elitea-desktop: could not update the pending revokes: {error}");
        }
        waiting.len()
    }

    /// Forget the session and everything derived from it, without a server call.
    /// The connected deployment is kept so the person only has to sign in again.
    pub async fn wipe(&self) -> Result<(), HostError> {
        let _gate = self.refresh_gate.lock().await;
        self.wipe_locked().await
    }

    /// [`Self::wipe`] for a caller that already holds `refresh_gate`.
    async fn wipe_locked(&self) -> Result<(), HostError> {
        *self.cached.lock().await = None;
        *self.unsaved.lock().await = None;
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
        "linux" => "linux",
        _ => "other",
    }
}

#[cfg(test)]
mod tests;
