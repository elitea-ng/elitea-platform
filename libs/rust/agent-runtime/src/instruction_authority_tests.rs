//! Unit tests of the instruction authority on its own: catalog admission,
//! scoped state, fresh-turn reset and activation deltas. The worker keeps the
//! suites that compose it with its sessions, internal tools and compaction.

use serde_json::json;

use super::*;
use crate::request::ProjectContextSnapshot;

fn plan(content: &str) -> InstructionPlan {
    let mut plan = InstructionPlan {
        run_id: "run-1".to_owned(),
        ..InstructionPlan::default()
    };
    plan.add_skills(&[json!({"id":"skill:1:version:2","name":"review","revision":content_digest(content),"scope":"project:1","instructions":content})]).unwrap();
    plan
}

#[test]
fn duplicate_revision_collision_scope_and_missing_state_are_checked() {
    let mut original = plan("old");
    let skill = json!({"id":"skill:1:version:2","name":"review","revision":content_digest("old"),"scope":"project:1","instructions":"old"});
    original.add_skills(&[skill]).unwrap();
    assert_eq!(original.catalog.len(), 1);
    let collision = json!({"id":"skill:9","name":"review","revision":content_digest("new"),"scope":"project:1","instructions":"new"});
    assert!(original.add_skills(&[collision]).is_err());
    assert!(original.resolve_skill(&json!({"name":"unknown"})).is_err());
    let state = original.start(None, "scope-a".to_owned()).unwrap();
    assert!(state.validate("scope-b").is_err());
    original.resume = true;
    assert!(original.start(None, "scope-a".to_owned()).is_err());
}
#[test]
fn project_context_eager_and_on_demand_activation_and_fresh_turn_reset() {
    let context = |description: &str| ProjectContextSnapshot {
        id: "project:17".to_owned(),
        revision: content_digest("Verbatim context.\n"),
        scope: "project:17".to_owned(),
        content: "Verbatim context.\n".to_owned(),
        activation_description: description.to_owned(),
    };
    let mut eager = InstructionPlan {
        run_id: "run-1".to_owned(),
        ..Default::default()
    };
    eager.add_project_context(&context("")).unwrap();
    assert_eq!(eager.active.len(), 1);
    let state = eager.start(None, "scope".to_owned()).unwrap();
    assert!(state.render().contains("Verbatim context.\n"));
    let mut next = InstructionPlan {
        run_id: "run-2".to_owned(),
        ..Default::default()
    };
    next.add_project_context(&context("release requests"))
        .unwrap();
    let reset = next
        .start(
            Some(serde_json::to_value(state).unwrap()),
            "scope".to_owned(),
        )
        .unwrap();
    assert!(reset.active.is_empty());
    assert!(!reset.render().contains("Verbatim context."));
    assert!(reset.render().contains("release requests"));
    let mut corrupt_context = context("");
    corrupt_context.content.push('!');
    assert!(next.add_project_context(&corrupt_context).is_err());
}

#[test]
fn fresh_nested_scope_does_not_inherit_parent_activation_or_catalog() {
    let mut parent = plan("Parent instructions.");
    parent.active.insert("skill:1:version:2".to_owned());
    parent.resume = true;
    let child = InstructionPlan::nested(&serde_json::Map::new(), &parent).unwrap();
    assert!(child.active.is_empty() && child.catalog.is_empty());
    assert!(child.start(None, "new-child-scope".to_owned()).is_ok());
}

#[test]
fn activation_delta_rejects_another_run_or_catalog() {
    let original = plan("Original instruction")
        .start(None, "scope".into())
        .unwrap();
    for changed in [
        InstructionState {
            activated_run_id: "other-run".into(),
            ..original.clone()
        },
        plan("Changed instruction")
            .start(None, "scope".into())
            .unwrap(),
        InstructionState {
            scope: "other-scope".into(),
            ..original.clone()
        },
    ] {
        let mut value = serde_json::to_value(changed).unwrap();
        assert!(
            merge_activation_delta(&mut value, Some(serde_json::to_value(&original).unwrap()))
                .is_err()
        );
    }
}

#[test]
fn check_skill_admits_exactly_what_the_plan_admits() {
    let good = json!({"id":"skill:1","name":"review","revision":content_digest("Check."),"scope":"project:1","instructions":"Check."});
    assert!(check_skill(&good).is_ok());
    let mut bad = Vec::new();
    for (key, value) in [
        ("revision", json!(content_digest("Other."))),
        ("revision", json!("")),
        ("id", json!("")),
        ("scope", json!("")),
        ("instructions", json!("")),
        ("name", json!("x".repeat(257))),
    ] {
        let mut skill = good.clone();
        skill[key] = value;
        bad.push(skill);
    }
    for key in ["id", "name", "revision", "scope", "instructions"] {
        let mut skill = good.clone();
        skill.as_object_mut().unwrap().remove(key);
        bad.push(skill);
    }
    bad.push(json!("review"));
    for skill in bad {
        assert!(check_skill(&skill).is_err(), "{skill}");
        let mut plan = InstructionPlan::default();
        assert!(plan.add_skills(&[skill]).is_err());
    }
}
