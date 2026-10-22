//! The Office host over a fake of elitea-main's `ArtifactObjectService`: the
//! exact calls it makes, the failures it classifies, and a real `pptx` tool
//! round trip of a deck's bytes through it (ADR-0032 P3, #1359 U5).

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
    TextExtractionFailure, materialize_configured_toolsets_with_artifact_authority,
};
use elitea_platform_rpc::artifacts::{ArtifactError, Refusal};
use elitea_platform_rpc::testing::{ArtifactCall, FakeArtifactStore, serve};
use serde_json::{Map, Value, json};

use super::office_host::{ClaimOfficeHost, OfficeRuntime};
use crate::indexing::tools::{LazyStepbackModel, StepbackBinding};
use crate::toolkits::{FrozenToolSnapshot, ToolAdmissionPolicy};

/// A fake of elitea-main's artifact store, served in process: objects are
/// keyed by name whatever the bucket, the claim's tokens act in one project
/// (42 unless a test names another), and some names answer a fixed refusal
/// (the classification table).
pub(crate) struct ObjectApi {
    project_id: i64,
    store: FakeArtifactStore,
    channel: tonic::transport::Channel,
    /// The runtime serving the fake when the test itself has none.
    _runtime: Option<tokio::runtime::Runtime>,
}

impl ObjectApi {
    pub(crate) fn new() -> Arc<Self> {
        Self::for_project(&["pat-token", "pat"], 42)
    }

    /// A store whose objects live in `project_id`, where `tokens` may act.
    pub(crate) fn for_project(tokens: &[&str], project_id: i64) -> Arc<Self> {
        let store = tokens
            .iter()
            .fold(FakeArtifactStore::new().flat(), |store, token| {
                store.grant(token, project_id)
            })
            .refuse("typed", 415, "UnsupportedMediaType", "not allowed")
            .refuse("invalid", 422, "Unprocessable", "invalid")
            .refuse("huge", 413, "TooLarge", "too large")
            .refuse("broken", 500, "Internal", "broken")
            .refuse("locked", 403, "Forbidden", "locked")
            .refuse("taken", 409, "AlreadyExists", "taken")
            .refuse("bad-name", 400, "BadRequest", "bad name")
            .refuse("down", 500, "Internal", "down");
        let (channel, runtime) = if tokio::runtime::Handle::try_current().is_ok() {
            (serve(Some(store.clone()), None), None)
        } else {
            let runtime = tokio::runtime::Builder::new_multi_thread()
                .worker_threads(1)
                .enable_all()
                .build()
                .expect("runtime");
            let channel = {
                let _entered = runtime.enter();
                serve(Some(store.clone()), None)
            };
            (channel, Some(runtime))
        };
        Arc::new(Self {
            project_id,
            store,
            channel,
            _runtime: runtime,
        })
    }

    /// An Office runtime over this store.
    pub(crate) fn runtime(&self) -> OfficeRuntime {
        OfficeRuntime::new(self.channel.clone())
    }

    pub(crate) fn put(&self, key: &str, bytes: impl AsRef<[u8]>) {
        self.store.put(self.project_id, "", key, bytes.as_ref(), "");
    }

    pub(crate) fn get(&self, key: &str) -> Option<Vec<u8>> {
        self.store
            .object(self.project_id, "", key)
            .map(|object| object.bytes)
    }

    fn calls(&self) -> Vec<ArtifactCall> {
        self.store.calls()
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
    let runtime = Arc::new(api.runtime());
    let llm = Arc::new(Scripted(
        Mutex::new(replies.into_iter().map(str::to_owned).collect()),
        Mutex::new(Vec::new()),
    ));
    let model = LazyStepbackModel::bound(StepbackBinding {
        model: Arc::clone(&llm) as Arc<dyn Llm>,
        model_name: "gpt-test".to_owned(),
        config: adk_rust::GenerateContentConfig::default(),
        budget: None,
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

    let calls = api.calls();
    let put = calls
        .iter()
        .find(|c| c.op == "PutObject")
        .expect("an upload");
    assert_eq!((put.project_id, put.bucket.as_str()), (42, "decks"));
    assert!(put.overwrite, "replacing is the tool's contract");
    for call in &calls {
        assert_eq!(call.bearer.as_deref(), Some("pat-token"), "{call:?}");
    }
    // The object's info arrives before its bytes: one call reads it.
    let ops: Vec<&str> = calls.iter().map(|c| c.op).collect();
    assert_eq!(ops, ["PutObject", "GetObject"]);
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
    let before = api.calls().len();
    assert_eq!(
        kind(
            host.read_object("decks", "big.pptx", 100)
                .await
                .expect_err("too big")
        ),
        OfficeFailure::TooLarge
    );
    let after: Vec<&str> = api.calls()[before..].iter().map(|c| c.op).collect();
    assert_eq!(after, ["GetObject"], "refused from the declared size");
    // Names the platform's key rules refuse are never sent.
    let before = api.calls().len();
    for name in ["a\r\nb.pptx", "a\\b.pptx", "../x.pptx", "/abs.pptx", ""] {
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
    assert_eq!(api.calls().len(), before, "an invalid name sends nothing");
    // A quote is an ordinary character over gRPC: no header to break out of.
    host.write_object("decks", "a\"b.pptx", any, vec![1], true)
        .await
        .expect("a quoted name");
    assert_eq!(api.get("a\"b.pptx"), Some(vec![1]));
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
    // The caller's overwrite flag travels, and a 409 is "already exists".
    let before = api.calls().len();
    let taken = host
        .write_object("decks", "taken.pptx", any, vec![2], false)
        .await;
    assert_eq!(kind(taken.expect_err("409")), OfficeFailure::AlreadyExists);
    assert_eq!(api.get("taken.pptx"), Some(vec![1]), "nothing was replaced");
    assert!(!api.calls()[before].overwrite, "no overwrite unless asked");
    host.write_object("decks", "taken.pptx", any, vec![3], true)
        .await
        .expect("replaced when asked");
    assert_eq!(api.get("taken.pptx"), Some(vec![3]));
    assert!(api.calls().last().expect("calls").overwrite);
    // Causes that are not a route's refusal.
    let refused = |http_status| {
        super::office_host::failure(&ArtifactError::Refused(Refusal {
            http_status,
            code: String::new(),
            message: String::new(),
        }))
        .kind()
    };
    assert_eq!(refused(401), OfficeFailure::AccessRefused, "no credential");
    assert_eq!(refused(503), OfficeFailure::Unreachable);
    let hop = super::office_host::failure(&ArtifactError::Hop(tonic::Status::unavailable("reset")));
    assert_eq!(
        hop.kind(),
        OfficeFailure::Unreachable,
        "the hop, not a refusal"
    );
    let unadmitted = super::office_host::failure(&ArtifactError::Hop(
        tonic::Status::permission_denied("peer not admitted"),
    ));
    assert_eq!(
        unadmitted.kind(),
        OfficeFailure::Unreachable,
        "a refused peer is a deployment fault, not the caller's access"
    );
    let protocol = super::office_host::failure(&ArtifactError::Protocol("no info frame"));
    assert_eq!(protocol.kind(), OfficeFailure::Unreachable);
}

/// `model_failure` classifies on the error CODE, never its message: the
/// claim's toolkit cap and the gateway's budgets are budget refusals (the
/// cap with its own sentence), a reasoning-only reply is unusable, and a
/// message that merely mentions a budget is not a budget refusal.
#[test]
fn model_failures_are_classified_on_their_code() {
    use super::model_text::toolkit_budget_exhausted;
    use crate::indexing::tools::reply_codes;
    let error = |code: &'static str, message: &str| {
        adk_rust::AdkError::new(
            adk_rust::ErrorComponent::Model,
            adk_rust::ErrorCategory::Unavailable,
            code,
            message.to_owned(),
        )
    };
    let cap = super::office_host::model_failure(&toolkit_budget_exhausted());
    assert_eq!(cap.kind(), OfficeFailure::BudgetRefused);
    assert!(cap.is_toolkit_budget());
    for code in [
        "model_gateway.budget_exhausted",
        "model_gateway.member_budget_exhausted",
        "model_gateway.project_budget_exhausted",
    ] {
        let refused = super::office_host::model_failure(&error(code, "x"));
        assert_eq!(refused.kind(), OfficeFailure::BudgetRefused, "{code}");
        assert!(!refused.is_toolkit_budget(), "{code}");
    }
    // The message is not read.
    assert_eq!(
        super::office_host::model_failure(&error(
            "model_gateway.transport",
            "the project model budget is exhausted"
        ))
        .kind(),
        OfficeFailure::Unreachable
    );
    assert_eq!(
        super::office_host::model_failure(&error(reply_codes::REASONING_ONLY, "x")).kind(),
        OfficeFailure::ReplyCutOff
    );
    assert_eq!(
        super::office_host::model_failure(&error("model_gateway.rate_limited", "x")).kind(),
        OfficeFailure::RateLimited
    );
}

/// #1292 rounds 2-3: the categories and codes the binder and the facades
/// actually produce: a request refused as invalid is `InvalidRequest`, not
/// `Unreachable` (D3); a reply that came back over a bound is
/// `ReplyTooLarge`. (Bind failures are classified from the surfaced
/// `ModelFacadeError`, `a_bind_failure_names_whether_...`.)
#[test]
fn bind_failures_invalid_requests_and_oversized_replies_have_their_own_kind() {
    use adk_rust::ErrorCategory as C;
    let error = |category, code: &'static str| {
        adk_rust::AdkError::new(adk_rust::ErrorComponent::Model, category, code, "x")
    };
    for (category, code, kind) in [
        (
            C::InvalidInput,
            "model_gateway.invalid_request",
            OfficeFailure::InvalidRequest,
        ),
        (
            C::InvalidInput,
            "model_gateway.rejected",
            OfficeFailure::InvalidRequest,
        ),
        (
            C::Unavailable,
            "model_gateway.stream_too_large",
            OfficeFailure::ReplyTooLarge,
        ),
        (
            C::Unavailable,
            "model_gateway.event_too_large",
            OfficeFailure::ReplyTooLarge,
        ),
        (
            C::Unavailable,
            "model_gateway.unavailable",
            OfficeFailure::Unreachable,
        ),
    ] {
        assert_eq!(
            super::office_host::model_failure(&error(category, code)).kind(),
            kind,
            "{code}"
        );
    }
}

/// A run WITH a chat model whose binding fails is told the model is unusable
/// for the tool; only a run with no model at all is `NoModel`.
#[tokio::test]
async fn an_unbindable_model_is_unusable_not_absent() {
    let runtime = Arc::new(ObjectApi::new().runtime());
    let failing = LazyStepbackModel::new(Box::new(|| {
        Err(crate::transport::model_facade::ModelFacadeError::InvalidConfiguration)
    }));
    let host = ClaimOfficeHost::new(Arc::clone(&runtime), "pat", "42", failing).expect("host");
    assert_eq!(
        host.complete(OfficeModelRequest::default())
            .await
            .expect_err("unbindable")
            .kind(),
        OfficeFailure::ModelUnusable
    );
    let none =
        ClaimOfficeHost::new(runtime, "pat", "42", LazyStepbackModel::unbound()).expect("host");
    assert_eq!(
        none.complete(OfficeModelRequest::default())
            .await
            .expect_err("no model")
            .kind(),
        OfficeFailure::NoModel
    );
}

/// The Office tools see the claim's spent toolkit cap as a budget refusal
/// with the cap's sentence, through the real `complete` path; an empty reply
/// is an answer (`Ok("")`), never "unreachable".
#[tokio::test]
async fn the_office_host_reports_the_toolkit_cap_and_accepts_an_empty_reply() {
    let api = ObjectApi::new();
    let runtime = Arc::new(api.runtime());
    let llm = Arc::new(Scripted(
        Mutex::new(vec![String::new()]),
        Mutex::new(Vec::new()),
    ));
    let budget = super::model_text::ModelCallBudget::new(1);
    let model = LazyStepbackModel::bound(StepbackBinding {
        model: Arc::clone(&llm) as Arc<dyn Llm>,
        model_name: "gpt-test".to_owned(),
        config: adk_rust::GenerateContentConfig::default(),
        budget: Some(budget),
    });
    let host = ClaimOfficeHost::new(runtime, "pat-token", "42", model).expect("host");
    let request = || OfficeModelRequest {
        prompt: "summarise".to_owned(),
        images_png: std::sync::Arc::from(Vec::<Vec<u8>>::new()),
        image_media_types: std::sync::Arc::from(Vec::<String>::new()),
    };
    assert_eq!(
        host.complete(request()).await.expect("empty is an answer"),
        ""
    );
    let spent = host.complete(request()).await.expect_err("budget spent");
    assert_eq!(spent.kind(), OfficeFailure::BudgetRefused);
    assert!(spent.is_toolkit_budget());
    assert_eq!(
        llm.1.lock().expect("seen").len(),
        1,
        "the refused call sent nothing"
    );
    // What a tool tells the model for it.
    let error = elitea_agent_runtime::toolkits::OfficeHostError::toolkit_budget();
    assert!(
        elitea_agent_runtime::toolkits::TOOLKIT_BUDGET_SENTENCE
            .contains("worker.runtime.toolkitModelCallCap")
    );
    assert_eq!(
        error.reason_code(),
        elitea_agent_runtime::toolkits::TOOLKIT_BUDGET_REASON
    );
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
    let runtime = Arc::new(api.runtime());
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
    let runtime = Arc::new(api.runtime());
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
    let calls = api.calls();
    assert_eq!(calls.len(), 1);
    assert_eq!(calls[0].op, "CreateBucket");
    assert_eq!(
        (calls[0].project_id, calls[0].bucket.as_str()),
        (42, "reports")
    );
    assert_eq!(
        calls[0].bearer.as_deref(),
        Some("pat-token"),
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
    let before = api.calls().len();
    for name in ["", "a", "has/slash", &"x".repeat(64)] {
        assert_eq!(
            kind(host.create_bucket(name, None).await.expect_err(name)),
            OfficeFailure::InvalidRequest,
            "{name:?}"
        );
    }
    assert_eq!(api.calls().len(), before, "an invalid name sends nothing");
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
        api.runtime()
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

/// The document reader the `SharePoint` text tools ask: plain text and code are
/// read here, a PDF or Office file needs the extraction sidecar, and what is
/// neither is named, never guessed at.
#[tokio::test]
async fn the_host_reads_text_itself_and_names_what_it_cannot_read() {
    let runtime = Arc::new(ObjectApi::new().runtime());
    let host =
        ClaimOfficeHost::new(runtime, "pat", "42", LazyStepbackModel::unbound()).expect("host");
    for (name, bytes, expected) in [
        ("notes.txt", "hello\nthere\n".as_bytes(), "hello\nthere\n"),
        ("data.json", br#"{"a":1}"#, r#"{"a":1}"#),
        ("src/main.rs", b"fn main() {}\n", "fn main() {}\n"),
        // Not UTF-8, and not binary: read with the legacy encoding it implies.
        ("legacy.txt", b"caf\xe9 cr\xe8me", "caf\u{e9} cr\u{e8}me"),
    ] {
        assert_eq!(
            host.extract_text(name, bytes.to_vec()).await.as_deref(),
            Ok(expected),
            "{name}"
        );
    }
    let page = host
        .extract_text("page.html", b"<h1>Title</h1><p>Body</p>".to_vec())
        .await
        .expect("html");
    assert!(
        page.contains("Title") && page.contains("Body") && !page.contains("<h1>"),
        "{page}"
    );

    for name in [
        "archive.zzz",
        "Makefile",
        "photo.png",
        "scan.JPG",
        "clip.mp4",
    ] {
        assert_eq!(
            host.extract_text(name, b"x".to_vec()).await,
            Err(TextExtractionFailure::Unsupported),
            "{name}"
        );
    }
    // A text name over bytes that are not text.
    assert_eq!(
        host.extract_text("binary.txt", vec![0, 159, 146, 150, 0, 0, 1, 2])
            .await,
        Err(TextExtractionFailure::Unreadable)
    );
    // A PDF or Office document with no sidecar configured.
    for name in ["a.pdf", "b.docx", "c.xlsx", "d.pptx"] {
        assert_eq!(
            host.extract_text(name, b"%PDF-1.4".to_vec()).await,
            Err(TextExtractionFailure::NoExtractor),
            "{name}"
        );
    }
}

#[test]
fn every_sidecar_skip_is_one_extraction_failure() {
    use super::extractor_client::SkipReason as Skip;
    use super::office_host::text_failure;
    for (reason, expected) in [
        (Skip::TooLarge, TextExtractionFailure::TooLarge),
        (Skip::TooManyPages, TextExtractionFailure::TooLarge),
        (Skip::OutputTooLarge, TextExtractionFailure::TooLarge),
        (Skip::Encrypted, TextExtractionFailure::Unreadable),
        (Skip::Corrupt, TextExtractionFailure::Unreadable),
        (Skip::Empty, TextExtractionFailure::Unreadable),
        (Skip::Unsupported, TextExtractionFailure::Unsupported),
        (Skip::Timeout, TextExtractionFailure::TimedOut),
    ] {
        assert_eq!(text_failure(reason), expected, "{reason:?}");
    }
}

/// A file is refused from its name alone, before anything is downloaded.
#[test]
fn a_file_is_refused_by_its_name_before_it_is_downloaded() {
    use super::office_host::readable_kind;
    for name in ["notes.txt", "a.md", "data.json", "src/main.rs", "page.html"] {
        assert_eq!(readable_kind(false, name), Ok(()), "{name}");
    }
    for name in ["a.pdf", "b.docx", "c.xlsx", "d.pptx"] {
        assert_eq!(readable_kind(true, name), Ok(()), "{name}");
        assert_eq!(
            readable_kind(false, name),
            Err(TextExtractionFailure::NoExtractor),
            "{name}"
        );
    }
    for name in [
        "photo.png",
        "scan.JPG",
        "archive.zzz",
        "Makefile",
        "clip.mp4",
    ] {
        assert_eq!(
            readable_kind(true, name),
            Err(TextExtractionFailure::Unsupported),
            "{name}"
        );
    }
    let runtime = Arc::new(ObjectApi::new().runtime());
    let host =
        ClaimOfficeHost::new(runtime, "pat", "42", LazyStepbackModel::unbound()).expect("host");
    assert_eq!(host.can_read_text("a.txt"), Ok(()));
    assert_eq!(
        host.can_read_text("a.pdf"),
        Err(TextExtractionFailure::NoExtractor)
    );
}

/// D2: a synchronous tool run (the UI's "test tool" panel) of each Office
/// family. The read tool reaches the tool through the lent claim host (here a
/// fake store without the file, so the tool answers its own "not found"
/// sentence); the write tool is refused with the typed read-only reason, never
/// an unsupported family; and the tools are listed whether or not a host does
/// any work (the do-nothing host a discovery is lent).
#[tokio::test]
async fn a_direct_run_serves_each_office_family_reads_and_refuses_its_writes() {
    use crate::toolkits::{
        DirectToolkitRequest, DirectToolkitRuntime, DirectToolkitRuntimeErrorCode,
        IndexToolSupport, IndexToolsWithheld,
    };
    use crate::transport::office_host::NoRoundTripImagesHost;
    use crate::transport::platform_writer::HostOnlyPlatformWriter;
    use elitea_agent_runtime::host::PlatformWriter;
    use elitea_agent_runtime::toolkits::ArtifactToolAuthority;

    let withheld = || IndexToolSupport::Withheld(IndexToolsWithheld::NotServedOnThisPath);
    let api = ObjectApi::new();
    let (claim_host, _) = host(&api, vec![]);
    let claimed =
        ArtifactToolAuthority::new(Arc::new(HostOnlyPlatformWriter) as Arc<dyn PlatformWriter>)
            .with_office(claim_host);
    let idle =
        ArtifactToolAuthority::new(Arc::new(HostOnlyPlatformWriter) as Arc<dyn PlatformWriter>)
            .with_office(Arc::new(NoRoundTripImagesHost));
    let cases = [
        ("pptx", "list_placeholders", "add_slide", "x.pptx"),
        ("docx", "read_document", "add_heading", "x.docx"),
        ("xlsx", "list_sheets", "write_cells", "x.xlsx"),
    ];
    for (family, read, write, file) in cases {
        let config = serde_json::to_vec(&json!({
            "id": 5, "type": family, "toolkit_name": "office",
            "settings": {"bucket_name": "decks", "selected_tools": []}
        }))
        .expect("config");
        let call = |tool: &str| {
            DirectToolkitRequest::parse_call(
                family,
                "5",
                tool,
                &config,
                json!({"file_name": file}).to_string().as_bytes(),
                br#"{"toolkit_security":{}}"#,
            )
            .expect("request")
        };
        let runtime = DirectToolkitRuntime::default();
        // No host lent: the family is unsupported on the path.
        assert_eq!(
            runtime
                .execute_test(&call(read), "e", "c", withheld(), None)
                .await
                .expect_err("no host")
                .code(),
            DirectToolkitRuntimeErrorCode::UnsupportedToolkit,
            "{family}"
        );
        // The read tool runs.
        runtime
            .execute_test(&call(read), "e", "c", withheld(), Some(&claimed))
            .await
            .unwrap_or_else(|error| panic!("{family} {read}: {error}"));
        // The write tool is refused as an effect, with the typed reason.
        assert_eq!(
            runtime
                .execute_test(&call(write), "e", "c", withheld(), Some(&claimed))
                .await
                .expect_err("a write on the read-only path")
                .code(),
            DirectToolkitRuntimeErrorCode::EffectfulToolUnavailable,
            "{family} {write}"
        );
        // Tools are listed on the do-nothing host.
        let settings = serde_json::to_vec(&json!({"bucket_name": "decks", "selected_tools": []}))
            .expect("settings");
        let discovery = DirectToolkitRequest::parse_discovery(
            family,
            "5",
            &settings,
            br#"{"toolkit_security":{}}"#,
        )
        .expect("discovery");
        let listed = runtime
            .discover(&discovery, Some(&idle))
            .await
            .unwrap_or_else(|error| panic!("{family} discovery: {error}"));
        let names: Vec<&str> = listed["tools"]
            .as_array()
            .expect("tools")
            .iter()
            .map(|tool| tool["name"].as_str().expect("name"))
            .collect();
        assert!(
            names.contains(&read) && names.contains(&write),
            "{family}: {names:?}"
        );
    }
}

/// D2: the artifact toolkit's tools are listed on a host-only authority (its
/// discovery was a 500 for want of any authority).
#[tokio::test]
async fn artifact_tools_are_listed_without_a_claim() {
    use crate::toolkits::{DirectToolkitRequest, DirectToolkitRuntime};
    use crate::transport::platform_writer::HostOnlyPlatformWriter;
    use elitea_agent_runtime::host::PlatformWriter;
    use elitea_agent_runtime::toolkits::ArtifactToolAuthority;
    let authority =
        ArtifactToolAuthority::new(Arc::new(HostOnlyPlatformWriter) as Arc<dyn PlatformWriter>);
    let settings =
        serde_json::to_vec(&json!({"bucket": "files", "selected_tools": []})).expect("settings");
    let discovery = DirectToolkitRequest::parse_discovery(
        "artifact",
        "6",
        &settings,
        br#"{"toolkit_security":{}}"#,
    )
    .expect("discovery");
    let runtime = DirectToolkitRuntime::default();
    assert!(runtime.discover(&discovery, None).await.is_err());
    let listed = runtime
        .discover(&discovery, Some(&authority))
        .await
        .expect("listed");
    let names: Vec<&str> = listed["tools"]
        .as_array()
        .expect("tools")
        .iter()
        .map(|tool| tool["name"].as_str().expect("name"))
        .collect();
    assert!(
        names.contains(&"list_files") && names.contains(&"read_file"),
        "{names:?}"
    );
}

/// A provider that streams `text` and ends the reply with `finish`.
struct Finishing(&'static str, adk_rust::FinishReason);

#[async_trait]
impl Llm for Finishing {
    fn name(&self) -> &'static str {
        "finishing"
    }

    async fn generate_content(
        &self,
        _: LlmRequest,
        _: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        let frames = vec![
            Ok(LlmResponse {
                content: Some(Content::new("model").with_text(self.0)),
                partial: true,
                ..Default::default()
            }),
            Ok(LlmResponse {
                finish_reason: Some(self.1),
                turn_complete: true,
                ..Default::default()
            }),
        ];
        Ok(Box::pin(stream::iter(frames)))
    }
}

fn finishing_host(text: &'static str, finish: adk_rust::FinishReason) -> ClaimOfficeHost {
    let runtime = Arc::new(ObjectApi::new().runtime());
    let model = LazyStepbackModel::bound(StepbackBinding {
        model: Arc::new(Finishing(text, finish)) as Arc<dyn Llm>,
        model_name: "finishing".to_owned(),
        config: adk_rust::GenerateContentConfig::default(),
        budget: None,
    });
    ClaimOfficeHost::new(runtime, "pat", "42", model).expect("host")
}

/// #1292 round 4 (B), through the real `model_text` path (not synthetic
/// codes): a reply cut at `max_tokens` and one that spent the limit
/// reasoning are `ReplyCutOff` ("raise `max_tokens`"); a safety stop is
/// `ReplyDeclined`; a reasoning-only reply that closed is not an answer.
#[tokio::test]
async fn reply_causes_reach_the_office_host_as_their_own_kinds() {
    use adk_rust::FinishReason as F;
    for (text, finish, kind) in [
        ("half an answ", F::MaxTokens, OfficeFailure::ReplyCutOff),
        (
            "<think>still reasoning",
            F::MaxTokens,
            OfficeFailure::ReplyCutOff,
        ),
        (
            "<think>all of it</think>",
            F::Stop,
            OfficeFailure::ReplyCutOff,
        ),
        ("", F::Safety, OfficeFailure::ReplyDeclined),
        ("", F::Recitation, OfficeFailure::ReplyDeclined),
    ] {
        let error = finishing_host(text, finish)
            .complete(OfficeModelRequest::default())
            .await
            .expect_err("unusable");
        assert_eq!(error.kind(), kind, "{text:?} {finish:?}");
    }
    // A finished answer is the answer.
    assert_eq!(
        finishing_host("Fine.", F::Stop)
            .complete(OfficeModelRequest::default())
            .await
            .expect("answer"),
        "Fine."
    );
}

/// #1292 round 4 (C): the bind error is surfaced and classified. A claim-side
/// failure (empty credential, bad execution id, project 0) tells the model to
/// retry the turn; a model-side one is `ModelUnusable`; no model is
/// `NoModel`.
#[tokio::test]
async fn a_bind_failure_names_whether_the_claim_or_the_model_is_at_fault() {
    use crate::transport::model_facade::ModelFacadeError as E;
    let runtime = || Arc::new(ObjectApi::new().runtime());
    for (error, kind) in [
        (E::InvalidClaimContext, OfficeFailure::ContextUnusable),
        (E::InvalidConfiguration, OfficeFailure::ModelUnusable),
        (E::InvalidInvocation, OfficeFailure::ModelUnusable),
        (E::DependencyUnavailable, OfficeFailure::Unreachable),
    ] {
        let model = LazyStepbackModel::new(Box::new(move || Err(error)));
        let host = ClaimOfficeHost::new(runtime(), "pat", "42", model).expect("host");
        assert_eq!(
            host.complete(OfficeModelRequest::default())
                .await
                .expect_err("unbound")
                .kind(),
            kind,
            "{error:?}"
        );
    }
    let none =
        ClaimOfficeHost::new(runtime(), "pat", "42", LazyStepbackModel::unbound()).expect("host");
    assert_eq!(
        none.complete(OfficeModelRequest::default())
            .await
            .expect_err("no model")
            .kind(),
        OfficeFailure::NoModel
    );
    // A real facade: a claim with no credential is the claim's fault.
    let (client, _) = super::openai_compatible_facade::test_model_gateway_client(
        Vec::new(),
        super::openai_compatible_facade::test_model_gateway_config(),
    )
    .expect("client");
    assert!(matches!(
        client.bind_ordinary(
            &super::runtime_context::ClaimScopedEliteaContext::fixture(17, ""),
            17,
            super::openai_compatible_facade::test_model_facade_invocation(),
        ),
        Err(E::InvalidClaimContext)
    ));
}

/// #1292 round 4 (C): the native Anthropic codes map as the `OpenAI` ones do.
#[test]
fn anthropic_codes_map_like_the_openai_ones() {
    use adk_rust::ErrorCategory as C;
    let error = |code: &'static str, category| {
        adk_rust::AdkError::new(adk_rust::ErrorComponent::Model, category, code, "x")
    };
    for (code, category, kind) in [
        (
            "anthropic_gateway.response_header_timeout",
            C::Timeout,
            OfficeFailure::Timeout,
        ),
        (
            "model_gateway.response_header_timeout",
            C::Timeout,
            OfficeFailure::Timeout,
        ),
        (
            "anthropic_gateway.transport",
            C::Unavailable,
            OfficeFailure::Unreachable,
        ),
        (
            "model_gateway.transport",
            C::Unavailable,
            OfficeFailure::Unreachable,
        ),
        (
            "anthropic_gateway.sampling_unsupported",
            C::InvalidInput,
            OfficeFailure::ModelUnusable,
        ),
    ] {
        assert_eq!(
            super::office_host::model_failure(&error(code, category)).kind(),
            kind,
            "{code}"
        );
    }
}

const OWN_CONVERSATION: &str = "5f5a1ad4-2b30-4a54-9b7f-2d05a0d3f6c1";
const OTHER_CONVERSATION: &str = "0f5a1ad4-2b30-4a54-9b7f-2d05a0d3f6c1";

/// Review round 2 (defence in depth for Main's rule): the Office host names
/// its claim on every object request, and in a system bucket it reaches only
/// the attachment bucket under the turn's own conversation, for read and
/// write. A prompt-injected path to another conversation, or to the index
/// result bucket, is refused before any request is sent.
#[tokio::test]
async fn the_office_host_names_its_claim_and_keeps_to_its_conversation() {
    use super::office_host::OfficeClaim;
    let api = ObjectApi::new();
    let runtime = Arc::new(api.runtime());
    let host = ClaimOfficeHost::new(runtime, "pat-token", "42", LazyStepbackModel::unbound())
        .expect("host")
        .with_claim(OfficeClaim::new(
            &crate::protocol::control::test_runtime_context_authority(),
            Some(OWN_CONVERSATION.to_owned()),
        ));
    let own = format!("{OWN_CONVERSATION}/report.xlsx");
    host.write_object(
        "chat-attachments",
        &own,
        "text/plain",
        b"data".to_vec(),
        true,
    )
    .await
    .expect("own-conversation write");
    assert_eq!(
        host.read_object("chat-attachments", &own, 1 << 20)
            .await
            .expect("own-conversation read"),
        b"data"
    );
    let sent = api.calls();
    assert!(!sent.is_empty());
    assert!(
        sent.iter()
            .all(|call| call.claim_id.as_deref() == Some("claim-1")),
        "{sent:?}"
    );

    let before = api.calls().len();
    let other = format!("{OTHER_CONVERSATION}/x.xlsx");
    for (bucket, key) in [
        ("chat-attachments", other.as_str()),
        ("index-artifacts", own.as_str()),
    ] {
        let read = host
            .read_object(bucket, key, 1 << 20)
            .await
            .expect_err("read refused");
        assert_eq!(read.kind(), OfficeFailure::AccessRefused, "{bucket}/{key}");
        let write = host
            .write_object(bucket, key, "text/plain", b"x".to_vec(), true)
            .await
            .expect_err("write refused");
        assert_eq!(write.kind(), OfficeFailure::AccessRefused, "{bucket}/{key}");
    }
    assert_eq!(
        api.calls().len(),
        before,
        "a refused path reached the platform"
    );
}

/// The project's attachment bucket as Main's policy names it.
struct PolicyBucket(&'static str);

#[async_trait]
impl elitea_agent_runtime::toolkits::AttachmentBucketResolver for PolicyBucket {
    async fn attachment_bucket(
        &self,
    ) -> Result<String, elitea_agent_runtime::toolkits::AttachmentBucketError> {
        Ok(self.0.to_owned())
    }
}

/// Review round 3: an Office toolkit whose `bucket_name` is a provisioned
/// `reports` system bucket reads and writes it in an agent turn and in a
/// direct call; the project's policy-named attachment bucket is confined to
/// the turn's own conversation, like the default one.
#[tokio::test]
async fn a_reports_bucket_works_on_every_path_and_the_policy_bucket_is_confined() {
    use super::office_host::OfficeClaim;
    let api = ObjectApi::new();
    let runtime = Arc::new(api.runtime());
    let authority = crate::protocol::control::test_runtime_context_authority();
    for conversation in [Some(OWN_CONVERSATION.to_owned()), None] {
        let host = ClaimOfficeHost::new(
            Arc::clone(&runtime),
            "pat-token",
            "42",
            LazyStepbackModel::unbound(),
        )
        .expect("host")
        .with_claim(
            OfficeClaim::new(&authority, conversation.clone())
                .with_attachment_bucket(Arc::new(PolicyBucket("team-uploads"))),
        );
        host.write_object(
            "reports",
            "q3/summary.xlsx",
            "text/plain",
            b"r".to_vec(),
            true,
        )
        .await
        .unwrap_or_else(|error| panic!("reports write ({conversation:?}): {error:?}"));
        assert_eq!(
            host.read_object("reports", "q3/summary.xlsx", 1 << 20)
                .await
                .expect("reports read"),
            b"r"
        );
        let other = format!("{OTHER_CONVERSATION}/x.xlsx");
        assert_eq!(
            host.read_object("team-uploads", &other, 1 << 20)
                .await
                .expect_err("another conversation in the policy bucket")
                .kind(),
            OfficeFailure::AccessRefused
        );
    }
}

/// A direct toolkit call names its claim but has no conversation: it reaches
/// no attachment bucket, while every bucket a person made works as before.
/// Review round 3: the confinement is the project's RESOLVED attachment
/// bucket (a policy may name one other than the default) and the index
/// result bucket only; a provisioned `reports`/`tasks` system bucket is the
/// per-bucket access list's.
#[test]
fn the_confined_buckets_are_the_attachment_and_index_buckets_only() {
    use super::office_host::office_object_allowed_for_test as allowed;
    let own = format!("{OWN_CONVERSATION}/x");
    let other = format!("{OTHER_CONVERSATION}/x");
    // Without a conversation (a direct call).
    assert!(allowed("decks", Some("q3/review.pptx"), None, None));
    assert!(!allowed("chat-attachments", Some(&own), None, None));
    assert!(!allowed("index-artifacts", Some("x"), None, None));
    // An agent turn of OWN_CONVERSATION, the default attachment bucket.
    let turn = Some(OWN_CONVERSATION);
    assert!(allowed("chat-attachments", Some(&own), turn, None));
    assert!(!allowed("chat-attachments", Some(&other), turn, None));
    assert!(!allowed(
        "chat-attachments",
        Some(OWN_CONVERSATION),
        turn,
        None
    ));
    assert!(!allowed("index-artifacts", Some(&own), turn, None));
    // A policy-named attachment bucket is confined too, and the default name
    // stays confined whatever the policy says.
    let policy = Some("team-uploads");
    assert!(allowed("team-uploads", Some(&own), turn, policy));
    assert!(!allowed("team-uploads", Some(&other), turn, policy));
    assert!(!allowed("team-uploads", Some(&own), None, policy));
    assert!(!allowed("chat-attachments", Some(&other), turn, policy));
    // A provisioned `reports` system bucket: the access list's, on every path.
    for conversation in [None, turn] {
        assert!(allowed(
            "reports",
            Some("q3/summary.xlsx"),
            conversation,
            policy
        ));
        assert!(allowed("tasks", Some("t.txt"), conversation, policy));
    }
}

/// Review round 2 (C2 regression): a `SharePoint` discovery is lent the
/// do-nothing host, so its readers and upload are listed in the Test Tool
/// picker and a reader-only toolkit discovers instead of answering
/// unsupported.
#[tokio::test]
async fn a_sharepoint_discovery_lists_its_readers() {
    use crate::toolkits::{DirectToolkitRequest, DirectToolkitRuntime};
    use crate::transport::office_host::NoRoundTripImagesHost;
    use crate::transport::platform_writer::HostOnlyPlatformWriter;
    use elitea_agent_runtime::toolkits::ArtifactToolAuthority;
    let idle =
        ArtifactToolAuthority::new(Arc::new(HostOnlyPlatformWriter) as Arc<dyn PlatformWriter>)
            .with_office(Arc::new(NoRoundTripImagesHost));
    for (selected, want) in [
        (
            json!(["get_lists", "read_document", "upload_file"]),
            vec!["get_lists", "read_document", "upload_file"],
        ),
        (json!(["read_document"]), vec!["read_document"]),
    ] {
        // A discovery carries the toolkit's settings alone.
        let config = serde_json::to_vec(&json!({
            "sharepoint_configuration": {
                "client_id": "client-id", "client_secret": "secret",
                "site_url": "https://tenant.sharepoint.com/sites/default",
                "oauth_discovery_endpoint": "https://login.example.test/.well-known/openid-configuration",
                "configuration_uuid": "00000000-0000-4000-8000-000000000123",
                "scopes": ["Sites.Read.All"]
            },
            "site_path": "teams/project",
            "selected_tools": selected
        }))
        .expect("config");
        let request = DirectToolkitRequest::parse_discovery(
            "sharepoint",
            "9",
            &config,
            br#"{"toolkit_security":{}}"#,
        )
        .expect("request");
        let listed = DirectToolkitRuntime::default()
            .discover(&request, Some(&idle))
            .await
            .expect("a SharePoint discovery");
        let mut names: Vec<&str> = listed["tools"]
            .as_array()
            .expect("tools")
            .iter()
            .filter_map(|tool| tool["name"].as_str())
            .collect();
        names.sort_unstable();
        assert_eq!(names, want);
    }
}
