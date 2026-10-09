//! Compiled-pipeline tests for the runtime last-writer result trace.

use std::collections::HashMap;
use std::sync::Arc;

use adk_rust::graph::{
    Checkpointer, ExecutionConfig, GraphAgent, GraphError, MemoryCheckpointer, State,
};
use adk_rust::runner::Runner;
use adk_rust::session::{CreateRequest, InMemorySessionService, SessionService};
use adk_rust::{Content, Part, SessionId, UserId};
use async_trait::async_trait;
use serde_json::json;

use super::agent::EliteaGraphAgent;
use super::code_runtime::{CodeInvocation, CodeSandboxRuntime};
use super::compiler::{PipelineDefinition, PipelineNodeRuntimes};
use super::pipeline_result::PIPELINE_RESULT_TRACE_STATE_KEY;
use super::turn_checkpointer::TurnCheckpointer;
use crate::agents::runtime::NativeAgentInvocation;

const ROUTER_PIPELINE: &str = r#"
state:
  choice: {type: str, value: left}
  left_out: {type: str, value: ""}
  right_out: {type: str, value: "stale right default"}
entry_point: prepare
nodes:
  - id: prepare
    type: state_modifier
    template: ""
    transition: choose
  - id: choose
    type: router
    condition: "{{ choice }}"
    routes: [left, right]
    default_output: right
    input: [choice]
  - id: left
    type: state_modifier
    template: "left ran"
    output: [left_out]
    transition: END
  - id: right
    type: state_modifier
    template: "right ran"
    output: [right_out]
    transition: END
"#;

const CLEANUP_PIPELINE: &str = r#"
state:
  summary: {type: str, value: ""}
  scratch: {type: str, value: temp}
  zz_note: {type: str, value: "stale note"}
entry_point: write
nodes:
  - id: write
    type: state_modifier
    template: "the real summary"
    output: [summary]
    transition: tidy
  - id: tidy
    type: state_modifier
    template: ""
    variables_to_clean: [scratch]
    transition: END
"#;

const APP: &str = "elitea";
const ROOT: &str = "pipeline-root";
const USER: &str = "user-1";
const THREAD: &str = "pipeline-result-thread";

async fn sessions() -> Arc<InMemorySessionService> {
    let sessions = Arc::new(InMemorySessionService::new());
    sessions
        .create(CreateRequest {
            app_name: APP.to_owned(),
            user_id: USER.to_owned(),
            session_id: Some(THREAD.to_owned()),
            state: HashMap::new(),
        })
        .await
        .expect("pipeline session");
    sessions
}

/// Run one fresh turn and return the text of its terminal event.
async fn run_text(graph: GraphAgent, sessions: Arc<InMemorySessionService>, input: &str) -> String {
    let session_service: Arc<dyn SessionService> = sessions;
    let runner = Runner::builder()
        .app_name(APP)
        .agent(Arc::new(EliteaGraphAgent::new(graph)))
        .session_service(session_service)
        .build()
        .expect("pipeline runner");
    let mut running = NativeAgentInvocation::new(
        runner,
        UserId::new(USER).expect("fixture user"),
        SessionId::new(THREAD).expect("fixture session"),
        Content::new("user").with_text(input),
    )
    .start()
    .expect("pipeline invocation");
    let mut last = String::new();
    while let Some(event) = running.next_event().await.expect("pipeline event") {
        if let Some(content) = event.content() {
            for part in &content.parts {
                if let Part::Text { text } = part {
                    last.clone_from(text);
                }
            }
        }
    }
    last
}

async fn run_pure(yaml: &str, input: &str) -> String {
    let definition = PipelineDefinition::from_yaml(yaml).expect("pipeline definition");
    let graph = definition
        .compile(ROOT, Arc::new(MemoryCheckpointer::new()), None)
        .expect("pipeline graph");
    run_text(graph, sessions().await, input).await
}

#[tokio::test]
async fn terminal_list_of_records_is_shown_as_fenced_json() {
    let text = run_pure(
        r#"
state:
  records: {type: list, value: []}
entry_point: shape
nodes:
  - id: shape
    type: state_modifier
    template: '[{"id":"A","items":[10,20]}]'
    output: [records]
    transition: END
"#,
        "go",
    )
    .await;
    assert_eq!(
        text,
        "```json\n[\n  {\n    \"id\": \"A\",\n    \"items\": [\n      10,\n      20\n    ]\n  }\n]\n```"
    );
}

#[tokio::test]
async fn router_branch_that_ran_wins_over_a_later_declared_stale_default() {
    let text = run_pure(ROUTER_PIPELINE, "go").await;
    assert_eq!(text, "left ran");
}

#[tokio::test]
async fn cleaning_only_last_node_keeps_the_previous_real_writer() {
    let text = run_pure(CLEANUP_PIPELINE, "go").await;
    assert_eq!(text, "the real summary");
}

/// A nested saved pipeline compiles as a subgraph. Its terminal result node
/// projects the same trace-aware selection into `elitea_response`.
///
/// The fixture has one END edge. ADK defers a subgraph terminal node that two
/// unconditional END edges reach, which is a separate open defect.
#[tokio::test]
async fn a_nested_pipeline_subgraph_uses_the_same_result_trace() {
    let definition = PipelineDefinition::from_yaml(CLEANUP_PIPELINE).expect("nested pipeline");
    let graph = definition
        .compile_subgraph_with_runtime(
            Arc::new(MemoryCheckpointer::new()),
            &PipelineNodeRuntimes::default(),
        )
        .expect("nested graph");
    let state = graph
        .invoke(State::new(), ExecutionConfig::new("nested-result-thread"))
        .await
        .expect("nested run");
    assert_eq!(
        state.get("elitea_response"),
        Some(&json!("the real summary"))
    );
    assert_eq!(
        state.get(PIPELINE_RESULT_TRACE_STATE_KEY),
        Some(&json!({"node": "write", "keys": ["summary"], "messages": false}))
    );
}

struct StructuredResult;

#[async_trait]
impl CodeSandboxRuntime for StructuredResult {
    async fn execute(&self, _: CodeInvocation<'_>) -> Result<Vec<u8>, GraphError> {
        let stdout = json!({
            "revision": 1,
            "result": {"zeta": 3, "alpha": "two", "mid": [1, 2]}
        })
        .to_string();
        serde_json::to_vec(&json!({
            "revision": 1, "status": "completed", "exit_code": 0,
            "stdout": stdout, "stderr": ""
        }))
        .map_err(|_| GraphError::Other("fixture receipt".to_owned()))
    }
}

#[tokio::test]
async fn several_written_outputs_render_as_one_object_in_declared_order() {
    let definition = PipelineDefinition::from_yaml(
        r"
state:
  zeta: int
  alpha: str
  mid: list
entry_point: run
nodes:
  - id: run
    type: code
    code: 'result = 1'
    output: [zeta, alpha, mid]
    structured_output: true
    transition: END
",
    )
    .expect("code pipeline");
    let graph = definition
        .compile_with_runtime(
            ROOT,
            Arc::new(MemoryCheckpointer::new()),
            None,
            &PipelineNodeRuntimes::default().with_code(Arc::new(StructuredResult)),
        )
        .expect("code graph");
    let text = run_text(graph, sessions().await, "run").await;
    assert_eq!(
        text,
        "```json\n{\n  \"zeta\": 3,\n  \"alpha\": \"two\",\n  \"mid\": [\n    1,\n    2\n  ]\n}\n```"
    );
    let body = text
        .strip_prefix("```json\n")
        .and_then(|text| text.strip_suffix("\n```"))
        .expect("fenced JSON");
    let parsed: serde_json::Value = serde_json::from_str(body).expect("valid JSON object");
    assert_eq!(parsed, json!({"zeta": 3, "alpha": "two", "mid": [1, 2]}));
}

/// A fresh turn starts from defaults. The trace of an earlier turn is not
/// visible to it, even when both turns share one checkpointer.
#[tokio::test]
async fn a_fresh_turn_starts_without_the_previous_result_trace() {
    let definition = PipelineDefinition::from_yaml(
        r#"
state:
  input: {type: str}
  answer: {type: str, value: ""}
entry_point: choose
nodes:
  - id: choose
    type: router
    condition: "{{ input }}"
    routes: [write, END]
    default_output: END
    input: [input]
  - id: write
    type: state_modifier
    template: "written"
    output: [answer]
    transition: END
"#,
    )
    .expect("two-turn pipeline");
    let checkpointer: Arc<dyn Checkpointer> = Arc::new(MemoryCheckpointer::new());
    let sessions = sessions().await;
    let turn = |name: &'static str| {
        definition
            .compile(
                ROOT,
                Arc::new(TurnCheckpointer::new(
                    Arc::clone(&checkpointer),
                    name,
                    1,
                    false,
                )),
                None,
            )
            .expect("turn graph")
    };

    assert_eq!(
        run_text(turn("first"), sessions.clone(), "write").await,
        "written"
    );
    let first = checkpointer
        .load(THREAD)
        .await
        .expect("load")
        .expect("first");
    assert_eq!(
        first.state.get(PIPELINE_RESULT_TRACE_STATE_KEY),
        Some(&json!({"node": "write", "keys": ["answer"], "messages": false}))
    );

    run_text(turn("second"), sessions, "skip").await;
    let second = checkpointer
        .load(THREAD)
        .await
        .expect("load")
        .expect("second");
    assert_eq!(
        second.state.get(PIPELINE_RESULT_TRACE_STATE_KEY),
        Some(&serde_json::Value::Null)
    );
}

#[test]
fn the_result_trace_channel_is_reserved_and_does_not_change_the_definition_digest() {
    let base =
        "entry_point: run\nnodes:\n  - id: run\n    type: state_modifier\n    transition: END\n";
    let reserved = format!("state:\n  {PIPELINE_RESULT_TRACE_STATE_KEY}: dict\n{base}");
    let Err(error) = PipelineDefinition::from_yaml(&reserved) else {
        panic!("a user state declared the reserved result trace channel");
    };
    assert_eq!(error.code(), "graph.pipeline.invalid_configuration");

    // Golden digest of an unchanged fixture: runtime channels are not digested.
    let definition = PipelineDefinition::from_yaml(base).expect("fixture");
    assert_eq!(
        hex(&definition.definition_digest()),
        GOLDEN_FIXTURE_DIGEST,
        "a runtime-only channel changed the stored pipeline definition digest"
    );
}

// Recorded on origin/main 85cabcc8, before the result trace channel existed.
const GOLDEN_FIXTURE_DIGEST: &str =
    "b66dc3ea18d6b2ce1504d883ee8de9747b2a4a6225bcbec6ee0eed475658933c";

fn hex(bytes: &[u8]) -> String {
    use std::fmt::Write as _;
    bytes.iter().fold(String::new(), |mut output, byte| {
        let _ = write!(output, "{byte:02x}");
        output
    })
}
