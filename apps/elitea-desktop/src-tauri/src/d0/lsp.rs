//! A workspace's language servers (#1236) as the desktop runs them: the
//! session's [`LspSettings`] from the workspace's agent settings (host
//! data, never the folder), and their status on the app channel
//! (`lsp://event`, IPC.md "Language servers") for the composer's chip.

use std::collections::BTreeMap;
use std::sync::Arc;
use std::sync::Mutex;
use std::time::Duration;

use elitea_local_tools::lsp::{LspEvents, LspSettings, LspStatus};
use serde::Serialize;

use super::events::EventEmitter;
use crate::workspaces::AgentSettings;

/// The event name the chip listens to.
pub const EVENT_NAME: &str = "lsp://event";

/// One app-channel event, as the webview receives it.
#[derive(Clone, Debug, Serialize, PartialEq)]
pub struct LspEvent {
    pub workspace_id: String,
    /// Grows by one per event of this workspace's session, from 0.
    pub seq: u64,
    /// `lsp_status`.
    pub kind: &'static str,
    pub payload: LspStatus,
}

/// The session's settings from the workspace's: diagnostics on unless the
/// person turned them off, and the waits they set.
#[must_use]
pub fn settings_for(agent: &AgentSettings) -> LspSettings {
    LspSettings {
        enabled: agent.diagnostics_enabled(),
        waits: agent
            .diagnostics_wait_ms
            .entries()
            .into_iter()
            .filter_map(|(language, wait)| {
                wait.map(|ms| (language, Duration::from_millis(u64::from(ms))))
            })
            .collect::<BTreeMap<_, _>>(),
        ..LspSettings::default()
    }
}

/// A workspace session's language-server events.
pub struct LspSink {
    workspace_id: String,
    emitter: Arc<dyn EventEmitter>,
    seq: Mutex<u64>,
}

impl LspSink {
    #[must_use]
    pub fn new(workspace_id: &str, emitter: Arc<dyn EventEmitter>) -> Self {
        Self {
            workspace_id: workspace_id.to_owned(),
            emitter,
            seq: Mutex::new(0),
        }
    }
}

impl LspEvents for LspSink {
    fn status(&self, status: &LspStatus) {
        let Ok(mut seq) = self.seq.lock() else {
            return;
        };
        self.emitter.emit_lsp(LspEvent {
            workspace_id: self.workspace_id.clone(),
            seq: *seq,
            kind: "lsp_status",
            payload: status.clone(),
        });
        *seq += 1;
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::d0::events::VecEmitter;
    use crate::workspaces::DiagnosticsWaits;
    use elitea_local_tools::lsp::{Language, LspState};

    #[test]
    fn workspace_settings_turn_diagnostics_on_by_default() {
        let default = settings_for(&AgentSettings::default());
        assert!(default.enabled);
        assert!(default.waits.is_empty());
        let off = settings_for(&AgentSettings {
            diagnostics: Some(false),
            diagnostics_wait_ms: DiagnosticsWaits {
                rust: Some(6_000),
                ..DiagnosticsWaits::default()
            },
            ..AgentSettings::default()
        });
        assert!(!off.enabled);
        assert_eq!(
            off.waits.get(&Language::Rust),
            Some(&Duration::from_secs(6))
        );
        assert!(off.programs.is_empty(), "never a program from settings");
    }

    #[test]
    fn statuses_go_out_numbered_per_workspace() {
        let emitter = Arc::new(VecEmitter::default());
        let sink = LspSink::new("ws", emitter.clone());
        for state in [LspState::Starting, LspState::Ready] {
            sink.status(&LspStatus {
                language: Language::Rust,
                state,
                message: None,
            });
        }
        let events = emitter.lsp();
        assert_eq!(events.len(), 2);
        assert_eq!((events[0].seq, events[1].seq), (0, 1));
        let wire = serde_json::to_value(&events[1]).unwrap();
        assert_eq!(
            wire,
            serde_json::json!({
                "workspace_id": "ws",
                "seq": 1,
                "kind": "lsp_status",
                "payload": { "language": "rust", "state": "ready" },
            })
        );
    }
}
