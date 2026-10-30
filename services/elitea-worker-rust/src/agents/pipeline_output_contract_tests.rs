//! ADR-0043 decision 2 verification: a pipeline LLM node with a JSON Schema
//! output contract whose model answers in prose is retried once, then fails
//! with `output_contract_violation`; the next node (a state modifier that
//! copies the answer) never runs, so its output stays empty.

use super::*;

const CONTRACT_PIPELINE: &str = "state:\n  answer: str\n  final_text: str\n  messages: list\nentry_point: extract\nnodes:\n  - id: extract\n    type: llm\n    output: [answer, messages]\n    output_contract:\n      type: json_schema\n      schema:\n        type: object\n        properties:\n          total:\n            type: number\n        required: [total]\n        additionalProperties: false\n    transition: report\n  - id: report\n    type: state_modifier\n    template: '{{ answer }}'\n    input: [answer]\n    output: [final_text]\n    transition: END\n";

#[tokio::test]
async fn a_contract_node_answering_in_prose_is_retried_once_then_ends_the_run() {
    let mut request = pipeline_request();
    request.payload.context_settings = json!({"enabled":false}).as_object().unwrap().clone();
    request.payload.application["version_details"]["instructions"] = json!(CONTRACT_PIPELINE);
    let ((platform, facade, _, _), captured) = pipeline_runtime_from_responses_with_capture(
        VecDeque::from([runtime_response(
            &json!({"schema_version":"elitea.runtime.elitea-client-token.v1","project_id":17,"token":"ephemeral-pipeline-token"}),
        )]),
        vec![
            TestModelGatewayOutcome::Response(pipeline_text_response("The total is 42.")),
            TestModelGatewayOutcome::Response(pipeline_text_response("It is forty-two.")),
            // A third answer is there to show that no other call is made.
            TestModelGatewayOutcome::Response(pipeline_text_response(r#"{"total": 42}"#)),
        ],
        Arc::new(AtomicUsize::new(0)),
        Arc::new(Mutex::new(vec![])),
    );
    let graph = Arc::new(MemoryCheckpointer::new());
    let assembler = PipelineNativeAgentAssembler::with_state(
        Arc::new(InMemorySessionService::new()),
        graph.clone(),
    )
    .with_runtime_clients(platform, facade);
    let (mut invocation, _, _) = assembler
        .assemble(authorized(&request))
        .await
        .unwrap()
        .start()
        .unwrap();
    let failure = loop {
        match invocation.next_event().await {
            Ok(Some(_)) => {}
            Ok(None) => panic!("a contract violation must fail the run"),
            Err(error) => break error,
        }
    };
    assert_eq!(
        failure.upstream_code(),
        Some(elitea_agent_runtime::output_contract::VIOLATION_CODE)
    );
    assert_eq!(
        crate::protocol::output::model_failure(failure.upstream_code()),
        crate::protocol::output::RuntimeFailureKind::OutputContractViolation
    );
    let requests = captured.lock().unwrap();
    // The first answer and one retry; the next node was never invoked.
    assert_eq!(requests.len(), 2);
    let first: Value = serde_json::from_slice(&requests[0].body).unwrap();
    let retry: Value = serde_json::from_slice(&requests[1].body).unwrap();
    // The schema is the provider's response format.
    assert_eq!(
        first["response_format"]["json_schema"]["schema"]["required"],
        json!(["total"])
    );
    let retry_text = retry["messages"].to_string();
    assert!(retry_text.contains("The total is 42."));
    assert!(retry_text.contains("not a JSON document"));
    drop(requests);
    let saved = graph
        .load(&private_pipeline_session_id(&request))
        .await
        .unwrap()
        .expect("the graph frontier exists before the first model call");
    assert_eq!(saved.pending_nodes, ["extract"]);
    assert_eq!(saved.state.get("final_text"), Some(&json!("")));
}

#[tokio::test]
async fn a_contract_node_corrected_on_the_retry_feeds_the_next_node() {
    let mut request = pipeline_request();
    request.payload.context_settings = json!({"enabled":false}).as_object().unwrap().clone();
    request.payload.application["version_details"]["instructions"] = json!(CONTRACT_PIPELINE);
    let ((platform, facade, _, _), captured) = pipeline_runtime_from_responses_with_capture(
        VecDeque::from([runtime_response(
            &json!({"schema_version":"elitea.runtime.elitea-client-token.v1","project_id":17,"token":"ephemeral-pipeline-token"}),
        )]),
        vec![
            TestModelGatewayOutcome::Response(pipeline_text_response("The total is 42.")),
            TestModelGatewayOutcome::Response(pipeline_text_response(r#"{"total": 42}"#)),
        ],
        Arc::new(AtomicUsize::new(0)),
        Arc::new(Mutex::new(vec![])),
    );
    let graph = Arc::new(MemoryCheckpointer::new());
    let assembler = PipelineNativeAgentAssembler::with_state(
        Arc::new(InMemorySessionService::new()),
        graph.clone(),
    )
    .with_runtime_clients(platform, facade);
    let (mut invocation, _, _) = assembler
        .assemble(authorized(&request))
        .await
        .unwrap()
        .start()
        .unwrap();
    loop {
        match invocation.next_event().await {
            Ok(Some(_)) => {}
            Ok(None) => break,
            Err(error) => panic!(
                "run failed: {:?} after {} model calls",
                error.upstream_code(),
                captured.lock().unwrap().len()
            ),
        }
    }
    assert_eq!(captured.lock().unwrap().len(), 2);
    let saved = graph
        .load(&private_pipeline_session_id(&request))
        .await
        .unwrap()
        .unwrap();
    assert_eq!(
        saved.state.get("final_text"),
        Some(&json!(r#"{"total": 42}"#))
    );
}
