use super::super::request::ModelContextLimits;
use super::*;
use adk_rust::session::ListRequest;

fn enable_compaction(request: &mut super::super::request::AgentExecutionRequest) {
    request.payload.context_settings =
        json!({"enabled":true,"budget_mode":"balanced","preserve_recent_messages":1})
            .as_object()
            .unwrap()
            .clone();
    request.payload.model_context_limits = Some(ModelContextLimits {
        context_window_tokens: 8000,
        max_output_tokens: 2048,
        context_window_fallback: false,
        max_output_fallback: false,
        max_input_tokens: None,
    });
    request.payload.application["version_details"]["llm_settings"]["max_tokens"] = json!(2048);
    request.payload.summary_model = Some(crate::agents::request::SummaryModelSnapshot {
        llm_settings: json!({"model_name":"dedicated-summary","model_project_id":17,"max_tokens":1024,"temperature":null,"openai_compatible":true}).as_object().unwrap().clone(),
        model_context_limits: ModelContextLimits { context_window_tokens:32_000, max_output_tokens:4_000, context_window_fallback:false, max_output_fallback:false, max_input_tokens:None },
    });
}

struct EvidenceConnector(Arc<AtomicUsize>);
#[async_trait]
impl McpConnector for EvidenceConnector {
    async fn connect(
        &self,
        _: &RemoteMcpConfig,
    ) -> Result<Arc<dyn Toolset>, McpMaterializationError> {
        Ok(Arc::new(BasicToolset::new(
            "fixture",
            vec![Arc::new(EvidenceTool(self.0.clone()))],
        )))
    }
}
struct EvidenceTool(Arc<AtomicUsize>);
#[async_trait]
impl Tool for EvidenceTool {
    fn name(&self) -> &'static str {
        "lookup_release"
    }
    fn description(&self) -> &'static str {
        "Read release evidence"
    }
    fn parameters_schema(&self) -> Option<Value> {
        Some(json!({"type":"object","properties":{"release":{"type":"string"}}}))
    }
    fn is_read_only(&self) -> bool {
        true
    }
    async fn execute(&self, _: Arc<dyn ToolContext>, _: Value) -> adk_rust::Result<Value> {
        let call = self.0.fetch_add(1, Ordering::SeqCst);
        Ok(json!({"evidence":format!("SOURCE_{call} {}", "evidence ".repeat(1000))}))
    }
}

#[tokio::test]
async fn deterministic_pipeline_accepts_policy_without_a_model_or_summary_call() {
    let mut request = pipeline_request();
    enable_compaction(&mut request);
    request.payload.application["version_details"]["instructions"] = json!(STATE_MODIFIER_PIPELINE);
    let graph = Arc::new(MemoryCheckpointer::new());
    let assembler = PipelineNativeAgentAssembler::with_state(
        Arc::new(InMemorySessionService::new()),
        graph.clone(),
    );
    let browser =
        collect_pipeline_completion(assembler.assemble(authorized(&request)).await.unwrap()).await;
    assert!(
        browser
            .iter()
            .any(|event| event["content"] == "Hello, current")
    );
    let saved = graph
        .load(&private_pipeline_session_id(&request))
        .await
        .unwrap()
        .unwrap();
    assert_eq!(saved.state.get("prefix"), Some(&json!("Hello")));
    assert_eq!(
        saved.state.get("final_text"),
        Some(&json!("Hello, current"))
    );
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // One mixed graph proves model compaction and exact deterministic state.
async fn pipeline_model_compacts_tool_history_without_rewriting_graph_data() {
    let mut request = llm_mcp_pipeline_request(
        "release intelligence",
        &["lookup_release"],
        &["lookup_release"],
    );
    enable_compaction(&mut request);
    let exact_data = "exact graph value ".repeat(200);
    request.payload.application["version_details"]["instructions"] = json!(format!(
        "state:\n  answer: str\n  final_text: str\n  messages: list\n  exact_data:\n    type: str\n    value: {exact_data:?}\nentry_point: answer\nnodes:\n  - id: answer\n    type: llm\n    output: [answer, messages]\n    tool_names:\n      release intelligence: [lookup_release]\n    transition: finish\n  - id: finish\n    type: state_modifier\n    template: '{{{{ answer }}}}'\n    input: [answer]\n    output: [final_text]\n    transition: END\n"
    ));
    let ((platform, facade, _, _), captured) = pipeline_runtime_from_responses_with_capture(
        VecDeque::from([runtime_response(
            &json!({"schema_version":"elitea.runtime.elitea-client-token.v1","project_id":17,"token":"ephemeral-pipeline-token"}),
        )]),
        vec![
            TestModelGatewayOutcome::Response(pipeline_named_mcp_tool_call_response(
                "call-one",
                "lookup_release",
            )),
            TestModelGatewayOutcome::Response(pipeline_named_mcp_tool_call_response(
                "call-two",
                "lookup_release",
            )),
            TestModelGatewayOutcome::Response(pipeline_text_response(
                &crate::agents::context_summary::fixture(),
            )),
            TestModelGatewayOutcome::Response(pipeline_text_response("Verified pipeline answer")),
        ],
        Arc::new(AtomicUsize::new(0)),
        Arc::new(Mutex::new(vec![])),
    );
    let sessions = Arc::new(InMemorySessionService::new());
    let graph = Arc::new(MemoryCheckpointer::new());
    let tool_calls = Arc::new(AtomicUsize::new(0));
    let assembler = PipelineNativeAgentAssembler::with_state(sessions.clone(), graph.clone())
        .with_runtime_clients(platform, facade)
        .with_mcp_connector(Arc::new(EvidenceConnector(tool_calls.clone())));
    let browser =
        collect_pipeline_completion(assembler.assemble(authorized(&request)).await.unwrap()).await;
    assert!(
        browser
            .iter()
            .any(|event| event["content"] == "Verified pipeline answer")
    );
    assert_eq!(tool_calls.load(Ordering::SeqCst), 2);
    let calls: Vec<Value> = captured
        .lock()
        .unwrap()
        .iter()
        .map(|call| serde_json::from_slice(&call.body).unwrap())
        .collect();
    assert_eq!(calls.len(), 4);
    assert_eq!(calls[2]["model"], "dedicated-summary");
    assert_eq!(calls[2]["max_completion_tokens"], 1024);
    assert_eq!(calls[3]["model"], calls[0]["model"]);
    assert!(calls[2].to_string().contains("SOURCE_0"));
    assert!(!calls[3].to_string().contains("SOURCE_0"));
    assert!(calls[3].to_string().contains("SOURCE_1"));
    let saved = graph
        .load(&private_pipeline_session_id(&request))
        .await
        .unwrap()
        .unwrap();
    assert_eq!(saved.state.get("exact_data"), Some(&json!(exact_data)));
    assert_eq!(
        saved.state.get("answer"),
        Some(&json!("Verified pipeline answer"))
    );
    assert_eq!(
        saved.state.get("final_text"),
        Some(&json!("Verified pipeline answer"))
    );
    assert!(
        !serde_json::to_string(&saved.state)
            .unwrap()
            .contains("Continue the original task.")
    );
    let profile = PipelineExecutionProfile::validate(&request, false).unwrap();
    let plan = OrdinaryNativeAgentPlan::from_authorized_pipeline(
        &request,
        profile.shell(),
        &AuthorizedNativeCommandBinding::fixture(),
        &[],
        false,
        false,
    )
    .unwrap();
    let stored = sessions
        .list(ListRequest {
            app_name: "elitea-agent-v1".into(),
            user_id: plan.user_id().into(),
            limit: None,
            offset: None,
        })
        .await
        .unwrap();
    assert_eq!(stored.len(), 2);
    let model = stored
        .iter()
        .find(|session| session.id().starts_with("elitea-model-"))
        .unwrap();
    assert!(model.state().get("elitea.context.root.v1").is_some());
    let history = serde_json::to_string(&model.events().all()).unwrap();
    assert!(history.contains("SOURCE_0") && history.contains("SOURCE_1"));
    assert!(history.contains("Verified pipeline answer"));
    assert!(
        model
            .events()
            .all()
            .iter()
            .all(|event| !event.llm_response.partial)
    );
}
