use super::super::request::ModelContextLimits;
use super::*;
use adk_rust::session::ListRequest;

struct EvidenceConnector(Arc<AtomicUsize>);
#[async_trait]
impl McpConnector for EvidenceConnector {
    async fn connect(
        &self,
        _: &RemoteMcpConfig,
    ) -> Result<Arc<dyn Toolset>, McpMaterializationError> {
        Ok(Arc::new(BasicToolset::new(
            "evidence",
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
    fn parameters_schema(&self) -> Option<serde_json::Value> {
        Some(serde_json::json!({"type":"object","properties":{"release":{"type":"string"}}}))
    }
    fn is_read_only(&self) -> bool {
        true
    }
    async fn execute(
        &self,
        _: Arc<dyn ToolContext>,
        _: serde_json::Value,
    ) -> adk_rust::Result<serde_json::Value> {
        let call = self.0.fetch_add(1, Ordering::SeqCst);
        Ok(serde_json::json!({"evidence": format!("SOURCE_{call} {}", "evidence ".repeat(1000))}))
    }
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // One model/tool loop proves compaction, parent isolation, and durable event retention.
async fn nested_agent_compacts_its_tool_loop_without_compacting_parent_or_losing_events() {
    let mut request = ordinary_request(AgentExecutionKind::Application);
    attach_nested_agent(&mut request);
    request.payload.context_settings =
        serde_json::json!({"enabled":true,"budget_mode":"balanced","preserve_recent_messages":1})
            .as_object()
            .unwrap()
            .clone();
    request.payload.model_context_limits = Some(ModelContextLimits {
        context_window_tokens: 100_000,
        max_output_tokens: 16_000,
        context_window_fallback: false,
        max_output_fallback: false,
        max_input_tokens: None,
    });
    request.payload.summary_model = Some(crate::agents::request::SummaryModelSnapshot {
        llm_settings: serde_json::json!({"model_name":"dedicated-summary","model_project_id":23,"max_tokens":1024,"temperature":null,"openai_compatible":true}).as_object().unwrap().clone(),
        model_context_limits: ModelContextLimits { context_window_tokens: 32_000, max_output_tokens: 4_000, context_window_fallback: false, max_output_fallback: false, max_input_tokens: None },
    });
    let mut child = nested_agent_version(
        "Answer only the delegated task.",
        "child-model",
        23,
        vec![remote_mcp_tool()],
    );
    child["model_context_limits"] = serde_json::json!({"context_window_tokens":8000,"max_output_tokens":2048,"context_window_fallback":false,"max_output_fallback":false,"max_input_tokens":null});
    let context = runtime_context_client_from(
        VecDeque::from([
            runtime_context_response(
                &serde_json::json!({"schema_version":"elitea.runtime.elitea-client-token.v1","project_id":17,"token":TOKEN}),
            ),
            application_version_response(31, 41, child),
        ]),
        Arc::new(AtomicUsize::new(0)),
        Arc::new(Mutex::new(vec![])),
    );
    let (gateway, captured) = test_model_gateway_client(
        vec![
            TestModelGatewayOutcome::Response(nested_agent_call_response()),
            TestModelGatewayOutcome::Response(named_mcp_batch_response(
                "lookup_release",
                1,
                "call-one",
            )),
            TestModelGatewayOutcome::Response(named_mcp_batch_response(
                "lookup_release",
                1,
                "call-two",
            )),
            TestModelGatewayOutcome::Response(text_response(
                &crate::agents::context_summary::fixture(),
            )),
            TestModelGatewayOutcome::Response(text_response("Child done.")),
            TestModelGatewayOutcome::Response(text_response("Parent done.")),
        ],
        test_model_gateway_config(),
    )
    .unwrap();
    let sessions = Arc::new(InMemorySessionService::new());
    let tool_calls = Arc::new(AtomicUsize::new(0));
    let assembler = OrdinaryNativeAgentAssembler::new(
        platform_client(context),
        Arc::new(ModelFacade::from_gateway(gateway)),
        empty_tool_policy(),
    )
    .with_sessions(sessions.clone())
    .with_mcp_connector(Arc::new(EvidenceConnector(tool_calls.clone())));
    let output = drain_nested_resume(&assembler, request).await;
    assert_eq!(output, "Parent done.");
    assert_eq!(tool_calls.load(Ordering::SeqCst), 2);
    let captured: Vec<serde_json::Value> = captured
        .lock()
        .unwrap()
        .iter()
        .map(|request| serde_json::from_slice(&request.body).unwrap())
        .collect();
    assert_eq!(captured.len(), 6);
    assert_eq!(captured[3]["model"], "dedicated-summary");
    assert_eq!(captured[3]["max_completion_tokens"], 1024);
    assert_eq!(captured[4]["model"], "child-model");
    assert_eq!(captured[5]["model"], "fixture-model");
    assert!(
        captured[3].to_string().contains("SOURCE_0"),
        "summary consumes older evidence"
    );
    assert!(
        !captured[4].to_string().contains("SOURCE_0"),
        "child projection replaces covered evidence"
    );
    assert!(
        captured[4].to_string().contains("SOURCE_1"),
        "recent complete tool group remains exact"
    );
    assert!(captured[4].to_string().contains("Summarize release risk"));
    assert!(
        !captured[5].to_string().contains("SOURCE_1"),
        "parent receives only child output"
    );
    let plan_request = ordinary_request(AgentExecutionKind::Application);
    let plan = OrdinaryNativeAgentPlan::from_authorized(
        &plan_request,
        &OrdinaryNoToolProfile::validate(&plan_request).unwrap(),
        &AuthorizedNativeCommandBinding::fixture(),
        &[],
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
    let child = stored
        .iter()
        .find(|session| session.id().starts_with("elitea-model-"))
        .unwrap();
    assert!(child.state().get("elitea.context.root.v1").is_some());
    let serialized = serde_json::to_string(&child.events().all()).unwrap();
    assert!(serialized.contains("SOURCE_0") && serialized.contains("SOURCE_1"));
    let root = stored
        .iter()
        .find(|session| session.id() == plan.session_id())
        .unwrap();
    assert!(
        root.state()
            .get("elitea.context.root.v1")
            .is_none_or(|value| value.is_null())
    );
}
