use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use adk_rust::tool::SimpleToolContext;
use adk_rust::{ReadonlyContext, Tool, Toolset};
use async_trait::async_trait;
use bytes::Bytes;
use http::{Request, Response, StatusCode, Version};
use http_body_util::Full;
use serde_json::{Map, Value, json};
use tonic::body::Body;

use super::families::artifact::ArtifactToolAuthority;
use super::families::artifact::config::{ArtifactConfigErrorCode, ArtifactToolkitConfig};
use super::families::artifact::tools::build_artifact_toolset;
use super::policy::ToolAdmissionPolicy;
use crate::protocol::control::test_runtime_context_authority;
use crate::transport::platform_client::PlatformClient;
use crate::transport::runtime_context::{
    RuntimeContextClient, RuntimeContextConfig, RuntimeContextRpc, RuntimeContextTransportError,
};

/// The SDK's own names, which the stored `selected_tools` of every artifact
/// toolkit created through the product's form carries.
const SDK_TOOL_NAMES: &[&str] = &[
    "list_files",
    "create_file",
    "read_file",
    "read_multiple_files",
    "get_file_metadata",
    "delete_file",
    "append_data",
    "create_new_bucket",
];

struct FixtureRpc {
    body: Mutex<String>,
    paths: Mutex<Vec<String>>,
}

#[async_trait]
impl RuntimeContextRpc for FixtureRpc {
    async fn post(
        &self,
        request: Request<Body>,
    ) -> Result<Response<Body>, RuntimeContextTransportError> {
        self.paths
            .lock()
            .expect("fixture paths")
            .push(request.uri().path().to_owned());
        let raw = self.body.lock().expect("fixture body").clone();
        Ok(Response::builder()
            .status(StatusCode::OK)
            .version(Version::HTTP_2)
            .header("content-type", "application/json")
            .header("cache-control", "private, no-cache, no-store")
            .header("pragma", "no-cache")
            .header("content-length", raw.len())
            .body(Body::new(Full::new(Bytes::copy_from_slice(raw.as_bytes()))))
            .expect("fixture response"))
    }
}

fn authority(body: &str) -> (ArtifactToolAuthority, Arc<FixtureRpc>) {
    let rpc = Arc::new(FixtureRpc {
        body: Mutex::new(body.to_owned()),
        paths: Mutex::new(Vec::new()),
    });
    let client = RuntimeContextClient::with_rpc(
        FixtureRpcHandle(Arc::clone(&rpc)),
        RuntimeContextConfig {
            origin: "https://content.internal:443".to_owned(),
            deadline: Duration::from_secs(1),
            max_response_bytes: 32 * 1_024,
            max_application_response_bytes: 1_024 * 1_024,
            max_attachment_response_bytes: 1_024 * 1_024,
            max_artifact_response_bytes: 2 * 1_024 * 1_024,
        },
    )
    .expect("fixture runtime-context client");
    (
        ArtifactToolAuthority::new(
            Arc::new(PlatformClient::new(Arc::new(client))),
            Arc::new(test_runtime_context_authority()),
        ),
        rpc,
    )
}

struct FixtureRpcHandle(Arc<FixtureRpc>);

#[async_trait]
impl RuntimeContextRpc for FixtureRpcHandle {
    async fn post(
        &self,
        request: Request<Body>,
    ) -> Result<Response<Body>, RuntimeContextTransportError> {
        self.0.post(request).await
    }
}

fn settings(selected_tools: &[&str]) -> Map<String, Value> {
    json!({"bucket": "agent-artifacts", "selected_tools": selected_tools})
        .as_object()
        .expect("artifact settings fixture")
        .clone()
}

fn policy() -> Arc<ToolAdmissionPolicy> {
    Arc::new(ToolAdmissionPolicy::new(&[], &BTreeMap::new()).expect("artifact policy fixture"))
}

fn context() -> Arc<SimpleToolContext> {
    Arc::new(
        SimpleToolContext::new("artifact-tool-test")
            .with_session_id("session-1")
            .with_function_call_id("call-1"),
    )
}

async fn tools_of(body: &str, selected: &[&str]) -> (Vec<Arc<dyn Tool>>, Arc<FixtureRpc>) {
    let (authority, rpc) = authority(body);
    let config = ArtifactToolkitConfig::parse(&settings(selected)).expect("artifact configuration");
    let toolset =
        build_artifact_toolset("files", &config, &policy(), &authority).expect("artifact toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("artifact tools");
    (tools, rpc)
}

/// The stored catalogue is the SDK's and it names tools this runtime does not
/// serve yet. Those names must SELECT NOTHING rather than refuse the whole
/// configuration: refusing would fail materialization for every toolkit the
/// product's own form creates, which is the dead-agent outcome #906 is about.
#[tokio::test]
async fn the_sdk_catalogue_selects_what_this_runtime_serves_and_refuses_nothing() {
    let (tools, _rpc) = tools_of("{}", SDK_TOOL_NAMES).await;
    let mut names = tools.iter().map(|tool| tool.name()).collect::<Vec<_>>();
    names.sort_unstable();
    assert_eq!(
        names,
        ["create_file", "delete_file", "list_files", "read_file"]
    );

    // An empty selection is every tool this runtime implements — the
    // convention every other family follows.
    let (all, _rpc) = tools_of("{}", &[]).await;
    assert_eq!(all.len(), 4);

    // And a selection that names only one binds only that one, so an author
    // who narrowed the toolkit keeps their narrowing.
    let (one, _rpc) = tools_of("{}", &["read_file"]).await;
    assert_eq!(one.len(), 1);
    assert_eq!(one[0].name(), "read_file");
}

/// The argument names are the SDK's, so a prompt written against the Python
/// worker ports unchanged. `filename` in particular is what every ported
/// onetest case scripts.
#[tokio::test]
async fn the_argument_names_are_the_sdks() {
    let (tools, _rpc) = tools_of("{}", &[]).await;
    for tool in &tools {
        let schema = tool
            .parameters_schema()
            .expect("every artifact tool has an argument schema");
        let properties = schema["properties"]
            .as_object()
            .expect("argument schema properties");
        assert!(
            properties.contains_key("bucket_name"),
            "{} must accept the SDK's bucket override",
            tool.name()
        );
        match tool.name() {
            "list_files" => {
                assert!(properties.contains_key("folder"));
                assert!(properties.contains_key("recursive"));
            }
            "create_file" => {
                assert!(properties.contains_key("filename"));
                assert!(properties.contains_key("filedata"));
            }
            other => assert!(
                properties.contains_key("filename"),
                "{other} must take the SDK's `filename`"
            ),
        }
    }
}

/// A read within the cap comes back as the file's TEXT, and the call reaches
/// the claim-bound route rather than any public endpoint.
#[tokio::test]
async fn a_read_within_the_cap_returns_the_files_text() {
    let body = json!({
        "schema_version": "elitea.runtime.artifact-read.v1",
        "project_id": 17,
        "bucket": "agent-artifacts",
        "name": "notes.txt",
        "media_type": "text/plain",
        "byte_length": 11,
        "char_length": 11,
        "total_lines": 1,
        "max_chars": 200_000,
        "over_limit": false,
        "content": "TOKEN-VALUE",
    })
    .to_string();
    let (tools, rpc) = tools_of(&body, &["read_file"]).await;

    let answer = tools[0]
        .execute(context(), json!({"filename": "/notes.txt"}))
        .await
        .expect("artifact read");

    assert_eq!(answer, json!("TOKEN-VALUE"));
    assert_eq!(
        rpc.paths.lock().expect("fixture paths").as_slice(),
        ["/executions/execution%2Fone/generations/2/runtime-context/artifacts/read"]
    );
}

/// A read over the cap comes back as the SDK's own structured refusal, which
/// NAMES the limit and the actual size. A refusal that named neither would
/// leave the model unable to choose a slice that fits, and one that carried
/// the file as well would not be a cap at all.
#[tokio::test]
async fn a_read_over_the_cap_is_refused_with_the_limit_it_enforced() {
    let body = json!({
        "schema_version": "elitea.runtime.artifact-read.v1",
        "project_id": 17,
        "bucket": "agent-artifacts",
        "name": "big.txt",
        "media_type": "text/plain",
        "byte_length": 300_000,
        "char_length": 300_000,
        "total_lines": 4_167,
        "max_chars": 200_000,
        "over_limit": true,
        "content": "",
    })
    .to_string();
    let (tools, _rpc) = tools_of(&body, &["read_file"]).await;

    let answer = tools[0]
        .execute(context(), json!({"filename": "big.txt"}))
        .await
        .expect("artifact read");

    assert_eq!(answer["__result_status__"], json!("content_too_large"));
    assert_eq!(answer["read_limits"]["max_output_chars"], json!(200_000));
    assert_eq!(answer["context"]["actual_chars"], json!(300_000));
    assert_eq!(answer["total_lines"], json!(4_167));
    let rendered = answer.to_string();
    assert!(rendered.contains("200000"));
    assert!(!rendered.contains("\"content\""));
}

/// A write answers with the stored path, which is what the model has to tell
/// the user — and what a following `read_file` can be given verbatim.
#[tokio::test]
async fn a_write_answers_with_the_stored_path() {
    let body = json!({
        "schema_version": "elitea.runtime.artifact-write.v1",
        "project_id": 17,
        "bucket": "agent-artifacts",
        "name": "notes.txt",
        "media_type": "text/plain",
        "byte_length": 4,
    })
    .to_string();
    let (tools, _rpc) = tools_of(&body, &["create_file"]).await;

    let answer = tools[0]
        .execute(
            context(),
            json!({"filename": "notes.txt", "filedata": "data"}),
        )
        .await
        .expect("artifact write");

    assert!(
        answer
            .as_str()
            .is_some_and(|text| text.contains("/agent-artifacts/notes.txt")),
        "unexpected write answer: {answer}"
    );
}

/// A malformed argument is answered, never raised: an `AdkError` here would
/// abort the turn and discard the conversation that produced the request,
/// which is the rule every tool in this family follows.
#[tokio::test]
async fn a_missing_argument_is_answered_rather_than_failing_the_turn() {
    let (tools, rpc) = tools_of("{}", &["read_file"]).await;

    let answer = tools[0]
        .execute(context(), json!({"filename": "   "}))
        .await
        .expect("a missing argument must not fail the turn");

    assert!(
        answer
            .as_str()
            .is_some_and(|text| text.contains("filename"))
    );
    assert!(
        rpc.paths.lock().expect("fixture paths").is_empty(),
        "a malformed call must not reach the claim-bound route"
    );
}

/// The configuration is bounded and the bucket must be a name the platform can
/// actually address: a toolkit configured with one it cannot builds tools that
/// could only ever fail.
#[test]
fn the_configuration_is_bounded_and_the_bucket_addressable() {
    let parsed = ArtifactToolkitConfig::parse(&settings(&["read_file", "read_file"]))
        .expect("valid artifact configuration");
    assert_eq!(parsed.bucket(), "agent-artifacts");
    assert_eq!(parsed.selected_tools().len(), 1);

    for invalid in [
        json!({"selected_tools": []}),
        json!({"bucket": "", "selected_tools": []}),
        json!({"bucket": "Bad-Bucket", "selected_tools": []}),
        json!({"bucket": "a", "selected_tools": []}),
        json!({"bucket": "agent-artifacts", "selected_tools": "read_file"}),
    ] {
        let object = invalid.as_object().expect("invalid artifact fixture");
        let Err(error) = ArtifactToolkitConfig::parse(object) else {
            panic!("a malformed artifact configuration must not parse: {invalid}");
        };
        assert_eq!(error.code(), ArtifactConfigErrorCode::InvalidConfiguration);
    }

    let oversized = json!({"bucket": "b".repeat(64), "selected_tools": []});
    let Err(error) =
        ArtifactToolkitConfig::parse(oversized.as_object().expect("oversized fixture"))
    else {
        panic!("an oversized bucket name must not parse");
    };
    assert_eq!(error.code(), ArtifactConfigErrorCode::ResourceExhausted);
}
