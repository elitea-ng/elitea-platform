//! The `imagegen` toolkit through the worker's host (#864): a toolkit
//! materialized from a frozen snapshot, its images calls sent over the model
//! gateway's channel under the claim's credential, project and execution, and
//! its results written through the platform object API.

use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use adk_rust::tool::SimpleToolContext;
use adk_rust::{ReadonlyContext, ToolContext};
use async_trait::async_trait;
use base64::Engine as _;
use bytes::Bytes;
use elitea_agent_runtime::host::PlatformWriter;
use elitea_agent_runtime::toolkits::{
    ArtifactToolAuthority, OfficeToolHost, materialize_configured_toolsets_with_artifact_authority,
};
use elitea_connectors::transport::Transport;
use http::{Request, Response, StatusCode, Version};
use http_body_util::{BodyExt as _, Full};
use serde_json::{Map, Value, json};
use tonic::body::Body;

use super::images_facade::ImagesClient;
use super::office_host::{ClaimOfficeHost, OfficeRuntime};
use super::office_host_tests::{NoPlatform, ObjectApi};
use super::openai_compatible_facade::{ModelGatewayTransport, ModelGatewayTransportError};
use super::runtime_context::ClaimScopedEliteaContext;
use crate::indexing::tools::LazyStepbackModel;
use crate::toolkits::{FrozenToolSnapshot, ToolAdmissionPolicy};

const ORIGIN: &str = "https://platform.internal:9446";
const PNG: &[u8] = b"\x89PNG\r\n\x1a\nfake";

/// What the gateway saw: route, headers of interest and the body.
#[derive(Clone, Debug)]
struct Seen {
    path: String,
    content_type: String,
    project: String,
    execution: String,
    authorization: String,
    body: Vec<u8>,
}

/// A gateway that answers the images routes with one inline PNG, or with a
/// scripted status.
struct Gateway {
    seen: Mutex<Vec<Seen>>,
    status: Mutex<StatusCode>,
    /// A raw 2xx body to answer with instead of one PNG.
    reply: Mutex<Option<Vec<u8>>>,
}

impl Gateway {
    fn new() -> Arc<Self> {
        Arc::new(Self {
            seen: Mutex::new(Vec::new()),
            status: Mutex::new(StatusCode::OK),
            reply: Mutex::new(None),
        })
    }

    fn refuse_with(&self, status: StatusCode) {
        *self.status.lock().expect("status") = status;
    }
}

#[async_trait]
impl ModelGatewayTransport for Gateway {
    async fn post(
        &self,
        request: Request<Body>,
    ) -> Result<Response<Body>, ModelGatewayTransportError> {
        let (parts, body) = request.into_parts();
        let body = body
            .collect()
            .await
            .map_err(|_| ModelGatewayTransportError::Unavailable)?
            .to_bytes();
        let header = |name: &str| {
            parts
                .headers
                .get(name)
                .and_then(|v| v.to_str().ok())
                .unwrap_or_default()
                .to_owned()
        };
        self.seen.lock().expect("seen").push(Seen {
            path: parts.uri.path().to_owned(),
            content_type: header("content-type"),
            project: header("x-project-id"),
            execution: header("x-elitea-execution-id"),
            authorization: header("authorization"),
            body: body.to_vec(),
        });
        let status = *self.status.lock().expect("status");
        let custom = self.reply.lock().expect("reply").clone();
        let reply = if let Some(custom) = custom {
            String::from_utf8_lossy(&custom).into_owned()
        } else if status.is_success() {
            json!({"created": 1, "data": [{"b64_json": base64::engine::general_purpose::STANDARD.encode(PNG)}]}).to_string()
        } else {
            json!({"error": {"message": "no"}}).to_string()
        };
        Ok(Response::builder()
            .status(status)
            .version(Version::HTTP_2)
            .header("content-type", "application/json")
            .body(Body::new(Full::new(Bytes::from(reply))))
            .expect("response"))
    }
}

struct Rig {
    api: Arc<ObjectApi>,
    gateway: Arc<Gateway>,
    tools: Vec<Arc<dyn adk_rust::Tool>>,
}

impl Rig {
    async fn new() -> Self {
        let api = ObjectApi::new();
        let gateway = Gateway::new();
        let runtime = Arc::new(OfficeRuntime::with_transport(
            ORIGIN,
            Arc::clone(&api) as Arc<dyn Transport>,
        ));
        let context = Arc::new(ClaimScopedEliteaContext::fixture(42, "pat-token"));
        let images = ImagesClient::with_transport(
            Arc::clone(&gateway) as Arc<dyn ModelGatewayTransport>,
            Duration::from_secs(5),
        );
        let host = ClaimOfficeHost::new(runtime, "pat-token", "42", LazyStepbackModel::unbound())
            .expect("host")
            .with_images(images, context);
        assert!(host.serves_images());
        let authority = ArtifactToolAuthority::new(Arc::new(NoPlatform) as Arc<dyn PlatformWriter>)
            .with_office(Arc::new(host));
        let version = json!({"tools": [{
            "id": 9, "type": "imagegen", "toolkit_name": "pics",
            "settings": {"image_generation_model": "E2E-MOCK-IMAGE-MODEL", "bucket": "pics",
                         "name_prefix": "e2e-proof-"}
        }]});
        let policy = Arc::new(ToolAdmissionPolicy::new(&[], &BTreeMap::new()).expect("policy"));
        let snapshot =
            FrozenToolSnapshot::from_version_details(version.as_object().expect("version"))
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
        let context = Arc::new(SimpleToolContext::new("images-host"));
        let tools = toolsets[0]
            .tools(context as Arc<dyn ReadonlyContext>)
            .await
            .expect("tools");
        Self {
            api,
            gateway,
            tools,
        }
    }

    async fn call(&self, name: &str, args: Value) -> Value {
        let tool = self
            .tools
            .iter()
            .find(|t| t.name() == name)
            .expect("tool")
            .clone();
        tool.execute(
            Arc::new(SimpleToolContext::new("images-host")) as Arc<dyn ToolContext>,
            args,
        )
        .await
        .expect("a result")
    }

    fn seen(&self) -> Vec<Seen> {
        self.gateway.seen.lock().expect("seen").clone()
    }
}

#[tokio::test]
async fn generate_image_calls_the_gateway_under_the_claim_and_stores_the_png() {
    let rig = Rig::new().await;
    let out = rig
        .call(
            "generate_image",
            json!({"prompt": "a red circle", "size": "512x512", "n": 1}),
        )
        .await;
    // The path the E2E journey asserts on, byte for byte.
    assert_eq!(
        out,
        json!({"artifacts": [{
            "filepath": "/pics/e2e-proof-generate-0.png",
            "filename": "e2e-proof-generate-0.png"
        }]})
    );
    assert_eq!(
        rig.api.get("e2e-proof-generate-0.png").as_deref(),
        Some(PNG),
        "the decoded image, byte for byte, through the object API"
    );
    let seen = rig.seen();
    assert_eq!(seen.len(), 1);
    assert_eq!(seen[0].path, "/llm/v1/images/generations");
    assert_eq!(seen[0].content_type, "application/json");
    assert_eq!(seen[0].project, "42");
    assert_eq!(seen[0].execution, "0123456789abcdef0123456789abcdef");
    assert_eq!(seen[0].authorization, "Bearer pat-token");
    let body: Value = serde_json::from_slice(&seen[0].body).expect("json");
    assert_eq!(
        body,
        json!({"prompt": "a red circle", "model": "E2E-MOCK-IMAGE-MODEL", "n": 1,
               "response_format": "b64_json", "size": "512x512"})
    );
}

#[tokio::test]
async fn edit_image_sends_the_source_and_mask_as_multipart_fields() {
    let rig = Rig::new().await;
    rig.api.put("cat.png", b"CAT-BYTES".to_vec());
    rig.api.put("cat-mask.png", b"MASK-BYTES".to_vec());
    let out = rig
        .call(
            "edit_image",
            json!({"image": "/pics/cat.png", "prompt": "add a hat", "mask": "/pics/cat-mask.png"}),
        )
        .await;
    assert_eq!(
        out["artifacts"][0]["filepath"],
        "/pics/e2e-proof-edit-0.png"
    );
    assert_eq!(rig.api.get("e2e-proof-edit-0.png").as_deref(), Some(PNG));
    let seen = rig.seen();
    assert_eq!(seen[0].path, "/llm/v1/images/edits");
    assert!(
        seen[0]
            .content_type
            .starts_with("multipart/form-data; boundary="),
        "{}",
        seen[0].content_type
    );
    let body = String::from_utf8_lossy(&seen[0].body).into_owned();
    for needle in [
        "name=\"prompt\"\r\n\r\nadd a hat",
        "name=\"model\"\r\n\r\nE2E-MOCK-IMAGE-MODEL",
        "name=\"response_format\"\r\n\r\nb64_json",
        "name=\"image\"; filename=\"cat.png\"\r\nContent-Type: image/png\r\n\r\nCAT-BYTES",
        "name=\"mask\"; filename=\"cat-mask.png\"\r\nContent-Type: image/png\r\n\r\nMASK-BYTES",
    ] {
        assert!(body.contains(needle), "missing {needle:?} in {body}");
    }
}

#[tokio::test]
async fn the_gateways_refusals_reach_the_model_as_their_own_sentences() {
    let rig = Rig::new().await;
    for (status, expected) in [
        (StatusCode::PAYMENT_REQUIRED, "budget is exhausted"),
        (StatusCode::UNAUTHORIZED, "refused access"),
        (StatusCode::FORBIDDEN, "refused access"),
        (StatusCode::BAD_REQUEST, "refused as invalid"),
        (StatusCode::NOT_FOUND, "refused as invalid"),
        (StatusCode::BAD_GATEWAY, "could not be reached"),
        (
            StatusCode::TOO_MANY_REQUESTS,
            "rate-limited right now. Try again shortly",
        ),
        (StatusCode::REQUEST_TIMEOUT, "timed out"),
        (StatusCode::GATEWAY_TIMEOUT, "timed out"),
        (StatusCode::CONFLICT, "reported a conflict"),
    ] {
        rig.gateway.refuse_with(status);
        let out = rig.call("generate_image", json!({"prompt": "x"})).await;
        let text = out.as_str().expect("sentence");
        assert!(text.contains(expected), "{status}: {text}");
    }
    // Nothing was stored for a refused call.
    assert!(rig.api.get("e2e-proof-generate-0.png").is_none());
}

/// The failure table of the host, cause by cause: a rate limit, a timeout and
/// a conflict are retryable causes of their own, and only a real connection
/// or gateway failure is "unreachable".
#[test]
fn the_images_failure_table_names_each_cause() {
    use super::images_facade::ImagesError;
    use super::office_host::images_failure;
    use elitea_agent_runtime::toolkits::OfficeFailure as F;
    use elitea_llm_wire::refusal::{BudgetScope, Refusal as R};
    let table = [
        (
            ImagesError::Refused {
                refusal: R::BudgetExhausted(BudgetScope::Unscoped),
            },
            F::BudgetRefused,
        ),
        (
            ImagesError::Refused {
                refusal: R::Unauthorized,
            },
            F::AccessRefused,
        ),
        (
            ImagesError::Refused {
                refusal: R::Forbidden,
            },
            F::AccessRefused,
        ),
        (
            ImagesError::Refused {
                refusal: R::Rejected,
            },
            F::InvalidRequest,
        ),
        (
            ImagesError::Refused {
                refusal: R::RateLimited,
            },
            F::RateLimited,
        ),
        (
            ImagesError::Refused {
                refusal: R::UpstreamTimeout,
            },
            F::Timeout,
        ),
        (
            ImagesError::Refused {
                refusal: R::Conflict,
            },
            F::Conflict,
        ),
        (
            ImagesError::Refused {
                refusal: R::Unavailable,
            },
            F::Unreachable,
        ),
        (ImagesError::Transport, F::Unreachable),
        (ImagesError::UnusableReply("x"), F::ReplyUnusable),
        (ImagesError::ReplyTooLarge, F::ReplyTooLarge),
        (ImagesError::RequestTooLarge, F::TooLarge),
        (ImagesError::Malformed, F::InvalidRequest),
        (ImagesError::InvalidFileName, F::InvalidRequest),
    ];
    for (error, expected) in table {
        assert_eq!(images_failure(&error).kind(), expected, "{error:?}");
    }
}

#[tokio::test]
async fn a_2xx_with_an_unusable_or_oversized_reply_may_have_been_billed() {
    let rig = Rig::new().await;
    // Not the images object.
    *rig.gateway.reply.lock().expect("reply") = Some(b"<html>nope</html>".to_vec());
    let out = rig.call("generate_image", json!({"prompt": "x"})).await;
    let text = out.as_str().expect("sentence");
    assert!(
        text.contains("answered but its reply was unusable"),
        "{text}"
    );
    assert!(text.contains("may have been billed"), "{text}");
    assert!(!text.contains("did not happen"), "{text}");
    // More images than the API's maximum in one answer.
    let eleven = vec![json!({"b64_json": "AQID"}); 11];
    *rig.gateway.reply.lock().expect("reply") =
        Some(json!({"data": eleven}).to_string().into_bytes());
    let out = rig.call("generate_image", json!({"prompt": "x"})).await;
    assert!(
        out.as_str()
            .expect("sentence")
            .contains("may have been billed"),
        "{out}"
    );
    assert!(rig.api.get("e2e-proof-generate-0.png").is_none());
}

#[tokio::test]
async fn object_size_is_the_declared_length_of_a_head() {
    let api = ObjectApi::new();
    api.put("cat.png", vec![7_u8; 1234]);
    let runtime = Arc::new(OfficeRuntime::with_transport(
        ORIGIN,
        Arc::clone(&api) as Arc<dyn Transport>,
    ));
    let host =
        ClaimOfficeHost::new(runtime, "pat", "42", LazyStepbackModel::unbound()).expect("host");
    assert_eq!(
        host.object_size("pics", "cat.png").await.expect("size"),
        Some(1234)
    );
    let missing = host
        .object_size("pics", "none.png")
        .await
        .expect_err("a missing object");
    assert_eq!(
        missing.kind(),
        elitea_agent_runtime::toolkits::OfficeFailure::NotFound
    );
}

#[tokio::test]
async fn an_unusable_upload_file_name_is_an_invalid_request_and_nothing_is_sent() {
    let api = ObjectApi::new();
    let gateway = Gateway::new();
    let runtime = Arc::new(OfficeRuntime::with_transport(
        ORIGIN,
        Arc::clone(&api) as Arc<dyn Transport>,
    ));
    let images = ImagesClient::with_transport(
        Arc::clone(&gateway) as Arc<dyn ModelGatewayTransport>,
        Duration::from_secs(5),
    );
    let host = ClaimOfficeHost::new(runtime, "pat", "42", LazyStepbackModel::unbound())
        .expect("host")
        .with_images(
            images,
            Arc::new(ClaimScopedEliteaContext::fixture(42, "pat")),
        );
    let error = host
        .edit_images(elitea_agent_runtime::toolkits::ImageEditRequest {
            model: "m".into(),
            prompt: "p".into(),
            image: elitea_agent_runtime::toolkits::ImageEditInput {
                name: "a\"b\r\n.png".into(),
                bytes: vec![1],
            },
            mask: None,
        })
        .await
        .expect_err("refused");
    assert_eq!(
        error.kind(),
        elitea_agent_runtime::toolkits::OfficeFailure::InvalidRequest
    );
    assert_eq!(error.reason_code(), "office_host.images_file_name");
    assert!(gateway.seen.lock().expect("seen").is_empty());
}

/// A direct toolkit call of `imagegen` lists its tools and refuses to run
/// them, with the read-only rule's reason, on a host that redeems nothing and
/// calls nothing; without any host it is unsupported.
#[tokio::test]
async fn a_direct_call_lists_imagegen_and_refuses_its_write_tools_without_a_round_trip() {
    use crate::toolkits::{
        DirectToolkitRequest, DirectToolkitRuntime, DirectToolkitRuntimeErrorCode,
        IndexToolSupport, IndexToolsWithheld,
    };
    use crate::transport::office_host::NoRoundTripImagesHost;
    let authority = ArtifactToolAuthority::new(Arc::new(
        crate::transport::platform_writer::HostOnlyPlatformWriter,
    ) as Arc<dyn PlatformWriter>)
    .with_office(Arc::new(NoRoundTripImagesHost));
    let config = serde_json::to_vec(&json!({
        "id": 9, "type": "imagegen", "toolkit_name": "pics",
        "settings": {"image_generation_model": "m", "bucket": "pics"}
    }))
    .expect("config");
    let request = DirectToolkitRequest::parse_call(
        "imagegen",
        "9",
        "generate_image",
        &config,
        br#"{"prompt":"a cat"}"#,
        br#"{"toolkit_security":{}}"#,
    )
    .expect("request");
    let withheld = || IndexToolSupport::Withheld(IndexToolsWithheld::NotServedOnThisPath);
    let refused = DirectToolkitRuntime::default()
        .execute_test(&request, "e", "c", withheld(), None)
        .await
        .expect_err("no host lent");
    assert_eq!(
        refused.code(),
        DirectToolkitRuntimeErrorCode::UnsupportedToolkit
    );
    let effectful = DirectToolkitRuntime::default()
        .execute_test(&request, "e", "c", withheld(), Some(&authority))
        .await
        .expect_err("an effectful tool on the read-only path");
    assert_eq!(
        effectful.code(),
        DirectToolkitRuntimeErrorCode::EffectfulToolUnavailable
    );
    let listed = DirectToolkitRuntime::default()
        .discover(&request, Some(&authority))
        .await
        .expect("discovered");
    let names: Vec<&str> = listed["tools"]
        .as_array()
        .expect("tools")
        .iter()
        .map(|t| t["name"].as_str().expect("name"))
        .collect();
    assert_eq!(names, ["generate_image", "edit_image"]);
    assert_eq!(
        DirectToolkitRuntime::default()
            .discover(&request, None)
            .await
            .expect_err("no host lent")
            .code(),
        DirectToolkitRuntimeErrorCode::UnsupportedToolkit
    );
}

#[tokio::test]
async fn a_host_without_the_images_client_does_not_serve_the_toolkit() {
    let api = ObjectApi::new();
    let runtime = Arc::new(OfficeRuntime::with_transport(
        ORIGIN,
        api as Arc<dyn Transport>,
    ));
    let host =
        ClaimOfficeHost::new(runtime, "pat", "42", LazyStepbackModel::unbound()).expect("host");
    assert!(!host.serves_images());
    let refused = host
        .generate_images(elitea_agent_runtime::toolkits::ImageGenerateRequest {
            model: "m".into(),
            prompt: "p".into(),
            size: None,
            count: 1,
        })
        .await
        .expect_err("no images API");
    assert_eq!(
        refused.kind(),
        elitea_agent_runtime::toolkits::OfficeFailure::NoModel
    );
}

/// A provider that answers every completion with "ok" and counts them.
struct CountingModel(std::sync::atomic::AtomicUsize);

#[async_trait]
impl adk_rust::Llm for CountingModel {
    fn name(&self) -> &'static str {
        "fixture-model"
    }

    async fn generate_content(
        &self,
        _: adk_rust::LlmRequest,
        _: bool,
    ) -> adk_rust::Result<adk_rust::LlmResponseStream> {
        self.0.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
        let frames = vec![
            Ok(adk_rust::LlmResponse {
                content: Some(adk_rust::Content::new("model").with_text("ok")),
                partial: true,
                ..Default::default()
            }),
            Ok(adk_rust::LlmResponse {
                finish_reason: Some(adk_rust::FinishReason::Stop),
                turn_complete: true,
                ..Default::default()
            }),
        ];
        Ok(Box::pin(adk_rust::futures::stream::iter(frames)))
    }
}

/// Review round 2, items 3 and 5: the claim's ONE toolkit budget is shared by
/// the step-back model, the Office host's prompts and its image calls (it was
/// one budget per call site, so the cap was 400 and images were uncapped),
/// and the binder binds nothing until a completion is asked for.
#[tokio::test]
async fn stepback_office_and_images_stop_at_the_one_shared_cap() {
    use crate::agents::ordinary::{BindOne, stepback_binder};
    use crate::transport::toolkit_completion::ModelCallBudget;
    use elitea_agent_runtime::toolkits::{ImageGenerateRequest, OfficeModelRequest};
    use std::sync::atomic::{AtomicUsize, Ordering};

    let budget = ModelCallBudget::new(3);
    let provider = Arc::new(CountingModel(AtomicUsize::new(0)));
    let binds = Arc::new(AtomicUsize::new(0));
    let binder = |budget: &ModelCallBudget| {
        let (provider, binds) = (Arc::clone(&provider), Arc::clone(&binds));
        let bind_one: BindOne = Arc::new(move |_| {
            binds.fetch_add(1, Ordering::SeqCst);
            Ok(Arc::clone(&provider) as Arc<dyn adk_rust::Llm>)
        });
        let mut invocation = super::openai_compatible_facade::test_model_facade_invocation();
        invocation.max_model_turns = 1;
        stepback_binder(bind_one, invocation, budget.clone())
    };
    // The index authority's step-back model and the Office host's model are
    // built from the SAME budget, as `materialize_runtime` builds them.
    let stepback = LazyStepbackModel::new(binder(&budget));
    let office_model = LazyStepbackModel::new(binder(&budget));
    assert_eq!(binds.load(Ordering::SeqCst), 0, "no probe bind at assembly");

    let gateway = Gateway::new();
    let runtime = Arc::new(OfficeRuntime::with_transport(
        ORIGIN,
        ObjectApi::new() as Arc<dyn Transport>,
    ));
    let images = ImagesClient::with_transport(
        Arc::clone(&gateway) as Arc<dyn ModelGatewayTransport>,
        Duration::from_secs(5),
    );
    let host = ClaimOfficeHost::new(runtime, "pat-token", "42", office_model)
        .expect("host")
        .with_images(
            images,
            Arc::new(ClaimScopedEliteaContext::fixture(42, "pat-token")),
        )
        .with_budget(budget.clone());

    // 1: a step-back completion.
    let binding = stepback.get().expect("bound lazily");
    crate::indexing::tools::generate_text(
        binding,
        vec![adk_rust::Part::Text {
            text: "step back".to_owned(),
        }],
    )
    .await
    .expect("step-back reply");
    // 2: an Office prompt.
    host.complete(OfficeModelRequest {
        prompt: "fill".to_owned(),
        ..OfficeModelRequest::default()
    })
    .await
    .expect("office reply");
    // 3: an image call.
    let image = || ImageGenerateRequest {
        model: "img".to_owned(),
        prompt: "a red square".to_owned(),
        size: None,
        count: 1,
    };
    host.generate_images(image()).await.expect("image");
    // The cap (3) is reached: each of the three is refused now.
    assert_eq!(
        host.generate_images(image())
            .await
            .expect_err("spent")
            .kind(),
        elitea_agent_runtime::toolkits::OfficeFailure::BudgetRefused
    );
    assert_eq!(
        host.complete(OfficeModelRequest::default())
            .await
            .expect_err("spent")
            .kind(),
        elitea_agent_runtime::toolkits::OfficeFailure::BudgetRefused
    );
    let refused = crate::indexing::tools::generate_text(
        binding,
        vec![adk_rust::Part::Text {
            text: "again".to_owned(),
        }],
    )
    .await
    .expect_err("spent");
    assert_eq!(refused.code, "model.toolkit_budget_exhausted");
    assert_eq!(
        provider.0.load(Ordering::SeqCst),
        2,
        "two model completions sent"
    );
    assert_eq!(
        gateway.seen.lock().expect("seen").len(),
        1,
        "one image call sent"
    );
    // One bind per completion that was asked for (each binds first).
    assert_eq!(binds.load(Ordering::SeqCst), 4);
}

/// An image call refused locally (an upload name that cannot be sent) is not charged.
#[tokio::test]
async fn an_image_call_refused_before_sending_is_refunded() {
    use crate::transport::toolkit_completion::ModelCallBudget;
    use elitea_agent_runtime::toolkits::{ImageEditInput, ImageEditRequest};
    let budget = ModelCallBudget::new(1);
    let gateway = Gateway::new();
    let runtime = Arc::new(OfficeRuntime::with_transport(
        ORIGIN,
        ObjectApi::new() as Arc<dyn Transport>,
    ));
    let images = ImagesClient::with_transport(
        Arc::clone(&gateway) as Arc<dyn ModelGatewayTransport>,
        Duration::from_secs(5),
    );
    let host = ClaimOfficeHost::new(runtime, "pat-token", "42", LazyStepbackModel::unbound())
        .expect("host")
        .with_images(
            images,
            Arc::new(ClaimScopedEliteaContext::fixture(42, "pat-token")),
        )
        .with_budget(budget.clone());
    let bad = ImageEditRequest {
        model: "img".to_owned(),
        prompt: "add a hat".to_owned(),
        image: ImageEditInput {
            name: "bad\"name\r\n.png".to_owned(),
            bytes: b"CAT".to_vec(),
        },
        mask: None,
    };
    assert!(host.edit_images(bad).await.is_err());
    assert!(
        gateway.seen.lock().expect("seen").is_empty(),
        "nothing sent"
    );
    assert!(budget.try_take(), "the unit was refunded");
}
