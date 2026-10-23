//! The port against the Python engine's own answers
//! (`tests/fixtures/artifact_source.json`, written by
//! `parity/python_artifact_source.py`), the path and limit checks, and the
//! transfer against a scripted `ArtifactObjectService`.

use super::*;
use crate::names::SettingNames;
use elitea_engine_core::errors::classify;
use std::collections::BTreeMap;
use std::sync::atomic::AtomicUsize;
use std::sync::{Arc, Mutex};

const FIXTURE: &str = include_str!("../../tests/fixtures/artifact_source.json");

fn fixture() -> Value {
    serde_json::from_str(FIXTURE).unwrap_or_else(|e| panic!("fixture: {e}"))
}

/// The names the Python engine's messages carry: the fixture is its own
/// output, so the parity cases run under the `DeepWiki` engine's names. A
/// consumer of this crate sets its own (see `crate::names`).
const PYTHON_NAMES: SettingNames = SettingNames {
    max_clone_bytes: "ELITEA_DEEPWIKI_MAX_CLONE_BYTES",
    max_file_count: "ELITEA_DEEPWIKI_MAX_FILE_COUNT",
    max_file_bytes: "ELITEA_DEEPWIKI_MAX_FILE_BYTES",
    max_parsed_bytes: "ELITEA_DEEPWIKI_MAX_PARSED_BYTES",
    clone_timeout_seconds: "ELITEA_DEEPWIKI_CLONE_TIMEOUT_SECONDS",
    artifact_max_files: "ELITEA_DEEPWIKI_ARTIFACT_MAX_FILES",
    artifact_max_bytes: "ELITEA_DEEPWIKI_ARTIFACT_MAX_BYTES",
    git_allowlist: "ELITEA_DEEPWIKI_GIT_ALLOWLIST",
    user_agent: "elitea-deepwiki-engine/test",
};

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
        let collected = collect_objects(&folder, &page_items(case)).and_then(|objects| {
            check_caps(&objects, &folder, caps, &PYTHON_NAMES).map(|()| objects)
        });
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

        // The listing's identity. What a consumer NAMES the folder (DeepWiki's
        // wiki id) is checked by that consumer against the same fixture.
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
        assert_eq!(
            identity.to_string(),
            text(&case["repo_identifier"]),
            "{name}"
        );
    }
}

#[test]
fn the_platform_settings_match_python() {
    for case in fixture()["settings"].as_array().into_iter().flatten() {
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
        assert_eq!(parsed.project_id().to_string(), project);
    }
}

#[test]
fn the_platform_credential_is_checked() {
    for bad in [
        json!({"api_base": "https://p/llm/v1", "organization": "1"}),
        json!({"api_key": "k", "organization": "x"}),
        json!({"api_key": "k", "organization": "0"}),
        json!({"api_key": "k"}),
        json!({"api_key": "line\nbreak", "organization": "1"}),
    ] {
        let error = PlatformObjects::from_llm_settings(&bad).err();
        assert_eq!(error.map(|e| e.error_type), Some(ErrorType::Value), "{bad}");
    }
    // `api_base` is not needed: the peer is the configured platform listener.
    let parsed = PlatformObjects::from_llm_settings(
        &json!({"api_key": "bearer-secret-9", "organization": 5}),
    );
    assert_eq!(
        parsed.as_ref().map(PlatformObjects::project_id).ok(),
        Some(5)
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
        names: &PYTHON_NAMES,
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
    let error = check_caps(&three, &folder, caps, &PYTHON_NAMES).err();
    assert_eq!(
        error.map(|e| e.message),
        Some("the artifact folder artifact://docs holds 3 objects, over the limit of 2 (ELITEA_DEEPWIKI_ARTIFACT_MAX_FILES)".to_owned())
    );
    let error = check_caps(&[object("a", 11)], &folder, caps, &PYTHON_NAMES).err();
    assert!(error.is_some_and(|e| e.message.contains("holds 11 bytes")));
    assert!(check_caps(&[object("a", 10)], &folder, caps, &PYTHON_NAMES).is_ok());
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
// The transfer, against a scripted ArtifactObjectService
// ---------------------------------------------------------------------------

use elitea_platform_rpc::pb;
use elitea_platform_rpc::testing::{ArtifactObjectService, artifact_refusal, serve_artifacts};
use std::pin::Pin;
use tonic::{Request, Response, Status, Streaming};

type Frames<T> = Pin<Box<dyn tokio_stream::Stream<Item = Result<T, Status>> + Send>>;

/// One call the platform saw: the RPC, the bucket, the key (the prefix for
/// a listing), the cursor, the bearer and the project.
#[derive(Clone, Debug, PartialEq, Eq)]
struct Seen {
    op: &'static str,
    bucket: String,
    key: String,
    cursor: String,
    bearer: String,
    project_id: i64,
}

/// The scripted elitea-main: a listing (paged by `page_size`) and the
/// objects.
#[derive(Clone)]
struct Platform {
    objects: Arc<BTreeMap<String, Vec<u8>>>,
    /// Extra raw listing entries (placeholders, hostile keys).
    extra: Arc<Vec<Value>>,
    page_size: usize,
    /// The HTTP status every download is refused with, when set.
    download_status: Option<u16>,
    /// Never answer a download.
    hang: bool,
    /// List every object as 1 byte, whatever it holds.
    lie_size: bool,
    /// Refuse 404 every listing page after the first (a cursor is set).
    vanish_after_first_page: bool,
    seen: Arc<Mutex<Vec<Seen>>>,
}

const BEARER: &str = "callback-bearer-xyz";

impl Platform {
    fn record<T>(
        &self,
        request: &Request<T>,
        op: &'static str,
        bucket: &str,
        key: &str,
        cursor: &str,
        project_id: i64,
    ) -> Result<(), Status> {
        let bearer = request
            .metadata()
            .get("authorization")
            .and_then(|v| v.to_str().ok())
            .unwrap_or_default()
            .to_owned();
        if let Ok(mut seen) = self.seen.lock() {
            seen.push(Seen {
                op,
                bucket: bucket.to_owned(),
                key: key.to_owned(),
                cursor: cursor.to_owned(),
                bearer: bearer.clone(),
                project_id,
            });
        }
        if bearer != format!("Bearer {BEARER}") {
            return Err(artifact_refusal(401, "Unauthorized", "no bearer"));
        }
        if project_id != 42 || bucket != "docs" {
            return Err(artifact_refusal(404, "NotFound", "bucket not found"));
        }
        Ok(())
    }
}

#[tonic::async_trait]
impl ArtifactObjectService for Platform {
    type ListObjectsStream = Frames<pb::ArtifactListObjectsResponseV1>;
    type GetObjectStream = Frames<pb::ArtifactGetObjectResponseV1>;

    async fn list_buckets(
        &self,
        _: Request<pb::ArtifactListBucketsRequestV1>,
    ) -> Result<Response<pb::ArtifactListBucketsResponseV1>, Status> {
        Err(Status::unimplemented("not a listing of buckets"))
    }

    async fn create_bucket(
        &self,
        _: Request<pb::ArtifactCreateBucketRequestV1>,
    ) -> Result<Response<pb::ArtifactCreateBucketResponseV1>, Status> {
        Err(Status::unimplemented("read only"))
    }

    async fn list_objects(
        &self,
        request: Request<pb::ArtifactListObjectsRequestV1>,
    ) -> Result<Response<Self::ListObjectsStream>, Status> {
        let message = request.get_ref().clone();
        self.record(
            &request,
            "ListObjects",
            &message.bucket,
            &message.prefix,
            &message.cursor,
            message.project_id,
        )?;
        if self.vanish_after_first_page && !message.cursor.is_empty() {
            return Err(artifact_refusal(404, "NotFound", "bucket not found"));
        }
        let start: usize = message.cursor.parse().unwrap_or(0);
        let mut all: Vec<pb::ArtifactObjectInfoV1> = self
            .objects
            .iter()
            .filter(|(key, _)| key.starts_with(&message.prefix))
            .map(|(key, body)| pb::ArtifactObjectInfoV1 {
                key: key.clone(),
                size_bytes: if self.lie_size {
                    1
                } else {
                    i64::try_from(body.len()).unwrap_or(i64::MAX)
                },
                modified_at: "2026-10-01T10:00:00Z".to_owned(),
                ..pb::ArtifactObjectInfoV1::default()
            })
            .collect();
        all.extend(self.extra.iter().map(|entry| pb::ArtifactObjectInfoV1 {
            key: text(&entry["key"]).to_owned(),
            size_bytes: entry["size_bytes"].as_i64().unwrap_or_default(),
            ..pb::ArtifactObjectInfoV1::default()
        }));
        let end = (start + self.page_size).min(all.len());
        let page = pb::ArtifactListObjectsResponseV1 {
            objects: all[start.min(end)..end].to_vec(),
            common_prefixes: Vec::new(),
            next_cursor: if end < all.len() {
                end.to_string()
            } else {
                String::new()
            },
        };
        Ok(Response::new(Box::pin(tokio_stream::iter([Ok(page)]))))
    }

    async fn stat(
        &self,
        _: Request<pb::ArtifactStatRequestV1>,
    ) -> Result<Response<pb::ArtifactStatResponseV1>, Status> {
        Err(Status::unimplemented("the ingest never stats"))
    }

    async fn get_object(
        &self,
        request: Request<pb::ArtifactGetObjectRequestV1>,
    ) -> Result<Response<Self::GetObjectStream>, Status> {
        use pb::artifact_get_object_response_v1::Frame;
        let object = request.get_ref().object.clone().unwrap_or_default();
        self.record(
            &request,
            "GetObject",
            &object.bucket,
            &object.key,
            "",
            object.project_id,
        )?;
        if self.hang {
            std::future::pending::<()>().await;
        }
        if let Some(status) = self.download_status {
            return Err(artifact_refusal(status, "Refused", "refused"));
        }
        let Some(body) = self.objects.get(&object.key) else {
            return Err(artifact_refusal(404, "NotFound", "object not found"));
        };
        let info = pb::ArtifactObjectInfoV1 {
            key: object.key.clone(),
            size_bytes: i64::try_from(body.len()).unwrap_or(i64::MAX),
            ..pb::ArtifactObjectInfoV1::default()
        };
        // Two chunks, so a file is written from more than one.
        let (head, tail) = body.split_at(body.len() / 2);
        let frames = [
            Frame::Info(info),
            Frame::Data(head.to_vec().into()),
            Frame::Data(tail.to_vec().into()),
        ]
        .map(|frame| Ok(pb::ArtifactGetObjectResponseV1 { frame: Some(frame) }));
        Ok(Response::new(Box::pin(tokio_stream::iter(frames))))
    }

    async fn put_object(
        &self,
        _: Request<Streaming<pb::ArtifactPutObjectRequestV1>>,
    ) -> Result<Response<pb::ArtifactPutObjectResponseV1>, Status> {
        Err(Status::unimplemented("read only"))
    }

    async fn delete(
        &self,
        _: Request<pb::ArtifactDeleteRequestV1>,
    ) -> Result<Response<pb::ArtifactDeleteResponseV1>, Status> {
        Err(Status::unimplemented("read only"))
    }

    async fn delete_batch(
        &self,
        _: Request<pb::ArtifactDeleteBatchRequestV1>,
    ) -> Result<Response<pb::ArtifactDeleteBatchResponseV1>, Status> {
        Err(Status::unimplemented("read only"))
    }
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

fn seen(platform: &Platform) -> Vec<Seen> {
    platform.seen.lock().map(|s| s.clone()).unwrap_or_default()
}

struct Run {
    target: ArtifactTarget,
    platform: PlatformObjects,
    artifacts: ArtifactClient,
    limits: IngestLimits,
    caps: ArtifactCaps,
    scratch: PathBuf,
    cancel: Arc<AtomicBool>,
}

impl Run {
    fn new(mock: &Platform, repository: &str, name: &str) -> Self {
        let config =
            json!({"provider_type": "artifact", "repository": repository, "branch": "main"});
        let target = ArtifactTarget::from_repo_config(&config)
            .ok()
            .flatten()
            .unwrap_or_else(|| panic!("not a folder"));
        // `api_base` names only the model route: it never decides the peer.
        let platform = PlatformObjects::from_llm_settings(&json!({
            "api_base": "https://elitea-platform.invalid/llm/v1",
            "api_key": BEARER,
            "organization": "42"
        }))
        .unwrap_or_else(|e| panic!("{e}"));
        Self {
            target,
            platform,
            artifacts: ArtifactClient::new(serve_artifacts(mock.clone())),
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
            &self.artifacts,
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
    let run = Run::new(&mock, "artifact://docs/handbook", "ok");
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

    // Paged (2 a page, the prefix with its slash), one GetObject per
    // object, the bearer and the project on every call, nothing outside
    // the folder fetched.
    let seen = seen(&mock);
    let listings: Vec<(&str, &str)> = seen
        .iter()
        .filter(|call| call.op == "ListObjects")
        .map(|call| (call.key.as_str(), call.cursor.as_str()))
        .collect();
    assert_eq!(listings, [("handbook/", ""), ("handbook/", "2")]);
    let downloads: Vec<&str> = seen
        .iter()
        .filter(|call| call.op == "GetObject")
        .map(|call| call.key.as_str())
        .collect();
    assert_eq!(
        downloads,
        [
            "handbook/README.md",
            "handbook/a b/ü+?#.md",
            "handbook/src/app.py",
        ]
    );
    assert!(
        seen.iter()
            .all(|call| call.bearer == format!("Bearer {BEARER}")
                && call.project_id == 42
                && call.bucket == "docs")
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
        let run = Run::new(&mock, "artifact://docs/handbook", "hostile");
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
    let mut run = Run::new(&mock, "artifact://docs", "limits");
    run.limits = small_limits();
    let error = run.run().await.err();
    assert!(error.is_some_and(|e| e.message.contains("MAX_FILE_BYTES")));
    assert_eq!(seen(&mock).len(), 1);
    assert_eq!(run.left(), 0);

    // Listed as 1 byte, 500 sent: refused while receiving, nothing left.
    let mut mock = state(&[("x.bin", &[0_u8; 500])]);
    mock.lie_size = true;
    let mut run = Run::new(&mock, "artifact://docs", "lying");
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
        (401, "artifact_error"),
    ] {
        let mut mock = state(&[("a.md", b"a")]);
        mock.download_status = Some(status);
        let run = Run::new(&mock, "artifact://docs", "status");
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
    let run = Run::new(&state(&[]), "artifact://nothing", "empty");
    let error = run.run().await.err();
    assert!(error.is_some_and(|e| e.message.contains("holds no objects to index")));
}

#[tokio::test]
async fn an_unreachable_platform_is_a_transfer_error_not_a_refusal() {
    let mock = state(&[("a.md", b"a")]);
    let mut run = Run::new(&mock, "artifact://docs", "unreachable");
    run.artifacts = ArtifactClient::new(
        tonic::transport::Endpoint::from_static("http://127.0.0.1:9").connect_lazy(),
    );
    let error = run
        .run()
        .await
        .err()
        .unwrap_or_else(|| panic!("listed without a platform"));
    assert!(
        error
            .message
            .starts_with("Failed to list the artifact folder")
            && !error.message.contains("HTTP"),
        "{error}"
    );
    assert!(!error.message.contains(BEARER), "{error}");
    assert_eq!(run.left(), 0);
}

#[tokio::test]
async fn a_404_after_the_first_listing_page_is_an_error() {
    // Page 1 answers with a cursor, page 2 is refused 404: the listing is
    // partial, so the run fails and indexes nothing.
    let mut mock = state(&[("a.md", b"a"), ("b.md", b"b"), ("c.md", b"c")]);
    mock.vanish_after_first_page = true;
    let run = Run::new(&mock, "artifact://docs", "vanish");
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
    let seen = seen(&mock);
    assert_eq!(seen.len(), 2);
    assert!(seen.iter().all(|call| call.op == "ListObjects"));
    assert_eq!(run.left(), 0);
}

#[tokio::test]
async fn a_stop_and_the_deadline_end_a_hanging_download() {
    let mut mock = state(&[("a.md", b"a")]);
    mock.hang = true;
    let run = Run::new(&mock, "artifact://docs", "stop");
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
    let mut run = Run::new(&mock, "artifact://docs", "deadline");
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
    let mut run = Run::new(&mock, "artifact://docs", "count");
    run.caps.max_files = 7;
    let error = run.run().await.err();
    assert!(error.is_some_and(|e| e.message.contains("holds more than 7 objects")));
    // Two pages were enough to know.
    assert_eq!(seen(&mock).len(), 2);
}
