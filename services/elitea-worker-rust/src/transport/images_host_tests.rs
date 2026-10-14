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
}

impl Gateway {
    fn new() -> Arc<Self> {
        Arc::new(Self {
            seen: Mutex::new(Vec::new()),
            status: Mutex::new(StatusCode::OK),
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
        let reply = if status.is_success() {
            json!({"created": 1, "data": [{"b64_json": base64::engine::general_purpose::STANDARD.encode(PNG)}]})
        } else {
            json!({"error": {"message": "no"}})
        };
        Ok(Response::builder()
            .status(status)
            .version(Version::HTTP_2)
            .header("content-type", "application/json")
            .body(Body::new(Full::new(Bytes::from(reply.to_string()))))
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
        (ImagesError::InvalidResponse("x"), F::Unreachable),
        (ImagesError::TooLarge, F::TooLarge),
        (ImagesError::InvalidFileName, F::InvalidRequest),
    ];
    for (error, expected) in table {
        assert_eq!(images_failure(&error).kind(), expected, "{error:?}");
    }
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

/// A direct toolkit call serves `imagegen` when the claim's Office host is
/// lent (and says why a write tool still does not run), and reports the
/// toolkit unsupported when it is not.
#[tokio::test]
async fn a_direct_call_serves_imagegen_when_the_host_is_lent() {
    use crate::toolkits::{
        DirectToolkitRequest, DirectToolkitRuntime, DirectToolkitRuntimeErrorCode,
        IndexToolSupport, IndexToolsWithheld,
    };
    let rig_api = ObjectApi::new();
    let gateway = Gateway::new();
    let runtime = Arc::new(OfficeRuntime::with_transport(
        ORIGIN,
        Arc::clone(&rig_api) as Arc<dyn Transport>,
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
    let authority = ArtifactToolAuthority::new(Arc::new(
        crate::transport::platform_writer::HostOnlyPlatformWriter,
    ) as Arc<dyn PlatformWriter>)
    .with_office(Arc::new(host));
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

    // Lent the host, the toolkit is served; its tools write artifacts, and
    // `toolkit.call_tool.v1` runs read-only tools only, so the refusal now
    // names THAT cause instead of an unsupported toolkit.
    let effectful = DirectToolkitRuntime::default()
        .execute_test(&request, "e", "c", withheld(), Some(&authority))
        .await
        .expect_err("an effectful tool on the read-only path");
    assert_eq!(
        effectful.code(),
        DirectToolkitRuntimeErrorCode::EffectfulToolUnavailable
    );
    assert!(
        gateway.seen.lock().expect("seen").is_empty(),
        "no billed call"
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
