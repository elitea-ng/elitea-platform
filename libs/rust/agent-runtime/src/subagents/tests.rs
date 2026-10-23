use std::collections::HashMap;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use adk_agent::LlmAgentBuilder;
use adk_core::{
    Content, Event, Llm, LlmRequest, LlmResponse, LlmResponseStream, Part, Tool, ToolContext,
    Toolset, UsageMetadata,
};
use adk_runner::Runner;
use adk_session::{CreateRequest, InMemorySessionService, SessionService};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use futures::StreamExt as _;
use serde_json::{Value, json};

use super::*;

/// The child's model, scripted by its latest message (its task, or what
/// `send_input` sent):
/// * `answer <ms> <text>`: sleep, then answer `<text>`;
/// * `loop`: text and a `look` call every step, 40k input tokens each;
/// * `write`: call `write_file`, then answer;
/// * `hang`: never answer.
///
/// It records the tools each request offered.
#[derive(Default)]
struct ChildModel {
    offered: Mutex<Vec<Vec<String>>>,
    calls: AtomicUsize,
    /// Set when a `hang` call is dropped (its child was stopped).
    dropped: Arc<AtomicBool>,
}

fn user_texts(request: &LlmRequest) -> Vec<String> {
    request
        .contents
        .iter()
        .filter(|content| content.role == "user")
        .flat_map(|content| &content.parts)
        .filter_map(|part| match part {
            Part::Text { text } => Some(text.clone()),
            _ => None,
        })
        .collect()
}

fn answered(request: &LlmRequest) -> bool {
    request.contents.iter().any(|content| {
        content
            .parts
            .iter()
            .any(|part| matches!(part, Part::FunctionResponse { .. }))
    })
}

fn response(parts: Vec<Part>, prompt_tokens: i32) -> LlmResponseStream {
    let calls = parts
        .iter()
        .any(|part| matches!(part, Part::FunctionCall { .. }));
    let response = LlmResponse {
        content: Some(Content {
            role: "model".to_owned(),
            parts,
        }),
        usage_metadata: Some(UsageMetadata {
            prompt_token_count: prompt_tokens,
            candidates_token_count: 10,
            total_token_count: prompt_tokens + 10,
            ..UsageMetadata::default()
        }),
        turn_complete: !calls,
        ..LlmResponse::default()
    };
    Box::pin(futures::stream::iter([Ok(response)]))
}

fn call(id: &str, name: &str, args: Value) -> Part {
    Part::FunctionCall {
        name: name.to_owned(),
        args,
        id: Some(id.to_owned()),
        thought_signature: None,
    }
}

fn text(text: &str) -> Part {
    Part::Text {
        text: text.to_owned(),
    }
}

struct DropFlag(Arc<AtomicBool>);

impl Drop for DropFlag {
    fn drop(&mut self) {
        self.0.store(true, Ordering::SeqCst);
    }
}

#[async_trait]
impl Llm for ChildModel {
    fn name(&self) -> &'static str {
        "child"
    }

    async fn generate_content(
        &self,
        request: LlmRequest,
        _stream: bool,
    ) -> adk_core::Result<LlmResponseStream> {
        let n = self.calls.fetch_add(1, Ordering::SeqCst);
        let mut offered: Vec<String> = request.tools.keys().cloned().collect();
        offered.sort();
        self.offered.lock().unwrap().push(offered);
        let texts = user_texts(&request);
        let task = texts.first().cloned().unwrap_or_default();
        let latest = texts.last().cloned().unwrap_or_default();
        let mut words = latest.split_whitespace();
        Ok(match words.next() {
            Some("answer") => {
                let ms: u64 = words.next().unwrap().parse().unwrap();
                tokio::time::sleep(Duration::from_millis(ms)).await;
                response(vec![text(&words.collect::<Vec<_>>().join(" "))], 100)
            }
            Some("loop") => response(
                vec![
                    text(&format!("partial {n}")),
                    call(&format!("look-{n}"), "look", json!({})),
                ],
                40_000,
            ),
            Some("write") if !answered(&request) => response(
                vec![call("w-1", "write_file", json!({"path": "a.txt"}))],
                100,
            ),
            Some("write") => response(vec![text("written?")], 100),
            Some("hang") => {
                let _flag = DropFlag(self.dropped.clone());
                std::future::pending::<()>().await;
                unreachable!()
            }
            // A follow-up: the reply names the task, which only this
            // child's session holds.
            _ => response(vec![text(&format!("follow-up: {task}"))], 100),
        })
    }
}

struct Look;

#[async_trait]
impl Tool for Look {
    fn name(&self) -> &'static str {
        "look"
    }

    fn description(&self) -> &'static str {
        "Look around."
    }

    fn is_read_only(&self) -> bool {
        true
    }

    async fn execute(&self, _ctx: Arc<dyn ToolContext>, _args: Value) -> adk_core::Result<Value> {
        Ok(json!({"seen": true}))
    }
}

#[derive(Default)]
struct WriteFile(AtomicUsize);

#[async_trait]
impl Tool for WriteFile {
    fn name(&self) -> &'static str {
        "write_file"
    }

    fn description(&self) -> &'static str {
        "Write a file."
    }

    async fn execute(&self, _ctx: Arc<dyn ToolContext>, _args: Value) -> adk_core::Result<Value> {
        self.0.fetch_add(1, Ordering::SeqCst);
        Ok(json!({"written": true}))
    }
}

struct TestHost {
    model: Arc<ChildModel>,
    write: Arc<WriteFile>,
    /// Offered to children beside the tools: the sub-agent tools themselves.
    extra: Mutex<Vec<Arc<dyn Toolset>>>,
    started: Mutex<Vec<ChildIdentity>>,
    finished: Mutex<Vec<ChildReport>>,
    instructions: Mutex<Vec<String>>,
}

impl TestHost {
    fn new() -> Arc<Self> {
        Arc::new(Self {
            model: Arc::new(ChildModel::default()),
            write: Arc::new(WriteFile::default()),
            extra: Mutex::new(Vec::new()),
            started: Mutex::new(Vec::new()),
            finished: Mutex::new(Vec::new()),
            instructions: Mutex::new(Vec::new()),
        })
    }
}

#[async_trait]
impl SubAgentHost for TestHost {
    fn bind_model(
        &self,
        _child: &ChildIdentity,
        system_instruction: &str,
    ) -> Result<Arc<dyn Llm>, HostError> {
        self.instructions
            .lock()
            .unwrap()
            .push(system_instruction.to_owned());
        Ok(self.model.clone())
    }

    async fn toolsets(&self, _child: &ChildIdentity) -> Result<Vec<Arc<dyn Toolset>>, HostError> {
        let mut toolsets: Vec<Arc<dyn Toolset>> = vec![Arc::new(BasicToolset::new(
            "local",
            vec![Arc::new(Look) as Arc<dyn Tool>, self.write.clone()],
        ))];
        toolsets.extend(self.extra.lock().unwrap().iter().cloned());
        Ok(toolsets)
    }

    fn started(&self, child: &ChildIdentity) {
        self.started.lock().unwrap().push(child.clone());
    }

    fn finished(&self, _child: &ChildIdentity, report: &ChildReport) {
        self.finished.lock().unwrap().push(report.clone());
    }
}

/// Every `agent_id` in the function responses `request` carries, in order.
fn agent_ids(request: &LlmRequest) -> Vec<String> {
    let mut ids = Vec::new();
    for content in &request.contents {
        for part in &content.parts {
            if let Part::FunctionResponse {
                function_response, ..
            } = part
                && function_response.name == SPAWN_AGENT
                && let Some(id) = find_key(&function_response.response, "agent_id")
            {
                ids.push(id);
            }
        }
    }
    ids
}

fn find_key(value: &Value, key: &str) -> Option<String> {
    match value {
        Value::Object(map) => map
            .get(key)
            .and_then(Value::as_str)
            .map(str::to_owned)
            .or_else(|| map.values().find_map(|v| find_key(v, key))),
        Value::Array(items) => items.iter().find_map(|v| find_key(v, key)),
        _ => None,
    }
}

type Script = dyn Fn(usize, &LlmRequest) -> Vec<Part> + Send + Sync;

/// The parent's model: `script` answers each step; the last request it saw
/// is kept.
struct Parent {
    script: Box<Script>,
    calls: AtomicUsize,
    last: Mutex<Option<LlmRequest>>,
}

#[async_trait]
impl Llm for Parent {
    fn name(&self) -> &'static str {
        "parent"
    }

    async fn generate_content(
        &self,
        request: LlmRequest,
        _stream: bool,
    ) -> adk_core::Result<LlmResponseStream> {
        let n = self.calls.fetch_add(1, Ordering::SeqCst);
        let parts = (self.script)(n, &request);
        *self.last.lock().unwrap() = Some(request);
        Ok(response(parts, 10))
    }
}

/// Run a parent agent named `name` with the sub-agent tools on `script`;
/// the function responses of its run, by tool name, in order.
async fn run_parent(
    name: &str,
    subagents: &SubAgents,
    script: Box<Script>,
) -> Vec<(String, Value)> {
    let parent = Arc::new(Parent {
        script,
        calls: AtomicUsize::new(0),
        last: Mutex::new(None),
    });
    let agent = LlmAgentBuilder::new(name)
        .model(parent)
        .toolset(subagents.toolset())
        .max_iterations(10)
        .build()
        .unwrap();
    let sessions = Arc::new(InMemorySessionService::new());
    sessions
        .create(CreateRequest {
            app_name: "app".to_owned(),
            user_id: "user".to_owned(),
            session_id: Some("s".to_owned()),
            state: HashMap::new(),
        })
        .await
        .unwrap();
    let runner = Runner::builder()
        .app_name("app")
        .agent(Arc::new(agent))
        .session_service(sessions)
        .build()
        .unwrap();
    let mut stream = runner
        .run_str("user", "s", Content::new("user").with_text("go"))
        .await
        .unwrap();
    let mut responses = Vec::new();
    while let Some(event) = stream.next().await {
        let event: Event = event.unwrap();
        if let Some(content) = &event.llm_response.content {
            for part in &content.parts {
                if let Part::FunctionResponse {
                    function_response, ..
                } = part
                {
                    responses.push((
                        function_response.name.clone(),
                        function_response.response.clone(),
                    ));
                }
            }
        }
    }
    responses
}

fn spawn_call(n: usize, task: &str) -> Part {
    call(&format!("spawn-{n}"), SPAWN_AGENT, json!({"task": task}))
}

/// Spawn one child per task in the first step, wait for them all in the
/// second, answer in the third.
fn spawn_then_wait(tasks: Vec<String>) -> Box<Script> {
    Box::new(move |n, request| match n {
        0 => tasks
            .iter()
            .enumerate()
            .map(|(i, task)| spawn_call(i, task))
            .collect(),
        1 => vec![call(
            "wait-1",
            WAIT_AGENT,
            json!({"agent_ids": agent_ids(request)}),
        )],
        _ => vec![text("done")],
    })
}

fn wait_results(responses: &[(String, Value)]) -> Vec<Value> {
    responses
        .iter()
        .find(|(name, _)| name == WAIT_AGENT)
        .and_then(|(_, value)| value.get("results"))
        .and_then(Value::as_array)
        .cloned()
        .unwrap_or_default()
}

fn subagents(host: &Arc<TestHost>) -> SubAgents {
    SubAgents::new(
        host.clone(),
        SubAgentConfig::new("## Environment\nA fixture."),
    )
}

#[tokio::test(start_paused = true)]
async fn children_run_concurrently() {
    let host = TestHost::new();
    let children = subagents(&host);
    let started = tokio::time::Instant::now();
    let responses = run_parent(
        "parent",
        &children,
        spawn_then_wait(vec![
            "answer 300 one".into(),
            "answer 200 two".into(),
            "answer 100 three".into(),
        ]),
    )
    .await;
    let took = started.elapsed();
    // Each child slept on its own task: the wall time is the slowest's.
    assert!(took < Duration::from_millis(500), "took {took:?}");
    let results = wait_results(&responses);
    assert_eq!(results.len(), 3);
    let reports: Vec<(&str, &str)> = results
        .iter()
        .map(|r| (r["status"].as_str().unwrap(), r["result"].as_str().unwrap()))
        .collect();
    assert_eq!(
        reports,
        [
            ("completed", "one"),
            ("completed", "two"),
            ("completed", "three")
        ]
    );
    for result in &results {
        assert_eq!(result["steps"], 1);
        assert_eq!(result["input_tokens"], 100);
        assert_eq!(result["output_tokens"], 10);
    }
    // Each child got the preamble and the host's sections, and its ids,
    // spawn calls and ordinals are what the host's notices name.
    let instructions = host.instructions.lock().unwrap().clone();
    assert!(
        instructions
            .iter()
            .all(|i| i.starts_with(CHILD_PREAMBLE) && i.ends_with("A fixture."))
    );
    let started = host.started.lock().unwrap().clone();
    assert_eq!(
        started.iter().map(|c| c.ordinal).collect::<Vec<_>>(),
        [1, 2, 3]
    );
    assert_eq!(
        started
            .iter()
            .map(|c| c.parent_call_id.as_str())
            .collect::<Vec<_>>(),
        ["spawn-0", "spawn-1", "spawn-2"]
    );
    assert!(
        started
            .iter()
            .all(|c| c.agent_id.len() == 10 && c.agent_id.starts_with("a_"))
    );
    assert_eq!(host.finished.lock().unwrap().len(), 3);
}

/// A wait that names no one waits for every child still running, so a model
/// can spawn and wait in the same round of calls, before it knows the ids.
#[tokio::test(start_paused = true)]
async fn an_unnamed_wait_collects_every_running_child() {
    let host = TestHost::new();
    let children = subagents(&host);
    let responses = run_parent(
        "parent",
        &children,
        Box::new(|n, _| match n {
            0 => vec![
                spawn_call(0, "answer 200 one"),
                spawn_call(1, "answer 100 two"),
                call("wait-0", WAIT_AGENT, json!({})),
            ],
            _ => vec![text("done")],
        }),
    )
    .await;
    let results = wait_results(&responses);
    let reports: Vec<(&str, &str)> = results
        .iter()
        .map(|r| (r["status"].as_str().unwrap(), r["result"].as_str().unwrap()))
        .collect();
    assert_eq!(reports, [("completed", "one"), ("completed", "two")]);
}

#[tokio::test]
async fn child_toolset_is_read_only_only() {
    // A fixed list with a writing tool is refused at construction.
    assert!(ChildTools::new(vec![Arc::new(Look)]).is_ok());
    let refused = ChildTools::new(vec![Arc::new(Look), Arc::new(WriteFile::default())])
        .err()
        .unwrap();
    assert_eq!(refused.name, "write_file");
    // So is one of the sub-agent tools, read-only or not.
    let host = TestHost::new();
    let children = subagents(&host);
    let ctx: Arc<dyn adk_core::ReadonlyContext> = Arc::new(NoContext);
    for tool in children.toolset().tools(ctx).await.unwrap() {
        assert!(ChildTools::new(vec![tool]).is_err());
    }

    // A host's tools, filtered: the child is offered `look` only.
    let responses = run_parent(
        "parent",
        &children,
        spawn_then_wait(vec!["answer 1 fine".into()]),
    )
    .await;
    assert_eq!(wait_results(&responses)[0]["status"], "completed");
    let offered = host.model.offered.lock().unwrap().clone();
    assert_eq!(offered, [vec!["look".to_owned()]]);
}

/// The regression test: without the read-only filter the child would be
/// offered `write_file` and run it.
#[tokio::test]
async fn write_tool_cannot_reach_a_child() {
    let host = TestHost::new();
    let children = subagents(&host);
    let responses = run_parent("parent", &children, spawn_then_wait(vec!["write".into()])).await;
    assert_eq!(wait_results(&responses).len(), 1);
    assert_eq!(host.write.0.load(Ordering::SeqCst), 0, "a child wrote");
    for offered in host.model.offered.lock().unwrap().iter() {
        assert!(!offered.contains(&"write_file".to_owned()), "{offered:?}");
    }
}

#[tokio::test]
async fn depth_one_only() {
    let host = TestHost::new();
    let children = subagents(&host);
    // The sub-agent tools themselves, offered to a child by a careless host:
    // the filter drops them (`wait_agent` is read-only, and still dropped).
    host.extra.lock().unwrap().push(children.toolset());
    let responses = run_parent(
        "parent",
        &children,
        spawn_then_wait(vec!["answer 1 fine".into()]),
    )
    .await;
    assert_eq!(wait_results(&responses)[0]["status"], "completed");
    for offered in host.model.offered.lock().unwrap().iter() {
        for name in TOOL_NAMES {
            assert!(!offered.contains(&name.to_owned()), "{offered:?}");
        }
    }
    // And a call from a child's context is refused.
    let nested = subagents(&TestHost::new());
    let responses = run_parent(
        "subagent_a_00000000",
        &nested,
        Box::new(|n, _| match n {
            0 => vec![spawn_call(0, "answer 1 deeper")],
            _ => vec![text("done")],
        }),
    )
    .await;
    assert_eq!(responses[0].1["code"], DEPTH_EXCEEDED);
    assert_eq!(nested.reports(), []);
}

#[tokio::test]
async fn budget_exhausted_returns_partial() {
    let host = TestHost::new();
    let children = subagents(&host);
    let responses = run_parent("parent", &children, spawn_then_wait(vec!["loop".into()])).await;
    let result = &wait_results(&responses)[0];
    assert_eq!(result["status"], "budget_exhausted");
    assert_eq!(result["error"], "subagent_budget_exhausted");
    // Two calls of 40k: the third would pass the 60k budget and is not made.
    assert_eq!(host.model.calls.load(Ordering::SeqCst), 2);
    assert_eq!(result["input_tokens"], 80_000);
    assert_eq!(result["result"], "partial 1");
}

#[tokio::test]
async fn close_cancels() {
    let host = TestHost::new();
    let children = subagents(&host);
    let responses = run_parent(
        "parent",
        &children,
        Box::new(|n, request| match n {
            0 => vec![spawn_call(0, "hang")],
            1 => vec![call(
                "wait-1",
                WAIT_AGENT,
                json!({"agent_ids": agent_ids(request), "timeout_seconds": 1}),
            )],
            2 => vec![call(
                "close-1",
                CLOSE_AGENT,
                json!({"agent_id": agent_ids(request)[0]}),
            )],
            _ => vec![text("done")],
        }),
    )
    .await;
    // Still running at the wait's timeout, then closed.
    assert_eq!(wait_results(&responses)[0]["status"], "running");
    let closed = &responses
        .iter()
        .find(|(name, _)| name == CLOSE_AGENT)
        .unwrap()
        .1;
    assert_eq!(closed["status"], "cancelled");
    assert_eq!(closed["error"], "subagent_cancelled");
    let finished = host.finished.lock().unwrap().clone();
    assert_eq!(finished.len(), 1);
    assert_eq!(finished[0].status, ChildStatus::Cancelled);
    // The child's model call was dropped with its task.
    for _ in 0..100 {
        if host.model.dropped.load(Ordering::SeqCst) {
            break;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    assert!(host.model.dropped.load(Ordering::SeqCst));
}

#[tokio::test]
async fn dropping_the_run_stops_its_children() {
    let host = TestHost::new();
    let children = subagents(&host);
    run_parent(
        "parent",
        &children,
        Box::new(|n, _| match n {
            0 => vec![spawn_call(0, "hang")],
            _ => vec![text("done")],
        }),
    )
    .await;
    assert_eq!(children.reports()[0].status, ChildStatus::Running);
    // Once its model call is under way.
    for _ in 0..100 {
        if host.model.calls.load(Ordering::SeqCst) > 0 {
            break;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    drop(children);
    for _ in 0..100 {
        if host.model.dropped.load(Ordering::SeqCst) {
            break;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    assert!(host.model.dropped.load(Ordering::SeqCst));
}

#[tokio::test]
async fn a_completed_child_continues_on_its_own_session() {
    let host = TestHost::new();
    let children = subagents(&host);
    let responses = run_parent(
        "parent",
        &children,
        Box::new(|n, request| {
            let ids = agent_ids(request);
            match n {
                0 => vec![spawn_call(0, "answer 1 first")],
                1 | 3 => vec![call(
                    &format!("wait-{n}"),
                    WAIT_AGENT,
                    json!({"agent_ids": ids}),
                )],
                2 => vec![call(
                    "send-1",
                    SEND_INPUT,
                    json!({"agent_id": ids[0], "message": "and more"}),
                )],
                _ => vec![text("done")],
            }
        }),
    )
    .await;
    let waits: Vec<&Value> = responses
        .iter()
        .filter(|(name, _)| name == WAIT_AGENT)
        .map(|(_, value)| &value["results"][0])
        .collect();
    assert_eq!(waits[0]["result"], "first");
    // The second run's request carries the task: it is the same session.
    assert_eq!(waits[1]["result"], "follow-up: answer 1 first");
    assert_eq!(waits[1]["steps"], 2);
    assert_eq!(host.finished.lock().unwrap().len(), 2);
}

#[tokio::test]
async fn the_running_cap_and_the_turn_cap_refuse_more() {
    let host = TestHost::new();
    let children = SubAgents::new(
        host.clone(),
        SubAgentConfig::new("").with_limits(SubAgentLimits {
            max_concurrent: 2,
            max_per_turn: 3,
            ..SubAgentLimits::default()
        }),
    );
    let responses = run_parent(
        "parent",
        &children,
        Box::new(|n, request| match n {
            0 => (0..3).map(|i| spawn_call(i, "hang")).collect(),
            1 => vec![call(
                "close-1",
                CLOSE_AGENT,
                json!({"agent_id": agent_ids(request)[0]}),
            )],
            2 => vec![spawn_call(3, "hang"), spawn_call(4, "hang")],
            _ => vec![text("done")],
        }),
    )
    .await;
    let codes: Vec<Value> = responses
        .iter()
        .filter(|(name, _)| name == SPAWN_AGENT)
        .map(|(_, value)| value.get("code").cloned().unwrap_or(Value::Null))
        .collect();
    assert_eq!(
        codes,
        [
            Value::Null,
            Value::Null,
            json!("subagent_busy"),
            Value::Null,
            json!("subagent_limit"),
        ]
    );
    // An empty host section leaves the preamble alone.
    assert_eq!(host.instructions.lock().unwrap()[0], CHILD_PREAMBLE);
}

#[test]
fn limits_are_capped_to_the_runtimes() {
    let capped = SubAgentLimits {
        max_concurrent: 99,
        max_per_turn: 0,
        default_steps: 1_000,
        input_token_budget: u64::MAX,
    }
    .capped();
    assert_eq!(capped.max_concurrent, MAX_CONCURRENT_CHILDREN);
    assert_eq!(capped.max_per_turn, 1);
    assert_eq!(capped.default_steps, MAX_CHILD_STEPS);
    assert_eq!(capped.input_token_budget, MAX_INPUT_TOKEN_BUDGET);
}

/// A context for listing tools outside a run.
struct NoContext;

#[async_trait]
impl adk_core::ReadonlyContext for NoContext {
    fn invocation_id(&self) -> &'static str {
        "i"
    }
    fn agent_name(&self) -> &'static str {
        "a"
    }
    fn user_id(&self) -> &'static str {
        "u"
    }
    fn app_name(&self) -> &'static str {
        "app"
    }
    fn session_id(&self) -> &'static str {
        "s"
    }
    fn branch(&self) -> &'static str {
        ""
    }
    fn user_content(&self) -> &Content {
        static CONTENT: std::sync::OnceLock<Content> = std::sync::OnceLock::new();
        CONTENT.get_or_init(|| Content::new("user"))
    }
}
