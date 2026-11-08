//! The ADR-0049 decision 9 bake-off runner (#1599): every task of
//! `bakeoff/corpus.json` on each engine, through the same browser tools the
//! agent uses, by the same scripted agent on every engine.
//!
//! Gated by `ELITEA_BAKEOFF_OUT` (a JSON-lines file it writes); without it
//! the test reports a skip. The browser-bakeoff workflow runs it inside the
//! lite session image under gVisor, as production runs a session, with:
//!
//! * `ELITEA_BROWSER_ENGINE`: chrome-headless-shell (`full`, and `auto`'s
//!   fallback); `ELITEA_BROWSER_MOLI`: Moli (`lite`, `auto`);
//! * `ELITEA_BAKEOFF_ENGINES`: the engines to run (default `full,lite,auto`);
//! * `ELITEA_BAKEOFF_CORPUS`: the corpus (default the checked-in one);
//! * `ELITEA_BAKEOFF_ONLY`: a comma-separated task id filter.
//!
//! One line per engine and task. It holds measurements only, no verdict:
//! `scripts/ci/browser-bakeoff-report.mjs` applies the admission rule. A
//! fresh session per task, so one engine crash costs one task:
//!
//! * `ok`: every expectation held (see the corpus's `about`);
//! * `refs`: the actionable refs of the first tree snapshot;
//! * `first_snapshot_ms`: from `browser_open` to the first tree snapshot's
//!   answer; `start_ms`: the engine start;
//! * `peak_memory_bytes`: the largest sum, sampled every 200 ms, of the
//!   engine's process tree (PSS from `/proc/<pid>/smaps_rollup`, else RSS),
//!   with `memory_source`; null where `/proc` has neither;
//! * `crashed`: the engine was gone after the task; `hung`: the task passed
//!   [`TASK_LIMIT`] and was abandoned;
//! * `engine_end` and `switched`: the engine that answered last, and an
//!   `auto` switch's reason.
//!
//! The scripted agent is the bake-off's "same agent": each task names its
//! steps (click or type by role and name, wait), so every engine sees the
//! same calls in the same order and no model cost or variance enters the
//! comparison.
// The engines run on unix only (their debugging pipe).
#![cfg(unix)]

use std::collections::{HashMap, HashSet};
use std::path::PathBuf;
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::{Duration, Instant};

use elitea_browser_session::engine::EngineKind;
use elitea_browser_session::protocol::{Backend, Start};
use elitea_browser_session::session::{Engines, Session};
use elitea_browser_tools::frame::unframe;
use elitea_browser_tools::spec;
use serde_json::{Value, json};

/// Past this a task is abandoned and recorded as hung.
const TASK_LIMIT: Duration = Duration::from_secs(180);
/// The engine start's own limit (the protocol's `ready` limit).
const START_LIMIT: Duration = Duration::from_secs(60);

fn env_path(name: &str) -> Option<PathBuf> {
    std::env::var_os(name).map(PathBuf::from)
}

fn ms(elapsed: Duration) -> u64 {
    u64::try_from(elapsed.as_millis()).unwrap_or(u64::MAX)
}

/// The content inside a result's untrusted frame.
fn content(result: &Value) -> String {
    let framed = result["content"].as_str().unwrap_or_default();
    unframe(framed)
        .map_or(framed, |(inner, _)| inner)
        .to_owned()
}

/// `*` matches any run of characters; everything else is literal.
fn glob(pattern: &str, text: &str) -> bool {
    let parts: Vec<&str> = pattern.split('*').collect();
    let mut rest = text;
    for (index, part) in parts.iter().enumerate() {
        if index == 0 {
            let Some(after) = rest.strip_prefix(part) else {
                return false;
            };
            rest = after;
        } else if index == parts.len() - 1 {
            return rest.ends_with(part);
        } else if let Some(at) = rest.find(part) {
            rest = &rest[at + part.len()..];
        } else {
            return false;
        }
    }
    rest.is_empty() || parts.len() > 1
}

/// The page URL from a header's `# url "<json string>"` line.
fn page_url(text: &str) -> String {
    text.lines()
        .find_map(|line| line.strip_prefix("# url "))
        .and_then(|quoted| serde_json::from_str::<String>(quoted).ok())
        .unwrap_or_default()
}

/// The first `[ref=eN]` on a tree line whose role is one of `roles`
/// (`a|b`) and whose name contains `name`, case-insensitively.
fn find_ref(tree: &str, roles: &str, name: &str) -> Option<String> {
    let roles: Vec<&str> = roles.split('|').collect();
    let name = name.to_lowercase();
    tree.lines().find_map(|line| {
        let node = line.trim_start().strip_prefix("- ")?;
        let (role, rest) = node.split_once(' ').unwrap_or((node, ""));
        if !roles.contains(&role) || !rest.to_lowercase().contains(&name) {
            return None;
        }
        let at = rest.find("[ref=")?;
        let tail = &rest[at + 5..];
        Some(tail[..tail.find(']')?].to_owned())
    })
}

/// The PSS (else RSS) of `root` and its descendants, from `/proc`.
fn tree_memory(root: u32) -> Option<(u64, &'static str)> {
    let mut parents: HashMap<u32, u32> = HashMap::new();
    for entry in std::fs::read_dir("/proc").ok()?.flatten() {
        let Some(pid) = entry
            .file_name()
            .to_str()
            .and_then(|n| n.parse::<u32>().ok())
        else {
            continue;
        };
        let Ok(stat) = std::fs::read_to_string(entry.path().join("stat")) else {
            continue;
        };
        // `pid (comm) state ppid ...`: comm may hold spaces, so split after
        // the last `)`.
        let parent = stat
            .rsplit_once(')')
            .and_then(|(_, rest)| rest.split_whitespace().nth(1))
            .and_then(|parent| parent.parse().ok());
        if let Some(parent) = parent {
            parents.insert(pid, parent);
        }
    }
    let mut members = HashSet::from([root]);
    loop {
        let before = members.len();
        for (pid, ppid) in &parents {
            if members.contains(ppid) {
                members.insert(*pid);
            }
        }
        if members.len() == before {
            break;
        }
    }
    let field = |path: String, key: &str| -> Option<u64> {
        let text = std::fs::read_to_string(path).ok()?;
        text.lines().find_map(|line| {
            let value = line.strip_prefix(key)?.trim().strip_suffix("kB")?.trim();
            value.parse::<u64>().ok().map(|kib| kib * 1024)
        })
    };
    let mut total = 0;
    let mut source = "pss";
    let mut any = false;
    for pid in members {
        if let Some(pss) = field(format!("/proc/{pid}/smaps_rollup"), "Pss:") {
            total += pss;
            any = true;
        } else if let Some(rss) = field(format!("/proc/{pid}/status"), "VmRSS:") {
            total += rss;
            source = "rss";
            any = true;
        }
    }
    any.then_some((total, source))
}

struct Outcome {
    ok: bool,
    refs: Option<u64>,
    first_snapshot_ms: Option<u64>,
    failure: Option<String>,
    switched: Option<String>,
}

async fn call(session: &Session, name: &str, args: &Value, switched: &mut Option<String>) -> Value {
    let result = session.call(name, args).await.result().clone();
    if let Some(reason) = result["engine_switched"]["reason"].as_str() {
        switched.get_or_insert_with(|| reason.to_owned());
    }
    result
}

/// One task's steps and checks on a started session.
#[allow(
    clippy::too_many_lines,
    reason = "one task: open, first snapshot, the scripted steps, then every check in order"
)]
async fn run_task(session: &Session, task: &Value) -> Outcome {
    let mut switched = None;
    let fail = |why: String, refs, first, switched| Outcome {
        ok: false,
        refs,
        first_snapshot_ms: first,
        failure: Some(why),
        switched,
    };
    let opened_at = Instant::now();
    let opened = call(
        session,
        spec::BROWSER_OPEN,
        &json!({ "url": task["url"] }),
        &mut switched,
    )
    .await;
    if opened["status"] != "ok" {
        return fail(format!("open: {}", opened["code"]), None, None, switched);
    }
    let tree_args = json!({ "max_nodes": spec::MAX_NODES });
    let first = call(session, spec::BROWSER_SNAPSHOT, &tree_args, &mut switched).await;
    let first_ms = ms(opened_at.elapsed());
    if first["status"] != "ok" {
        return fail(
            format!("snapshot: {}", first["code"]),
            None,
            Some(first_ms),
            switched,
        );
    }
    let refs = first["refs"].as_u64();
    let mut tree = content(&first);
    for step in task["steps"].as_array().into_iter().flatten() {
        let result = if let Some(click) = step.get("click") {
            let Some(target) = find_ref(
                &tree,
                click["role"].as_str().unwrap_or_default(),
                click["name"].as_str().unwrap_or_default(),
            ) else {
                return fail(
                    format!("no ref for {click}"),
                    refs,
                    Some(first_ms),
                    switched,
                );
            };
            call(
                session,
                spec::BROWSER_CLICK,
                &json!({ "ref": target }),
                &mut switched,
            )
            .await
        } else if let Some(typed) = step.get("type") {
            let Some(target) = find_ref(
                &tree,
                typed["role"].as_str().unwrap_or_default(),
                typed["name"].as_str().unwrap_or_default(),
            ) else {
                return fail(
                    format!("no ref for {typed}"),
                    refs,
                    Some(first_ms),
                    switched,
                );
            };
            let args = json!({ "ref": target, "text": typed["text"], "submit": typed["submit"].as_bool().unwrap_or(false) });
            call(session, spec::BROWSER_TYPE, &args, &mut switched).await
        } else if let Some(wait) = step.get("wait") {
            call(session, spec::BROWSER_WAIT, wait, &mut switched).await
        } else {
            return fail(
                format!("unknown step {step}"),
                refs,
                Some(first_ms),
                switched,
            );
        };
        if result["status"] != "ok" {
            return fail(
                format!("step {step}: {}", result["code"]),
                refs,
                Some(first_ms),
                switched,
            );
        }
        let next = call(session, spec::BROWSER_SNAPSHOT, &tree_args, &mut switched).await;
        tree = content(&next);
    }
    let expect = &task["expect"];
    if let Some(min) = expect["min_refs"].as_u64()
        && refs.unwrap_or_default() < min
    {
        return fail(
            format!("refs {} < {min}", refs.unwrap_or_default()),
            refs,
            Some(first_ms),
            switched,
        );
    }
    let text_args = json!({ "mode": "text", "max_chars": spec::MAX_CHARS });
    let text = content(&call(session, spec::BROWSER_SNAPSHOT, &text_args, &mut switched).await);
    let lower = text.to_lowercase();
    for wanted in expect["text"].as_array().into_iter().flatten() {
        let wanted = wanted.as_str().unwrap_or_default();
        if !lower.contains(&wanted.to_lowercase()) {
            return fail(
                format!("text {wanted:?} missing"),
                refs,
                Some(first_ms),
                switched,
            );
        }
    }
    if let Some(pattern) = expect["url"].as_str()
        && !glob(pattern, &page_url(&text))
    {
        return fail(
            format!("url {:?} is not {pattern}", page_url(&text)),
            refs,
            Some(first_ms),
            switched,
        );
    }
    Outcome {
        ok: true,
        refs,
        first_snapshot_ms: Some(first_ms),
        failure: None,
        switched,
    }
}

/// A forward proxy on loopback, standing in for the egress proxy: CONNECT
/// tunnels and absolute-form HTTP, no authentication and no allowlist (the
/// bake-off measures engines, not egress policy). `proxy: null` would leave
/// the engine offline, so every session gets this one.
/// The run token the bake-off's proxy asks for, as the egress proxy asks
/// for the run's.
const PROXY_TOKEN: &str = "bakeoff";

/// Whether a proxy request head carries `Proxy-Authorization: Basic
/// base64(run:<token>)`, the egress proxy's rule.
fn proxy_authorized(head: &str, token: &str) -> bool {
    use base64::Engine as _;
    let expected = base64::engine::general_purpose::STANDARD.encode(format!("run:{token}"));
    head.lines().skip(1).any(|line| {
        line.split_once(':').is_some_and(|(name, value)| {
            name.trim().eq_ignore_ascii_case("proxy-authorization") && {
                let mut words = value.split_whitespace();
                words
                    .next()
                    .is_some_and(|scheme| scheme.eq_ignore_ascii_case("basic"))
                    && words.next() == Some(expected.as_str())
                    && words.next().is_none()
            }
        })
    })
}

#[test]
fn the_proxy_asks_for_the_run_token() {
    let head =
        |auth: &str| format!("CONNECT a.test:443 HTTP/1.1\r\nHost: a.test:443\r\n{auth}\r\n");
    // base64("run:bakeoff")
    assert!(proxy_authorized(
        &head("Proxy-Authorization: Basic cnVuOmJha2VvZmY="),
        PROXY_TOKEN
    ));
    assert!(proxy_authorized(
        &head("proxy-authorization: basic cnVuOmJha2VvZmY="),
        PROXY_TOKEN
    ));
    assert!(!proxy_authorized(&head(""), PROXY_TOKEN));
    assert!(!proxy_authorized(
        &head("Proxy-Authorization: Basic cnVuOm90aGVy"),
        PROXY_TOKEN
    ));
    assert!(!proxy_authorized(
        &head("Authorization: Basic cnVuOmJha2VvZmY="),
        PROXY_TOKEN
    ));
}

/// A loopback forward proxy that, like the egress proxy, answers 407 to a
/// request without the run's credentials: every engine in the bake-off
/// answers the challenge (`Fetch.authRequired`) or loads nothing.
async fn forward_proxy() -> String {
    use tokio::io::{AsyncReadExt as _, AsyncWriteExt as _};
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind");
    let address = listener.local_addr().expect("address");
    tokio::spawn(async move {
        while let Ok((mut client, _)) = listener.accept().await {
            tokio::spawn(async move {
                let mut head = Vec::new();
                let mut buffer = [0_u8; 8192];
                while !head.windows(4).any(|window| window == b"\r\n\r\n") {
                    match client.read(&mut buffer).await {
                        Ok(0) | Err(_) => return,
                        Ok(read) => head.extend_from_slice(&buffer[..read]),
                    }
                    if head.len() > 64 << 10 {
                        return;
                    }
                }
                let text = String::from_utf8_lossy(&head).into_owned();
                if !proxy_authorized(&text, PROXY_TOKEN) {
                    let _ = client
                        .write_all(b"HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\"egress\"\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
                        .await;
                    return;
                }
                let mut words = text.split_whitespace();
                let (method, target) = (
                    words.next().unwrap_or_default(),
                    words.next().unwrap_or_default(),
                );
                let authority = if method == "CONNECT" {
                    target.to_owned()
                } else {
                    let Some(rest) = target.strip_prefix("http://") else {
                        return;
                    };
                    let host = rest.split('/').next().unwrap_or_default();
                    if host.contains(':') {
                        host.to_owned()
                    } else {
                        format!("{host}:80")
                    }
                };
                let Ok(Ok(mut upstream)) = tokio::time::timeout(
                    Duration::from_secs(15),
                    tokio::net::TcpStream::connect(&authority),
                )
                .await
                else {
                    let _ = client
                        .write_all(b"HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
                        .await;
                    return;
                };
                if method == "CONNECT" {
                    if client
                        .write_all(b"HTTP/1.1 200 Connection Established\r\n\r\n")
                        .await
                        .is_err()
                    {
                        return;
                    }
                } else if upstream.write_all(&head).await.is_err() {
                    return;
                }
                let _ = tokio::io::copy_bidirectional(&mut client, &mut upstream).await;
            });
        }
    });
    format!("http://{address}")
}

async fn measure(engines: &Engines, proxy: &str, engine: &str, task: &Value) -> Value {
    let session = Arc::new(Session::with_engines(engines.clone(), false));
    let start = Start {
        engine: engine.to_owned(),
        // The runner is the session image under gVisor: the production
        // arrangement, where the engine runs without its own sandbox.
        engine_sandbox: false,
        max_pages: 3,
        width: 1280,
        height: 800,
        // The proxy never challenges, so the token is never sent.
        proxy: Some((proxy.to_owned(), PROXY_TOKEN.to_owned())),
        attended: false,
    };
    let started_at = Instant::now();
    let started = tokio::time::timeout(START_LIMIT, session.start(&start)).await;
    let start_ms = ms(started_at.elapsed());
    let base = json!({ "engine": engine, "task": task["id"], "class": task["class"], "kind": task["kind"], "start_ms": start_ms });
    let version = match started {
        Ok(Ok(version)) => version,
        Ok(Err(error)) => {
            return json!({ "ok": false, "crashed": false, "hung": false, "failure": format!("start: {error:?}"), "base": base });
        }
        Err(_) => {
            session.stop().await;
            return json!({ "ok": false, "crashed": false, "hung": true, "failure": "start timed out", "base": base });
        }
    };
    // The memory sampler: the engine's process tree every 200 ms.
    let peak = Arc::new(AtomicU64::new(0));
    let source = Arc::new(std::sync::Mutex::new(None::<&'static str>));
    let sampler = {
        let (session, peak, source) =
            (Arc::clone(&session), Arc::clone(&peak), Arc::clone(&source));
        tokio::spawn(async move {
            loop {
                if let Some((_, Some(pid), _)) = session.engine_state()
                    && let Some((bytes, from)) = tree_memory(pid)
                {
                    peak.fetch_max(bytes, Ordering::Relaxed);
                    *source
                        .lock()
                        .unwrap_or_else(std::sync::PoisonError::into_inner) = Some(from);
                }
                tokio::time::sleep(Duration::from_millis(200)).await;
            }
        })
    };
    let outcome = tokio::time::timeout(TASK_LIMIT, run_task(&session, task)).await;
    sampler.abort();
    let state = session.engine_state();
    let engine_end = state.map(|(kind, _, _)| {
        if kind == EngineKind::Moli {
            "lite"
        } else {
            "full"
        }
    });
    let alive = state.is_some_and(|(_, _, alive)| alive);
    session.stop().await;
    let memory = source
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
        .map(|from| (peak.load(Ordering::Relaxed), from));
    let mut line = match outcome {
        Ok(outcome) => json!({
            "ok": outcome.ok && alive, "hung": false, "crashed": !alive,
            "refs": outcome.refs, "first_snapshot_ms": outcome.first_snapshot_ms,
            "failure": outcome.failure, "switched": outcome.switched,
        }),
        Err(_) => {
            json!({ "ok": false, "hung": true, "crashed": !alive, "failure": "task timed out" })
        }
    };
    let object = line.as_object_mut().expect("an object");
    object.insert("version".into(), json!(version));
    object.insert("engine_end".into(), json!(engine_end));
    object.insert(
        "peak_memory_bytes".into(),
        json!(memory.map(|(bytes, _)| bytes)),
    );
    object.insert("memory_source".into(), json!(memory.map(|(_, from)| from)));
    object.insert("base".into(), base);
    line
}

/// Flatten `base` into the line.
fn flat(mut line: Value) -> Value {
    if let Some(Value::Object(base)) = line
        .as_object_mut()
        .and_then(|object| object.remove("base"))
    {
        let object = line.as_object_mut().expect("an object");
        for (key, value) in base {
            object.insert(key, value);
        }
    }
    line
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn the_bake_off() {
    let Some(out) = env_path("ELITEA_BAKEOFF_OUT") else {
        eprintln!("skipped: set ELITEA_BAKEOFF_OUT to run the bake-off corpus");
        return;
    };
    let corpus_path = env_path("ELITEA_BAKEOFF_CORPUS").unwrap_or_else(|| {
        PathBuf::from(concat!(env!("CARGO_MANIFEST_DIR"), "/bakeoff/corpus.json"))
    });
    let corpus: Value =
        serde_json::from_str(&std::fs::read_to_string(&corpus_path).expect("the corpus"))
            .expect("JSON");
    // The workflow always passes the variable; empty means the whole corpus.
    let only = only_tasks(std::env::var("ELITEA_BAKEOFF_ONLY").ok().as_deref());
    let engines = Engines {
        chrome: env_path("ELITEA_BROWSER_ENGINE")
            .expect("ELITEA_BROWSER_ENGINE (chrome-headless-shell)"),
        moli: env_path("ELITEA_BROWSER_MOLI"),
    };
    if let Some(only) = &only {
        let known: HashSet<&str> = corpus["tasks"]
            .as_array()
            .expect("tasks")
            .iter()
            .filter_map(|task| task["id"].as_str())
            .collect();
        let mut unknown: Vec<&str> = only
            .iter()
            .map(String::as_str)
            .filter(|id| !known.contains(id))
            .collect();
        unknown.sort_unstable();
        assert!(unknown.is_empty(), "not corpus task ids: {unknown:?}");
    }
    let names =
        std::env::var("ELITEA_BAKEOFF_ENGINES").unwrap_or_else(|_| "full,lite,auto".to_owned());
    let proxy = forward_proxy().await;
    let mut lines = String::new();
    let mut ran = 0_usize;
    for engine in names.split(',') {
        for task in corpus["tasks"].as_array().expect("tasks") {
            let id = task["id"].as_str().unwrap_or_default();
            if only.as_ref().is_some_and(|only| !only.contains(id)) {
                continue;
            }
            ran += 1;
            let line = flat(measure(&engines, &proxy, engine, task).await);
            eprintln!("{line}");
            lines.push_str(&line.to_string());
            lines.push('\n');
            // Written as it goes: a runner killed half-way keeps its lines.
            std::fs::write(&out, &lines).expect("the output");
        }
    }
    assert!(ran > 0, "the selection matched no corpus task: {only:?}");
}

/// `ELITEA_BAKEOFF_ONLY` as a task filter: unset, empty or only commas
/// means every task, never none.
fn only_tasks(value: Option<&str>) -> Option<HashSet<String>> {
    let ids: HashSet<String> = value?
        .split(',')
        .map(str::trim)
        .filter(|id| !id.is_empty())
        .map(str::to_owned)
        .collect();
    (!ids.is_empty()).then_some(ids)
}

#[test]
fn an_empty_selection_runs_the_whole_corpus() {
    assert_eq!(only_tasks(None), None);
    assert_eq!(only_tasks(Some("")), None);
    assert_eq!(only_tasks(Some(" , ")), None);
    let picked = only_tasks(Some("read-01, act-02")).expect("two ids");
    assert!(picked.contains("read-01") && picked.contains("act-02") && picked.len() == 2);
}

#[test]
fn the_corpus_is_well_formed() {
    let corpus: Value = serde_json::from_str(include_str!("../bakeoff/corpus.json")).expect("JSON");
    let tasks = corpus["tasks"].as_array().expect("tasks");
    assert!(
        tasks.len() >= 40,
        "ADR-0049 asks for about 40 tasks: {}",
        tasks.len()
    );
    let mut ids = HashSet::new();
    let mut classes = HashSet::new();
    for task in tasks {
        let id = task["id"].as_str().expect("an id");
        assert!(ids.insert(id), "{id} twice");
        classes.insert(task["class"].as_str().expect("a class"));
        assert!(
            matches!(task["kind"].as_str(), Some("read" | "extract" | "act")),
            "{id}: kind"
        );
        assert!(
            task["url"]
                .as_str()
                .is_some_and(|url| url.starts_with("https://")),
            "{id}: url"
        );
        let expect = task["expect"].as_object().expect("expect");
        assert!(!expect.is_empty(), "{id}: nothing is checked");
        assert_eq!(
            task["kind"] == "act",
            task.get("steps").is_some(),
            "{id}: act tasks and only they have steps"
        );
    }
    for class in [
        "atlassian",
        "office",
        "salesforce",
        "servicenow",
        "github",
        "docs",
        "gov",
        "spa",
    ] {
        assert!(classes.contains(class), "no {class} task");
    }
}

#[test]
fn the_scripted_agent_reads_trees_urls_and_globs() {
    let tree = "# snapshot s1\n- link \"Issues 5k\" [ref=e3]\n- searchbox \"Search Wikipedia\" [ref=e7]\n- button \"Go\" [ref=e8]";
    assert_eq!(find_ref(tree, "link", "issues").as_deref(), Some("e3"));
    assert_eq!(
        find_ref(tree, "combobox|searchbox", "Search").as_deref(),
        Some("e7")
    );
    assert_eq!(find_ref(tree, "button", "Stop"), None);
    assert_eq!(
        page_url("# url \"https://x.test/a/issues?q=1\"\n# title \"t\""),
        "https://x.test/a/issues?q=1"
    );
    assert!(glob("*/issues*", "https://x.test/a/issues?q=1"));
    assert!(!glob("*/pulls*", "https://x.test/a/issues"));
    assert!(glob(
        "*holiday-entitlement/*",
        "https://www.gov.uk/calculate-your-holiday-entitlement/y"
    ));
}
