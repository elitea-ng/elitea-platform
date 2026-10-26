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

/// #1236: `diagnostics` is local-only (the cloud executor runs no language
/// server): in neither table, and its name is no other tool's.
#[test]
fn diagnostics_is_local_only() {
    let name = elitea_local_tools::session::DIAGNOSTICS.name;
    assert!(TOOLS.iter().all(|spec| spec.name != name));
    assert!(WORKSPACE_TOOLS.iter().all(|spec| spec.name != name));
}

/// #1223: background processes are local-only. The shared `TOOLS` table
/// keeps the foreground `run_command` (the cloud executor offers the same
/// schema); the desktop swaps in `RUN_COMMAND_PROCESSES` and appends
/// `PROCESS_TOOLS`, none of which the cloud table names.
#[test]
fn background_processes_are_local_only() {
    use elitea_local_tools::session::{PROCESS_TOOLS, RUN_COMMAND_PROCESSES};
    for spec in PROCESS_TOOLS {
        assert!(
            TOOLS.iter().all(|tool| tool.name != spec.name),
            "{}",
            spec.name
        );
        assert!(
            WORKSPACE_TOOLS.iter().all(|tool| tool.name != spec.name),
            "{}",
            spec.name
        );
    }
    let foreground = TOOLS
        .iter()
        .find(|spec| spec.name == RUN_COMMAND_PROCESSES.name)
        .expect("run_command");
    let cloud = WORKSPACE_TOOLS
        .iter()
        .find(|spec| spec.name == RUN_COMMAND_PROCESSES.name)
        .expect("run_command");
    assert_eq!((foreground.parameters)(), (cloud.parameters)());
    let local = (RUN_COMMAND_PROCESSES.parameters)();
    for switch in ["background", "tty", "loopback", "keep_alive"] {
        assert!(local["properties"].get(switch).is_some(), "{switch}");
        assert!(
            (cloud.parameters)()["properties"].get(switch).is_none(),
            "{switch}"
        );
    }
}

/// The person's stdio MCP servers (#1230) are local-only: they run on the
/// device with the person's own credentials, never in the cloud executor
/// (the #1187 handoff note lists them as unavailable there). No shared or
/// cloud tool carries the `mcp__` prefix, and the runner's session has
/// `local_mcp` off, so no server could start in it.
#[tokio::test]
async fn local_mcp_tools_are_local_only() {
    use elitea_local_tools::mcp::naming::PREFIX;
    assert!(TOOLS.iter().all(|tool| !tool.name.starts_with(PREFIX)));
    assert!(
        WORKSPACE_TOOLS
            .iter()
            .all(|tool| !tool.name.starts_with(PREFIX))
    );
    let workspace = tempfile::tempdir().expect("workspace");
    let data = tempfile::tempdir().expect("data");
    let session = elitea_workspace_runner::open_session(&elitea_workspace_runner::RunnerConfig {
        root: workspace.path().to_path_buf(),
        data_dir: data.path().to_path_buf(),
        policy: elitea_workspace_runner::RunnerPolicy::default(),
        env: Vec::new(),
    })
    .expect("session");
    assert!(!session.rules().policy().local_mcp);
    assert_eq!(session.start_mcp_servers("cloud").await.len(), 0);
}

/// #1233: the memory tools are not workspace tools. Both hosts get the same
/// three from the runtime's own `memory` toolset over their `MemoryStore`
/// (`elitea_agent_runtime::memory_tools`), so neither table names them and
/// no workspace tool takes their names.
#[test]
fn memory_tools_are_the_runtimes_not_the_workspaces() {
    use elitea_agent_runtime::memory_tools::TOOL_NAMES;
    for name in TOOL_NAMES {
        assert!(TOOLS.iter().all(|tool| tool.name != name), "{name}");
        assert!(
            WORKSPACE_TOOLS.iter().all(|tool| tool.name != name),
            "{name}"
        );
    }
}
