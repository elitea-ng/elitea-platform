//! Saved declaration selection. Main owns repository access and original visit admission.
use crate::sandbox::workspace::WorkspaceSelection;
use serde::Serialize;

/// No Debug. Exact configuration can contain saved source or private names.
pub(super) struct CodeOriginalDeclaration {
    pub(super) node_id: String,
    pub(super) graph_thread_id: String,
    pub(super) graph_step: u64,
    pub(super) configuration_json: String,
    #[allow(
        dead_code,
        reason = "Retain required protocol foundations without enabling deferred execution paths."
    )]
    pub(super) definition: [u8; 32],
    pub(super) yaml: [u8; 32],
}

/// This value comes only from one frozen Code definition and graph activation.
pub(super) struct CodeWorkspaceInvocation {
    pub(super) selection: WorkspaceSelection,
    #[allow(
        dead_code,
        reason = "Retain required protocol foundations without enabling deferred execution paths."
    )]
    pub(super) declaration: CodeOriginalDeclaration,
}

/// Private data-plane request. Main independently reads the original declaration.
#[derive(Serialize)]
#[allow(
    dead_code,
    reason = "Retain required protocol foundations without enabling deferred execution paths."
)]
pub(super) struct CodeWorkspaceDeclarationBinding<'a> {
    node_id: &'a str,
    graph_thread_id: &'a str,
    graph_step: String,
    configuration_json: &'a str,
    definition_sha256: String,
    yaml_sha256: String,
}

impl CodeOriginalDeclaration {
    #[allow(
        dead_code,
        reason = "Retain required protocol foundations without enabling deferred execution paths."
    )]
    pub(super) fn binding(&self) -> CodeWorkspaceDeclarationBinding<'_> {
        CodeWorkspaceDeclarationBinding {
            node_id: &self.node_id,
            graph_thread_id: &self.graph_thread_id,
            graph_step: self.graph_step.to_string(),
            configuration_json: &self.configuration_json,
            definition_sha256: hex(&self.definition),
            yaml_sha256: hex(&self.yaml),
        }
    }
}

#[allow(
    dead_code,
    reason = "Retain required protocol foundations without enabling deferred execution paths."
)]
fn hex(bytes: &[u8; 32]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    bytes
        .iter()
        .flat_map(|value| {
            [
                char::from(HEX[usize::from(value >> 4)]),
                char::from(HEX[usize::from(value & 15)]),
            ]
        })
        .collect()
}

#[cfg(test)]
#[path = "code_workspace_tests.rs"]
mod tests;
