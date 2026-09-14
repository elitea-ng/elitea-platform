use std::collections::{BTreeMap, HashMap};
use std::sync::{Arc, Mutex};

use adk_rust::futures::stream;
use adk_rust::graph::{Checkpointer, ExecutionConfig, MemoryCheckpointer, Node, NodeContext};
use adk_rust::runner::Runner;
use adk_rust::session::{CreateRequest, InMemorySessionService, SessionService};
use adk_rust::{Agent, Content, Event, EventStream, InvocationContext, Part, SessionId, UserId};
use async_trait::async_trait;
use serde_json::json;

use super::llm::{
    LlmExecutionError, LlmExecutionInput, LlmInputMapping, LlmNode, LlmNodeDefinition,
    PipelineLlmAgentBinding, PipelineLlmAgentFactory,
};
use super::{EliteaGraphAgent, compiler::PipelineDefinition};
use crate::agents::runtime::NativeAgentInvocation;

const LLM_NODE: &str = r#"
id: summarize
type: llm
input_mapping:
  system:
    type: fixed
    value: Return a bounded result.
  task:
    type: fstring
    value: "Summarize {source}"
  chat_history:
    type: variable
    value: messages
input: [source, messages]
output: [summary, facts, messages]
tool_names:
  jira: [search_issues]
  slack: [read_messages]
structured_output: true
tool_execution_timeout: 120
transition: END
"#;

fn state_types() -> BTreeMap<String, String> {
    BTreeMap::from([
        ("source".to_owned(), "str".to_owned()),
        ("summary".to_owned(), "str".to_owned()),
        ("facts".to_owned(), "list".to_owned()),
        ("messages".to_owned(), "list".to_owned()),
    ])
}

#[test]
fn llm_node_admits_ui_mapping_tool_scope_and_structured_schema() {
    let node = LlmNodeDefinition::from_yaml(LLM_NODE).expect("LLM node");
    assert_eq!(node.id(), "summarize");
    assert_eq!(node.input_keys(), ["source", "messages"]);
    assert_eq!(node.output_keys(), ["summary", "facts", "messages"]);
    assert_eq!(node.transition(), Some("END"));
    assert!(node.structured_output());
    assert_eq!(node.tool_execution_timeout(), 120);
    assert!(matches!(
        node.input_mapping().get("chat_history"),
        Some(LlmInputMapping::Variable(value)) if value == "messages"
    ));
    assert_eq!(node.tool_selections().len(), 2);
    assert_eq!(node.tool_selections()[0].alias(), "jira");
    assert_eq!(node.tool_selections()[0].tools(), ["search_issues"]);
    assert_eq!(node.tool_selections()[1].alias(), "slack");
    assert_eq!(node.tool_selections()[1].tools(), ["read_messages"]);

    let schema = node
        .output_schema(&state_types())
        .expect("output schema")
        .expect("structured schema");
    assert_eq!(
        schema,
        json!({
            "type": "object",
            "properties": {
                "facts": {"type": "array"},
                "summary": {"type": "string"}
            },
            "required": ["summary", "facts"],
            "additionalProperties": false
        })
    );
    assert!(node.config_digest().iter().any(|byte| *byte != 0));
}

#[test]
fn structured_output_projects_declared_keys_and_always_retains_messages() {
    let node = LlmNodeDefinition::from_yaml(LLM_NODE).expect("LLM node");
    let updates = node
        .project_response(r#"{"summary":"done","facts":["a","b"]}"#, &state_types())
        .expect("structured projection");
    assert_eq!(updates.get("summary"), Some(&json!("done")));
    assert_eq!(updates.get("facts"), Some(&json!(["a", "b"])));
    assert_eq!(
        updates.get("messages"),
        Some(&json!([{
            "role": "assistant",
            "content": r#"{"summary":"done","facts":["a","b"]}"#
        }]))
    );

    assert_eq!(
        node.project_response(r#"{"summary":"done","facts":{}}"#, &state_types()),
        Err(LlmExecutionError::InvalidStructuredOutput)
    );
    assert_eq!(
        node.project_response("not json", &state_types()),
        Err(LlmExecutionError::InvalidStructuredOutput)
    );
    assert_eq!(
        node.project_response(
            r#"{"summary":"done","facts":[],"unexpected":true}"#,
            &state_types()
        ),
        Err(LlmExecutionError::InvalidStructuredOutput)
    );
}

#[test]
fn nonstructured_output_writes_only_first_data_channel() {
    let node = LlmNodeDefinition::from_yaml(
        r"
id: draft
type: llm
output: [answer, messages]
transition: END
",
    )
    .expect("plain LLM node");
    let updates = node
        .project_response("plain answer", &state_types())
        .expect("plain projection");
    assert_eq!(updates.get("answer"), Some(&json!("plain answer")));
    assert_eq!(
        updates.get("messages"),
        Some(&json!([{"role": "assistant", "content": "plain answer"}]))
    );
}

#[test]
fn llm_node_rejects_ambiguous_tools_outputs_and_mapping_authority() {
    for yaml in [
        LLM_NODE.replace(
            "slack: [read_messages]",
            "slack: [read_messages, read_messages]",
        ),
        LLM_NODE.replace(
            "output: [summary, facts, messages]",
            "output: [summary, summary]",
        ),
        LLM_NODE.replace("type: fstring", "type: expression"),
        LLM_NODE.replace("tool_execution_timeout: 120", "tool_execution_timeout: 901"),
        LLM_NODE.replace("jira: [search_issues]", "\"\": [search_issues]"),
    ] {
        assert!(LlmNodeDefinition::from_yaml(&yaml).is_err());
    }

    let plain_multiple = LLM_NODE
        .replace("structured_output: true", "structured_output: false")
        .replace(
            "output: [summary, facts, messages]",
            "output: [summary, facts]",
        );
    assert!(LlmNodeDefinition::from_yaml(&plain_multiple).is_err());
}

#[test]
fn llm_node_preserves_same_operation_selected_from_distinct_toolkits() {
    let node = LlmNodeDefinition::from_yaml(
        "id: answer\ntype: llm\ntool_names:\n  attachments: [list_indexes]\n  configurations: [list_indexes]\ntransition: END\n",
    )
    .expect("toolkit-scoped duplicate operation names");
    assert_eq!(node.tool_selections().len(), 2);
    assert!(node.tool_selections().iter().all(|selection| {
        selection.tools() == ["list_indexes"]
            && matches!(selection.alias(), "attachments" | "configurations")
    }));
}

#[test]
fn missing_mapping_uses_legacy_messages_input_without_selecting_tools() {
    let node = LlmNodeDefinition::from_yaml(
        r"
id: answer
type: llm
transition: END
",
    )
    .expect("legacy minimal LLM node");
    assert!(matches!(
        node.input_mapping().get("messages"),
        Some(LlmInputMapping::Variable(value)) if value == "messages"
    ));
    assert!(node.tool_selections().is_empty());
    assert_eq!(node.input_keys(), ["messages"]);

    let empty_ui_selection = LlmNodeDefinition::from_yaml(
        "id: answer\ntype: llm\ntool_names:\n  jira: []\ntransition: END\n",
    )
    .expect("empty UI toolkit selection");
    assert!(empty_ui_selection.tool_selections().is_empty());
}

#[derive(Default)]
struct CapturedInvocation {
    system: String,
    schema: Option<serde_json::Value>,
    task: String,
    history: Vec<(String, String)>,
    context_identity: (String, String),
    session_identity: (String, String),
}

struct CapturingFactory {
    captured: Arc<Mutex<CapturedInvocation>>,
    response: String,
}

impl PipelineLlmAgentFactory for CapturingFactory {
    fn build(
        &self,
        _definition: &LlmNodeDefinition,
        input: &LlmExecutionInput,
        output_schema: Option<serde_json::Value>,
        _replay: Option<&super::llm::PipelineLlmReplayEnvelope>,
    ) -> Result<PipelineLlmAgentBinding, LlmExecutionError> {
        let mut captured = self.captured.lock().expect("capture lock");
        captured.system = input.system().to_owned();
        captured.schema = output_schema;
        drop(captured);
        Ok(PipelineLlmAgentBinding::new(
            Arc::new(CapturingAgent {
                captured: Arc::clone(&self.captured),
                response: self.response.clone(),
            }),
            BTreeMap::new(),
        ))
    }
}

struct CapturingAgent {
    captured: Arc<Mutex<CapturedInvocation>>,
    response: String,
}

#[async_trait]
impl Agent for CapturingAgent {
    fn name(&self) -> &'static str {
        "summarize"
    }

    fn description(&self) -> &'static str {
        "pipeline LLM node fixture"
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
        captured.task = text(context.user_content());
        captured.history = context
            .session()
            .conversation_history()
            .iter()
            .map(|content| (content.role.clone(), text(content)))
            .collect();
        captured.context_identity = (context.app_name().to_owned(), context.user_id().to_owned());
        captured.session_identity = (
            context.session().app_name().to_owned(),
            context.session().user_id().to_owned(),
        );
        drop(captured);

        let mut event = Event::new(context.invocation_id());
        event.set_content(Content::new("model").with_text(self.response.clone()));
        Ok(Box::pin(stream::iter([Ok(event)])))
    }
}

#[tokio::test]
async fn native_llm_node_maps_utf8_prompt_history_and_structured_state() {
    let definition = LlmNodeDefinition::from_yaml(LLM_NODE).expect("LLM node");
    let captured = Arc::new(Mutex::new(CapturedInvocation::default()));
    let factory: Arc<dyn PipelineLlmAgentFactory> = Arc::new(CapturingFactory {
        captured: Arc::clone(&captured),
        response: r#"{"summary":"готово","facts":["a"]}"#.to_owned(),
    });
    let node = LlmNode::new(definition, state_types(), factory);
    let state = std::collections::HashMap::from([
        ("source".to_owned(), json!("Київ")),
        (
            "messages".to_owned(),
            json!([
                {"role":"user","content":"earlier"},
                {"role":"assistant","content":"prior answer"}
            ]),
        ),
    ]);
    let output = node
        .execute(&NodeContext::new(
            state,
            ExecutionConfig::new("pipeline-thread"),
            0,
        ))
        .await
        .expect("native LLM node");

    assert_eq!(output.updates.get("summary"), Some(&json!("готово")));
    assert_eq!(output.updates.get("facts"), Some(&json!(["a"])));
    assert_eq!(
        output.updates.get("messages"),
        Some(&json!([{
            "role":"assistant",
            "content": r#"{"summary":"готово","facts":["a"]}"#
        }]))
    );

    let captured = captured.lock().expect("capture lock");
    assert_eq!(captured.system, "Return a bounded result.");
    assert_eq!(captured.task, "Summarize Київ");
    assert_eq!(
        captured.history,
        [
            ("user".to_owned(), "earlier".to_owned()),
            ("model".to_owned(), "prior answer".to_owned()),
            ("user".to_owned(), "Summarize Київ".to_owned())
        ]
    );
    assert_eq!(
        captured.context_identity,
        ("graph_app".to_owned(), "graph_user".to_owned())
    );
    assert_eq!(captured.session_identity, captured.context_identity);
    assert_eq!(
        captured.schema,
        Some(json!({
            "type": "object",
            "properties": {
                "facts": {"type": "array"},
                "summary": {"type": "string"}
            },
            "required": ["summary", "facts"],
            "additionalProperties": false
        }))
    );
}

#[tokio::test]
async fn legacy_messages_mapping_preserves_bounded_text_content_blocks() {
    let definition = LlmNodeDefinition::from_yaml(
        "id: answer\ntype: llm\noutput: [answer, messages]\ntransition: END\n",
    )
    .expect("legacy messages LLM node");
    let captured = Arc::new(Mutex::new(CapturedInvocation::default()));
    let factory: Arc<dyn PipelineLlmAgentFactory> = Arc::new(CapturingFactory {
        captured: Arc::clone(&captured),
        response: "ok".to_owned(),
    });
    let node = LlmNode::new(definition, state_types(), factory);
    let state = std::collections::HashMap::from([(
        "messages".to_owned(),
        json!([
            {"role":"assistant","content":[{"type":"text","text":"earlier"}]},
            {"role":"user","content":[
                {"type":"input_text","text":"Привіт, "},
                {"type":"text","text":"світ"}
            ]}
        ]),
    )]);
    node.execute(&NodeContext::new(
        state,
        ExecutionConfig::new("pipeline-thread"),
        0,
    ))
    .await
    .expect("text content-block LLM node");

    let captured = captured.lock().expect("capture lock");
    assert_eq!(captured.task, "Привіт, світ");
    assert_eq!(
        captured.history,
        [
            ("model".to_owned(), "earlier".to_owned()),
            ("user".to_owned(), "Привіт, світ".to_owned()),
        ]
    );
}

#[tokio::test]
async fn compiled_llm_graph_checkpoints_projected_state_and_returns_terminal_data() {
    let definition = PipelineDefinition::from_yaml(
        r#"
state:
  summary: str
  messages: list
entry_point: summarize
nodes:
  - id: summarize
    type: llm
    input_mapping:
      task:
        type: fstring
        value: "Summarize {input}"
      chat_history:
        type: fixed
        value: []
    input: [input]
    output: [summary, messages]
    structured_output: true
    transition: END
"#,
    )
    .expect("pipeline with LLM node");
    let captured = Arc::new(Mutex::new(CapturedInvocation::default()));
    let factory: Arc<dyn PipelineLlmAgentFactory> = Arc::new(CapturingFactory {
        captured: Arc::clone(&captured),
        response: r#"{"summary":"done"}"#.to_owned(),
    });
    let checkpointer = Arc::new(MemoryCheckpointer::new());
    let graph = definition
        .compile_with_llm_runtime("pipeline-root", checkpointer.clone(), None, Some(&factory))
        .expect("compiled LLM graph");
    let sessions = Arc::new(InMemorySessionService::new());
    sessions
        .create(CreateRequest {
            app_name: "elitea".to_owned(),
            user_id: "user-1".to_owned(),
            session_id: Some("llm-pipeline-thread".to_owned()),
            state: HashMap::new(),
        })
        .await
        .expect("pipeline session");
    let session_service: Arc<dyn SessionService> = sessions;
    let runner = Runner::builder()
        .app_name("elitea")
        .agent(Arc::new(EliteaGraphAgent::new(graph)))
        .session_service(session_service)
        .build()
        .expect("pipeline runner");
    let mut running = NativeAgentInvocation::new(
        runner,
        UserId::new("user-1").expect("fixture user"),
        SessionId::new("llm-pipeline-thread").expect("fixture session"),
        Content::new("user").with_text("this input"),
    )
    .start()
    .expect("LLM pipeline invocation");
    let mut events = Vec::new();
    while let Some(event) = running.next_event().await.expect("pipeline event") {
        events.push(event);
    }
    let final_text = events.last().and_then(Event::content).map(|content| {
        content
            .parts
            .iter()
            .filter_map(Part::text)
            .collect::<Vec<_>>()
            .join("")
    });
    assert_eq!(final_text.as_deref(), Some("done"));
    assert_eq!(
        captured.lock().expect("capture lock").task,
        "Summarize this input"
    );

    let checkpoint = checkpointer
        .load("llm-pipeline-thread")
        .await
        .expect("checkpoint load")
        .expect("LLM graph checkpoint");
    assert_eq!(checkpoint.state.get("summary"), Some(&json!("done")));
    assert_eq!(
        checkpoint.state.get("messages"),
        Some(&json!([
            {"role":"user","content":"this input"},
            {"role":"assistant","content":"{\"summary\":\"done\"}"}
        ]))
    );
}
