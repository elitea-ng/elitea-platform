//! The REST transport against a local mock of the contract's routes: the
//! paths, the bearer only on the deployment's origin, the error envelope,
//! a 401 refreshed once, and the whole flow end to end.
#![cfg(feature = "rest")]
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::needless_pass_by_value)]

use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

use async_trait::async_trait;
use elitea_workspace_store::StoreError;
use elitea_workspace_store::digest::sha256_hex;
use elitea_workspace_store::rest::{RestStore, TokenSource};
use elitea_workspace_store::sync::{self, Backoff};
use elitea_workspace_store::transport::{PublishRequest, RefUpdate, StoreTransport as _};
use serde_json::{Value, json};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpListener;

#[derive(Clone, Debug)]
struct Seen {
    method: String,
    path: String,
    authorization: Option<String>,
}

#[derive(Default)]
struct Mock {
    seen: Vec<Seen>,
    objects: BTreeMap<String, Vec<u8>>,
    refs: BTreeMap<String, String>,
    seq: u64,
    unauthorized_once: bool,
    base: String,
    other: String,
}

type Shared = Arc<Mutex<Mock>>;

fn respond(status: u16, body: &Value) -> Vec<u8> {
    let text = body.to_string();
    format!("HTTP/1.1 {status} X\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{text}", text.len()).into_bytes()
}

fn handle(mock: &Shared, method: &str, target: &str, auth: Option<String>, body: &[u8]) -> Vec<u8> {
    let mut m = mock.lock().unwrap();
    m.seen.push(Seen {
        method: method.to_owned(),
        path: target.to_owned(),
        authorization: auth.clone(),
    });
    let api = target.starts_with("/api/");
    if api && auth.as_deref() != Some("Bearer good") {
        if m.unauthorized_once || auth.as_deref() == Some("Bearer stale") {
            m.unauthorized_once = false;
            return respond(401, &json!({"error": "unauthorized", "message": "no"}));
        }
        return respond(403, &json!({"error": "forbidden", "message": "no"}));
    }
    let body_json: Value = serde_json::from_slice(body).unwrap_or(Value::Null);
    match (method, target.split('?').next().unwrap()) {
        (
            "POST",
            "/api/v2/elitea_core/workspace_store_upload/prompt_lib/7/c0ffee00-0000-4000-8000-000000000001",
        ) => {
            let sha = body_json["sha256"].as_str().unwrap().to_owned();
            if m.objects.contains_key(&sha) {
                return respond(200, &json!({"exists": true}));
            }
            // The object store is "another host" (the second listener).
            respond(
                200,
                &json!({"exists": false, "put": {"url": format!("{}/obj/{sha}", m.other), "headers": {"Content-Type": "application/x-git-bundle"}}, "multipart": null}),
            )
        }
        (
            "POST",
            "/api/v2/elitea_core/workspace_store_publish/prompt_lib/7/c0ffee00-0000-4000-8000-000000000001",
        ) => {
            let request: PublishRequest = serde_json::from_value(body_json).unwrap();
            if !m.objects.contains_key(&request.segment_sha256) {
                return respond(
                    422,
                    &json!({"error": "segment_missing", "message": "missing"}),
                );
            }
            for update in &request.refs {
                if m.refs.get(&update.name) != update.old.as_ref() {
                    return respond(
                        409,
                        &json!({"error": "ref_conflict", "message": "moved", "details": {"current": m.refs}}),
                    );
                }
            }
            for update in &request.refs {
                m.refs.insert(update.name.clone(), update.new.clone());
            }
            m.seq += 1;
            respond(
                200,
                &json!({"seq": m.seq, "refs": m.refs, "superseded": []}),
            )
        }
        (
            "GET",
            "/api/v2/elitea_core/workspace_store_fetch/prompt_lib/7/c0ffee00-0000-4000-8000-000000000001",
        ) => {
            assert!(target.ends_with("?since_seq=0"), "{target}");
            let (sha, bytes) = m
                .objects
                .iter()
                .next()
                .map(|(s, b)| (s.clone(), b.len()))
                .unwrap();
            respond(
                200,
                &json!({"head_seq": m.seq, "refs": m.refs, "segments": [{"seq": m.seq, "kind": "COMPACT", "sha256": sha, "bytes": bytes, "prerequisites": [], "refs": m.refs, "url": format!("{}/obj/{sha}", m.other)}]}),
            )
        }
        ("PUT", path) if path.starts_with("/obj/") => {
            m.objects
                .insert(path.trim_start_matches("/obj/").to_owned(), body.to_vec());
            respond(200, &json!({}))
        }
        ("GET", path) if path.starts_with("/obj/") => {
            let bytes = m
                .objects
                .get(path.trim_start_matches("/obj/"))
                .cloned()
                .unwrap_or_default();
            let mut out = format!(
                "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                bytes.len()
            )
            .into_bytes();
            out.extend_from_slice(&bytes);
            out
        }
        _ => respond(404, &json!({"error": "not_found", "message": "no route"})),
    }
}

async fn serve(listener: TcpListener, mock: Shared) {
    loop {
        let Ok((mut socket, _)) = listener.accept().await else {
            return;
        };
        let mock = mock.clone();
        tokio::spawn(async move {
            let mut buffer = Vec::new();
            let mut chunk = [0u8; 8192];
            let (head_end, length) = loop {
                let read = socket.read(&mut chunk).await.unwrap();
                if read == 0 {
                    return;
                }
                buffer.extend_from_slice(&chunk[..read]);
                if let Some(end) = buffer.windows(4).position(|w| w == b"\r\n\r\n") {
                    let head = String::from_utf8_lossy(&buffer[..end]).to_ascii_lowercase();
                    let length = head
                        .lines()
                        .find_map(|line| line.strip_prefix("content-length:"))
                        .map_or(0, |v| v.trim().parse::<usize>().unwrap());
                    break (end + 4, length);
                }
            };
            while buffer.len() < head_end + length {
                let read = socket.read(&mut chunk).await.unwrap();
                if read == 0 {
                    break;
                }
                buffer.extend_from_slice(&chunk[..read]);
            }
            let head = String::from_utf8_lossy(&buffer[..head_end]).into_owned();
            let mut lines = head.lines();
            let mut first = lines.next().unwrap().split(' ');
            let method = first.next().unwrap().to_owned();
            let target = first.next().unwrap().to_owned();
            let auth = lines
                .find_map(|line| {
                    line.strip_prefix("authorization: ")
                        .or_else(|| line.strip_prefix("Authorization: "))
                })
                .map(str::to_owned);
            let response = handle(&mock, &method, &target, auth, &buffer[head_end..]);
            let _ = socket.write_all(&response).await;
            let _ = socket.shutdown().await;
        });
    }
}

struct Tokens(Mutex<&'static str>);

#[async_trait]
impl TokenSource for Tokens {
    async fn bearer(&self) -> Result<String, StoreError> {
        Ok((*self.0.lock().unwrap()).to_owned())
    }
    async fn refreshed(&self) -> Result<String, StoreError> {
        *self.0.lock().unwrap() = "good";
        Ok("good".to_owned())
    }
}

async fn start() -> (Shared, RestStore) {
    let api = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let objects = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let base = format!("http://{}", api.local_addr().unwrap());
    // Another origin for the object store: `localhost`, not `127.0.0.1`.
    let other = format!("http://localhost:{}", objects.local_addr().unwrap().port());
    let mock: Shared = Arc::new(Mutex::new(Mock {
        base: base.clone(),
        other,
        ..Mock::default()
    }));
    tokio::spawn(serve(api, mock.clone()));
    tokio::spawn(serve(objects, mock.clone()));
    let store = RestStore::new(
        reqwest::Client::builder()
            .redirect(reqwest::redirect::Policy::none())
            .build()
            .unwrap(),
        &base,
        7,
        "c0ffee00-0000-4000-8000-000000000001",
        Arc::new(Tokens(Mutex::new("stale"))),
    )
    .with_header("X-Elitea-Client-Version", "test");
    (mock, store)
}

#[tokio::test]
async fn send_and_fetch_through_the_rest_routes() {
    let (mock, store) = start().await;
    let bytes = b"# v2 git bundle\n...".to_vec();
    let request = PublishRequest {
        epoch: 1,
        kind: "COMPACT".into(),
        segment_sha256: sha256_hex(&bytes),
        bytes: bytes.len() as u64,
        prerequisites: vec![],
        refs: vec![RefUpdate {
            name: "refs/elitea/base".into(),
            old: None,
            new: "1111111111111111111111111111111111111111".into(),
        }],
        object_format: "sha1".into(),
    };
    let published = sync::upload_and_publish(&store, &bytes, &request, Backoff::immediate(3))
        .await
        .unwrap();
    assert_eq!(published.seq, 1);
    let (_plan, segment, got) = sync::fetch_latest(&store, 0, Backoff::immediate(3))
        .await
        .unwrap();
    assert_eq!(got, bytes);
    assert_eq!(
        segment.refs["refs/elitea/base"],
        "1111111111111111111111111111111111111111"
    );
    // Someone else's conflict comes back with the server's code.
    let mut theirs = request.clone();
    theirs.refs[0].new = "2222222222222222222222222222222222222222".into();
    let error = sync::publish(&store, &theirs, Backoff::immediate(2))
        .await
        .unwrap_err();
    assert_eq!(error.code(), "ref_conflict");

    let m = mock.lock().unwrap();
    // A 401 was refreshed once, then every API call carried the good token.
    assert_eq!(m.seen[0].authorization.as_deref(), Some("Bearer stale"));
    assert!(
        m.seen[1..]
            .iter()
            .filter(|s| s.path.starts_with("/api/"))
            .all(|s| s.authorization.as_deref() == Some("Bearer good"))
    );
    // The object store never saw the bearer.
    let object_calls: Vec<&Seen> = m
        .seen
        .iter()
        .filter(|s| s.path.starts_with("/obj/"))
        .collect();
    assert_eq!(object_calls.len(), 2);
    assert!(
        object_calls.iter().all(|s| s.authorization.is_none()),
        "{object_calls:?}"
    );
    assert!(object_calls.iter().any(|s| s.method == "PUT"));
    let _ = &m.base;
}

#[tokio::test]
async fn a_url_with_a_foreign_scheme_is_refused() {
    let (_mock, store) = start().await;
    assert_eq!(
        store.get("file:///etc/passwd").await.unwrap_err().code(),
        "invalid"
    );
}
