//! `subagents.max_depth` 2 (#1824) and `profile.model` (#1825).

use std::sync::Arc;
use std::sync::atomic::Ordering;

use serde_json::json;

use super::*;

/// The run's sub-agents with children that may start children (#1824).
fn nesting(host: &Arc<TestHost>, config: SubAgentConfig) -> SubAgents {
    SubAgents::new(host.clone(), config.with_max_depth(MAX_DEPTH))
}

/// The tools offered to each request whose task contains `needle`.
fn offered_by(host: &Arc<TestHost>, needle: &str) -> Vec<Vec<String>> {
    let tasks = host.model.tasks.lock().unwrap().clone();
    let offered = host.model.offered.lock().unwrap().clone();
    tasks
        .iter()
        .zip(offered)
        .filter(|(task, _)| task.contains(needle))
        .map(|(_, offered)| offered)
        .collect()
}

fn has_no_subagent_tool(offered: &[String]) -> bool {
    TOOL_NAMES
        .iter()
        .all(|name| !offered.contains(&(*name).to_owned()))
}

fn grant(keys: &[&str]) -> crate::tool_envelope::SubAgentGrant {
    use crate::tool_envelope::{SubAgentGrant, ToolEnvelope, ToolEnvelopeInput, ToolkitKinds};
    SubAgentGrant::new(
        ToolEnvelope::from_input(&ToolEnvelopeInput {
            subagent_tools: keys.iter().map(|key| (*key).to_owned()).collect(),
            subagent_tools_set: true,
            ..ToolEnvelopeInput::default()
        })
        .unwrap(),
        ToolkitKinds::new(),
    )
}

/// #1824: with depth 1 (the default) a child is offered no sub-agent tool,
/// as before; with depth 2 a child gets the four tools, its grandchild
/// answers it, and the grandchild gets none of them.
#[tokio::test]
async fn depth_two_lets_a_child_start_one_level_more() {
    // Depth 1: the child has no spawn tool, so no grandchild starts.
    let host = TestHost::new();
    let children = subagents(&host);
    let responses = run_parent(
        "parent",
        &children,
        spawn_then_wait(vec!["nest answer 1 deep".into()]),
    )
    .await;
    assert_eq!(wait_results(&responses).len(), 1);
    for offered in host.model.offered.lock().unwrap().iter() {
        assert!(has_no_subagent_tool(offered), "{offered:?}");
    }
    assert_eq!(host.started.lock().unwrap().len(), 1, "no grandchild");
    assert!(host.instructions.lock().unwrap()[0].starts_with(CHILD_PREAMBLE));

    // Depth 2.
    let host = TestHost::new();
    let children = nesting(&host, SubAgentConfig::new(""));
    let responses = run_parent(
        "parent",
        &children,
        spawn_then_wait(vec!["nest answer 1 deep".into()]),
    )
    .await;
    let results = wait_results(&responses);
    assert_eq!(results[0]["status"], "completed");
    assert_eq!(results[0]["result"], "nested: deep");
    let started = host.started.lock().unwrap().clone();
    assert_eq!(started.len(), 2);
    assert_eq!((started[0].depth, started[1].depth), (1, 2));
    assert_eq!(started[0].parent_agent_id, None);
    assert_eq!(
        started[1].parent_agent_id.as_deref(),
        Some(started[0].agent_id.as_str())
    );
    // Run-wide ordinals: the grandchild is the run's second spawn.
    assert_eq!((started[0].ordinal, started[1].ordinal), (1, 2));
    // The child was told it may start sub-agents; the grandchild was not.
    let instructions = host.instructions.lock().unwrap().clone();
    assert_eq!(instructions[0], NESTING_CHILD_PREAMBLE);
    assert_eq!(instructions[1], CHILD_PREAMBLE);
    // The child is offered the four tools; the grandchild none of them.
    let child = offered_by(&host, "nest");
    assert_ne!(child.len(), 0);
    for offered in child {
        for name in TOOL_NAMES {
            assert!(offered.contains(&name.to_owned()), "{offered:?}");
        }
    }
    let grandchild = offered_by(&host, "answer 1 deep")
        .into_iter()
        .filter(|offered| !offered.contains(&SPAWN_AGENT.to_owned()))
        .count();
    assert!(
        grandchild > 0,
        "the grandchild's request has no sub-agent tool"
    );
    assert_eq!(children.run_reports().len(), 2);
    assert_eq!(children.reports().len(), 1, "the parent's own children");
}

/// #1824: a grandchild never starts children, and a call from a child's
/// context into a registry it does not own is refused as at depth 1.
#[tokio::test]
async fn a_grandchild_is_refused_at_depth_two() {
    let host = TestHost::new();
    let children = nesting(&host, SubAgentConfig::new(""));
    let responses = run_parent(
        "parent",
        &children,
        spawn_then_wait(vec!["nest nest answer 1 deeper".into()]),
    )
    .await;
    assert_eq!(wait_results(&responses)[0]["status"], "completed");
    assert_eq!(host.started.lock().unwrap().len(), 2, "no third level");

    let nested = nesting(&TestHost::new(), SubAgentConfig::new(""));
    let responses = run_parent(
        "subagent_a_00000000",
        &nested,
        Box::new(|n, _| match n {
            0 => vec![spawn_call(0, "answer 1 deeper")],
            _ => vec![text("done")],
        }),
    )
    .await;
    assert_eq!(responses[0].1["code"], DEPTH_EXCEEDED);
    assert_eq!(nested.run_reports().len(), 0);
}

/// #1824: the run's spawn budget counts every level: a child cannot pass
/// what the run has left.
#[tokio::test]
async fn the_spawn_budget_is_the_runs_across_levels() {
    let host = TestHost::new();
    let children = nesting(
        &host,
        SubAgentConfig::new("").with_limits(SubAgentLimits {
            max_per_turn: 2,
            ..SubAgentLimits::default()
        }),
    );
    let responses = run_parent(
        "parent",
        &children,
        // Two children; the first then tries a grandchild after the second
        // took the run's last slot.
        Box::new(|n, request| match n {
            0 => vec![
                spawn_call(0, "answer 1 two"),
                spawn_call(1, "nest answer 1 deep"),
            ],
            1 => vec![call(
                "wait-1",
                WAIT_AGENT,
                json!({"agent_ids": agent_ids(request)}),
            )],
            _ => vec![text("done")],
        }),
    )
    .await;
    let results = wait_results(&responses);
    assert_eq!(results[0]["result"], "two");
    assert_eq!(results[1]["result"], "refused subagent_limit");
    assert_eq!(host.started.lock().unwrap().len(), 2);
}

/// #1824: the grant narrows at each level. A grandchild gets only tools its
/// parent was offered (the parent's `profile.tools`), and a write still
/// needs `subagent_tools`.
#[tokio::test]
async fn the_grant_narrows_at_each_level() {
    // The child asks for `look` only; its grandchild asks for the write the
    // child lacks: refused, nothing more starts.
    let host = TestHost::new();
    let children = nesting(
        &host,
        SubAgentConfig::new("").with_grant(grant(&["local/*"])),
    );
    let responses = run_parent(
        "parent",
        &children,
        spawn_once(json!({
            "task": "nest tools=local/write_file write",
            "profile": {"tools": ["local/look"]}
        })),
    )
    .await;
    assert_eq!(
        wait_results(&responses)[0]["result"],
        format!(
            "refused {}",
            crate::tool_envelope::SUBAGENT_TOOL_NOT_GRANTED
        )
    );
    assert_eq!(host.started.lock().unwrap().len(), 1);
    assert_eq!(host.write.0.load(Ordering::SeqCst), 0);

    // A grandchild that names no tools is offered only what its parent was.
    let host = TestHost::new();
    let children = nesting(
        &host,
        SubAgentConfig::new("").with_grant(grant(&["local/*"])),
    );
    let responses = run_parent(
        "parent",
        &children,
        spawn_once(json!({
            "task": "nest write",
            "profile": {"tools": ["local/look"]}
        })),
    )
    .await;
    assert_eq!(wait_results(&responses)[0]["status"], "completed");
    assert_eq!(host.started.lock().unwrap().len(), 2);
    assert_eq!(host.write.0.load(Ordering::SeqCst), 0, "a grandchild wrote");
    for offered in host.model.offered.lock().unwrap().iter() {
        assert!(!offered.contains(&"write_file".to_owned()), "{offered:?}");
    }

    // With the write granted and asked for at each level, it reaches the
    // grandchild.
    let host = TestHost::new();
    let children = nesting(
        &host,
        SubAgentConfig::new("").with_grant(grant(&["local/*"])),
    );
    run_parent(
        "parent",
        &children,
        spawn_once(json!({
            "task": "nest tools=local/write_file write",
            "profile": {"tools": ["local/write_file"]}
        })),
    )
    .await;
    assert_eq!(host.started.lock().unwrap().len(), 2);
    assert_eq!(
        host.write.0.load(Ordering::SeqCst),
        1,
        "the granted write ran"
    );

    // Without `subagent_tools` no level is offered a write.
    let host = TestHost::new();
    let children = nesting(&host, SubAgentConfig::new(""));
    run_parent(
        "parent",
        &children,
        spawn_then_wait(vec!["nest write".into()]),
    )
    .await;
    assert_eq!(host.started.lock().unwrap().len(), 2);
    assert_eq!(host.write.0.load(Ordering::SeqCst), 0);
    for offered in host.model.offered.lock().unwrap().iter() {
        assert!(!offered.contains(&"write_file".to_owned()), "{offered:?}");
    }
}

/// #1824: a grandchild is one delegation deeper than its parent: under a
/// maximum delegation depth of 1 the child's spawn is refused.
#[tokio::test]
async fn a_grandchild_counts_toward_the_delegation_depth() {
    use crate::delegation::{
        DEPTH_EXCEEDED as DELEGATION_DEPTH_EXCEEDED, DelegationPolicy, DelegationScope, GrantSet,
        NarrowingMode,
    };
    let policy = DelegationPolicy::new(1, NarrowingMode::Enforce, []).expect("policy");
    let root = DelegationScope::root(Arc::new(policy), GrantSet::unconstrained());
    let host = TestHost::new();
    let children = nesting(&host, SubAgentConfig::new("").with_delegation(root));
    let responses = run_parent(
        "parent",
        &children,
        spawn_then_wait(vec!["nest answer 1 deep".into()]),
    )
    .await;
    assert_eq!(
        wait_results(&responses)[0]["result"],
        format!("refused {DELEGATION_DEPTH_EXCEEDED}")
    );
    assert_eq!(host.started.lock().unwrap().len(), 1);
}

#[test]
fn the_depth_is_held_to_its_range() {
    assert_eq!(SubAgentConfig::new("").max_depth, DEFAULT_MAX_DEPTH);
    assert_eq!(SubAgentConfig::new("").with_max_depth(0).max_depth, 1);
    assert_eq!(
        SubAgentConfig::new("").with_max_depth(9).max_depth,
        MAX_DEPTH
    );
}

/// #1825: `profile.model` binds the child on the named model when the host
/// admits it; an unknown one is refused at spawn with
/// `SUBAGENT_MODEL_NOT_ALLOWED`; a grandchild inherits its parent's model.
#[tokio::test]
async fn a_named_model_binds_or_is_refused() {
    let host = TestHost::new();
    let children = subagents(&host);
    let responses = run_parent(
        "parent",
        &children,
        spawn_once(json!({"task": "answer 1 fine", "profile": {"model": "small-model"}})),
    )
    .await;
    assert_eq!(wait_results(&responses)[0]["status"], "completed");
    assert_eq!(
        host.models.lock().unwrap().clone(),
        [Some("small-model".to_owned())]
    );
    let digest = host.started.lock().unwrap()[0]
        .record
        .profile_digest
        .clone();
    assert_ne!(
        digest,
        Profile::default().digest(),
        "the digest names the model"
    );

    let host = TestHost::new();
    let children = subagents(&host);
    let responses = run_parent(
        "parent",
        &children,
        spawn_once(json!({"task": "answer 1 fine", "profile": {"model": "gpt-unknown"}})),
    )
    .await;
    assert_eq!(
        spawn_response(&responses)["code"],
        SUBAGENT_MODEL_NOT_ALLOWED
    );
    assert_eq!(host.started.lock().unwrap().len(), 0);
    assert_eq!(children.run_reports().len(), 0, "the slot was given back");

    let host = TestHost::new();
    let children = nesting(&host, SubAgentConfig::new(""));
    run_parent(
        "parent",
        &children,
        spawn_once(json!({"task": "nest answer 1 deep", "profile": {"model": "small-model"}})),
    )
    .await;
    assert_eq!(
        host.models.lock().unwrap().clone(),
        [
            Some("small-model".to_owned()),
            Some("small-model".to_owned())
        ],
        "the grandchild inherits its parent's model"
    );
    // "inherit" is the parent's model, and keeps the empty profile's digest.
    assert_eq!(
        profile::parse_profile(&json!({"profile": {"model": "inherit"}}))
            .unwrap()
            .digest(),
        Profile::default().digest()
    );
}

/// #1824 review: a child that answers without waiting for its own children
/// stops them, and the host is told, so none runs on after its parent.
#[tokio::test]
async fn a_childs_children_stop_when_it_answers() {
    let host = TestHost::new();
    let children = nesting(&host, SubAgentConfig::new(""));
    let responses = run_parent(
        "parent",
        &children,
        spawn_then_wait(vec!["leave hang".into()]),
    )
    .await;
    let results = wait_results(&responses);
    assert_eq!(results[0]["status"], "completed");
    assert_eq!(results[0]["result"], "left");
    assert_eq!(host.started.lock().unwrap().len(), 2);
    let finished = host.finished.lock().unwrap().clone();
    assert_eq!(finished.len(), 2, "{finished:?}");
    let grandchild = &host.started.lock().unwrap()[1];
    let report = finished
        .iter()
        .find(|report| report.agent_id == grandchild.agent_id)
        .expect("the grandchild is reported");
    assert_eq!(report.status, ChildStatus::Cancelled);
    // The run sees it settled, and closing the run reports it no second time.
    let run = children.run_reports();
    assert_eq!(run[1].1.status, ChildStatus::Cancelled);
    children.close_all();
    assert_eq!(host.finished.lock().unwrap().len(), 2);
}

/// #1824 review: only the run's agent may give a child a checkout of its
/// own; a sub-agent's spawn is offered no `isolation` and refused one, so an
/// isolated grandchild never escapes its ancestors' `profile.tools`.
#[tokio::test]
async fn a_sub_agent_cannot_start_an_isolated_child() {
    let host = TestHost::with_isolation(true);
    let nested = SubAgents::new(
        host.clone(),
        SubAgentConfig::new("").with_max_depth(MAX_DEPTH),
    )
    .inner
    .nested(Nesting {
        owner: ChildIdentity {
            agent_id: "a_00000000".to_owned(),
            parent_call_id: "call-0".to_owned(),
            label: None,
            ordinal: 1,
            depth: 1,
            parent_agent_id: None,
            record: profile::SpawnRecord::new(&Profile::default(), &Brief::default()),
        },
        delegation: None,
        model: None,
        requested: None,
    });
    assert!(!nested.isolation_allowed());
    let spawn = tools::tool(tools::Action::Spawn, Arc::clone(&nested));
    let schema = spawn.parameters_schema().expect("schema");
    assert!(schema["properties"].get("isolation").is_none(), "{schema}");

    let root = nesting(&host, SubAgentConfig::new(""));
    assert!(root.inner.isolation_allowed());
    let spawn = tools::tool(tools::Action::Spawn, Arc::clone(&root.inner));
    let schema = spawn.parameters_schema().expect("schema");
    assert!(schema["properties"].get("isolation").is_some(), "{schema}");
}
