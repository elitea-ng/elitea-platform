//! The reusable runtime's log events (ADR-0027) are visible in the Worker's
//! process log, so none of them may carry a data value. Every marker below is
//! planted in the input of a runtime path that emits an event, and the
//! production log layer at its most verbose level must never print it.

use std::collections::{BTreeMap, HashMap, VecDeque};
use std::sync::Arc;

use adk_rust::graph::{ExecutionConfig, Node as _, NodeContext};
use adk_rust::tool::SimpleToolContext;
use elitea_agent_runtime::context_management::ContextManagementPlan;
use elitea_agent_runtime::graph::router::{RouterNode, RouterNodeDefinition};
use elitea_agent_runtime::toolkits::{
    FrozenToolSnapshot, ToolAdmissionPolicy,
    materialize_configured_toolsets_with_tokens_and_authorization,
};
use reqwest::StatusCode;
use serde_json::{Value, json};
use tracing_subscriber::layer::SubscriberExt as _;
use tracing_subscriber::util::SubscriberInitExt as _;

use crate::toolkits::artifact_tests::tools_of;
use crate::toolkits::families::openapi::tools::test_support::{
    RejectedTokenTransport, operation_tool,
};

const SAFETY_CHILD_ENV: &str = "ELITEA_DIAGNOSTICS_RUNTIME_SAFETY_CHILD";

/// Data values the runtime is handed. None may reach a log line.
const SECRET_MARKERS: [&str; 14] = [
    "ROUTER-STATE-SECRET",
    "ROUTER-TEMPLATE-SECRET",
    "TOOL-ARGUMENT-SECRET",
    "TOOL-ERROR-SECRET",
    "TOOL-RESPONSE-SECRET",
    "TOOL-TOKEN-SECRET",
    "private-provider-body",
    "TOOLKIT-NAME-SECRET",
    "TOOLKIT-SETTINGS-SECRET",
    "TOOLKIT-TOKEN-SECRET",
    "URL-PASSWORD-SECRET",
    "URL-QUERY-SECRET",
    "SETTING-KEY-SECRET",
    "CONVERSATION-ID-SECRET",
];

/// Events and spans that must be present, so an empty capture cannot pass.
const EXPECTED_EVENTS: [&str; 5] = [
    "agent.pipeline.router_node",
    "agent.tool.invoke",
    "agent_artifact_tool_failed",
    "agent_toolkit_skipped",
    "context_management_setting_refused",
];

/// A child process owns the global subscriber, so parallel tests cannot change
/// callsite interest and turn a missing line into a silent pass.
#[test]
fn runtime_log_events_never_carry_data_values() {
    let output = std::process::Command::new(std::env::current_exe().expect("test binary"))
        .args([
            "--exact",
            "diagnostics::runtime_log_safety_tests::runtime_log_safety_child",
            "--nocapture",
            "--test-threads=1",
        ])
        .env(SAFETY_CHILD_ENV, "1")
        .output()
        .expect("runtime log safety child process");
    let logged = String::from_utf8_lossy(&output.stdout);
    assert!(
        output.status.success(),
        "{logged}\n{}",
        String::from_utf8_lossy(&output.stderr)
    );
    assert!(logged.contains("CHILD-RAN"), "{logged}");
    for expected in EXPECTED_EVENTS {
        assert!(logged.contains(expected), "{expected} missing: {logged}");
    }
    // Both outcomes of the router and the tool wrapper, not just the happy one.
    for outcome in [
        "outcome=\"completed\"",
        "outcome=\"failed\"",
        "outcome=\"succeeded\"",
    ] {
        assert!(logged.contains(outcome), "{outcome} missing: {logged}");
    }
    for secret in SECRET_MARKERS {
        assert!(!logged.contains(secret), "{secret} leaked: {logged}");
    }
}

#[test]
fn runtime_log_safety_child() {
    if std::env::var(SAFETY_CHILD_ENV).is_err() {
        return;
    }
    tracing_subscriber::registry()
        .with(super::log_layer(Some("trace"), std::io::stdout).expect("production log layer"))
        .init();
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .expect("child runtime");
    runtime.block_on(async {
        drive_router().await;
        drive_openapi_tool().await;
        drive_artifact_tool().await;
        drive_toolkit_skip().await;
    });
    drive_context_management();
    println!("CHILD-RAN");
}

async fn drive_router() {
    let rendered = RouterNodeDefinition::from_yaml(
        "id: choose\ntype: router\ncondition: \"{{ choice }} ROUTER-TEMPLATE-SECRET\"\n\
         routes: [approved]\ndefault_output: fallback\ninput: [choice]\n",
    )
    .expect("router definition");
    let context = NodeContext::new(
        HashMap::from([("choice".to_owned(), json!("ROUTER-STATE-SECRET"))]),
        ExecutionConfig::new("router-thread"),
        0,
    );
    RouterNode::new(rendered)
        .execute(&context)
        .await
        .expect("router execution");

    // A state value the condition cannot parse fails the node.
    let failing = RouterNodeDefinition::from_yaml(
        "id: broken\ntype: router\n\
         condition: \"{{ (payload | json_loads).route }} ROUTER-TEMPLATE-SECRET\"\n\
         routes: [approved]\ndefault_output: fallback\ninput: [payload]\n",
    )
    .expect("failing router definition");
    let context = NodeContext::new(
        HashMap::from([("payload".to_owned(), json!("ROUTER-STATE-SECRET"))]),
        ExecutionConfig::new("router-thread"),
        0,
    );
    RouterNode::new(failing)
        .execute(&context)
        .await
        .err()
        .expect("router must fail on an unparsable payload");
}

/// A native tool call through the policy wrapper, once answered and once failed
/// by the provider. The provider body is the inner error's only content.
async fn drive_openapi_tool() {
    for status in [StatusCode::OK, StatusCode::INTERNAL_SERVER_ERROR] {
        let transport = Arc::new(RejectedTokenTransport {
            responses: std::sync::Mutex::new(VecDeque::from([status])),
            calls: std::sync::atomic::AtomicUsize::new(0),
            requests: std::sync::Mutex::new(Vec::new()),
        });
        let tool = operation_tool(false, "TOOL-TOKEN-SECRET", transport).await;
        // The result is not the subject here: the wrapper's span closes either way.
        let _ignored = tool
            .execute(
                Arc::new(
                    SimpleToolContext::new("runtime-log-safety")
                        .with_session_id("session-1")
                        .with_function_call_id("call-1"),
                ),
                json!({"marker": "TOOL-ARGUMENT-SECRET"}),
            )
            .await;
    }
}

/// The artifact family answers a failed platform call instead of raising it, so
/// its own warning is the only record of the failure.
async fn drive_artifact_tool() {
    let (tools, _rpc) = tools_of(
        &json!({
            "schema_version": "elitea.runtime.artifact-read.v1",
            "project_id": 17,
            "bucket": "agent-artifacts",
            "name": "notes.txt",
            "media_type": "text/plain",
            "byte_length": 20,
            "char_length": 20,
            "total_lines": 1,
            "max_chars": 200_000,
            "over_limit": false,
            "content": "TOOL-RESPONSE-SECRET",
        })
        .to_string(),
        &["read_file"],
    )
    .await;
    tools[0]
        .execute(
            Arc::new(SimpleToolContext::new("runtime-log-safety")),
            json!({"filename": "TOOL-ARGUMENT-SECRET.txt"}),
        )
        .await
        .expect("artifact read");

    let (tools, _rpc) = tools_of("TOOL-ERROR-SECRET", &["create_file"]).await;
    tools[0]
        .execute(
            Arc::new(SimpleToolContext::new("runtime-log-safety")),
            json!({"filename": "TOOL-ARGUMENT-SECRET.txt", "filedata": "TOOL-ARGUMENT-SECRET"}),
        )
        .await
        .expect("a failed write is answered, not raised");
}

/// A toolkit family this runtime does not serve is skipped with a warning.
async fn drive_toolkit_skip() {
    let toolkit: Value = json!({
        "id": 7,
        "type": "unsupported_family",
        "toolkit_name": "TOOLKIT-NAME-SECRET",
        "settings": {
            "api_key": "TOOLKIT-SETTINGS-SECRET",
            "authorization": "Bearer TOOLKIT-TOKEN-SECRET",
            "url": "https://user:URL-PASSWORD-SECRET@example.invalid/?token=URL-QUERY-SECRET",
        },
    });
    let policy =
        Arc::new(ToolAdmissionPolicy::new(&[], &BTreeMap::new()).expect("admission policy"));
    let snapshot = FrozenToolSnapshot::from_toolkit(&toolkit)
        .expect("frozen toolkit")
        .apply_policy(&policy);
    let (toolsets, _catalog) = materialize_configured_toolsets_with_tokens_and_authorization(
        &snapshot,
        &policy,
        &serde_json::Map::new(),
    )
    .await
    .expect("an unsupported family is skipped, not refused");
    assert!(toolsets.is_empty());
}

/// The unrecognized key is profile-authored text and must not be logged.
fn drive_context_management() {
    let settings = json!({"SETTING-KEY-SECRET": true});
    let refused = ContextManagementPlan::admit_current(
        settings.as_object().expect("settings object"),
        Some("CONVERSATION-ID-SECRET"),
    );
    assert!(refused.is_err());
}
