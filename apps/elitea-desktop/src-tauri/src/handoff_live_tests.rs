//! The handoff host against a REAL elitea-main (ADR-0034), not the mock.
//! Ignored by default; run against a live stack (IPC.md, "Cloud handoff:
//! live run"):
//!
//! ```text
//! ELITEA_HANDOFF_LIVE_URL=http://127.0.0.1:39880 \
//! ELITEA_HANDOFF_LIVE_TOKEN=elnat_… ELITEA_HANDOFF_LIVE_OTHER_TOKEN=elnat_… \
//! ELITEA_HANDOFF_LIVE_PROJECT=1 ELITEA_HANDOFF_LIVE_SIM=/path/to/cloudturn-sim \
//! cargo test -p elitea-desktop --lib handoff::tests::live -- --ignored --test-threads=1
//! ```
//!
//! `ELITEA_HANDOFF_LIVE_SIM` plays the cloud executor (there is none yet):
//! `services/elitea-main/cmd/handoff-live-cloudturn` (build tag
//! `handoff_live`; IPC.md, "Cloud handoff: live run"). `-project P
//! -conversation <uuid> -bundle <file> -commit <sha>` runs one cloud turn
//! through Main's application services, `-sql <query>` answers rows as JSON.

use std::process::Command;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;
use elitea_local_tools::handoff::apply::{Mode, Outcome};
use elitea_workspace_store::sync::Backoff;
use elitea_workspace_store::transport::StoreTransport as _;
use reqwest::Method;
use serde_json::{Value, json};

use super::super::{HandoffDeps, HandoffHost};
use super::{Events, Policy, git};
use crate::d0::api::{ApiError, Bearer, Credentials};
use crate::d0::events::VecEmitter;
use crate::d0::remote_tools::RetryPolicy;
use crate::d0::turn::{AgentHost, HostDeps};
use crate::handoff_settings::{FixedEnv, FolderSettings};
use crate::history::{HistoryStore, Owner};
use crate::workspaces::WorkspaceStore;

struct Live {
    origin: String,
    token: String,
    other_token: Option<String>,
    project: i64,
    sim: String,
}

fn live() -> Live {
    let var = |name: &str| {
        std::env::var(name).unwrap_or_else(|_| panic!("{name} is required for the live run"))
    };
    Live {
        origin: var("ELITEA_HANDOFF_LIVE_URL")
            .trim_end_matches('/')
            .to_owned(),
        token: var("ELITEA_HANDOFF_LIVE_TOKEN"),
        other_token: std::env::var("ELITEA_HANDOFF_LIVE_OTHER_TOKEN").ok(),
        project: std::env::var("ELITEA_HANDOFF_LIVE_PROJECT")
            .ok()
            .and_then(|p| p.parse().ok())
            .unwrap_or(1),
        sim: var("ELITEA_HANDOFF_LIVE_SIM"),
    }
}

struct LiveCreds(String, String);

#[async_trait]
impl Credentials for LiveCreds {
    async fn bearer(&self) -> Result<Bearer, ApiError> {
        Ok(Bearer {
            origin: self.0.clone(),
            token: self.1.clone(),
        })
    }
    async fn refreshed(&self) -> Result<Bearer, ApiError> {
        self.bearer().await
    }
    fn identity(&self) -> Option<String> {
        Some(format!("{}#live", self.0))
    }
}

/// One HTTP call as the web or another device makes it.
async fn raw(
    live: &Live,
    token: &str,
    method: Method,
    path: &str,
    body: Option<Value>,
) -> (u16, Value) {
    let mut request = reqwest::Client::new()
        .request(method, format!("{}{path}", live.origin))
        .bearer_auth(token);
    if let Some(body) = body {
        request = request.json(&body);
    }
    let response = request.send().await.unwrap();
    let status = response.status().as_u16();
    let bytes = response.bytes().await.unwrap();
    (
        status,
        serde_json::from_slice(&bytes).unwrap_or(Value::Null),
    )
}

fn sim(live: &Live, args: &[&str]) -> Value {
    let output = Command::new(&live.sim).args(args).output().unwrap();
    assert!(
        output.status.success(),
        "sim {args:?}: {}",
        String::from_utf8_lossy(&output.stderr)
    );
    serde_json::from_slice(&output.stdout).unwrap()
}

fn sql(live: &Live, query: &str) -> Vec<Vec<String>> {
    serde_json::from_value(sim(live, &["-sql", query])).unwrap()
}

/// A new Local work thread of the live user.
async fn conversation(live: &Live) -> String {
    let (status, body) = raw(
        live,
        &live.token,
        Method::POST,
        &format!(
            "/api/v2/elitea_core/conversations/prompt_lib/{}",
            live.project
        ),
        Some(json!({"name": "handoff live run", "source": "local_work"})),
    )
    .await;
    assert!(status < 300, "create conversation: {status} {body}");
    assert_eq!(body["source"], "local_work");
    body["uuid"].as_str().unwrap().to_owned()
}

fn state_path(live: &Live, uuid: &str) -> String {
    format!(
        "/api/v2/elitea_core/workspace_handoff/prompt_lib/{}/{uuid}",
        live.project
    )
}

struct LiveHarness {
    live: Live,
    host: Arc<HandoffHost>,
    history: Arc<HistoryStore>,
    events: Arc<Events>,
    workspace_id: String,
    folder: std::path::PathBuf,
    app: tempfile::TempDir,
    _dir: tempfile::TempDir,
}

/// A repository with every kind of file a send meets: committed, staged,
/// unstaged, untracked, ignored, a `.env` (platform exclusion), a project
/// exclusion (`secrets/**`) and an explicit include (`.env.example`).
async fn live_harness() -> LiveHarness {
    let live = live();
    let app = tempfile::tempdir().unwrap();
    let dir = tempfile::tempdir().unwrap();
    let folder = std::fs::canonicalize(dir.path()).unwrap().join("repo");
    std::fs::create_dir_all(folder.join("src")).unwrap();
    git(&folder, &["init", "-q"]);
    std::fs::write(folder.join("README.md"), "hello\n").unwrap();
    std::fs::write(folder.join("src/lib.rs"), "pub fn a() {}\n").unwrap();
    std::fs::write(folder.join(".gitignore"), "build/\n").unwrap();
    git(&folder, &["add", "-A"]);
    git(&folder, &["commit", "-q", "-m", "first"]);
    std::fs::write(folder.join("src/staged.rs"), "pub fn s() {}\n").unwrap();
    git(&folder, &["add", "src/staged.rs"]);
    std::fs::write(folder.join("src/lib.rs"), "pub fn a() { /* unstaged */ }\n").unwrap();
    std::fs::write(folder.join("notes.md"), "untracked\n").unwrap();
    std::fs::create_dir_all(folder.join("build")).unwrap();
    std::fs::write(folder.join("build/out.log"), "ignored\n").unwrap();
    std::fs::write(folder.join(".env"), "TOKEN=local\n").unwrap();
    std::fs::create_dir_all(folder.join("secrets")).unwrap();
    std::fs::write(folder.join("secrets/token.txt"), "project exclusion\n").unwrap();
    std::fs::write(folder.join(".env.example"), "EXAMPLE=1\n").unwrap();

    let workspaces = Arc::new(WorkspaceStore::new(app.path().to_owned()));
    let workspace_id = workspaces.add(&folder).unwrap().id;
    workspaces
        .bind_project(&workspace_id, live.project)
        .unwrap();
    let policy = Arc::new(Policy(Mutex::new(
        json!({"allowed": true, "cloud_handoff": true}),
    )));
    let credentials: Arc<dyn Credentials> =
        Arc::new(LiveCreds(live.origin.clone(), live.token.clone()));
    let history = Arc::new(HistoryStore::open(app.path()).unwrap());
    let sessions = Arc::new(crate::sessions::DesktopStateStore::open(app.path()).unwrap());
    let http = crate::net::SharedHttp::new("0.1.0");
    let agents = Arc::new(
        AgentHost::new(HostDeps {
            http: http.clone(),
            credentials: credentials.clone(),
            client_version: "0.1.0".into(),
            policy: policy.clone(),
            workspaces: workspaces.clone(),
            emitter: Arc::new(VecEmitter::default()),
            retry: RetryPolicy {
                attempts: 2,
                delay: Duration::from_millis(5),
            },
            history: Some(history.clone()),
            sessions: Some(sessions.clone()),
            index: None,
            sandbox_deny: crate::d0::turn::app_dirs_for_sandbox([Some(app.path().to_owned())]),
            app_id: None,
            user_rules_dir: None,
            user_commands_dir: None,
            cache_dir: None,
        })
        .unwrap(),
    );
    let global = folder.parent().unwrap().join("gitconfig");
    std::fs::write(&global, "").unwrap();
    let events = Arc::new(Events::default());
    let host = Arc::new(
        HandoffHost::new(HandoffDeps {
            agents,
            http,
            credentials,
            client_version: "0.1.0".into(),
            workspaces,
            policy,
            history: Some(history.clone()),
            data_dir: app.path().to_owned(),
            events: events.clone(),
            device_id: Arc::new(|| None),
            backoff: Backoff::immediate(3),
            git_global_config: Some(global),
            env_source: Arc::new(FixedEnv(vec![
                (
                    "DATABASE_URL".into(),
                    "postgres://live:secret-value@db/x".into(),
                ),
                ("HOME".into(), "/Users/live".into()),
            ])),
        })
        .unwrap(),
    );
    host.settings_set(
        &workspace_id,
        FolderSettings {
            exclude_patterns: Vec::new(),
            include: vec![".env.example".into()],
            env_names: vec!["DATABASE_URL".into()],
        },
    )
    .await
    .unwrap();
    LiveHarness {
        live,
        host,
        history,
        events,
        workspace_id,
        folder,
        app,
        _dir: dir,
    }
}

/// The cloud executor's turn: fetch the newest segment as the desktop
/// does, one commit on the handoff snapshot, a bundle of every ref plus
/// `refs/elitea/cloud`, then Main's own publish (the simulator).
async fn live_cloud_turn(h: &LiveHarness, uuid: &str) -> Value {
    let args = cloud_turn_args(h, uuid).await;
    let args: Vec<&str> = args.iter().map(String::as_str).collect();
    sim(&h.live, &args)
}

/// The simulator's arguments for one cloud turn (its bundle prepared).
async fn cloud_turn_args(h: &LiveHarness, uuid: &str) -> Vec<String> {
    let store = h.host.store(h.live.project, uuid).await.unwrap();
    let plan = store.fetch_plan(0).await.unwrap();
    let segment = plan.segments.last().unwrap().clone();
    let bytes = store.get(&segment.url).await.unwrap();
    let dir = h.app.path().join(format!("cloud-{uuid}"));
    std::fs::create_dir(&dir).unwrap();
    git(&dir, &["init", "-q"]);
    let file = h.app.path().join(format!("sent-{uuid}.bundle"));
    std::fs::write(&file, bytes).unwrap();
    git(
        &dir,
        &[
            "fetch",
            "-q",
            file.to_str().unwrap(),
            "refs/elitea/*:refs/elitea/*",
        ],
    );
    git(
        &dir,
        &["checkout", "-q", "--detach", "refs/elitea/handoff/1"],
    );
    assert!(
        !dir.join(".env").exists(),
        "the excluded .env never reached the cloud"
    );
    assert!(
        !dir.join("secrets/token.txt").exists(),
        "the project exclusion never reached the cloud"
    );
    assert_eq!(
        std::fs::read_to_string(dir.join(".env.example")).unwrap(),
        "EXAMPLE=1\n",
        "the explicit include reached the cloud"
    );
    assert!(dir.join("src/staged.rs").exists() && dir.join("notes.md").exists());
    assert!(!dir.join("build/out.log").exists(), "ignored files stay");
    let trailers = git(&dir, &["log", "-1", "--format=%B", "refs/elitea/handoff/1"]);
    assert!(
        trailers.contains("Elitea-Included: ./.env.example"),
        "{trailers}"
    );
    std::fs::write(dir.join("README.md"), "hello from the cloud\n").unwrap();
    std::fs::write(dir.join("src/cloud.rs"), "pub fn c() {}\n").unwrap();
    git(&dir, &["add", "-A"]);
    git(&dir, &["commit", "-q", "-m", "cloud turn 1"]);
    let commit = git(&dir, &["rev-parse", "HEAD"]);
    git(&dir, &["update-ref", "refs/elitea/cloud", "HEAD"]);
    let out = h.app.path().join(format!("cloud-{uuid}.bundle"));
    let mut args = vec!["bundle", "create", "-q", out.to_str().unwrap()];
    args.extend(segment.refs.keys().map(String::as_str));
    args.push("refs/elitea/cloud");
    git(&dir, &args);
    vec![
        "-project".into(),
        h.live.project.to_string(),
        "-conversation".into(),
        uuid.to_owned(),
        "-bundle".into(),
        out.to_string_lossy().into_owned(),
        "-commit".into(),
        commit,
    ]
}

async fn server_state(h: &LiveHarness, uuid: &str) -> Value {
    raw(
        &h.live,
        &h.live.token,
        Method::GET,
        &state_path(&h.live, uuid),
        None,
    )
    .await
    .1
}

/// Take back while a cloud turn runs: 202 `reclaim_pending`, the turn ends,
/// the thread is `RECLAIMING` one epoch on, and the take-back goes through
/// with that epoch even though the desktop never asked `handoff_status`.
#[tokio::test(flavor = "multi_thread")]
#[ignore = "needs a live stack: IPC.md, Cloud handoff: live run"]
async fn live_take_back_while_a_cloud_turn_runs() {
    let h = live_harness().await;
    let uuid = conversation(&h.live).await;
    let sent = h
        .host
        .send(&h.workspace_id, &uuid, Vec::new(), false)
        .await
        .unwrap();
    let hold = h.app.path().join("turn-may-end");
    let mut args = cloud_turn_args(&h, &uuid).await;
    args.extend(["-hold".to_owned(), hold.to_string_lossy().into_owned()]);
    let child = Command::new(&h.live.sim)
        .args(&args)
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::piped())
        .spawn()
        .unwrap();
    let mut running = Value::Null;
    for _ in 0..100 {
        running = server_state(&h, &uuid).await;
        if running["state"] == "CLOUD_RUNNING" {
            break;
        }
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
    assert_eq!(running["state"], "CLOUD_RUNNING", "{running}");
    let pending = h.host.reclaim(&uuid).await.unwrap();
    assert_eq!(pending["reclaim_pending"], true, "{pending}");
    assert_eq!(pending["state"], "CLOUD_RUNNING");
    assert_eq!(pending["epoch"].as_u64(), Some(sent.epoch));
    assert_eq!(
        h.host.preflight(&uuid, None).await.unwrap_err().code,
        "handoff_not_reclaimed"
    );
    std::fs::write(&hold, "").unwrap();
    let output = child.wait_with_output().unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    let turn: Value = serde_json::from_slice(&output.stdout).unwrap();
    assert_eq!(turn["state"], "RECLAIMING");
    assert_eq!(turn["epoch"].as_u64(), Some(sent.epoch + 1));
    let preflight = h.host.preflight(&uuid, None).await.unwrap();
    assert_eq!(preflight.report.outcome, Outcome::FastPath);
    let applied = h
        .host
        .apply(&preflight.preflight_id, Mode::Apply, None)
        .await
        .unwrap();
    assert!(applied.released, "{applied:?}");
    assert_eq!(applied.release_refused, None);
    assert_eq!(applied.imported_turns, 1);
    assert_eq!(server_state(&h, &uuid).await["state"], "LOCAL");
}

#[tokio::test(flavor = "multi_thread")]
#[ignore = "needs a live stack: IPC.md, Cloud handoff: live run"]
async fn live_send_cloud_turn_take_back_import_and_undo() {
    let h = live_harness().await;
    let uuid = conversation(&h.live).await;

    // 1. policy → preview → send.
    let preview = h
        .host
        .preview(&h.workspace_id, &uuid, Vec::new())
        .await
        .unwrap();
    assert_eq!(preview.policy["source"], "server");
    assert!(
        preview.policy["exclusions"]["project"]
            .as_array()
            .unwrap()
            .contains(&json!("secrets/**")),
        "{}",
        preview.policy
    );
    assert!(preview.findings.is_empty(), "{:?}", preview.findings);
    let excluded: Vec<&str> = preview.excluded.iter().map(|e| e.path.as_str()).collect();
    for path in [".env", "secrets/token.txt"] {
        assert!(excluded.contains(&path), "{path} in {excluded:?}");
    }
    assert!(preview.included.iter().any(|i| i.path == ".env.example"));
    assert!(preview.project_workspace_bytes_left.is_some());
    let offered: Vec<&str> = preview.env["offered"]
        .as_array()
        .unwrap()
        .iter()
        .filter_map(|v| v["name"].as_str())
        .collect();
    assert_eq!(offered, vec!["DATABASE_URL"], "HOME is deny-listed");

    let sent = h
        .host
        .send(&h.workspace_id, &uuid, Vec::new(), false)
        .await
        .unwrap();
    assert_eq!(sent.state, "CLOUD_IDLE");
    assert_eq!(sent.env_vars, 1);
    assert_eq!(sent.included, 1);
    let phases: Vec<&str> = h.events.0.lock().unwrap().iter().map(|e| e.phase).collect();
    for phase in [
        "snapshot", "scan", "bundle", "begin", "upload", "publish", "complete",
    ] {
        assert!(phases.contains(&phase), "{phase} in {phases:?}");
    }

    // 2. state (owner and another member) and the web's take-back request.
    let status = h.host.status(&uuid, None).await.unwrap();
    assert_eq!(status["state"], "CLOUD_IDLE");
    assert_eq!(status["executor"], "cloud");
    assert_eq!(status["read_only"], true);
    assert!(
        status["stats"]["excluded"]
            .as_array()
            .unwrap()
            .contains(&json!("./.env"))
    );
    assert_eq!(
        status["stats"]["excluded_count"].as_u64(),
        status["stats"]["excluded"]
            .as_array()
            .map(|a| a.len() as u64)
    );
    if let Some(other) = h.live.other_token.clone() {
        let (code, body) = raw(
            &h.live,
            &other,
            Method::GET,
            &state_path(&h.live, &uuid),
            None,
        )
        .await;
        // A private thread is not readable by another member (404); a
        // readable one never shows the owner's paths.
        assert!(
            code == 404
                || body
                    .get("stats")
                    .is_some_and(|s| s.get("excluded").is_none()),
            "{code} {body}"
        );
    }
    let (code, body) = raw(
        &h.live,
        &h.live.token,
        Method::POST,
        &format!(
            "/api/v2/elitea_core/workspace_handoff_reclaim_request/prompt_lib/{}/{uuid}",
            h.live.project
        ),
        Some(json!({})),
    )
    .await;
    assert_eq!(code, 202, "{body}");
    let rows = sql(
        &h.live,
        &format!(
            "SELECT meta->>'conversation_uuid', meta->>'conversation_id' FROM centry.notifications \
             WHERE event_type = 'workspace_reclaim_requested' AND meta->>'conversation_uuid' = '{uuid}'"
        ),
    );
    assert_eq!(rows.len(), 1, "{rows:?}");
    assert_eq!(rows[0][1], uuid, "the web toast matches conversation_id");
    // The env values were sent once, never stored on the desktop.
    let env_rows = sql(
        &h.live,
        &format!(
            "SELECT env_names::text FROM elitea_runtime.workspace_handoffs WHERE conversation_uuid = '{uuid}'"
        ),
    );
    assert!(env_rows[0][0].contains("DATABASE_URL"), "{env_rows:?}");

    // 3. one cloud turn.
    let turn = live_cloud_turn(&h, &uuid).await;
    assert_eq!(turn["answer_meta"]["executed_by"], "cloud_executor");
    let status = h.host.status(&uuid, None).await.unwrap();
    assert_eq!(status["state"], "CLOUD_IDLE");
    assert!(status["cloud_sha"].is_string());

    // 4. take back: reclaim, preflight with diffs, apply, release, import.
    assert_eq!(
        h.host.preflight(&uuid, None).await.unwrap_err().code,
        "handoff_not_reclaimed"
    );
    let reclaimed = h.host.reclaim(&uuid).await.unwrap();
    assert_eq!(reclaimed["state"], "RECLAIMING");
    let before = std::fs::read_to_string(h.folder.join("README.md")).unwrap();
    let preflight = h.host.preflight(&uuid, None).await.unwrap();
    assert_eq!(
        preflight.report.outcome,
        Outcome::FastPath,
        "{:?}",
        preflight.report.conflict_paths
    );
    assert_eq!(preflight.cloud_turns, 1);
    let readme = preflight
        .files
        .iter()
        .find(|f| f.path == "README.md")
        .unwrap();
    assert!(
        readme.diff.contains("+hello from the cloud"),
        "{}",
        readme.diff
    );
    assert!(
        preflight
            .files
            .iter()
            .any(|f| f.path == "src/cloud.rs" && f.status == "added")
    );
    let applied = h
        .host
        .apply(&preflight.preflight_id, Mode::Apply, None)
        .await
        .unwrap();
    assert!(applied.applied && applied.released, "{applied:?}");
    assert_eq!(applied.imported_turns, 1);
    assert_eq!(
        std::fs::read_to_string(h.folder.join("README.md")).unwrap(),
        "hello from the cloud\n"
    );
    for (path, text) in [
        (".env", "TOKEN=local\n"),
        ("secrets/token.txt", "project exclusion\n"),
        ("build/out.log", "ignored\n"),
        ("notes.md", "untracked\n"),
        ("src/lib.rs", "pub fn a() { /* unstaged */ }\n"),
    ] {
        assert_eq!(
            std::fs::read_to_string(h.folder.join(path)).unwrap(),
            text,
            "{path}"
        );
    }
    let (_, state) = raw(
        &h.live,
        &h.live.token,
        Method::GET,
        &state_path(&h.live, &uuid),
        None,
    )
    .await;
    assert_eq!(state["state"], "LOCAL", "{state}");
    assert!(!h.host.is_away(&uuid));
    let owner_id: i64 = sql(
        &h.live,
        "SELECT s.user_id FROM elitea_auth.native_sessions s ORDER BY s.user_id LIMIT 1",
    )[0][0]
        .parse()
        .unwrap();
    let owner = Owner {
        origin: h.live.origin.clone(),
        user_id: owner_id,
    };
    let turns = h.history.thread(&owner, &h.workspace_id, &uuid).unwrap();
    assert_eq!(turns.len(), 1, "{turns:?}");
    assert_eq!(turns[0].origin, "cloud");
    assert_eq!(turns[0].prompt, "Make it greet the cloud");
    assert_eq!(
        turns[0].turn_id,
        format!("cloud:{}", turn["answer_uuid"].as_str().unwrap())
    );
    let undone = h
        .host
        .undo(applied.checkpoint_id.as_deref().unwrap())
        .await
        .unwrap();
    assert!(!undone.is_empty());
    assert_eq!(
        std::fs::read_to_string(h.folder.join("README.md")).unwrap(),
        before
    );
    assert!(!h.folder.join("src/cloud.rs").exists());
}

#[tokio::test(flavor = "multi_thread")]
#[ignore = "needs a live stack: IPC.md, Cloud handoff: live run"]
async fn live_cancel_during_handing_off_and_erase_in_cloud_idle() {
    let h = live_harness().await;
    // Cancel while the bytes move: the server's DELETE in HANDING_OFF.
    let uuid = conversation(&h.live).await;
    let host = h.host.clone();
    let cancel_uuid = uuid.clone();
    *h.events.1.lock().unwrap() = Some(Box::new(move |event| {
        if event.phase == "upload" && event.done == 0 {
            host.cancel(&cancel_uuid).unwrap();
        }
    }));
    let error = h
        .host
        .send(&h.workspace_id, &uuid, Vec::new(), false)
        .await
        .unwrap_err();
    assert_eq!(error.code, "handoff_cancelled", "{error:?}");
    *h.events.1.lock().unwrap() = None;
    let (_, state) = raw(
        &h.live,
        &h.live.token,
        Method::GET,
        &state_path(&h.live, &uuid),
        None,
    )
    .await;
    assert_eq!(state["state"], "LOCAL", "{state}");
    assert!(!h.host.is_away(&uuid));
    // The same thread can be sent again right away.
    let sent = h
        .host
        .send(&h.workspace_id, &uuid, Vec::new(), false)
        .await
        .unwrap();
    assert_eq!(sent.state, "CLOUD_IDLE");

    // Erase in CLOUD_IDLE: LOCAL, the folder untouched, the record gone.
    let erased = h.host.erase(&uuid, None).await.unwrap();
    assert_eq!(erased["state"], "LOCAL");
    assert!(!h.host.is_away(&uuid));
    let status = h.host.status(&uuid, Some(h.live.project)).await.unwrap();
    assert_eq!(status["state"], "LOCAL");
    assert_eq!(
        std::fs::read_to_string(h.folder.join("README.md")).unwrap(),
        "hello\n"
    );
    // The objects and the env secret are erased with the thread.
    let rows = sql(
        &h.live,
        &format!(
            "SELECT deleted_at IS NOT NULL, env_secret_name IS NULL FROM elitea_runtime.workspace_handoffs \
             WHERE conversation_uuid = '{uuid}'"
        ),
    );
    assert_eq!(rows, vec![vec!["true".to_owned(), "true".to_owned()]]);
}

#[tokio::test(flavor = "multi_thread")]
#[ignore = "needs a live stack: IPC.md, Cloud handoff: live run"]
async fn live_stale_epoch_erased_release_and_quota() {
    let h = live_harness().await;
    let uuid = conversation(&h.live).await;
    let sent = h
        .host
        .send(&h.workspace_id, &uuid, Vec::new(), false)
        .await
        .unwrap();
    // A stale epoch on the store and on reclaim.
    let store = h.host.store(h.live.project, &uuid).await.unwrap();
    let error = store
        .upload_grant(&elitea_workspace_store::transport::UploadRequest {
            epoch: sent.epoch - 1,
            sha256: "0".repeat(64),
            bytes: 10,
        })
        .await
        .unwrap_err();
    assert_eq!(error.code(), "stale_epoch", "{error:?}");
    let error = h
        .host
        .call(
            Method::POST,
            &format!(
                "/api/v2/elitea_core/workspace_handoff_reclaim/prompt_lib/{}/{uuid}",
                h.live.project
            ),
            Some(&json!({"epoch": sent.epoch - 1, "device_id": "x"})),
        )
        .await
        .unwrap_err();
    assert_eq!(error.code, "stale_epoch", "{error:?}");

    // The cloud copy erased (from the web) between preflight and apply:
    // the take-back still completes locally and the release is not kept
    // for retry forever.
    live_cloud_turn(&h, &uuid).await;
    h.host.reclaim(&uuid).await.unwrap();
    let preflight = h.host.preflight(&uuid, None).await.unwrap();
    let (code, body) = raw(
        &h.live,
        &h.live.token,
        Method::DELETE,
        &state_path(&h.live, &uuid),
        None,
    )
    .await;
    assert_eq!(code, 200, "{body}");
    let applied = h
        .host
        .apply(&preflight.preflight_id, Mode::Discard, None)
        .await
        .unwrap();
    assert_eq!(applied.outcome, "discarded");
    assert_eq!(applied.release_refused.as_deref(), Some("handoff_erased"));
    assert!(applied.released, "nothing is left to retry");
    assert!(!h.host.is_away(&uuid), "an erased thread is LOCAL here too");
    let status = h.host.status(&uuid, Some(h.live.project)).await.unwrap();
    assert_eq!(status["state"], "LOCAL");

    // Quota: a project whose cloud copies may take nothing more.
    let uuid = conversation(&h.live).await;
    sql(
        &h.live,
        &format!(
            "UPDATE p_{}.configuration SET data = data || '{{\"max_project_workspace_gb\": 0}}'::jsonb \
             WHERE type = 'cloud_executor' RETURNING id",
            h.live.project
        ),
    );
    let refused = h
        .host
        .send(&h.workspace_id, &uuid, Vec::new(), false)
        .await
        .unwrap_err();
    // The server's figure, through the policy route, refuses before begin.
    let (begin_code, begin) = raw(
        &h.live,
        &h.live.token,
        Method::POST,
        &state_path(&h.live, &uuid),
        Some(json!({"device_id": "x", "history_mode": "none",
                    "stats": {"files": 1, "bytes": 10, "unpacked_bytes": 10, "excluded": [], "scan_findings": 0},
                    "has_upstream": false, "note": ""})),
    )
    .await;
    sql(
        &h.live,
        &format!(
            "UPDATE p_{}.configuration SET data = data - 'max_project_workspace_gb' WHERE type = 'cloud_executor' RETURNING id",
            h.live.project
        ),
    );
    // The desktop refuses before begin, with the quota in the caps list.
    assert_eq!(refused.code, "workspace_quota_exceeded", "{refused:?}");
    assert_eq!(
        refused.details["violations"][0]["cap"],
        "project_workspace_bytes_left"
    );
    assert_eq!(
        (begin_code, begin["error"].as_str()),
        (413, Some("workspace_quota_exceeded")),
        "{begin}"
    );
    let (_, state) = raw(
        &h.live,
        &h.live.token,
        Method::GET,
        &state_path(&h.live, &uuid),
        None,
    )
    .await;
    assert_eq!(state["state"], "LOCAL");
}
