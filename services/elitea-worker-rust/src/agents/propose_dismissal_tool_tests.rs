use std::sync::Mutex;
use std::time::Duration;

use super::*;
use crate::protocol::control::test_runtime_context_authority;
use crate::transport::input_content::content_fake::{
    ContextAnswer, ContextCall, ContextRoutes, Routes,
};
use crate::transport::runtime_context::{RuntimeContextClient, RuntimeContextConfig};
use tonic::Status;

struct DismissalRpc {
    reply: Value,
    calls: Arc<Mutex<Vec<ContextCall>>>,
}

#[tonic::async_trait]
impl ContextRoutes for DismissalRpc {
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
        Routes(DismissalRpc {
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
    .expect("propose dismissal fixture client");
    (Arc::new(PlatformClient::new(Arc::new(client))), calls)
}

fn bound(platform: Arc<PlatformClient>) -> Arc<dyn Toolset> {
    toolset(true, platform, Arc::new(test_runtime_context_authority())).expect("bound")
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
    Arc::new(adk_rust::tool::SimpleToolContext::new(
        "propose-dismissal-test",
    ))
}

#[test]
fn without_the_capability_nothing_is_bound() {
    let (platform, _) = platform(json!({}));
    assert!(toolset(false, platform, Arc::new(test_runtime_context_authority())).is_none());
}

#[tokio::test(flavor = "current_thread")]
async fn with_the_capability_the_tool_is_bound() {
    let (platform, _) = platform(json!({}));
    let toolset = bound(platform);
    assert_eq!(toolset.name(), TOOLSET_NAME);
    let tool = the_tool(&toolset).await;
    assert_eq!(tool.name(), PROPOSE_DISMISSAL);
    assert!(!tool.is_read_only());
    assert!(tool.description().contains("You never decide"));
}

#[tokio::test(flavor = "current_thread")]
async fn a_malformed_call_is_answered_locally() {
    let (platform, calls) = platform(json!({}));
    let tool = the_tool(&bound(platform)).await;
    for arguments in [
        json!("not an object"),
        json!({"stage": "watch", "rationale": "why"}),
        json!({"application_id": 0, "stage": "watch", "rationale": "why"}),
        json!({"application_id": 9, "stage": "delete", "rationale": "why"}),
        json!({"application_id": 9, "stage": "watch", "rationale": " "}),
        json!({"application_id": 9, "stage": "watch", "rationale": "x".repeat(4_097)}),
        json!({"application_id": 9, "stage": "watch", "rationale": "why", "extra": 1}),
        json!({"application_id": 9, "stage": "demote", "rationale": "why"}),
        json!({"application_id": 9, "stage": "demote", "rationale": "why", "narrowing": {}}),
        json!({"application_id": 9, "stage": "suspend", "rationale": "why", "narrowing": {"remove_lead": true}}),
        json!({"application_id": 9, "stage": "demote", "rationale": "why", "narrowing": {"remove_toolkit_ids": [-1]}}),
        json!({"application_id": 9, "stage": "demote", "rationale": "why", "narrowing": {"remove_capabilities": ["admin"]}}),
        json!({"application_id": 9, "stage": "demote", "rationale": "why", "narrowing": {"memory_review": "yes"}}),
        json!({"application_id": 9, "stage": "demote", "rationale": "why", "narrowing": {"grant": true}}),
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
async fn a_valid_demotion_sends_the_exact_narrowing_and_returns_mains_answer() {
    let answer = json!({
        "schema_version": "elitea.runtime.propose-dismissal.v1",
        "status": "proposed",
        "proposal_id": "7",
        "message": "Proposed."
    });
    let (platform, calls) = platform(answer.clone());
    let tool = the_tool(&bound(platform)).await;
    let result = tool
        .execute(
            context(),
            json!({
                "application_id": 9,
                "stage": "demote",
                "rationale": " Failed 40% of runs this month. ",
                "narrowing": {"remove_toolkit_ids": [3], "remove_lead": true, "remove_capabilities": ["can_propose_agents"]}
            }),
        )
        .await
        .expect("a tool result");
    assert_eq!(result, answer);
    let calls = calls.lock().expect("calls");
    assert_eq!(calls.len(), 1);
    assert_eq!(calls[0].route, "agent-workforce/propose-dismissal");
    let body: Value = serde_json::from_slice(&calls[0].body).expect("json body");
    assert_eq!(
        body,
        json!({
            "application_id": 9,
            "stage": "demote",
            "rationale": "Failed 40% of runs this month.",
            "narrowing": {
                "remove_toolkit_ids": [3],
                "remove_capabilities": ["can_propose_agents"],
                "remove_lead": true
            }
        })
    );
}

#[tokio::test(flavor = "current_thread")]
async fn mains_guard_refusal_is_passed_to_the_model() {
    let answer = json!({
        "schema_version": "elitea.runtime.propose-dismissal.v1",
        "status": "guard_refused",
        "message": "You cannot propose a stage for the agent that hired you."
    });
    let (platform, _) = platform(answer.clone());
    let tool = the_tool(&bound(platform)).await;
    let result = tool
        .execute(
            context(),
            json!({"application_id": 9, "stage": "retire", "rationale": "unused 90 days"}),
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
            json!({"application_id": 9, "stage": "watch", "rationale": "why"}),
        )
        .await
        .expect("a tool result");
    assert_eq!(result["status"], "workforce_unavailable");
}
