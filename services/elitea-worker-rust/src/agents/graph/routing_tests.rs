use std::collections::{BTreeMap, HashMap};
use std::sync::{Arc, Mutex};

use adk_rust::futures::stream;
use adk_rust::graph::{ExecutionConfig, GraphAgent, MemoryCheckpointer, Node, NodeContext};
use adk_rust::runner::Runner;
use adk_rust::session::{CreateRequest, InMemorySessionService, SessionService};
use adk_rust::{Agent, Content, Event, EventStream, InvocationContext, Part, SessionId, UserId};
use async_trait::async_trait;
use serde_json::{Value, json};

use super::EliteaGraphAgent;
use super::compiler::PipelineDefinition;
use super::decision::{DecisionNode, DecisionNodeDefinition};
use super::llm::{
    LlmExecutionError, LlmExecutionInput, LlmNodeDefinition, PipelineLlmAgentBinding,
    PipelineLlmAgentFactory,
};
use super::router::{RouterNode, RouterNodeDefinition};
use crate::agents::runtime::NativeAgentInvocation;

#[tokio::test]
async fn router_renders_selected_state_and_routes_only_to_declared_targets() {
    let definition = RouterNodeDefinition::from_yaml(
        r#"
id: choose
type: router
condition: " {{ choice }} !!"
routes: [approved, next.step]
default_output: fallback
input: [choice]
"#,
    )
    .expect("Router definition");
    let digest = definition.config_digest();
    assert!(digest.iter().any(|byte| *byte != 0));
    assert_eq!(definition.input_keys(), ["choice"]);
    assert_eq!(
        definition.route_targets().collect::<Vec<_>>(),
        ["approved", "next.step", "fallback"]
    );

    for (choice, expected_label, expected_target) in [
        ("approved", "approved", "approved"),
        ("next.step", "next_step", "next.step"),
        ("undeclared", "fallback", "fallback"),
    ] {
        let context = NodeContext::new(
            HashMap::from([("choice".to_owned(), json!(choice))]),
            ExecutionConfig::new("router-thread"),
            0,
        );
        let output = RouterNode::new(definition.clone())
            .execute(&context)
            .await
            .expect("Router execution");
        assert_eq!(
            output.updates.get("router_output"),
            Some(&json!(expected_label))
        );
        assert_eq!(
            output.goto.as_deref(),
            Some(&[expected_target.to_owned()][..])
        );
    }
}

#[tokio::test]
async fn router_empty_input_uses_whole_state_and_end_requires_an_exact_label() {
    let definition = RouterNodeDefinition::from_yaml(
        r#"
id: choose
type: router
condition: "{{ (payload | json_loads).route }}"
routes: [END]
default_output: fallback
input: []
"#,
    )
    .expect("whole-state Router");
    let context = NodeContext::new(
        HashMap::from([("payload".to_owned(), json!(r#"{"route":"END"}"#))]),
        ExecutionConfig::new("router-whole-state"),
        0,
    );
    let output = RouterNode::new(definition)
        .execute(&context)
        .await
        .expect("END Router execution");
    assert_eq!(output.updates.get("router_output"), Some(&json!("END")));
    assert_eq!(output.goto.as_deref(), Some(&["__end__".to_owned()][..]));

    let exact = RouterNodeDefinition::from_yaml(
        "id: exact\ntype: router\ncondition: FRIEND\nroutes: [END]\ndefault_output: fallback\ninput: []\n",
    )
    .expect("exact END Router");
    let output = RouterNode::new(exact)
        .execute(&NodeContext::new(
            HashMap::new(),
            ExecutionConfig::new("router-exact-end"),
            0,
        ))
        .await
        .expect("exact END check");
    assert_eq!(
        output.updates.get("router_output"),
        Some(&json!("fallback"))
    );
}

#[test]
fn router_rejects_ambiguous_labels_unknown_fields_and_invalid_inputs() {
    for yaml in [
        "id: choose\ntype: router\nroutes: [next.step, next_step]\n",
        "id: choose\ntype: router\nroutes: ['bad/target']\n",
        "id: choose\ntype: router\ninput: [value, value]\n",
        "id: choose\ntype: router\ntransition: END\n",
    ] {
        assert!(RouterNodeDefinition::from_yaml(yaml).is_err());
    }
}

#[derive(Default)]
struct CapturedDecision {
    definition_id: String,
    selected_tools: usize,
    prompt: String,
    history: Vec<(String, String)>,
}

struct DecisionFactory {
    captured: Arc<Mutex<CapturedDecision>>,
    response: String,
}

impl PipelineLlmAgentFactory for DecisionFactory {
    fn build(
        &self,
        definition: &LlmNodeDefinition,
        _input: &LlmExecutionInput,
        output_schema: Option<Value>,
        _replay: Option<&super::llm::PipelineLlmReplayEnvelope>,
    ) -> Result<PipelineLlmAgentBinding, LlmExecutionError> {
        assert!(output_schema.is_none());
        let mut captured = self.captured.lock().expect("capture lock");
        captured.definition_id = definition.id().to_owned();
        captured.selected_tools = definition
            .tool_selections()
            .iter()
            .map(|selection| selection.tools().len())
            .sum();
        drop(captured);
        Ok(PipelineLlmAgentBinding::new(
            Arc::new(DecisionAgent {
                captured: Arc::clone(&self.captured),
                response: self.response.clone(),
            }),
            BTreeMap::new(),
        ))
    }
}

struct DecisionAgent {
    captured: Arc<Mutex<CapturedDecision>>,
    response: String,
}

#[async_trait]
impl Agent for DecisionAgent {
    fn name(&self) -> &'static str {
        "decision-fixture"
    }

    fn description(&self) -> &'static str {
        "pipeline Decision fixture"
    }

    fn sub_agents(&self) -> &[Arc<dyn Agent>] {
        &[]
    }

    async fn run(&self, context: Arc<dyn InvocationContext>) -> adk_rust::Result<EventStream> {
        let text = |content: &Content| {
            content
                .parts
                .iter()
                .filter_map(Part::text)
                .collect::<Vec<_>>()
                .join("")
        };
        let mut captured = self.captured.lock().expect("capture lock");
        captured.prompt = text(context.user_content());
        captured.history = context
            .session()
            .conversation_history()
            .iter()
            .map(|content| (content.role.clone(), text(content)))
            .collect();
        drop(captured);
        let mut event = Event::new(context.invocation_id());
        event.set_content(Content::new("model").with_text(self.response.clone()));
        Ok(Box::pin(stream::iter([Ok(event)])))
    }
}

fn decision_definition() -> DecisionNodeDefinition {
    DecisionNodeDefinition::from_yaml(
        r#"
id: decide
type: decision
nodes: [first, second.step]
description: "Route request for {customer}"
input: [messages, customer, facts]
default_output: fallback
"#,
    )
    .expect("Decision definition")
}

#[tokio::test]
async fn decision_reuses_claim_bound_model_without_tools_and_selects_exact_route() {
    let captured = Arc::new(Mutex::new(CapturedDecision::default()));
    let factory: Arc<dyn PipelineLlmAgentFactory> = Arc::new(DecisionFactory {
        captured: Arc::clone(&captured),
        response: " second.step \n".to_owned(),
    });
    let context = NodeContext::new(
        HashMap::from([
            ("customer".to_owned(), json!("Alice")),
            ("facts".to_owned(), json!({"priority":"high"})),
            (
                "messages".to_owned(),
                json!([
                    {"role":"user","content":"earlier"},
                    {"role":"assistant","content":"prior answer"}
                ]),
            ),
        ]),
        ExecutionConfig::new("decision-thread"),
        0,
    );
    let output = DecisionNode::new(decision_definition(), factory)
        .execute(&context)
        .await
        .expect("Decision execution");
    assert_eq!(
        output.updates.get("router_output"),
        Some(&json!("second_step"))
    );
    assert_eq!(
        output.goto.as_deref(),
        Some(&["second.step".to_owned()][..])
    );
    assert!(!output.updates.contains_key("messages"));

    let captured = captured.lock().expect("capture lock");
    assert_eq!(captured.definition_id, "decide");
    assert_eq!(captured.selected_tools, 0);
    assert!(
        captured
            .prompt
            .contains("Steps available: first,second_step")
    );
    assert!(
        captured
            .prompt
            .contains("Explanation: Route request for Alice")
    );
    assert!(captured.prompt.contains("facts: {\"priority\":\"high\"}"));
    assert_eq!(
        captured.history,
        [
            ("user".to_owned(), "earlier".to_owned()),
            ("model".to_owned(), "prior answer".to_owned())
        ]
    );
}

#[tokio::test]
async fn invalid_decision_answer_uses_the_declared_default() {
    let factory: Arc<dyn PipelineLlmAgentFactory> = Arc::new(DecisionFactory {
        captured: Arc::new(Mutex::new(CapturedDecision::default())),
        response: "invented node".to_owned(),
    });
    let output = DecisionNode::new(decision_definition(), factory)
        .execute(&NodeContext::new(
            HashMap::from([
                ("messages".to_owned(), json!([])),
                ("customer".to_owned(), json!("Alice")),
                ("facts".to_owned(), json!({})),
            ]),
            ExecutionConfig::new("decision-default"),
            0,
        ))
        .await
        .expect("Decision fallback");
    assert_eq!(
        output.updates.get("router_output"),
        Some(&json!("fallback"))
    );
    assert_eq!(output.goto.as_deref(), Some(&["fallback".to_owned()][..]));
}

#[test]
fn decision_admits_current_and_legacy_input_fields_but_rejects_route_drift() {
    let current = decision_definition();
    assert_eq!(current.input_keys(), ["messages", "customer", "facts"]);
    assert_eq!(
        current.route_targets().collect::<Vec<_>>(),
        ["first", "second.step", "fallback"]
    );
    assert!(current.config_digest().iter().any(|byte| *byte != 0));

    let legacy = DecisionNodeDefinition::from_yaml(
        "id: decide\ntype: decision\nnodes: [next]\ninput: [ignored]\ndecisional_inputs: [messages]\n",
    )
    .expect("legacy top-level Decision inputs");
    assert_eq!(legacy.input_keys(), ["messages"]);

    for yaml in [
        "id: decide\ntype: decision\nnodes: [next]\nroutes: [next]\n",
        "id: decide\ntype: decision\nnodes: [next, next]\n",
        "id: decide\ntype: decision\nnodes: [next]\ninput: [messages, messages]\n",
    ] {
        assert!(DecisionNodeDefinition::from_yaml(yaml).is_err());
    }
}

#[test]
fn compiler_accepts_active_router_and_decision_documents_with_exact_authority() {
    let router = PipelineDefinition::from_yaml(
        r#"
state:
  choice: {type: str, value: left}
entry_point: choose
nodes:
  - id: choose
    type: router
    condition: "{{ choice }}"
    routes: [left]
    default_output: END
    input: [choice]
  - id: left
    type: state_modifier
    transition: END
"#,
    )
    .expect("Router pipeline");
    router
        .compile("router-pipeline", Arc::new(MemoryCheckpointer::new()), None)
        .expect("pure Router graph");
    assert!(!router.has_llm_nodes());

    let decision = PipelineDefinition::from_yaml(
        r"
state:
  messages: list
entry_point: decide
nodes:
  - id: decide
    type: decision
    nodes: [left]
    default_output: END
  - id: left
    type: state_modifier
    transition: END
",
    )
    .expect("Decision pipeline");
    assert!(decision.has_llm_nodes());
    assert!(
        decision
            .compile(
                "decision-pipeline",
                Arc::new(MemoryCheckpointer::new()),
                None
            )
            .is_err()
    );
}

async fn run_compiled_route(graph: GraphAgent, session_id: &str) -> String {
    let sessions = Arc::new(InMemorySessionService::new());
    sessions
        .create(CreateRequest {
            app_name: "elitea".to_owned(),
            user_id: "route-user".to_owned(),
            session_id: Some(session_id.to_owned()),
            state: HashMap::new(),
        })
        .await
        .expect("route session");
    let session_service: Arc<dyn SessionService> = sessions;
    let runner = Runner::builder()
        .app_name("elitea")
        .agent(Arc::new(EliteaGraphAgent::new(graph)))
        .session_service(session_service)
        .build()
        .expect("route Runner");
    let mut run = NativeAgentInvocation::new(
        runner,
        UserId::new("route-user").expect("fixture user"),
        SessionId::new(session_id).expect("fixture session"),
        Content::new("user").with_text("start"),
    )
    .start()
    .expect("route invocation");
    let mut final_text = None;
    while let Some(event) = run.next_event().await.expect("route event") {
        final_text = event.content().map(|content| {
            content
                .parts
                .iter()
                .filter_map(Part::text)
                .collect::<Vec<_>>()
                .join("")
        });
    }
    final_text.expect("route terminal output")
}

#[tokio::test]
async fn router_and_decision_run_through_the_common_graph_runner() {
    let router = PipelineDefinition::from_yaml(
        r#"
state:
  choice: {type: str, value: left}
  answer: str
entry_point: choose
nodes:
  - id: choose
    type: router
    condition: "{{ choice }}"
    routes: [left]
    default_output: fallback
    input: [choice]
  - id: left
    type: state_modifier
    template: router-left
    output: [answer]
    transition: END
  - id: fallback
    type: state_modifier
    template: router-fallback
    output: [answer]
    transition: END
"#,
    )
    .expect("executable Router pipeline");
    let graph = router
        .compile("router-pipeline", Arc::new(MemoryCheckpointer::new()), None)
        .expect("compiled Router pipeline");
    assert_eq!(
        run_compiled_route(graph, "router-runner-thread").await,
        "router-left"
    );

    let decision = PipelineDefinition::from_yaml(
        r"
state:
  answer: str
  messages: list
entry_point: decide
nodes:
  - id: decide
    type: decision
    nodes: [left]
    default_output: fallback
  - id: left
    type: state_modifier
    template: decision-left
    output: [answer]
    transition: END
  - id: fallback
    type: state_modifier
    template: decision-fallback
    output: [answer]
    transition: END
",
    )
    .expect("executable Decision pipeline");
    let factory: Arc<dyn PipelineLlmAgentFactory> = Arc::new(DecisionFactory {
        captured: Arc::new(Mutex::new(CapturedDecision::default())),
        response: "left".to_owned(),
    });
    let graph = decision
        .compile_with_llm_runtime(
            "decision-pipeline",
            Arc::new(MemoryCheckpointer::new()),
            None,
            Some(&factory),
        )
        .expect("compiled Decision pipeline");
    assert_eq!(
        run_compiled_route(graph, "decision-runner-thread").await,
        "decision-left"
    );
}
