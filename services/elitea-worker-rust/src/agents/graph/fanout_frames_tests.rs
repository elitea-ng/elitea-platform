use serde_json::Value;

use super::*;

const FIXTURES: &str = concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/../../libs/jsonschema/runtime/v1/fixtures/"
);

fn fixture(name: &str) -> Vec<u8> {
    std::fs::read(format!("{FIXTURES}{name}")).expect("fixture")
}

fn fixture_json(name: &str) -> Value {
    serde_json::from_slice(&fixture(name)).expect("fixture json")
}

fn member() -> FanoutMemberV1 {
    FanoutMemberV1 {
        activation: "9f2c4e1a7b3d5f60".to_owned(),
        kind: "parallel",
        member: "jira_branch".to_owned(),
        node: "research".to_owned(),
        ordinal: 1,
        total: 3,
    }
}

fn hierarchy() -> MemberHierarchy {
    MemberHierarchy {
        path: vec![AgentPathTier {
            call_id: "fo1_fd47d5a520dc10ff429e8c38e13656305daf31781c78c477db942ad604db4031"
                .to_owned(),
            name: "Researcher".to_owned(),
            sibling_ordinal: Some(1),
        }],
    }
}

fn tool_guard_display() -> CardDisplay {
    CardDisplay {
        action_label: Some("Create issue".to_owned()),
        guardrail_type: "sensitive_tool",
        message: "Approve creating a Jira issue?".to_owned(),
        node_name: Some("sensitive_tool_guard".to_owned()),
        tool_args_json: Some(r#"{"project":"OPS","summary":"Disk full"}"#.to_owned()),
        tool_call_id: Some("call_jira_1".to_owned()),
        tool_name: Some("create_issue".to_owned()),
        toolkit_name: Some("jira".to_owned()),
        toolkit_type: Some("jira".to_owned()),
        ..CardDisplay::default()
    }
}

fn card_input<'a>(
    hierarchy: &'a MemberHierarchy,
    member: Option<&'a FanoutMemberV1>,
    actions: &'a [&'static str],
) -> CardInput<'a> {
    CardInput {
        interrupt_key: "93ba421dbbbcc4f07e5b3366f081d1ef3ac983c422a3b9e48ed1fbe05b2d6a87",
        interrupt_id: "hitl:sensitive:0001",
        kind: CardKind::ToolGuard,
        available_actions: actions,
        payload_sha256: "187c94b83c2e41baadf2b343172080a6a152294c3c6eaebb7e639b1b8f5e76c4",
        display: tool_guard_display(),
        hierarchy,
        member,
    }
}

#[test]
fn the_card_serializes_to_the_contract_fixture_bytes() {
    let hierarchy = hierarchy();
    let member = member();
    let card = InterruptCardV1::new(card_input(
        &hierarchy,
        Some(&member),
        &["approve", "reject", "block_with_comment"],
    ))
    .expect("card");
    assert_eq!(
        serde_json::to_vec(&card).expect("json"),
        fixture("fanout-interrupt-card-v1.json")
    );
    let event = interrupt_pending_event(&card).expect("frame");
    assert_eq!(event.r#type, "agent_interrupt_pending");
    let metadata: Value = serde_json::from_slice(&event.response_metadata).expect("metadata");
    assert_eq!(
        metadata["interrupt_card_v1"],
        fixture_json("fanout-interrupt-card-v1.json")
    );
    assert_eq!(metadata["parent_agent_name"], "Researcher");
    assert_eq!(metadata["fanout_v1"]["ordinal"], 1);
}

#[test]
fn incoherent_cards_are_refused() {
    let hierarchy = hierarchy();
    let member = member();
    // An action the kind does not offer.
    assert!(InterruptCardV1::new(card_input(&hierarchy, Some(&member), &["answer"])).is_err());
    // Duplicate actions, no actions, five actions.
    assert!(
        InterruptCardV1::new(card_input(
            &hierarchy,
            Some(&member),
            &["approve", "approve"]
        ))
        .is_err()
    );
    assert!(InterruptCardV1::new(card_input(&hierarchy, Some(&member), &[])).is_err());
    // A fan-out member without a tier carrying its ordinal.
    let flat = MemberHierarchy {
        path: vec![AgentPathTier {
            sibling_ordinal: None,
            ..hierarchy.path[0].clone()
        }],
    };
    assert!(InterruptCardV1::new(card_input(&flat, Some(&member), &["approve"])).is_err());
    // A static pause offers only `continue`.
    let mut input = card_input(&hierarchy, Some(&member), &["approve"]);
    input.kind = CardKind::HitlNode;
    input.display.guardrail_type = "pipeline_static";
    assert!(InterruptCardV1::new(input).is_err());
    let mut input = card_input(&hierarchy, Some(&member), &["continue"]);
    input.kind = CardKind::HitlNode;
    input.display.guardrail_type = "pipeline_static";
    assert!(InterruptCardV1::new(input).is_ok());
    // Guardrail of another kind.
    let mut input = card_input(&hierarchy, Some(&member), &["answer"]);
    input.kind = CardKind::AskUser;
    assert!(
        InterruptCardV1::new(input).is_err(),
        "ask_user with sensitive_tool"
    );
}

#[test]
fn a_coordinator_card_has_an_empty_hierarchy() {
    let empty = MemberHierarchy { path: Vec::new() };
    let card = InterruptCardV1::new(card_input(&empty, None, &["approve"])).expect("card");
    let value = serde_json::to_value(&card).expect("json");
    assert!(value["parent_agent_name"].is_null());
    assert!(value["parent_agent_call_id"].is_null());
    assert_eq!(value["parent_agent_path"], serde_json::json!([]));
    assert!(InterruptCardV1::new(card_input(&hierarchy(), None, &["approve"])).is_err());
}

#[test]
fn member_ordinal_bounds_parallel_16_map_64() {
    let mut parallel = member();
    for (total, ok) in [(16, true), (17, false)] {
        parallel.total = total;
        parallel.ordinal = total;
        let tiers = MemberHierarchy {
            path: vec![AgentPathTier {
                sibling_ordinal: Some(total),
                ..hierarchy().path[0].clone()
            }],
        };
        assert_eq!(
            member_status_event(&parallel, &tiers, MemberLifecycle::End).is_ok(),
            ok,
            "parallel {total}"
        );
    }
    let mut map = member();
    map.kind = "map";
    for (total, ok) in [(64, true), (65, false)] {
        map.total = total;
        map.ordinal = total;
        let tiers = MemberHierarchy {
            path: vec![AgentPathTier {
                sibling_ordinal: Some(total),
                ..hierarchy().path[0].clone()
            }],
        };
        assert_eq!(
            member_status_event(&map, &tiers, MemberLifecycle::End).is_ok(),
            ok,
            "map {total}"
        );
    }
}

#[test]
fn agent_path_tiers_limit_and_limit_plus_one() {
    let tier = |name: &str, ordinal: Option<usize>| AgentPathTier {
        call_id: format!("call_{name}"),
        name: name.to_owned(),
        sibling_ordinal: ordinal,
    };
    let three = MemberHierarchy {
        path: vec![
            tier("Owner", None),
            tier("Sub", None),
            tier("Member", Some(1)),
        ],
    };
    assert!(member_status_event(&member(), &three, MemberLifecycle::End).is_ok());
    let four = MemberHierarchy {
        path: vec![
            tier("Owner", None),
            tier("Sub", None),
            tier("Deeper", None),
            tier("Member", Some(1)),
        ],
    };
    assert!(member_status_event(&member(), &four, MemberLifecycle::End).is_err());
}

#[test]
fn the_member_status_frame_matches_the_contract_fixture() {
    let event = member_status_event(
        &member(),
        &hierarchy(),
        MemberLifecycle::Paused { open_interrupts: 1 },
    )
    .expect("frame");
    assert_eq!(event.r#type, "agent_fanout_member");
    let metadata: Value = serde_json::from_slice(&event.response_metadata).expect("metadata");
    assert_eq!(
        serde_json::to_vec(&metadata["fanout_member_status_v1"]).expect("json"),
        fixture("fanout-member-status-v1.json")
    );
    assert!(
        member_status_event(
            &member(),
            &hierarchy(),
            MemberLifecycle::Paused { open_interrupts: 0 }
        )
        .is_err()
    );
}

#[test]
fn member_call_ids_match_the_contract_vectors() {
    let vectors = fixture_json("fanout-interrupt-key-vectors-v1.json");
    let calls = vectors["member_call_ids"].as_array().expect("vectors");
    assert!(!calls.is_empty());
    for vector in calls {
        assert_eq!(
            member_call_id(vector["child_thread"].as_str().expect("thread")),
            vector["call_id"].as_str().expect("call id")
        );
    }
}

#[test]
fn the_activation_label_is_sixteen_hex_and_stable() {
    let label = activation_label(b"private-activation");
    assert_eq!(label.len(), 16);
    assert!(label.bytes().all(|byte| byte.is_ascii_hexdigit()));
    assert_eq!(label, activation_label(b"private-activation"));
    assert_ne!(label, activation_label(b"another-activation"));
}
