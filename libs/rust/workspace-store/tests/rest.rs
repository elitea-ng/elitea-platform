//! The REST transport against a local mock of the contract's routes, in the
//! shapes the real Main answers (checked against elitea-main on
//! 2026-10-16): the paths, the bearer only on the deployment's origin, the
//! flat error envelope, a 401 refreshed once, the grant's `headers` bound
//! like an S3 presigned PUT (every one must be sent; the checksum must
//! match the bytes), Main's origin-relative facade and multipart
//! completion URLs, and the whole flow end to end.
#![cfg(feature = "rest")]
#![allow(
    clippy::unwrap_used,
    clippy::expect_used,
    clippy::needless_pass_by_value
)]

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
    /// Lowercased names.
    headers: BTreeMap<String, String>,
}

/// How the mock's store takes uploads.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
enum Upload {
    /// A presigned PUT on another host whose grant binds
    /// `x-amz-checksum-sha256` (S3, `RustFS`).
    #[default]
    Presigned,
    /// Main's streaming facade: an origin-relative PUT/GET with the bearer.
    Facade,
    /// Parts on another host, completed through an origin-relative POST.
    Multipart,
}

#[derive(Default)]
struct Mock {
    seen: Vec<Seen>,
    objects: BTreeMap<String, Vec<u8>>,
    /// The grant headers a PUT must carry, by object path.
    bound: BTreeMap<String, BTreeMap<String, String>>,
    parts: BTreeMap<u32, Vec<u8>>,
    refs: BTreeMap<String, String>,
    seq: u64,
    unauthorized_once: bool,
    upload: Upload,
    base: String,
    other: String,
}

const THREAD: &str = "prompt_lib/7/c0ffee00-0000-4000-8000-000000000001";

fn base64(bytes: &[u8]) -> String {
    const ALPHABET: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    let mut out = String::new();
    for chunk in bytes.chunks(3) {
        let n = chunk
            .iter()
            .enumerate()
            .fold(0u32, |n, (i, b)| n | (u32::from(*b) << (16 - 8 * i)));
        for i in 0..4 {
            if i <= chunk.len() {
                out.push(ALPHABET[((n >> (18 - 6 * i)) & 63) as usize] as char);
            } else {
                out.push('=');
            }
        }
    }
    out
}

fn checksum(bytes: &[u8]) -> String {
    use sha2::Digest as _;
    base64(&sha2::Sha256::digest(bytes))
}

fn s3_error(status: u16, code: &str) -> Vec<u8> {
    let text = format!("<?xml version=\"1.0\"?><Error><Code>{code}</Code></Error>");
    format!("HTTP/1.1 {status} X\r\nContent-Type: application/xml\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{text}", text.len()).into_bytes()
}

fn no_content() -> Vec<u8> {
    b"HTTP/1.1 204 No Content\r\nCache-Control: no-store\r\nConnection: close\r\n\r\n".to_vec()
}

fn bytes_response(bytes: &[u8], etag: Option<&str>) -> Vec<u8> {
    let etag = etag.map(|e| format!("ETag: {e}\r\n")).unwrap_or_default();
    let mut out = format!(
        "HTTP/1.1 200 OK\r\n{etag}Content-Length: {}\r\nConnection: close\r\n\r\n",
        bytes.len()
    )
    .into_bytes();
    out.extend_from_slice(bytes);
    out
}

type Shared = Arc<Mutex<Mock>>;

fn respond(status: u16, body: &Value) -> Vec<u8> {
    let text = body.to_string();
    format!("HTTP/1.1 {status} X\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{text}", text.len()).into_bytes()
}

#[allow(clippy::too_many_lines)]
fn handle(
    mock: &Shared,
    method: &str,
    target: &str,
    headers: BTreeMap<String, String>,
    body: &[u8],
) -> Vec<u8> {
    let mut m = mock.lock().unwrap();
    let auth = headers.get("authorization").cloned();
    m.seen.push(Seen {
        method: method.to_owned(),
        path: target.to_owned(),
        authorization: auth.clone(),
        headers: headers.clone(),
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
            let expires = "2026-10-16T04:39:59.049482Z";
            if m.objects.contains_key(&sha) {
                return respond(
                    200,
                    &json!({"exists": true, "put": null, "multipart": null, "expires_at": expires}),
                );
            }
            match m.upload {
                // The object store is "another host" (the second listener).
                // Main binds the digest: the store refuses a PUT without
                // every header, or with other bytes.
                Upload::Presigned => {
                    let path = format!("/obj/{sha}");
                    let mut bound = BTreeMap::new();
                    bound.insert(
                        "content-type".to_owned(),
                        "application/x-git-bundle".to_owned(),
                    );
                    let hex = sha
                        .as_bytes()
                        .chunks(2)
                        .map(|pair| {
                            u8::from_str_radix(std::str::from_utf8(pair).unwrap(), 16).unwrap()
                        })
                        .collect::<Vec<u8>>();
                    bound.insert("x-amz-checksum-sha256".to_owned(), base64(&hex));
                    m.bound.insert(path.clone(), bound.clone());
                    respond(
                        200,
                        &json!({"exists": false, "put": {"url": format!("{}{path}?X-Amz-Signature=abc", m.other),
                                "headers": {"Content-Type": "application/x-git-bundle", "x-amz-checksum-sha256": bound["x-amz-checksum-sha256"]}},
                                "multipart": null, "expires_at": expires}),
                    )
                }
                // Main answers its facade as an origin-relative path.
                Upload::Facade => respond(
                    200,
                    &json!({"exists": false, "put": {"url": format!("/api/v2/elitea_core/workspace_store_object/{THREAD}/{sha}"),
                            "headers": {"Content-Type": "application/x-git-bundle"}}, "multipart": null, "expires_at": expires}),
                ),
                Upload::Multipart => {
                    let size = body_json["bytes"].as_u64().unwrap();
                    let part_size = 8u64;
                    let urls: Vec<String> = (1..=size.div_ceil(part_size))
                        .map(|n| format!("{}/part/{n}", m.other))
                        .collect();
                    respond(
                        200,
                        &json!({"exists": false, "put": null, "expires_at": expires,
                                "multipart": {"upload_id": "up-1", "part_size": part_size, "part_urls": urls,
                                              "complete_url": format!("/api/v2/elitea_core/workspace_store_upload_complete/prompt_lib/7/grant-1?sha={sha}")}}),
                    )
                }
            }
        }
        ("PUT", path) if path.starts_with("/api/v2/elitea_core/workspace_store_object/") => {
            let sha = path.rsplit('/').next().unwrap().to_owned();
            m.objects.insert(sha, body.to_vec());
            no_content()
        }
        ("GET", path) if path.starts_with("/api/v2/elitea_core/workspace_store_object/") => {
            let sha = path.rsplit('/').next().unwrap();
            let bytes = m.objects.get(sha).cloned().unwrap_or_default();
            bytes_response(&bytes, None)
        }
        ("POST", "/api/v2/elitea_core/workspace_store_upload_complete/prompt_lib/7/grant-1") => {
            assert_eq!(body_json["upload_id"], "up-1");
            let mut all = Vec::new();
            for (index, part) in body_json["parts"].as_array().unwrap().iter().enumerate() {
                let number = part["part_number"].as_u64().unwrap();
                assert_eq!(number, index as u64 + 1);
                assert_eq!(part["etag"], format!("\"etag-{number}\""));
                all.extend_from_slice(&m.parts[&u32::try_from(number).unwrap()]);
            }
            let sha = target.split("sha=").nth(1).unwrap().to_owned();
            assert_eq!(sha256_hex(&all), sha);
            m.objects.insert(sha, all);
            no_content()
        }
        ("PUT", path) if path.starts_with("/part/") => {
            let number: u32 = path.trim_start_matches("/part/").parse().unwrap();
            m.parts.insert(number, body.to_vec());
            bytes_response(b"", Some(&format!("\"etag-{number}\"")))
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
            let url = if m.upload == Upload::Facade {
                format!("/api/v2/elitea_core/workspace_store_object/{THREAD}/{sha}")
            } else {
                format!("{}/obj/{sha}?X-Amz-Signature=abc", m.other)
            };
            respond(
                200,
                &json!({"head_seq": m.seq, "refs": m.refs, "segments": [{"seq": m.seq, "kind": "COMPACT", "sha256": sha, "bytes": bytes,
                        "prerequisites": [], "refs": m.refs, "url": url, "expires_at": "2026-10-16T04:39:59.049482Z"}]}),
            )
        }
        ("PUT", path) if path.starts_with("/obj/") => {
            // As S3 does for a presigned PUT: every signed header must be
            // sent with its value, and the checksum must be the bytes'.
            let bound = m.bound.get(path).cloned().unwrap_or_default();
            for (name, value) in &bound {
                if headers.get(name) != Some(value) {
                    return s3_error(403, "SignatureDoesNotMatch");
                }
            }
            if let Some(declared) = headers.get("x-amz-checksum-sha256")
                && *declared != checksum(body)
            {
                return s3_error(400, "BadDigest");
            }
            m.objects
                .insert(path.trim_start_matches("/obj/").to_owned(), body.to_vec());
            bytes_response(b"", Some("\"md5\""))
        }
        ("GET", path) if path.starts_with("/obj/") => {
            let bytes = m
                .objects
                .get(path.trim_start_matches("/obj/"))
                .cloned()
                .unwrap_or_default();
            bytes_response(&bytes, None)
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
            let headers: BTreeMap<String, String> = lines
                .filter_map(|line| line.split_once(':'))
                .map(|(name, value)| (name.trim().to_ascii_lowercase(), value.trim().to_owned()))
                .collect();
            let response = handle(&mock, &method, &target, headers, &buffer[head_end..]);
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
    start_with(Upload::Presigned).await
}

async fn start_with(upload: Upload) -> (Shared, RestStore) {
    let api = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let objects = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let base = format!("http://{}", api.local_addr().unwrap());
    // Another origin for the object store: `localhost`, not `127.0.0.1`.
    let other = format!("http://localhost:{}", objects.local_addr().unwrap().port());
    let mock: Shared = Arc::new(Mutex::new(Mock {
        base: base.clone(),
        other,
        upload,
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
    // The PUT carried every header of the grant (the checksum binding).
    let put = object_calls.iter().find(|s| s.method == "PUT").unwrap();
    assert_eq!(put.headers["x-amz-checksum-sha256"], checksum(&bytes));
    assert_eq!(put.headers["content-type"], "application/x-git-bundle");
    let _ = &m.base;
}

fn request_for(bytes: &[u8]) -> PublishRequest {
    PublishRequest {
        epoch: 1,
        kind: "COMPACT".into(),
        segment_sha256: sha256_hex(bytes),
        bytes: bytes.len() as u64,
        prerequisites: vec![],
        refs: vec![RefUpdate {
            name: "refs/elitea/base".into(),
            old: None,
            new: "1111111111111111111111111111111111111111".into(),
        }],
        object_format: "sha1".into(),
    }
}

/// Main answers its streaming facade (a backend without presign) as an
/// origin-relative path: the client resolves it against the deployment and
/// sends the bearer there, for the PUT (204, no body) and the GET.
#[tokio::test]
async fn the_streaming_facade_is_an_origin_relative_url_with_the_bearer() {
    let (mock, store) = start_with(Upload::Facade).await;
    let bytes = b"# v2 git bundle\nfacade".to_vec();
    sync::upload_and_publish(&store, &bytes, &request_for(&bytes), Backoff::immediate(3))
        .await
        .unwrap();
    let (_plan, _segment, got) = sync::fetch_latest(&store, 0, Backoff::immediate(3))
        .await
        .unwrap();
    assert_eq!(got, bytes);
    let m = mock.lock().unwrap();
    let facade: Vec<&Seen> = m
        .seen
        .iter()
        .filter(|s| s.path.contains("/workspace_store_object/"))
        .collect();
    assert_eq!(facade.len(), 2, "{facade:?}");
    assert!(
        facade
            .iter()
            .all(|s| s.authorization.as_deref() == Some("Bearer good"))
    );
}

/// Parts go to the store without the bearer; the completion is Main's
/// origin-relative route (204, no body), with the bearer and the part `ETag`s.
#[tokio::test]
async fn a_multipart_upload_completes_through_mains_relative_route() {
    let (mock, store) = start_with(Upload::Multipart).await;
    let bytes = b"# v2 git bundle\nin three parts".to_vec();
    sync::upload_and_publish(&store, &bytes, &request_for(&bytes), Backoff::immediate(3))
        .await
        .unwrap();
    let m = mock.lock().unwrap();
    let parts: Vec<&Seen> = m
        .seen
        .iter()
        .filter(|s| s.path.starts_with("/part/"))
        .collect();
    assert_eq!(parts.len(), bytes.len().div_ceil(8));
    assert!(parts.iter().all(|s| s.authorization.is_none()));
    let complete = m
        .seen
        .iter()
        .find(|s| s.path.contains("/workspace_store_upload_complete/"))
        .unwrap();
    assert_eq!(complete.authorization.as_deref(), Some("Bearer good"));
    assert!(m.objects.contains_key(&sha256_hex(&bytes)));
}

#[tokio::test]
async fn a_url_with_a_foreign_scheme_is_refused() {
    let (_mock, store) = start().await;
    assert_eq!(
        store.get("file:///etc/passwd").await.unwrap_err().code(),
        "invalid"
    );
}
