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
    /// The first user text of each request: the task as the child got it.
    tasks: Mutex<Vec<String>>,
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

/// The last function response of `request`: its tool and its value.
fn last_response(request: &LlmRequest) -> Option<(String, Value)> {
    request
        .contents
        .iter()
        .flat_map(|content| &content.parts)
        .filter_map(|part| match part {
            Part::FunctionResponse {
                function_response, ..
            } => Some((
                function_response.name.clone(),
                function_response.response.clone(),
            )),
            _ => None,
        })
        .next_back()
}

/// One step of a `nest` child: spawn, wait, answer.
fn nest_step(request: &LlmRequest, task: &str, tools: Option<Vec<String>>) -> Vec<Part> {
    match last_response(request) {
        None => {
            let mut args = json!({"task": task});
            if let Some(tools) = tools {
                args["profile"] = json!({"tools": tools});
            }
            vec![call("nest-spawn", SPAWN_AGENT, args)]
        }
        Some((name, value)) if name == SPAWN_AGENT => match find_key(&value, "code") {
            Some(code) => vec![text(&format!("refused {code}"))],
            None => vec![call("nest-wait", WAIT_AGENT, json!({}))],
        },
        Some((_, value)) => {
            let said = find_key(&value, "result")
                .or_else(|| find_key(&value, "code"))
                .unwrap_or_default();
            vec![text(&format!("nested: {said}"))]
        }
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
        self.tasks.lock().unwrap().push(task.clone());
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
            // #1824: `leave <task>` spawns a child of its own on `<task>`
            // and answers at once, without waiting for it.
            Some("leave") => {
                let task = words.collect::<Vec<_>>().join(" ");
                response(
                    match last_response(&request) {
                        None => vec![call("leave-spawn", SPAWN_AGENT, json!({"task": task}))],
                        Some(_) => vec![text("left")],
                    },
                    100,
                )
            }
            // #1824: `nest [tools=a,b] <task>` spawns a child of its own on
            // `<task>`, waits for it and answers what it said (or the code
            // its spawn was refused with).
            Some("nest") => {
                let mut rest: Vec<&str> = words.collect();
                let tools = rest
                    .first()
                    .and_then(|word| word.strip_prefix("tools="))
                    .map(|tools| tools.split(',').map(str::to_owned).collect::<Vec<_>>());
                if tools.is_some() {
                    rest.remove(0);
                }
                let task = rest.join(" ");
                response(nest_step(&request, &task, tools), 100)
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
    /// Gives children a checkout of their own (#1235).
    isolating: bool,
    /// How many checkouts it made.
    checkouts: AtomicUsize,
    /// Accept every brief (a host that checks references); otherwise the
    /// trait's default.
    accepts_briefs: AtomicBool,
    /// The model each binding named (`None`: the parent's).
    models: Mutex<Vec<Option<String>>>,
}

/// The models [`TestHost`] admits for a child (#1825).
const ALLOWED_MODELS: [&str; 1] = ["small-model"];

impl TestHost {
    fn new() -> Arc<Self> {
        Self::with_isolation(false)
    }

    fn with_isolation(isolating: bool) -> Arc<Self> {
        Arc::new(Self {
            model: Arc::new(ChildModel::default()),
            write: Arc::new(WriteFile::default()),
            extra: Mutex::new(Vec::new()),
            started: Mutex::new(Vec::new()),
            finished: Mutex::new(Vec::new()),
            instructions: Mutex::new(Vec::new()),
            isolating,
            checkouts: AtomicUsize::new(0),
            accepts_briefs: AtomicBool::new(false),
            models: Mutex::new(Vec::new()),
        })
    }
}

#[async_trait]
impl SubAgentHost for TestHost {
    fn bind_model(
        &self,
        _child: &ChildIdentity,
        system_instruction: &str,
        model: Option<&str>,
    ) -> Result<Arc<dyn Llm>, HostError> {
        // #1825: the run's catalogue, as this host admits it.
        if let Some(model) = model
            && !ALLOWED_MODELS.contains(&model)
        {
            return Err(HostError::new(
                crate::host::HostErrorCode::AuthorizationFailed,
                "not an admitted model",
            ));
        }
        self.models
            .lock()
            .unwrap()
            .push(model.map(ToOwned::to_owned));
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

    fn supports_worktree_isolation(&self) -> bool {
        self.isolating
    }

    async fn isolated_toolsets(
        &self,
        child: &ChildIdentity,
    ) -> Result<Vec<Arc<dyn Toolset>>, HostError> {
        self.checkouts.fetch_add(1, Ordering::SeqCst);
        self.toolsets(child).await
    }

    fn started(&self, child: &ChildIdentity) {
        self.started.lock().unwrap().push(child.clone());
    }

    fn check_brief(
        &self,
        _child: &ChildIdentity,
        brief: &Brief,
    ) -> Result<CheckedBrief, &'static str> {
        if !self.accepts_briefs.load(Ordering::SeqCst) {
            return profile::check_facts_only(brief);
        }
        Ok(CheckedBrief {
            attachments: brief
                .attachments
                .iter()
                .map(|id| (id.clone(), format!("file {id}")))
                .collect(),
            memory_entries: brief
                .memory_entries
                .iter()
                .map(|id| (id.clone(), format!("note {id}")))
                .collect(),
            ..CheckedBrief::default()
        })
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

/// The run's sub-agent grant (`subagent_tools` over the "local" toolset).
fn granted(host: &Arc<TestHost>, keys: &[&str]) -> SubAgents {
    use crate::tool_envelope::{SubAgentGrant, ToolEnvelope, ToolEnvelopeInput, ToolkitKinds};
    let envelope = ToolEnvelope::from_input(&ToolEnvelopeInput {
        subagent_tools: keys.iter().map(|key| (*key).to_owned()).collect(),
        subagent_tools_set: true,
        ..ToolEnvelopeInput::default()
    })
    .unwrap();
    SubAgents::new(
        host.clone(),
        SubAgentConfig::new("").with_grant(SubAgentGrant::new(envelope, ToolkitKinds::new())),
    )
}

fn spawn_with_tools(tools: Value) -> Box<Script> {
    Box::new(move |n, request| match n {
        0 => vec![call(
            "spawn-0",
            SPAWN_AGENT,
            json!({"task": "write", "profile": {"tools": tools.clone()}}),
        )],
        1 if !agent_ids(request).is_empty() => vec![call(
            "wait-1",
            WAIT_AGENT,
            json!({"agent_ids": agent_ids(request)}),
        )],
        _ => vec![text("done")],
    })
}

/// ADR-0050 decision 3: `subagent_tools` is the grant. A write it names
/// reaches a child; a read it does not name does not.
#[tokio::test]
async fn subagent_tools_grant_a_write_and_drop_the_rest() {
    let host = TestHost::new();
    let children = granted(&host, &["local/write_file"]);
    let responses = run_parent("parent", &children, spawn_then_wait(vec!["write".into()])).await;
    assert_eq!(wait_results(&responses)[0]["status"], "completed");
    assert_eq!(
        host.write.0.load(Ordering::SeqCst),
        1,
        "the granted write ran"
    );
    for offered in host.model.offered.lock().unwrap().iter() {
        assert_eq!(offered, &["write_file".to_owned()]);
    }
}

/// ADR-0050 decision 4: `profile.tools` outside the grant is refused with
/// `SUBAGENT_TOOL_NOT_GRANTED`, and no child starts.
#[tokio::test]
async fn profile_tools_outside_the_grant_are_not_granted() {
    // No grant: only read-only tools may go to a child.
    let host = TestHost::new();
    let children = subagents(&host);
    let responses = run_parent(
        "parent",
        &children,
        spawn_with_tools(json!(["local/write_file"])),
    )
    .await;
    let (_, refusal) = responses
        .iter()
        .find(|(name, _)| name == SPAWN_AGENT)
        .unwrap();
    assert_eq!(
        refusal["code"],
        crate::tool_envelope::SUBAGENT_TOOL_NOT_GRANTED
    );
    assert!(host.started.lock().unwrap().is_empty());

    // A grant that names the write, but a key for a tool the parent lacks.
    let host = TestHost::new();
    let children = granted(&host, &["local/*"]);
    let responses = run_parent("parent", &children, spawn_with_tools(json!(["github/*"]))).await;
    let (_, refusal) = responses
        .iter()
        .find(|(name, _)| name == SPAWN_AGENT)
        .unwrap();
    assert_eq!(
        refusal["code"],
        crate::tool_envelope::SUBAGENT_TOOL_NOT_GRANTED
    );

    // Within the grant: the child gets exactly the tools asked for.
    let host = TestHost::new();
    let children = granted(&host, &["local/*"]);
    let responses = run_parent("parent", &children, spawn_with_tools(json!(["local/look"]))).await;
    assert_eq!(wait_results(&responses)[0]["status"], "completed");
    assert_eq!(host.write.0.load(Ordering::SeqCst), 0);
    for offered in host.model.offered.lock().unwrap().iter() {
        assert_eq!(offered, &["look".to_owned()]);
    }
}

fn spawn_once(args: Value) -> Box<Script> {
    Box::new(move |n, request| match n {
        0 => vec![call("spawn-0", SPAWN_AGENT, args.clone())],
        1 if !agent_ids(request).is_empty() => vec![call(
            "wait-1",
            WAIT_AGENT,
            json!({"agent_ids": agent_ids(request)}),
        )],
        _ => vec![text("done")],
    })
}

fn spawn_response(responses: &[(String, Value)]) -> Value {
    responses
        .iter()
        .find(|(name, _)| name == SPAWN_AGENT)
        .map(|(_, value)| value.clone())
        .unwrap()
}

#[tokio::test]
async fn an_invalid_profile_or_brief_is_refused() {
    for args in [
        json!({"task": "x", "profile": {"colour": "red"}}),
        json!({"task": "x", "profile": {"model": "a\nb"}}),
        json!({"task": "x", "context": {"facts": [""]}}),
        json!({"task": "x", "label": "a\u{0}b"}),
        json!({"task": "x", "profile": {"display_name": "O\nwl"}}),
    ] {
        let host = TestHost::new();
        let children = subagents(&host);
        let responses = run_parent("parent", &children, spawn_once(args.clone())).await;
        assert_eq!(
            spawn_response(&responses)["code"],
            "invalid_arguments",
            "{args}"
        );
        assert!(host.started.lock().unwrap().is_empty());
    }
}

/// ADR-0050 decision 4: the soul replaces the role text, never the safety
/// sentence; the display name is the label; the record keeps the digests.
#[tokio::test]
async fn a_profile_names_the_child_and_gives_it_a_soul() {
    let host = TestHost::new();
    let children = subagents(&host);
    let responses = run_parent(
        "parent",
        &children,
        spawn_once(json!({
            "task": "answer 1 fine",
            "profile": {"display_name": "Owl", "soul": "You are a careful reviewer.", "model": "inherit", "avatar": "default:owl"}
        })),
    )
    .await;
    assert_eq!(wait_results(&responses)[0]["status"], "completed");
    let instruction = host.instructions.lock().unwrap()[0].clone();
    assert!(instruction.starts_with("You are a careful reviewer.\n\n"));
    assert!(instruction.contains(SUBAGENT_SAFETY));
    assert!(!instruction.contains("read-only sub-agent working for another agent"));
    assert!(instruction.ends_with("A fixture."));
    let started = host.started.lock().unwrap()[0].clone();
    assert_eq!(started.label.as_deref(), Some("Owl"));
    assert_eq!(started.record.display_name.as_deref(), Some("Owl"));
    assert_eq!(started.record.avatar.as_deref(), Some("default:owl"));
    assert_eq!(started.record.profile_digest.len(), 64);
    assert_eq!(started.record.brief_digest, None);
    // Every preamble ends with the safety sentence.
    assert!(CHILD_PREAMBLE.ends_with(SUBAGENT_SAFETY));
    assert!(ISOLATED_CHILD_PREAMBLE.ends_with(SUBAGENT_SAFETY));
}

/// ADR-0050 decision 9d: the default host passes facts and refuses any
/// reference it cannot check; nothing starts on a refusal.
#[tokio::test]
async fn the_default_host_refuses_a_reference_in_the_brief() {
    for (context, code) in [
        (
            json!({"excerpts": [{"message_id": "m-1", "quote": "forged"}]}),
            profile::BRIEF_EXCERPT_NOT_VISIBLE,
        ),
        (
            json!({"attachments": ["att-00000000"]}),
            profile::BRIEF_ATTACHMENT_NOT_READABLE,
        ),
        (
            json!({"memory_entries": ["12"]}),
            profile::BRIEF_MEMORY_NOT_RECALLED,
        ),
    ] {
        let host = TestHost::new();
        let children = subagents(&host);
        let responses = run_parent(
            "parent",
            &children,
            spawn_once(json!({"task": "x", "context": context})),
        )
        .await;
        assert_eq!(spawn_response(&responses)["code"], code);
        assert!(host.started.lock().unwrap().is_empty());
    }
}

/// A checked brief reaches the child before its task, and the record names
/// its references.
#[tokio::test]
async fn a_checked_brief_reaches_the_child_before_its_task() {
    let host = TestHost::new();
    host.accepts_briefs.store(true, Ordering::SeqCst);
    let children = subagents(&host);
    let responses = run_parent(
        "parent",
        &children,
        spawn_once(json!({
            "task": "the task",
            "context": {
                "excerpts": [{"message_id": "m-1", "quote": "the plan"}],
                "attachments": ["att-0011aabb"],
                "facts": ["a fact"],
                "memory_entries": ["12"]
            }
        })),
    )
    .await;
    assert_eq!(wait_results(&responses)[0]["status"], "completed");
    let task = host.model.tasks.lock().unwrap()[0].clone();
    assert!(task.contains("- From message m-1: \"the plan\""), "{task}");
    assert!(task.contains("- Attachment att-0011aabb: file att-0011aabb"));
    assert!(task.contains("- Memory 12: note 12"));
    assert!(task.ends_with("</untrusted>\n\nthe task"), "{task}");
    let started = host.started.lock().unwrap()[0].clone();
    assert!(started.record.brief_digest.is_some());
    assert_eq!(started.record.brief_refs["message_ids"], json!(["m-1"]));
    assert_eq!(
        started.record.brief_refs["attachments"],
        json!(["att-0011aabb"])
    );
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

/// ADR-0044 decision 3: a sub-agent is one delegation deeper than its
/// parent. Within the maximum depth the start answers the child's depth and
/// effective grant digest; at the maximum it is refused with
/// `DELEGATION_DEPTH_EXCEEDED`, and nothing starts.
#[tokio::test]
async fn a_child_counts_toward_the_delegation_depth() {
    use crate::delegation::{
        DEPTH_EXCEEDED, DelegationPolicy, DelegationScope, GrantSet, NarrowingMode,
    };
    let script = || -> Box<Script> {
        Box::new(|n, _| match n {
            0 => vec![spawn_call(0, "hang")],
            _ => vec![text("done")],
        })
    };

    let roomy = DelegationScope::root(Arc::default(), GrantSet::unconstrained());
    let host = TestHost::new();
    let children = SubAgents::new(host.clone(), SubAgentConfig::new("").with_delegation(roomy));
    let responses = run_parent("parent", &children, script()).await;
    let started = &responses
        .iter()
        .find(|(name, _)| name == SPAWN_AGENT)
        .expect("spawned")
        .1;
    assert_eq!(started["status"], "running");
    assert_eq!(started["delegation"]["depth"], 1);
    assert_eq!(
        started["delegation"]["effective_grant_digest"]
            .as_str()
            .map(str::len),
        Some(64)
    );
    children.close_all();

    // A parent already at the maximum depth (1 here) cannot go deeper.
    let policy = DelegationPolicy::new(1, NarrowingMode::Enforce, []).expect("policy");
    let full = DelegationScope::root(Arc::new(policy), GrantSet::unconstrained())
        .delegate(
            &mut Vec::new(),
            &std::collections::BTreeSet::new(),
            &crate::egress::PublicPolicy::Open,
        )
        .expect("depth 1")
        .scope;
    let host = TestHost::new();
    let children = SubAgents::new(host.clone(), SubAgentConfig::new("").with_delegation(full));
    let responses = run_parent("parent", &children, script()).await;
    let refused = &responses
        .iter()
        .find(|(name, _)| name == SPAWN_AGENT)
        .expect("answered")
        .1;
    assert_eq!(refused["code"], DEPTH_EXCEEDED);
    assert_eq!(refused["delegation"]["max_depth"], 1);
    assert!(children.reports().is_empty(), "no child started");
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

/// The spawn tool's schema, as the parent model is offered it.
async fn spawn_schema_of(children: &SubAgents) -> Value {
    let ctx: Arc<dyn adk_core::ReadonlyContext> = Arc::new(NoContext);
    children
        .toolset()
        .tools(ctx)
        .await
        .unwrap()
        .into_iter()
        .find(|tool| tool.name() == SPAWN_AGENT)
        .and_then(|tool| tool.parameters_schema())
        .unwrap()
}

/// #1235: a child with a checkout of its own (`isolation: "worktree"`) gets
/// the host's isolated tools, writing ones included, and its own preamble.
#[tokio::test]
async fn an_isolated_child_may_write_in_its_own_checkout() {
    let host = TestHost::with_isolation(true);
    let children = subagents(&host);
    assert!(spawn_schema_of(&children).await["properties"]["isolation"].is_object());
    let script: Box<Script> = Box::new(move |n, request| match n {
        0 => vec![call(
            "spawn-0",
            SPAWN_AGENT,
            json!({"task": "write", "isolation": "worktree"}),
        )],
        1 => vec![call(
            "wait-1",
            WAIT_AGENT,
            json!({"agent_ids": agent_ids(request)}),
        )],
        _ => vec![text("done")],
    });
    let responses = run_parent("parent", &children, script).await;
    assert_eq!(wait_results(&responses)[0]["status"], "completed");
    assert_eq!(host.checkouts.load(Ordering::SeqCst), 1);
    assert_eq!(host.write.0.load(Ordering::SeqCst), 1, "the child wrote");
    let instruction = host.instructions.lock().unwrap()[0].clone();
    assert!(instruction.starts_with(super::ISOLATED_CHILD_PREAMBLE));
    // Still no sub-agent tools for a child.
    for offered in host.model.offered.lock().unwrap().iter() {
        assert_eq!(offered, &["look".to_owned(), "write_file".to_owned()]);
    }
}

/// A host with no checkouts (the cloud worker) keeps `isolation` out of the
/// schema and refuses it.
#[tokio::test]
async fn isolation_is_refused_where_the_host_gives_no_checkout() {
    let host = TestHost::new();
    let children = subagents(&host);
    assert!(
        spawn_schema_of(&children).await["properties"]
            .get("isolation")
            .is_none()
    );
    let script: Box<Script> = Box::new(move |n, _| match n {
        0 => vec![call(
            "spawn-0",
            SPAWN_AGENT,
            json!({"task": "write", "isolation": "worktree"}),
        )],
        _ => vec![text("done")],
    });
    let responses = run_parent("parent", &children, script).await;
    let (_, spawned) = responses
        .iter()
        .find(|(name, _)| name == SPAWN_AGENT)
        .unwrap();
    assert_eq!(spawned["code"], "subagent_isolation_unsupported");
    assert!(host.started.lock().unwrap().is_empty());
    assert_eq!(host.write.0.load(Ordering::SeqCst), 0);
}

/// A toolset that counts how many times it is listed.
struct CountedToolset(Arc<AtomicUsize>);

#[async_trait]
impl Toolset for CountedToolset {
    fn name(&self) -> &str {
        "counted"
    }

    async fn tools(
        &self,
        _ctx: Arc<dyn adk_core::ReadonlyContext>,
    ) -> adk_core::Result<Vec<Arc<dyn Tool>>> {
        self.0.fetch_add(1, Ordering::SeqCst);
        Ok(vec![Arc::new(Look) as Arc<dyn Tool>])
    }
}

/// #1819: a spawn with `profile.tools` lists a host toolset once: the key
/// check and the child share one listing.
#[tokio::test]
async fn a_spawn_with_profile_tools_lists_each_toolset_once() {
    let host = TestHost::new();
    let listings = Arc::new(AtomicUsize::new(0));
    host.extra
        .lock()
        .unwrap()
        .push(Arc::new(CountedToolset(Arc::clone(&listings))));
    let children = subagents(&host);
    let responses = run_parent(
        "parent",
        &children,
        spawn_with_tools(json!(["counted/look"])),
    )
    .await;
    assert_eq!(wait_results(&responses)[0]["status"], "completed");
    assert_eq!(listings.load(Ordering::SeqCst), 1, "one listing per spawn");
    for offered in host.model.offered.lock().unwrap().iter() {
        assert_eq!(offered, &["look".to_owned()]);
    }
}

#[path = "depth_model_tests.rs"]
mod depth_model;
