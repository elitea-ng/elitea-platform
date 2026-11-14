//! Live index progress (#2041): the publisher's sequencing, rejection and
//! replay, the terminal that follows it, and the recovery of progress frames
//! an earlier claim left in the spool. No tool or network runs here: the output
//! plane is a scripted fake and the spool is the real encrypted one.

use std::collections::VecDeque;
use std::fs;
use std::os::unix::fs::PermissionsExt;
use std::path::PathBuf;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;

use super::output_delivery::{
    AgentOutputPreflight, AgentOutputSpoolFactory, AgentProgressConnector,
    AgentProgressPublisherConfig, AgentProgressReplaySession, AgentProgressSession,
};
use super::toolkit_delivery::{FreshToolkitDelivery, test_fresh_toolkit_delivery};
use super::toolkit_output::{ToolkitOutputPreflightOutcome, prepare_toolkit_output};
use super::toolkit_progress::{
    Narrator, ProgressChannel, ToolkitProgressError, ToolkitProgressPublisher,
};
use crate::indexing::progress::{
    ProgressNote, ProgressSignal, ProgressSink, ProgressStage, ProgressStop,
};
use crate::protocol::command::{
    TestOnlyConformanceHmacAuthenticator, VerifiedToolkitExecuteReadCommand,
    parse_and_verify_toolkit_execute_read_command,
};
use crate::protocol::control::{
    AcceptedAgentClaim, index_command_and_claim, test_accepted_agent_claim,
    test_agent_output_authority, test_toolkit_progress_cursor,
};
use crate::protocol::elitea::runtime::v1::{
    ClaimCommandResponseV1, ExecutionFenceV1, ExecutionOutputFrameV1, NodeEventV1,
    execution_output_frame_v1,
};
use crate::protocol::output::{
    RuntimeFailureKind, ToolkitExecuteReadTerminalOutput, build_execution_node_event_output_frame,
};
use crate::spool::{SpoolLimits, SpoolMasterKey};
use crate::transport::output_grpc::{
    ProgressRejectionWinner, ProgressReplayDecision, test_acknowledged_progress,
    test_rejected_progress,
};
use crate::transport::{OutputGrpcConfig, OutputGrpcError, PreparedOutputSpool};

const NOW: i64 = 1_700_000_000_000;

// --- fixtures ---------------------------------------------------------------

fn root() -> tempfile::TempDir {
    let temporary = tempfile::tempdir_in(std::env::temp_dir().canonicalize().expect("temp root"))
        .expect("private root");
    fs::set_permissions(temporary.path(), fs::Permissions::from_mode(0o700)).expect("permissions");
    temporary
}

fn preflight(root: PathBuf) -> AgentOutputPreflight {
    AgentOutputPreflight::new(
        root,
        SpoolMasterKey::new([b'p'; 32]),
        SpoolLimits {
            max_frames: 2,
            max_encrypted_bytes: 130 * 1024,
            max_frame_bytes: 64 * 1024,
        },
        OutputGrpcConfig {
            max_queued_frames: 1,
            max_queued_bytes: 64 * 1024,
            max_frame_bytes: 64 * 1024,
            max_server_credit_frames: 1,
            max_server_credit_bytes: 64 * 1024,
            stream_deadline: Duration::from_mins(1),
            ack_timeout: Duration::from_secs(1),
            workload_session_id: "workload-1".to_owned(),
            producer_id: "worker-1".to_owned(),
        },
    )
}

fn index_fixture(watermark: u64) -> (Vec<u8>, ClaimCommandResponseV1) {
    let (raw, mut response) = index_command_and_claim();
    response
        .receipt
        .as_mut()
        .expect("receipt")
        .claim_handoff_watermark = watermark;
    (raw, response)
}

fn verified(raw: &[u8]) -> VerifiedToolkitExecuteReadCommand {
    parse_and_verify_toolkit_execute_read_command(raw, Some(&TestOnlyConformanceHmacAuthenticator))
        .expect("authenticated index command")
}

fn claim(
    verified: &VerifiedToolkitExecuteReadCommand,
    response: &ClaimCommandResponseV1,
) -> AcceptedAgentClaim {
    test_accepted_agent_claim(verified, response.clone(), "workload-1", "worker-1", NOW)
        .expect("accepted claim")
}

fn fresh_delivery(raw: &[u8], response: &ClaimCommandResponseV1) -> FreshToolkitDelivery {
    let verified = verified(raw);
    let claim = claim(&verified, response);
    test_fresh_toolkit_delivery(
        crate::transport::command_bus::test_agent_delivery(raw.to_vec()),
        verified,
        claim,
    )
}

fn event(kind: &str) -> NodeEventV1 {
    NodeEventV1 {
        r#type: kind.to_owned(),
        message_id: Some("index-run:test".to_owned()),
        response_metadata: br#"{"tool_run_id":"plan","tool_name":"Planning"}"#.to_vec(),
        ..NodeEventV1::default()
    }
}

/// A fence of an earlier claim of the same run.
fn old_fence(response: &ClaimCommandResponseV1) -> ExecutionFenceV1 {
    let mut fence = response
        .receipt
        .as_ref()
        .expect("receipt")
        .fence
        .clone()
        .expect("fence");
    fence.claim_attempt -= 1;
    fence.lease_epoch -= 1;
    fence.fence_token = vec![b'o'; 32];
    fence
}

fn current_fence(response: &ClaimCommandResponseV1) -> ExecutionFenceV1 {
    response
        .receipt
        .as_ref()
        .expect("receipt")
        .fence
        .clone()
        .expect("fence")
}

fn progress_frame(raw: &[u8], fence: &ExecutionFenceV1, sequence: u64) -> ExecutionOutputFrameV1 {
    build_execution_node_event_output_frame(
        &verified(raw),
        fence,
        event("agent_tool_start"),
        sequence,
        NOW,
        sequence - 1,
    )
    .expect("progress frame")
}

async fn persist(
    output: &AgentOutputPreflight,
    fresh: &FreshToolkitDelivery,
    frame: &ExecutionOutputFrameV1,
) -> AgentOutputSpoolFactory {
    let factory =
        AgentOutputSpoolFactory::new(output.shared_policy(), Arc::new(fresh.spool_identity()));
    factory
        .reopen()
        .await
        .expect("open spool")
        .persist(frame.clone())
        .expect("persist frame");
    factory
}

// --- the scripted output plane ---------------------------------------------

#[derive(Clone)]
enum Live {
    Acknowledge,
    PersistThenUnavailable,
    PersistThenReject(ProgressRejectionWinner),
    /// The stream failed after Main's ACK arrived: the session retains it.
    RetainAckThenFail,
}

enum Replay {
    Acknowledge,
    Unavailable,
}

#[derive(Default)]
struct State {
    live: Mutex<VecDeque<Live>>,
    replay: Mutex<VecDeque<Replay>>,
    frames: Mutex<Vec<ExecutionOutputFrameV1>>,
    connects: AtomicUsize,
    replays: AtomicUsize,
    closes: AtomicUsize,
}

impl State {
    fn scripted(live: Vec<Live>, replay: Vec<Replay>) -> Arc<Self> {
        Arc::new(Self {
            live: Mutex::new(live.into()),
            replay: Mutex::new(replay.into()),
            ..Self::default()
        })
    }

    fn sequences(&self) -> Vec<u64> {
        self.frames
            .lock()
            .expect("frames")
            .iter()
            .map(|frame| frame.sequence)
            .collect()
    }
}

struct FakeConnector(Arc<State>);

struct FakeSession {
    prepared: Option<PreparedOutputSpool>,
    state: Arc<State>,
    retained: Mutex<Option<ProgressReplayDecision>>,
}

#[async_trait]
impl AgentProgressSession for FakeSession {
    async fn publish_progress(
        &mut self,
        frame: &ExecutionOutputFrameV1,
    ) -> Result<ProgressReplayDecision, OutputGrpcError> {
        self.state
            .frames
            .lock()
            .expect("frames")
            .push(frame.clone());
        let action = self
            .state
            .live
            .lock()
            .expect("live")
            .pop_front()
            .expect("scripted live action");
        match action {
            Live::Acknowledge => Ok(ProgressReplayDecision::Acknowledged(
                test_acknowledged_progress(frame).expect("synthetic ACK"),
            )),
            Live::PersistThenUnavailable => {
                self.prepared
                    .as_mut()
                    .expect("spool")
                    .persist(frame.clone())?;
                Err(OutputGrpcError::Unavailable("the scripted stream dropped"))
            }
            Live::RetainAckThenFail => {
                *self.retained.lock().expect("retained") =
                    Some(ProgressReplayDecision::Acknowledged(
                        test_acknowledged_progress(frame).expect("synthetic ACK"),
                    ));
                Err(OutputGrpcError::Unavailable("the scripted stream dropped"))
            }
            Live::PersistThenReject(winner) => {
                self.prepared
                    .as_mut()
                    .expect("spool")
                    .persist(frame.clone())?;
                Ok(ProgressReplayDecision::Rejected(
                    test_rejected_progress(frame, winner).expect("synthetic rejection"),
                ))
            }
        }
    }

    fn take_progress_decision(
        &self,
        _frame: &ExecutionOutputFrameV1,
    ) -> Result<Option<ProgressReplayDecision>, OutputGrpcError> {
        Ok(self.retained.lock().expect("retained").take())
    }

    async fn close(&mut self) -> Result<(), OutputGrpcError> {
        self.state.closes.fetch_add(1, Ordering::SeqCst);
        Ok(())
    }
}

struct FakeReplay {
    prepared: PreparedOutputSpool,
    expected: ExecutionOutputFrameV1,
    action: Replay,
}

#[async_trait]
impl AgentProgressReplaySession for FakeReplay {
    async fn wait(&mut self) -> Result<ProgressReplayDecision, OutputGrpcError> {
        match self.action {
            Replay::Unavailable => Err(OutputGrpcError::Unavailable("the replay dropped")),
            Replay::Acknowledge => {
                assert!(self.prepared.replays(&self.expected));
                self.prepared
                    .reconcile_pending_through(self.expected.sequence)?;
                Ok(ProgressReplayDecision::Acknowledged(
                    test_acknowledged_progress(&self.expected).expect("synthetic ACK"),
                ))
            }
        }
    }

    async fn close(&mut self) -> Result<(), OutputGrpcError> {
        Ok(())
    }
}

#[async_trait]
impl AgentProgressConnector for FakeConnector {
    type Session = FakeSession;
    type Replay = FakeReplay;

    async fn connect_progress(
        &self,
        prepared: PreparedOutputSpool,
    ) -> Result<Self::Session, OutputGrpcError> {
        assert_eq!(prepared.pending_frame_count(), 0);
        self.0.connects.fetch_add(1, Ordering::SeqCst);
        Ok(FakeSession {
            prepared: Some(prepared),
            state: Arc::clone(&self.0),
            retained: Mutex::new(None),
        })
    }

    async fn start_progress_replay(
        &self,
        prepared: PreparedOutputSpool,
        expected: &ExecutionOutputFrameV1,
    ) -> Result<Self::Replay, OutputGrpcError> {
        assert_eq!(prepared.pending_frame_count(), 1);
        assert!(prepared.replays(expected));
        self.0.replays.fetch_add(1, Ordering::SeqCst);
        let action = self
            .0
            .replay
            .lock()
            .expect("replay")
            .pop_front()
            .expect("scripted replay action");
        Ok(FakeReplay {
            prepared,
            expected: expected.clone(),
            action,
        })
    }
}

struct Run {
    _root: tempfile::TempDir,
    verified: VerifiedToolkitExecuteReadCommand,
    /// A second copy of the claim: the terminal's output authority.
    authority_claim: AcceptedAgentClaim,
    publisher: ToolkitProgressPublisher<FakeConnector>,
}

async fn run(state: Arc<State>, max_output_sessions: usize, watermark: u64) -> Run {
    let root = root();
    let output = preflight(root.path().to_path_buf());
    let (raw, response) = index_fixture(watermark);
    let ToolkitOutputPreflightOutcome::Empty(empty) =
        prepare_toolkit_output(&output, fresh_delivery(&raw, &response), NOW)
            .await
            .expect("preflight")
    else {
        panic!("a new spool is empty");
    };
    let (fresh, prepared) = empty.into_parts();
    let (_delivery, verified, accepted) = fresh.into_parts();
    let cursor = test_toolkit_progress_cursor(accepted, &verified).expect("progress cursor");
    let publisher = ToolkitProgressPublisher::new(
        cursor,
        prepared,
        Arc::new(FakeConnector(state)),
        AgentProgressPublisherConfig::new(max_output_sessions).expect("config"),
    );
    Run {
        _root: root,
        authority_claim: claim(&verified, &response),
        verified,
        publisher,
    }
}

// --- the publisher ----------------------------------------------------------

/// Frames take the watermark's successors one at a time, and the terminal the
/// sequence after the last acknowledged frame; it persists as the sole frame.
#[tokio::test]
async fn acknowledged_progress_is_sequential_and_the_terminal_takes_the_next_sequence() {
    let state = State::scripted(
        vec![Live::Acknowledge, Live::Acknowledge, Live::Acknowledge],
        vec![],
    );
    let mut run = run(Arc::clone(&state), 2, 6).await;
    for kind in ["agent_tool_start", "agent_tool_end", "agent_tool_start"] {
        run.publisher.publish(&run.verified, event(kind), NOW).await;
    }
    assert_eq!(state.sequences(), vec![7, 8, 9]);
    // One session carried every frame.
    assert_eq!(state.connects.load(Ordering::SeqCst), 1);
    let settled = run.publisher.finish(&run.verified).await.expect("settled");
    assert_eq!(settled.terminal_sequence, 10);
    assert_eq!(state.closes.load(Ordering::SeqCst), 1);

    let terminal = test_agent_output_authority(run.authority_claim)
        .bind_toolkit_execute_read_terminal_at(
            &run.verified,
            ToolkitExecuteReadTerminalOutput::Failure(RuntimeFailureKind::Internal),
            NOW,
            settled.terminal_sequence,
        )
        .expect("terminal");
    let (spool, _reopener) = settled
        .output
        .persist_terminal(&terminal)
        .await
        .expect("the terminal persists on the settled spool");
    assert_eq!(spool.pending_frame_count(), 1);
    assert_eq!(spool.pending_replay_frame(), Some(terminal));
}

/// A run that published nothing leaves the terminal at the watermark's
/// successor, as before progress existed.
#[tokio::test]
async fn a_run_without_progress_keeps_the_watermark_successor_for_its_terminal() {
    let state = State::scripted(vec![], vec![]);
    let run = run(Arc::clone(&state), 2, 3).await;
    let settled = run.publisher.finish(&run.verified).await.expect("settled");
    assert_eq!(settled.terminal_sequence, 4);
    assert_eq!(state.connects.load(Ordering::SeqCst), 0);
}

/// An event the contract refuses is dropped alone: no sequence is spent.
#[tokio::test]
async fn a_refused_event_is_dropped_without_spending_a_sequence() {
    let state = State::scripted(vec![Live::Acknowledge], vec![]);
    let mut run = run(Arc::clone(&state), 2, 0).await;
    run.publisher.publish(&run.verified, event(""), NOW).await;
    run.publisher
        .publish(&run.verified, event("agent_tool_start"), NOW)
        .await;
    assert_eq!(state.sequences(), vec![1]);
    assert_eq!(
        run.publisher
            .finish(&run.verified)
            .await
            .expect("settled")
            .terminal_sequence,
        2
    );
}

/// An uncertain delivery with no attempts left stops progress; if even the
/// final delivery at the end of the run fails the frame stays unresolved and no
/// terminal is written: recovery settles it from Main's watermark.
#[tokio::test]
async fn an_uncertain_frame_leaves_no_terminal_when_the_final_delivery_fails_too() {
    let state = State::scripted(
        vec![Live::PersistThenUnavailable],
        vec![Replay::Unavailable],
    );
    let mut run = run(Arc::clone(&state), 1, 0).await;
    run.publisher
        .publish(&run.verified, event("agent_tool_start"), NOW)
        .await;
    // Progress is over: a later event is not even attempted.
    run.publisher
        .publish(&run.verified, event("agent_tool_end"), NOW)
        .await;
    assert_eq!(state.sequences(), vec![1]);
    let Err(error) = run.publisher.finish(&run.verified).await else {
        panic!("an unresolved frame must not settle");
    };
    assert!(matches!(error, ToolkitProgressError::RecoveryRequired(_)));
}

/// Progress is best effort: a frame whose delivery stayed uncertain ends
/// progress, but the run's own terminal is still written once the output plane
/// answers at the end (here the replay is acknowledged).
#[tokio::test]
async fn an_uncertain_progress_frame_does_not_cost_the_terminal() {
    let state = State::scripted(
        vec![Live::PersistThenUnavailable],
        vec![Replay::Acknowledge],
    );
    let mut run = run(Arc::clone(&state), 1, 0).await;
    run.publisher
        .publish(&run.verified, event("agent_tool_start"), NOW)
        .await;
    assert!(run.publisher.is_stopped());
    let settled = run.publisher.finish(&run.verified).await.expect("settled");
    assert_eq!(settled.terminal_sequence, 2);
    let terminal = test_agent_output_authority(run.authority_claim)
        .bind_toolkit_execute_read_terminal_at(
            &run.verified,
            ToolkitExecuteReadTerminalOutput::Failure(RuntimeFailureKind::Internal),
            NOW,
            settled.terminal_sequence,
        )
        .expect("terminal");
    let (spool, _reopener) = settled
        .output
        .persist_terminal(&terminal)
        .await
        .expect("the terminal persists");
    assert_eq!(spool.pending_replay_frame(), Some(terminal));
}

/// A decision the stream received before it failed is taken, not lost: the
/// frame is acknowledged without a replay.
#[tokio::test]
async fn a_decision_retained_by_a_failed_stream_is_not_lost() {
    let state = State::scripted(vec![Live::RetainAckThenFail, Live::Acknowledge], vec![]);
    let mut run = run(Arc::clone(&state), 1, 0).await;
    run.publisher
        .publish(&run.verified, event("agent_tool_start"), NOW)
        .await;
    assert!(!run.publisher.is_stopped());
    assert_eq!(state.replays.load(Ordering::SeqCst), 0);
    run.publisher
        .publish(&run.verified, event("agent_tool_end"), NOW)
        .await;
    assert_eq!(state.sequences(), vec![1, 2]);
}

/// A frame whose first stream dropped is replayed exactly, once, from the
/// spool on a reopened session, and the cursor advances on that ACK.
#[tokio::test]
async fn an_uncertain_frame_is_replayed_exactly_and_the_sequence_continues() {
    let state = State::scripted(
        vec![Live::PersistThenUnavailable, Live::Acknowledge],
        vec![Replay::Acknowledge],
    );
    let mut run = run(Arc::clone(&state), 3, 0).await;
    run.publisher
        .publish(&run.verified, event("agent_tool_start"), NOW)
        .await;
    assert_eq!(state.replays.load(Ordering::SeqCst), 1);
    run.publisher
        .publish(&run.verified, event("agent_tool_end"), NOW)
        .await;
    // Sequence 1 was sent once live and replayed once; 2 followed it.
    assert_eq!(state.sequences(), vec![1, 2]);
    let settled = run.publisher.finish(&run.verified).await.expect("settled");
    assert_eq!(settled.terminal_sequence, 3);
}

/// Replays that keep failing stop at the bounded attempts, leaving the frame
/// unresolved (no terminal, no second frame).
#[tokio::test]
async fn replay_attempts_are_bounded() {
    let state = State::scripted(
        vec![Live::PersistThenUnavailable],
        vec![
            Replay::Unavailable,
            Replay::Unavailable,
            Replay::Unavailable,
            Replay::Unavailable,
            Replay::Unavailable,
        ],
    );
    let mut run = run(Arc::clone(&state), 3, 0).await;
    run.publisher
        .publish(&run.verified, event("agent_tool_start"), NOW)
        .await;
    assert_eq!(state.replays.load(Ordering::SeqCst), 2);
    assert!(matches!(
        run.publisher.finish(&run.verified).await,
        Err(ToolkitProgressError::RecoveryRequired(_))
    ));
}

/// Main's rejection (Stop, the deadline, a refused frame) ends progress. The
/// rejected frame is set aside as a record and the run's real terminal takes
/// its sequence (Main's decision is applied when the terminal is replayed).
#[tokio::test]
async fn a_rejected_frame_is_set_aside_and_the_real_terminal_takes_its_sequence() {
    for winner in [
        ProgressRejectionWinner::Cancelled,
        ProgressRejectionWinner::DeadlineExceeded,
        ProgressRejectionWinner::FrameRefused,
    ] {
        let state = State::scripted(
            vec![Live::Acknowledge, Live::PersistThenReject(winner)],
            vec![],
        );
        let mut run = run(Arc::clone(&state), 2, 0).await;
        run.publisher
            .publish(&run.verified, event("agent_tool_start"), NOW)
            .await;
        run.publisher
            .publish(&run.verified, event("agent_tool_end"), NOW)
            .await;
        assert!(run.publisher.rejected_by() == Some(winner));
        // Nothing follows a rejection.
        run.publisher
            .publish(&run.verified, event("agent_tool_start"), NOW)
            .await;
        assert_eq!(state.sequences(), vec![1, 2]);

        let settled = run.publisher.finish(&run.verified).await.expect("settled");
        assert_eq!(settled.terminal_sequence, 2);
        let terminal = test_agent_output_authority(run.authority_claim)
            .bind_toolkit_execute_read_terminal_at(
                &run.verified,
                ToolkitExecuteReadTerminalOutput::Failure(RuntimeFailureKind::Internal),
                NOW,
                settled.terminal_sequence,
            )
            .expect("terminal");
        let (spool, _reopener) = settled
            .output
            .persist_terminal(&terminal)
            .await
            .expect("the terminal takes the rejected frame's sequence");
        assert_eq!(spool.pending_frame_count(), 1);
        assert_eq!(spool.pending_replay_frame(), Some(terminal));
    }
}

// --- recovery of frames an earlier claim left -------------------------------

async fn spool_is_empty(factory: &AgentOutputSpoolFactory) -> bool {
    factory
        .reopen()
        .await
        .expect("reopen")
        .pending_frame_count()
        == 0
}

/// An unaccepted progress frame of an older claim beyond Main's watermark can
/// never be accepted: the new claim retires it and proceeds on an empty spool.
#[tokio::test]
async fn a_superseded_progress_frame_is_retired_and_the_claim_proceeds() {
    let root = root();
    let output = preflight(root.path().to_path_buf());
    let (raw, response) = index_fixture(4);
    let fresh = fresh_delivery(&raw, &response);
    let factory = persist(
        &output,
        &fresh,
        &progress_frame(&raw, &old_fence(&response), 5),
    )
    .await;
    let outcome = prepare_toolkit_output(&output, fresh, NOW)
        .await
        .expect("a superseded frame does not block the claim");
    assert!(matches!(outcome, ToolkitOutputPreflightOutcome::Empty(_)));
    // The empty outcome owns the spool; release it before looking.
    drop(outcome);
    assert!(spool_is_empty(&factory).await);
}

/// A frame at or below the watermark is already durable in Main: reconciled,
/// whichever claim bound it.
#[tokio::test]
async fn a_progress_frame_the_watermark_covers_is_reconciled() {
    for older in [true, false] {
        let root = root();
        let output = preflight(root.path().to_path_buf());
        let (raw, response) = index_fixture(5);
        let fresh = fresh_delivery(&raw, &response);
        let fence = if older {
            old_fence(&response)
        } else {
            current_fence(&response)
        };
        let factory = persist(&output, &fresh, &progress_frame(&raw, &fence, 3)).await;
        let outcome = prepare_toolkit_output(&output, fresh, NOW)
            .await
            .expect("a covered frame does not block the claim");
        assert!(matches!(outcome, ToolkitOutputPreflightOutcome::Empty(_)));
        drop(outcome);
        assert!(spool_is_empty(&factory).await);
    }
}

/// A frame above the watermark is set aside whichever fence bound it: the
/// claim's own, or one newer in either component. It never wedges the command.
#[tokio::test]
async fn progress_above_the_watermark_is_set_aside_whatever_its_fence() {
    for bump in [(0_u64, 0_u64), (1, 0), (0, 1), (1, 1)] {
        let root = root();
        let output = preflight(root.path().to_path_buf());
        let (raw, response) = index_fixture(4);
        let fresh = fresh_delivery(&raw, &response);
        let mut fence = current_fence(&response);
        fence.claim_attempt += bump.0;
        fence.lease_epoch += bump.1;
        let factory = persist(&output, &fresh, &progress_frame(&raw, &fence, 5)).await;
        let outcome = prepare_toolkit_output(&output, fresh, NOW)
            .await
            .expect("the frame does not block the claim");
        assert!(matches!(outcome, ToolkitOutputPreflightOutcome::Empty(_)));
        drop(outcome);
        assert!(spool_is_empty(&factory).await);
    }
}

fn assert_silent(events: &[NodeEventV1]) {
    assert_eq!(events.len(), 0, "{events:?}");
}

// --- what the run says ------------------------------------------------------

fn types(events: &[NodeEventV1]) -> Vec<(String, String)> {
    events
        .iter()
        .map(|event| {
            let metadata: serde_json::Value =
                serde_json::from_slice(&event.response_metadata).expect("metadata JSON");
            (
                event.r#type.clone(),
                metadata["tool_run_id"].as_str().expect("run id").to_owned(),
            )
        })
        .collect()
}

/// The stages read as steps in the shape Main projects and the index view
/// renders; every event is valid under the `NodeEvent` contract.
#[test]
fn stages_become_tool_steps_main_and_the_view_understand() {
    let mut narrator = Narrator::new("exec-1");
    let mut events = Vec::new();
    events.extend(narrator.events(ProgressNote::Started(ProgressStage::Plan), NOW));
    events.extend(narrator.events(
        ProgressNote::Planned {
            to_write: 12,
            unchanged: 3,
            vanished: 1,
        },
        NOW + 10,
    ));
    events.extend(narrator.events(ProgressNote::Started(ProgressStage::Write), NOW + 20));
    events.extend(narrator.events(
        ProgressNote::Written {
            documents: 12,
            chunks: 40,
        },
        NOW + 30,
    ));
    assert_eq!(
        types(&events),
        vec![
            ("agent_tool_start".to_owned(), "plan".to_owned()),
            ("agent_tool_end".to_owned(), "plan".to_owned()),
            ("agent_tool_start".to_owned(), "write".to_owned()),
            ("agent_tool_end".to_owned(), "write".to_owned()),
        ]
    );
    for event in &events {
        assert_eq!(event.message_id.as_deref(), Some("index-run:exec-1"));
        crate::protocol::node_event::encode_current_node_event_json(event)
            .expect("a valid current NodeEvent");
        let metadata: serde_json::Value =
            serde_json::from_slice(&event.response_metadata).expect("metadata JSON");
        // Strings, as Main's projection decodes them.
        let stamp = metadata
            .get("timestamp_start")
            .or_else(|| metadata.get("timestamp_finish"))
            .and_then(serde_json::Value::as_str)
            .expect("an RFC 3339 timestamp");
        assert!(stamp.ends_with('Z'), "{stamp}");
    }
    let end = events.last().expect("end");
    assert_eq!(
        end.content,
        br#""12 documents indexed (40 chunks)""#.to_vec()
    );
}

/// A run that writes nothing says nothing of writing.
#[test]
fn a_run_with_nothing_to_write_reports_no_write_stage() {
    let mut narrator = Narrator::new("exec-1");
    narrator.events(
        ProgressNote::Planned {
            to_write: 0,
            unchanged: 9,
            vanished: 0,
        },
        NOW,
    );
    assert_silent(&narrator.events(ProgressNote::Started(ProgressStage::Write), NOW));
    assert_silent(&narrator.events(
        ProgressNote::Documents {
            processed: 50,
            total: 100,
            chunks: 1,
        },
        NOW + 60_000,
    ));
    assert_silent(&narrator.events(
        ProgressNote::Written {
            documents: 0,
            chunks: 0,
        },
        NOW,
    ));
}

/// A large index does not flood the stream: document updates are spaced by
/// count and time and capped.
#[test]
fn document_updates_are_throttled_by_count_time_and_cap() {
    let mut narrator = Narrator::new("exec-1");
    narrator.events(
        ProgressNote::Planned {
            to_write: 10_000,
            unchanged: 0,
            vanished: 0,
        },
        NOW,
    );
    narrator.events(ProgressNote::Started(ProgressStage::Write), NOW);
    let total = 10_000;
    let mut updates = 0_usize;
    let mut at = NOW;
    for processed in 1..total {
        // Ten documents a second, a fast index.
        at += 100;
        let events = narrator.events(
            ProgressNote::Documents {
                processed,
                total,
                chunks: processed * 3,
            },
            at,
        );
        if !events.is_empty() {
            // One frame: the running write step again, with its new text.
            assert_eq!(events.len(), 1);
            assert_eq!(events[0].r#type, "agent_tool_start");
            updates += 1;
        }
    }
    assert!(updates > 0, "a long run still reports");
    assert!(updates <= 20, "{updates} updates");

    // The count rule: a small index reports every few documents, not each one.
    let mut narrator = Narrator::new("exec-2");
    narrator.events(
        ProgressNote::Planned {
            to_write: 30,
            unchanged: 0,
            vanished: 0,
        },
        NOW,
    );
    narrator.events(ProgressNote::Started(ProgressStage::Write), NOW);
    let mut sent = Vec::new();
    for processed in 1..=30 {
        let events = narrator.events(
            ProgressNote::Documents {
                processed,
                total: 30,
                chunks: processed,
            },
            NOW + i64::try_from(processed).expect("small") * 2_000,
        );
        if !events.is_empty() {
            sent.push(processed);
        }
    }
    // Every fifth document, never the last (the stage's end reports it).
    assert_eq!(sent, vec![5, 10, 15, 20, 25]);

    // The time rule: documents that arrive in a burst wait for the interval.
    let mut narrator = Narrator::new("exec-3");
    narrator.events(
        ProgressNote::Planned {
            to_write: 100,
            unchanged: 0,
            vanished: 0,
        },
        NOW,
    );
    narrator.events(ProgressNote::Started(ProgressStage::Write), NOW);
    assert_silent(&narrator.events(
        ProgressNote::Documents {
            processed: 50,
            total: 100,
            chunks: 1,
        },
        NOW + 10,
    ));
}

// --- the channel: the run never waits on the output plane --------------------

fn ticking_clock() -> impl Fn() -> i64 + Send + Sync {
    let ticks = std::sync::atomic::AtomicI64::new(NOW);
    move || ticks.fetch_add(2_000, Ordering::SeqCst)
}

fn written_by(state: &State) -> Vec<(String, String)> {
    state
        .frames
        .lock()
        .expect("frames")
        .iter()
        .map(|frame| {
            let Some(execution_output_frame_v1::Payload::NodeEvent(event)) = frame.payload.as_ref()
            else {
                panic!("a progress frame")
            };
            (
                event.r#type.clone(),
                String::from_utf8_lossy(&event.content).into_owned(),
            )
        })
        .collect()
}

/// `note` only queues: a long run of document updates with nothing draining
/// leaves one frame of them, the newest.
#[tokio::test]
async fn document_updates_coalesce_and_never_wait_for_the_output_plane() {
    let state = State::scripted(vec![Live::Acknowledge; 8], vec![]);
    let mut run = run(Arc::clone(&state), 2, 0).await;
    let clock = ticking_clock();
    let channel = ProgressChannel::new("exec-1", &clock);
    assert_eq!(
        channel.note(ProgressNote::Started(ProgressStage::Plan)),
        ProgressSignal::Continue
    );
    channel.note(ProgressNote::Planned {
        to_write: 100,
        unchanged: 0,
        vanished: 0,
    });
    channel.note(ProgressNote::Started(ProgressStage::Write));
    for processed in 1..100 {
        channel.note(ProgressNote::Documents {
            processed,
            total: 100,
            chunks: processed,
        });
    }
    channel.finish_run("done");
    channel.drain(&mut run.publisher, &run.verified).await;
    // plan start, plan end, write start, ONE document update, write end.
    assert_eq!(state.sequences(), vec![1, 2, 3, 4, 5]);
}

/// Main's rejection reaches the run at its next note.
#[tokio::test]
async fn a_rejection_reaches_the_run_at_its_next_note() {
    for (winner, expected) in [
        (
            ProgressRejectionWinner::Cancelled,
            ProgressSignal::Stop(ProgressStop::Cancelled),
        ),
        (
            ProgressRejectionWinner::DeadlineExceeded,
            ProgressSignal::Stop(ProgressStop::DeadlineExceeded),
        ),
        // A refused frame only ends progress.
        (
            ProgressRejectionWinner::FrameRefused,
            ProgressSignal::Continue,
        ),
    ] {
        let state = State::scripted(vec![Live::PersistThenReject(winner)], vec![]);
        let mut run = run(Arc::clone(&state), 2, 0).await;
        let clock = ticking_clock();
        let channel = ProgressChannel::new("exec-1", &clock);
        channel.note(ProgressNote::Started(ProgressStage::Plan));
        channel.finish_run("stopped");
        channel.drain(&mut run.publisher, &run.verified).await;
        assert_eq!(
            channel.note(ProgressNote::Started(ProgressStage::Write)),
            expected
        );
    }
}

/// A halt closes every step still open, with the failure text, before the
/// terminal.
#[tokio::test]
async fn a_halt_closes_the_open_steps_with_its_text() {
    let state = State::scripted(vec![Live::Acknowledge; 8], vec![]);
    let mut run = run(Arc::clone(&state), 2, 0).await;
    let clock = ticking_clock();
    let channel = ProgressChannel::new("exec-1", &clock);
    channel.note(ProgressNote::Started(ProgressStage::Plan));
    channel.note(ProgressNote::Planned {
        to_write: 3,
        unchanged: 0,
        vanished: 0,
    });
    channel.note(ProgressNote::Started(ProgressStage::Write));
    channel.finish_run("the vector store is unavailable");
    channel.drain(&mut run.publisher, &run.verified).await;
    let frames = written_by(&state);
    assert_eq!(
        frames.last(),
        Some(&(
            "agent_tool_end".to_owned(),
            r#""the vector store is unavailable""#.to_owned()
        ))
    );
    // Plan start/end, write start, write end; nothing left open.
    assert_eq!(frames.len(), 4);
    let mut narrator = Narrator::new("x");
    narrator.events(ProgressNote::Started(ProgressStage::Plan), NOW);
    assert_eq!(narrator.close_open("boom", NOW).len(), 1);
    assert_eq!(narrator.close_open("boom", NOW).len(), 0);
}
