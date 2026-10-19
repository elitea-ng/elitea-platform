//! ADR-0034 decision 9: the cloud executor's tool names and schemas equal
//! local-tools' `TOOLS` minus `git_commit`, in the same order.

use elitea_agent_runtime::workspace_tools::{REPLACED_TOOL, WORKSPACE_TOOLS};
use elitea_local_tools::session::TOOLS;

#[test]
fn workspace_tools_are_the_local_tools_minus_git_commit() {
    let local: Vec<_> = TOOLS
        .iter()
        .filter(|spec| spec.name != REPLACED_TOOL)
        .collect();
    assert!(TOOLS.iter().any(|spec| spec.name == REPLACED_TOOL));
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
