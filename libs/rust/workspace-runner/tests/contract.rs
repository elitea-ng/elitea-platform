//! ADR-0034 decision 9: the cloud executor's tool names and schemas equal
//! local-tools' `TOOLS` minus `git_commit`, in the same order.

use elitea_agent_runtime::edit_profile::EditProfile;
use elitea_agent_runtime::workspace_tools::{REPLACED_TOOL, WORKSPACE_TOOLS, workspace_tools_for};
use elitea_local_tools::session::{TOOLS, tools_for};

#[test]
fn workspace_tools_are_the_local_tools_minus_git_commit() {
    let local: Vec<_> = TOOLS
        .iter()
        .filter(|spec| spec.name != REPLACED_TOOL)
        .collect();
    assert!(TOOLS.iter().any(|spec| spec.name == REPLACED_TOOL));
    let mut unique: Vec<_> = TOOLS.iter().map(|spec| spec.name).collect();
    unique.sort_unstable();
    unique.dedup();
    assert_eq!(unique.len(), TOOLS.len(), "every tool name once");
    assert_eq!(
        local.iter().map(|spec| spec.name).collect::<Vec<_>>(),
        WORKSPACE_TOOLS
            .iter()
            .map(|spec| spec.name)
            .collect::<Vec<_>>(),
        "names and order"
    );
    for (local, cloud) in local.iter().zip(WORKSPACE_TOOLS) {
        assert_eq!(local.description, cloud.description, "{}", local.name);
        assert_eq!((local.parameters)(), (cloud.parameters)(), "{}", local.name);
        assert_eq!(local.kind.is_read_only(), cloud.read_only, "{}", local.name);
    }
}

/// #1214: each edit profile offers the same subset, descriptions and
/// schemas locally and in the cloud executor.
#[test]
fn every_edit_profile_offers_the_same_tools_locally_and_in_the_cloud() {
    for profile in [EditProfile::StrReplace, EditProfile::Patch] {
        let local: Vec<_> = tools_for(profile)
            .into_iter()
            .filter(|spec| spec.name != REPLACED_TOOL)
            .collect();
        let cloud = workspace_tools_for(profile);
        assert_eq!(
            local.iter().map(|spec| spec.name).collect::<Vec<_>>(),
            cloud.iter().map(|spec| spec.name).collect::<Vec<_>>(),
            "{profile:?}: names and order"
        );
        // The test tool goes wherever the shell tool does, under every
        // profile, and no name is offered twice.
        for names in [
            local.iter().map(|spec| spec.name).collect::<Vec<_>>(),
            cloud.iter().map(|spec| spec.name).collect::<Vec<_>>(),
        ] {
            assert!(names.contains(&"run_command"), "{profile:?}: {names:?}");
            assert!(names.contains(&"run_tests"), "{profile:?}: {names:?}");
            let mut unique = names.clone();
            unique.sort_unstable();
            unique.dedup();
            assert_eq!(unique.len(), names.len(), "{profile:?}: {names:?}");
        }
        for (local, cloud) in local.iter().zip(&cloud) {
            assert_eq!(
                local.description, cloud.description,
                "{profile:?} {}",
                local.name
            );
            assert_eq!(
                (local.parameters)(),
                (cloud.parameters)(),
                "{profile:?} {}",
                local.name
            );
            assert_eq!(
                local.kind.is_read_only(),
                cloud.read_only,
                "{profile:?} {}",
                local.name
            );
        }
    }
}

/// #1225: `exit_plan_mode` is local-only (the cloud executor has no plan
/// mode): in neither table, and its name is no other tool's.
#[test]
fn exit_plan_mode_is_local_only() {
    let name = elitea_local_tools::session::EXIT_PLAN_MODE.name;
    assert!(TOOLS.iter().all(|spec| spec.name != name));
    assert!(WORKSPACE_TOOLS.iter().all(|spec| spec.name != name));
}
