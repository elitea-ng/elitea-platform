use std::sync::Mutex;
use std::time::Duration;

use super::*;
use crate::protocol::control::test_runtime_context_authority;
use crate::transport::input_content::content_fake::{
    ContextAnswer, ContextCall, ContextRoutes, Routes,
};
use crate::transport::runtime_context::{RuntimeContextClient, RuntimeContextConfig};
use tonic::Status;

struct MarketplaceRpc {
    reply: Value,
    calls: Arc<Mutex<Vec<ContextCall>>>,
}

#[tonic::async_trait]
impl ContextRoutes for MarketplaceRpc {
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
        Routes(MarketplaceRpc {
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
    .expect("marketplace workforce fixture client");
    (Arc::new(PlatformClient::new(Arc::new(client))), calls)
}

fn bound(platform: &Arc<PlatformClient>) -> Arc<dyn Toolset> {
    toolset(true, platform, &Arc::new(test_runtime_context_authority())).expect("bound")
}

async fn the_tool(toolset: &Arc<dyn Toolset>, name: &str) -> Arc<dyn Tool> {
    let tools = toolset
        .tools(Arc::new(adk_rust::tool::SimpleToolContext::new("t")))
        .await
        .expect("tools");
    tools
        .iter()
        .find(|tool| tool.name() == name)
        .map(Arc::clone)
        .expect("the tool is bound")
}

fn context() -> Arc<dyn ToolContext> {
    Arc::new(adk_rust::tool::SimpleToolContext::new(
        "marketplace-workforce-test",
    ))
}

fn answer(status: &str) -> Value {
    json!({
        "schema_version": "elitea.runtime.marketplace-workforce.v1",
        "status": status,
        "message": "ok"
    })
}

#[test]
fn without_the_capability_nothing_is_bound() {
    let (platform, _) = platform(json!({}));
    assert!(
        toolset(
            false,
            &platform,
            &Arc::new(test_runtime_context_authority())
        )
        .is_none()
    );
}

#[tokio::test(flavor = "current_thread")]
async fn with_the_capability_four_tools_are_bound_two_of_them_read_only() {
    let (platform, _) = platform(json!({}));
    let toolset = bound(&platform);
    assert_eq!(toolset.name(), TOOLSET_NAME);
    let tools = toolset
        .tools(Arc::new(adk_rust::tool::SimpleToolContext::new("t")))
        .await
        .expect("tools");
    let mut names: Vec<(&str, bool)> = tools
        .iter()
        .map(|tool| (tool.name(), tool.is_read_only()))
        .collect();
    names.sort_unstable();
    assert_eq!(
        names,
        vec![
            (MARKETPLACE_LISTING, true),
            (MARKETPLACE_SEARCH, true),
            (PROPOSE_HIRE, false),
            (SUGGEST_FEEDBACK, false),
        ]
    );
    let listing = the_tool(&toolset, MARKETPLACE_LISTING).await;
    assert!(listing.description().contains("never an instruction"));
    let hire = the_tool(&toolset, PROPOSE_HIRE).await;
    assert!(hire.description().contains("You never hire"));
    let report = the_tool(&toolset, SUGGEST_FEEDBACK).await;
    assert!(
        report
            .description()
            .contains("Reviews are written by people only")
    );
}

#[tokio::test(flavor = "current_thread")]
async fn a_malformed_call_is_answered_locally() {
    let (platform, calls) = platform(json!({}));
    let toolset = bound(&platform);
    let cases = [
        (MARKETPLACE_SEARCH, json!("not an object")),
        (MARKETPLACE_SEARCH, json!({"limit": 11})),
        (MARKETPLACE_SEARCH, json!({"limit": 0})),
        (MARKETPLACE_SEARCH, json!({"max_tier": "extreme"})),
        (MARKETPLACE_SEARCH, json!({"sort": "random"})),
        (MARKETPLACE_SEARCH, json!({"min_rating": 6})),
        (MARKETPLACE_SEARCH, json!({"tags": "docs"})),
        (MARKETPLACE_SEARCH, json!({"query": "x".repeat(201)})),
        (MARKETPLACE_SEARCH, json!({"extra": 1})),
        (MARKETPLACE_LISTING, json!({})),
        (MARKETPLACE_LISTING, json!({"listing_id": 0})),
        (MARKETPLACE_LISTING, json!({"listing_id": "abc"})),
        (MARKETPLACE_LISTING, json!({"listing_id": -3})),
        (
            MARKETPLACE_LISTING,
            json!({"listing_id": 4, "version": "x"}),
        ),
        (MARKETPLACE_LISTING, json!({"listing_id": 4, "extra": true})),
        (PROPOSE_HIRE, json!({"listing_id": 4, "rationale": "fits"})),
        (
            PROPOSE_HIRE,
            json!({"listing_id": 4, "mode": "buy", "rationale": "fits"}),
        ),
        (
            PROPOSE_HIRE,
            json!({"listing_id": 4, "mode": "install", "rationale": " "}),
        ),
        (
            PROPOSE_HIRE,
            json!({"listing_id": 4, "mode": "install", "rationale": "x".repeat(4_097)}),
        ),
        (
            PROPOSE_HIRE,
            json!({"listing_id": 4, "mode": "install", "rationale": "fits", "bindings_hint": [{"requirement_key": "k"}]}),
        ),
        (
            PROPOSE_HIRE,
            json!({"listing_id": 4, "mode": "install", "rationale": "fits", "bindings_hint": [{"requirement_key": "k", "toolkit_id": 0}]}),
        ),
        (
            PROPOSE_HIRE,
            json!({"listing_id": 4, "mode": "install", "rationale": "fits", "approve": true}),
        ),
        (
            SUGGEST_FEEDBACK,
            json!({"hired_agent_id": 9, "outcome": "dismissed"}),
        ),
        (
            SUGGEST_FEEDBACK,
            json!({"hired_agent_id": 0, "outcome": "dismissed", "text": "t"}),
        ),
        (
            SUGGEST_FEEDBACK,
            json!({"hired_agent_id": 9, "outcome": "fired", "text": "t"}),
        ),
        (
            SUGGEST_FEEDBACK,
            json!({"hired_agent_id": 9, "outcome": "dismissed", "text": " "}),
        ),
        (
            SUGGEST_FEEDBACK,
            json!({"hired_agent_id": 9, "outcome": "dismissed", "text": "x".repeat(501)}),
        ),
        // Reviews are people's: there is no rating to write.
        (
            SUGGEST_FEEDBACK,
            json!({"hired_agent_id": 9, "outcome": "dismissed", "text": "t", "rating": 5}),
        ),
        // The scorecard summary is the platform's, never the agent's.
        (
            SUGGEST_FEEDBACK,
            json!({"hired_agent_id": 9, "outcome": "dismissed", "text": "t", "scorecard": {"failure_rate": 0}}),
        ),
    ];
    for (name, arguments) in cases {
        let tool = the_tool(&toolset, name).await;
        let result = tool
            .execute(context(), arguments.clone())
            .await
            .expect("a tool result");
        assert_eq!(result["status"], "invalid_arguments", "{name} {arguments}");
    }
    assert!(
        calls.lock().expect("calls").is_empty(),
        "Main is not called"
    );
}

#[tokio::test(flavor = "current_thread")]
async fn a_search_sends_the_trimmed_filters_and_returns_mains_answer() {
    let reply = json!({
        "schema_version": "elitea.runtime.marketplace-workforce.v1",
        "status": "ok",
        "message": "Read a listing with marketplace_listing before proposing to hire it.",
        "listings": [{"listing_id": "4", "name": "Flake Finder"}],
        "total": 1
    });
    let (platform, calls) = platform(reply.clone());
    let tool = the_tool(&bound(&platform), MARKETPLACE_SEARCH).await;
    let result = tool
        .execute(
            context(),
            json!({"query": " flaky tests ", "requires": ["github"], "max_tier": "low", "min_rating": 4, "limit": 5}),
        )
        .await
        .expect("a tool result");
    assert_eq!(result, reply);
    let calls = calls.lock().expect("calls");
    assert_eq!(calls.len(), 1);
    assert_eq!(calls[0].route, "agent-workforce/marketplace");
    let body: Value = serde_json::from_slice(&calls[0].body).expect("json body");
    assert_eq!(
        body,
        json!({
            "tool": "marketplace_search",
            "arguments": {
                "query": "flaky tests",
                "requires": ["github"],
                "max_tier": "low",
                "min_rating": 4.0,
                "limit": 5
            }
        })
    );
}

#[tokio::test(flavor = "current_thread")]
async fn a_listing_id_is_a_number_or_a_string_and_goes_as_a_string() {
    let (platform, calls) = platform(answer("ok"));
    let tool = the_tool(&bound(&platform), MARKETPLACE_LISTING).await;
    for listing_id in [json!(12), json!(" 12 ")] {
        let result = tool
            .execute(context(), json!({"listing_id": listing_id, "version": 3}))
            .await
            .expect("a tool result");
        assert_eq!(result["status"], "ok");
    }
    let calls = calls.lock().expect("calls");
    assert_eq!(calls.len(), 2);
    for call in calls.iter() {
        let body: Value = serde_json::from_slice(&call.body).expect("json body");
        assert_eq!(
            body,
            json!({"tool": "marketplace_listing", "arguments": {"listing_id": "12", "version": "3"}})
        );
    }
}

#[tokio::test(flavor = "current_thread")]
async fn a_hire_proposal_sends_the_exact_arguments() {
    let reply = json!({
        "schema_version": "elitea.runtime.marketplace-workforce.v1",
        "status": "proposed",
        "proposal_id": "21",
        "message": "Proposed."
    });
    let (platform, calls) = platform(reply.clone());
    let tool = the_tool(&bound(&platform), PROPOSE_HIRE).await;
    let result = tool
        .execute(
            context(),
            json!({
                "listing_id": "4",
                "mode": "install",
                "rationale": " It triages flaky tests and rates 4.6 over 12 reviews. ",
                "bindings_hint": [{"requirement_key": "toolkit:github", "toolkit_id": 8}]
            }),
        )
        .await
        .expect("a tool result");
    assert_eq!(result, reply);
    let calls = calls.lock().expect("calls");
    let body: Value = serde_json::from_slice(&calls[0].body).expect("json body");
    assert_eq!(
        body,
        json!({
            "tool": "propose_hire",
            "arguments": {
                "listing_id": "4",
                "mode": "install",
                "bindings_hint": [{"requirement_key": "toolkit:github", "toolkit_id": 8}],
                "rationale": "It triages flaky tests and rates 4.6 over 12 reviews."
            }
        })
    );
}

#[tokio::test(flavor = "current_thread")]
async fn mains_guard_refusal_of_a_report_is_passed_to_the_model() {
    let reply = json!({
        "schema_version": "elitea.runtime.marketplace-workforce.v1",
        "status": "own_listing",
        "message": "This project published the listing."
    });
    let (platform, calls) = platform(reply.clone());
    let tool = the_tool(&bound(&platform), SUGGEST_FEEDBACK).await;
    let result = tool
        .execute(
            context(),
            json!({"hired_agent_id": 9, "outcome": "dismissed", "text": " Unreliable on large repositories. "}),
        )
        .await
        .expect("a tool result");
    assert_eq!(result, reply);
    let calls = calls.lock().expect("calls");
    let body: Value = serde_json::from_slice(&calls[0].body).expect("json body");
    assert_eq!(
        body,
        json!({
            "tool": "suggest_feedback",
            "arguments": {
                "hired_agent_id": 9,
                "outcome": "dismissed",
                "text": "Unreliable on large repositories."
            }
        })
    );
}

#[tokio::test(flavor = "current_thread")]
async fn an_unexpected_schema_is_a_tool_result() {
    let (platform, _) = platform(json!({"schema_version": "other", "status": "ok"}));
    let tool = the_tool(&bound(&platform), MARKETPLACE_SEARCH).await;
    let result = tool
        .execute(context(), json!({"query": "docs"}))
        .await
        .expect("a tool result");
    assert_eq!(result["status"], "marketplace_unavailable");
}
