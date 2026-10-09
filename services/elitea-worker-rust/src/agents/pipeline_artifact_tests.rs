//! Direct `toolkit` nodes on an attached `artifact` toolkit (#906 follow-up).
//!
//! The artifact family's authority is the live execution claim, not a frozen
//! credential, so the pipeline assembler has to lend that claim exactly as the
//! ordinary agent path does. Before this, the family was skipped as
//! unsupported, the node's toolset never existed, and the run was refused as
//! "a pipeline direct tool node references a tool outside its frozen scope".

use super::*;

/// The YAML of the browser-created pipeline that was refused (version 191 of
/// the reference stack), unchanged.
const ARTIFACT_PIPELINE: &str = "state:
  input: {type: str}
  messages: {type: list}
  created: {type: dict}
  listing: {type: dict}
entry_point: mk
nodes:
  - id: mk
    type: toolkit
    toolkit_name: test
    tool: create_file
    input_mapping:
      filename: {type: fixed, value: verify-1ab920d-edge.txt}
      filedata: {type: fixed, value: edge identity ok}
    output: [created]
    transition: ls
  - id: ls
    type: toolkit
    toolkit_name: test
    tool: list_files
    input_mapping: {}
    output: [listing]
    transition: END
";

const ARTIFACT_LIST_PIPELINE: &str = "state:
  input: {type: str}
  messages: {type: list}
  listing: {type: dict}
entry_point: ls
nodes:
  - id: ls
    type: toolkit
    toolkit_name: test
    tool: list_files
    input_mapping: {}
    output: [listing]
    transition: END
";

/// The frozen reference Main sends for the product form's artifact toolkit:
/// the stored settings keep the SDK catalogue, including names this runtime
/// does not serve, and the attachment row carries no per-agent selection.
fn artifact_pipeline_request(definition: &str) -> super::super::request::AgentExecutionRequest {
    let mut request = pipeline_request();
    let version = request
        .payload
        .application
        .get_mut("version_details")
        .and_then(Value::as_object_mut)
        .expect("application version fixture");
    version.insert("instructions".to_owned(), json!(definition));
    let selected_tools = json!([
        "index_data",
        "list_indexes",
        "search_index",
        "stepback_search_index",
        "stepback_summary_index",
        "remove_index",
        "list_files",
        "create_file",
        "read_file",
        "get_file_metadata",
        "delete_file",
        "append_data",
        "create_new_bucket",
        "read_multiple_files",
        "grep_file",
        "edit_file"
    ]);
    version.insert(
        "tools".to_owned(),
        json!([{
            "id": 5,
            "type": "artifact",
            "name": "test",
            "toolkit_name": "test",
            "settings": {
                "bucket": "test",
                "selected_tools": selected_tools,
                "available_tools": selected_tools,
                "embedding_model": "text-embedding-3-small",
                "pgvector_configuration": {"private": false, "elitea_title": "elitea-pgvector"}
            }
        }]),
    );
    request
}

/// A platform whose claim-bound runtime-context listener answers `responses`
/// in order and records every path it was asked for.
fn artifact_assembler(
    responses: &[Value],
) -> (PipelineNativeAgentAssembler, Arc<Mutex<Vec<String>>>) {
    let paths = Arc::new(Mutex::new(Vec::new()));
    let runtime = RuntimeContextClient::with_rpc(
        PipelineRuntimeContextFixture {
            responses: Mutex::new(responses.iter().map(runtime_response).collect()),
            calls: Arc::new(AtomicUsize::new(0)),
            paths: Arc::clone(&paths),
        },
        RuntimeContextConfig {
            origin: "https://content.internal".to_owned(),
            deadline: Duration::from_secs(1),
            max_response_bytes: 32 * 1_024,
            max_application_response_bytes: 1_024 * 1_024,
            max_attachment_response_bytes: 1_024 * 1_024,
            max_artifact_response_bytes: 2 * 1_024 * 1_024,
        },
    )
    .expect("artifact runtime-context fixture");
    let (gateway, _) = test_model_gateway_client(Vec::new(), test_model_gateway_config())
        .expect("unused artifact model gateway");
    let assembler = PipelineNativeAgentAssembler::with_state(
        Arc::new(InMemorySessionService::new()),
        Arc::new(MemoryCheckpointer::new()),
    )
    .with_runtime_clients(
        Arc::new(PlatformClient::new(Arc::new(runtime))),
        Arc::new(ModelFacade::from_gateway(gateway)),
    );
    (assembler, paths)
}

#[tokio::test]
async fn artifact_direct_toolkit_nodes_assemble_against_the_attached_toolkit() {
    let request = artifact_pipeline_request(ARTIFACT_PIPELINE);
    let (assembler, paths) = artifact_assembler(&[]);
    if let Err(error) = assembler.assemble(authorized(&request)).await {
        panic!("artifact direct-node pipeline was refused: {error}");
    }
    // Assembly binds the tools; nothing reaches the platform until a node runs.
    assert!(paths.lock().expect("fixture paths").is_empty());
}

#[tokio::test(flavor = "current_thread")]
async fn artifact_list_node_runs_under_the_claim_and_projects_its_listing() {
    let request = artifact_pipeline_request(ARTIFACT_LIST_PIPELINE);
    let (assembler, paths) = artifact_assembler(&[json!({
        "schema_version": "elitea.runtime.artifact-list.v1",
        "project_id": 17,
        "bucket": "test",
        "files": [{
            "name": "verify-1ab920d-edge.txt",
            "byte_length": 16,
            "media_type": "text/plain",
            "modified_at": "2026-10-09T14:16:13Z"
        }],
        "truncated": false
    })]);
    let invocation = assembler
        .assemble(authorized(&request))
        .await
        .unwrap_or_else(|error| panic!("artifact list pipeline was refused: {error}"));
    let browser = collect_pipeline_completion(invocation).await;

    let paths = paths.lock().expect("fixture paths").clone();
    assert_eq!(paths.len(), 1, "exactly one claim-bound call: {paths:?}");
    assert!(
        paths[0].ends_with("/runtime-context/artifacts/list"),
        "the listing must use the claim-bound artifact route: {paths:?}"
    );
    let rendered = serde_json::to_string(&browser).expect("browser events");
    assert!(
        rendered.contains("verify-1ab920d-edge.txt"),
        "the listing must reach the pipeline output: {rendered}"
    );
}

/// A position that still cannot lend the claim says so in its own terms:
/// the toolkit IS in the frozen scope, so "outside its frozen scope" was the
/// wrong failure and sent the investigation to Main's freeze.
#[tokio::test]
async fn an_unserved_artifact_toolkit_is_refused_as_unsupported_not_out_of_scope() {
    let request = artifact_pipeline_request(ARTIFACT_PIPELINE);
    let assembler = PipelineNativeAgentAssembler::with_state(
        Arc::new(InMemorySessionService::new()),
        Arc::new(MemoryCheckpointer::new()),
    );
    let Err(error) = assembler.assemble(authorized(&request)).await else {
        panic!("an artifact toolkit without a claim-bound platform was bound");
    };
    assert_eq!(
        error.code(),
        super::super::runtime::NativeAgentAssemblyErrorCode::UnsupportedCapability
    );
    let message = error.to_string();
    assert!(!message.contains("outside its frozen scope"), "{message}");
    assert_direct_tool_node_message(&error);
}

/// The production-reachable unserved position: a saved child pipeline still
/// materializes without the claim, so its artifact direct node is refused as
/// unsupported before anything runs, never as out of scope.
#[tokio::test]
async fn a_saved_child_pipelines_artifact_node_is_refused_as_unsupported() {
    let child = artifact_pipeline_request(ARTIFACT_LIST_PIPELINE)
        .payload
        .application["version_details"]
        .clone();
    let (platform, model_facade, _, _) = pipeline_runtime(&child, Vec::new());
    let assembler = PipelineNativeAgentAssembler::with_state(
        Arc::new(InMemorySessionService::new()),
        Arc::new(MemoryCheckpointer::new()),
    )
    .with_runtime_clients(platform, model_facade);
    let request = agent_pipeline_request("release-agent", "pipeline");
    let Err(error) = assembler.assemble(authorized(&request)).await else {
        panic!("a saved child pipeline bound an artifact toolkit without the claim");
    };
    assert_eq!(
        error.code(),
        super::super::runtime::NativeAgentAssemblyErrorCode::UnsupportedCapability,
        "{error}"
    );
    assert!(
        !error.to_string().contains("outside its frozen scope"),
        "{error}"
    );
    assert_eq!(
        error
            .cause()
            .map(elitea_agent_runtime::assembly_error::NativeAgentAssemblyCause::code),
        Some(super::super::pipeline::UNSERVED_DIRECT_TOOLKIT_CODE)
    );
    assert_direct_tool_node_message(&error);
}
