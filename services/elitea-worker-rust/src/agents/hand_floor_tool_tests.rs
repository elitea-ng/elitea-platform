use std::sync::Mutex;
use std::time::Duration;

use super::*;
use crate::agents::request::{AgentFloor, AgentFloorPeer, FloorCause};
use crate::protocol::control::test_runtime_context_authority;
use crate::transport::input_content::content_fake::{
    ContextAnswer, ContextCall, ContextRoutes, Routes,
};
use crate::transport::runtime_context::{RuntimeContextClient, RuntimeContextConfig};
use tonic::Status;

struct HandFloorRpc {
    reply: Value,
    calls: Arc<Mutex<Vec<ContextCall>>>,
}

#[tonic::async_trait]
impl ContextRoutes for HandFloorRpc {
    async fn answer(&self, call: ContextCall) -> Result<ContextAnswer, Status> {
        self.calls
            .lock()
            .map_err(|_| Status::unavailable("calls"))?
            .push(call);
        Ok(ContextAnswer::from(&self.reply))
    }
}

fn floor(peers: Vec<AgentFloorPeer>) -> AgentFloor {
    AgentFloor {
        cause: FloorCause::HumanMessage,
        peers,
        unattended_tools_only: false,
    }
}

fn peer(id: i64, name: &str) -> AgentFloorPeer {
    AgentFloorPeer {
        participant_id: id,
        name: name.to_owned(),
        role: "reviewer".to_owned(),
    }
}

fn platform(reply: Value) -> (Arc<PlatformClient>, Arc<Mutex<Vec<ContextCall>>>) {
    let calls = Arc::new(Mutex::new(Vec::new()));
    let client = RuntimeContextClient::with_rpc(
        Routes(HandFloorRpc {
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
    .expect("hand floor fixture client");
    (Arc::new(PlatformClient::new(Arc::new(client))), calls)
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
    Arc::new(adk_rust::tool::SimpleToolContext::new("hand-floor-test"))
}

#[test]
fn no_peers_binds_nothing() {
    let (platform, _) = platform(json!({}));
    assert!(
        toolset(
            &floor(Vec::new()),
            platform,
            Arc::new(test_runtime_context_authority())
        )
        .is_none()
    );
}

#[tokio::test(flavor = "current_thread")]
async fn the_description_names_every_peer() {
    let (platform, _) = platform(json!({}));
    let toolset = toolset(
        &floor(vec![peer(7, "Ada"), peer(9, "Lin")]),
        platform,
        Arc::new(test_runtime_context_authority()),
    )
    .expect("bound");
    assert_eq!(toolset.name(), TOOLSET_NAME);
    let tool = the_tool(&toolset).await;
    assert_eq!(tool.name(), HAND_FLOOR);
    assert!(!tool.is_read_only());
    assert!(
        tool.description()
            .contains("Ada (reviewer) — participant 7")
    );
    assert!(
        tool.description()
            .contains("Lin (reviewer) — participant 9")
    );
}

#[tokio::test(flavor = "current_thread")]
async fn a_malformed_call_is_answered_locally() {
    let (platform, calls) = platform(json!({}));
    let toolset = toolset(
        &floor(vec![peer(7, "Ada"), peer(9, "Lin")]),
        platform,
        Arc::new(test_runtime_context_authority()),
    )
    .expect("bound");
    let tool = the_tool(&toolset).await;
    for arguments in [
        json!({"participant_ids": [], "message": "go"}),
        json!({"participant_ids": [8], "message": "go"}),
        json!({"participant_ids": [7, 7], "message": "go"}),
        json!({"participant_ids": [7, 9, 7, 9, 7], "message": "go"}),
        json!({"participant_ids": ["7"], "message": "go"}),
        json!({"participant_ids": [7], "message": "  "}),
        json!({"participant_ids": [7], "message": "x".repeat(4_001)}),
        json!({"participant_ids": [7], "message": "go", "extra": true}),
        json!("not an object"),
    ] {
        let result = tool
            .execute(context(), arguments.clone())
            .await
            .expect("a tool result");
        assert_eq!(result["status"], "error", "{arguments}");
        assert_eq!(result["code"], "invalid_argument", "{arguments}");
    }
    assert!(
        calls.lock().expect("calls").is_empty(),
        "Main is not called"
    );
}

#[tokio::test(flavor = "current_thread")]
async fn a_valid_call_returns_mains_answer() {
    let answer = json!({
        "schema_version": "elitea.runtime.hand-floor.v1",
        "handed_to": [{"participant_id": 9, "name": "Lin"}],
        "stopped": "",
        "message": "Lin answers after your turn."
    });
    let (platform, calls) = platform(answer.clone());
    let toolset = toolset(
        &floor(vec![peer(7, "Ada"), peer(9, "Lin")]),
        platform,
        Arc::new(test_runtime_context_authority()),
    )
    .expect("bound");
    let tool = the_tool(&toolset).await;
    let result = tool
        .execute(
            context(),
            json!({"participant_ids": [9], "message": "review the patch"}),
        )
        .await
        .expect("a tool result");
    assert_eq!(result, answer);
    let calls = calls.lock().expect("calls");
    assert_eq!(calls.len(), 1);
    assert_eq!(calls[0].route, "turn-taking/hand-floor");
    let body: Value = serde_json::from_slice(&calls[0].body).expect("json body");
    assert_eq!(
        body,
        json!({"participant_ids": [9], "message": "review the patch"})
    );
    assert_eq!(
        std::str::from_utf8(&calls[0].body).expect("utf8"),
        r#"{"participant_ids":[9],"message":"review the patch"}"#
    );
}

#[tokio::test(flavor = "current_thread")]
async fn an_unexpected_schema_is_a_tool_result() {
    let (platform, _) = platform(json!({"schema_version": "other"}));
    let toolset = toolset(
        &floor(vec![peer(9, "Lin")]),
        platform,
        Arc::new(test_runtime_context_authority()),
    )
    .expect("bound");
    let tool = the_tool(&toolset).await;
    let result = tool
        .execute(context(), json!({"participant_ids": [9], "message": "go"}))
        .await
        .expect("a tool result");
    assert_eq!(result["status"], "error");
}

struct FixedTool {
    name: &'static str,
    read_only: bool,
}

#[async_trait]
impl Tool for FixedTool {
    fn name(&self) -> &str {
        self.name
    }

    fn description(&self) -> &'static str {
        "fixture"
    }

    fn is_read_only(&self) -> bool {
        self.read_only
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        _arguments: Value,
    ) -> adk_rust::Result<Value> {
        Ok(json!({}))
    }
}

#[test]
fn an_unattended_turn_keeps_only_read_only_tools() {
    let mixed = FrozenToolset::new(
        "github",
        vec![
            Arc::new(FixedTool {
                name: "read_file",
                read_only: true,
            }) as Arc<dyn Tool>,
            Arc::new(FixedTool {
                name: "create_pr",
                read_only: false,
            }),
        ],
    );
    let writes_only = FrozenToolset::new(
        "mailer",
        vec![Arc::new(FixedTool {
            name: "send",
            read_only: false,
        }) as Arc<dyn Tool>],
    );
    let (retained, kept, dropped) = retain_read_only(vec![mixed, writes_only]);
    assert_eq!((kept, dropped), (1, 2));
    assert_eq!(retained.len(), 1);
    assert_eq!(retained[0].name(), "github");
    let names: Vec<&str> = retained[0].tools().iter().map(|tool| tool.name()).collect();
    assert_eq!(names, vec!["read_file"]);
}
