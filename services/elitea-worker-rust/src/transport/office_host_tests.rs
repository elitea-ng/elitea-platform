//! The Office host over a fake of the platform object API: the exact
//! requests it sends, the failures it classifies, and a real `pptx` tool
//! round trip of a deck's bytes through it (ADR-0032 P3).

use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

use adk_rust::futures::stream;
use adk_rust::tool::SimpleToolContext;
use adk_rust::{Content, Llm, LlmRequest, LlmResponse, LlmResponseStream, Part, ReadonlyContext};
use async_trait::async_trait;
use elitea_agent_runtime::host::{HostError, HostErrorCode, PlatformWriter};
use elitea_agent_runtime::platform::{
    ArtifactDeleteOutcome, ArtifactDeleteRequest, ArtifactListOutcome, ArtifactListRequest,
    ArtifactReadOutcome, ArtifactReadRequest, ArtifactWriteOutcome, ArtifactWriteRequest,
    ProjectContextWriteOutcome, ProjectContextWriteRequest, SkillWriteOutcome, SkillWriteRequest,
};
use elitea_agent_runtime::toolkits::{
    ArtifactToolAuthority, OfficeFailure, OfficeModelRequest, OfficeToolHost,
    materialize_configured_toolsets_with_artifact_authority,
};
use elitea_connectors::source::Failure;
use elitea_connectors::transport::{
    HeaderMap, HeaderValue, Method, Request, Response, StatusCode, Transport, TransportError,
    header,
};
use serde_json::{Map, Value, json};

use super::office_host::{ClaimOfficeHost, OfficeRuntime};
use crate::indexing::tools::{LazyStepbackModel, StepbackBinding};
use crate::toolkits::{FrozenToolSnapshot, ToolAdmissionPolicy};

const ORIGIN: &str = "https://platform.internal:9446";

/// What the fake store saw.
#[derive(Default)]
struct Store {
    objects: BTreeMap<String, Vec<u8>>,
    log: Vec<(String, String, Option<String>, Option<String>)>,
}

/// A fake of `/api/v2/artifacts/objects/{project}/{bucket}[/{key}]`: HEAD and
/// GET by key, and the multipart POST that stores a `file` part.
pub(crate) struct ObjectApi {
    store: Mutex<Store>,
}

impl ObjectApi {
    pub(crate) fn new() -> Arc<Self> {
        Arc::new(Self {
            store: Mutex::new(Store::default()),
        })
    }

    pub(crate) fn put(&self, key: &str, bytes: Vec<u8>) {
        self.store
            .lock()
            .expect("store")
            .objects
            .insert(key.to_owned(), bytes);
    }

    pub(crate) fn get(&self, key: &str) -> Option<Vec<u8>> {
        self.store.lock().expect("store").objects.get(key).cloned()
    }

    fn log(&self) -> Vec<(String, String, Option<String>, Option<String>)> {
        self.store.lock().expect("store").log.clone()
    }
}

fn split_multipart(body: &[u8], boundary: &str) -> Option<(String, String, Vec<u8>)> {
    let marker = format!("--{boundary}").into_bytes();
    let start = body.windows(marker.len()).position(|w| w == marker)? + marker.len();
    let head_end = body[start..].windows(4).position(|w| w == b"\r\n\r\n")? + start;
    let head = String::from_utf8_lossy(&body[start..head_end]).into_owned();
    let data_start = head_end + 4;
    let closing = format!("\r\n--{boundary}--").into_bytes();
    let data_end = body.windows(closing.len()).rposition(|w| w == closing)?;
    let filename = head
        .split("filename=\"")
        .nth(1)?
        .split('"')
        .next()?
        .to_owned();
    let content_type = head
        .split("Content-Type: ")
        .nth(1)?
        .lines()
        .next()?
        .trim()
        .to_owned();
    Some((filename, content_type, body[data_start..data_end].to_vec()))
}

#[async_trait]
#[allow(clippy::too_many_lines, reason = "a fake of one HTTP API")]
impl Transport for ObjectApi {
    async fn execute(&self, request: Request) -> Result<Response, TransportError> {
        let url = request.url().clone();
        let auth = request
            .headers()
            .get(header::AUTHORIZATION)
            .and_then(|v| v.to_str().ok())
            .map(str::to_owned);
        let path = url.path().to_owned();
        let after = path
            .strip_prefix("/api/v2/artifacts/objects/")
            .unwrap_or("");
        // Any project: the first segment.
        let rest = after.split_once('/').map_or("", |(_, r)| r).to_owned();
        let (bucket, key) = rest
            .split_once('/')
            .map_or((rest.clone(), String::new()), |(b, k)| {
                (b.to_owned(), k.to_owned())
            });
        let mut store = self.store.lock().expect("store");
        store.log.push((
            request.method().to_string(),
            path.clone(),
            url.query().map(str::to_owned),
            auth,
        ));
        // `POST /api/v2/artifacts/buckets/{project}`: the name is in the body.
        if path.starts_with("/api/v2/artifacts/buckets/") {
            let body: Value = request
                .body()
                .and_then(|b| b.as_bytes())
                .and_then(|b| serde_json::from_slice(b).ok())
                .unwrap_or(Value::Null);
            let name = body.get("name").and_then(Value::as_str).unwrap_or_default();
            let status = match name {
                "taken" => StatusCode::CONFLICT,
                "locked" => StatusCode::FORBIDDEN,
                "bad-name" => StatusCode::BAD_REQUEST,
                "down" => StatusCode::INTERNAL_SERVER_ERROR,
                _ => StatusCode::OK,
            };
            let mut headers = HeaderMap::new();
            headers.insert(
                header::CONTENT_TYPE,
                HeaderValue::from_static("application/json"),
            );
            let reply = if status == StatusCode::OK {
                json!({"name": name, "retention_days": body.get("retention_days")}).to_string()
            } else {
                String::new()
            };
            return Ok(Response::from_bytes(status, headers, reply));
        }
        // Buckets that answer a fixed status, for the classification table.
        let fixed = match bucket.as_str() {
            "typed" => Some(StatusCode::UNSUPPORTED_MEDIA_TYPE),
            "invalid" => Some(StatusCode::UNPROCESSABLE_ENTITY),
            "huge" => Some(StatusCode::PAYLOAD_TOO_LARGE),
            "broken" => Some(StatusCode::INTERNAL_SERVER_ERROR),
            _ => None,
        };
        if let Some(status) = fixed {
            return Ok(Response::from_bytes(status, HeaderMap::new(), Vec::new()));
        }
        if bucket == "locked" {
            return Ok(Response::from_bytes(
                StatusCode::FORBIDDEN,
                HeaderMap::new(),
                Vec::new(),
            ));
        }
        if request.method() == Method::POST {
            let content_type = request
                .headers()
                .get(header::CONTENT_TYPE)
                .and_then(|v| v.to_str().ok())
                .unwrap_or("")
                .to_owned();
            let boundary = content_type
                .split("boundary=")
                .nth(1)
                .unwrap_or("")
                .to_owned();
            let body = request
                .body()
                .and_then(|b| b.as_bytes())
                .unwrap_or_default()
                .to_vec();
            let Some((filename, media, data)) = split_multipart(&body, &boundary) else {
                return Ok(Response::from_bytes(
                    StatusCode::BAD_REQUEST,
                    HeaderMap::new(),
                    Vec::new(),
                ));
            };
            // A taken name is a 409 unless the write asked to replace it.
            if store.objects.contains_key(&filename)
                && url.query().is_none_or(|q| !q.contains("overwrite=true"))
            {
                return Ok(Response::from_bytes(
                    StatusCode::CONFLICT,
                    HeaderMap::new(),
                    Vec::new(),
                ));
            }
            let size = data.len();
            store.objects.insert(filename.clone(), data);
            let mut headers = HeaderMap::new();
            headers.insert(
                header::CONTENT_TYPE,
                HeaderValue::from_static("application/json"),
            );
            let reply = json!({"key": filename, "size_bytes": size, "media_type": media});
            return Ok(Response::from_bytes(
                StatusCode::CREATED,
                headers,
                reply.to_string(),
            ));
        }
        let Some(bytes) = store.objects.get(&key).cloned() else {
            return Ok(Response::from_bytes(
                StatusCode::NOT_FOUND,
                HeaderMap::new(),
                Vec::new(),
            ));
        };
        let mut headers = HeaderMap::new();
        headers.insert(
            header::CONTENT_LENGTH,
            HeaderValue::from_str(&bytes.len().to_string()).expect("length"),
        );
        let body = if request.method() == Method::HEAD {
            Vec::new()
        } else {
            bytes
        };
        Ok(Response::from_bytes(StatusCode::OK, headers, body))
    }
}

struct CaptureWriter(Arc<Mutex<Vec<u8>>>);

impl std::io::Write for CaptureWriter {
    fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
        self.0.lock().expect("log").extend_from_slice(buf);
        Ok(buf.len())
    }

    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

struct Scripted(Mutex<Vec<String>>, Mutex<Vec<(String, usize)>>);

#[async_trait]
impl Llm for Scripted {
    fn name(&self) -> &'static str {
        "scripted"
    }

    async fn generate_content(
        &self,
        request: LlmRequest,
        _streaming: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        let text: String = request
            .contents
            .iter()
            .flat_map(|c| c.parts.iter())
            .filter_map(|p| match p {
                Part::Text { text } => Some(text.clone()),
                _ => None,
            })
            .collect();
        let images = request
            .contents
            .iter()
            .flat_map(|c| c.parts.iter())
            .filter(|p| matches!(p, Part::InlineData { .. }))
            .count();
        self.1.lock().expect("seen").push((text, images));
        let reply = self.0.lock().expect("replies").remove(0);
        Ok(Box::pin(stream::once(async move {
            Ok(LlmResponse {
                content: Some(Content::new("model").with_text(reply)),
                turn_complete: true,
                finish_reason: Some(adk_rust::FinishReason::Stop),
                ..Default::default()
            })
        })))
    }
}

fn host(api: &Arc<ObjectApi>, replies: Vec<&str>) -> (Arc<ClaimOfficeHost>, Arc<Scripted>) {
    let runtime = Arc::new(OfficeRuntime::with_transport(
        ORIGIN,
        Arc::clone(api) as Arc<dyn Transport>,
    ));
    let llm = Arc::new(Scripted(
        Mutex::new(replies.into_iter().map(str::to_owned).collect()),
        Mutex::new(Vec::new()),
    ));
    let model = LazyStepbackModel::bound(StepbackBinding {
        model: Arc::clone(&llm) as Arc<dyn Llm>,
        model_name: "gpt-test".to_owned(),
    });
    let host = ClaimOfficeHost::new(runtime, "pat-token", "42", model).expect("host");
    (Arc::new(host), llm)
}

#[tokio::test]
async fn objects_are_read_and_written_as_bytes_under_the_claims_credential() {
    let api = ObjectApi::new();
    let (host, _) = host(&api, vec![]);
    // Bytes that no text path could carry: NULs, CRLF and a multipart-like tail.
    let bytes: Vec<u8> = (0..=255u8)
        .chain(b"\r\n--x--\r\n".iter().copied())
        .collect();
    let written = host
        .write_object(
            "decks",
            "q3/review.pptx",
            "application/octet-stream",
            bytes.clone(),
            true,
        )
        .await
        .expect("written");
    assert_eq!(written.bucket, "decks");
    assert_eq!(written.name, "q3/review.pptx");
    assert_eq!(written.byte_length, bytes.len() as u64);
    assert_eq!(api.get("q3/review.pptx").as_deref(), Some(bytes.as_slice()));
    assert_eq!(
        host.read_object("decks", "q3/review.pptx", 1 << 20)
            .await
            .expect("read"),
        bytes
    );

    let log = api.log();
    let post = log.iter().find(|r| r.0 == "POST").expect("an upload");
    assert_eq!(post.1, "/api/v2/artifacts/objects/42/decks");
    assert_eq!(
        post.2.as_deref(),
        Some("overwrite=true"),
        "replacing is the tool's contract"
    );
    for request in &log {
        assert_eq!(
            request.3.as_deref(),
            Some("Bearer pat-token"),
            "{request:?}"
        );
    }
    // The size is asked of a HEAD before the body is fetched.
    let methods: Vec<&str> = log.iter().map(|r| r.0.as_str()).collect();
    assert_eq!(methods, ["POST", "HEAD", "GET"]);
}

#[tokio::test]
#[allow(clippy::too_many_lines, reason = "one table of causes")]
async fn failures_are_classified_and_a_large_object_is_never_buffered() {
    let api = ObjectApi::new();
    api.put("big.pptx", vec![7; 4096]);
    api.put("taken.pptx", vec![1]);
    let (host, _) = host(&api, vec![]);
    let kind = |e: elitea_agent_runtime::toolkits::OfficeHostError| e.kind();
    let any = "application/octet-stream";
    assert_eq!(
        kind(
            host.read_object("decks", "missing.pptx", 1 << 20)
                .await
                .expect_err("404")
        ),
        OfficeFailure::NotFound
    );
    assert_eq!(
        kind(
            host.read_object("locked", "a.pptx", 1 << 20)
                .await
                .expect_err("403")
        ),
        OfficeFailure::AccessRefused
    );
    let before = api.log().len();
    assert_eq!(
        kind(
            host.read_object("decks", "big.pptx", 100)
                .await
                .expect_err("too big")
        ),
        OfficeFailure::TooLarge
    );
    let after: Vec<String> = api.log()[before..].iter().map(|r| r.0.clone()).collect();
    assert_eq!(after, ["HEAD"], "refused from the size alone, no GET");
    // Names that could break out of the multipart header or the URL.
    for name in [
        "a\"b.pptx",
        "a\r\nb.pptx",
        "a\\b.pptx",
        "../x.pptx",
        "/abs.pptx",
        "",
    ] {
        assert_eq!(
            kind(
                host.write_object("decks", name, any, vec![1], true)
                    .await
                    .expect_err(name)
            ),
            OfficeFailure::InvalidRequest,
            "{name:?}"
        );
    }
    assert!(api.get("a\"b.pptx").is_none());
    // Each status of the object API is its own cause.
    let write = |bucket: &'static str| {
        let host = &host;
        async move {
            host.write_object(bucket, "n.pptx", any, vec![1], true)
                .await
        }
    };
    assert_eq!(
        kind(write("locked").await.expect_err("403")),
        OfficeFailure::AccessRefused
    );
    assert_eq!(
        kind(write("typed").await.expect_err("415")),
        OfficeFailure::UnsupportedType
    );
    assert_eq!(
        kind(write("invalid").await.expect_err("422")),
        OfficeFailure::InvalidRequest
    );
    assert_eq!(
        kind(write("huge").await.expect_err("413")),
        OfficeFailure::TooLarge
    );
    assert_eq!(
        kind(write("broken").await.expect_err("500")),
        OfficeFailure::Unreachable
    );
    // The caller's overwrite flag decides the query, and a 409 is "already exists".
    let before = api.log().len();
    let taken = host
        .write_object("decks", "taken.pptx", any, vec![2], false)
        .await;
    assert_eq!(kind(taken.expect_err("409")), OfficeFailure::AlreadyExists);
    assert_eq!(api.get("taken.pptx"), Some(vec![1]), "nothing was replaced");
    assert_eq!(api.log()[before].2, None, "no overwrite query unless asked");
    host.write_object("decks", "taken.pptx", any, vec![3], true)
        .await
        .expect("replaced when asked");
    assert_eq!(api.get("taken.pptx"), Some(vec![3]));
    assert_eq!(
        api.log().last().expect("log").2.as_deref(),
        Some("overwrite=true")
    );
    // Transport-level causes.
    let refused = super::office_host::failure(&Failure::Refused);
    assert_eq!(
        refused.kind(),
        OfficeFailure::AccessRefused,
        "egress refused"
    );
    let client = super::office_host::failure(&Failure::Client("bad".into()));
    assert_eq!(
        client.kind(),
        OfficeFailure::AccessRefused,
        "unusable credential"
    );
    let down = super::office_host::failure(&Failure::Transport("reset".into()));
    assert_eq!(down.kind(), OfficeFailure::Unreachable);
    assert_eq!(
        super::office_host::model_failure("the project model budget is exhausted").kind(),
        OfficeFailure::BudgetRefused
    );
    assert_eq!(
        super::office_host::model_failure("the model gateway timed out").kind(),
        OfficeFailure::Unreachable
    );
}

#[test]
fn the_only_reachable_host_is_the_platform_origin() {
    let runtime = OfficeRuntime::new("https://platform.internal:9446").expect("runtime");
    assert!(runtime.permits("https://platform.internal:9446/api/v2/artifacts/objects/1/b/k"));
    assert!(runtime.permits("https://PLATFORM.internal/x"));
    for other in [
        "https://evil.example/x",
        "https://sub.platform.internal/x",
        "https://platform.internal.evil.example/x",
        "http://169.254.169.254/latest",
    ] {
        assert!(!runtime.permits(other), "{other}");
    }
    assert!(OfficeRuntime::new("not a url").is_err());
}

#[tokio::test]
async fn the_model_call_sends_the_prompt_and_images_on_the_runs_model() {
    let api = ObjectApi::new();
    let (host, llm) = host(&api, vec!["the reply"]);
    let reply = host
        .complete(OfficeModelRequest {
            prompt: "translate this".to_owned(),
            images_png: std::sync::Arc::from(vec![
                vec![0x89, b'P', b'N', b'G'],
                vec![0x89, b'P', b'N', b'G'],
            ]),
            image_media_types: std::sync::Arc::from(vec![
                "image/png".to_owned(),
                "image/jpeg".to_owned(),
            ]),
        })
        .await
        .expect("reply");
    assert_eq!(reply, "the reply");
    let seen = llm.1.lock().expect("seen").clone();
    assert_eq!(seen, [("translate this".to_owned(), 2)]);
    // No model bound at all (a pipeline node): a fixed sentence, not a panic,
    // and no "model could not be bound" warning, which only a failed binding
    // deserves.
    let runtime = Arc::new(OfficeRuntime::with_transport(
        ORIGIN,
        Arc::clone(&api) as Arc<dyn Transport>,
    ));
    let unbound =
        ClaimOfficeHost::new(runtime, "pat", "42", LazyStepbackModel::unbound()).expect("host");
    let captured = Arc::new(Mutex::new(Vec::<u8>::new()));
    let writer = captured.clone();
    let subscriber = tracing_subscriber::fmt()
        .with_writer(move || CaptureWriter(writer.clone()))
        .with_max_level(tracing::Level::TRACE)
        .finish();
    let guard = tracing::subscriber::set_default(subscriber);
    let error = unbound
        .complete(OfficeModelRequest::default())
        .await
        .expect_err("no model");
    drop(guard);
    let logged = String::from_utf8_lossy(&captured.lock().expect("log")).into_owned();
    assert!(
        !logged.contains("index_tools_stepback_model_unbound"),
        "no index step-back warning for a run that binds no model: {logged}"
    );
    assert_eq!(error.kind(), OfficeFailure::NoModel);
    // The capture works: a binding that FAILS is what that warning is for.
    let runtime = Arc::new(OfficeRuntime::with_transport(
        ORIGIN,
        Arc::clone(&api) as Arc<dyn Transport>,
    ));
    let failing = ClaimOfficeHost::new(
        runtime,
        "pat",
        "42",
        LazyStepbackModel::new(Box::new(|| {
            Err(crate::transport::model_facade::ModelFacadeError::InvalidConfiguration)
        })),
    )
    .expect("host");
    let guard = tracing::subscriber::set_default(
        tracing_subscriber::fmt()
            .with_writer({
                let writer = captured.clone();
                move || CaptureWriter(writer.clone())
            })
            .finish(),
    );
    let _ = failing.complete(OfficeModelRequest::default()).await;
    drop(guard);
    let logged = String::from_utf8_lossy(&captured.lock().expect("log")).into_owned();
    assert!(
        logged.contains("index_tools_stepback_model_unbound"),
        "{logged}"
    );
}

pub(crate) struct NoPlatform;

#[async_trait]
impl PlatformWriter for NoPlatform {
    async fn list_artifacts(
        &self,
        _: &ArtifactListRequest,
    ) -> Result<ArtifactListOutcome, HostError> {
        Err(HostError::new(HostErrorCode::NotFound, "none"))
    }
    async fn read_artifact(
        &self,
        _: &ArtifactReadRequest,
    ) -> Result<ArtifactReadOutcome, HostError> {
        Err(HostError::new(HostErrorCode::NotFound, "none"))
    }
    async fn write_artifact(
        &self,
        _: &ArtifactWriteRequest,
    ) -> Result<ArtifactWriteOutcome, HostError> {
        Err(HostError::new(HostErrorCode::NotFound, "none"))
    }
    async fn delete_artifact(
        &self,
        _: &ArtifactDeleteRequest,
    ) -> Result<ArtifactDeleteOutcome, HostError> {
        Err(HostError::new(HostErrorCode::NotFound, "none"))
    }
    async fn write_skill(&self, _: &SkillWriteRequest) -> Result<SkillWriteOutcome, HostError> {
        Err(HostError::new(HostErrorCode::NotFound, "none"))
    }
    async fn write_project_context(
        &self,
        _: &ProjectContextWriteRequest,
    ) -> Result<ProjectContextWriteOutcome, HostError> {
        Err(HostError::new(HostErrorCode::NotFound, "none"))
    }
}

/// A `pptx` toolkit materialized from a frozen snapshot, called through the
/// worker's host: a deck is created, edited, filled through the model and
/// read back, with every byte going through the object API fake.
#[tokio::test]
async fn a_pptx_toolkit_round_trips_a_deck_through_the_worker_host() {
    let api = ObjectApi::new();
    let (host, llm) = host(&api, vec![r#"[{"key": "{{who}}", "content": "Ada"}]"#]);
    let authority = ArtifactToolAuthority::new(Arc::new(NoPlatform) as Arc<dyn PlatformWriter>)
        .with_office(host);
    let version = json!({"tools": [{
        "id": 9, "type": "pptx", "toolkit_name": "deck",
        "settings": {"bucket_name": "decks"}
    }]});
    let policy = Arc::new(ToolAdmissionPolicy::new(&[], &BTreeMap::new()).expect("policy"));
    let snapshot = FrozenToolSnapshot::from_version_details(version.as_object().expect("version"))
        .expect("snapshot")
        .apply_policy(policy.as_ref());
    let (toolsets, _) = materialize_configured_toolsets_with_artifact_authority(
        &snapshot,
        &policy,
        &Map::new(),
        Some(&authority),
    )
    .await
    .expect("materialized");
    assert_eq!(toolsets.len(), 1);
    let context = Arc::new(SimpleToolContext::new("office-host"));
    let tools = toolsets[0]
        .tools(Arc::clone(&context) as Arc<dyn ReadonlyContext>)
        .await
        .expect("tools");
    assert_eq!(tools.len(), 21);
    let call = |name: &str, args: Value| {
        let tool = tools
            .iter()
            .find(|t| t.name() == name)
            .expect("tool")
            .clone();
        let context = Arc::clone(&context);
        async move {
            tool.execute(context as Arc<dyn adk_rust::ToolContext>, args)
                .await
                .expect("a result")
        }
    };
    let created = call(
        "create_presentation",
        json!({"file_name": "hello.pptx", "title": "{{who}}"}),
    )
    .await;
    assert!(
        created
            .as_str()
            .expect("sentence")
            .starts_with("Created `hello.pptx` in bucket `decks` ("),
        "{created}"
    );
    let stored = api.get("hello.pptx").expect("stored by the host");
    assert!(stored.starts_with(b"PK"), "a zip, byte for byte");
    let filled = call(
        "fill_template",
        json!({"file_name": "hello.pptx", "output_file_name": "ada.pptx", "content_description": "greet Ada"}),
    )
    .await;
    assert_eq!(filled["status"], "success", "{filled}");
    let md = call("to_markdown", json!({"file_name": "ada.pptx"})).await;
    assert!(md.as_str().expect("markdown").contains("Ada"), "{md}");
    let prompts = llm.1.lock().expect("seen").clone();
    assert_eq!(prompts.len(), 1);
    assert!(prompts[0].0.contains("greet Ada"));
}

#[tokio::test]
async fn a_bucket_is_created_through_the_platform_api_under_the_claims_credential() {
    let api = ObjectApi::new();
    let (host, _) = host(&api, vec![]);
    let created = host
        .create_bucket("reports", Some(14))
        .await
        .expect("created");
    assert_eq!(created.name, "reports");
    assert_eq!(created.retention_days, Some(14));
    let log = api.log();
    assert_eq!(log.len(), 1);
    assert_eq!(log[0].0, "POST");
    assert_eq!(log[0].1, "/api/v2/artifacts/buckets/42");
    assert_eq!(
        log[0].3.as_deref(),
        Some("Bearer pat-token"),
        "the turn's own credential, as for objects"
    );

    // Each real cause is its own kind, and a name that cannot exist is not sent.
    let kind = |e: elitea_agent_runtime::toolkits::OfficeHostError| e.kind();
    for (name, expected) in [
        ("taken", OfficeFailure::AlreadyExists),
        ("locked", OfficeFailure::AccessRefused),
        ("bad-name", OfficeFailure::InvalidRequest),
        ("down", OfficeFailure::Unreachable),
    ] {
        assert_eq!(
            kind(host.create_bucket(name, None).await.expect_err(name)),
            expected,
            "{name}"
        );
    }
    let before = api.log().len();
    for name in ["", "a", "has/slash", &"x".repeat(64)] {
        assert_eq!(
            kind(host.create_bucket(name, None).await.expect_err(name)),
            OfficeFailure::InvalidRequest,
            "{name:?}"
        );
    }
    assert_eq!(api.log().len(), before, "an invalid name sends nothing");
}

#[test]
fn an_object_link_is_root_relative_and_never_the_in_cluster_origin() {
    let api = ObjectApi::new();
    let (host, _) = host(&api, vec![]);
    let link = host
        .object_link("chat-attachments", "conv 1/chart_ab.png")
        .expect("link");
    assert_eq!(
        link,
        "/api/v2/artifacts/artifact/default/42/chat-attachments/conv%201/chart_ab.png"
    );
    assert!(!link.contains("platform.internal") && !link.contains("://"));
    assert!(host.object_link("chat-attachments", "../x.png").is_none());
    assert!(host.object_link("bad/bucket", "x.png").is_none());
}

#[test]
fn with_a_public_base_url_the_link_is_absolute_on_it_and_never_the_edge() {
    let api = ObjectApi::new();
    let runtime = Arc::new(
        OfficeRuntime::with_transport(ORIGIN, Arc::clone(&api) as Arc<dyn Transport>)
            .with_public_base_url(Some("https://elitea.example.com".to_owned())),
    );
    let model = LazyStepbackModel::unbound();
    let host = ClaimOfficeHost::new(runtime, "pat-token", "42", model).expect("host");
    let link = host
        .object_link("chat-attachments", "conv 1/chart_ab.png")
        .expect("link");
    assert_eq!(
        link,
        "https://elitea.example.com/api/v2/artifacts/artifact/default/42/chat-attachments/conv%201/chart_ab.png"
    );
    assert!(!link.contains("platform.internal"));
}

/// Object writes and bucket creation are effects: the host's HTTP client is
/// built without protocol-level retries.
#[test]
fn the_host_client_is_built_without_protocol_retries() {
    let retries_disabled = std::hint::black_box(super::office_host::OFFICE_POLICY.disable_retries);
    assert!(retries_disabled);
    assert!(super::office_host::OfficeRuntime::new(ORIGIN).is_ok());
}
