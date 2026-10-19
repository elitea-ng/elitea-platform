//! The handoff host against an in-process mock of the contract's routes
//! (`testutil::serve`): send → status (read-only) → a cloud turn played
//! with the git CLI → reclaim → preflight → apply → release → the cloud
//! turn imported into the thread history → undo; and the refusals.
//!
//! The mock answers in the shapes the real elitea-main answers (checked
//! against it on 2026-10-16 by `live`, which runs this host against a real
//! server): the flat error envelope, `allowed_refs`/`expected_old` without
//! the upstream ref when the send has none, an upload grant whose
//! `headers` bind `x-amz-checksum-sha256` (a PUT without every header, or
//! with other bytes, is refused as S3 refuses it), the state's
//! `excluded_count`, and `handoff_erased` for a release an erase beat.

use std::collections::{BTreeMap, HashMap};
use std::path::Path;
use std::process::Command;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;
use elitea_local_tools::handoff::apply::{Mode, Outcome};
use elitea_workspace_store::digest::sha256_hex;
use elitea_workspace_store::sync::Backoff;
use serde_json::{Value, json};

use super::{HandoffDeps, HandoffEvent, HandoffEvents, HandoffHost};
use crate::d0::api::{ApiError, Bearer, Credentials};
use crate::d0::events::VecEmitter;
use crate::d0::remote_tools::RetryPolicy;
use crate::d0::turn::{AgentHost, HostDeps, PolicySource};
use crate::handoff_settings::FolderSettings;
use crate::history::{HistoryStore, Owner};
use crate::testutil::{MockServer, Req, Res, serve_dropping};
use crate::workspaces::WorkspaceStore;

const CONVERSATION: &str = "c0ffee00-0000-4000-8000-000000000001";
const PREFIX: &str = "/api/v2/elitea_core";

struct Creds(String);

#[async_trait]
impl Credentials for Creds {
    async fn bearer(&self) -> Result<Bearer, ApiError> {
        Ok(Bearer {
            origin: self.0.clone(),
            token: "elnat_test".into(),
        })
    }
    async fn refreshed(&self) -> Result<Bearer, ApiError> {
        self.bearer().await
    }
    fn identity(&self) -> Option<String> {
        Some(format!("{}#1", self.0))
    }
}

struct Policy(Mutex<Value>);

impl PolicySource for Policy {
    fn local_work(&self) -> Option<Value> {
        Some(self.0.lock().unwrap().clone())
    }
}

type Hook = Box<dyn Fn(&HandoffEvent) + Send + Sync>;

#[derive(Default)]
struct Events(Mutex<Vec<HandoffEvent>>, Mutex<Option<Hook>>);

impl HandoffEvents for Events {
    fn emit(&self, event: HandoffEvent) {
        if let Some(hook) = self.1.lock().unwrap().as_ref() {
            hook(&event);
        }
        self.0.lock().unwrap().push(event);
    }
}

/// The platform's side of the contract, in memory.
#[derive(Default)]
struct Platform {
    origin: String,
    state: String,
    epoch: u64,
    objects: HashMap<String, Vec<u8>>,
    refs: BTreeMap<String, String>,
    seq: u64,
    segment: Option<(String, u64, BTreeMap<String, String>)>,
    begin: Option<Value>,
    groups: Vec<Value>,
    release: Option<Value>,
    fail_release: bool,
    /// `GET workspace_handoff_policy` (`None`: 404, an older server).
    policy: Option<Value>,
    /// The begin answer's `limits` instead of the defaults.
    begin_limits: Option<Value>,
    fail_upload: bool,
    /// Publishes answered `409 thread_busy` (HTTP `Retry-After: 1`, no
    /// details) before one is taken.
    busy_publishes: u32,
    publish_calls: u32,
    /// An erase (from the web) lands between the take-back's preflight and
    /// its release: the release answers 409 `handoff_erased`.
    erase_before_release: bool,
    /// A cloud turn runs: a reclaim answers 202 `reclaim_pending`.
    turn_running: bool,
    releases: u64,
    deletes: u64,
    state_gets: u64,
    /// The begin body's stats (the state route answers them back).
    stats: Value,
}

/// The base64 of the SHA-256 of `bytes` (`x-amz-checksum-sha256`).
fn checksum_header(bytes: &[u8]) -> String {
    use base64::Engine as _;
    use sha2::Digest as _;
    base64::engine::general_purpose::STANDARD.encode(sha2::Sha256::digest(bytes))
}

fn handle(platform: &Mutex<Platform>, req: &Req) -> Res {
    let mut p = platform.lock().unwrap();
    let conv = format!("prompt_lib/1/{CONVERSATION}");
    let path = req.path.split('?').next().unwrap_or_default().to_owned();
    let body: Value = serde_json::from_str(&req.body).unwrap_or(Value::Null);
    let state = |p: &Platform| {
        if p.state == "LOCAL" && p.epoch == 0 {
            return json!({"state": "LOCAL", "epoch": 0});
        }
        let mut stats = p.stats.clone();
        if let Some(stats) = stats.as_object_mut() {
            let count = stats
                .get("excluded")
                .and_then(Value::as_array)
                .map_or(0, Vec::len);
            stats.insert("excluded_count".into(), json!(count));
        }
        json!({"state": p.state, "epoch": p.epoch, "reclaim_pending": false, "handoff_id": "h1",
               "device_name": "Test laptop", "history_mode": "none", "has_upstream": false, "stats": stats,
               "cloud_sha": p.refs.get("refs/elitea/cloud"), "updated_at": "2026-10-16T04:24:12.789394Z",
               "delete_after": null, "workspace_bytes": p.objects.values().map(Vec::len).sum::<usize>()})
    };
    match (req.method.as_str(), path.as_str()) {
        ("POST", x) if x == format!("{PREFIX}/workspace_handoff/{conv}") => {
            p.state = "HANDING_OFF".into();
            p.epoch += 1;
            p.stats = body["stats"].clone();
            let upstream = body["has_upstream"] == json!(true);
            p.begin = Some(body);
            let mut allowed = vec!["refs/elitea/base", "refs/elitea/handoff/1"];
            if upstream {
                allowed.insert(1, "refs/elitea/upstream");
            }
            let expected: serde_json::Map<String, Value> = allowed
                .iter()
                .map(|name| ((*name).to_owned(), Value::Null))
                .collect();
            let limits = p.begin_limits.clone().unwrap_or_else(|| json!({"max_segment_bytes": 209_715_200, "max_file_bytes": 26_214_400, "max_files": 20000,
                               "max_unpacked_bytes": 2_147_483_648_u64, "project_workspace_bytes_left": 53_687_091_200_u64}));
            Res::json(
                201,
                &json!({
                    "handoff_id": "h1", "epoch": p.epoch, "state": "HANDING_OFF",
                    "allowed_refs": allowed,
                    "expected_old": expected,
                    "limits": limits,
                    "expires_at": "2026-10-15T00:15:00Z"
                }),
            )
        }
        ("POST", x) if x == format!("{PREFIX}/workspace_store_upload/{conv}") => {
            assert_eq!(body["epoch"], json!(p.epoch));
            let sha = body["sha256"].as_str().unwrap().to_owned();
            let expires = "2026-10-15T00:15:00Z";
            if p.objects.contains_key(&sha) {
                return Res::json(
                    200,
                    &json!({"exists": true, "put": null, "multipart": null, "expires_at": expires}),
                );
            }
            let digest: Vec<u8> = (0..sha.len())
                .step_by(2)
                .map(|i| u8::from_str_radix(&sha[i..i + 2], 16).unwrap())
                .collect();
            let bound = {
                use base64::Engine as _;
                base64::engine::general_purpose::STANDARD.encode(digest)
            };
            Res::json(
                200,
                &json!({"exists": false, "multipart": null, "expires_at": expires,
                        "put": {"url": format!("{}/obj/{sha}?X-Amz-Signature=s", p.origin),
                                "headers": {"Content-Type": "application/x-git-bundle", "x-amz-checksum-sha256": bound}}}),
            )
        }
        ("GET", x) if x == format!("{PREFIX}/workspace_handoff_policy/prompt_lib/1") => {
            match p.policy.clone() {
                Some(policy) => Res::json(200, &policy),
                None => Res::json(404, &json!({"error": "not_found", "message": "no route"})),
            }
        }
        ("PUT", x) if x.starts_with("/obj/") && p.fail_upload => Res::json(
            503,
            &json!({"error": "unavailable", "message": "storage down"}),
        ),
        ("PUT", x) if x.starts_with("/obj/") => {
            // As S3 answers a presigned PUT: every signed header, and the
            // bytes the checksum names.
            let declared = req.headers.get("x-amz-checksum-sha256").cloned();
            if req.headers.get("content-type").map(String::as_str)
                != Some("application/x-git-bundle")
                || declared.is_none()
            {
                return Res::json(403, &json!({"Code": "SignatureDoesNotMatch"}));
            }
            if declared != Some(checksum_header(&req.raw)) {
                return Res::json(400, &json!({"Code": "BadDigest"}));
            }
            p.objects
                .insert(x.trim_start_matches("/obj/").to_owned(), req.raw.clone());
            Res::json(200, &json!({}))
        }
        ("GET", x) if x.starts_with("/obj/") => Res::bytes(
            200,
            p.objects
                .get(x.trim_start_matches("/obj/"))
                .cloned()
                .unwrap_or_default(),
        ),
        ("POST", x) if x == format!("{PREFIX}/workspace_store_publish/{conv}") => {
            p.publish_calls += 1;
            if p.busy_publishes > 0 {
                p.busy_publishes -= 1;
                let mut res = Res::json(
                    409,
                    &json!({"error": "thread_busy",
                            "message": "Another change to this thread is being saved. Retry the same request shortly."}),
                );
                res.headers.push(("Retry-After", "1".into()));
                return res;
            }
            let sha = body["segment_sha256"].as_str().unwrap().to_owned();
            let Some(object) = p.objects.get(&sha) else {
                return Res::json(
                    422,
                    &json!({"error": "segment_missing", "message": "missing"}),
                );
            };
            assert_eq!(sha256_hex(object), sha);
            let len = object.len() as u64;
            assert_eq!(body["epoch"], json!(p.epoch));
            for update in body["refs"].as_array().unwrap() {
                p.refs.insert(
                    update["name"].as_str().unwrap().into(),
                    update["new"].as_str().unwrap().into(),
                );
            }
            p.seq += 1;
            let refs = p.refs.clone();
            p.segment = Some((sha, len, refs.clone()));
            Res::json(200, &json!({"seq": p.seq, "refs": refs, "superseded": []}))
        }
        ("POST", x) if x == format!("{PREFIX}/workspace_handoff_complete/prompt_lib/1/h1") => {
            assert_eq!(
                body["sha"].as_str(),
                p.refs.get("refs/elitea/handoff/1").map(String::as_str)
            );
            p.state = "CLOUD_IDLE".into();
            p.epoch += 1;
            Res::json(200, &json!({"state": "CLOUD_IDLE", "epoch": p.epoch}))
        }
        ("GET", x) if x == format!("{PREFIX}/workspace_handoff/{conv}") => {
            p.state_gets += 1;
            Res::json(200, &state(&p))
        }
        ("DELETE", x) if x == format!("{PREFIX}/workspace_handoff/{conv}") => {
            p.deletes += 1;
            p.state = "LOCAL".into();
            p.epoch += 1;
            Res::json(200, &json!({"state": "LOCAL", "epoch": p.epoch}))
        }
        ("POST", x) if x == format!("{PREFIX}/workspace_handoff_reclaim/{conv}") => {
            assert_eq!(body["epoch"], json!(p.epoch));
            if p.turn_running {
                p.state = "CLOUD_RUNNING".into();
                return Res::json(
                    202,
                    &json!({"state": "CLOUD_RUNNING", "reclaim_pending": true, "epoch": p.epoch,
                            "waiting_for_execution_id": "0123456789abcdef0123456789abcdef"}),
                );
            }
            p.state = "RECLAIMING".into();
            p.epoch += 1;
            Res::json(
                200,
                &json!({"state": "RECLAIMING", "epoch": p.epoch, "cloud_sha": p.refs.get("refs/elitea/cloud")}),
            )
        }
        ("GET", x) if x == format!("{PREFIX}/workspace_store_fetch/{conv}") => {
            let (sha, bytes, refs) = p.segment.clone().unwrap();
            Res::json(
                200,
                &json!({"head_seq": p.seq, "refs": p.refs, "segments": [
                {"seq": p.seq, "kind": "COMPACT", "sha256": sha, "bytes": bytes, "prerequisites": [], "refs": refs,
                 "url": format!("{}/obj/{sha}", p.origin)}]}),
            )
        }
        ("POST", x) if x == format!("{PREFIX}/workspace_handoff_release/{conv}") => {
            p.releases += 1;
            if p.erase_before_release {
                p.erase_before_release = false;
                p.state = "LOCAL".into();
                p.epoch += 1;
            }
            if p.state == "LOCAL" {
                return Res::json(
                    409,
                    &json!({"error": "handoff_erased", "message": "The cloud copy of this thread was erased, so nothing was handed back."}),
                );
            }
            if p.fail_release {
                p.fail_release = false;
                return Res::json(503, &json!({"error": "unavailable", "message": "later"}));
            }
            if p.state != "RECLAIMING" || body["epoch"] != json!(p.epoch) {
                return Res::json(
                    409,
                    &json!({"error": "stale_epoch", "message": "The thread's state changed. Read the state and retry with its epoch."}),
                );
            }
            p.release = Some(body);
            p.state = "LOCAL".into();
            p.epoch += 1;
            Res::json(
                200,
                &json!({"state": "LOCAL", "epoch": p.epoch, "cloud_copy_delete_after": "2026-10-22T00:00:00Z"}),
            )
        }
        ("GET", x) if x == format!("{PREFIX}/conversation/{conv}") => Res::json(
            200,
            &json!({"uuid": CONVERSATION, "message_groups": p.groups}),
        ),
        ("GET", "/api/v2/social/author") => Res::json(200, &json!({"id": 7})),
        _ => Res::json(404, &json!({"error": "not_found", "message": path})),
    }
}

fn git(dir: &Path, args: &[&str]) -> String {
    let output = Command::new("git")
        .current_dir(dir)
        .args([
            "-c",
            "init.defaultBranch=main",
            "-c",
            "user.name=T",
            "-c",
            "user.email=t@example.invalid",
            "-c",
            "commit.gpgSign=false",
        ])
        .args(args)
        .env("GIT_CONFIG_GLOBAL", "/dev/null")
        .env("GIT_CONFIG_NOSYSTEM", "1")
        .output()
        .unwrap();
    assert!(
        output.status.success(),
        "git {args:?}: {}",
        String::from_utf8_lossy(&output.stderr)
    );
    String::from_utf8_lossy(&output.stdout).trim().to_owned()
}

struct Harness {
    _server: MockServer,
    platform: Arc<Mutex<Platform>>,
    host: Arc<HandoffHost>,
    agents: Arc<AgentHost>,
    history: Arc<HistoryStore>,
    sessions: Arc<crate::sessions::DesktopStateStore>,
    emitter: Arc<VecEmitter>,
    events: Arc<Events>,
    policy: Arc<Policy>,
    workspace_id: String,
    folder: std::path::PathBuf,
    app: tempfile::TempDir,
    _dir: tempfile::TempDir,
}

async fn harness() -> Harness {
    harness_with(Vec::new()).await
}

async fn harness_with(environment: Vec<(String, String)>) -> Harness {
    harness_env(Arc::new(crate::handoff_settings::FixedEnv(environment))).await
}

async fn harness_env(env_source: Arc<dyn crate::handoff_settings::EnvSource>) -> Harness {
    harness_full(env_source, None, None).await
}

/// `drop_first`: the first `n` requests to a path are handled (committed)
/// and then dropped without an answer.
async fn harness_lossy(
    env_source: Arc<dyn crate::handoff_settings::EnvSource>,
    drop_first: Option<(&'static str, usize)>,
) -> Harness {
    harness_full(env_source, drop_first, None).await
}

/// The platform when the agent's own routes are served too (`convo`): the
/// local turn, its commit, `/llm`, the trace routes, and one conversation
/// whose groups are the local turns' and the cloud's.
fn agent_platform(
    platform: &Arc<Mutex<Platform>>,
    convo: &Arc<Mutex<crate::d0::tests::Convo>>,
    inner: &impl Fn(&Req) -> Res,
    req: &Req,
) -> Res {
    let path = req.path.split('?').next().unwrap_or_default();
    if req.method == "GET" && path == format!("{PREFIX}/conversation/prompt_lib/1/{CONVERSATION}") {
        let p = platform.lock().unwrap();
        let c = convo.lock().unwrap();
        let mut groups: Vec<Value> = c.groups.iter().chain(p.groups.iter()).cloned().collect();
        groups.sort_by_key(|g| g["id"].as_i64().unwrap_or(i64::MAX));
        let mut detail = json!({
            "id": 42, "uuid": CONVERSATION, "message_count": groups.len(),
            "participants": [
                {"id": 70, "entity_name": "user", "entity_meta": {"id": 1}},
                {"id": 77, "entity_name": "application", "entity_meta": {"id": 5, "project_id": 1},
                 "entity_settings": {"version_id": 9}}
            ]
        });
        if let Some(limit) = crate::d0::tests::query_of(&req.path, "messages_limit")
            .and_then(|v| v.parse::<usize>().ok())
        {
            if crate::d0::tests::query_of(&req.path, "sort_order").as_deref() == Some("asc") {
                groups.truncate(limit);
            } else {
                groups = groups.into_iter().rev().take(limit).collect();
            }
            detail["message_groups"] = Value::Array(groups);
        }
        return Res::json(200, &detail);
    }
    let agent = [
        "/local_turn/",
        "/local_turn_commit/",
        "/message_trace",
        "/resolved_version/",
    ]
    .iter()
    .any(|part| path.contains(part))
        || path.starts_with("/llm/");
    if agent {
        inner(req)
    } else {
        handle(platform, req)
    }
}

/// The shared harness. `drop_first` drops requests as above; `convo`, when
/// provided, also serves the agent's own routes.
async fn harness_full(
    env_source: Arc<dyn crate::handoff_settings::EnvSource>,
    drop_first: Option<(&'static str, usize)>,
    convo: Option<Arc<Mutex<crate::d0::tests::Convo>>>,
) -> Harness {
    let platform = Arc::new(Mutex::new(Platform {
        state: "LOCAL".into(),
        ..Platform::default()
    }));
    let inner = convo.as_ref().map(|convo| {
        crate::d0::tests::memory_platform_for(
            CONVERSATION,
            convo.clone(),
            crate::d0::tests::memory_script(),
        )
    });
    let server = serve_dropping(drop_first, {
        let platform = platform.clone();
        let convo = convo.clone();
        move |req| match (&inner, &convo) {
            (Some(inner), Some(convo)) => agent_platform(&platform, convo, inner, req),
            _ => handle(&platform, req),
        }
    })
    .await;
    platform.lock().unwrap().origin = server.origin.clone();
    let app = tempfile::tempdir().unwrap();
    let dir = tempfile::tempdir().unwrap();
    let folder = std::fs::canonicalize(dir.path()).unwrap().join("repo");
    std::fs::create_dir_all(folder.join("src")).unwrap();
    git(&folder, &["init", "-q"]);
    std::fs::write(folder.join("README.md"), "hello\n").unwrap();
    std::fs::write(folder.join("src/lib.rs"), "pub fn a() {}\n").unwrap();
    std::fs::write(folder.join(".env"), "TOKEN=local\n").unwrap();
    git(&folder, &["add", "-A"]);
    git(&folder, &["commit", "-q", "-m", "first"]);
    std::fs::write(folder.join("notes.md"), "uncommitted\n").unwrap();
    let workspaces = Arc::new(WorkspaceStore::new(app.path().to_owned()));
    let workspace_id = workspaces.add(&folder).unwrap().id;
    workspaces.bind_project(&workspace_id, 1).unwrap();
    let policy = Arc::new(Policy(Mutex::new(
        json!({"allowed": true, "cloud_handoff": true}),
    )));
    let credentials: Arc<dyn Credentials> = Arc::new(Creds(server.origin.clone()));
    let history = Arc::new(HistoryStore::open(app.path()).unwrap());
    let sessions = Arc::new(crate::sessions::DesktopStateStore::open(app.path()).unwrap());
    let emitter = Arc::new(VecEmitter::default());
    let http = crate::net::SharedHttp::new("0.1.0");
    let agents = Arc::new(
        AgentHost::new(HostDeps {
            http: http.clone(),
            credentials: credentials.clone(),
            client_version: "0.1.0".into(),
            policy: policy.clone(),
            workspaces: workspaces.clone(),
            emitter: emitter.clone(),
            retry: RetryPolicy {
                attempts: 2,
                delay: Duration::from_millis(5),
            },
            history: Some(history.clone()),
            sessions: Some(sessions.clone()),
            index: None,
            sandbox_deny: crate::d0::turn::app_dirs_for_sandbox([Some(app.path().to_owned())]),
            app_id: None,
        })
        .unwrap(),
    );
    // Outside the app data: host git cannot read that (the sandbox denies it).
    let global = folder.parent().unwrap().join("gitconfig");
    std::fs::write(&global, "").unwrap();
    let events = Arc::new(Events::default());
    let host = Arc::new(
        HandoffHost::new(HandoffDeps {
            agents: agents.clone(),
            http,
            credentials,
            client_version: "0.1.0".into(),
            workspaces,
            policy: policy.clone(),
            history: Some(history.clone()),
            data_dir: app.path().to_owned(),
            events: events.clone(),
            device_id: Arc::new(|| Some("dev-1".to_owned())),
            backoff: Backoff::immediate(3),
            git_global_config: Some(global),
            env_source,
        })
        .unwrap(),
    );
    Harness {
        _server: server,
        platform,
        host,
        agents,
        history,
        sessions,
        emitter,
        events,
        policy,
        workspace_id,
        folder,
        app,
        _dir: dir,
    }
}

/// The cloud executor: hydrate the stored segment, one commit, publish a
/// COMPACT with `refs/elitea/cloud`, and the conversation's two messages.
fn cloud_turn(h: &Harness) -> String {
    let (sha, refs) = {
        let p = h.platform.lock().unwrap();
        let (sha, _, refs) = p.segment.clone().unwrap();
        (sha, refs)
    };
    let bytes = h.platform.lock().unwrap().objects[&sha].clone();
    let dir = h.app.path().join("cloud");
    std::fs::create_dir(&dir).unwrap();
    git(&dir, &["init", "-q"]);
    let file = h.app.path().join("sent.bundle");
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
        "the excluded file never reached the cloud"
    );
    std::fs::write(dir.join("README.md"), "hello from the cloud\n").unwrap();
    std::fs::write(dir.join("src/cloud.rs"), "pub fn c() {}\n").unwrap();
    git(&dir, &["add", "-A"]);
    git(&dir, &["commit", "-q", "-m", "cloud turn 1"]);
    let commit = git(&dir, &["rev-parse", "HEAD"]);
    git(&dir, &["update-ref", "refs/elitea/cloud", "HEAD"]);
    let out = h.app.path().join("cloud.bundle");
    let mut args = vec!["bundle", "create", "-q", out.to_str().unwrap()];
    args.extend(refs.keys().map(String::as_str));
    args.push("refs/elitea/cloud");
    git(&dir, &args);
    let segment = std::fs::read(&out).unwrap();
    let sha = sha256_hex(&segment);
    let mut p = h.platform.lock().unwrap();
    p.refs.insert("refs/elitea/cloud".into(), commit.clone());
    let all = p.refs.clone();
    p.seq += 1;
    p.segment = Some((sha.clone(), segment.len() as u64, all));
    p.objects.insert(sha, segment);
    p.groups = vec![
        json!({"id": 10, "uuid": "q1", "meta": {}, "created_at": "2026-10-15 10:00:00.000000",
               "message_items": [{"item_type": "text_message", "item_details": {"content": "Make it greet the cloud"}}]}),
        json!({"id": 11, "uuid": "a1", "reply_to_id": 10, "created_at": "2026-10-15 10:01:00.000000",
               "meta": {"executed_by": "cloud_executor", "workspace_epoch": p.epoch, "workspace_commit": commit},
               "message_items": [{"item_type": "text_message", "item_details": {"content": "Done: README greets the cloud."}}]}),
    ];
    commit
}

#[tokio::test(flavor = "multi_thread")]
async fn send_take_back_import_and_undo() {
    let h = harness().await;
    let preview = h
        .host
        .preview(&h.workspace_id, CONVERSATION, Vec::new())
        .await
        .unwrap();
    assert!(preview.findings.is_empty());
    assert!(
        preview
            .excluded
            .iter()
            .any(|e| e.path == ".env" && e.tracked)
    );
    assert!(preview.bytes > 0);

    let sent = h
        .host
        .send(&h.workspace_id, CONVERSATION, Vec::new(), false)
        .await
        .unwrap();
    assert_eq!(sent.state, "CLOUD_IDLE");
    let phases: Vec<&str> = h.events.0.lock().unwrap().iter().map(|e| e.phase).collect();
    for phase in [
        "snapshot", "scan", "bundle", "upload", "publish", "complete",
    ] {
        assert!(phases.contains(&phase), "{phase} in {phases:?}");
    }
    let begin = h.platform.lock().unwrap().begin.clone().unwrap();
    assert_eq!(begin["history_mode"], "none");
    assert_eq!(begin["device_id"], "dev-1");
    assert_eq!(begin["stats"]["excluded"], json!(["./.env"]));
    assert_eq!(
        h.platform.lock().unwrap().refs.len(),
        2,
        "base and handoff/1, no upstream"
    );

    // Away: read-only, and no local turn.
    let status = h.host.status(CONVERSATION, None).await.unwrap();
    assert_eq!(status["executor"], "cloud");
    assert_eq!(status["read_only"], true);
    assert!(h.host.is_away(CONVERSATION));
    assert_eq!(
        h.host
            .send(&h.workspace_id, CONVERSATION, Vec::new(), false)
            .await
            .unwrap_err()
            .code,
        "thread_executor_is_cloud"
    );

    let commit = cloud_turn(&h);
    // Preflight before reclaim is refused.
    assert_eq!(
        h.host.preflight(CONVERSATION).await.unwrap_err().code,
        "handoff_not_reclaimed"
    );
    let reclaimed = h.host.reclaim(CONVERSATION).await.unwrap();
    assert_eq!(reclaimed["state"], "RECLAIMING");
    let before = std::fs::read_to_string(h.folder.join("README.md")).unwrap();
    let preflight = h.host.preflight(CONVERSATION).await.unwrap();
    assert_eq!(preflight.outcome, Outcome::FastPath);
    assert_eq!(preflight.cloud_turns, 1);
    // Risk 7: per-file unified diffs of what the take-back writes.
    let readme = preflight
        .files
        .iter()
        .find(|file| file.path == "README.md")
        .unwrap();
    assert_eq!(readme.status, "modified");
    assert!(
        readme.diff.contains("+hello from the cloud"),
        "{}",
        readme.diff
    );
    assert!(readme.diff.contains("-hello"));
    assert!(
        preflight
            .files
            .iter()
            .any(|file| file.path == "src/cloud.rs" && file.status == "added")
    );
    assert!(!preflight.diffs_truncated);
    assert_eq!(
        std::fs::read_to_string(h.folder.join("README.md")).unwrap(),
        before,
        "preflight wrote nothing"
    );

    let applied = h
        .host
        .apply(&preflight.preflight_id, Mode::Apply)
        .await
        .unwrap();
    assert!(applied.applied && applied.released);
    assert_eq!(applied.imported_turns, 1);
    assert_eq!(
        std::fs::read_to_string(h.folder.join("README.md")).unwrap(),
        "hello from the cloud\n"
    );
    assert_eq!(
        std::fs::read_to_string(h.folder.join(".env")).unwrap(),
        "TOKEN=local\n"
    );
    let release = h.platform.lock().unwrap().release.clone().unwrap();
    assert_eq!(release["outcome"], "applied");
    assert!(release["applied_sha"].is_string());
    assert!(!h.host.is_away(CONVERSATION));

    // The cloud turn is in the thread history, with its changed files.
    let owner = Owner {
        origin: h.platform.lock().unwrap().origin.clone(),
        user_id: 7,
    };
    let turns = h
        .history
        .thread(&owner, &h.workspace_id, CONVERSATION)
        .unwrap();
    assert_eq!(turns.len(), 1);
    assert_eq!(turns[0].origin, "cloud");
    assert_eq!(turns[0].turn_id, "cloud:a1");
    assert_eq!(turns[0].prompt, "Make it greet the cloud");
    assert_eq!(turns[0].state, "done");
    let changes = turns[0].changes.clone().unwrap();
    let paths: Vec<&str> = changes["files"].as_array().map_or_else(
        || {
            changes
                .as_array()
                .unwrap()
                .iter()
                .filter_map(|c| c["path"].as_str())
                .collect()
        },
        |files| files.iter().filter_map(|c| c["path"].as_str()).collect(),
    );
    assert!(
        paths.contains(&"README.md") && paths.contains(&"src/cloud.rs"),
        "{paths:?}"
    );
    let _ = commit;

    // Undo the take-back.
    h.host
        .undo(applied.checkpoint_id.as_deref().unwrap())
        .await
        .unwrap();
    assert_eq!(
        std::fs::read_to_string(h.folder.join("README.md")).unwrap(),
        before
    );
    assert!(!h.folder.join("src/cloud.rs").exists());
    let _ = &h.agents;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_secret_blocks_the_send_and_the_policy_gates_it() {
    let h = harness().await;
    let key = ["AKIA", "Z7QWERTYUIOPASDF"].concat();
    std::fs::write(h.folder.join("config.ini"), format!("key = {key}\n")).unwrap();
    let error = h
        .host
        .send(&h.workspace_id, CONVERSATION, Vec::new(), false)
        .await
        .unwrap_err();
    assert_eq!(error.code, "secret_found");
    assert_eq!(error.details["findings"][0]["path"], "config.ini");
    assert!(
        h.platform.lock().unwrap().begin.is_none(),
        "nothing reached the platform"
    );
    assert!(
        h.events
            .0
            .lock()
            .unwrap()
            .iter()
            .any(|e| e.phase == "error")
    );
    // Left on the laptop: the send goes through.
    let sent = h
        .host
        .send(
            &h.workspace_id,
            CONVERSATION,
            vec!["config.ini".into()],
            false,
        )
        .await
        .unwrap();
    assert_eq!(sent.state, "CLOUD_IDLE");
    // Off by policy.
    *h.policy.0.lock().unwrap() = json!({"allowed": true});
    assert_eq!(
        h.host
            .preview(&h.workspace_id, CONVERSATION, Vec::new())
            .await
            .unwrap_err()
            .code,
        "cloud_handoff_disabled"
    );
}

#[tokio::test(flavor = "multi_thread")]
async fn a_lost_release_is_kept_and_sent_again_by_status() {
    let h = harness().await;
    h.host
        .send(&h.workspace_id, CONVERSATION, Vec::new(), false)
        .await
        .unwrap();
    cloud_turn(&h);
    h.host.reclaim(CONVERSATION).await.unwrap();
    let preflight = h.host.preflight(CONVERSATION).await.unwrap();
    h.platform.lock().unwrap().fail_release = true;
    let applied = h
        .host
        .apply(&preflight.preflight_id, Mode::Discard)
        .await
        .unwrap();
    assert!(!applied.released);
    assert!(
        h.host.is_away(CONVERSATION),
        "still not LOCAL until the release lands"
    );
    let status = h.host.status(CONVERSATION, None).await.unwrap();
    assert_eq!(status["state"], "LOCAL");
    assert_eq!(
        h.platform.lock().unwrap().release.clone().unwrap()["outcome"],
        "discarded"
    );
    assert!(!h.host.is_away(CONVERSATION));
    // Discard never touched the folder.
    assert_eq!(
        std::fs::read_to_string(h.folder.join("README.md")).unwrap(),
        "hello\n"
    );
}

/// ADR-0034 x #1212: a release that is refused once and sent again by
/// `status` marks the agent's session, so the next local turn re-seeds from
/// the server (the `apply` path is pinned by the take-back test; this is the
/// retry path).
#[tokio::test(flavor = "multi_thread")]
async fn a_retried_release_marks_the_session_for_reseeding() {
    use crate::d0::turn::TurnRequest;
    let convo = Arc::new(Mutex::new(crate::d0::tests::Convo::default()));
    let h = harness_full(
        Arc::new(crate::handoff_settings::FixedEnv(Vec::new())),
        None,
        Some(convo.clone()),
    )
    .await;
    let local = |prompt: &str| TurnRequest {
        workspace_id: h.workspace_id.clone(),
        project_id: 1,
        conversation_id: json!(CONVERSATION),
        application_id: 5,
        version_id: 9,
        prompt: prompt.to_owned(),
        plan_mode: false,
        mentions: Vec::new(),
        skills: Vec::new(),
    };
    let emitter = h.emitter.clone();
    let run = |prompt: &'static str| {
        let started = h.agents.start(local(prompt));
        let (emitter, agents) = (emitter.clone(), h.agents.clone());
        async move {
            let started = started.await.unwrap();
            for _ in 0..500 {
                // Its `done` and its commit: the commit holds the workspace
                // (a send before it settles is refused as busy).
                if emitter
                    .all()
                    .iter()
                    .any(|e| e.kind == "done" && e.turn_id == started.turn_id)
                {
                    tokio::time::timeout(Duration::from_secs(5), agents.commits_idle())
                        .await
                        .expect("the turn's commit settled");
                    return;
                }
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
            panic!("the turn did not finish");
        }
    };
    let seeds = |h: &Harness| {
        h._server
            .seen()
            .iter()
            .filter(|r| r.path.contains("messages_limit=200"))
            .count()
    };

    // A local turn, so the conversation has a session.
    run("create hello.txt with hi").await;
    assert_eq!(seeds(&h), 0);

    // The thread goes to the cloud, one cloud turn comes back.
    h.host
        .send(&h.workspace_id, CONVERSATION, Vec::new(), false)
        .await
        .unwrap();
    cloud_turn(&h);
    h.host.reclaim(CONVERSATION).await.unwrap();
    let preflight = h.host.preflight(CONVERSATION).await.unwrap();
    h.platform.lock().unwrap().fail_release = true;
    let applied = h
        .host
        .apply(&preflight.preflight_id, Mode::Apply)
        .await
        .unwrap();
    assert!(!applied.released, "the release is refused once");

    // `status` sends the release again: it lands, and the session is marked.
    let status = h.host.status(CONVERSATION, None).await.unwrap();
    assert_eq!(status["state"], "LOCAL");
    let origin = h.platform.lock().unwrap().origin.clone();
    let info = h
        .sessions
        .store()
        .info(&origin, 7, CONVERSATION)
        .await
        .unwrap()
        .unwrap();
    assert!(
        !info.seeded_from_server,
        "marked for re-seeding by the retried release"
    );

    // And the next local turn re-seeds from the server.
    run("what did the cloud do?").await;
    assert_eq!(seeds(&h), 1, "one seed from the server");
}

#[test]
fn a_version_1_history_gains_the_turn_origin_column() {
    use std::os::unix::fs::PermissionsExt as _;
    let dir = tempfile::tempdir().unwrap();
    let app = dir.path().join("app");
    std::fs::create_dir_all(&app).unwrap();
    std::fs::set_permissions(&app, std::fs::Permissions::from_mode(0o700)).unwrap();
    let path = app.join(crate::history::FILE_NAME);
    {
        let file = std::fs::File::create(&path).unwrap();
        file.set_permissions(std::fs::Permissions::from_mode(0o600))
            .unwrap();
        let conn = rusqlite::Connection::open(&path).unwrap();
        conn.execute_batch(
            "CREATE TABLE turns (turn_id TEXT PRIMARY KEY NOT NULL, origin TEXT NOT NULL,
                 user_id INTEGER NOT NULL, workspace_id TEXT NOT NULL, conversation_id TEXT NOT NULL,
                 conversation_uuid TEXT, prompt TEXT NOT NULL, mentions TEXT NOT NULL,
                 started_at INTEGER NOT NULL, finished_at INTEGER, changes TEXT,
                 events_bytes INTEGER NOT NULL DEFAULT 0, events_truncated INTEGER NOT NULL DEFAULT 0);
             CREATE TABLE events (turn_id TEXT NOT NULL REFERENCES turns (turn_id) ON DELETE CASCADE,
                 seq INTEGER NOT NULL, kind TEXT NOT NULL, payload TEXT NOT NULL,
                 PRIMARY KEY (turn_id, seq)) WITHOUT ROWID;
             INSERT INTO turns (turn_id, origin, user_id, workspace_id, conversation_id, prompt, mentions, started_at)
                 VALUES ('t1', 'https://x', 7, 'w', 'c', 'old', '[]', 1);
             PRAGMA user_version = 1;",
        )
        .unwrap();
    }
    let store = HistoryStore::open(&app).unwrap();
    let owner = Owner {
        origin: "https://x".into(),
        user_id: 7,
    };
    let turns = store.thread(&owner, "w", "c").unwrap();
    assert_eq!(turns.len(), 1);
    assert_eq!(turns[0].origin, "local");
}

/// Risk 2: a take-back worktree (the folder moved on since the send)
/// carries the folder's `.env`, is clean, and opens as a workspace with git
/// checkpoints once its repository is an open workspace.
#[tokio::test(flavor = "multi_thread")]
async fn a_take_back_worktree_opens_as_a_workspace_with_git_checkpoints() {
    let h = harness().await;
    h.host
        .send(&h.workspace_id, CONVERSATION, Vec::new(), false)
        .await
        .unwrap();
    cloud_turn(&h);
    git(&h.folder, &["add", "-A"]);
    git(&h.folder, &["commit", "-q", "-m", "moved on locally"]);
    h.host.reclaim(CONVERSATION).await.unwrap();
    let preflight = h.host.preflight(CONVERSATION).await.unwrap();
    assert_eq!(preflight.outcome, Outcome::Conflicts);
    let applied = h
        .host
        .apply(&preflight.preflight_id, Mode::Worktree)
        .await
        .unwrap();
    let path = std::path::PathBuf::from(applied.worktree_path.unwrap());
    assert_eq!(
        std::fs::read_to_string(path.join(".env")).unwrap(),
        "TOKEN=local\n"
    );
    assert_eq!(git(&path, &["status", "--porcelain"]), "");
    super::trust_open_workspaces(h.host.deps.workspaces.clone());
    let opened = h.host.deps.workspaces.add(&path).unwrap();
    let session = h.agents.local_session(&opened.id, path.clone()).unwrap();
    assert_eq!(session.checkpoints().kind(), "git");
    assert!(session.checkpoints().git_refusal().is_none());
}

fn server_policy(extra: Value) -> Value {
    let mut policy = json!({
        "enabled": true, "shell": true, "network": true, "history_modes": ["none"],
        "exclusions": [".env", ".env.*", "*.pem", "id_rsa*", "*.sqlite"],
        "exclusions_platform": [".env", ".env.*", "*.pem", "id_rsa*"],
        "exclusions_project": ["*.sqlite"],
        "path_deny": ["private/**"],
        "allow_user_include": true,
        "env_harvest": false, "env_deny": ["PATH", "HOME", "SSH_*"], "env_allow": null,
        "limits": {"max_segment_bytes": 209_715_200, "max_file_bytes": 26_214_400, "max_files": 20000,
                   "max_unpacked_bytes": 2_147_483_648_u64, "project_workspace_bytes_left": 53_687_091_200_u64},
        "env_limits": {"max_vars": 200, "max_bytes": 65536}
    });
    for (key, value) in extra.as_object().unwrap() {
        policy[key] = value.clone();
    }
    policy
}

/// Risk 3: the effective policy is read before begin — its quota refuses a
/// send before any request that changes state; without the route (404) the
/// ADR's defaults apply and the preview says so.
#[tokio::test(flavor = "multi_thread")]
async fn the_policy_is_read_before_begin_and_404_falls_back_to_defaults() {
    let h = harness().await;
    let preview = h
        .host
        .preview(&h.workspace_id, CONVERSATION, Vec::new())
        .await
        .unwrap();
    assert_eq!(preview.policy["source"], "defaults");
    assert_eq!(preview.project_workspace_bytes_left, None);
    h.platform.lock().unwrap().policy = Some(server_policy(json!({
        "limits": {"max_segment_bytes": 209_715_200, "max_file_bytes": 26_214_400, "max_files": 20000,
                   "max_unpacked_bytes": 2_147_483_648_u64, "project_workspace_bytes_left": 10}
    })));
    let preview = h
        .host
        .preview(&h.workspace_id, CONVERSATION, Vec::new())
        .await
        .unwrap();
    assert_eq!(preview.policy["source"], "server");
    assert_eq!(preview.policy["exclusions"]["project"], json!(["*.sqlite"]));
    assert_eq!(
        preview.policy["exclusions"]["path_deny"],
        json!(["private/**"])
    );
    assert_eq!(preview.project_workspace_bytes_left, Some(10));
    assert!(
        preview
            .caps
            .violations
            .iter()
            .any(|v| v.cap == "project_workspace_bytes_left")
    );
    let error = h
        .host
        .send(&h.workspace_id, CONVERSATION, Vec::new(), false)
        .await
        .unwrap_err();
    // The project's quota is what is exceeded: the server's own code.
    assert_eq!(error.code, "workspace_quota_exceeded");
    assert_eq!(error.details["bytes_left"], json!(10));
    assert!(error.details["bytes_needed"].as_u64().unwrap() > 10);
    assert!(
        h.platform.lock().unwrap().begin.is_none(),
        "refused before begin"
    );
    // A per-segment cap stays handoff_too_large; with the quota also over,
    // the breakdown names both.
    h.platform.lock().unwrap().policy = Some(server_policy(json!({
        "limits": {"max_segment_bytes": 5, "max_file_bytes": 26_214_400, "max_files": 20000,
                   "max_unpacked_bytes": 2_147_483_648_u64, "project_workspace_bytes_left": 10}
    })));
    let error = h
        .host
        .send(&h.workspace_id, CONVERSATION, Vec::new(), false)
        .await
        .unwrap_err();
    assert_eq!(error.code, "handoff_too_large");
    let caps: Vec<&str> = error.details["violations"]
        .as_array()
        .unwrap()
        .iter()
        .filter_map(|v| v["cap"].as_str())
        .collect();
    assert_eq!(caps, ["max_segment_bytes", "project_workspace_bytes_left"]);
    assert!(h.platform.lock().unwrap().begin.is_none());
    // A policy that is off refuses at once.
    h.platform.lock().unwrap().policy = Some(server_policy(json!({"enabled": false})));
    assert_eq!(
        h.host
            .preview(&h.workspace_id, CONVERSATION, Vec::new())
            .await
            .unwrap_err()
            .code,
        "cloud_handoff_disabled"
    );
}

/// Risk 4: after begin, a refusal by the begin answer's caps, an upload
/// failure and a cancel by the person each cancel the handoff on the
/// server, so the thread is LOCAL again at once.
#[tokio::test(flavor = "multi_thread")]
async fn a_failure_or_a_cancel_after_begin_returns_the_thread_to_local() {
    let h = harness().await;
    // The begin answer's caps.
    h.platform.lock().unwrap().begin_limits = Some(json!({"max_segment_bytes": 209_715_200,
        "max_file_bytes": 26_214_400, "max_files": 1, "max_unpacked_bytes": 2_147_483_648_u64}));
    let error = h
        .host
        .send(&h.workspace_id, CONVERSATION, Vec::new(), false)
        .await
        .unwrap_err();
    assert_eq!(error.code, "handoff_too_large");
    {
        let p = h.platform.lock().unwrap();
        assert_eq!((p.deletes, p.state.as_str()), (1, "LOCAL"));
    }
    assert!(!h.host.is_away(CONVERSATION));
    // An upload failure.
    {
        let mut p = h.platform.lock().unwrap();
        p.begin_limits = None;
        p.fail_upload = true;
    }
    h.host
        .send(&h.workspace_id, CONVERSATION, Vec::new(), false)
        .await
        .unwrap_err();
    assert_eq!(h.platform.lock().unwrap().deletes, 2);
    assert_eq!(h.platform.lock().unwrap().state, "LOCAL");
    // The person cancels during the upload.
    h.platform.lock().unwrap().fail_upload = false;
    {
        let host = Arc::downgrade(&h.host);
        *h.events.1.lock().unwrap() = Some(Box::new(move |event| {
            if event.phase == "upload"
                && event.done == 0
                && let Some(host) = host.upgrade()
            {
                assert!(host.cancel(CONVERSATION).unwrap());
            }
        }));
    }
    let error = h
        .host
        .send(&h.workspace_id, CONVERSATION, Vec::new(), false)
        .await
        .unwrap_err();
    assert_eq!(error.code, "handoff_cancelled");
    {
        let p = h.platform.lock().unwrap();
        assert_eq!((p.deletes, p.state.as_str()), (3, "LOCAL"));
        assert!(p.segment.is_none(), "nothing was published");
    }
    assert!(
        h.events
            .0
            .lock()
            .unwrap()
            .iter()
            .any(|e| e.phase == "cancelled")
    );
    assert!(!h.host.is_away(CONVERSATION));
    assert!(!h.host.cancel(CONVERSATION).unwrap(), "no send runs now");
    *h.events.1.lock().unwrap() = None;
    // And the next send goes through.
    let sent = h
        .host
        .send(&h.workspace_id, CONVERSATION, Vec::new(), false)
        .await
        .unwrap();
    assert_eq!(sent.state, "CLOUD_IDLE");
}

/// Risk 5: the preview honours `leave_on_laptop`: the counts and the
/// findings are the send's.
#[tokio::test(flavor = "multi_thread")]
async fn the_preview_honours_leave_on_laptop() {
    let h = harness().await;
    let key = ["AKIA", "Z7QWERTYUIOPASDF"].concat();
    std::fs::write(h.folder.join("config.ini"), format!("key = {key}\n")).unwrap();
    let all = h
        .host
        .preview(&h.workspace_id, CONVERSATION, Vec::new())
        .await
        .unwrap();
    assert_eq!(all.findings[0].path, "config.ini");
    let left = h
        .host
        .preview(&h.workspace_id, CONVERSATION, vec!["config.ini".into()])
        .await
        .unwrap();
    assert!(left.findings.is_empty());
    assert_eq!(left.files, all.files - 1);
    assert!(left.unpacked_bytes < all.unpacked_bytes);
    assert!(
        left.excluded
            .iter()
            .any(|e| e.path == "config.ini" && !e.includable)
    );
}

/// Risk 6: the sidebar's batch: a thread never sent from here costs no
/// request; the others are cached for 15 s and refreshed at most once per
/// project per call.
#[tokio::test(flavor = "multi_thread")]
async fn status_many_is_local_first_and_cached() {
    let h = harness().await;
    let other = "c0ffee00-0000-4000-8000-000000000002".to_owned();
    let answer = h
        .host
        .status_many(std::slice::from_ref(&other))
        .await
        .unwrap();
    assert_eq!(answer["statuses"][&other]["state"], "LOCAL");
    assert_eq!(h.platform.lock().unwrap().state_gets, 0);
    h.host
        .send(&h.workspace_id, CONVERSATION, Vec::new(), false)
        .await
        .unwrap();
    let uuids = [CONVERSATION.to_owned(), other.clone()];
    let answer = h.host.status_many(&uuids).await.unwrap();
    assert_eq!(answer["statuses"][CONVERSATION]["executor"], "cloud");
    assert_eq!(answer["statuses"][&other]["executor"], "local");
    assert_eq!(h.platform.lock().unwrap().state_gets, 1);
    for _ in 0..3 {
        h.host.status_many(&uuids).await.unwrap();
    }
    assert_eq!(h.platform.lock().unwrap().state_gets, 1, "cached");
}

static LOG: Mutex<Vec<String>> = Mutex::new(Vec::new());

struct CaptureLog;

impl log::Log for CaptureLog {
    fn enabled(&self, _: &log::Metadata<'_>) -> bool {
        true
    }
    fn log(&self, record: &log::Record<'_>) {
        LOG.lock().unwrap().push(format!("{}", record.args()));
    }
    fn flush(&self) {}
}

fn files_containing(dir: &Path, needle: &[u8], out: &mut Vec<String>) {
    for entry in std::fs::read_dir(dir).unwrap().flatten() {
        let path = entry.path();
        let meta = std::fs::symlink_metadata(&path).unwrap();
        if meta.is_dir() {
            files_containing(&path, needle, out);
        } else if meta.is_file() {
            let bytes = std::fs::read(&path).unwrap_or_default();
            if bytes.windows(needle.len()).any(|w| w == needle) {
                out.push(path.display().to_string());
            }
        }
    }
}

/// The amendment: an explicit include (scanned, its finding needs a
/// confirm per send, named in `included_excluded`) and the env harvest
/// (names filtered, values only in the begin call): a planted value is on
/// no disk under the app data, in no log line and in no event.
#[tokio::test(flavor = "multi_thread")]
async fn includes_and_env_harvest_and_the_value_never_lands_anywhere() {
    let _ = log::set_logger(&CaptureLog).map(|()| log::set_max_level(log::LevelFilter::Trace));
    log::info!("capture probe");
    assert!(
        LOG.lock()
            .unwrap()
            .iter()
            .any(|line| line == "capture probe"),
        "the log is captured"
    );
    let planted = "pl4nted-Secret-Value-7c1e";
    // The value comes from a login shell's rc file, as in real life (the
    // script lives outside the app data the test searches).
    let shells = tempfile::tempdir().unwrap();
    let shell = fake_shell(
        shells.path(),
        &format!("export DATABASE_URL='{planted}'\nexport SSH_AUTH_SOCK=/tmp/agent"),
    );
    let h = harness_env(Arc::new(crate::handoff_settings::LoginShellEnv {
        shell: Some(shell),
        timeout: Duration::from_secs(10),
    }))
    .await;
    h.platform.lock().unwrap().policy = Some(server_policy(json!({"env_harvest": true})));
    let key = ["AKIA", "Z7QWERTYUIOPASDF"].concat();
    std::fs::write(h.folder.join(".env"), format!("AWS_ACCESS_KEY_ID={key}\n")).unwrap();
    // The folder's settings: include `.env`, select DATABASE_URL.
    let got = h.host.settings_get(&h.workspace_id).await.unwrap();
    let offered: Vec<&str> = got["env_offered"]
        .as_array()
        .unwrap()
        .iter()
        .map(|v| v["name"].as_str().unwrap())
        .collect();
    assert!(offered.contains(&"DATABASE_URL"), "{offered:?}");
    for denied in ["PATH", "HOME", "SSH_AUTH_SOCK"] {
        assert!(!offered.contains(&denied), "{denied} is denied");
    }
    let database = got["env_offered"]
        .as_array()
        .unwrap()
        .iter()
        .find(|v| v["name"] == "DATABASE_URL")
        .unwrap();
    assert_eq!(database["length"], planted.len());
    assert!(!got.to_string().contains(planted));
    assert_eq!(
        h.host
            .settings_set(
                &h.workspace_id,
                FolderSettings {
                    include: vec!["../outside".into()],
                    ..FolderSettings::default()
                }
            )
            .await
            .unwrap_err()
            .code,
        "invalid_request"
    );
    h.host
        .settings_set(
            &h.workspace_id,
            FolderSettings {
                exclude_patterns: vec!["notes.md".into()],
                include: vec![".env".into()],
                env_names: vec!["DATABASE_URL".into()],
            },
        )
        .await
        .unwrap();
    let preview = h
        .host
        .preview(&h.workspace_id, CONVERSATION, Vec::new())
        .await
        .unwrap();
    assert!(preview.findings.is_empty());
    assert_eq!(preview.include_findings[0].path, ".env");
    assert_eq!(preview.included[0].path, ".env");
    assert!(
        preview.excluded.iter().any(|e| e.path == "notes.md"
            && e.reason == elitea_local_tools::handoff::exclude::Reason::User)
    );
    assert_eq!(preview.env["selected"], json!(["DATABASE_URL"]));
    assert!(!serde_json::to_string(&preview).unwrap().contains(planted));
    // The include's finding needs a confirm for this send.
    let error = h
        .host
        .send(&h.workspace_id, CONVERSATION, Vec::new(), false)
        .await
        .unwrap_err();
    assert_eq!(error.code, "include_confirm_required");
    assert!(h.platform.lock().unwrap().begin.is_none());
    let sent = h
        .host
        .send(&h.workspace_id, CONVERSATION, Vec::new(), true)
        .await
        .unwrap();
    assert_eq!((sent.included, sent.env_vars), (1, 1));
    let begin = h.platform.lock().unwrap().begin.clone().unwrap();
    assert_eq!(begin["included_excluded"], json!(["./.env"]));
    assert_eq!(begin["env"], json!({"DATABASE_URL": planted}));
    assert!(!begin["stats"]["excluded"].to_string().contains(".env\""));
    // The value is nowhere on disk under the app data (records, settings,
    // threads.sqlite, shadows), in no log line and in no event.
    let mut found = Vec::new();
    files_containing(h.app.path(), planted.as_bytes(), &mut found);
    assert!(found.is_empty(), "{found:?}");
    assert!(
        !LOG.lock()
            .unwrap()
            .iter()
            .any(|line| line.contains(planted))
    );
    assert!(
        !h.events
            .0
            .lock()
            .unwrap()
            .iter()
            .any(|e| format!("{e:?}").contains(planted))
    );
    // The segment carries the included file and never the env.
    let p = h.platform.lock().unwrap();
    let (sha, _, _) = p.segment.clone().unwrap();
    assert!(
        !p.objects[&sha]
            .windows(planted.len())
            .any(|w| w == planted.as_bytes())
    );
}

/// A login shell for tests: `body` (its rc), then the `-c` script.
fn fake_shell(dir: &Path, body: &str) -> std::path::PathBuf {
    use std::os::unix::fs::PermissionsExt as _;
    let path = dir.join(format!("shell-{}", body.len()));
    std::fs::write(
        &path,
        format!("#!/bin/sh\n{body}\nexec /bin/sh -c \"$4\"\n"),
    )
    .unwrap();
    std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o755)).unwrap();
    path
}

/// The env harvest reads the login shell: what its rc exports is offered
/// (deny-filtered); a shell that fails or hangs offers nothing, with a
/// notice, and the preview still works.
#[tokio::test(flavor = "multi_thread")]
async fn the_harvest_offers_the_login_shell_environment() {
    let shells = tempfile::tempdir().unwrap();
    let rc = "export DATABASE_URL=postgres://dev\nexport SSH_AUTH_SOCK=/tmp/agent";
    let h = harness_env(Arc::new(crate::handoff_settings::LoginShellEnv {
        shell: Some(fake_shell(shells.path(), rc)),
        timeout: Duration::from_secs(10),
    }))
    .await;
    h.platform.lock().unwrap().policy = Some(server_policy(json!({"env_harvest": true})));
    let preview = h
        .host
        .preview(&h.workspace_id, CONVERSATION, Vec::new())
        .await
        .unwrap();
    let names: Vec<&str> = preview.env["offered"]
        .as_array()
        .unwrap()
        .iter()
        .map(|v| v["name"].as_str().unwrap())
        .collect();
    assert!(names.contains(&"DATABASE_URL"), "{names:?}");
    assert!(!names.contains(&"SSH_AUTH_SOCK") && !names.contains(&"PATH"));
    assert!(preview.env["notice"].is_null());
    for (body, timeout) in [
        ("exit 4", Duration::from_secs(10)),
        ("sleep 30", Duration::from_millis(300)),
    ] {
        let h = harness_env(Arc::new(crate::handoff_settings::LoginShellEnv {
            shell: Some(fake_shell(shells.path(), body)),
            timeout,
        }))
        .await;
        h.platform.lock().unwrap().policy = Some(server_policy(json!({"env_harvest": true})));
        let preview = h
            .host
            .preview(&h.workspace_id, CONVERSATION, Vec::new())
            .await
            .unwrap();
        assert_eq!(preview.env["offered"], json!([]), "{body}");
        assert!(preview.env["notice"].is_string(), "{body}");
        let got = h.host.settings_get(&h.workspace_id).await.unwrap();
        assert_eq!(got["env_offered"], json!([]));
        assert!(got["env_notice"].is_string());
    }
}

/// Live run 2026-10-16: an erase (from the web) between the take-back's
/// preflight and its release made the release answer 409 `handoff_erased`
/// for ever; the host kept it as pending and the thread read-only. A
/// release refused for good is not retried, and the thread follows the
/// server.
#[tokio::test(flavor = "multi_thread")]
async fn a_release_refused_for_good_is_not_retried() {
    let h = harness().await;
    h.host
        .send(&h.workspace_id, CONVERSATION, Vec::new(), false)
        .await
        .unwrap();
    cloud_turn(&h);
    h.host.reclaim(CONVERSATION).await.unwrap();
    let preflight = h.host.preflight(CONVERSATION).await.unwrap();
    h.platform.lock().unwrap().erase_before_release = true;
    let applied = h
        .host
        .apply(&preflight.preflight_id, Mode::Apply)
        .await
        .unwrap();
    assert_eq!(applied.release_refused.as_deref(), Some("handoff_erased"));
    assert!(applied.released, "nothing is kept to send again");
    assert!(!h.host.is_away(CONVERSATION));
    assert_eq!(
        applied.imported_turns, 1,
        "the cloud turn's messages remain"
    );
    let status = h.host.status(CONVERSATION, None).await.unwrap();
    assert_eq!(status["state"], "LOCAL");
    assert_eq!(h.platform.lock().unwrap().releases, 1, "not sent again");
}

/// Live run 2026-10-16: a take-back asked while a cloud turn runs is
/// answered 202 `reclaim_pending`; the thread becomes RECLAIMING one epoch
/// later when the turn ends. The release must carry that epoch, not the
/// 202's (it was refused `stale_epoch`, leaving the server RECLAIMING with
/// the folder already applied).
#[tokio::test(flavor = "multi_thread")]
async fn a_take_back_asked_during_a_cloud_turn_releases_with_the_later_epoch() {
    let h = harness().await;
    h.host
        .send(&h.workspace_id, CONVERSATION, Vec::new(), false)
        .await
        .unwrap();
    cloud_turn(&h);
    h.platform.lock().unwrap().turn_running = true;
    let pending = h.host.reclaim(CONVERSATION).await.unwrap();
    assert_eq!(pending["reclaim_pending"], true);
    assert_eq!(
        h.host.preflight(CONVERSATION).await.unwrap_err().code,
        "handoff_not_reclaimed"
    );
    {
        // The turn ends: RECLAIMING, one epoch on.
        let mut p = h.platform.lock().unwrap();
        p.turn_running = false;
        p.state = "RECLAIMING".into();
        p.epoch += 1;
    }
    let preflight = h.host.preflight(CONVERSATION).await.unwrap();
    let applied = h
        .host
        .apply(&preflight.preflight_id, Mode::Apply)
        .await
        .unwrap();
    assert!(applied.released, "{applied:?}");
    assert_eq!(applied.release_refused, None);
    assert_eq!(h.platform.lock().unwrap().state, "LOCAL");
}

#[path = "handoff_live_tests.rs"]
mod live;

/// A thread another writer is saving answers the publish `409 thread_busy`;
/// the host sends the same publish again and the send still lands, once.
#[tokio::test(flavor = "multi_thread")]
async fn a_busy_thread_does_not_fail_the_send() {
    let h = harness().await;
    h.platform.lock().unwrap().busy_publishes = 1;
    let sent = h
        .host
        .send(&h.workspace_id, CONVERSATION, Vec::new(), false)
        .await
        .unwrap();
    assert_eq!(sent.state, "CLOUD_IDLE");
    let p = h.platform.lock().unwrap();
    assert_eq!(p.publish_calls, 2, "one busy answer, then the publish");
    assert_eq!(p.seq, 1);
}

/// A begin the server committed whose answer was lost (a dropped
/// connection) is cancelled: the thread is LOCAL again and the next send
/// goes through.
#[tokio::test(flavor = "multi_thread")]
async fn a_lost_begin_answer_does_not_strand_the_thread() {
    let h = harness_lossy(
        Arc::new(crate::handoff_settings::FixedEnv(Vec::new())),
        Some((
            "/api/v2/elitea_core/workspace_handoff/prompt_lib/1/c0ffee00-0000-4000-8000-000000000001",
            1,
        )),
    )
    .await;
    // The first request to that path is the begin POST (committed, then
    // dropped).
    h.host
        .send(&h.workspace_id, CONVERSATION, Vec::new(), false)
        .await
        .unwrap_err();
    {
        let p = h.platform.lock().unwrap();
        assert_eq!(p.deletes, 1, "the committed begin was cancelled");
        assert_eq!(p.state, "LOCAL");
    }
    assert!(!h.host.is_away(CONVERSATION));
    let sent = h
        .host
        .send(&h.workspace_id, CONVERSATION, Vec::new(), false)
        .await
        .unwrap();
    assert_eq!(sent.state, "CLOUD_IDLE");
}

/// A cancel that lands while `complete` is in flight does not abandon: the
/// fresh cloud copy stays.
#[tokio::test(flavor = "multi_thread")]
async fn a_cancel_during_complete_keeps_the_cloud_copy() {
    let h = harness().await;
    {
        let host = Arc::downgrade(&h.host);
        *h.events.1.lock().unwrap() = Some(Box::new(move |event| {
            if event.phase == "publish"
                && event.done == 1
                && let Some(host) = host.upgrade()
            {
                assert!(host.cancel(CONVERSATION).unwrap());
            }
        }));
    }
    let sent = h
        .host
        .send(&h.workspace_id, CONVERSATION, Vec::new(), false)
        .await
        .unwrap();
    assert_eq!(sent.state, "CLOUD_IDLE");
    {
        let p = h.platform.lock().unwrap();
        assert_eq!((p.deletes, p.state.as_str()), (0, "CLOUD_IDLE"));
        assert!(p.segment.is_some());
    }
    assert!(h.host.is_away(CONVERSATION));
}

/// A failed `complete` whose server state is already past HANDING_OFF is
/// not deleted (the delete would erase the cloud copy).
#[tokio::test(flavor = "multi_thread")]
async fn an_abandon_never_deletes_a_completed_handoff() {
    let h = harness().await;
    h.platform.lock().unwrap().state = "CLOUD_IDLE".into();
    h.platform.lock().unwrap().epoch = 3;
    h.host.abandon(1, CONVERSATION).await;
    assert_eq!(h.platform.lock().unwrap().deletes, 0);
}

/// ADR-0034 x #1212: a take-back brings the cloud's turns into the
/// conversation; the agent's session is marked, the next local turn
/// re-seeds from the server, and its `/llm` request carries the cloud
/// turns' messages and tool calls after the local ones.
#[tokio::test(flavor = "multi_thread")]
async fn a_take_back_makes_the_next_local_turn_see_the_cloud_turns() {
    use crate::d0::turn::TurnRequest;
    let convo = Arc::new(Mutex::new(crate::d0::tests::Convo::default()));
    let h = harness_full(
        Arc::new(crate::handoff_settings::FixedEnv(Vec::new())),
        None,
        Some(convo.clone()),
    )
    .await;
    let local = |prompt: &str| TurnRequest {
        workspace_id: h.workspace_id.clone(),
        project_id: 1,
        conversation_id: json!(CONVERSATION),
        application_id: 5,
        version_id: 9,
        prompt: prompt.to_owned(),
        plan_mode: false,
        mentions: Vec::new(),
        skills: Vec::new(),
    };
    let emitter = h.emitter.clone();
    let run = |prompt: &'static str| {
        let started = h.agents.start(local(prompt));
        let (emitter, agents) = (emitter.clone(), h.agents.clone());
        async move {
            let started = started.await.unwrap();
            for _ in 0..500 {
                // Its `done` and its commit: the commit holds the workspace
                // (a send before it settles is refused as busy).
                if emitter
                    .all()
                    .iter()
                    .any(|e| e.kind == "done" && e.turn_id == started.turn_id)
                {
                    tokio::time::timeout(Duration::from_secs(5), agents.commits_idle())
                        .await
                        .expect("the turn's commit settled");
                    return;
                }
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
            panic!("the turn did not finish");
        }
    };
    let llm = |h: &Harness| -> Vec<Value> {
        h._server
            .seen()
            .into_iter()
            .filter(|r| r.path == "/llm/v1/chat/completions")
            .map(|r| serde_json::from_str(&r.body).unwrap())
            .collect()
    };
    let seeds = |h: &Harness| {
        h._server
            .seen()
            .iter()
            .filter(|r| r.path.contains("messages_limit=200"))
            .count()
    };

    // A local turn: it writes hello.txt.
    run("create hello.txt with hi").await;
    assert_eq!(seeds(&h), 0);

    // The thread goes to the cloud and two cloud turns come back.
    h.host
        .send(&h.workspace_id, CONVERSATION, Vec::new(), false)
        .await
        .unwrap();
    let commit = cloud_turn(&h);
    {
        let mut p = h.platform.lock().unwrap();
        // The helper wrote cloud turn 1 (groups 10 and 11); add turn 2.
        p.groups.push(json!({"id": 12, "uuid": "q2", "meta": {}, "created_at": "2026-10-15 10:02:00.000000",
            "message_items": [{"item_type": "text_message", "item_details": {"content": "Now add a changelog"}}]}));
        p.groups.push(json!({"id": 13, "uuid": "a2", "reply_to_id": 12, "created_at": "2026-10-15 10:03:00.000000",
            "meta": {"executed_by": "cloud_executor", "workspace_epoch": 2},
            "message_items": [{"item_type": "text_message", "item_details": {"content": "Changelog added."}}]}));
        assert!(commit.len() > 8);
    }
    {
        let mut c = convo.lock().unwrap();
        // Their tool calls, as the trace routes hold them.
        c.steps.push(json!({"id": 901, "message_group_id": 11, "kind": "tool_call", "tool_name": "edit_file",
            "tool_inputs": {"path": "README.md"}, "tool_output": "{\"edited\":true}", "is_error": false}));
        c.steps.push(json!({"id": 902, "message_group_id": 13, "kind": "tool_call", "tool_name": "write_file",
            "tool_inputs": {"path": "CHANGELOG.md"}, "tool_output": "{\"written\":true}", "is_error": false}));
    }
    h.host.reclaim(CONVERSATION).await.unwrap();
    let preflight = h.host.preflight(CONVERSATION).await.unwrap();
    let applied = h
        .host
        .apply(&preflight.preflight_id, Mode::Apply)
        .await
        .unwrap();
    assert!(applied.released);
    assert_eq!(applied.imported_turns, 2, "two cloud turns imported");

    // The take-back marked the session: the next local turn re-seeds.
    let origin = h.platform.lock().unwrap().origin.clone();
    let info = h
        .sessions
        .store()
        .info(&origin, 7, CONVERSATION)
        .await
        .unwrap()
        .unwrap();
    assert!(
        !info.seeded_from_server,
        "marked for re-seeding by the release"
    );
    let before = llm(&h).len();
    run("what did the cloud do?").await;
    assert_eq!(seeds(&h), 1, "one seed from the server");
    let bodies = llm(&h);
    let body = &bodies[before];
    let text = body.to_string();
    for wanted in [
        "create hello.txt with hi", // the local turn
        "Created hello.txt.",
        "Make it greet the cloud", // cloud turn 1
        "Done: README greets the cloud.",
        "edit_file",           // its tool call
        "Now add a changelog", // cloud turn 2
        "Changelog added.",
        "CHANGELOG.md",
        "what did the cloud do?",
    ] {
        assert!(text.contains(wanted), "{wanted} in {text}");
    }
    // In order: the local turn first, the cloud's after it, the new prompt last.
    let at = |needle: &str| text.find(needle).unwrap();
    assert!(at("create hello.txt with hi") < at("Make it greet the cloud"));
    assert!(at("Make it greet the cloud") < at("Now add a changelog"));
    assert!(at("Now add a changelog") < at("what did the cloud do?"));
    let tool_results: Vec<&str> = body["messages"]
        .as_array()
        .unwrap()
        .iter()
        .filter(|m| m["role"] == "tool")
        .filter_map(|m| m["content"].as_str())
        .collect();
    assert_eq!(tool_results.len(), 3, "{tool_results:?}");
    // The rows say who ran what.
    let rows = h
        .sessions
        .store()
        .rows(&crate::sessions::session_key(&origin, 7, CONVERSATION))
        .await
        .unwrap();
    assert!(
        rows.iter().any(|r| r.executed_by == "cloud_executor"),
        "{rows:?}"
    );
    assert!(rows.iter().any(|r| r.executed_by == "desktop"));
    assert_eq!(
        rows.iter()
            .filter(|r| r.executed_by == "cloud_executor")
            .map(|r| r.response_message_id.clone().unwrap())
            .collect::<std::collections::BTreeSet<_>>()
            .len(),
        2
    );
    // And the next turn needs no seed.
    run("thanks").await;
    assert_eq!(seeds(&h), 1);
}
