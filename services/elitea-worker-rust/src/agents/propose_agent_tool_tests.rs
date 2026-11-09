use std::sync::Mutex;
use std::time::Duration;

use super::*;
use crate::protocol::control::test_runtime_context_authority;
use crate::transport::input_content::content_fake::{
    ContextAnswer, ContextCall, ContextRoutes, Routes,
};
use crate::transport::runtime_context::{RuntimeContextClient, RuntimeContextConfig};
use elitea_agent_runtime::subagents::ChildIdentity;
use elitea_agent_runtime::subagents::profile::{Brief, SpawnRecord};
use elitea_agent_runtime::tool_envelope::RequestedTool;
use tonic::Status;

struct ProposeRpc {
    reply: Value,
    calls: Arc<Mutex<Vec<ContextCall>>>,
}

#[tonic::async_trait]
impl ContextRoutes for ProposeRpc {
    async fn answer(&self, call: ContextCall) -> Result<ContextAnswer, Status> {
        self.calls
            .lock()
            .map_err(|_| Status::unavailable("calls"))?
            .push(call);
        Ok(ContextAnswer::from(&self.reply))
    }
}

fn platform(reply: Value) -> (Arc<PlatformClient>, Arc<Mutex<Vec<ContextCall>>>) {
    let calls = Arc::new(Mutex::new(Vec::new()));
    let client = RuntimeContextClient::with_rpc(
        Routes(ProposeRpc {
            reply,
            calls: Arc::clone(&calls),
        }),
        RuntimeContextConfig {
            origin: "https://content.internal".to_owned(),
            deadline: Duration::from_secs(1),
            max_response_bytes: 32 * 1_024,
            max_application_response_bytes: 1_024 * 1_024,
            max_attachment_response_bytes: 1_024 * 1_024,
            max_artifact_response_bytes: 1_024 * 1_024,
        },
    )
    .expect("propose agent fixture client");
    (Arc::new(PlatformClient::new(Arc::new(client))), calls)
}

/// A ledger with one spawn, `a_0000002a`, labelled Researcher.
fn ledger() -> Arc<EphemeralAgentLedger> {
    let ledger = Arc::new(EphemeralAgentLedger::default());
    let profile = Profile {
        display_name: Some("Ada".to_owned()),
        soul: Some("Precise and brief.".to_owned()),
        tools: Some(vec![
            RequestedTool::parse("GitHub/Read_File").expect("tool"),
            RequestedTool::parse("search/*").expect("tool"),
        ]),
        avatar: None,
    };
    ledger.started(
        &ChildIdentity {
            agent_id: "a_0000002a".to_owned(),
            parent_call_id: "call-1".to_owned(),
            label: Some("Researcher".to_owned()),
            ordinal: 1,
            record: SpawnRecord::new(&profile, &Brief::default()),
        },
        1,
    );
    ledger
}

fn kinds() -> BTreeMap<String, String> {
    BTreeMap::from([("GitHub".to_owned(), "github".to_owned())])
}

fn bound(platform: Arc<PlatformClient>) -> Arc<dyn Toolset> {
    toolset(
        true,
        ledger(),
        kinds(),
        platform,
        Arc::new(test_runtime_context_authority()),
    )
    .expect("bound")
}

async fn the_tool(toolset: &Arc<dyn Toolset>) -> Arc<dyn Tool> {
    let tools = toolset
        .tools(Arc::new(adk_rust::tool::SimpleToolContext::new("t")))
        .await
        .expect("tools");
    assert_eq!(tools.len(), 1);
    Arc::clone(&tools[0])
}

fn context() -> Arc<dyn ToolContext> {
    Arc::new(adk_rust::tool::SimpleToolContext::new("propose-agent-test"))
}

#[test]
fn without_the_capability_nothing_is_bound() {
    let (platform, _) = platform(json!({}));
    assert!(
        toolset(
            false,
            ledger(),
            kinds(),
            platform,
            Arc::new(test_runtime_context_authority())
        )
        .is_none()
    );
}

#[tokio::test(flavor = "current_thread")]
async fn with_the_capability_the_tool_is_bound_and_writes() {
    let (platform, _) = platform(json!({}));
    let toolset = bound(platform);
    assert_eq!(toolset.name(), TOOLSET_NAME);
    let tool = the_tool(&toolset).await;
    assert_eq!(tool.name(), PROPOSE_AGENT);
    assert!(!tool.is_read_only());
    assert!(tool.description().contains("nothing is created until"));
}

#[tokio::test(flavor = "current_thread")]
async fn a_malformed_call_is_answered_locally() {
    let (platform, calls) = platform(json!({}));
    let tool = the_tool(&bound(platform)).await;
    for arguments in [
        json!("not an object"),
        json!({"ephemeral_agent_id": "a_0000002a"}),
        json!({"ephemeral_agent_id": "a_0000002A", "rationale": "why"}),
        json!({"ephemeral_agent_id": "agent", "rationale": "why"}),
        json!({"ephemeral_agent_id": "a_0000002a", "rationale": "  "}),
        json!({"ephemeral_agent_id": "a_0000002a", "rationale": "x".repeat(4_097)}),
        json!({"ephemeral_agent_id": "a_0000002a", "rationale": "why", "extra": 1}),
        json!({"ephemeral_agent_id": "a_0000002a", "rationale": "why", "memory_seeds": [{"kind": "secret", "content": "x"}]}),
        json!({"ephemeral_agent_id": "a_0000002a", "rationale": "why", "memory_seeds": [{"content": ""}]}),
        json!({"ephemeral_agent_id": "a_0000002a", "rationale": "why", "memory_seeds": "x"}),
    ] {
        let result = tool
            .execute(context(), arguments.clone())
            .await
            .expect("a tool result");
        assert_eq!(result["status"], "invalid_arguments", "{arguments}");
    }
    assert!(
        calls.lock().expect("calls").is_empty(),
        "Main is not called"
    );
}

#[tokio::test(flavor = "current_thread")]
async fn a_spawn_this_run_did_not_start_is_unknown_locally() {
    let (platform, calls) = platform(json!({}));
    let tool = the_tool(&bound(platform)).await;
    let result = tool
        .execute(
            context(),
            json!({"ephemeral_agent_id": "a_ffffffff", "rationale": "why"}),
        )
        .await
        .expect("a tool result");
    assert_eq!(result["status"], "ephemeral_agent_unknown");
    assert!(calls.lock().expect("calls").is_empty());
}

#[tokio::test(flavor = "current_thread")]
async fn a_valid_call_sends_the_spawns_whole_profile_and_returns_mains_answer() {
    let answer = json!({
        "schema_version": "elitea.runtime.propose-agent.v1",
        "status": "proposed",
        "proposal_id": "12",
        "message": "Proposed."
    });
    let (platform, calls) = platform(answer.clone());
    let tool = the_tool(&bound(platform)).await;
    let result = tool
        .execute(
            context(),
            json!({
                "ephemeral_agent_id": "a_0000002a",
                "rationale": " It found the bug fast. ",
                "instructions": "Review pull requests.",
                "memory_seeds": [{"content": "The repo uses Go 1.26."}, {"kind": "preference", "content": "Short answers."}]
            }),
        )
        .await
        .expect("a tool result");
    assert_eq!(result, answer);
    let calls = calls.lock().expect("calls");
    assert_eq!(calls.len(), 1);
    assert_eq!(calls[0].route, "agent-proposals/propose");
    let body: Value = serde_json::from_slice(&calls[0].body).expect("json body");
    assert_eq!(
        body,
        json!({
            "ephemeral_agent_id": "a_0000002a",
            "rationale": "It found the bug fast.",
            "instructions": "Review pull requests.",
            "profile": {
                "label": "Researcher",
                "display_name": "Ada",
                "soul": "Precise and brief.",
                "avatar": "",
                "tools": [
                    {"key": "github/read_file", "toolkit_type": "github"},
                    {"key": "search/*"}
                ]
            },
            "memory_seeds": [
                {"kind": "fact", "content": "The repo uses Go 1.26."},
                {"kind": "preference", "content": "Short answers."}
            ]
        })
    );
}

#[tokio::test(flavor = "current_thread")]
async fn mains_refusal_is_passed_to_the_model() {
    let answer = json!({
        "schema_version": "elitea.runtime.propose-agent.v1",
        "status": "not_allowed",
        "message": "The person you work for cannot create agents in this project."
    });
    let (platform, _) = platform(answer.clone());
    let tool = the_tool(&bound(platform)).await;
    let result = tool
        .execute(
            context(),
            json!({"ephemeral_agent_id": "a_0000002a", "rationale": "why"}),
        )
        .await
        .expect("a tool result");
    assert_eq!(result, answer);
}

#[tokio::test(flavor = "current_thread")]
async fn an_unexpected_schema_is_a_tool_result() {
    let (platform, _) = platform(json!({"schema_version": "other", "status": "proposed"}));
    let tool = the_tool(&bound(platform)).await;
    let result = tool
        .execute(
            context(),
            json!({"ephemeral_agent_id": "a_0000002a", "rationale": "why"}),
        )
        .await
        .expect("a tool result");
    assert_eq!(result["status"], "proposals_unavailable");
}
