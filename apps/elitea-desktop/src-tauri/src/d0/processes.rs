//! A workspace's background processes on the two event channels (IPC.md,
//! "Background processes", #1223).
//!
//! The session's process registry (`elitea_local_tools::process`) calls
//! [`ProcessSink`] from the processes' own threads. Every event goes to the
//! app channel (`process://event`, the Processes pane), numbered per
//! workspace. While the turn that started a process runs, its start, output
//! and exit also go on that turn's `agent://event` stream, where the tool
//! card shows them; once the turn has ended (a pinned or kept process) only
//! the pane hears about it.

use std::collections::HashSet;
use std::sync::{Arc, Mutex};

use elitea_local_tools::process::{OutputDelta, ProcessEvents, ProcessInfo};
use serde::Serialize;
use serde_json::{Value, json};

use super::events::{EventEmitter, TurnEvents};

/// The event name the Processes pane listens to.
pub const EVENT_NAME: &str = "process://event";

/// One app-channel event, as the webview receives it.
#[derive(Clone, Debug, Serialize, PartialEq)]
pub struct ProcessEvent {
    pub workspace_id: String,
    /// Grows by one per event of this workspace, from 0 at the session's
    /// open: the pane can tell a gap.
    pub seq: u64,
    pub kind: &'static str,
    pub payload: Value,
}

/// The running turn a sink forwards to, and the processes it started.
struct TurnRoute {
    events: Arc<TurnEvents>,
    started: HashSet<String>,
}

/// A workspace session's process events.
pub struct ProcessSink {
    workspace_id: String,
    emitter: Arc<dyn EventEmitter>,
    seq: Mutex<u64>,
    turn: Mutex<Option<TurnRoute>>,
}

impl ProcessSink {
    #[must_use]
    pub fn new(workspace_id: &str, emitter: Arc<dyn EventEmitter>) -> Self {
        Self {
            workspace_id: workspace_id.to_owned(),
            emitter,
            seq: Mutex::new(0),
            turn: Mutex::new(None),
        }
    }

    /// The turn now running in the workspace (`None` when it ended).
    pub fn bind_turn(&self, events: Option<Arc<TurnEvents>>) {
        if let Ok(mut turn) = self.turn.lock() {
            *turn = events.map(|events| TurnRoute {
                events,
                started: HashSet::new(),
            });
        }
    }

    /// Whether a turn runs in the workspace now.
    pub fn turn_running(&self) -> bool {
        self.turn.lock().is_ok_and(|turn| turn.is_some())
    }

    fn app(&self, kind: &'static str, payload: Value) {
        let Ok(mut seq) = self.seq.lock() else {
            return;
        };
        self.emitter.emit_process(ProcessEvent {
            workspace_id: self.workspace_id.clone(),
            seq: *seq,
            kind,
            payload,
        });
        *seq += 1;
    }

    /// The running turn's channel, when it started `process_id`.
    fn turn_for(&self, process_id: &str) -> Option<Arc<TurnEvents>> {
        let turn = self.turn.lock().ok()?;
        let route = turn.as_ref()?;
        route
            .started
            .contains(process_id)
            .then(|| route.events.clone())
    }
}

fn info_payload(info: &ProcessInfo) -> Value {
    serde_json::to_value(info).unwrap_or(Value::Null)
}

impl ProcessEvents for ProcessSink {
    fn started(&self, info: &ProcessInfo) {
        let payload = info_payload(info);
        let turn = self.turn.lock().ok().and_then(|mut turn| {
            let route = turn.as_mut()?;
            route.started.insert(info.process_id.clone());
            Some(route.events.clone())
        });
        if let Some(events) = turn {
            events.send("process_started", payload.clone());
        }
        self.app("process_started", payload);
    }

    fn output(&self, delta: &OutputDelta<'_>) {
        let payload = json!({
            "process_id": delta.process_id,
            "stream": delta.stream,
            "data": delta.data,
            "offset": delta.offset,
        });
        if let Some(events) = self.turn_for(delta.process_id) {
            let mut turn_payload = payload.clone();
            turn_payload["call_id"] = json!(delta.call_id);
            events.send("tool_output_delta", turn_payload);
        }
        self.app("process_output", payload);
    }

    fn exited(&self, info: &ProcessInfo) {
        let payload = info_payload(info);
        if let Some(events) = self.turn_for(&info.process_id) {
            events.send("process_exited", payload.clone());
        }
        self.app("process_exited", payload);
    }

    fn updated(&self, info: &ProcessInfo) {
        self.app("process_updated", info_payload(info));
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::d0::events::VecEmitter;
    use elitea_local_tools::policy::SandboxMode;
    use elitea_local_tools::process::{
        KeepAlive, MAX_DELTA_BYTES, ProcessRegistry, ProcessState, SpawnSpec,
    };

    /// A plain `/bin/sh -c` spec: what these tests check is the events,
    /// not the sandbox (`elitea_local_tools` proves that one).
    fn spec(
        call_id: &str,
        script: &str,
        dir: &std::path::Path,
        keep_alive: KeepAlive,
    ) -> SpawnSpec {
        SpawnSpec {
            call_id: call_id.to_owned(),
            command: script.to_owned(),
            cwd: ".".to_owned(),
            cwd_path: dir.to_path_buf(),
            program: "/bin/sh".into(),
            args: vec!["-c".to_owned(), script.to_owned()],
            env: vec![("PATH".to_owned(), "/usr/bin:/bin".to_owned())],
            tty: false,
            sandbox: SandboxMode::WorkspaceWrite,
            network: false,
            loopback: false,
            keep_alive,
            max_wall: std::time::Duration::from_secs(120),
        }
    }

    /// The registry's pump batches a fast writer's output: on the turn's
    /// channel the start comes first, the deltas follow in offset order
    /// with no gap and none over the cap, and the exit comes last; the app
    /// channel numbers the same events per workspace.
    #[tokio::test]
    async fn tool_output_delta_is_throttled_and_ordered() {
        let emitter = Arc::new(VecEmitter::default());
        let turn = Arc::new(TurnEvents::new("t1".into(), emitter.clone()));
        let sink = Arc::new(ProcessSink::new("w1", emitter.clone()));
        sink.bind_turn(Some(turn));
        let registry = ProcessRegistry::default();
        registry.set_events(Some(sink.clone()));
        let dir = tempfile::tempdir().expect("dir");
        let process = registry
            .spawn(spec(
                "c1",
                // 40 000 lines, about 600 KB, as fast as the shell writes.
                "i=0; while [ $i -lt 40000 ]; do echo \"line $i of the output\"; i=$((i+1)); done",
                dir.path(),
                KeepAlive::Turn,
            ))
            .expect("spawn");
        assert!(process.wait_exit(std::time::Duration::from_secs(60)).await);
        assert!(
            process
                .wait_reported(std::time::Duration::from_secs(10))
                .await
        );
        assert_eq!(process.info().status, ProcessState::Exited);

        let turn_events = emitter.all();
        let kinds: Vec<_> = turn_events.iter().map(|event| event.kind).collect();
        assert_eq!(kinds.first(), Some(&"process_started"));
        assert_eq!(kinds.last(), Some(&"process_exited"));
        let deltas: Vec<_> = turn_events
            .iter()
            .filter(|event| event.kind == "tool_output_delta")
            .collect();
        // Batched: far fewer events than lines.
        assert!(
            !deltas.is_empty() && deltas.len() < 2000,
            "{}",
            deltas.len()
        );
        let mut next = 0;
        for delta in &deltas {
            assert_eq!(delta.payload["call_id"], "c1");
            let offset = delta.payload["offset"].as_u64().expect("offset");
            assert!(offset >= next, "out of order at {offset}");
            let data = delta.payload["data"].as_str().expect("data");
            assert!(data.len() <= MAX_DELTA_BYTES, "{}", data.len());
            next = offset + data.len() as u64;
        }
        assert_eq!(next, process.info().bytes);
        let seqs: Vec<_> = turn_events.iter().map(|event| event.seq).collect();
        assert!(seqs.windows(2).all(|pair| pair[1] == pair[0] + 1));

        let app = emitter.processes();
        assert_eq!(app.first().map(|event| event.kind), Some("process_started"));
        assert_eq!(app.last().map(|event| event.kind), Some("process_exited"));
        assert!(app.iter().all(|event| event.workspace_id == "w1"));
        let seqs: Vec<_> = app.iter().map(|event| event.seq).collect();
        assert_eq!(seqs, (0..app.len() as u64).collect::<Vec<_>>());
    }

    /// After the turn's end a pinned process is the pane's alone.
    #[tokio::test]
    async fn a_process_outliving_its_turn_reports_to_the_pane_only() {
        let emitter = Arc::new(VecEmitter::default());
        let sink = Arc::new(ProcessSink::new("w1", emitter.clone()));
        sink.bind_turn(Some(Arc::new(TurnEvents::new(
            "t1".into(),
            emitter.clone(),
        ))));
        let registry = ProcessRegistry::default();
        registry.set_events(Some(sink.clone()));
        let dir = tempfile::tempdir().expect("dir");
        let process = registry
            .spawn(spec(
                "c1",
                "read line; echo \"got $line\"",
                dir.path(),
                KeepAlive::Session,
            ))
            .expect("spawn");
        registry.end_turn().await;
        sink.bind_turn(None);
        let before = emitter.all().len();
        process.write(b"x\n").expect("write");
        assert!(process.wait_exit(std::time::Duration::from_secs(20)).await);
        assert!(
            process
                .wait_reported(std::time::Duration::from_secs(10))
                .await
        );
        assert_eq!(emitter.all().len(), before, "the ended turn heard more");
        let kinds: Vec<_> = emitter.processes().iter().map(|event| event.kind).collect();
        assert!(kinds.contains(&"process_output"), "{kinds:?}");
        assert_eq!(kinds.last(), Some(&"process_exited"));
    }
}
