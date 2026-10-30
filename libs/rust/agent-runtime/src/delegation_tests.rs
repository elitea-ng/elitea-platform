use std::collections::BTreeSet;
use std::sync::Arc;

use serde_json::{Value, json};

use super::*;

fn toolkit(id: u64, name: &str, settings: &Value) -> Value {
    json!({"id": id, "type": "jira", "toolkit_name": name, "settings": settings})
}

fn classes(names: &[&str]) -> BTreeSet<String> {
    names.iter().map(|name| (*name).to_owned()).collect()
}

fn scope(caller: &[Value], policy: DelegationPolicy) -> DelegationScope {
    DelegationScope::root(Arc::new(policy), GrantSet::from_tools(caller))
}

fn refusal_of(result: Result<Delegation, Box<RefusedDelegation>>) -> DelegationRefusal {
    result.expect_err("refused").refusal
}

#[test]
fn a_toolkit_the_caller_lacks_is_refused_by_name() {
    let caller = scope(
        &[toolkit(7, "Jira", &json!({}))],
        DelegationPolicy::default(),
    );
    let mut delegated = vec![
        toolkit(7, "Jira", &json!({})),
        toolkit(9, "Payroll", &json!({})),
    ];
    let refused = caller
        .delegate(&mut delegated, &BTreeSet::new(), &PublicPolicy::Open)
        .expect_err("refused");
    assert_eq!(
        refused.refusal,
        DelegationRefusal::GrantMissing {
            kind: GrantKind::Toolkit,
            name: "Payroll".to_owned(),
            toolkit: None,
        }
    );
    assert_eq!(refused.refusal.code(), GRANT_MISSING);
    assert!(refused.refusal.message().contains("'Payroll'"));
    assert_eq!(refused.evidence.depth, 1);
    assert!(refused.evidence.enforced);
    assert_eq!(refused.refusal.to_value()["kind"], "toolkit");
}

#[test]
fn a_tool_the_caller_did_not_select_is_refused_and_a_wider_selection_narrows() {
    let caller = scope(
        &[toolkit(
            7,
            "Jira",
            &json!({"selected_tools": ["search", "read"]}),
        )],
        DelegationPolicy::default(),
    );
    let mut explicit = vec![toolkit(
        7,
        "Jira",
        &json!({"selected_tools": ["search", "create"]}),
    )];
    assert_eq!(
        refusal_of(caller.delegate(&mut explicit, &BTreeSet::new(), &PublicPolicy::Open)),
        DelegationRefusal::GrantMissing {
            kind: GrantKind::Tool,
            name: "create".to_owned(),
            toolkit: Some("Jira".to_owned()),
        }
    );

    // Every tool selected: narrowed to the caller's two, not refused.
    let mut every = vec![toolkit(7, "Jira", &json!({}))];
    let delegated = caller
        .delegate(&mut every, &BTreeSet::new(), &PublicPolicy::Open)
        .expect("narrowed");
    assert_eq!(
        every[0]["settings"]["selected_tools"],
        json!(["read", "search"])
    );
    assert_eq!(delegated.evidence.refusal, None);
    assert_eq!(delegated.scope.depth(), 1);
}

#[test]
fn a_credential_reference_the_caller_lacks_is_refused() {
    let caller = scope(
        &[toolkit(
            7,
            "Jira",
            &json!({"credential_reference_id": "ref-a"}),
        )],
        DelegationPolicy::default(),
    );
    let mut delegated = vec![toolkit(
        7,
        "Jira",
        &json!({"jira_configuration": {"credential_reference_id": "ref-b"}}),
    )];
    assert_eq!(
        refusal_of(caller.delegate(&mut delegated, &BTreeSet::new(), &PublicPolicy::Open)),
        DelegationRefusal::GrantMissing {
            kind: GrantKind::Credential,
            name: "ref-b".to_owned(),
            toolkit: Some("Jira".to_owned()),
        }
    );
    let mut same = vec![toolkit(
        7,
        "Jira",
        &json!({"credential_reference_id": "ref-a"}),
    )];
    caller
        .delegate(&mut same, &BTreeSet::new(), &PublicPolicy::Open)
        .expect("held");
}

#[test]
fn a_data_class_the_caller_lacks_is_refused_and_none_imposes_nothing() {
    let policy =
        DelegationPolicy::new(4, NarrowingMode::Enforce, ["public".to_owned()]).expect("policy");
    let caller = scope(&[], policy);
    assert_eq!(
        refusal_of(caller.delegate(&mut vec![], &classes(&["pii"]), &PublicPolicy::Open)),
        DelegationRefusal::GrantMissing {
            kind: GrantKind::DataClass,
            name: "pii".to_owned(),
            toolkit: None,
        }
    );
    // A callee that declares nothing inherits the caller's classes.
    let delegated = caller
        .delegate(&mut vec![], &BTreeSet::new(), &PublicPolicy::Open)
        .expect("declares nothing");
    assert!(
        delegated
            .scope
            .delegate(&mut vec![], &classes(&["pii"]), &PublicPolicy::Open)
            .is_err(),
        "the inherited class still limits the next callee"
    );
    // A root with no classes imposes none.
    let open = scope(&[], DelegationPolicy::default());
    let narrowed = open
        .delegate(&mut vec![], &classes(&["pii"]), &PublicPolicy::Open)
        .expect("no constraint");
    assert!(
        narrowed
            .scope
            .delegate(&mut vec![], &classes(&["secret"]), &PublicPolicy::Open)
            .is_err(),
        "the callee's own classes limit its callees"
    );
}

#[test]
fn the_combined_depth_is_capped_by_the_policy() {
    let policy = DelegationPolicy::new(2, NarrowingMode::Enforce, []).expect("policy");
    let root = scope(&[], policy);
    let first = root
        .delegate(&mut vec![], &BTreeSet::new(), &PublicPolicy::Open)
        .expect("depth 1");
    let second = first
        .scope
        .delegate(&mut vec![], &BTreeSet::new(), &PublicPolicy::Open)
        .expect("depth 2");
    let refused = second
        .scope
        .delegate(&mut vec![], &BTreeSet::new(), &PublicPolicy::Open)
        .expect_err("depth 3");
    assert_eq!(
        refused.refusal,
        DelegationRefusal::DepthExceeded {
            depth: 3,
            max_depth: 2
        }
    );
    assert_eq!(refused.refusal.code(), DEPTH_EXCEEDED);
    // A sub-agent counts toward the same depth.
    assert!(
        second
            .scope
            .delegate_subagent("read_only", &BTreeSet::new())
            .is_err()
    );
    let evidence = first
        .scope
        .delegate_subagent("read_only", &BTreeSet::new())
        .expect("depth 2");
    assert_eq!(evidence.depth, 2);
    assert_eq!(evidence.effective_grant_digest.len(), 64);
}

#[test]
fn report_mode_records_the_missing_grant_and_runs() {
    let policy = DelegationPolicy::new(4, NarrowingMode::Report, []).expect("policy");
    let caller = scope(&[toolkit(7, "Jira", &json!({}))], policy);
    let mut tools = vec![toolkit(9, "Payroll", &json!({}))];
    let delegated = caller
        .delegate(&mut tools, &BTreeSet::new(), &PublicPolicy::Open)
        .expect("reported");
    assert_eq!(tools.len(), 1, "the callee keeps its toolkit");
    assert!(!delegated.evidence.enforced);
    assert_eq!(
        delegated
            .evidence
            .refusal
            .map(|refusal| refusal.to_value()["name"].clone()),
        Some(json!("Payroll"))
    );
}

#[test]
fn an_ad_hoc_root_imposes_no_toolkit_grant() {
    let root = DelegationScope::default();
    let mut delegated = vec![toolkit(9, "Payroll", &json!({"selected_tools": ["pay"]}))];
    let delegated = root
        .delegate(&mut delegated, &BTreeSet::new(), &PublicPolicy::Open)
        .expect("no constraint");
    // The callee's own grants bind its callees.
    let refused = delegated
        .scope
        .delegate(
            &mut vec![toolkit(10, "Mail", &json!({}))],
            &BTreeSet::new(),
            &PublicPolicy::Open,
        )
        .expect_err("refused");
    assert_eq!(refused.refusal.to_value()["name"], "Mail");
}

#[test]
fn the_digest_is_canonical_and_tracks_the_grant_set() {
    let caller = scope(
        &[
            toolkit(8, "Mail", &json!({})),
            toolkit(7, "Jira", &json!({"selected_tools": ["b", "a"]})),
        ],
        DelegationPolicy::default(),
    );
    let reordered = scope(
        &[
            toolkit(7, "Jira", &json!({"selected_tools": ["a", "b"]})),
            toolkit(8, "Mail", &json!({})),
        ],
        DelegationPolicy::default(),
    );
    assert_eq!(caller.grants().digest(), reordered.grants().digest());
    let narrower = scope(
        &[toolkit(7, "Jira", &json!({"selected_tools": ["a"]}))],
        DelegationPolicy::default(),
    );
    assert_ne!(caller.grants().digest(), narrower.grants().digest());
    let summary = caller
        .delegate(&mut vec![], &BTreeSet::new(), &PublicPolicy::Open)
        .expect("delegated")
        .evidence
        .summary();
    assert_eq!(summary[0], ("delegation_depth", json!(1)));
    assert_eq!(summary[1].0, "effective_grant_digest");
}

#[test]
fn the_policy_bounds_depth_and_classes() {
    assert_eq!(
        DelegationPolicy::new(0, NarrowingMode::Enforce, []),
        Err(DelegationPolicyError::DepthOutOfRange)
    );
    assert_eq!(
        DelegationPolicy::new(17, NarrowingMode::Enforce, []),
        Err(DelegationPolicyError::DepthOutOfRange)
    );
    assert_eq!(
        DelegationPolicy::new(4, NarrowingMode::Enforce, ["Not Valid".to_owned()]),
        Err(DelegationPolicyError::MalformedDataClass)
    );
    assert_eq!(
        DelegationPolicy::new(4, NarrowingMode::Enforce, (0..33).map(|n| format!("c{n}"))),
        Err(DelegationPolicyError::TooManyDataClasses)
    );
    assert_eq!(DelegationPolicy::default().max_depth(), DEFAULT_MAX_DEPTH);
}
