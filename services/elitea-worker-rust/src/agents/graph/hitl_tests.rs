use std::collections::HashMap;
use std::sync::Arc;

use adk_rust::graph::{
    END, ExecutionConfig, GraphAgent, GraphError, MemoryCheckpointer, Node, NodeContext,
    NodeOutput, START, State,
};
use adk_rust::runner::Runner;
use adk_rust::session::{CreateRequest, InMemorySessionService, SessionService};
use adk_rust::{Content, SessionId, UserId};
use serde_json::{Value, json};

use super::agent::EliteaGraphAgent;
use super::hitl::{
    HITL_RESUME_STATE_KEY, HitlAction, HitlMessageKind, HitlNode, HitlNodeDefinition,
    authorized_resume_fixture,
};
use crate::agents::runtime::NativeAgentInvocation;

const FULL_NODE: &str = r#"
id: review
type: hitl
input:
  - summary
  - messages
user_message:
  type: fstring
  value: "Review {summary}."
routes:
  approve: publish
  reject: END
  edit: revise
edit_state_key: summary
"#;

fn definition() -> HitlNodeDefinition {
    HitlNodeDefinition::from_yaml(FULL_NODE).expect("valid HITL definition")
}

#[test]
fn hitl_yaml_is_strict_bounded_and_source_compatible() {
    let definition = definition();
    assert_eq!(definition.id(), "review");
    assert_eq!(definition.input_keys(), ["summary", "messages"]);
    assert_eq!(definition.message_kind(), HitlMessageKind::Fstring);
    assert_eq!(definition.message_template(), "Review {summary}.");
    assert_eq!(definition.edit_state_key(), Some("summary"));
    assert_eq!(
        definition.available_actions(),
        [HitlAction::Approve, HitlAction::Reject, HitlAction::Edit]
    );

    let defaults =
        HitlNodeDefinition::from_yaml("id: review\ntype: hitl\nroutes:\n  approve: publish\n")
            .expect("source defaults");
    assert_eq!(defaults.input_keys(), ["messages"]);
    assert_eq!(defaults.message_kind(), HitlMessageKind::Fixed);
    assert_eq!(
        defaults.message_template(),
        "Please review and approve to continue."
    );

    for invalid in [
        "id: review\ntype: llm\nroutes:\n  approve: publish\n",
        "id: review\ntype: hitl\nroutes: {}\n",
        "id: review\ntype: hitl\nroutes:\n  approve: ../escape\n",
        "id: review\ntype: hitl\nuser_message:\n  type: fstring\n  value: '{x.__class__}'\nroutes:\n  approve: publish\n",
        "id: review\ntype: hitl\nunknown: true\nroutes:\n  approve: publish\n",
    ] {
        let error = HitlNodeDefinition::from_yaml(invalid).expect_err("invalid HITL YAML");
        assert!(
            matches!(
                error.code(),
                "graph.hitl.invalid_configuration" | "graph.hitl.malformed_yaml"
            ),
            "{}",
            error.code()
        );
    }

    let oversized = "x".repeat(64 * 1024 + 1);
    assert_eq!(
        HitlNodeDefinition::from_yaml(&oversized)
            .expect_err("bounded HITL YAML")
            .code(),
        "graph.hitl.configuration_resource_exhausted"
    );
}

#[tokio::test]
async fn node_interrupt_payload_and_message_modes_are_bounded() {
    let node = HitlNode::new(definition());
    let mut state = State::new();
    state.insert("summary".to_owned(), json!("the answer"));
    state.insert(
        "messages".to_owned(),
        json!([{"role": "user", "content": "secret"}]),
    );
    let context = NodeContext::new(state, ExecutionConfig::new("thread-1"), 0);
    let output = node.execute(&context).await.expect("HITL interrupt output");
    let interrupt = output.interrupt.expect("dynamic interrupt");
    let adk_rust::graph::interrupt::Interrupt::Dynamic { message, data } = interrupt else {
        panic!("HITL node must use a dynamic ADK interrupt");
    };
    assert_eq!(message, "Review the answer.");
    let data = data.expect("HITL payload data");
    assert_eq!(data["schema_revision"], "elitea.graph.hitl-interrupt.v1");
    assert_eq!(data["interaction_type"], "pipeline_hitl_node");
    assert_eq!(data["history_contract_version"], 1);
    assert_eq!(data["guardrail_type"], "pipeline_hitl");
    assert_eq!(data["node_name"], "review");
    assert_eq!(
        data["available_actions"],
        json!(["approve", "reject", "edit"])
    );
    assert_eq!(data["routes"]["reject"], "END");
    assert_eq!(data["edit_state_key"], "summary");
    assert!(
        data["definition_digest"]
            .as_str()
            .is_some_and(|value| value.starts_with("sha256:"))
    );
    assert!(!data.to_string().contains("secret"));

    let variable = HitlNodeDefinition::from_yaml(
        "id: review\ntype: hitl\nuser_message:\n  type: variable\n  value: summary\nroutes:\n  reject: END\n",
    )
    .expect("variable definition");
    let variable = HitlNode::new(variable);
    let mut state = State::new();
    state.insert("summary".to_owned(), json!({"status": "ready"}));
    let output = variable
        .execute(&NodeContext::new(
            state,
            ExecutionConfig::new("thread-2"),
            0,
        ))
        .await
        .expect("variable interrupt");
    let Some(adk_rust::graph::interrupt::Interrupt::Dynamic { message, .. }) = output.interrupt
    else {
        panic!("variable node must interrupt");
    };
    assert_eq!(message, r#"{"status":"ready"}"#);
}

#[tokio::test]
async fn approved_resume_is_one_use_and_routes_from_the_checkpoint() {
    let definition = definition();
    let checkpointer = Arc::new(MemoryCheckpointer::new());
    let graph = GraphAgent::builder("pipeline")
        .channels(&[HITL_RESUME_STATE_KEY, "summary", "result"])
        .node(HitlNode::new(definition.clone()))
        .node_fn("publish", |_context| async {
            Ok(NodeOutput::new().with_update("result", json!("published")))
        })
        .node_fn("revise", |context| async move {
            let summary = context.state.get("summary").cloned().unwrap_or(Value::Null);
            Ok(NodeOutput::new().with_update("result", summary))
        })
        .edge(START, "review")
        .edge("publish", END)
        .edge("revise", END)
        .checkpointer_arc(checkpointer)
        .build()
        .expect("compiled graph");

    let mut input = State::new();
    input.insert("summary".to_owned(), json!("draft"));
    let interrupted = graph
        .invoke(input, ExecutionConfig::new("thread-approve"))
        .await
        .expect_err("first run must pause");
    let GraphError::Interrupted(interrupted) = interrupted else {
        panic!("first run must return the ADK interrupted checkpoint");
    };
    assert_eq!(interrupted.thread_id, "thread-approve");
    let resume = authorized_resume_fixture(&definition, "approve", &Value::Null);
    let resume = resume
        .as_object()
        .expect("resume state")
        .clone()
        .into_iter()
        .collect();
    let result = graph
        .invoke(
            resume,
            ExecutionConfig::new("thread-approve").with_resume_from(&interrupted.checkpoint_id),
        )
        .await
        .expect("approved resume");
    assert_eq!(result["result"], "published");
    assert_eq!(result[HITL_RESUME_STATE_KEY], json!({}));
}

#[tokio::test]
async fn elitea_graph_adapter_restores_root_event_identity_before_projection() {
    let graph = GraphAgent::builder("pipeline-root")
        .channels(&[HITL_RESUME_STATE_KEY, "messages"])
        .node(HitlNode::new(definition()))
        .edge(START, "review")
        .checkpointer(MemoryCheckpointer::new())
        .build()
        .expect("compiled GraphAgent");
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    sessions
        .create(CreateRequest {
            app_name: "elitea".to_owned(),
            user_id: "user-1".to_owned(),
            session_id: Some("thread-1".to_owned()),
            state: HashMap::new(),
        })
        .await
        .expect("graph session");
    let runner = Runner::builder()
        .app_name("elitea")
        .agent(Arc::new(EliteaGraphAgent::new(graph)))
        .session_service(sessions)
        .build()
        .expect("graph runner");
    let invocation = NativeAgentInvocation::new(
        runner,
        UserId::new("user-1").expect("fixture user"),
        SessionId::new("thread-1").expect("fixture session"),
        Content::new("user").with_text("draft"),
    );
    let mut running = invocation.start().expect("graph invocation");
    let event = running
        .next_event()
        .await
        .expect("graph event")
        .expect("interrupt event");
    assert_eq!(event.author, "pipeline-root");
    assert_ne!(event.invocation_id, "graph_interrupted");
    assert_ne!(event.invocation_id, "");
    let payload = adk_rust::graph::interrupt::GraphInterruptPayload::from_event(&event)
        .expect("graph interrupt metadata");
    assert_eq!(payload.thread_id, "thread-1");
    assert_ne!(payload.checkpoint_id, "");
    assert!(
        running
            .next_event()
            .await
            .expect("graph stream end")
            .is_none()
    );
}

#[tokio::test]
async fn edit_and_block_are_explicit_while_unknown_or_stale_decisions_fail_closed() {
    let definition = definition();
    for (action, expected_target, expected_value) in [
        ("edit", "revise", json!("corrected")),
        ("block_with_comment", END, json!("draft")),
        ("reject_with_comment", END, json!("draft")),
    ] {
        let node = HitlNode::new(definition.clone());
        let resume = authorized_resume_fixture(&definition, action, &json!("corrected"));
        let mut state: State = resume
            .as_object()
            .expect("resume object")
            .clone()
            .into_iter()
            .collect();
        state.insert("summary".to_owned(), json!("draft"));
        let output = node
            .execute(&NodeContext::new(
                state,
                ExecutionConfig::new("thread-decision"),
                1,
            ))
            .await
            .expect("configured decision");
        assert_eq!(output.goto, Some(vec![expected_target.to_owned()]));
        assert_eq!(
            output
                .updates
                .get("summary")
                .cloned()
                .unwrap_or(json!("draft")),
            expected_value
        );
        assert_eq!(output.updates[HITL_RESUME_STATE_KEY], json!({}));
    }

    for action in ["", "continue", "APPROVE_AND_RUN"] {
        let node = HitlNode::new(definition.clone());
        let resume = authorized_resume_fixture(&definition, action, &Value::Null);
        let state = resume
            .as_object()
            .expect("resume object")
            .clone()
            .into_iter()
            .collect();
        let Err(error) = node
            .execute(&NodeContext::new(
                state,
                ExecutionConfig::new("thread-invalid"),
                1,
            ))
            .await
        else {
            panic!("unknown actions must not become approve");
        };
        assert!(error.to_string().contains("graph.hitl.action_invalid"));
    }

    let mut injected_resume = authorized_resume_fixture(&definition, "approve", &Value::Null);
    injected_resume[HITL_RESUME_STATE_KEY]["review"]["route"] = json!("publish");
    let injected_state = injected_resume
        .as_object()
        .expect("resume object")
        .clone()
        .into_iter()
        .collect();
    let Err(error) = HitlNode::new(definition.clone())
        .execute(&NodeContext::new(
            injected_state,
            ExecutionConfig::new("thread-injected"),
            1,
        ))
        .await
    else {
        panic!("unknown resume fields must fail closed");
    };
    assert!(error.to_string().contains("graph.hitl.resume_invalid"));

    let mut stale_resume = authorized_resume_fixture(&definition, "approve", &Value::Null);
    stale_resume[HITL_RESUME_STATE_KEY]["review"]["definition_digest"] = json!("sha256:stale");
    let stale_state = stale_resume
        .as_object()
        .expect("stale object")
        .clone()
        .into_iter()
        .collect();
    let Err(error) = HitlNode::new(definition)
        .execute(&NodeContext::new(
            stale_state,
            ExecutionConfig::new("thread-stale"),
            1,
        ))
        .await
    else {
        panic!("stale decision must fail closed");
    };
    assert!(
        error
            .to_string()
            .contains("graph.hitl.resume_identity_mismatch")
    );
}
