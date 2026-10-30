//! Scoped application continuation is the only application path of a graph run
//! (Point 5 Wave 2a-1).
//!
//! Every test assembles through `PipelineNativeAgentAssembler::assemble`, so the real
//! `bind_node_runtimes`, the frozen composition admission and the real model-scope sessions are
//! under test; only the platform RPC and the model gateway are scripted. The platform fixture is
//! keyed by saved identity, not by call order, so a saved tree of any shape can be described.

use super::*;
use crate::agents::application_tools::ApplicationContinuation;
use crate::agents::application_tools::{
    MAX_APPLICATION_HOPS, MAX_MATERIALIZATION_VERSION_BYTES, bound, is_composition_bound,
};
use crate::agents::ordinary::OrdinaryNativeAgentAssembler;
use crate::agents::pipeline::scope_receipts::has_graph_call_receipts;
use crate::agents::pipeline::scoped_applications::MAX_ORDINARY_GRAPH_SCOPES;
use std::collections::BTreeMap;
use std::fmt::Write as _;

pub(crate) type Saved = (u64, u64);

pub(super) struct SavedVersions {
    versions: BTreeMap<Saved, Value>,
    reads: Arc<Mutex<Vec<Saved>>>,
    /// The project Main answers every read for (the claim's resource project).
    project_id: u64,
}

#[tonic::async_trait]
impl ContextRoutes for SavedVersions {
    async fn answer(&self, call: ContextCall) -> RuntimeReply {
        let unavailable = || tonic::Status::unavailable("saved version fixture");
        let value = if call.route == "elitea-client-token" {
            json!({"schema_version":"elitea.runtime.elitea-client-token.v1","project_id":self.project_id,
                "token":"ephemeral-scoped-token"})
        } else {
            // `applications/{a}/versions/{v}`
            let parts = call.route.split('/').collect::<Vec<_>>();
            let identity = parts
                .get(1)
                .and_then(|value| value.parse::<u64>().ok())
                .zip(parts.get(3).and_then(|value| value.parse::<u64>().ok()))
                .filter(|_| {
                    parts.len() == 4 && parts[0] == "applications" && parts[2] == "versions"
                })
                .ok_or_else(unavailable)?;
            self.reads.lock().map_err(|_| unavailable())?.push(identity);
            let details = self.versions.get(&identity).ok_or_else(unavailable)?;
            json!({"schema_version":"elitea.runtime.application-version.v1","project_id":self.project_id,
                "application_id":identity.0,"version_id":identity.1,"version_details":details})
        };
        runtime_response(&value)
    }
}

pub(crate) type SavedRuntime = (
    Arc<PlatformClient>,
    Arc<ModelFacade>,
    CapturedModelRequests,
    Arc<Mutex<Vec<Saved>>>,
);

pub(crate) fn saved_runtime(
    versions: &BTreeMap<Saved, Value>,
    outcomes: Vec<TestModelGatewayOutcome>,
) -> SavedRuntime {
    saved_runtime_for_project(versions, outcomes, 17)
}

/// `saved_runtime` for a claim of another resource project.
pub(crate) fn saved_runtime_for_project(
    versions: &BTreeMap<Saved, Value>,
    outcomes: Vec<TestModelGatewayOutcome>,
    project_id: u64,
) -> SavedRuntime {
    let reads = Arc::new(Mutex::new(Vec::new()));
    let runtime = RuntimeContextClient::with_rpc(
        Routes(SavedVersions {
            versions: versions.clone(),
            reads: Arc::clone(&reads),
            project_id,
        }),
        RuntimeContextConfig {
            origin: "https://content.internal".to_owned(),
            deadline: Duration::from_secs(1),
            max_response_bytes: 32 * 1_024,
            max_application_response_bytes: 1_024 * 1_024,
            max_attachment_response_bytes: 1_024 * 1_024,
            max_artifact_response_bytes: 2 * 1_024 * 1_024,
        },
    )
    .expect("saved-version runtime-context fixture");
    let (gateway, captured) = test_model_gateway_client(outcomes, test_model_gateway_config())
        .expect("scripted model gateway");
    (
        Arc::new(PlatformClient::new(Arc::new(runtime))),
        Arc::new(ModelFacade::from_gateway(gateway)),
        captured,
        reads,
    )
}

pub(crate) fn reference(identity: Saved, kind: &str, alias: &str) -> Value {
    json!({"id":identity.0,"type":"application","name":alias,"toolkit_name":alias,
        "description":"A saved application.","agent_type":kind,"author_id":11,
        "settings":{"application_id":identity.0,"application_version_id":identity.1},
        "meta":{},"variables":[],"is_pinned":false,"created_at":"2026-08-21T10:00:00Z"})
}

pub(crate) fn saved_version(
    kind: &str,
    instructions: &str,
    meta: &Value,
    tools: Vec<Value>,
) -> Value {
    json!({"agent_type":kind,"instructions":instructions,"meta":meta,"variables":[],
        "tools":Value::Array(tools),
        "llm_settings":{"model_name":"child-model","model_project_id":23,"max_tokens":2048,
            "reasoning_effort":null,"temperature":0.2,"openai_compatible":true}})
}

pub(crate) fn asking_agent() -> Value {
    saved_version(
        "agent",
        "Resolve the release.",
        &json!({"internal_tools":["ask_user"]}),
        Vec::new(),
    )
}

pub(crate) fn plain_agent(tools: Vec<Value>) -> Value {
    saved_version("agent", "Resolve the release.", &json!({}), tools)
}

/// One `agent` node delegating to `alias`; the task is fixed, as in a root pipeline.
pub(super) fn root_yaml(alias: &str) -> String {
    format!(
        "state:\n  answer: str\n  messages: list\nentry_point: delegate\nnodes:\n  - id: delegate\n    type: agent\n    tool: {alias:?}\n    input_mapping:\n      task: {{type: fixed, value: 'Summarize the release'}}\n    output: [answer, messages]\n    transition: END\n"
    )
}

/// The saved child pipeline: one `agent` node that hands its own task to `alias`.
pub(super) fn child_pipeline_yaml(alias: &str) -> String {
    format!(
        "state:\n  input: str\n  messages: list\n  answer: str\nentry_point: delegate\nnodes:\n  - id: delegate\n    type: agent\n    tool: {alias:?}\n    input_mapping:\n      task: {{type: variable, value: input}}\n    output: [answer]\n    transition: END\n"
    )
}

const PLAIN_GRAPH: &str = "state: {input: str, messages: list, answer: str}\nentry_point: report\nnodes:\n  - id: report\n    type: state_modifier\n    template: 'DONE {{ input }}'\n    input: [input]\n    output: [answer]\n    transition: END\n";
pub(crate) const STATIC_GRAPH: &str = "interrupt_after: [tick]\nstate: {input: str, messages: list, count: {type: int, value: 0}, answer: str}\nentry_point: tick\nnodes:\n  - id: tick\n    type: state_modifier\n    template: '{{ count + 1 }}'\n    input: [count]\n    output: [count]\n    transition: report\n  - id: report\n    type: state_modifier\n    template: 'COUNT {{ count }}'\n    input: [count]\n    output: [answer]\n    transition: END\n";

pub(super) fn root_request(
    yaml: &str,
    tools: Vec<Value>,
) -> super::super::request::AgentExecutionRequest {
    let mut request = pipeline_request();
    let version = request
        .payload
        .application
        .get_mut("version_details")
        .and_then(Value::as_object_mut)
        .expect("application version fixture");
    version.insert("instructions".to_owned(), json!(yaml));
    version.insert("tools".to_owned(), Value::Array(tools));
    request
}

pub(crate) fn tool_call_response(call_id: &str, name: &str, arguments: &Value) -> Response<Body> {
    let chunk = json!({"choices":[{"delta":{"tool_calls":[{"index":0,"id":call_id,"type":"function",
        "function":{"name":name,"arguments":arguments.to_string()}}]},"finish_reason":null}]});
    let raw = format!(
        "data: {chunk}\n\ndata: {{\"choices\":[{{\"delta\":{{}},\"finish_reason\":\"tool_calls\"}}]}}\n\ndata: {{\"choices\":[],\"usage\":{{\"prompt_tokens\":3,\"completion_tokens\":2}}}}\n\ndata: [DONE]\n\n"
    );
    test_model_gateway_response(Body::new(Full::<Bytes>::from(raw)))
}

pub(super) fn answer(
    mut request: super::super::request::AgentExecutionRequest,
    pending: &Value,
    digest: u8,
) -> super::super::request::AgentExecutionRequest {
    let answer = r#"{"q1":"Staging"}"#;
    request.binding.request_content_digest = [digest; 32];
    request.payload.should_continue = true;
    request.payload.hitl_resume = true;
    request.payload.hitl_action = Some("answer".to_owned());
    request.payload.hitl_value = Some(answer.to_owned());
    request.payload.hitl_decisions = vec![json!({
        "interrupt_id": pending["interrupt_id"], "tool_call_id": pending["tool_call_id"],
        "action": "answer", "value": answer
    })];
    request
}

fn model_calls(captured: &CapturedModelRequests) -> usize {
    captured.lock().expect("captured model requests").len()
}

fn offered_tools(captured: &CapturedModelRequests, request: usize) -> Vec<String> {
    let requests = captured.lock().expect("captured model requests");
    let body: Value = serde_json::from_slice(&requests[request].body).expect("model request JSON");
    body["tools"]
        .as_array()
        .map(|tools| {
            tools
                .iter()
                .filter_map(|tool| tool["function"]["name"].as_str().map(str::to_owned))
                .collect()
        })
        .unwrap_or_default()
}

fn state() -> (Arc<dyn SessionService>, Arc<MemoryCheckpointer>) {
    (
        Arc::new(InMemorySessionService::new()),
        Arc::new(MemoryCheckpointer::new()),
    )
}

/// A Worker process: fresh clients and assembler over the durable session and checkpoint stores.
fn process(
    sessions: &Arc<dyn SessionService>,
    checkpointer: &Arc<MemoryCheckpointer>,
    versions: &BTreeMap<Saved, Value>,
    outcomes: Vec<TestModelGatewayOutcome>,
) -> (
    PipelineNativeAgentAssembler,
    CapturedModelRequests,
    Arc<Mutex<Vec<Saved>>>,
) {
    let (platform, model_facade, captured, reads) = saved_runtime(versions, outcomes);
    (
        PipelineNativeAgentAssembler::with_state(
            Arc::clone(sessions),
            Arc::clone(checkpointer) as Arc<dyn Checkpointer>,
        )
        .with_runtime_clients(platform, model_facade),
        captured,
        reads,
    )
}

pub(super) fn text(value: &str) -> TestModelGatewayOutcome {
    TestModelGatewayOutcome::Response(pipeline_text_response(value))
}

#[tokio::test(flavor = "current_thread")]
async fn a_graph_agent_node_runs_on_the_scoped_resolver_and_owns_original_call_receipts() {
    let (sessions, checkpointer) = state();
    let versions = BTreeMap::from([((5, 6), plain_agent(Vec::new()))]);
    let (assembler, captured, _) = process(
        &sessions,
        &checkpointer,
        &versions,
        vec![text("worker answer")],
    );
    let request = root_request(
        &root_yaml("worker"),
        vec![reference((5, 6), "agent", "worker")],
    );
    let thread = private_pipeline_session_id(&request);
    let invocation = assembler
        .assemble(authorized(&request))
        .await
        .expect("scoped graph Agent node assembles");
    let browser = collect_pipeline_completion(invocation).await;
    assert!(
        browser
            .iter()
            .any(|event| event["content"] == "worker answer"),
        "{browser:?}"
    );
    assert_eq!(model_calls(&captured), 1);
    let checkpoint = checkpointer
        .load(&thread)
        .await
        .expect("checkpoint lookup")
        .expect("root checkpoint");
    assert!(
        has_graph_call_receipts(&checkpoint).expect("receipt metadata decodes"),
        "a graph Agent call must own an original-call receipt in the root checkpoint"
    );
}

/// REVIEW-1541 R5: a child re-entered at its own paused frontier keeps the
/// task message it already holds; the resume does not append it again.
async fn assert_child_task_once(
    checkpointer: &MemoryCheckpointer,
    request: &super::super::request::AgentExecutionRequest,
) {
    let child = checkpointer
        .load(&format!(
            "{}/delegate",
            private_pipeline_session_id(request)
        ))
        .await
        .expect("read the child checkpoint")
        .expect("the child saved its rows");
    let tasks = child
        .state
        .get("messages")
        .and_then(Value::as_array)
        .expect("the child's messages")
        .iter()
        .filter(|message| message["content"] == "Summarize the release")
        .count();
    assert_eq!(tasks, 1, "the child's task message once: {:?}", child.state);
}

#[tokio::test(flavor = "current_thread")]
async fn an_ordinary_agent_in_a_saved_child_pipeline_asks_and_resumes_once_after_process_replacement()
 {
    let (sessions, checkpointer) = state();
    let versions = BTreeMap::from([
        (
            (3, 4),
            saved_version(
                "pipeline",
                &child_pipeline_yaml("worker"),
                &json!({}),
                vec![reference((5, 6), "agent", "worker")],
            ),
        ),
        ((5, 6), asking_agent()),
    ]);
    let make_request = || {
        root_request(
            &root_yaml("child-flow"),
            vec![reference((3, 4), "pipeline", "child-flow")],
        )
    };
    let request = make_request();
    let (assembler, captured, _) = process(
        &sessions,
        &checkpointer,
        &versions,
        vec![TestModelGatewayOutcome::Response(
            pipeline_ask_user_call_response(),
        )],
    );
    let invocation = assembler
        .assemble(authorized(&request))
        .await
        .expect("a saved child pipeline with an ordinary Agent assembles");
    let (browser, _) = collect_pipeline_pause_events(invocation).await;
    let cards = browser
        .iter()
        .filter(|event| event["type"] == "agent_hitl_interrupt")
        .collect::<Vec<_>>();
    assert_eq!(cards.len(), 1, "one ask_user card: {browser:?}");
    let pending = cards[0]["response_metadata"]["hitl_interrupts"][0].clone();
    let path = cards[0]["response_metadata"]["parent_agent_path"]
        .as_array()
        .expect("nested card path");
    assert_eq!(
        path.len(),
        2,
        "child pipeline then ordinary Agent: {path:?}"
    );
    assert_eq!(path[0]["name"], "child-flow");
    assert_eq!(path[1]["name"], "worker");
    assert_eq!(model_calls(&captured), 1);
    drop(assembler);

    // A replacement Worker: new clients and assembler over the same durable state.
    let (replacement, replaced_calls, _) = process(
        &sessions,
        &checkpointer,
        &versions,
        vec![text("clarified nested answer")],
    );
    let resume = answer(make_request(), &pending, 21);
    let invocation = replacement
        .assemble(authorized(&resume))
        .await
        .expect("the replacement Worker resumes the nested card");
    let resumed = collect_pipeline_completion(invocation).await;
    assert!(
        resumed
            .iter()
            .any(|event| event["content"] == "clarified nested answer"),
        "{resumed:?}"
    );
    assert_eq!(
        model_calls(&replaced_calls),
        1,
        "only the resumed Agent calls the model"
    );
    assert_child_task_once(&checkpointer, &make_request()).await;

    // The consumed card cannot be answered a second time: no model call, no second answer.
    let (again, again_calls, _) = process(
        &sessions,
        &checkpointer,
        &versions,
        vec![text("unexpected")],
    );
    let repeated = answer(make_request(), &pending, 22);
    let outcome = again.assemble(authorized(&repeated)).await;
    if let Ok(invocation) = outcome {
        let _ = tokio::time::timeout(
            Duration::from_secs(5),
            collect_pipeline_completion(invocation),
        )
        .await;
    }
    assert_eq!(
        model_calls(&again_calls),
        0,
        "an answered card must not resume the nested Agent again"
    );
}

#[tokio::test(flavor = "current_thread")]
async fn a_nested_agent_offers_its_saved_pipeline_child_as_a_tool_and_runs_it() {
    let (sessions, checkpointer) = state();
    let versions = BTreeMap::from([
        (
            (5, 6),
            plain_agent(vec![reference((7, 8), "pipeline", "inner")]),
        ),
        (
            (7, 8),
            saved_version("pipeline", PLAIN_GRAPH, &json!({}), Vec::new()),
        ),
    ]);
    let (assembler, captured, _) = process(
        &sessions,
        &checkpointer,
        &versions,
        vec![
            TestModelGatewayOutcome::Response(tool_call_response(
                "inner-call",
                "elitea_agent_7_v_8",
                &json!({"task":"inner work"}),
            )),
            text("worker used inner"),
        ],
    );
    let request = root_request(
        &root_yaml("worker"),
        vec![reference((5, 6), "agent", "worker")],
    );
    let invocation = assembler
        .assemble(authorized(&request))
        .await
        .expect("an ordinary Agent with a saved pipeline attachment assembles");
    let browser = collect_pipeline_completion(invocation).await;
    assert!(
        offered_tools(&captured, 0).contains(&"elitea_agent_7_v_8".to_owned()),
        "the nested Agent's saved pipeline must be offered as a tool: {:?}",
        offered_tools(&captured, 0)
    );
    assert!(
        browser
            .iter()
            .any(|event| event["content"] == "worker used inner"),
        "{browser:?}"
    );
    let requests = captured.lock().expect("captured model requests");
    assert_eq!(requests.len(), 2);
    let second: Value = serde_json::from_slice(&requests[1].body).expect("second request");
    assert!(
        second["messages"].to_string().contains("DONE inner work"),
        "the pipeline child's result reaches the nested Agent: {second}"
    );
}

pub(crate) fn ordinary_agent_request(
    tools: Vec<Value>,
) -> super::super::request::AgentExecutionRequest {
    let mut request = ordinary_request(AgentExecutionKind::Application);
    request.payload.application["version_details"]["tools"] = Value::Array(tools);
    request
}

/// One ordinary-root turn: the projected browser events and whether the run is paused.
async fn run_ordinary(
    assembler: &OrdinaryNativeAgentAssembler,
    request: &super::super::request::AgentExecutionRequest,
) -> (Vec<Value>, bool) {
    run_ordinary_authorized(assembler, authorized(request)).await
}

/// One ordinary-root turn under the given authority, ending as the lifecycle
/// does: a pause stops it; otherwise the completion is selected (a static tool
/// pause completes with its inventory) and projected.
pub(super) async fn run_ordinary_authorized(
    assembler: &OrdinaryNativeAgentAssembler,
    authority: AuthorizedNativeAssembly<'_>,
) -> (Vec<Value>, bool) {
    let mut invocation = assembler
        .assemble(authority)
        .await
        .expect("ordinary Agent assembles");
    invocation
        .project_start(timestamp(0))
        .expect("browser start");
    let (mut run, mut projector, completion) = invocation.start().expect("ordinary start");
    let mut browser = Vec::new();
    while let Some(event) = run.next_event().await.expect("ordinary event") {
        browser.extend(
            projector
                .project(&event)
                .expect("ordinary projection")
                .into_iter()
                .map(|event| current(&event)),
        );
    }
    let paused = projector.is_paused();
    if paused {
        return (browser, true);
    }
    let selected = if projector.has_static_tool_pause() {
        completion.select_static_pause().await
    } else {
        completion.select().await
    }
    .expect("ordinary completion");
    browser.extend(
        projector
            .finish_after_eos(selected, timestamp(1))
            .expect("ordinary browser completion")
            .into_iter()
            .map(|event| current(&event)),
    );
    (browser, paused)
}

fn ordinary_assembler(
    sessions: &Arc<dyn SessionService>,
    versions: &BTreeMap<Saved, Value>,
    outcomes: Vec<TestModelGatewayOutcome>,
) -> (OrdinaryNativeAgentAssembler, CapturedModelRequests) {
    let (platform, model_facade, captured, _) = saved_runtime(versions, outcomes);
    (
        OrdinaryNativeAgentAssembler::new(
            platform,
            model_facade,
            Arc::new(ToolAdmissionPolicy::new(&[], &BTreeMap::new()).expect("empty tool policy")),
        )
        .with_sessions(Arc::clone(sessions)),
        captured,
    )
}

#[tokio::test(flavor = "current_thread")]
async fn an_ordinary_agent_offers_a_static_saved_pipeline_and_continues_it_once() {
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let versions = BTreeMap::from([(
        (7, 8),
        saved_version("pipeline", STATIC_GRAPH, &json!({}), Vec::new()),
    )]);
    let tools = vec![reference((7, 8), "pipeline", "inner")];
    let (assembler, captured) = ordinary_assembler(
        &sessions,
        &versions,
        vec![TestModelGatewayOutcome::Response(tool_call_response(
            "inner-call",
            "elitea_agent_7_v_8",
            &json!({"task":"inner work"}),
        ))],
    );
    let request = ordinary_agent_request(tools.clone());
    let (browser, paused) = run_ordinary(&assembler, &request).await;
    // The static pause ends the turn as a completed result that carries the
    // inventory, the terminal Main stores and continues from: never a pause
    // the lifecycle has no card for.
    assert!(!paused, "the static pause completes the turn: {browser:?}");
    assert!(
        offered_tools(&captured, 0).contains(&"elitea_agent_7_v_8".to_owned()),
        "a static saved pipeline must be offered as a tool: {:?}",
        offered_tools(&captured, 0)
    );
    let carriers = browser
        .iter()
        .filter(|event| !event["response_metadata"]["pipeline_static_tools_v1"].is_null())
        .collect::<Vec<_>>();
    assert_eq!(
        carriers.len(),
        1,
        "one result carries the inventory: {browser:?}"
    );
    let terminal = browser.last().expect("a completed result");
    assert_eq!(terminal["type"], "full_message", "{browser:?}");
    assert_eq!(
        carriers[0], terminal,
        "the inventory rides the terminal message"
    );
    assert!(
        terminal["response_metadata"]
            .get("hitl_interrupts")
            .is_none(),
        "{terminal:?}"
    );
    let inventory = terminal["response_metadata"]["pipeline_static_tools_v1"]["pauses"]
        .as_array()
        .cloned()
        .unwrap_or_else(|| panic!("static pause inventory missing: {terminal:?}"));
    assert_eq!(inventory.len(), 1);

    // A replacement Worker continues the pause once.
    let (replacement, replaced_calls) = ordinary_assembler(
        &sessions,
        &versions,
        vec![text("agent continued after tick")],
    );
    let mut resume = ordinary_agent_request(tools);
    resume.binding.request_content_digest = [23; 32];
    resume.payload.should_continue = true;
    resume.payload.meta.insert(
        "pipeline_static_tool_resume_v1".to_owned(),
        json!({"revision":1,"decisions":[{
            "pause_id":inventory[0]["proof"]["pause_id"],
            "child_thread_id":inventory[0]["child_thread_id"],
            "tool_call_id":inventory[0]["tool_call_id"],
            "action":"continue","value":"finish once"}]}),
    );
    let (resumed, paused) = run_ordinary(&replacement, &resume).await;
    assert!(!paused, "{resumed:?}");
    assert!(
        resumed
            .iter()
            .any(|event| event["content"] == "agent continued after tick"),
        "{resumed:?}"
    );
    let requests = replaced_calls.lock().expect("captured model requests");
    assert_eq!(requests.len(), 1);
    let body = String::from_utf8_lossy(&requests[0].body).into_owned();
    assert!(
        body.contains("COUNT 1"),
        "the static child resumes after its tick: {body}"
    );
    assert!(
        !body.contains("COUNT 2"),
        "the tick is not replayed: {body}"
    );
}

/// A pipeline attachment of a NESTED Agent that this Worker cannot build (its
/// `agent` node names a tool the child does not attach) degrades the way a root
/// attachment does (#990): skipped and named, and the root Agent still answers.
/// It never fails every turn of the root.
#[tokio::test(flavor = "current_thread")]
async fn an_unbuildable_pipeline_of_a_nested_agent_is_skipped_and_the_root_still_answers() {
    const UNBUILDABLE: &str = "state:\n  answer: str\n  messages: list\nentry_point: delegate\nnodes:\n  - id: delegate\n    type: agent\n    tool: \"missing\"\n    input_mapping:\n      task: {type: fixed, value: 'Summarize'}\n    output: [answer, messages]\n    transition: END\n";
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let versions = BTreeMap::from([
        (
            (5, 6),
            plain_agent(vec![reference((7, 8), "pipeline", "broken-flow")]),
        ),
        (
            (7, 8),
            saved_version("pipeline", UNBUILDABLE, &json!({}), Vec::new()),
        ),
    ]);
    let (assembler, captured) = ordinary_assembler(&sessions, &versions, vec![text("root answer")]);
    let request = ordinary_agent_request(vec![reference((5, 6), "agent", "worker")]);
    let (browser, paused) = run_ordinary(&assembler, &request).await;
    assert!(!paused, "{browser:?}");
    assert!(
        browser
            .iter()
            .any(|event| event["content"] == "root answer"),
        "the root Agent answers: {browser:?}"
    );
    assert_eq!(model_calls(&captured), 1);
    let rendered = serde_json::to_string(&browser).expect("browser events");
    assert!(
        rendered.contains("broken-flow"),
        "the skipped nested attachment is named: {rendered}"
    );
}

/// The pipeline-root twin: a graph `agent` node whose Agent has one
/// unbuildable pipeline attachment still runs; the attachment is not offered.
#[tokio::test(flavor = "current_thread")]
async fn an_unbuildable_attachment_of_a_pipeline_agent_node_is_skipped_and_the_node_answers() {
    const UNBUILDABLE: &str = "state:\n  answer: str\n  messages: list\nentry_point: delegate\nnodes:\n  - id: delegate\n    type: agent\n    tool: \"missing\"\n    input_mapping:\n      task: {type: fixed, value: 'Summarize'}\n    output: [answer, messages]\n    transition: END\n";
    let (sessions, checkpointer) = state();
    let versions = BTreeMap::from([
        (
            (5, 6),
            plain_agent(vec![reference((7, 8), "pipeline", "broken-flow")]),
        ),
        (
            (7, 8),
            saved_version("pipeline", UNBUILDABLE, &json!({}), Vec::new()),
        ),
    ]);
    let (assembler, captured, _) = process(
        &sessions,
        &checkpointer,
        &versions,
        vec![text("node answer")],
    );
    let request = root_request(
        &root_yaml("worker"),
        vec![reference((5, 6), "agent", "worker")],
    );
    let invocation = assembler
        .assemble(authorized(&request))
        .await
        .expect("an unbuildable nested attachment never fails the pipeline at assembly");
    let browser = collect_pipeline_completion(invocation).await;
    assert!(
        browser
            .iter()
            .any(|event| event["content"] == "node answer"),
        "the Agent node answers: {browser:?}"
    );
    assert_eq!(model_calls(&captured), 1);
    assert!(
        !offered_tools(&captured, 0)
            .iter()
            .any(|tool| tool.starts_with("elitea_agent_7")),
        "the unbuildable attachment is not offered: {:?}",
        offered_tools(&captured, 0)
    );
    // FU-243: a pipeline root names the skipped attachment like an ordinary root does.
    let rendered = serde_json::to_string(&browser).expect("browser events");
    assert!(
        rendered.contains("skipped_capabilities") && rendered.contains("broken-flow"),
        "the skipped attachment is named on the pipeline root: {rendered}"
    );
}

/// FU-244: a static-pausing saved pipeline attached to a graph `agent` node's Agent cannot be
/// continued beneath a graph root (one checkpoint owner). That is an authoring error of the
/// composition, so the run is refused with the typed composition cause before any model call;
/// it is never silently dropped.
#[tokio::test(flavor = "current_thread")]
async fn a_static_saved_pipeline_tool_under_a_graph_root_is_refused_before_any_model_call() {
    let (sessions, checkpointer) = state();
    let versions = BTreeMap::from([
        (
            (5, 6),
            plain_agent(vec![reference((7, 8), "pipeline", "inner")]),
        ),
        (
            (7, 8),
            saved_version("pipeline", STATIC_GRAPH, &json!({}), Vec::new()),
        ),
    ]);
    let (assembler, captured, _) = process(&sessions, &checkpointer, &versions, Vec::new());
    let request = root_request(
        &root_yaml("worker"),
        vec![reference((5, 6), "agent", "worker")],
    );
    let Err(error) = assembler.assemble(authorized(&request)).await else {
        panic!("a static pause beneath a graph root must be refused");
    };
    assert_composition_bound(&error, bound::STATIC_PAUSE_UNDER_GRAPH_ROOT);
    assert_eq!(model_calls(&captured), 0);
}

/// A saved pipeline whose root is a Parallel of two Agent nodes running `helper`.
const PARALLEL_GRAPH: &str = "state:\n  input: str\n  messages: list\n  b1: str\n  b2: str\n  joined: list\n  answer: str\nentry_point: gather\nnodes:\n  - id: gather\n    type: parallel\n    branches: [{id: one, node: ask_one}, {id: two, node: ask_two}]\n    max_concurrency: 2\n    wait: all\n    output: [joined]\n    transition: END\n  - id: ask_one\n    type: agent\n    tool: helper\n    input_mapping:\n      task: {type: fixed, value: 'one'}\n    output: [b1]\n  - id: ask_two\n    type: agent\n    tool: helper\n    input_mapping:\n      task: {type: fixed, value: 'two'}\n    output: [b2]\n";

fn parallel_pipeline() -> Value {
    saved_version(
        "pipeline",
        PARALLEL_GRAPH,
        &json!({}),
        vec![reference((9, 10), "agent", "helper")],
    )
}

fn assert_parallel_in_child_refused(error: &super::super::runtime::NativeAgentAssemblyError) {
    let cause = error.cause().expect("a typed cause");
    assert_eq!(
        cause.code(),
        "graph.fanout.parallel_in_child_pipeline",
        "{error:?}"
    );
    // The lifecycle maps every fan-out child refusal to the registered message
    // (`a_parallel_in_a_saved_child_pipeline_ends_as_the_registered_deployment_message`).
    assert!(crate::agents::graph::compiler::FanoutChildRefusal::is_code(
        cause.code()
    ));
}

/// FU-249: the same pipeline as an Agent's tool is refused typed at assembly, never skipped
/// and never failing only when the tool is called.
#[tokio::test(flavor = "current_thread")]
async fn a_parallel_pipeline_tool_of_an_agent_node_is_refused_typed_before_any_model_call() {
    let (sessions, checkpointer) = state();
    let versions = BTreeMap::from([
        (
            (5, 6),
            plain_agent(vec![reference((7, 8), "pipeline", "inner")]),
        ),
        ((7, 8), parallel_pipeline()),
        ((9, 10), plain_agent(Vec::new())),
    ]);
    let (assembler, captured, _) = process(&sessions, &checkpointer, &versions, Vec::new());
    let request = root_request(
        &root_yaml("worker"),
        vec![reference((5, 6), "agent", "worker")],
    );
    let Err(error) = assembler.assemble(authorized(&request)).await else {
        panic!("a Parallel pipeline tool beneath a graph root must be refused");
    };
    assert_parallel_in_child_refused(&error);
    assert_eq!(model_calls(&captured), 0);
}

fn assert_composition_bound(error: &super::super::runtime::NativeAgentAssemblyError, detail: &str) {
    assert!(is_composition_bound(error), "{error:?}");
    assert_eq!(
        error
            .cause()
            .and_then(elitea_agent_runtime::assembly_error::NativeAgentAssemblyCause::detail),
        Some(detail),
        "{error:?}"
    );
}

/// A root with one agent node per alias, in a row.
fn chain_yaml(aliases: &[String]) -> String {
    let nodes = aliases.len();
    let mut yaml = String::from("state:\n  answer: str\n  messages: list\n");
    yaml.push_str("entry_point: n0\nnodes:\n");
    for (index, alias) in aliases.iter().enumerate() {
        let next = if index + 1 == nodes {
            "END".to_owned()
        } else {
            format!("n{}", index + 1)
        };
        let _ = write!(
            yaml,
            "  - id: n{index}\n    type: agent\n    tool: {alias:?}\n    input_mapping:\n      task: {{type: fixed, value: 'Summarize the release'}}\n    output: [answer, messages]\n    transition: {next}\n"
        );
    }
    yaml
}

/// The assembly result and how many saved versions the platform was asked for.
async fn assemble_chain(
    nodes: usize,
) -> (
    Result<(), super::super::runtime::NativeAgentAssemblyError>,
    usize,
) {
    let (sessions, checkpointer) = state();
    let versions = BTreeMap::from([((5, 6), plain_agent(Vec::new()))]);
    let (assembler, _, reads) = process(&sessions, &checkpointer, &versions, Vec::new());
    let aliases = vec!["worker".to_owned(); nodes];
    let request = root_request(
        &chain_yaml(&aliases),
        vec![reference((5, 6), "agent", "worker")],
    );
    let result = assembler.assemble(authorized(&request)).await.map(|_| ());
    let reads = reads.lock().expect("version reads").len();
    (result, reads)
}

#[tokio::test(flavor = "current_thread")]
async fn the_application_hop_budget_admits_the_limit_and_refuses_one_more() {
    let (result, reads) = assemble_chain(MAX_APPLICATION_HOPS).await;
    result.expect("exactly the hop budget assembles");
    // The performance budget of the scoped resolver: one version read per application node.
    assert_eq!(reads, MAX_APPLICATION_HOPS);
    let (result, reads) = assemble_chain(MAX_APPLICATION_HOPS + 1).await;
    let Err(error) = result else {
        panic!("one hop past the budget must be refused before any model call");
    };
    assert_eq!(
        reads, MAX_APPLICATION_HOPS,
        "the refused node reads nothing more"
    );
    assert_eq!(
        error.code(),
        NativeAgentAssemblyErrorCode::ResourceExhausted
    );
    assert_composition_bound(&error, bound::APPLICATION_HOPS);
}

/// A saved Agent whose serialized version is exactly `bytes` long.
fn agent_version_of_exactly(bytes: usize) -> Value {
    let mut version = plain_agent(Vec::new());
    version["meta"] = json!({"padding": ""});
    let base = serde_json::to_vec(&version).expect("version JSON").len();
    version["meta"] = json!({"padding": "x".repeat(bytes - base)});
    let actual = serde_json::to_vec(&version)
        .expect("padded version JSON")
        .len();
    assert_eq!(actual, bytes, "fixture padding is exact");
    version
}

/// The transport admits one version up to 1 MiB, so the 4 MiB budget is reached by five saved
/// Agents whose versions add up to exactly `total` bytes.
async fn assemble_versions_totalling(
    total: usize,
) -> Result<(), super::super::runtime::NativeAgentAssemblyError> {
    const EACH: usize = 800_000;
    let sizes = [EACH, EACH, EACH, EACH, total - 4 * EACH];
    let (sessions, checkpointer) = state();
    let mut versions = BTreeMap::new();
    let mut tools = Vec::new();
    let mut aliases = Vec::new();
    for (index, size) in sizes.iter().enumerate() {
        let identity = (5 + index as u64, 6 + index as u64);
        let alias = format!("worker{index}");
        versions.insert(identity, agent_version_of_exactly(*size));
        tools.push(reference(identity, "agent", &alias));
        aliases.push(alias);
    }
    let (assembler, _, _) = process(&sessions, &checkpointer, &versions, Vec::new());
    let request = root_request(&chain_yaml(&aliases), tools);
    assembler.assemble(authorized(&request)).await.map(|_| ())
}

#[tokio::test(flavor = "current_thread")]
async fn the_version_byte_budget_admits_four_mib_and_refuses_one_more_byte_at_a_pipeline_root() {
    assemble_versions_totalling(MAX_MATERIALIZATION_VERSION_BYTES)
        .await
        .expect("exactly 4 MiB of saved versions assembles");
    let Err(error) = assemble_versions_totalling(MAX_MATERIALIZATION_VERSION_BYTES + 1).await
    else {
        panic!("one byte past the budget must be refused");
    };
    assert_eq!(
        error.code(),
        NativeAgentAssemblyErrorCode::ResourceExhausted
    );
    // Main cannot measure the loaded documents, so this is its own terminal, not a bound.
    assert!(!is_composition_bound(&error), "{error:?}");
    assert!(
        crate::agents::application_tools::is_composition_size(&error),
        "{error:?}"
    );
}

/// A root whose chain reaches `scopes` ordinary graph scopes: each scope is a saved pipeline whose
/// `agent` node calls an ordinary Agent, which in turn carries the next saved pipeline as a tool.
fn scope_chain(scopes: usize) -> (BTreeMap<Saved, Value>, Vec<Value>) {
    let mut versions = BTreeMap::new();
    for scope in 0..scopes {
        let pipeline = (10 + scope as u64 * 2, 11 + scope as u64 * 2);
        let agent = (pipeline.0 + 1, pipeline.1 + 1);
        let next_pipeline = (pipeline.0 + 2, pipeline.1 + 2);
        versions.insert(
            pipeline,
            saved_version(
                "pipeline",
                &child_pipeline_yaml("worker"),
                &json!({}),
                vec![reference(agent, "agent", "worker")],
            ),
        );
        let attachments = if scope + 1 < scopes {
            vec![reference(next_pipeline, "pipeline", "next")]
        } else {
            Vec::new()
        };
        versions.insert(agent, plain_agent(attachments));
    }
    (versions, vec![reference((10, 11), "pipeline", "first")])
}

/// The assembly result and the saved versions the platform was asked for.
async fn assemble_scopes(
    scopes: usize,
) -> (
    Result<(), super::super::runtime::NativeAgentAssemblyError>,
    Vec<Saved>,
) {
    let (sessions, checkpointer) = state();
    let (versions, tools) = scope_chain(scopes);
    let (assembler, _, reads) = process(&sessions, &checkpointer, &versions, Vec::new());
    let request = root_request(&root_yaml("first"), tools);
    let result = assembler.assemble(authorized(&request)).await.map(|_| ());
    let reads = reads.lock().expect("version reads").clone();
    (result, reads)
}

/// FU-244: one ordinary graph scope past the budget is refused with the typed composition
/// cause at assembly; the Agent past the budget is never read.
#[tokio::test(flavor = "current_thread")]
async fn the_ordinary_graph_scope_budget_admits_the_limit_and_refuses_one_more() {
    let (result, reads) = assemble_scopes(MAX_ORDINARY_GRAPH_SCOPES).await;
    result.expect("exactly the scope budget assembles");
    let last = MAX_ORDINARY_GRAPH_SCOPES as u64 - 1;
    assert!(
        reads.contains(&(11 + last * 2, 12 + last * 2)),
        "the last Agent within the budget is read: {reads:?}"
    );
    let (result, reads) = assemble_scopes(MAX_ORDINARY_GRAPH_SCOPES + 1).await;
    let Err(error) = result else {
        panic!("one scope past the budget must be refused");
    };
    assert_composition_bound(&error, bound::ORDINARY_GRAPH_SCOPES);
    let past = MAX_ORDINARY_GRAPH_SCOPES as u64;
    let beyond_agent = (11 + past * 2, 12 + past * 2);
    assert!(
        !reads.contains(&beyond_agent),
        "the Agent past the scope budget is never read: {reads:?}"
    );
}

/// The ad hoc reference shape, the only one that names a project.
fn foreign_reference(identity: Saved, kind: &str, alias: &str) -> Value {
    json!({"id":null,"type":"application","name":alias,"toolkit_name":alias,
        "description":"A saved application.","author_id":11,"created_at":"2026-08-21T10:00:00Z",
        "settings":{"application_id":identity.0,"application_version_id":identity.1,
            "variables":[],"selected_tools":[]},
        "project_id":99,"participant_id":7,"agent_type":kind})
}

#[tokio::test(flavor = "current_thread")]
async fn a_saved_participant_from_a_foreign_project_is_refused_before_any_effect() {
    for (kind, versions) in [
        (
            "pipeline",
            BTreeMap::from([(
                (3, 4),
                saved_version("pipeline", PLAIN_GRAPH, &json!({}), Vec::new()),
            )]),
        ),
        ("agent", BTreeMap::from([((3, 4), plain_agent(Vec::new()))])),
    ] {
        let (sessions, checkpointer) = state();
        let (assembler, captured, reads) =
            process(&sessions, &checkpointer, &versions, vec![text("never")]);
        let request = root_request(
            &root_yaml("foreign"),
            vec![foreign_reference((3, 4), kind, "foreign")],
        );
        let Err(error) = assembler.assemble(authorized(&request)).await else {
            panic!("{kind}: a saved participant of another project must be refused");
        };
        assert!(
            matches!(
                error.code(),
                NativeAgentAssemblyErrorCode::InvalidInput
                    | NativeAgentAssemblyErrorCode::InvalidConfiguration
            ),
            "{kind}: {:?}",
            error.code()
        );
        assert_eq!(
            model_calls(&captured),
            0,
            "{kind}: refusal precedes every model effect"
        );
        assert!(
            reads.lock().expect("reads").is_empty(),
            "{kind}: the foreign version is never fetched"
        );
    }
}

fn approve(
    mut request: super::super::request::AgentExecutionRequest,
    interrupt_id: &Value,
    digest: u8,
) -> super::super::request::AgentExecutionRequest {
    request.binding.request_content_digest = [digest; 32];
    request.payload.should_continue = true;
    request.payload.hitl_resume = true;
    request.payload.hitl_action = Some("approve".to_owned());
    request.payload.hitl_value = Some(String::new());
    request.payload.hitl_decisions = vec![json!({
        "interrupt_id": interrupt_id, "tool_call_id": "", "action": "approve", "value": ""
    })];
    request
}

fn first_card(browser: &[Value]) -> Value {
    browser
        .iter()
        .find(|event| event["type"] == "agent_hitl_interrupt")
        .unwrap_or_else(|| panic!("a pending card is expected: {browser:?}"))["response_metadata"]
        ["hitl_interrupts"][0]
        .clone()
}

const REVIEW_THEN_AGENT: &str = "state:\n  answer: str\n  messages: list\nentry_point: review\nnodes:\n  - id: review\n    type: hitl\n    user_message:\n      type: fixed\n      value: Review the draft.\n    routes:\n      approve: delegate\n      reject: END\n  - id: delegate\n    type: agent\n    tool: \"worker\"\n    input_mapping:\n      task: {type: fixed, value: 'Summarize the release'}\n    output: [answer, messages]\n    transition: END\n";

#[tokio::test(flavor = "current_thread")]
async fn a_new_conversation_paused_before_its_first_agent_call_continues_scoped() {
    let (sessions, checkpointer) = state();
    let versions = BTreeMap::from([((5, 6), plain_agent(Vec::new()))]);
    let make_request = || {
        root_request(
            REVIEW_THEN_AGENT,
            vec![reference((5, 6), "agent", "worker")],
        )
    };
    let request = make_request();
    let thread = private_pipeline_session_id(&request);
    let (assembler, _, _) = process(&sessions, &checkpointer, &versions, Vec::new());
    let invocation = assembler
        .assemble(authorized(&request))
        .await
        .expect("review-then-agent pipeline assembles");
    let (browser, _) = collect_pipeline_pause_events(invocation).await;
    let card = first_card(&browser);
    let paused = checkpointer
        .load(&thread)
        .await
        .expect("checkpoint lookup")
        .expect("paused checkpoint");
    assert!(
        !has_graph_call_receipts(&paused).expect("receipt metadata decodes"),
        "no application call has started yet, so no receipt exists"
    );

    let (replacement, calls, _) = process(
        &sessions,
        &checkpointer,
        &versions,
        vec![text("worker after review")],
    );
    let resume = approve(make_request(), &card["interrupt_id"], 31);
    let invocation = replacement
        .assemble(authorized(&resume))
        .await
        .expect("the review decision resumes the pipeline");
    let resumed = collect_pipeline_completion(invocation).await;
    assert!(
        resumed
            .iter()
            .any(|event| event["content"] == "worker after review"),
        "{resumed:?}"
    );
    assert_eq!(model_calls(&calls), 1);
    let finished = checkpointer
        .load(&thread)
        .await
        .expect("checkpoint lookup")
        .expect("final checkpoint");
    assert!(
        has_graph_call_receipts(&finished).expect("receipt metadata decodes"),
        "the first Agent call of a new conversation must run scoped even after a pause before it"
    );
}

#[tokio::test(flavor = "current_thread")]
async fn a_conversation_an_older_worker_paused_in_an_agent_node_continues_unscoped() {
    let (sessions, checkpointer) = state();
    let versions = BTreeMap::from([((5, 6), asking_agent())]);
    let make_request = || {
        root_request(
            &root_yaml("worker"),
            vec![reference((5, 6), "agent", "worker")],
        )
    };
    let request = make_request();
    let thread = private_pipeline_session_id(&request);
    // The older worker: its resolver is the one this branch keeps for exactly this case.
    let (older, _, _) = process(
        &sessions,
        &checkpointer,
        &versions,
        vec![TestModelGatewayOutcome::Response(
            pipeline_ask_user_call_response(),
        )],
    );
    let older = older.with_fresh_start_continuation(ApplicationContinuation::LegacyUnscoped);
    let invocation = older
        .assemble(authorized(&request))
        .await
        .expect("the older worker assembles");
    let (browser, _) = collect_pipeline_pause_events(invocation).await;
    let pending = first_card(&browser);
    let paused = checkpointer
        .load(&thread)
        .await
        .expect("checkpoint lookup")
        .expect("paused checkpoint");
    assert!(!has_graph_call_receipts(&paused).expect("receipt metadata decodes"));

    let (current_worker, calls, _) = process(
        &sessions,
        &checkpointer,
        &versions,
        vec![text("clarified by the older card")],
    );
    let resume = answer(make_request(), &pending, 32);
    let invocation = current_worker
        .assemble(authorized(&resume))
        .await
        .expect("this worker resumes the older conversation");
    let resumed = collect_pipeline_completion(invocation).await;
    assert!(
        resumed
            .iter()
            .any(|event| event["content"] == "clarified by the older card"),
        "{resumed:?}"
    );
    assert_eq!(model_calls(&calls), 1);
    let finished = checkpointer
        .load(&thread)
        .await
        .expect("checkpoint lookup")
        .expect("final checkpoint");
    assert!(
        !has_graph_call_receipts(&finished).expect("receipt metadata decodes"),
        "an older conversation keeps the resolver that paused it and writes no receipts"
    );
}

#[tokio::test(flavor = "current_thread")]
async fn a_saved_child_pipeline_an_older_worker_paused_continues_unscoped() {
    let (sessions, checkpointer) = state();
    let gate = "state:\n  input: str\n  messages: list\n  answer: str\nentry_point: approve\nnodes:\n  - id: approve\n    type: hitl\n    input: [input]\n    user_message:\n      type: fstring\n      value: 'Approve {input}.'\n    routes:\n      approve: done\n      reject: END\n  - id: done\n    type: state_modifier\n    template: 'Approved: {{ input }}'\n    input: [input]\n    output: [answer]\n    transition: END\n";
    let versions = BTreeMap::from([(
        (3, 4),
        saved_version("pipeline", gate, &json!({}), Vec::new()),
    )]);
    let make_request = || {
        root_request(
            &root_yaml("child-flow"),
            vec![reference((3, 4), "pipeline", "child-flow")],
        )
    };
    let request = make_request();
    let thread = private_pipeline_session_id(&request);
    let (older, _, _) = process(&sessions, &checkpointer, &versions, Vec::new());
    let older = older.with_fresh_start_continuation(ApplicationContinuation::LegacyUnscoped);
    let invocation = older
        .assemble(authorized(&request))
        .await
        .expect("the older worker assembles");
    let (browser, _) = collect_pipeline_pause_events(invocation).await;
    let card = first_card(&browser);

    let (current_worker, _, _) = process(&sessions, &checkpointer, &versions, Vec::new());
    let resume = approve(make_request(), &card["interrupt_id"], 33);
    let invocation = current_worker
        .assemble(authorized(&resume))
        .await
        .expect("this worker resumes the older saved child pipeline");
    let resumed = collect_pipeline_completion(invocation).await;
    assert!(
        resumed
            .iter()
            .any(|event| event["content"] == "Approved: Summarize the release"),
        "{resumed:?}"
    );
    let finished = checkpointer
        .load(&thread)
        .await
        .expect("checkpoint lookup")
        .expect("final checkpoint");
    assert!(!has_graph_call_receipts(&finished).expect("receipt metadata decodes"));
}

pub(super) struct BatchConnector;
#[async_trait]
impl McpConnector for BatchConnector {
    async fn connect(
        &self,
        _: &RemoteMcpConfig,
    ) -> Result<Arc<dyn Toolset>, McpMaterializationError> {
        Ok(Arc::new(BasicToolset::new(
            "batch_fixture",
            vec![
                Arc::new(NamedReadTool("get_issues")),
                Arc::new(NamedReadTool("echo")),
                Arc::new(NamedReadTool("reverse")),
            ],
        )))
    }
}

pub(super) fn batch_agent() -> Value {
    json!({
        "agent_type": "agent", "instructions": "Run the three tools and report.",
        "meta": {}, "variables": [],
        "tools": [{"id": 92, "type": "mcp", "toolkit_name": "batch",
            "settings": {"url": "https://mcp.example.invalid/v1/mcp", "headers": null,
                "client_id": null, "client_secret": null, "scopes": null, "timeout": 30,
                "selected_tools": ["get_issues", "echo", "reverse"], "enable_caching": true,
                "cache_ttl": 300, "ssl_verify": true}}],
        "llm_settings": {"model_name": "child-model", "model_project_id": 23,
            "max_tokens": 2048, "reasoning_effort": null, "temperature": 0.2,
            "openai_compatible": true}
    })
}

pub(super) fn batch_policy() -> Arc<ToolAdmissionPolicy> {
    Arc::new(runtime_tool_policy(&json!({
        "toolkit_security": {
            "sensitive_tools": {"mcp": ["reverse"]},
            "sensitive_action_company_name": "Example Org"
        }
    })))
}

pub(super) fn three_call_response() -> Response<Body> {
    let call = |index: usize, id: &str, name: &str| json!({"index":index,"id":id,"type":"function","function":{"name":name,"arguments":"{}"}});
    let chunk = json!({"choices":[{"delta":{"tool_calls":[
        call(0, "call_issues", "get_issues"), call(1, "call_echo", "echo"),
        call(2, "call_mcp", "reverse")]},"finish_reason":null}]});
    let raw = format!(
        "data: {chunk}\n\ndata: {{\"choices\":[{{\"delta\":{{}},\"finish_reason\":\"tool_calls\"}}]}}\n\ndata: {{\"choices\":[],\"usage\":{{\"prompt_tokens\":3,\"completion_tokens\":2}}}}\n\ndata: [DONE]\n\n"
    );
    test_model_gateway_response(Body::new(Full::<Bytes>::from(raw)))
}

#[tokio::test(flavor = "current_thread")]
async fn a_three_call_batch_with_one_sensitive_call_resumes_after_approval() {
    let (sessions, checkpointer) = state();
    let versions = BTreeMap::from([((3, 4), batch_agent())]);
    let make_assembler = |outcomes| {
        let (platform, model_facade, captured, _) = saved_runtime(&versions, outcomes);
        (
            PipelineNativeAgentAssembler::with_state(
                Arc::clone(&sessions),
                Arc::clone(&checkpointer) as Arc<dyn Checkpointer>,
            )
            .with_runtime_clients(platform, model_facade)
            .with_mcp_connector(Arc::new(BatchConnector))
            .with_tool_policy(batch_policy()),
            captured,
        )
    };
    let request = agent_pipeline_request("release-agent", "agent");
    let (assembler, captured) = make_assembler(vec![TestModelGatewayOutcome::Response(
        three_call_response(),
    )]);
    let invocation = assembler
        .assemble(authorized(&request))
        .await
        .expect("assemble");
    let (cards, _) = collect_pipeline_pause(invocation).await;
    assert_eq!(cards.len(), 1, "{cards:?}");
    let id = cards[0]["response_metadata"]["hitl_interrupts"][0]["interrupt_id"]
        .as_str()
        .expect("card id")
        .to_owned();
    assert_eq!(model_calls(&captured), 1);
    drop(assembler);
    let (replacement, resumed_calls) = make_assembler(vec![text("batch done")]);
    let resume = pipeline_application_resume_request(vec![id], "approve", "", [51; 32]);
    let invocation = replacement
        .assemble(authorized(&resume))
        .await
        .expect("resume assembles");
    let browser = collect_pipeline_completion(invocation).await;
    assert!(
        browser.iter().any(|event| event["content"] == "batch done"),
        "{browser:?}"
    );
    assert_eq!(model_calls(&resumed_calls), 1);
}

#[test]
fn a_recovery_start_never_takes_the_older_conversation_resolver() {
    use crate::agents::pipeline::continues_a_decided_pause;
    use crate::agents::runtime::PipelineNativeStart;
    // A recovery re-runs an unfinished node: a missing receipt there is a crash before the
    // first receipt revision, never an older worker's in-flight call, so it stays scoped.
    for start in [
        PipelineNativeStart::Checkpoint,
        PipelineNativeStart::Fresh,
        PipelineNativeStart::Regenerate,
    ] {
        assert!(!continues_a_decided_pause(&start));
    }
    assert!(continues_a_decided_pause(
        &PipelineNativeStart::StaticTools(Vec::new())
    ));
}

/// Agent -> sub-Agent -> static saved pipeline (depth 2): the root's terminal
/// carries the nested pause's inventory, and one continuation resumes the
/// sub-Agent's pipeline, then the sub-Agent and the root each answer once.
#[tokio::test(flavor = "current_thread")]
async fn a_static_pause_of_a_sub_agents_pipeline_completes_and_continues_once() {
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let versions = BTreeMap::from([
        (
            (5, 6),
            plain_agent(vec![reference((7, 8), "pipeline", "inner")]),
        ),
        (
            (7, 8),
            saved_version("pipeline", STATIC_GRAPH, &json!({}), Vec::new()),
        ),
    ]);
    let tools = vec![reference((5, 6), "agent", "helper")];
    let (assembler, _) = ordinary_assembler(
        &sessions,
        &versions,
        vec![
            TestModelGatewayOutcome::Response(tool_call_response(
                "helper-call",
                "elitea_agent_5_v_6",
                &json!({"task":"help"}),
            )),
            TestModelGatewayOutcome::Response(tool_call_response(
                "inner-call",
                "elitea_agent_7_v_8",
                &json!({"task":"inner work"}),
            )),
        ],
    );
    let (browser, paused) = run_ordinary(&assembler, &ordinary_agent_request(tools.clone())).await;
    assert!(
        !paused,
        "the nested static pause completes the turn: {browser:?}"
    );
    let terminal = browser.last().expect("a completed result");
    assert_eq!(terminal["type"], "full_message", "{browser:?}");
    let inventory = terminal["response_metadata"]["pipeline_static_tools_v1"]["pauses"]
        .as_array()
        .cloned()
        .unwrap_or_else(|| panic!("nested static pause inventory missing: {terminal:?}"));
    assert_eq!(inventory.len(), 1);

    let (replacement, replaced_calls) = ordinary_assembler(
        &sessions,
        &versions,
        vec![text("helper continued"), text("root continued")],
    );
    let mut resume = ordinary_agent_request(tools);
    resume.binding.request_content_digest = [24; 32];
    resume.payload.should_continue = true;
    resume.payload.meta.insert(
        "pipeline_static_tool_resume_v1".to_owned(),
        json!({"revision":1,"decisions":[{
            "pause_id":inventory[0]["proof"]["pause_id"],
            "child_thread_id":inventory[0]["child_thread_id"],
            "tool_call_id":inventory[0]["tool_call_id"],
            "action":"continue","value":"finish once"}]}),
    );
    let (resumed, paused) = run_ordinary(&replacement, &resume).await;
    assert!(!paused, "{resumed:?}");
    assert!(
        resumed
            .iter()
            .any(|event| event["content"] == "root continued"),
        "{resumed:?}"
    );
    assert_eq!(model_calls(&replaced_calls), 2, "{resumed:?}");
}

pub(super) const GATE_GRAPH: &str = "state: {input: str, messages: list, answer: str}\nentry_point: review\nnodes:\n  - id: review\n    type: hitl\n    input: [input]\n    user_message:\n      type: fixed\n      value: Approve?\n    routes:\n      approve: approved\n      reject: rejected\n  - id: approved\n    type: state_modifier\n    template: 'APPROVED {{ input }}'\n    input: [input]\n    output: [answer]\n    transition: END\n  - id: rejected\n    type: state_modifier\n    template: 'REJECTED {{ input }}'\n    input: [input]\n    output: [answer]\n    transition: END\n";

/// ADR-0041 decision 5, the one `hitl` placement that stays unsupported: a
/// pipeline tool of a saved Agent beneath a graph root. The graph root owns
/// the checkpoint thread the gate would have to name (as for a static pause,
/// FU-244), so the tool is skipped and named before any model call, and the
/// Agent node still answers without it.
#[tokio::test(flavor = "current_thread")]
async fn a_gate_pipeline_tool_under_a_graph_root_is_skipped_and_the_agent_answers() {
    let (sessions, checkpointer) = state();
    let versions = BTreeMap::from([
        (
            (5, 6),
            plain_agent(vec![reference((7, 8), "pipeline", "gate")]),
        ),
        (
            (7, 8),
            saved_version("pipeline", GATE_GRAPH, &json!({}), Vec::new()),
        ),
    ]);
    let (assembler, captured, _) = process(
        &sessions,
        &checkpointer,
        &versions,
        vec![text("worker without the gate")],
    );
    let request = root_request(
        &root_yaml("worker"),
        vec![reference((5, 6), "agent", "worker")],
    );
    let invocation = assembler
        .assemble(authorized(&request))
        .await
        .expect("the graph root runs without the unsupported tool");
    let browser = collect_pipeline_completion(invocation).await;
    assert!(
        !offered_tools(&captured, 0).contains(&"elitea_agent_7_v_8".to_owned()),
        "the gate pipeline is not offered: {:?}",
        offered_tools(&captured, 0)
    );
    assert!(
        browser
            .iter()
            .any(|event| event["content"] == "worker without the gate"),
        "{browser:?}"
    );
    assert!(
        browser
            .iter()
            .all(|event| event["type"] != "agent_hitl_interrupt"),
        "{browser:?}"
    );
}

/// ADR-0041 decision 5 under a graph root: root → saved pipeline → saved
/// pipeline → `hitl`. One card names both pipeline hops, each level keeps its
/// own checkpoint, and the answer resumes the gate once and both parents.
async fn graph_root_three_level_gate(action: &str, expected: &str) {
    let (sessions, checkpointer) = state();
    let versions = BTreeMap::from([
        (
            (5, 6),
            saved_version(
                "pipeline",
                &child_pipeline_yaml("inner"),
                &json!({}),
                vec![reference((7, 8), "pipeline", "inner")],
            ),
        ),
        (
            (7, 8),
            saved_version("pipeline", GATE_GRAPH, &json!({}), Vec::new()),
        ),
    ]);
    let make_request = || {
        root_request(
            &root_yaml("outer"),
            vec![reference((5, 6), "pipeline", "outer")],
        )
    };
    let thread = private_pipeline_session_id(&make_request());
    let (assembler, _, _) = process(&sessions, &checkpointer, &versions, Vec::new());
    let invocation = assembler
        .assemble(authorized(&make_request()))
        .await
        .expect("a three-level graph assembles");
    let (browser, _) = collect_pipeline_pause_events(invocation).await;
    let cards = browser
        .iter()
        .filter(|event| event["type"] == "agent_hitl_interrupt")
        .collect::<Vec<_>>();
    assert_eq!(cards.len(), 1, "{browser:?}");
    let path = cards[0]["response_metadata"]["parent_agent_path"]
        .as_array()
        .cloned()
        .expect("owner path");
    assert_eq!(
        path.iter()
            .map(|tier| tier["name"].clone())
            .collect::<Vec<_>>(),
        vec![json!("outer"), json!("inner")],
        "{path:?}"
    );
    for level in [
        thread.clone(),
        format!("{thread}/delegate"),
        format!("{thread}/delegate/delegate"),
    ] {
        assert!(
            checkpointer
                .load(&level)
                .await
                .expect("checkpoint lookup")
                .is_some(),
            "every level keeps its checkpoint: {level}"
        );
    }
    let card = first_card(&browser);
    let (replacement, calls, _) = process(&sessions, &checkpointer, &versions, Vec::new());
    let mut resume = approve(make_request(), &card["interrupt_id"], 43);
    resume.payload.hitl_action = Some(action.to_owned());
    resume.payload.hitl_decisions[0]["action"] = json!(action);
    let invocation = replacement
        .assemble(authorized(&resume))
        .await
        .expect("the gate decision resumes the innermost pipeline");
    let resumed = collect_pipeline_completion(invocation).await;
    assert!(
        resumed.iter().any(|event| event["content"] == expected),
        "{resumed:?}"
    );
    assert!(
        resumed
            .iter()
            .all(|event| event["type"] != "agent_hitl_interrupt"),
        "{resumed:?}"
    );
    assert_eq!(model_calls(&calls), 0);
    let (again, _, _) = process(&sessions, &checkpointer, &versions, Vec::new());
    let mut replay = approve(make_request(), &card["interrupt_id"], 44);
    replay.payload.hitl_action = Some(action.to_owned());
    replay.payload.hitl_decisions[0]["action"] = json!(action);
    assert!(
        again.assemble(authorized(&replay)).await.is_err(),
        "an answered gate resumes exactly once"
    );
}

#[tokio::test(flavor = "current_thread")]
async fn a_three_level_graph_gate_approves_once_and_continues_both_parents() {
    graph_root_three_level_gate("approve", "APPROVED Summarize the release").await;
}

#[tokio::test(flavor = "current_thread")]
async fn a_three_level_graph_gate_rejects_and_the_parents_read_the_rejection() {
    graph_root_three_level_gate("reject", "REJECTED Summarize the release").await;
}
