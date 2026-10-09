//! The sign-in flow end to end against an in-process deployment: real loopback
//! listener, real PKCE, real HTTP, an in-memory keychain and a fake browser.

use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::atomic::{AtomicUsize, Ordering};

use serde_json::json;

use super::*;
use crate::store::MemoryStore;
use crate::testutil::{MockServer, Req, Res, serve, serve_dropping};

type Handler = dyn Fn(&Req, &HashMap<String, String>) -> Option<Res> + Send + Sync;

/// A deployment that serves discovery and delegates the rest to `extra`.
async fn deployment(extra: Box<Handler>) -> MockServer {
    serve(move |req| {
        let origin = format!("http://{}", req.headers["host"]);
        let path = req.path.split('?').next().unwrap_or_default().to_owned();
        if path == "/.well-known/elitea-client" {
            return Res::json(
                200,
                &json!({
                    "server_version": "1.60",
                    "client_contract": "1.0",
                    "deployment_kind": "self_hosted",
                    "display_name": "Acme Elitea",
                    "brand_pack_url": format!("{origin}/api/v2/branding/pack.json"),
                    "native_auth": {
                        "issuer": origin,
                        "authorization_endpoint": format!("{origin}/api/v2/auth/native/authorize"),
                        "token_endpoint": format!("{origin}/api/v2/auth/native/token"),
                        "revocation_endpoint": format!("{origin}/api/v2/auth/native/revoke"),
                        "code_challenge_methods_supported": ["S256"]
                    },
                    "client_policy": {"min_client_version": "0.1.0"},
                }),
            );
        }
        let form = req.form();
        extra(req, &form).unwrap_or_else(|| Res::json(404, &json!({"error": "not_found"})))
    })
    .await
}

fn tokens(refresh: &str) -> serde_json::Value {
    json!({
        "access_token": format!("access-for-{refresh}-padding"),
        "token_type": "bearer",
        "expires_in": 900,
        "refresh_token": refresh,
        "refresh_token_expires_in": 2_592_000,
        "device_id": "dev-1",
        "client_policy": {"idle_lock_seconds": 300, "local_work": {"allowed": false}}
    })
}

/// Plays the person: reads the authorize URL, then lets the browser hit the
/// loopback redirect with whatever `respond` builds from the request's `state`.
type Respond = Box<dyn Fn(&HashMap<String, String>, &str) -> Option<String> + Send + Sync>;

struct FakeBrowser {
    respond: Respond,
    opened: std::sync::Mutex<Vec<String>>,
}

impl BrowserOpener for FakeBrowser {
    fn open(&self, url: &str) -> Result<(), HostError> {
        self.opened.lock().unwrap().push(url.to_owned());
        let query: HashMap<String, String> = Url::parse(url)
            .unwrap()
            .query_pairs()
            .into_owned()
            .collect();
        if let Some(callback) = (self.respond)(&query, url) {
            let target = format!("{}?{callback}", query["redirect_uri"]);
            tokio::spawn(async move {
                let _ = reqwest::get(target).await;
            });
        }
        Ok(())
    }
}

fn temp_dir() -> PathBuf {
    let dir = std::env::temp_dir().join(format!("elitea-desktop-auth-{}", pkce::create_state()));
    std::fs::create_dir_all(&dir).unwrap();
    dir
}

struct Harness {
    service: AuthService,
    keychain: Arc<MemoryStore>,
    browser: Arc<FakeBrowser>,
    dir: PathBuf,
}

impl Drop for Harness {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.dir);
    }
}

fn harness(
    respond: impl Fn(&HashMap<String, String>, &str) -> Option<String> + Send + Sync + 'static,
) -> Harness {
    let keychain = Arc::new(MemoryStore::default());
    let browser = Arc::new(FakeBrowser {
        respond: Box::new(respond),
        opened: std::sync::Mutex::default(),
    });
    let dir = temp_dir();
    let service = AuthService::new(AuthConfig {
        store: keychain.clone(),
        files: SettingsFiles::new(dir.clone()),
        tokens: TokenEndpoint::new("0.1.0").unwrap(),
        opener: browser.clone(),
        build_client_id: None,
        runtime_client_id: None,
    })
    .with_deadline(Duration::from_secs(10));
    Harness {
        service,
        keychain,
        browser,
        dir,
    }
}

fn approve_with(
    issuer: String,
) -> impl Fn(&HashMap<String, String>, &str) -> Option<String> + Send + Sync {
    move |q, _| Some(format!("code=the-code&state={}&iss={issuer}", q["state"]))
}

/// A token endpoint that checks the PKCE verifier against the challenge the
/// authorize request carried (captured from the browser's URL).
fn pkce_checking_deployment(
    challenge: Arc<std::sync::Mutex<String>>,
    refreshes: Arc<AtomicUsize>,
) -> Box<Handler> {
    Box::new(move |req, form| {
        if req.path == "/api/v2/auth/native/token" {
            return Some(match form.get("grant_type").map(String::as_str) {
                Some("authorization_code") => {
                    let verifier = form.get("code_verifier").cloned().unwrap_or_default();
                    if pkce::code_challenge(&verifier) != *challenge.lock().unwrap()
                        || form["code"] != "the-code"
                    {
                        return Some(Res::json(400, &json!({"error": "invalid_grant"})));
                    }
                    Res::json(200, &tokens("refresh-1"))
                }
                Some("refresh_token") => {
                    let n = refreshes.fetch_add(1, Ordering::SeqCst) + 2;
                    if form["refresh_token"] == "stale" {
                        return Some(Res::json(401, &json!({"error": "device_revoked"})));
                    }
                    Res::json(200, &tokens(&format!("refresh-{n}")))
                }
                _ => Res::json(400, &json!({"error": "unsupported_grant_type"})),
            });
        }
        if req.path == "/api/v2/auth/native/revoke" {
            return Some(Res::json(200, &json!({})));
        }
        None
    })
}

#[tokio::test]
async fn connect_reads_discovery_and_remembers_the_deployment() {
    let server = deployment(Box::new(|_, _| None)).await;
    let h = harness(|_, _| None);
    let info = h.service.connect(&server.origin).await.unwrap();
    assert_eq!(info.display_name, "Acme Elitea");
    assert_eq!(info.origin, server.origin);
    let state = h.service.state().unwrap();
    assert!(state.configured && !state.signed_in);
    assert_eq!(state.display_name.as_deref(), Some("Acme Elitea"));
    assert_eq!(state.client_id, "desktop");
}

#[tokio::test]
async fn connect_refuses_a_remote_http_address_before_any_request() {
    let h = harness(|_, _| None);
    let err = h
        .service
        .connect("http://elitea.example.com")
        .await
        .unwrap_err();
    assert!(matches!(err, HostError::InvalidAddress(_)));
    assert!(!h.service.state().unwrap().configured);
}

#[tokio::test]
async fn full_sign_in_uses_pkce_state_and_the_loopback_redirect_and_stores_the_refresh_token_in_the_keychain()
 {
    let challenge = Arc::new(std::sync::Mutex::new(String::new()));
    let server = deployment(pkce_checking_deployment(challenge.clone(), Arc::default())).await;
    let issuer = server.origin.clone();
    let captured = challenge.clone();
    let h = harness(move |q, url| {
        *captured.lock().unwrap() = q["code_challenge"].clone();
        assert_eq!(q["response_type"], "code");
        assert_eq!(q["code_challenge_method"], "S256");
        assert_eq!(q["client_id"], "desktop");
        assert_eq!(q["client_version"], "0.1.0");
        assert!(q["redirect_uri"].starts_with("http://127.0.0.1:"));
        assert!(q["redirect_uri"].ends_with("/callback"));
        assert!(url.starts_with(&format!("{issuer}/api/v2/auth/native/authorize?")));
        approve_with(issuer.clone())(q, url)
    });
    h.service.connect(&server.origin).await.unwrap();

    let state = h.service.sign_in().await.unwrap();

    assert!(state.signed_in);
    assert_eq!(state.policy.unwrap()["idle_lock_seconds"], 300);
    // The refresh token is in the keychain item, and the webview is handed only the access token.
    let stored = h.keychain.raw().unwrap();
    assert!(stored.contains("refresh-1"));
    let token = h.service.access_token().await.unwrap().unwrap();
    assert_eq!(token.token, "access-for-refresh-1-padding");
    assert!(token.expires_in > 800 && token.expires_in <= 900);
    let wire = serde_json::to_string(&token).unwrap();
    assert_eq!(
        wire,
        format!(
            r#"{{"token":"access-for-refresh-1-padding","expiresIn":{}}}"#,
            token.expires_in
        )
    );
    assert!(!wire.contains("\"refresh-1\""));
    // Both the token request and the browser URL carried the client version.
    assert!(
        server
            .seen()
            .iter()
            .filter(|r| r.path.ends_with("/token"))
            .all(|r| r.headers["x-client-version"] == "0.1.0")
    );
}

#[tokio::test]
async fn a_callback_with_the_wrong_state_is_ignored_and_stores_nothing() {
    let server = deployment(Box::new(|_, _| None)).await;
    let issuer = server.origin.clone();
    let mut h =
        harness(move |_, _| Some(format!("code=the-code&state=attacker-chosen&iss={issuer}")));
    h.service = AuthService::new(AuthConfig {
        store: h.keychain.clone(),
        files: SettingsFiles::new(h.dir.clone()),
        tokens: TokenEndpoint::new("0.1.0").unwrap(),
        opener: h.browser.clone(),
        build_client_id: None,
        runtime_client_id: None,
    })
    .with_deadline(Duration::from_millis(300));
    h.service.connect(&server.origin).await.unwrap();
    // The foreign callback no longer aborts the attempt: it just times out.
    let err = h.service.sign_in().await.unwrap_err();
    assert!(matches!(err, HostError::SignInAborted), "{err:?}");
    assert!(h.keychain.raw().is_none());
    assert!(!h.service.state().unwrap().signed_in);
    assert!(
        server.seen().iter().all(|r| !r.path.ends_with("/token")),
        "no code may be exchanged"
    );
}

#[test]
fn the_platform_name_is_linux_on_linux() {
    #[cfg(target_os = "linux")]
    assert_eq!(platform(), "linux");
    #[cfg(target_os = "macos")]
    assert_eq!(platform(), "macos");
    #[cfg(target_os = "windows")]
    assert_eq!(platform(), "windows");
}

#[tokio::test]
async fn a_callback_from_another_issuer_is_rejected() {
    let server = deployment(Box::new(|_, _| None)).await;
    let h = harness(|q, _| {
        Some(format!(
            "code=the-code&state={}&iss=https%3A%2F%2Fevil.example",
            q["state"]
        ))
    });
    h.service.connect(&server.origin).await.unwrap();
    assert!(matches!(
        h.service.sign_in().await.unwrap_err(),
        HostError::SignInRejected
    ));
    assert!(h.keychain.raw().is_none());
}

#[tokio::test]
async fn the_person_denying_access_is_reported_as_denied() {
    let server = deployment(Box::new(|_, _| None)).await;
    let issuer = server.origin.clone();
    let h = harness(move |q, _| {
        Some(format!(
            "error=access_denied&state={}&iss={issuer}",
            q["state"]
        ))
    });
    h.service.connect(&server.origin).await.unwrap();
    assert!(matches!(
        h.service.sign_in().await.unwrap_err(),
        HostError::SignInDenied
    ));
}

#[tokio::test]
async fn closing_the_browser_without_finishing_times_out() {
    let server = deployment(Box::new(|_, _| None)).await;
    let mut h = harness(|_, _| None);
    h.service = AuthService::new(AuthConfig {
        store: h.keychain.clone(),
        files: SettingsFiles::new(h.dir.clone()),
        tokens: TokenEndpoint::new("0.1.0").unwrap(),
        opener: h.browser.clone(),
        build_client_id: None,
        runtime_client_id: None,
    })
    .with_deadline(Duration::from_millis(150));
    h.service.connect(&server.origin).await.unwrap();
    assert!(matches!(
        h.service.sign_in().await.unwrap_err(),
        HostError::SignInAborted
    ));
    assert_eq!(h.browser.opened.lock().unwrap().len(), 1);
}

#[tokio::test]
async fn an_unregistered_client_id_is_named_in_the_error() {
    let server = deployment(Box::new(|req, _| {
        (req.path == "/api/v2/auth/native/token")
            .then(|| Res::json(401, &json!({"error": "invalid_client"})))
    }))
    .await;
    let issuer = server.origin.clone();
    let h = harness(approve_with(issuer));
    h.service.connect(&server.origin).await.unwrap();
    let err = h.service.sign_in().await.unwrap_err();
    assert!(err.to_string().contains("\"desktop\""), "{err}");
    assert!(h.keychain.raw().is_none());
}

async fn signed_in(refreshes: Arc<AtomicUsize>) -> (Harness, MockServer) {
    let challenge = Arc::new(std::sync::Mutex::new(String::new()));
    let server = deployment(pkce_checking_deployment(challenge.clone(), refreshes)).await;
    let issuer = server.origin.clone();
    let h = harness(move |q, url| {
        *challenge.lock().unwrap() = q["code_challenge"].clone();
        approve_with(issuer.clone())(q, url)
    });
    h.service.connect(&server.origin).await.unwrap();
    h.service.sign_in().await.unwrap();
    (h, server)
}

#[tokio::test]
async fn refresh_rotates_the_refresh_token_and_replaces_the_access_token() {
    let (h, server) = signed_in(Arc::default()).await;
    assert_eq!(h.service.refresh().await.unwrap(), RefreshResult::Refreshed);
    assert!(h.keychain.raw().unwrap().contains("refresh-2"));
    assert_eq!(
        h.service.access_token().await.unwrap().unwrap().token,
        "access-for-refresh-2-padding"
    );
    let refresh = server
        .seen()
        .into_iter()
        .rfind(|r| {
            r.form()
                .get("grant_type")
                .is_some_and(|g| g == "refresh_token")
        })
        .unwrap();
    assert_eq!(refresh.form()["refresh_token"], "refresh-1");
    assert_eq!(refresh.form()["client_version"], "0.1.0");
}

#[tokio::test]
async fn concurrent_refreshes_collapse_into_one_exchange() {
    let count = Arc::new(AtomicUsize::new(0));
    let (h, _server) = signed_in(count.clone()).await;
    let (a, b, c) = tokio::join!(
        h.service.refresh(),
        h.service.refresh(),
        h.service.refresh()
    );
    assert_eq!(
        [a.unwrap(), b.unwrap(), c.unwrap()],
        [RefreshResult::Refreshed; 3]
    );
    assert_eq!(
        count.load(Ordering::SeqCst),
        1,
        "refresh tokens rotate: more than one exchange would burn the family"
    );
}

#[tokio::test]
async fn a_revoked_device_wipes_the_session_and_the_policy() {
    let (h, _server) = signed_in(Arc::default()).await;
    // Plant the token the deployment treats as revoked.
    let mut session = load_session(h.keychain.as_ref()).unwrap().unwrap();
    session.refresh_token = "stale".into();
    save_session(h.keychain.as_ref(), &session).unwrap();

    assert_eq!(h.service.refresh().await.unwrap(), RefreshResult::Ended);

    assert!(h.keychain.raw().is_none());
    let state = h.service.state().unwrap();
    assert!(!state.signed_in && state.policy.is_none());
    assert!(
        state.configured,
        "the deployment stays chosen so the person only signs in again"
    );
    assert!(h.service.access_token().await.unwrap().is_none());
}

#[tokio::test]
async fn a_deployment_that_is_down_keeps_the_session() {
    let (h, _server) = signed_in(Arc::default()).await;
    // Point the stored session at a port nothing listens on.
    let mut session = load_session(h.keychain.as_ref()).unwrap().unwrap();
    session.origin = "http://127.0.0.1:9".into();
    save_session(h.keychain.as_ref(), &session).unwrap();
    let mut settings = SettingsFiles::new(h.dir.clone()).settings().unwrap();
    settings.origin = Some("http://127.0.0.1:9".into());
    SettingsFiles::new(h.dir.clone())
        .save_settings(&settings)
        .unwrap();

    assert_eq!(
        h.service.refresh().await.unwrap(),
        RefreshResult::Unavailable
    );
    assert!(
        h.keychain.raw().unwrap().contains("refresh-1"),
        "an offline refresh must not lose the token"
    );
}

#[tokio::test]
async fn sign_out_revokes_on_the_server_then_forgets_everything() {
    let (h, server) = signed_in(Arc::default()).await;
    h.service.sign_out().await.unwrap();
    assert!(h.keychain.raw().is_none());
    let revoke = server
        .seen()
        .into_iter()
        .find(|r| r.path == "/api/v2/auth/native/revoke")
        .expect("revoked");
    assert_eq!(revoke.form()["token"], "refresh-1");
    assert!(!h.service.state().unwrap().signed_in);
}

/// The deployment answers refreshes slowly, so a sign-out can land mid-refresh.
fn slow_refresh_deployment(challenge: Arc<std::sync::Mutex<String>>) -> Box<Handler> {
    let inner = pkce_checking_deployment(challenge, Arc::default());
    Box::new(move |req, form| {
        if form.get("grant_type").is_some_and(|g| g == "refresh_token") {
            std::thread::sleep(Duration::from_millis(300));
        }
        inner(req, form)
    })
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_sign_out_during_a_refresh_is_not_undone_by_the_refresh() {
    let challenge = Arc::new(std::sync::Mutex::new(String::new()));
    let server = deployment(slow_refresh_deployment(challenge.clone())).await;
    let issuer = server.origin.clone();
    let h = Arc::new(harness(move |q, url| {
        *challenge.lock().unwrap() = q["code_challenge"].clone();
        approve_with(issuer.clone())(q, url)
    }));
    h.service.connect(&server.origin).await.unwrap();
    h.service.sign_in().await.unwrap();

    let refreshing = {
        let h = h.clone();
        tokio::spawn(async move { h.service.refresh().await })
    };
    tokio::time::sleep(Duration::from_millis(100)).await;
    h.service.sign_out().await.unwrap();
    let _ = refreshing.await.unwrap();

    assert!(h.keychain.raw().is_none(), "the refresh resurrected the session");
    assert!(h.service.access_token().await.unwrap().is_none());
    // The sign-out waited for the rotation and revoked the LIVE token.
    let revoked: Vec<String> = server
        .seen()
        .iter()
        .filter(|r| r.path == "/api/v2/auth/native/revoke")
        .map(|r| r.form()["token"].clone())
        .collect();
    assert_eq!(revoked, ["refresh-2"]);
}

#[tokio::test]
async fn a_session_for_another_deployment_does_not_count_as_signed_in() {
    let (h, _server) = signed_in(Arc::default()).await;
    let other = deployment(Box::new(|_, _| None)).await;
    h.service.connect(&other.origin).await.unwrap();
    // Choosing another deployment ended the old session.
    assert!(!h.service.state().unwrap().signed_in);
    assert!(h.keychain.raw().is_none());
}

/// A deployment whose token endpoint loses the first `lost` answers (the
/// request arrives and is processed, the response never does), and which
/// replays a rotation to the same refresh token like the real server.
async fn lossy_deployment(lost: usize) -> MockServer {
    let issued = Arc::new(AtomicUsize::new(0));
    serve_dropping(Some(("/api/v2/auth/native/token", lost)), move |req| {
        let origin = format!("http://{}", req.headers["host"]);
        if req.path == "/.well-known/elitea-client" {
            return Res::json(
                200,
                &json!({
                    "server_version": "1.60", "client_contract": "1.0",
                    "deployment_kind": "self_hosted", "display_name": "Acme",
                    "brand_pack_url": format!("{origin}/api/v2/branding/pack.json"),
                    "native_auth": {
                        "issuer": origin,
                        "authorization_endpoint": format!("{origin}/api/v2/auth/native/authorize"),
                        "token_endpoint": format!("{origin}/api/v2/auth/native/token"),
                        "revocation_endpoint": format!("{origin}/api/v2/auth/native/revoke"),
                        "code_challenge_methods_supported": ["S256"]
                    },
                    "client_policy": {},
                }),
            );
        }
        if req.path == "/api/v2/auth/native/token" {
            let n = issued.fetch_add(1, Ordering::SeqCst);
            return Res::json(200, &tokens(&format!("rotated-{n}")));
        }
        Res::json(404, &json!({"error": "not_found"}))
    })
    .await
}

fn seed(h: &Harness, origin: &str, refresh: &str) {
    save_session(
        h.keychain.as_ref(),
        &StoredSession {
            origin: origin.into(),
            client_id: "desktop".into(),
            device_id: "dev-1".into(),
            refresh_token: refresh.into(),
        },
    )
    .unwrap();
    let files = SettingsFiles::new(h.dir.clone());
    let mut settings = files.settings().unwrap();
    settings.origin = Some(origin.into());
    files.save_settings(&settings).unwrap();
}

fn token_posts(server: &MockServer) -> Vec<HashMap<String, String>> {
    server
        .seen()
        .iter()
        .filter(|r| r.path == "/api/v2/auth/native/token")
        .map(Req::form)
        .collect()
}

#[tokio::test]
async fn a_lost_refresh_answer_is_retried_at_once_with_the_same_refresh_token() {
    let server = lossy_deployment(1).await;
    let h = harness(|_, _| None);
    seed(&h, &server.origin, "old-refresh");

    assert_eq!(h.service.refresh().await.unwrap(), RefreshResult::Refreshed);

    let posts = token_posts(&server);
    assert_eq!(posts.len(), 2, "one lost, one immediate retry");
    assert!(posts.iter().all(|f| f["refresh_token"] == "old-refresh"));
    assert!(h.keychain.raw().unwrap().contains("rotated-1"));
}

#[tokio::test]
async fn two_lost_answers_keep_the_old_token_for_a_later_attempt() {
    let server = lossy_deployment(2).await;
    let h = harness(|_, _| None);
    seed(&h, &server.origin, "old-refresh");

    assert_eq!(
        h.service.refresh().await.unwrap(),
        RefreshResult::Unavailable
    );
    assert!(h.keychain.raw().unwrap().contains("old-refresh"));
}

#[tokio::test]
async fn a_keychain_write_failure_after_rotation_never_reuses_the_spent_token() {
    let refreshes = Arc::new(AtomicUsize::new(0));
    let (h, server) = signed_in(refreshes).await;
    h.keychain
        .fail_saves
        .store(true, std::sync::atomic::Ordering::SeqCst);
    assert_eq!(h.service.refresh().await.unwrap(), RefreshResult::Refreshed);
    // The keychain still holds the spent token; memory holds the live one.
    assert!(h.keychain.raw().unwrap().contains("refresh-1"));

    h.keychain
        .fail_saves
        .store(false, std::sync::atomic::Ordering::SeqCst);
    assert_eq!(h.service.refresh().await.unwrap(), RefreshResult::Refreshed);

    let refresh_tokens: Vec<String> = token_posts(&server)
        .into_iter()
        .filter(|f| f["grant_type"] == "refresh_token")
        .map(|f| f["refresh_token"].clone())
        .collect();
    assert_eq!(refresh_tokens, ["refresh-1", "refresh-2"]);
    assert!(h.keychain.raw().unwrap().contains("refresh-3"));
}

#[tokio::test]
async fn a_426_from_the_token_endpoint_is_reported_as_upgrade_required() {
    let server = deployment(Box::new(|req, _| {
        (req.path == "/api/v2/auth/native/token")
            .then(|| Res::json(426, &json!({"error": "upgrade_required"})))
    }))
    .await;
    let h = harness(|_, _| None);
    seed(&h, &server.origin, "r");
    assert_eq!(
        h.service.refresh().await.unwrap(),
        RefreshResult::UpgradeRequired
    );
    assert!(h.keychain.raw().is_some(), "the session is kept");
}

#[tokio::test]
async fn a_policy_file_that_cannot_be_written_does_not_fail_sign_in_or_refresh() {
    let challenge = Arc::new(std::sync::Mutex::new(String::new()));
    let server = deployment(pkce_checking_deployment(challenge.clone(), Arc::default())).await;
    let issuer = server.origin.clone();
    let h = harness(move |q, url| {
        *challenge.lock().unwrap() = q["code_challenge"].clone();
        approve_with(issuer.clone())(q, url)
    });
    h.service.connect(&server.origin).await.unwrap();
    // A directory where the staging file must go: every policy write fails.
    std::fs::create_dir_all(h.dir.join("client-policy.json.tmp")).unwrap();

    let state = h.service.sign_in().await.unwrap();
    assert!(state.signed_in);
    assert!(h.keychain.raw().unwrap().contains("refresh-1"));
    assert_eq!(h.service.refresh().await.unwrap(), RefreshResult::Refreshed);
    assert!(h.keychain.raw().unwrap().contains("refresh-2"));
}
