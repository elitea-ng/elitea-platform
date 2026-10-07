//! The port against the Python engine's own answers
//! (`tests/fixtures/ingest/artifact_source.json`, written by
//! `parity/python_artifact_source.py`), the path and limit checks, and the
//! transfer against a mock object API.

use super::*;
use crate::errors::classify;
use crate::source::wiki_id_for;
use crate::wiki::compose::{build_repo_identifier, normalize_wiki_id};
use axum::extract::{Request, State};
use axum::http::{HeaderMap, StatusCode as HttpStatus};
use axum::response::{IntoResponse, Response as HttpResponse};
use serde_json::json;
use std::collections::BTreeMap;
use std::sync::atomic::AtomicUsize;
use std::sync::{Arc, Mutex};

const FIXTURE: &str = include_str!("../../../tests/fixtures/ingest/artifact_source.json");

fn fixture() -> Value {
    serde_json::from_str(FIXTURE).unwrap_or_else(|e| panic!("fixture: {e}"))
}

fn text(value: &Value) -> &str {
    value.as_str().unwrap_or_default()
}

fn source(repository: &str) -> ArtifactSource {
    parse_artifact_source(repository).unwrap_or_else(|e| panic!("{repository}: {e}"))
}

/// Every object of every page, in order.
fn page_items(case: &Value) -> Vec<Value> {
    case["pages"]
        .as_array()
        .into_iter()
        .flatten()
        .filter_map(|page| page.get("objects").and_then(Value::as_array))
        .flatten()
        .cloned()
        .collect()
}

#[test]
fn parsing_matches_python() {
    for case in fixture()["parse"].as_array().into_iter().flatten() {
        let repository = text(&case["repository"]);
        match parse_artifact_source(repository) {
            Ok(parsed) => {
                assert!(case.get("error").is_none(), "{repository}: accepted");
                assert_eq!(parsed.bucket, text(&case["bucket"]), "{repository}");
                assert_eq!(parsed.prefix, text(&case["prefix"]), "{repository}");
                assert_eq!(parsed.url(), text(&case["url"]), "{repository}");
                assert_eq!(parsed.list_prefix(), text(&case["list_prefix"]));
                assert_eq!(parsed.slug(), text(&case["slug"]), "{repository}");
            }
            Err(error) => {
                assert_eq!(
                    error.message,
                    text(&case["error"]["message"]),
                    "{repository}"
                );
                assert_eq!(error.error_type, ErrorType::Value, "{repository}");
            }
        }
    }
}

#[test]
fn the_listing_identity_and_names_match_python() {
    let caps = ArtifactCaps::default();
    let document = fixture();
    assert_eq!(
        document["caps"],
        json!({
            "max_files_env": "ELITEA_DEEPWIKI_ARTIFACT_MAX_FILES",
            "max_bytes_env": "ELITEA_DEEPWIKI_ARTIFACT_MAX_BYTES",
            "default_max_files": caps.max_files,
            "default_max_bytes": caps.max_bytes,
        })
    );
    let listings = document["listings"].as_array().cloned().unwrap_or_default();
    assert_eq!(listings.len(), 8);
    for case in &listings {
        let name = text(&case["name"]);
        let repository = text(&case["repository"]);
        let branch = text(&case["branch"]);
        let folder = source(repository);
        let collected = collect_objects(&folder, &page_items(case))
            .and_then(|objects| check_caps(&objects, &folder, caps).map(|()| objects));
        let objects = match collected {
            Err(error) => {
                assert_eq!(error.message, text(&case["error"]["message"]), "{name}");
                assert_eq!(error.error_type, ErrorType::Value, "{name}");
                continue;
            }
            Ok(objects) => objects,
        };
        assert!(case.get("error").is_none(), "{name}: accepted");
        let expected: Vec<Value> = objects
            .iter()
            .map(|o| {
                json!({"key": o.key, "size": i64::try_from(o.size).unwrap_or(-1), "modified": o.modified, "relative_path": o.relative_path})
            })
            .collect();
        assert_eq!(Value::Array(expected), case["objects"], "{name}");
        let digest = listing_digest(&objects);
        assert_eq!(digest, text(&case["digest"]), "{name}");
        assert_eq!(digest, text(&case["marker"]["commit_hash"]), "{name}");
        assert_eq!(
            directory_name(&folder, branch, &digest),
            text(&case["directory"]),
            "{name}"
        );

        // The ONE id: the generation's, which ask and the context paths
        // (and the fixture runner) derive too.
        let config = json!({
            "provider_type": "artifact",
            "provider_config": {"bucket": folder.bucket, "prefix": folder.prefix},
            "repository": repository,
            "branch": branch,
            "project": null,
            "is_cloud": null,
        });
        let target = ArtifactTarget::from_repo_config(&config)
            .ok()
            .flatten()
            .unwrap_or_else(|| panic!("{name}: not a folder"));
        assert_eq!(target.repository(), text(&case["canonical_repository"]));
        assert_eq!(target.branch(), text(&case["marker"]["branch"]));
        let identity = RepoIdentity::new(target.repository(), target.branch(), digest.as_str());
        let identifier =
            build_repo_identifier(identity.repo(), identity.branch(), Some(identity.commit()));
        assert_eq!(identifier, text(&case["repo_identifier"]), "{name}");
        assert_eq!(identity.to_string(), identifier, "{name}");
        assert_eq!(
            normalize_wiki_id(&identifier),
            text(&case["wiki_id"]),
            "{name}"
        );
        let ask = crate::ask::parse_request(
            json!({"question": "q", "repo_config": config})
                .as_object()
                .unwrap_or_else(|| unreachable!()),
        )
        .map(|request| request.wiki_id);
        assert_eq!(ask.ok().as_deref(), Some(text(&case["wiki_id"])), "{name}");
        assert_eq!(
            wiki_id_for(Some(&config), Some(branch)).ok().as_deref(),
            Some(text(&case["wiki_id"])),
            "{name}"
        );
    }
}

#[test]
fn the_platform_settings_match_python() {
    for case in fixture()["settings"].as_array().into_iter().flatten() {
        let expected_base = text(&case["base_url"]);
        let project = text(&case["project_id"]);
        let parsed = PlatformObjects::from_llm_settings(&case["llm_settings"]);
        if project.is_empty() {
            // Deliberate: Python sent an empty project and read a 404.
            assert!(
                parsed.is_err_and(|e| e.message.contains("llm_settings.organization")),
                "{case}"
            );
            continue;
        }
        let Ok(parsed) = parsed else {
            panic!("{case}: refused");
        };
        assert_eq!(parsed.base().as_str().trim_end_matches('/'), expected_base);
        assert_eq!(parsed.project_id(), project);
        let url = parsed.url("docs", Some("a b/ü?#.md"));
        assert_eq!(
            url.as_str(),
            format!(
                "{expected_base}/api/v2/artifacts/objects/{project}/docs/a%20b/%C3%BC%3F%23.md"
            )
        );
    }
}

#[test]
fn the_platform_base_is_checked() {
    for bad in [
        json!({"api_base": "ftp://p/llm/v1", "api_key": "k", "organization": "1"}),
        json!({"api_base": "https://u:p@p/llm/v1", "api_key": "k", "organization": "1"}),
        json!({"api_base": "https://p/x?y=1", "api_key": "k", "organization": "1"}),
        json!({"api_base": "not a url", "api_key": "k", "organization": "1"}),
        json!({"api_base": "https://p/llm/v1", "organization": "1"}),
        json!({"api_key": "k", "organization": "1"}),
    ] {
        let error = PlatformObjects::from_llm_settings(&bad).err();
        assert_eq!(error.map(|e| e.error_type), Some(ErrorType::Value), "{bad}");
    }
    let parsed = PlatformObjects::from_llm_settings(
        &json!({"api_base": "https://p/llm/v1", "api_key": "bearer-secret-9", "organization": 5}),
    );
    let shown = format!("{parsed:?}");
    assert!(!shown.contains("bearer-secret-9"), "{shown}");
}

#[test]
fn python_int_coerces_as_python_does() {
    for (value, expected) in [
        (json!(12), 12),
        (json!(-3), -3),
        (json!(7.9), 7),
        (json!(-7.9), -7),
        (json!(true), 1),
        (json!(" 40 "), 40),
        (json!("1_000"), 1000),
        (json!("+5"), 5),
        (json!("-5"), -5),
        (json!("12.5"), 0),
        (json!("1__0"), 0),
        (json!("_1"), 0),
        (json!(""), 0),
        (Value::Null, 0),
        (json!([1]), 0),
        (json!(u64::MAX), i128::from(u64::MAX)),
    ] {
        assert_eq!(python_int(Some(&value)), expected, "{value}");
    }
    assert_eq!(python_int(None), 0);
}

/// A fresh directory under the system temp directory.
fn temp_dir(name: &str) -> PathBuf {
    static NEXT: AtomicUsize = AtomicUsize::new(0);
    let dir = std::env::temp_dir().join(format!(
        "dw-artifact-{name}-{}-{}",
        std::process::id(),
        NEXT.fetch_add(1, Ordering::SeqCst)
    ));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap_or_else(|e| panic!("{e}"));
    dir
}

#[test]
fn a_path_never_leaves_the_root() {
    let root = temp_dir("paths");
    let canonical = std::fs::canonicalize(&root).unwrap_or_else(|e| panic!("{e}"));
    for bad in [
        "../x",
        "a/../../x",
        "/etc/passwd",
        "a//b",
        "./a",
        "a\\b",
        "a\0b",
        "",
    ] {
        assert!(prepare_path(&root, &canonical, bad).is_err(), "{bad:?}");
    }
    let good = prepare_path(&root, &canonical, "sub/dir/file.md");
    assert_eq!(good.ok(), Some(root.join("sub/dir/file.md")));
    assert!(root.join("sub/dir").is_dir());
    // A file in the way is refused, not replaced.
    std::fs::write(root.join("plain"), b"x").unwrap_or_else(|e| panic!("{e}"));
    assert!(prepare_path(&root, &canonical, "plain/x.md").is_err());
    // A link in the way (never made here; planted) is refused, not followed.
    let outside = temp_dir("outside");
    #[cfg(unix)]
    {
        std::os::unix::fs::symlink(&outside, root.join("link")).unwrap_or_else(|e| panic!("{e}"));
        let error = prepare_path(&root, &canonical, "link/x.md").err();
        assert!(error.is_some_and(|e| e.message.contains("below a file")));
    }
    assert_eq!(
        std::fs::read_dir(&outside).map(Iterator::count).ok(),
        Some(0)
    );
    let _ = std::fs::remove_dir_all(&root);
    let _ = std::fs::remove_dir_all(&outside);
}

fn small_limits() -> IngestLimits {
    IngestLimits {
        max_clone_bytes: 1000,
        max_file_count: 3,
        max_file_bytes: 400,
        max_parsed_bytes: 500,
        clone_timeout: Duration::from_secs(5),
    }
}

fn object(relative: &str, size: i128) -> ArtifactObject {
    ArtifactObject {
        key: relative.to_owned(),
        size,
        modified: String::new(),
        relative_path: relative.to_owned(),
    }
}

#[test]
fn the_ingest_limits_apply_to_the_listing() {
    let limits = small_limits();
    for (objects, setting) in [
        (
            vec![
                object("a.md", 1),
                object("b.md", 1),
                object("c.md", 1),
                object("d.md", 1),
            ],
            "MAX_FILE_COUNT",
        ),
        (vec![object("big.bin", 401)], "MAX_FILE_BYTES"),
        (
            vec![object("a.py", 300), object("b.py", 300)],
            "MAX_PARSED_BYTES",
        ),
        (
            vec![
                object("a.bin", 400),
                object("b.bin", 400),
                object("c.bin", 300),
            ],
            "MAX_CLONE_BYTES",
        ),
    ] {
        let error = admit_listing(&objects, &limits, "artifact://docs").err();
        let error = error.unwrap_or_else(|| panic!("{setting}: admitted"));
        assert!(error.message.contains(setting), "{error}");
        // A ValueError; the source's name in it files it as artifact_error.
        assert_eq!(error.error_type, ErrorType::Value);
        assert_eq!(classify(error.error_type, &error.message), "artifact_error");
    }
    // A negative size counts nothing toward the limits.
    assert!(admit_listing(&[object("a.md", -5)], &limits, "artifact://docs").is_ok());
}

#[test]
fn pythons_caps_apply_too() {
    let folder = source("artifact://docs");
    let caps = ArtifactCaps {
        max_files: 2,
        max_bytes: 10,
    };
    let three = [object("a", 1), object("b", 1), object("c", 1)];
    let error = check_caps(&three, &folder, caps).err();
    assert_eq!(
        error.map(|e| e.message),
        Some("the artifact folder artifact://docs holds 3 objects, over the limit of 2 (ELITEA_DEEPWIKI_ARTIFACT_MAX_FILES)".to_owned())
    );
    let error = check_caps(&[object("a", 11)], &folder, caps).err();
    assert!(error.is_some_and(|e| e.message.contains("holds 11 bytes")));
    assert!(check_caps(&[object("a", 10)], &folder, caps).is_ok());
}

#[test]
fn received_bytes_are_held_to_the_limits() {
    let limits = small_limits();
    let mut received = Received {
        limits: &limits,
        caps: ArtifactCaps {
            max_files: 10,
            max_bytes: 600,
        },
        repo: "artifact://docs",
        url: "artifact://docs".to_owned(),
        total: 0,
    };
    assert!(received.admit("a", 300, 300).is_ok());
    let error = received.admit("b", 401, 401).err();
    assert!(error.is_some_and(|e| e.message.contains("MAX_FILE_BYTES")));
    let mut received = Received {
        total: 0,
        ..received
    };
    assert!(received.admit("a", 300, 300).is_ok());
    let error = received.admit("b", 301, 301).err();
    assert!(error.is_some_and(|e| e.message.contains("ARTIFACT_MAX_BYTES")));
}

// ---------------------------------------------------------------------------
// The transfer, against a mock object API
// ---------------------------------------------------------------------------

/// The mock elitea-main: a listing (paged by `page_size`) and the objects.
#[derive(Clone)]
struct Platform {
    objects: Arc<BTreeMap<String, Vec<u8>>>,
    /// Extra raw listing entries (placeholders, hostile keys).
    extra: Arc<Vec<Value>>,
    page_size: usize,
    /// The status every download answers with, when set.
    download_status: Option<u16>,
    /// Never answer a download.
    hang: bool,
    /// List every object as 1 byte, whatever it holds.
    lie_size: bool,
    /// Answer 404 to every listing page after the first (a cursor is set).
    vanish_after_first_page: bool,
    /// Every request: method, path, query, `Authorization`.
    seen: Arc<Mutex<Vec<(String, String, String)>>>,
}

const BEARER: &str = "callback-bearer-xyz";

async fn platform(
    State(state): State<Platform>,
    headers: HeaderMap,
    request: Request,
) -> HttpResponse {
    let path = request.uri().path().to_owned();
    let query = request.uri().query().unwrap_or_default().to_owned();
    let auth = headers
        .get("authorization")
        .and_then(|v| v.to_str().ok())
        .unwrap_or_default()
        .to_owned();
    if let Ok(mut seen) = state.seen.lock() {
        seen.push((path.clone(), query.clone(), auth.clone()));
    }
    if auth != format!("Bearer {BEARER}") {
        return (HttpStatus::UNAUTHORIZED, "").into_response();
    }
    let route = "/api/v2/artifacts/objects/42/docs";
    if path == route {
        let params: BTreeMap<String, String> = Url::parse(&format!("http://x/?{query}"))
            .map(|url| url.query_pairs().into_owned().collect())
            .unwrap_or_default();
        let prefix = params.get("prefix").cloned().unwrap_or_default();
        let start: usize = params
            .get("cursor")
            .and_then(|c| c.parse().ok())
            .unwrap_or(0);
        if state.vanish_after_first_page && params.contains_key("cursor") {
            return (HttpStatus::NOT_FOUND, "").into_response();
        }
        let mut all: Vec<Value> = state
            .objects
            .iter()
            .filter(|(key, _)| key.starts_with(&prefix))
            .map(|(key, body)| {
                let size = if state.lie_size { 1 } else { body.len() };
                json!({"key": key, "size_bytes": size, "modified_at": "2026-10-01T10:00:00Z"})
            })
            .collect();
        all.extend(state.extra.iter().cloned());
        let end = (start + state.page_size).min(all.len());
        let mut page = json!({"objects": all[start..end], "common_prefixes": []});
        if end < all.len() {
            page["next_cursor"] = json!(end.to_string());
        }
        return axum::Json(page).into_response();
    }
    let Some(encoded) = path.strip_prefix(&format!("{route}/")) else {
        return (HttpStatus::NOT_FOUND, "").into_response();
    };
    if state.hang {
        std::future::pending::<()>().await;
    }
    if let Some(status) = state.download_status {
        return HttpStatus::from_u16(status).map_or_else(
            |_| HttpStatus::IM_A_TEAPOT.into_response(),
            |s| (s, "").into_response(),
        );
    }
    let key = percent_decode(encoded);
    match state.objects.get(&key) {
        Some(body) => body.clone().into_response(),
        None => (HttpStatus::NOT_FOUND, "").into_response(),
    }
}

/// `%XX` escapes decoded (a `+` stays a `+` in a path).
fn percent_decode(text: &str) -> String {
    let bytes = text.as_bytes();
    let mut out = Vec::with_capacity(bytes.len());
    let mut index = 0;
    while index < bytes.len() {
        let hex = bytes
            .get(index + 1..index + 3)
            .and_then(|pair| std::str::from_utf8(pair).ok())
            .and_then(|pair| u8::from_str_radix(pair, 16).ok());
        match (bytes[index], hex) {
            (b'%', Some(byte)) => {
                out.push(byte);
                index += 3;
            }
            (byte, _) => {
                out.push(byte);
                index += 1;
            }
        }
    }
    String::from_utf8_lossy(&out).into_owned()
}

async fn serve(state: Platform) -> String {
    let app = axum::Router::new().fallback(platform).with_state(state);
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .unwrap_or_else(|e| panic!("{e}"));
    let port = listener.local_addr().map(|a| a.port()).unwrap_or_default();
    tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    format!("http://127.0.0.1:{port}")
}

fn state(objects: &[(&str, &[u8])]) -> Platform {
    Platform {
        objects: Arc::new(
            objects
                .iter()
                .map(|(k, v)| ((*k).to_owned(), v.to_vec()))
                .collect(),
        ),
        extra: Arc::new(Vec::new()),
        page_size: 2,
        download_status: None,
        hang: false,
        lie_size: false,
        vanish_after_first_page: false,
        seen: Arc::new(Mutex::new(Vec::new())),
    }
}

struct Run {
    target: ArtifactTarget,
    platform: PlatformObjects,
    client: Client,
    limits: IngestLimits,
    caps: ArtifactCaps,
    scratch: PathBuf,
    cancel: Arc<AtomicBool>,
}

impl Run {
    fn new(base: &str, repository: &str, name: &str) -> Self {
        let config =
            json!({"provider_type": "artifact", "repository": repository, "branch": "main"});
        let target = ArtifactTarget::from_repo_config(&config)
            .ok()
            .flatten()
            .unwrap_or_else(|| panic!("not a folder"));
        let platform = PlatformObjects::from_llm_settings(
            &json!({"api_base": format!("{base}/llm/v1"), "api_key": BEARER, "organization": "42"}),
        )
        .unwrap_or_else(|e| panic!("{e}"));
        let transport = crate::llm::Transport::new(&crate::llm::TransportSettings::default())
            .unwrap_or_else(|e| panic!("{e}"));
        Self {
            target,
            platform,
            client: transport.http_client().clone(),
            limits: IngestLimits::default(),
            caps: ArtifactCaps::default(),
            scratch: temp_dir(name),
            cancel: Arc::new(AtomicBool::new(false)),
        }
    }

    async fn run(&self) -> Result<ClonedRepository, EngineError> {
        let settings = super::super::IngestSettings {
            git_allowlist: super::super::egress::EgressPolicy::parse(None),
            limits: self.limits,
            scratch_path: self.scratch.clone(),
            artifact: self.caps,
        };
        super::super::ingest_artifact(
            &self.target,
            &self.platform,
            &self.client,
            &settings,
            &self.scratch,
            Arc::clone(&self.cancel),
        )
        .await
    }

    fn left(&self) -> usize {
        std::fs::read_dir(&self.scratch).map_or(0, Iterator::count)
    }
}

impl Drop for Run {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.scratch);
    }
}

#[tokio::test]
async fn a_folder_is_listed_paged_and_downloaded() {
    let mut mock = state(&[
        ("handbook/README.md", b"# Handbook\n"),
        ("handbook/src/app.py", b"def main():\n    return 1\n"),
        ("handbook/a b/ü+?#.md", b"odd name\n"),
        ("handbook-archive/old.md", b"not in the folder\n"),
        ("other/x.md", b"no\n"),
    ]);
    mock.extra = Arc::new(vec![json!({"key": "handbook/", "size_bytes": 0})]);
    let seen = Arc::clone(&mock.seen);
    let base = serve(mock).await;
    let run = Run::new(&base, "artifact://docs/handbook", "ok");
    let cloned = run.run().await.unwrap_or_else(|e| panic!("{e}"));

    let mut files: Vec<String> = Vec::new();
    let mut stack = vec![cloned.path.clone()];
    while let Some(dir) = stack.pop() {
        for entry in std::fs::read_dir(&dir).into_iter().flatten().flatten() {
            let meta = entry
                .path()
                .symlink_metadata()
                .unwrap_or_else(|e| panic!("{e}"));
            assert!(!meta.file_type().is_symlink());
            if meta.is_dir() {
                stack.push(entry.path());
            } else {
                let relative = entry
                    .path()
                    .strip_prefix(&cloned.path)
                    .map(|p| p.to_string_lossy().into_owned());
                files.push(relative.unwrap_or_default());
            }
        }
    }
    files.sort();
    assert_eq!(files, ["README.md", "a b/ü+?#.md", "src/app.py"]);
    assert_eq!(
        std::fs::read(cloned.path.join("src/app.py"))
            .ok()
            .as_deref(),
        Some(&b"def main():\n    return 1\n"[..])
    );
    assert_eq!(cloned.tree.files, 3);
    assert_eq!(cloned.tree.bytes, 11 + 25 + 9);

    // The identity is the listing's digest, as Python computes it.
    let listed = [
        ("handbook/README.md", 11),
        ("handbook/a b/ü+?#.md", 9),
        ("handbook/src/app.py", 25),
    ];
    let objects: Vec<ArtifactObject> = listed
        .iter()
        .map(|(key, size)| ArtifactObject {
            key: (*key).to_owned(),
            size: *size,
            modified: "2026-10-01T10:00:00Z".to_owned(),
            relative_path: String::new(),
        })
        .collect();
    let digest = listing_digest(&objects);
    assert_eq!(cloned.identity.commit(), digest);
    assert_eq!(
        cloned.identity.to_string(),
        format!("artifact://docs/handbook:main:{}", &digest[..8])
    );
    assert_eq!(
        cloned.path.file_name().and_then(|n| n.to_str()),
        Some(format!("docs_handbook_main_{}", &digest[..8]).as_str())
    );

    // Paged (2 a page, the prefix with its slash), one GET per object, the
    // bearer on every request, nothing outside the folder fetched.
    let seen = seen.lock().map(|s| s.clone()).unwrap_or_default();
    let listings: Vec<&str> = seen
        .iter()
        .filter(|(path, _, _)| path == "/api/v2/artifacts/objects/42/docs")
        .map(|(_, query, _)| query.as_str())
        .collect();
    assert_eq!(
        listings,
        ["prefix=handbook%2F", "prefix=handbook%2F&cursor=2"]
    );
    let downloads: Vec<&str> = seen
        .iter()
        .filter(|(path, _, _)| path != "/api/v2/artifacts/objects/42/docs")
        .map(|(path, _, _)| path.as_str())
        .collect();
    assert_eq!(
        downloads,
        [
            "/api/v2/artifacts/objects/42/docs/handbook/README.md",
            "/api/v2/artifacts/objects/42/docs/handbook/a%20b/%C3%BC+%3F%23.md",
            "/api/v2/artifacts/objects/42/docs/handbook/src/app.py",
        ]
    );
    assert!(
        seen.iter()
            .all(|(_, _, auth)| auth == &format!("Bearer {BEARER}"))
    );
}

#[tokio::test]
async fn a_hostile_listing_writes_nothing() {
    for hostile in [
        json!({"key": "handbook/../../etc/passwd", "size_bytes": 1}),
        json!({"key": "/etc/passwd", "size_bytes": 1}),
        json!({"key": "handbook/a\\..\\b", "size_bytes": 1}),
        json!({"key": "elsewhere/x.md", "size_bytes": 1}),
    ] {
        let mut mock = state(&[("handbook/a.md", b"a")]);
        mock.extra = Arc::new(vec![hostile.clone()]);
        let base = serve(mock).await;
        let run = Run::new(&base, "artifact://docs/handbook", "hostile");
        let error = run.run().await.err();
        assert_eq!(
            error.map(|e| classify(e.error_type, &e.message)),
            Some("invalid_input"),
            "{hostile}"
        );
        assert_eq!(run.left(), 0, "{hostile}: wrote a directory");
    }
}

#[tokio::test]
async fn the_limits_hold_over_the_listing_and_over_the_bytes() {
    // Listed honestly (500 > 400): refused before any download.
    let mock = state(&[("x.bin", &[0_u8; 500])]);
    let seen = Arc::clone(&mock.seen);
    let base = serve(mock).await;
    let mut run = Run::new(&base, "artifact://docs", "limits");
    run.limits = small_limits();
    let error = run.run().await.err();
    assert!(error.is_some_and(|e| e.message.contains("MAX_FILE_BYTES")));
    assert_eq!(seen.lock().map(|s| s.len()).unwrap_or_default(), 1);
    assert_eq!(run.left(), 0);

    // Listed as 1 byte, 500 sent: refused while receiving, nothing left.
    let mut mock = state(&[("x.bin", &[0_u8; 500])]);
    mock.lie_size = true;
    let base = serve(mock).await;
    let mut run = Run::new(&base, "artifact://docs", "lying");
    run.limits = small_limits();
    let error = run.run().await.err();
    assert!(
        error
            .as_ref()
            .is_some_and(|e| e.message.contains("at least") && e.message.contains("MAX_FILE_BYTES")),
        "{error:?}"
    );
    assert_eq!(run.left(), 0);
}

#[tokio::test]
async fn refusals_by_the_platform_are_reported_without_the_bearer() {
    for (status, category) in [
        (403, "artifact_error"),
        (404, "resource_not_found"),
        (500, "artifact_error"),
        (302, "artifact_error"),
    ] {
        let mut mock = state(&[("a.md", b"a")]);
        mock.download_status = Some(status);
        let base = serve(mock).await;
        let run = Run::new(&base, "artifact://docs", "status");
        let error = run
            .run()
            .await
            .err()
            .unwrap_or_else(|| panic!("{status}: ok"));
        assert_eq!(
            classify(error.error_type, &error.message),
            category,
            "{status}: {error}"
        );
        assert!(!error.message.contains(BEARER), "{error}");
        assert_eq!(run.left(), 0, "{status}: a partial directory was left");
    }
    // An unknown bucket lists as empty, as in Python.
    let base = serve(state(&[])).await;
    let run = Run::new(&base, "artifact://nothing", "empty");
    let error = run.run().await.err();
    assert!(error.is_some_and(|e| e.message.contains("holds no objects to index")));
}

#[tokio::test]
async fn a_404_after_the_first_listing_page_is_an_error() {
    // Page 1 answers 200 with a cursor, page 2 answers 404: the listing is
    // partial, so the run fails and indexes nothing.
    let mut mock = state(&[("a.md", b"a"), ("b.md", b"b"), ("c.md", b"c")]);
    mock.vanish_after_first_page = true;
    let seen = Arc::clone(&mock.seen);
    let base = serve(mock).await;
    let run = Run::new(&base, "artifact://docs", "vanish");
    let error = run
        .run()
        .await
        .err()
        .unwrap_or_else(|| panic!("a partial listing was indexed"));
    assert!(
        error.message.contains("HTTP 404") && error.message.contains("incomplete"),
        "{error}"
    );
    assert_eq!(classify(error.error_type, &error.message), "artifact_error");
    // Two listing pages, no download.
    let seen = seen.lock().map(|s| s.clone()).unwrap_or_default();
    assert_eq!(seen.len(), 2);
    assert!(
        seen.iter()
            .all(|(path, _, _)| path == "/api/v2/artifacts/objects/42/docs")
    );
    assert_eq!(run.left(), 0);
}

#[tokio::test]
async fn a_stop_and_the_deadline_end_a_hanging_download() {
    let mut mock = state(&[("a.md", b"a")]);
    mock.hang = true;
    let base = serve(mock).await;
    let run = Run::new(&base, "artifact://docs", "stop");
    let cancel = Arc::clone(&run.cancel);
    tokio::spawn(async move {
        tokio::time::sleep(Duration::from_millis(300)).await;
        cancel.store(true, Ordering::Release);
    });
    let started = std::time::Instant::now();
    let error = run.run().await.err();
    assert!(error.is_some_and(|e| e.message == "Invocation cancelled"));
    assert!(started.elapsed() < Duration::from_secs(3));
    assert_eq!(run.left(), 0);

    let mut mock = state(&[("a.md", b"a")]);
    mock.hang = true;
    let base = serve(mock).await;
    let mut run = Run::new(&base, "artifact://docs", "deadline");
    run.limits.clone_timeout = Duration::from_millis(400);
    let error = run.run().await.err();
    assert!(
        error.is_some_and(|e| e.message.contains("CLONE_TIMEOUT_SECONDS")),
        "no timeout"
    );
    assert_eq!(run.left(), 0);
}

#[tokio::test]
async fn a_listing_that_never_ends_is_cut_at_the_count() {
    let mut mock = state(&[]);
    mock.extra = Arc::new(
        (0..50)
            .map(|i| json!({"key": format!("f{i}.md"), "size_bytes": 1}))
            .collect(),
    );
    mock.page_size = 5;
    let seen = Arc::clone(&mock.seen);
    let base = serve(mock).await;
    let mut run = Run::new(&base, "artifact://docs", "count");
    run.caps.max_files = 7;
    let error = run.run().await.err();
    assert!(error.is_some_and(|e| e.message.contains("holds more than 7 objects")));
    // Two pages were enough to know.
    assert_eq!(seen.lock().map(|s| s.len()).unwrap_or_default(), 2);
}
