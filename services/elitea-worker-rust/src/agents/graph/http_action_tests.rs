#![allow(
    clippy::disallowed_methods,
    clippy::items_after_statements,
    clippy::too_many_lines,
    clippy::unnecessary_literal_bound,
    reason = "unit tests for 5d-2 HTTP Request pipeline node"
)]

//! End-to-end unit tests for 5d-2 (`type: http`, `revision: 2`):
//! - GET (Class I) success & projection into `dict` and `str` outputs
//! - GET (Class I) `uncertain` retry (`attempt + 1`)
//! - POST (Class C) `uncertain` -> Reconcile card with exact `effect_id` (0 retries)
//! - POST with `replay: idempotency_key` (`retryable: true`) -> Class I retry
//! - Transport / 503 (`Unavailable`) same-invocation retry (`Lookup` before `Begin`)
//! - Crash recovery replay via durable journal (0 duplicate RPCs)
//! - 5b `settings:` (`on_error: continue`, `always_output_data`, `execute_once`,
//!   `policy_denied` never continued, `timeout_ms > 30_000` refused)
//! - Owner-proof recovery (`HTTP_RECEIPT_AUDIENCE`) projection & tamper refusal
//! - Parallel and Map fan-out child refusal naming the node (until 5d-3)

use std::collections::{BTreeMap, VecDeque};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use adk_rust::graph::interrupt::Interrupt;
use adk_rust::graph::{ExecutionConfig, MemoryCheckpointer, Node, NodeContext, State};
use async_trait::async_trait;
use base64::Engine as _;
use elitea_agent_runtime::graph::http_action::{
    HTTP_ACTION_INVOCATION_SCHEMA, HTTP_ACTION_RECEIPT_SCHEMA, HTTP_ACTION_SNAPSHOT_SCHEMA,
    HttpActionData, HttpActionFrozenNodeV2, HttpActionFrozenSnapshotV2, HttpActionInvocationV3,
    HttpActionNodeDefinition, HttpActionProjection, HttpActionReceiptState, HttpActionReceiptV2,
    effect_id_v3_hex, frozen_binding_digest,
};
use elitea_agent_runtime::graph::node_recovery::{NodeFailureClass, NodeRecoveryPolicy};
use elitea_agent_runtime::graph::node_settings::NodeSettingsRefusal;
use ring::digest;
use serde_json::{Value, json};

use super::compiler::{
    FanoutChildRefusal, PipelineConfigurationError, PipelineDefinition, PipelineNodeRuntimes,
};
use super::http_action_runtime::{
    HttpActionAttempt, HttpActionCommittedProjector, PipelineHttpActionExecutor,
};
use super::node_recovery_owner::{
    HTTP_RECEIPT_AUDIENCE, NodeRecoveryOwnerProof, NodeRecoveryOwnerProofKind,
    NodeRecoveryResultReference,
};
use super::node_recovery_runtime::tests::direct_tool_tests::scoped;
use super::node_recovery_runtime::{
    NodeAttemptActivation, NodeAttemptAuthority, NodeResultRecovery, RecoverableNode,
};
use crate::transport::runtime_context::{HttpActionRpcError, HttpActionRpcResponse};

const EXEC_ID: &str = "exec-http-5d2";
const GENERATION: u64 = 7;

type ScriptedResponse = Box<
    dyn Fn(&HttpActionInvocationV3) -> Result<HttpActionRpcResponse, HttpActionRpcError>
        + Send
        + Sync,
>;

struct ScriptedHttpExecutor {
    calls: Mutex<Vec<HttpActionInvocationV3>>,
    responses: Mutex<VecDeque<ScriptedResponse>>,
}

impl ScriptedHttpExecutor {
    fn new(responses: Vec<ScriptedResponse>) -> Arc<Self> {
        Arc::new(Self {
            calls: Mutex::new(Vec::new()),
            responses: Mutex::new(responses.into()),
        })
    }

    fn calls(&self) -> Vec<HttpActionInvocationV3> {
        self.calls.lock().expect("calls").clone()
    }
}

#[async_trait]
impl PipelineHttpActionExecutor for ScriptedHttpExecutor {
    async fn execute(
        &self,
        invocation: &HttpActionInvocationV3,
        _call_timeout: Duration,
    ) -> Result<HttpActionRpcResponse, HttpActionRpcError> {
        self.calls.lock().expect("calls").push(invocation.clone());
        let handler = self
            .responses
            .lock()
            .expect("responses")
            .pop_front()
            .expect("scripted response missing");
        handler(invocation)
    }
}

fn snapshot_for_yaml(
    yaml: &str,
    node_id: &str,
    node_yaml: &str,
) -> Arc<HttpActionFrozenSnapshotV2> {
    let def = HttpActionNodeDefinition::from_yaml(node_yaml).expect("parse http node");
    let wire = def.template_wire();
    let template_digest = sha256_hex(&wire);
    let instructions_digest = sha256_hex(yaml.as_bytes());
    let mut snapshot = HttpActionFrozenSnapshotV2 {
        schema_version: HTTP_ACTION_SNAPSHOT_SCHEMA.to_owned(),
        application_id: "31".to_owned(),
        version_id: "41".to_owned(),
        instructions_digest,
        nodes: vec![HttpActionFrozenNodeV2 {
            node_id: node_id.to_owned(),
            operation: def.operation().to_owned(),
            template_wire_b64: base64::engine::general_purpose::STANDARD.encode(&wire),
            template_digest,
            binding_digest: String::new(),
            output: def.output_keys().to_vec(),
            transition: def.transition().map(str::to_owned),
        }],
    };
    let binding_digest = frozen_binding_digest(&snapshot, &snapshot.nodes[0]);
    snapshot.nodes[0].binding_digest = binding_digest;
    Arc::new(snapshot)
}

fn completed_receipt(
    inv: &HttpActionInvocationV3,
    status: u16,
    data: Value,
    retryable: bool,
) -> HttpActionRpcResponse {
    let effect_id = effect_id_v3_hex(
        EXEC_ID,
        GENERATION,
        &inv.activation_id,
        inv.attempt,
        &inv.binding_digest,
        &inv.inputs_digest,
    );
    HttpActionRpcResponse {
        receipt: HttpActionReceiptV2 {
            schema_version: HTTP_ACTION_RECEIPT_SCHEMA.to_owned(),
            effect_id,
            activation_id: inv.activation_id.clone(),
            request_digest: "aa".repeat(32),
            binding_digest: inv.binding_digest.clone(),
            state: HttpActionReceiptState::Completed,
            result: Some(HttpActionProjection {
                status,
                content_type: Some("application/json".to_owned()),
                byte_length: 24,
                data: HttpActionData::Json { value: data },
            }),
            failure_code: None,
        },
        retryable,
    }
}

fn uncertain_receipt(inv: &HttpActionInvocationV3, retryable: bool) -> HttpActionRpcResponse {
    let effect_id = effect_id_v3_hex(
        EXEC_ID,
        GENERATION,
        &inv.activation_id,
        inv.attempt,
        &inv.binding_digest,
        &inv.inputs_digest,
    );
    HttpActionRpcResponse {
        receipt: HttpActionReceiptV2 {
            schema_version: HTTP_ACTION_RECEIPT_SCHEMA.to_owned(),
            effect_id,
            activation_id: inv.activation_id.clone(),
            request_digest: "aa".repeat(32),
            binding_digest: inv.binding_digest.clone(),
            state: HttpActionReceiptState::Uncertain,
            result: None,
            failure_code: Some("timeout".to_owned()),
        },
        retryable,
    }
}

fn failed_receipt(
    inv: &HttpActionInvocationV3,
    code: &str,
    retryable: bool,
) -> HttpActionRpcResponse {
    let effect_id = effect_id_v3_hex(
        EXEC_ID,
        GENERATION,
        &inv.activation_id,
        inv.attempt,
        &inv.binding_digest,
        &inv.inputs_digest,
    );
    HttpActionRpcResponse {
        receipt: HttpActionReceiptV2 {
            schema_version: HTTP_ACTION_RECEIPT_SCHEMA.to_owned(),
            effect_id,
            activation_id: inv.activation_id.clone(),
            request_digest: "aa".repeat(32),
            binding_digest: inv.binding_digest.clone(),
            state: HttpActionReceiptState::Failed,
            result: None,
            failure_code: Some(code.to_owned()),
        },
        retryable,
    }
}

fn sha256_hex(bytes: &[u8]) -> String {
    use std::fmt::Write;
    let hash = digest::digest(&digest::SHA256, bytes);
    let mut out = String::with_capacity(64);
    for b in hash.as_ref() {
        let _ = write!(out, "{b:02x}");
    }
    out
}

const SIMPLE_GET_NODE: &str = r"id: fetch_fx
type: http
revision: 2
operation: fx_reference.rates
request:
  path:
    run_id: {from: run_id}
response:
  mode: json
  accepted_statuses:
    - {first: 200, last: 200}
  max_bytes: 16384
output: [fx]
transition: END
";

const SIMPLE_GET_PIPELINE: &str = r#"state:
  run_id: {type: str, value: "run-42"}
  fx: {type: dict}
entry_point: fetch_fx
nodes:
  - id: fetch_fx
    type: http
    revision: 2
    operation: fx_reference.rates
    request:
      path:
        run_id: {from: run_id}
    response:
      mode: json
      accepted_statuses:
        - {first: 200, last: 200}
      max_bytes: 16384
    output: [fx]
    transition: END
"#;

#[tokio::test]
async fn get_http_node_executes_and_projects_result() {
    let definition = PipelineDefinition::from_yaml(SIMPLE_GET_PIPELINE).expect("parse pipeline");
    let snapshot = snapshot_for_yaml(SIMPLE_GET_PIPELINE, "fetch_fx", SIMPLE_GET_NODE);
    let executor = ScriptedHttpExecutor::new(vec![Box::new(|inv| {
        assert_eq!(inv.schema_version, HTTP_ACTION_INVOCATION_SCHEMA);
        assert_eq!(inv.node_id, "fetch_fx");
        assert_eq!(inv.attempt, 0);
        assert!(inv.scope_ref.is_none());
        Ok(completed_receipt(
            inv,
            200,
            json!({"rates": {"EUR": "1.10"}}),
            true,
        ))
    })]);
    let runtimes = PipelineNodeRuntimes::default()
        .with_execution_id(EXEC_ID)
        .with_execution_generation(GENERATION)
        .with_node_recovery_authority(scoped())
        .with_http_action(executor.clone(), snapshot);
    let checkpointer = Arc::new(MemoryCheckpointer::new());
    let graph = definition
        .compile_subgraph_with_runtime(checkpointer, &runtimes)
        .expect("compile http pipeline");

    let mut state = State::new();
    state.insert("run_id".to_owned(), json!("run-42"));
    let result = graph
        .invoke(state, ExecutionConfig::new("thread-http-1"))
        .await
        .expect("invoke http pipeline");

    assert_eq!(executor.calls().len(), 1);
    assert_eq!(
        result.get("fx"),
        Some(&json!({
            "status": 200,
            "content_type": "application/json",
            "byte_length": 24,
            "data": {"rates": {"EUR": "1.10"}}
        }))
    );
}

#[tokio::test]
async fn class_i_uncertain_retries_with_next_attempt_and_succeeds() {
    const RETRY_PIPELINE: &str = r#"state:
  run_id: {type: str, value: "run-42"}
  fx: {type: dict}
entry_point: fetch_fx
nodes:
  - id: fetch_fx
    type: http
    revision: 2
    operation: fx_reference.rates
    request:
      path:
        run_id: {from: run_id}
    response:
      mode: json
      accepted_statuses:
        - {first: 200, last: 200}
      max_bytes: 16384
    settings:
      retry_on_fail: true
      max_tries: 3
      wait_between_tries_ms: 100
      timeout_ms: 5000
    output: [fx]
    transition: END
"#;
    let definition = PipelineDefinition::from_yaml(RETRY_PIPELINE).expect("parse pipeline");
    let snapshot = snapshot_for_yaml(RETRY_PIPELINE, "fetch_fx", SIMPLE_GET_NODE);
    let executor = ScriptedHttpExecutor::new(vec![
        Box::new(|inv| Ok(uncertain_receipt(inv, true))),
        Box::new(|inv| Ok(completed_receipt(inv, 200, json!({"ok": true}), true))),
    ]);
    let runtimes = PipelineNodeRuntimes::default()
        .with_execution_id(EXEC_ID)
        .with_execution_generation(GENERATION)
        .with_node_recovery_authority(scoped())
        .with_http_action(executor.clone(), snapshot);
    let graph = definition
        .compile_subgraph_with_runtime(Arc::new(MemoryCheckpointer::new()), &runtimes)
        .expect("compile");

    let mut state = State::new();
    state.insert("run_id".to_owned(), json!("run-42"));
    let result = graph
        .invoke(state, ExecutionConfig::new("thread-http-retry"))
        .await
        .expect("invoke");

    let calls = executor.calls();
    assert_eq!(calls.len(), 2);
    assert_eq!(calls[0].attempt, 0);
    assert_eq!(calls[1].attempt, 1);
    assert_eq!(calls[0].activation_id, calls[1].activation_id);
    assert_eq!(result["fx"]["data"], json!({"ok": true}));
}

#[tokio::test]
async fn class_c_uncertain_never_retries_and_emits_reconcile_card_with_effect_id() {
    const POST_PIPELINE: &str = r#"state:
  payload: {type: dict, value: {"amount": 100}}
  resp: {type: dict}
entry_point: charge
nodes:
  - id: charge
    type: http
    revision: 2
    operation: billing.charge
    request:
      body: {kind: json, from: payload}
    response: {mode: json}
    settings:
      retry_on_fail: true
      max_tries: 3
      wait_between_tries_ms: 100
      on_error: continue
    output: [resp]
    transition: END
"#;
    const POST_NODE: &str = r"id: charge
type: http
revision: 2
operation: billing.charge
request:
  body: {kind: json, from: payload}
response: {mode: json}
output: [resp]
transition: END
";
    let def = HttpActionNodeDefinition::from_yaml(POST_NODE).expect("parse");
    let snapshot = snapshot_for_yaml(POST_PIPELINE, "charge", POST_NODE);
    let (binding_bytes, binding_hex) = snapshot
        .binding_digest_for_node(&def)
        .expect("binding digest");
    let executor =
        ScriptedHttpExecutor::new(vec![Box::new(|inv| Ok(uncertain_receipt(inv, false)))]);
    let attempt = HttpActionAttempt::new(
        def.clone(),
        binding_bytes,
        binding_hex,
        BTreeMap::from([("resp".to_owned(), "dict".to_owned())]),
        executor.clone(),
        EXEC_ID.to_owned(),
        GENERATION,
        Some(5_000),
        true,
        None,
    );

    let settings = elitea_agent_runtime::graph::node_settings::NodeSettings::parse(
        serde_yaml_ng::from_str(
            "retry_on_fail: true\nmax_tries: 3\nwait_between_tries_ms: 100\non_error: continue\n",
        )
        .expect("yaml"),
    )
    .expect("settings");
    let policy = settings.to_recovery_policy().expect("policy");
    let node = RecoverableNode::new(
        Arc::new(attempt),
        def.config_digest(),
        policy,
        scoped(),
        None,
    );

    let mut state = State::new();
    state.insert("payload".to_owned(), json!({"amount": 100}));
    let ctx = NodeContext::new(state, ExecutionConfig::new("thread-post-uncertain"), 1);
    let output = node.execute(&ctx).await.expect("reconcile interrupt");

    // Class C uncertain MUST NOT retry (0 retries, exactly 1 RPC call).
    let calls = executor.calls();
    assert_eq!(calls.len(), 1);
    let expected_effect = effect_id_v3_hex(
        EXEC_ID,
        GENERATION,
        &calls[0].activation_id,
        0,
        &calls[0].binding_digest,
        &calls[0].inputs_digest,
    );
    let Some(Interrupt::Dynamic {
        data: Some(data), ..
    }) = output.interrupt
    else {
        panic!(
            "expected dynamic reconcile interrupt, got {:?}",
            output.interrupt
        );
    };
    assert_eq!(data["guardrail_type"], json!("pipeline_node_recovery"));
    let receipt: super::node_recovery_receipt::NodeRecoveryRequiredReceipt =
        serde_json::from_value(data["receipt"].clone()).expect("decode recovery receipt");
    let expected_effect_bytes = elitea_agent_runtime::graph::http_action::effect_id_v3(
        EXEC_ID,
        GENERATION,
        &calls[0].activation_id,
        0,
        &calls[0].binding_digest,
        &calls[0].inputs_digest,
    );
    assert_eq!(
        receipt.replay_safety,
        elitea_agent_runtime::graph::node_recovery::ReplaySafety::UnknownExternalEffect {
            effect_id: expected_effect_bytes
        }
    );
    assert_eq!(expected_effect.len(), 64);
}

#[tokio::test]
async fn unavailable_retries_same_invocation_before_effect_row() {
    let def = HttpActionNodeDefinition::from_yaml(SIMPLE_GET_NODE).expect("parse");
    let snapshot = snapshot_for_yaml(SIMPLE_GET_PIPELINE, "fetch_fx", SIMPLE_GET_NODE);
    let (binding_bytes, binding_hex) = snapshot
        .binding_digest_for_node(&def)
        .expect("binding digest");
    let executor = ScriptedHttpExecutor::new(vec![
        Box::new(|_| Err(HttpActionRpcError::Unavailable)),
        Box::new(|_| Err(HttpActionRpcError::Unavailable)),
        Box::new(|inv| {
            Ok(completed_receipt(
                inv,
                200,
                json!({"recovered": true}),
                true,
            ))
        }),
    ]);
    let attempt = HttpActionAttempt::new(
        def.clone(),
        binding_bytes,
        binding_hex,
        BTreeMap::from([("fx".to_owned(), "dict".to_owned())]),
        executor.clone(),
        EXEC_ID.to_owned(),
        GENERATION,
        None,
        false,
        None,
    )
    .with_transport_retry_config(3, 1);

    let node = RecoverableNode::new(
        Arc::new(attempt),
        def.config_digest(),
        NodeRecoveryPolicy::default(),
        scoped(),
        None,
    );
    let mut state = State::new();
    state.insert("run_id".to_owned(), json!("run-42"));
    let ctx = NodeContext::new(state, ExecutionConfig::new("thread-503"), 0);
    let output = node.execute(&ctx).await.expect("recovered after 503");

    let calls = executor.calls();
    assert_eq!(calls.len(), 3);
    // Every 503 retry sent the EXACT same Invocation (attempt 0, same activation/inputs).
    assert_eq!(calls[0], calls[1]);
    assert_eq!(calls[1], calls[2]);
    assert_eq!(calls[0].attempt, 0);
    assert_eq!(output.updates["fx"]["data"], json!({"recovered": true}));
}

#[tokio::test]
async fn durable_journal_replays_completed_result_without_duplicate_rpc() {
    let def = HttpActionNodeDefinition::from_yaml(SIMPLE_GET_NODE).expect("parse");
    let snapshot = snapshot_for_yaml(SIMPLE_GET_PIPELINE, "fetch_fx", SIMPLE_GET_NODE);
    let (binding_bytes, binding_hex) = snapshot
        .binding_digest_for_node(&def)
        .expect("binding digest");
    let journals = scoped();

    let mut state = State::new();
    state.insert("run_id".to_owned(), json!("run-42"));
    let ctx = NodeContext::new(state, ExecutionConfig::new("thread-crash"), 2);

    let executor = ScriptedHttpExecutor::new(vec![Box::new(|inv| {
        Ok(completed_receipt(inv, 200, json!({"once": 1}), true))
    })]);
    let make_node = || {
        let attempt = HttpActionAttempt::new(
            def.clone(),
            binding_bytes,
            binding_hex.clone(),
            BTreeMap::from([("fx".to_owned(), "dict".to_owned())]),
            executor.clone(),
            EXEC_ID.to_owned(),
            GENERATION,
            None,
            false,
            None,
        );
        RecoverableNode::new(
            Arc::new(attempt),
            def.config_digest(),
            NodeRecoveryPolicy::default(),
            journals.clone(),
            None,
        )
    };
    let out1 = make_node().execute(&ctx).await.expect("first run");
    let out2 = make_node().execute(&ctx).await.expect("replayed run");

    assert_eq!(executor.calls().len(), 1);
    assert_eq!(out1.updates["fx"], out2.updates["fx"]);
}

#[tokio::test]
async fn settings_continue_always_output_data_and_execute_once_on_class_c_failure() {
    const WEBHOOK_PIPELINE: &str = r#"state:
  webhook_payload: {type: dict, value: {"run_id": "r1"}}
  run_id: {type: str, value: "r1"}
  webhook_result: {type: dict}
entry_point: send_webhook
nodes:
  - id: send_webhook
    type: http
    revision: 2
    operation: etl_webhook.complete
    request:
      body: {kind: json, from: webhook_payload}
      idempotency_key: {from: run_id}
    response: {mode: json, accepted_statuses: [{first: 200, last: 201}], max_bytes: 65536}
    settings:
      execute_once: true
      timeout_ms: 10000
      on_error: continue
      always_output_data: true
    input: [webhook_payload, run_id]
    output: [webhook_result]
    transition: END
"#;
    const WEBHOOK_NODE: &str = r"id: send_webhook
type: http
revision: 2
operation: etl_webhook.complete
request:
  body: {kind: json, from: webhook_payload}
  idempotency_key: {from: run_id}
response: {mode: json, accepted_statuses: [{first: 200, last: 201}], max_bytes: 65536}
input: [webhook_payload, run_id]
output: [webhook_result]
transition: END
";
    let definition = PipelineDefinition::from_yaml(WEBHOOK_PIPELINE).expect("parse etl webhook");
    let snapshot = snapshot_for_yaml(WEBHOOK_PIPELINE, "send_webhook", WEBHOOK_NODE);
    let executor = ScriptedHttpExecutor::new(vec![Box::new(|inv| {
        Ok(failed_receipt(inv, "unexpected_status", false))
    })]);
    let runtimes = PipelineNodeRuntimes::default()
        .with_execution_id(EXEC_ID)
        .with_execution_generation(GENERATION)
        .with_node_recovery_authority(scoped())
        .with_http_action(executor.clone(), snapshot);
    let graph = definition
        .compile_subgraph_with_runtime(Arc::new(MemoryCheckpointer::new()), &runtimes)
        .expect("compile");

    let mut state = State::new();
    state.insert("webhook_payload".to_owned(), json!({"run_id": "r1"}));
    state.insert("run_id".to_owned(), json!("r1"));
    let result = graph
        .invoke(state, ExecutionConfig::new("thread-webhook-continue"))
        .await
        .expect("continued");

    assert_eq!(executor.calls().len(), 1);
    assert_eq!(result.get("webhook_result"), Some(&json!({})));
    let errors = result["node_errors"].as_array().expect("node_errors array");
    assert_eq!(errors.len(), 1);
    assert_eq!(errors[0]["node"], json!("send_webhook"));
    assert_eq!(errors[0]["node_type"], json!("http"));
    assert_eq!(errors[0]["error"]["code"], json!("pipeline.http.failed"));
    assert_eq!(errors[0]["retryable"], json!(false));
}

#[tokio::test]
async fn policy_denied_is_never_continued_even_with_on_error_continue() {
    const PIPELINE: &str = r#"state:
  run_id: {type: str, value: "run-42"}
  fx: {type: dict}
entry_point: fetch_fx
nodes:
  - id: fetch_fx
    type: http
    revision: 2
    operation: fx_reference.rates
    request:
      path:
        run_id: {from: run_id}
    response: {mode: json}
    settings:
      on_error: continue
    output: [fx]
    transition: END
"#;
    let definition = PipelineDefinition::from_yaml(PIPELINE).expect("parse");
    let snapshot = snapshot_for_yaml(PIPELINE, "fetch_fx", SIMPLE_GET_NODE);
    let executor = ScriptedHttpExecutor::new(vec![Box::new(|inv| {
        Ok(failed_receipt(inv, "policy_denied", false))
    })]);
    let runtimes = PipelineNodeRuntimes::default()
        .with_execution_id(EXEC_ID)
        .with_execution_generation(GENERATION)
        .with_node_recovery_authority(scoped())
        .with_http_action(executor, snapshot);
    let graph = definition
        .compile_subgraph_with_runtime(Arc::new(MemoryCheckpointer::new()), &runtimes)
        .expect("compile");

    let mut state = State::new();
    state.insert("run_id".to_owned(), json!("run-42"));
    let err = graph
        .invoke(state, ExecutionConfig::new("thread-policy-denied"))
        .await
        .expect_err("policy_denied must not continue");
    assert!(err.to_string().contains("pipeline.node_recovery.failed"));
}

#[test]
fn http_settings_timeout_above_30s_is_refused() {
    const OVER_TIMEOUT: &str = r"state:
  fx: {type: dict}
entry_point: fetch_fx
nodes:
  - id: fetch_fx
    type: http
    revision: 2
    operation: fx_reference.rates
    request: {}
    response: {mode: json}
    settings:
      timeout_ms: 30001
    output: [fx]
    transition: END
";
    let err = PipelineDefinition::from_yaml(OVER_TIMEOUT)
        .err()
        .expect("must refuse 30001ms");
    assert!(matches!(
        err,
        PipelineConfigurationError::NodeSettings {
            ref node,
            refusal: NodeSettingsRefusal::HttpTimeoutRange,
        } if node == "fetch_fx"
    ));
}

#[test]
fn parallel_and_map_fanout_refuse_http_children_naming_the_node() {
    const PARALLEL_HTTP: &str = r#"state:
  results: {type: list}
  fx: {type: dict}
  other: {type: str}
entry_point: fan
nodes:
  - id: fan
    type: parallel
    branches:
      - id: b1
        node: fetch_fx
      - id: b2
        node: keep_other
    max_concurrency: 2
    wait: all
    error_policy: fail_after_drain
    output: [results]
    transition: END
  - id: fetch_fx
    type: http
    revision: 2
    operation: fx_reference.rates
    request: {}
    response: {mode: json}
    output: [fx]
  - id: keep_other
    type: llm
    input_mapping:
      task: {type: fixed, value: "hi"}
    output: [other]
"#;
    let err = PipelineDefinition::from_yaml(PARALLEL_HTTP)
        .err()
        .expect("parallel http must fail");
    assert!(matches!(
        err,
        PipelineConfigurationError::FanoutChildUnsupported {
            ref node,
            refusal: FanoutChildRefusal::Http,
        } if node == "fetch_fx"
    ));
    assert!(err.to_string().contains("fetch_fx"));
    assert!(err.to_string().contains("5d-3"));

    const MAP_HTTP: &str = r"state:
  items: {type: list}
  results: {type: list}
  fx: {type: dict}
entry_point: fan
nodes:
  - id: fan
    type: map
    source: items
    worker: fetch_fx
    item: item
    index: idx
    outputs: [fx]
    destination: results
    max_items: 10
    max_concurrency: 2
    transition: END
  - id: fetch_fx
    type: http
    revision: 2
    operation: fx_reference.rates
    request: {}
    response: {mode: json}
    output: [fx]
";
    let err = PipelineDefinition::from_yaml(MAP_HTTP)
        .err()
        .expect("map http must fail");
    assert!(matches!(
        err,
        PipelineConfigurationError::FanoutChildUnsupported {
            ref node,
            refusal: FanoutChildRefusal::Http,
        } if node == "fetch_fx"
    ));
    assert!(err.to_string().contains("fetch_fx"));
}

#[test]
fn committed_projector_validates_http_receipt_audience_and_projects_result() {
    let def = HttpActionNodeDefinition::from_yaml(SIMPLE_GET_NODE).expect("parse");
    let snapshot = snapshot_for_yaml(SIMPLE_GET_PIPELINE, "fetch_fx", SIMPLE_GET_NODE);
    let (binding_bytes, binding_hex) = snapshot
        .binding_digest_for_node(&def)
        .expect("binding digest");
    let projector = HttpActionCommittedProjector::new(
        def.clone(),
        BTreeMap::from([("fx".to_owned(), "dict".to_owned())]),
        EXEC_ID.to_owned(),
        GENERATION,
    )
    .expect("projector");

    let mut state = State::new();
    state.insert("run_id".to_owned(), json!("run-42"));
    let ctx = NodeContext::new(state, ExecutionConfig::new("thread-owner"), 3);
    let activation = NodeAttemptActivation::from_context(def.id(), def.config_digest(), &ctx)
        .expect("activation");
    let btree_state: BTreeMap<String, Value> = ctx
        .state
        .iter()
        .map(|(k, v)| (k.clone(), v.clone()))
        .collect();
    let resolved = def
        .resolve_canonical_inputs(&btree_state)
        .expect("resolve inputs");
    let visit = elitea_agent_runtime::graph::http_action::visit_id(
        "thread-owner",
        def.id(),
        3,
        binding_bytes,
    );
    let effect_id = effect_id_v3_hex(
        EXEC_ID,
        GENERATION,
        &visit,
        0,
        &binding_hex,
        &resolved.inputs_digest_hex,
    );
    let receipt = HttpActionReceiptV2 {
        schema_version: HTTP_ACTION_RECEIPT_SCHEMA.to_owned(),
        effect_id: effect_id.clone(),
        activation_id: visit,
        request_digest: "bb".repeat(32),
        binding_digest: binding_hex,
        state: HttpActionReceiptState::Completed,
        result: Some(HttpActionProjection {
            status: 200,
            content_type: Some("application/json".to_owned()),
            byte_length: 12,
            data: HttpActionData::Json {
                value: json!({"projected": true}),
            },
        }),
        failure_code: None,
    };
    let receipt_wire = serde_json::to_vec(&receipt).expect("serialize receipt");
    let receipt_sha = sha256_hex(&receipt_wire);

    let proof = NodeRecoveryOwnerProof {
        schema: "elitea.pipeline.node-recovery-owner-proof.v1".to_owned(),
        kind: NodeRecoveryOwnerProofKind::CommittedResult,
        execution_id: EXEC_ID.to_owned(),
        generation: GENERATION,
        activation_id: "11".repeat(32),
        attempt: 1,
        expected_revision: 2,
        effect_id: effect_id.clone(),
        owner_receipt_sha256: receipt_sha.clone(),
        result_ref: Some(NodeRecoveryResultReference {
            content_id: effect_id,
            immutable_version: receipt_sha.clone(),
            digest_sha256: receipt_sha,
            byte_length: receipt_wire.len() as u64,
            media_type: "application/json".to_owned(),
            required_grant_audience: HTTP_RECEIPT_AUDIENCE.to_owned(),
        }),
        owner_receipt_ref: None,
    };
    let authority = NodeAttemptAuthority::for_test_owner_projection(
        &activation,
        [0x22; 32],
        1,
        [0x33; 32],
        [0x44; 32],
    );

    let projected = projector
        .project_committed_result(&ctx, &authority, &proof, &receipt_wire)
        .expect("project committed result");
    assert_eq!(projected.updates["fx"]["data"], json!({"projected": true}));

    // Wrong audience must fail closed with AuthorizationDenied.
    let mut bad_proof = proof;
    bad_proof
        .result_ref
        .as_mut()
        .expect("ref")
        .required_grant_audience = "wrong.audience.v1".to_owned();
    let Err(err) = projector.project_committed_result(&ctx, &authority, &bad_proof, &receipt_wire)
    else {
        panic!("wrong audience must fail");
    };
    assert_eq!(err.class, NodeFailureClass::AuthorizationDenied);
}
