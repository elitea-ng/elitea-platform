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

// The run principal's bound (ADR-0044 Amendment 1, #2157).

fn principal(
    toolkits: &[(u64, Option<&[&str]>)],
    credentials: Option<&[&str]>,
    data_classes: &[&str],
) -> PrincipalBound {
    PrincipalBound::new(
        toolkits.iter().map(|(id, tools)| PrincipalToolkit {
            id: *id,
            tools: tools.map(classes),
        }),
        credentials.map(|references| references.iter().map(|r| (*r).to_owned()).collect()),
        data_classes.iter().map(|class| (*class).to_owned()),
    )
    .expect("a valid bound")
}

fn bounded_policy(mode: NarrowingMode, bound: PrincipalBound) -> DelegationPolicy {
    DelegationPolicy::new(4, mode, [])
        .expect("policy")
        .with_principal(bound)
}

fn principal_refusal(kind: GrantKind, name: &str, toolkit: Option<&str>) -> DelegationRefusal {
    DelegationRefusal::PrincipalBound {
        kind,
        name: name.to_owned(),
        toolkit: toolkit.map(str::to_owned),
    }
}

#[test]
fn a_toolkit_the_principal_may_not_use_is_refused_in_both_modes() {
    // The caller holds T (id 9) and so does the callee: the caller check
    // would let it through. The principal may not use T.
    for mode in [NarrowingMode::Enforce, NarrowingMode::Report] {
        let caller = scope(
            &[toolkit(9, "Repo", &json!({}))],
            bounded_policy(mode, principal(&[(7, None)], None, &[])),
        );
        let mut delegated = vec![toolkit(9, "Repo", &json!({}))];
        let refused = caller
            .delegate(&mut delegated, &BTreeSet::new(), &PublicPolicy::Open)
            .expect_err("refused by the principal");
        assert_eq!(
            refused.refusal,
            principal_refusal(GrantKind::Toolkit, "Repo", None)
        );
        assert!(
            refused.evidence.enforced,
            "report never relaxes P ({mode:?})"
        );
        assert_eq!(refused.refusal.code(), GRANT_MISSING);
        assert_eq!(refused.refusal.to_value()["reason"], "principal");
        assert!(refused.refusal.message().contains("(principal)"));
    }
}

#[test]
fn a_specialist_the_parent_does_not_hold_is_refused_by_the_principal_not_the_caller() {
    // Agent2 holds no toolkit; Agent1 (the callee) holds the repo toolkit.
    // When the principal may use it the refusal is the caller's (until the
    // grant of story #2158 lands), and when it may not, the principal's.
    let allowed = scope(
        &[],
        bounded_policy(NarrowingMode::Enforce, principal(&[(9, None)], None, &[])),
    );
    let mut repo = vec![toolkit(9, "Repo", &json!({}))];
    assert_eq!(
        refusal_of(allowed.delegate(&mut repo, &BTreeSet::new(), &PublicPolicy::Open)),
        DelegationRefusal::GrantMissing {
            kind: GrantKind::Toolkit,
            name: "Repo".to_owned(),
            toolkit: None,
        }
    );
    let denied = scope(
        &[],
        bounded_policy(NarrowingMode::Enforce, principal(&[], None, &[])),
    );
    let mut repo = vec![toolkit(9, "Repo", &json!({}))];
    assert_eq!(
        refusal_of(denied.delegate(&mut repo, &BTreeSet::new(), &PublicPolicy::Open)),
        principal_refusal(GrantKind::Toolkit, "Repo", None)
    );
}

#[test]
fn an_ad_hoc_root_is_bounded_by_the_principal() {
    let root = DelegationScope::root(
        Arc::new(bounded_policy(
            NarrowingMode::Enforce,
            principal(&[(7, Some(&["search"][..]))], None, &[]),
        )),
        GrantSet::unconstrained(),
    );
    assert!(!root.grants().constrains_toolkits());
    // A toolkit outside P: refused although the ad hoc root holds no grant.
    let mut outside = vec![toolkit(9, "Payroll", &json!({}))];
    assert_eq!(
        refusal_of(root.delegate(&mut outside, &BTreeSet::new(), &PublicPolicy::Open)),
        principal_refusal(GrantKind::Toolkit, "Payroll", None)
    );
    // A toolkit inside P with every tool: narrowed to the principal's tools.
    let mut inside = vec![toolkit(7, "Jira", &json!({}))];
    let delegated = root
        .delegate(&mut inside, &BTreeSet::new(), &PublicPolicy::Open)
        .expect("within the principal");
    assert_eq!(inside[0]["settings"]["selected_tools"], json!(["search"]));
    assert_eq!(delegated.evidence.refusal, None);
    // A tool outside P is refused by name.
    let mut tool = vec![toolkit(7, "Jira", &json!({"selected_tools": ["delete"]}))];
    assert_eq!(
        refusal_of(root.delegate(&mut tool, &BTreeSet::new(), &PublicPolicy::Open)),
        principal_refusal(GrantKind::Tool, "delete", Some("Jira"))
    );
}

#[test]
fn the_root_is_its_own_grants_intersected_with_the_principal() {
    let policy = Arc::new(bounded_policy(
        NarrowingMode::Enforce,
        principal(&[(7, Some(&["search"][..])), (8, None)], None, &["public"]),
    ));
    let root = DelegationScope::root(
        Arc::clone(&policy),
        GrantSet::from_tools(&[
            toolkit(7, "Jira", &json!({"selected_tools": ["search", "create"]})),
            toolkit(9, "Payroll", &json!({})),
        ]),
    );
    let canonical = root.grants().canonical();
    let keys: Vec<&str> = canonical["toolkits"]
        .as_array()
        .expect("toolkits")
        .iter()
        .filter_map(|toolkit| toolkit["key"].as_str())
        .collect();
    assert_eq!(
        keys,
        ["toolkit:7"],
        "the toolkit the principal lacks is gone"
    );
    assert_eq!(
        canonical["toolkits"][0]["tools"],
        json!({"only": ["search"]})
    );
    assert_eq!(canonical["data_classes"], json!(["public"]));
}

#[test]
fn the_principal_is_carried_to_every_hop() {
    let policy = Arc::new(bounded_policy(
        NarrowingMode::Enforce,
        principal(&[(7, None), (8, None)], None, &[]),
    ));
    let root = DelegationScope::root(
        Arc::clone(&policy),
        GrantSet::from_tools(&[
            toolkit(7, "Jira", &json!({})),
            toolkit(8, "Wiki", &json!({})),
        ]),
    );
    let mut child = vec![toolkit(7, "Jira", &json!({}))];
    let first = root
        .delegate(&mut child, &BTreeSet::new(), &PublicPolicy::Open)
        .expect("within P");
    // The grandchild reaches for a toolkit outside P that the root never held.
    let mut grandchild = vec![toolkit(9, "Payroll", &json!({}))];
    assert_eq!(
        refusal_of(
            first
                .scope
                .delegate(&mut grandchild, &BTreeSet::new(), &PublicPolicy::Open)
        ),
        principal_refusal(GrantKind::Toolkit, "Payroll", None)
    );
    // A sub-agent holds a subset of its caller's grants, so nothing outside P.
    let sub = first
        .scope
        .delegate_subagent_scope("read_only", &BTreeSet::new())
        .expect("sub-agent");
    assert_eq!(sub.scope.depth(), 2);
    let mut outside = vec![toolkit(9, "Payroll", &json!({}))];
    assert!(matches!(
        refusal_of(
            sub.scope
                .delegate(&mut outside, &BTreeSet::new(), &PublicPolicy::Open)
        ),
        DelegationRefusal::PrincipalBound { .. }
    ));
}

#[test]
fn a_credential_reference_outside_the_principal_is_refused() {
    let held = toolkit(7, "Jira", &json!({"credential_reference_id": "ref-a"}));
    let caller = scope(
        std::slice::from_ref(&held),
        bounded_policy(
            NarrowingMode::Report,
            principal(&[(7, None)], Some(&["ref-z"][..]), &[]),
        ),
    );
    let mut same = vec![held.clone()];
    assert_eq!(
        refusal_of(caller.delegate(&mut same, &BTreeSet::new(), &PublicPolicy::Open)),
        principal_refusal(GrantKind::Credential, "ref-a", Some("Jira"))
    );
    // No credential bound (an ungoverned run): the reference is not judged.
    let open = scope(
        std::slice::from_ref(&held),
        bounded_policy(NarrowingMode::Enforce, principal(&[(7, None)], None, &[])),
    );
    let mut again = vec![held];
    open.delegate(&mut again, &BTreeSet::new(), &PublicPolicy::Open)
        .expect("no credential bound");
}

#[test]
fn a_data_class_the_project_does_not_allow_is_refused() {
    let caller = scope(
        &[],
        bounded_policy(
            NarrowingMode::Enforce,
            principal(&[], None, &["public", "internal"]),
        ),
    );
    assert_eq!(
        refusal_of(caller.delegate(&mut vec![], &classes(&["pii"]), &PublicPolicy::Open)),
        principal_refusal(GrantKind::DataClass, "pii", None)
    );
    // A project that declares none imposes no bound.
    let none = scope(
        &[],
        bounded_policy(NarrowingMode::Enforce, principal(&[], None, &[])),
    );
    none.delegate(&mut vec![], &classes(&["pii"]), &PublicPolicy::Open)
        .expect("no data class bound");
}

#[test]
fn the_principal_narrowing_shows_in_the_effective_digest() {
    let tools = [toolkit(7, "Jira", &json!({}))];
    let unbounded = scope(&tools, DelegationPolicy::default());
    let bounded = scope(
        &tools,
        bounded_policy(
            NarrowingMode::Enforce,
            principal(&[(7, Some(&["search"][..]))], None, &[]),
        ),
    );
    let mut a = vec![toolkit(7, "Jira", &json!({}))];
    let mut b = vec![toolkit(7, "Jira", &json!({}))];
    let wide = unbounded
        .delegate(&mut a, &BTreeSet::new(), &PublicPolicy::Open)
        .expect("wide");
    let narrow = bounded
        .delegate(&mut b, &BTreeSet::new(), &PublicPolicy::Open)
        .expect("narrow");
    assert_ne!(
        wide.evidence.effective_grant_digest,
        narrow.evidence.effective_grant_digest
    );
}

#[test]
fn a_callee_within_the_principal_is_unchanged() {
    // The same composition with and without a bound that covers it.
    let tools = [toolkit(7, "Jira", &json!({"selected_tools": ["search"]}))];
    let plain = scope(&tools, DelegationPolicy::default());
    let covered = scope(
        &tools,
        bounded_policy(NarrowingMode::Enforce, principal(&[(7, None)], None, &[])),
    );
    let mut a = vec![toolkit(7, "Jira", &json!({"selected_tools": ["search"]}))];
    let mut b = a.clone();
    let left = plain
        .delegate(&mut a, &BTreeSet::new(), &PublicPolicy::Open)
        .expect("plain");
    let right = covered
        .delegate(&mut b, &BTreeSet::new(), &PublicPolicy::Open)
        .expect("covered");
    assert_eq!(a, b);
    assert_eq!(
        left.evidence.effective_grant_digest,
        right.evidence.effective_grant_digest
    );
}

#[test]
fn a_built_in_toolkit_with_no_row_is_not_bounded_and_the_limits_hold() {
    let bound = principal(&[], None, &[]);
    assert!(bound.admits_toolkit(None));
    assert!(!bound.admits_toolkit(Some(3)));
    let too_many = PrincipalBound::new(
        (0..=MAX_PRINCIPAL_TOOLKITS as u64).map(|id| PrincipalToolkit { id, tools: None }),
        None,
        [],
    );
    assert_eq!(too_many, Err(DelegationPolicyError::PrincipalBoundTooLarge));
    // The limit is on distinct entries: a repeated id at the limit is fine.
    let at_limit = (0..MAX_PRINCIPAL_TOOLKITS as u64)
        .chain([0])
        .map(|id| PrincipalToolkit { id, tools: None });
    assert!(PrincipalBound::new(at_limit, None, []).is_ok());
    // A reference is compared without case.
    let cased = principal(&[], Some(&["0192F5E2-AAAA"][..]), &[]);
    assert!(cased.admits_credential("0192f5e2-aaaa"));
    assert!(cased.admits_credential("0192F5E2-AAAA"));
    assert!(!cased.admits_credential("0192f5e2-bbbb"));
    let long_reference = PrincipalBound::new(
        [],
        Some(vec!["r".repeat(MAX_PRINCIPAL_REFERENCE_BYTES + 1)]),
        [],
    );
    assert_eq!(
        long_reference,
        Err(DelegationPolicyError::PrincipalBoundTooLarge)
    );
    assert_eq!(
        PrincipalBound::new([], None, ["Not A Class".to_owned()]),
        Err(DelegationPolicyError::MalformedDataClass)
    );
}

/// The digest vectors Main runs too (`delegationgrant/grantset_test.go`): the
/// stored form parses, normalises and digests to the same bytes on both sides.
#[test]
fn grant_set_digests_match_the_shared_vectors() {
    let vectors: Value = serde_json::from_str(include_str!(
        "../../../proto/contracts/delegation-grant-digest-vectors-v1.json"
    ))
    .expect("vectors");
    let cases = vectors["cases"].as_array().expect("cases");
    assert!(cases.len() >= 7);
    for case in cases {
        let name = case["name"].as_str().expect("name");
        let set = GrantSet::from_canonical(&case["grant_set"])
            .unwrap_or_else(|error| panic!("{name}: {error}"));
        assert_eq!(
            crate::canonical::to_string(&set.canonical()).expect("canonical"),
            case["canonical"].as_str().expect("canonical"),
            "{name}: canonical form"
        );
        assert_eq!(
            set.digest(),
            case["digest"].as_str().expect("digest"),
            "{name}: digest"
        );
        let again = GrantSet::from_canonical(&set.canonical()).expect("round trip");
        assert_eq!(again, set, "{name}: round trip");
    }
}

/// A callee's toolkit references build the same grant set Main records
/// (`delegationgrant.FromTools`).
#[test]
fn callee_grant_sets_match_the_shared_vectors() {
    let vectors: Value = serde_json::from_str(include_str!(
        "../../../proto/contracts/delegation-grant-digest-vectors-v1.json"
    ))
    .expect("vectors");
    let cases = vectors["tools_cases"].as_array().expect("tools cases");
    assert!(cases.len() >= 5);
    for case in cases {
        let name = case["name"].as_str().expect("name");
        let tools = case["tools"].as_array().expect("tools");
        let classes: BTreeSet<String> = case["data_classes"]
            .as_array()
            .expect("data classes")
            .iter()
            .map(|class| class.as_str().expect("class").to_owned())
            .collect();
        let set = GrantSet::from_callee(tools, &classes);
        assert_eq!(
            crate::canonical::to_string(&set.canonical()).expect("canonical"),
            case["canonical"].as_str().expect("canonical"),
            "{name}: canonical form"
        );
        assert_eq!(
            set.digest(),
            case["digest"].as_str().expect("digest"),
            "{name}: digest"
        );
    }
}

// --- Delegation grants (ADR-0044 Amendment 1, #2166) ----------------------

/// A policy for a run rooted at agent 2 that carries `grants` (caller 2 unless
/// the edge says otherwise), each over `tools`.
fn grant_policy(mode: NarrowingMode, grants: &[(u64, u64, &[Value])]) -> DelegationPolicy {
    let edges = grants
        .iter()
        .enumerate()
        .map(|(index, (caller, callee, tools))| {
            let set = GrantSet::from_callee(tools, &BTreeSet::new());
            let grant =
                DelegationGrant::new(u64::try_from(index).expect("index") + 1, set.digest(), set)
                    .expect("grant");
            (
                EdgeKey {
                    caller: *caller,
                    callee: *callee,
                    kind: EdgeKind::AgentTool,
                },
                grant,
            )
        });
    DelegationPolicy::new(DEFAULT_MAX_DEPTH, mode, [])
        .expect("policy")
        .with_grants(2, edges)
        .expect("grants")
}

fn root_scope(policy: DelegationPolicy, own: &[Value]) -> DelegationScope {
    DelegationScope::root(Arc::new(policy), GrantSet::from_tools(own))
}

fn names(tools: &[Value]) -> Vec<String> {
    tools
        .iter()
        .filter_map(|tool| tool["toolkit_name"].as_str().map(str::to_owned))
        .collect()
}

#[test]
fn a_grant_lets_a_specialist_use_what_its_caller_does_not_hold() {
    // Agent2 holds nothing. Agent1 (id 1) holds git. The edge Agent2 -> Agent1 is granted.
    let git = [toolkit(7, "Git", &json!({}))];
    let policy = grant_policy(NarrowingMode::Enforce, &[(2, 1, &git)]);
    let caller = root_scope(policy.clone(), &[]);
    let mut callee = git.to_vec();
    let delegation = caller
        .delegate_edge(Some(1), &mut callee, &BTreeSet::new(), &PublicPolicy::Open)
        .expect("the grant lets it run");
    assert_eq!(names(&callee), ["Git"]);
    let grant = delegation.evidence.grant.as_ref().expect("grant evidence");
    assert_eq!(grant.grant_id, 1);
    assert_eq!(
        grant.grant_digest,
        GrantSet::from_callee(&git, &BTreeSet::new()).digest()
    );
    assert!(grant.narrowed.is_empty());
    let summary = delegation.evidence.summary();
    assert!(
        summary
            .iter()
            .any(|(key, value)| *key == "delegation_grant_id" && value == 1)
    );
    assert!(
        summary
            .iter()
            .any(|(key, _)| *key == "delegation_grant_digest")
    );
    assert!(!summary.iter().any(|(key, _)| *key == "delegation_narrowed"));

    // Without the grant (no edge, or an edge nobody approved) it is refused as before.
    let mut callee = git.to_vec();
    let refused = caller
        .delegate_edge(Some(5), &mut callee, &BTreeSet::new(), &PublicPolicy::Open)
        .expect_err("no grant for the edge Agent2 -> 5");
    assert_eq!(refused.refusal.code(), GRANT_MISSING);
    let mut callee = git.to_vec();
    assert!(
        caller
            .delegate(&mut callee, &BTreeSet::new(), &PublicPolicy::Open)
            .is_err()
    );

    // A run whose Main switch is off carries no grants and refuses too.
    let off = root_scope(DelegationPolicy::default(), &[]);
    let mut callee = git.to_vec();
    assert!(
        off.delegate_edge(Some(1), &mut callee, &BTreeSet::new(), &PublicPolicy::Open)
            .is_err()
    );
}

#[test]
fn what_the_callee_gained_after_approval_is_narrowed_out_and_recorded() {
    let approved = [toolkit(7, "Git", &json!({}))];
    let policy = grant_policy(NarrowingMode::Enforce, &[(2, 1, &approved)]);
    let caller = root_scope(policy, &[]);
    // The callee now also holds a database toolkit.
    let mut callee = vec![
        toolkit(7, "Git", &json!({})),
        toolkit(9, "Database", &json!({})),
    ];
    let delegation = caller
        .delegate_edge(Some(1), &mut callee, &BTreeSet::new(), &PublicPolicy::Open)
        .expect("it still runs");
    assert_eq!(names(&callee), ["Git"], "the new toolkit is narrowed out");
    let narrowed = &delegation.evidence.grant.as_ref().expect("grant").narrowed;
    assert_eq!(narrowed.len(), 1);
    assert_eq!(narrowed[0].kind, GrantKind::Toolkit);
    assert_eq!(narrowed[0].name, "Database");
    assert!(
        delegation
            .evidence
            .summary()
            .iter()
            .any(|(key, value)| *key == "delegation_narrowed" && value[0]["name"] == "Database")
    );
}

#[test]
fn a_newer_callee_with_the_same_or_a_smaller_set_loses_nothing() {
    let approved = [
        toolkit(7, "Git", &json!({"selected_tools": ["read", "write"]})),
        toolkit(8, "Wiki", &json!({})),
    ];
    let caller = root_scope(
        grant_policy(NarrowingMode::Enforce, &[(2, 1, &approved)]),
        &[],
    );
    let mut callee = vec![toolkit(7, "Git", &json!({"selected_tools": ["read"]}))];
    let delegation = caller
        .delegate_edge(Some(1), &mut callee, &BTreeSet::new(), &PublicPolicy::Open)
        .expect("runs");
    assert!(
        delegation
            .evidence
            .grant
            .expect("grant")
            .narrowed
            .is_empty()
    );
    assert_eq!(callee[0]["settings"]["selected_tools"], json!(["read"]));
}

#[test]
fn the_grant_and_the_caller_union_per_tool_and_a_wider_selection_is_cut() {
    // The caller selects [read]; the grant [write]; the callee selects every tool.
    let caller_tools = [toolkit(7, "Git", &json!({"selected_tools": ["read"]}))];
    let approved = [toolkit(7, "Git", &json!({"selected_tools": ["write"]}))];
    let caller = root_scope(
        grant_policy(NarrowingMode::Enforce, &[(2, 1, &approved)]),
        &caller_tools,
    );
    let mut callee = vec![toolkit(7, "Git", &json!({}))];
    let delegation = caller
        .delegate_edge(Some(1), &mut callee, &BTreeSet::new(), &PublicPolicy::Open)
        .expect("runs");
    assert_eq!(
        callee[0]["settings"]["selected_tools"],
        json!(["read", "write"])
    );
    let narrowed = delegation.evidence.grant.expect("grant").narrowed;
    assert!(
        narrowed
            .iter()
            .any(|item| item.kind == GrantKind::Tool && item.name == "*")
    );
    // A tool in neither set is narrowed by name.
    let mut callee = vec![toolkit(
        7,
        "Git",
        &json!({"selected_tools": ["read", "delete"]}),
    )];
    let delegation = caller
        .delegate_edge(Some(1), &mut callee, &BTreeSet::new(), &PublicPolicy::Open)
        .expect("runs");
    assert_eq!(callee[0]["settings"]["selected_tools"], json!(["read"]));
    assert!(
        delegation
            .evidence
            .grant
            .expect("grant")
            .narrowed
            .iter()
            .any(|item| item.name == "delete")
    );
}

#[test]
fn a_credential_the_grant_does_not_name_leaves_its_toolkit_out() {
    let named = [toolkit(
        7,
        "Git",
        &json!({"credential_reference_id": "ref-a"}),
    )];
    let caller = root_scope(grant_policy(NarrowingMode::Enforce, &[(2, 1, &named)]), &[]);
    let mut callee = named.to_vec();
    caller
        .delegate_edge(Some(1), &mut callee, &BTreeSet::new(), &PublicPolicy::Open)
        .expect("the grant names the credential");
    assert_eq!(names(&callee), ["Git"]);
    // The callee swapped its credential after approval: the toolkit cannot run on it.
    let mut swapped = vec![toolkit(
        7,
        "Git",
        &json!({"credential_reference_id": "ref-b"}),
    )];
    let delegation = caller
        .delegate_edge(Some(1), &mut swapped, &BTreeSet::new(), &PublicPolicy::Open)
        .expect("runs without it");
    assert!(swapped.is_empty());
    let narrowed = delegation.evidence.grant.expect("grant").narrowed;
    assert_eq!(narrowed[0].kind, GrantKind::Credential);
    assert_eq!(narrowed[0].name, "ref-b");
}

#[test]
fn the_grant_covers_its_edge_only_and_the_depth_cap_still_applies() {
    let git = [toolkit(7, "Git", &json!({}))];
    let policy = grant_policy(NarrowingMode::Enforce, &[(2, 1, &git)]);
    let caller = root_scope(policy, &[]);
    let mut callee = git.to_vec();
    let one = caller
        .delegate_edge(Some(1), &mut callee, &BTreeSet::new(), &PublicPolicy::Open)
        .expect("Agent1");
    // Agent1 (now the caller) delegates to Z (id 3), which holds git too. No grant for 1 -> 3.
    let mut z = git.to_vec();
    let refused = one
        .scope
        .delegate_edge(Some(3), &mut z, &BTreeSet::new(), &PublicPolicy::Open)
        .expect_err("the grant of 2 -> 1 does not cover 1 -> 3");
    assert_eq!(refused.refusal.code(), GRANT_MISSING);
    // The cap is enforced with a grant.
    let shallow = DelegationPolicy::new(1, NarrowingMode::Enforce, [])
        .expect("policy")
        .with_grants(
            2,
            grant_policy(NarrowingMode::Enforce, &[(2, 1, &git)])
                .grants
                .into_iter(),
        )
        .expect("grants");
    let caller = root_scope(shallow, &[]);
    let mut callee = git.to_vec();
    let one = caller
        .delegate_edge(Some(1), &mut callee, &BTreeSet::new(), &PublicPolicy::Open)
        .expect("depth 1");
    let mut deeper = git.to_vec();
    let refused = one
        .scope
        .delegate_edge(Some(1), &mut deeper, &BTreeSet::new(), &PublicPolicy::Open)
        .expect_err("depth 2 is past the cap of 1");
    assert_eq!(refused.refusal.code(), DEPTH_EXCEEDED);
}

#[test]
fn data_classes_follow_the_caller_and_the_grant() {
    let git = [toolkit(7, "Git", &json!({}))];
    let policy = DelegationPolicy::new(
        DEFAULT_MAX_DEPTH,
        NarrowingMode::Enforce,
        ["internal".to_owned()],
    )
    .expect("policy")
    .with_grants(
        2,
        [(
            EdgeKey {
                caller: 2,
                callee: 1,
                kind: EdgeKind::AgentTool,
            },
            {
                let set = GrantSet::from_callee(&git, &classes(&["pii"]));
                DelegationGrant::new(1, set.digest(), set).expect("grant")
            },
        )],
    )
    .expect("grants");
    let caller = root_scope(policy, &[]);
    // The grant covers pii; the caller's own class is internal; secret is in neither.
    let mut callee = git.to_vec();
    let delegation = caller
        .delegate_edge(
            Some(1),
            &mut callee,
            &classes(&["pii", "internal", "secret"]),
            &PublicPolicy::Open,
        )
        .expect("runs");
    let narrowed = delegation.evidence.grant.expect("grant").narrowed;
    assert_eq!(narrowed.len(), 1);
    assert_eq!(narrowed[0].kind, GrantKind::DataClass);
    assert_eq!(narrowed[0].name, "secret");
}

#[test]
fn an_unreachable_host_is_refused_with_or_without_a_grant_and_report_records_it() {
    let git = [toolkit(
        7,
        "Git",
        &json!({"base_url": "https://git.example.com/"}),
    )];
    let allow = PublicPolicy::Allowlist(crate::egress::Allowlist::default());
    let enforce = root_scope(grant_policy(NarrowingMode::Enforce, &[(2, 1, &git)]), &[]);
    let mut callee = git.to_vec();
    let refused = enforce
        .delegate_edge(Some(1), &mut callee, &BTreeSet::new(), &allow)
        .expect_err("the run's allowlist bounds every callee");
    assert!(matches!(
        refused.refusal,
        DelegationRefusal::GrantMissing {
            kind: GrantKind::EgressHost,
            ..
        }
    ));
    assert!(refused.evidence.grant.is_some());
    let report = root_scope(grant_policy(NarrowingMode::Report, &[(2, 1, &git)]), &[]);
    let mut callee = git.to_vec();
    let delegation = report
        .delegate_edge(Some(1), &mut callee, &BTreeSet::new(), &allow)
        .expect("report records and runs");
    assert!(!delegation.evidence.enforced);
    assert!(delegation.evidence.refusal.is_some());
}

#[test]
fn a_scope_cache_key_tells_two_callers_apart() {
    let git = [toolkit(7, "Git", &json!({}))];
    let policy = Arc::new(grant_policy(NarrowingMode::Enforce, &[(2, 1, &git)]));
    let a = DelegationScope::root(Arc::clone(&policy), GrantSet::from_tools(&git));
    let b = DelegationScope::root(policy, GrantSet::from_tools(&git));
    assert_eq!(a.cache_key(), b.cache_key());
    let mut callee = git.to_vec();
    let child = a
        .delegate_edge(Some(1), &mut callee, &BTreeSet::new(), &PublicPolicy::Open)
        .expect("runs");
    assert_ne!(child.scope.cache_key(), a.cache_key());
}

#[test]
fn a_policy_refuses_a_grant_that_does_not_match_its_digest() {
    let set = GrantSet::from_callee(&[toolkit(7, "Git", &json!({}))], &BTreeSet::new());
    assert_eq!(
        DelegationGrant::new(1, "0".repeat(64), set.clone()),
        Err(DelegationPolicyError::MalformedGrant)
    );
    assert_eq!(
        DelegationGrant::new(0, set.digest(), set),
        Err(DelegationPolicyError::MalformedGrant)
    );
    assert_eq!(EdgeKind::parse("agent_tool"), Some(EdgeKind::AgentTool));
    assert_eq!(EdgeKind::parse("skill"), None);
}

#[test]
fn a_grant_set_that_is_not_canonical_is_refused() {
    let schema = GRANT_SET_SCHEMA;
    for document in [
        json!("x"),
        json!({"schema": "other", "toolkits": [], "data_classes": null, "egress_hosts": []}),
        json!({"schema": schema, "toolkits": [{"key": "jira", "tools": {"only": []}, "credential": null}], "data_classes": null, "egress_hosts": []}),
        json!({"schema": schema, "toolkits": [{"key": "toolkit:1", "tools": {"some": []}, "credential": null}], "data_classes": null, "egress_hosts": []}),
        json!({"schema": schema, "toolkits": [{"key": "toolkit:1", "tools": {"only": [1]}, "credential": null}], "data_classes": null, "egress_hosts": []}),
        json!({"schema": schema, "toolkits": [{"key": "toolkit:1", "tools": {"only": []}, "credential": ""}], "data_classes": null, "egress_hosts": []}),
        json!({"schema": schema, "toolkits": [{"key": "toolkit:1", "tools": {"only": null}, "credential": null}], "data_classes": null, "egress_hosts": []}),
        json!({"schema": schema, "toolkits": [], "data_classes": ["Bad Class"], "egress_hosts": []}),
        json!({"schema": schema, "toolkits": [], "data_classes": null}),
    ] {
        assert_eq!(
            GrantSet::from_canonical(&document),
            Err(GrantSetError),
            "{document}"
        );
    }
}
