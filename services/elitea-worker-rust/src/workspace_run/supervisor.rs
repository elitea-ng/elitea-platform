//! The worker's half of the supervisor bridge (ADR-0034 decision 9): a
//! [`WorkspaceSandboxLauncher`] over the sandbox supervisor's
//! `WorkspaceRunService` (its dedicated `workspace` profile).
//!
//! * **launch**: a fresh Main grant for the run's spec, `StartWorkspaceRun`,
//!   then a `serve` exec kept open for the turn; its first stdin line is the
//!   `--env-stdin` handshake carrying the harvested env (never an argument,
//!   label or log field).
//! * **`push_tree`** / **`pull_tree`**: `unpack` and `pack` execs moving a
//!   workspace archive (`elitea_agent_runtime::workspace_archive`) through
//!   stdin and stdout in 32 KiB chunks; the archive is written and read on a
//!   blocking thread, never held whole.
//! * **`call_tool`**: one request line on the serve exec, one answer line
//!   back, one call at a time, bounded by the turn's wall.
//! * **destroy**: `DestroyWorkspaceRun`; idempotent. The supervisor also
//!   sweeps a run past its wall, so a crashed worker leaves nothing behind.

use std::path::Path;
use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use elitea_agent_runtime::workspace_archive::{self, ArchiveLimits};
use serde_json::Value;
use tokio::sync::{Mutex, mpsc};
use tokio_stream::StreamExt as _;
use tonic::transport::Channel;

use super::redact::SecretEnv;
use super::sandbox::{
    SandboxRunError, WorkspaceRunGrants, WorkspaceRunSpec, WorkspaceSandbox,
    WorkspaceSandboxLauncher,
};
use crate::protocol::elitea::runtime::v1::{
    DestroyWorkspaceRunRequestV1, StartWorkspaceRunRequestV1, WorkspaceRunExecInputV1,
    WorkspaceRunExecOpenV1, WorkspaceRunExecOutputV1, WorkspaceRunLimitsV1,
    WorkspaceRunOperationV1, WorkspaceRunSpecV1, workspace_run_exec_input_v1,
    workspace_run_exec_output_v1, workspace_run_service_client::WorkspaceRunServiceClient,
};
use crate::sandbox::workspace_run::spec_digest;

/// Bytes per stdin frame.
const CHUNK_BYTES: usize = 32 * 1024;
/// One answer line may be at most this long (a `read_file` of a large file).
const MAX_ANSWER_BYTES: usize = 40 * 1024 * 1024;
/// The deadline of start, unpack, pack and destroy calls.
const CALL_DEADLINE: Duration = Duration::from_mins(10);

fn status_error(status: &tonic::Status) -> SandboxRunError {
    match status.code() {
        tonic::Code::DeadlineExceeded => SandboxRunError::Timeout,
        tonic::Code::InvalidArgument | tonic::Code::PermissionDenied => {
            SandboxRunError::PolicyUnenforceable
        }
        _ => SandboxRunError::Unavailable,
    }
}

/// Starts workspace runs on the supervisor's `workspace` profile.
pub(crate) struct SupervisorWorkspaceLauncher {
    client: WorkspaceRunServiceClient<Channel>,
}

impl SupervisorWorkspaceLauncher {
    #[must_use]
    pub(crate) fn new(channel: Channel) -> Self {
        Self {
            client: WorkspaceRunServiceClient::new(channel)
                .max_encoding_message_size(64 * 1024 + 4096)
                .max_decoding_message_size(64 * 1024 + 4096),
        }
    }
}

fn limits(spec: &WorkspaceRunSpec) -> WorkspaceRunLimitsV1 {
    WorkspaceRunLimitsV1 {
        max_files: spec.max_files,
        max_file_bytes: spec.max_file_bytes,
        max_total_bytes: spec.max_total_bytes,
    }
}

fn archive_limits(spec: &WorkspaceRunSpec) -> ArchiveLimits {
    ArchiveLimits {
        max_files: spec.max_files,
        max_file_bytes: spec.max_file_bytes,
        max_total_bytes: spec.max_total_bytes,
    }
}

/// One open exec: its stdin sender and stdout stream.
struct Exec {
    stdin: mpsc::Sender<WorkspaceRunExecInputV1>,
    stdout: tonic::Streaming<WorkspaceRunExecOutputV1>,
}

/// What the exec's output ended with.
enum Output {
    Chunk(Vec<u8>),
    Exit { code: i32, failure: String },
}

impl Exec {
    async fn next(&mut self) -> Result<Output, SandboxRunError> {
        use workspace_run_exec_output_v1::Frame;
        match self.stdout.next().await {
            Some(Ok(WorkspaceRunExecOutputV1 {
                frame: Some(Frame::Stdout(chunk)),
            })) => Ok(Output::Chunk(chunk)),
            Some(Ok(WorkspaceRunExecOutputV1 {
                frame: Some(Frame::Exit(exit)),
            })) => Ok(Output::Exit {
                code: exit.exit_code,
                failure: exit.failure_code,
            }),
            Some(Err(status)) => Err(status_error(&status)),
            _ => Err(SandboxRunError::Protocol),
        }
    }

    async fn send(&self, chunk: Vec<u8>) -> Result<(), SandboxRunError> {
        self.stdin
            .send(WorkspaceRunExecInputV1 {
                frame: Some(workspace_run_exec_input_v1::Frame::Stdin(chunk)),
            })
            .await
            .map_err(|_| SandboxRunError::Unavailable)
    }

    async fn end(&self) -> Result<(), SandboxRunError> {
        self.stdin
            .send(WorkspaceRunExecInputV1 {
                frame: Some(workspace_run_exec_input_v1::Frame::StdinEnd(true)),
            })
            .await
            .map_err(|_| SandboxRunError::Unavailable)
    }
}

/// The exit of a finished operation of the run `run`, as the turn reads it.
/// A failure is logged with the runner's code (a stable `runner.*` word the
/// backend already filtered, never stderr text or workspace content).
fn exit_error(run: &str, code: i32, failure: &str) -> Result<(), SandboxRunError> {
    let error = match (code, failure) {
        (0, _) => return Ok(()),
        (_, "runner.workspace_too_large") => SandboxRunError::WorkspaceOverCap,
        (_, code) if code.ends_with("too_large") => SandboxRunError::TooLarge,
        _ => SandboxRunError::RunnerFailed,
    };
    tracing::warn!(
        event = "workspace_run_exec_failed",
        run,
        exit_code = code,
        failure_code = failure,
        cause_code = error.code(),
    );
    Err(error)
}

/// One running workspace sandbox on the supervisor.
pub(crate) struct SupervisorSandbox {
    client: WorkspaceRunServiceClient<Channel>,
    spec: WorkspaceRunSpec,
    wire: WorkspaceRunSpecV1,
    grants: Arc<dyn WorkspaceRunGrants>,
    /// The serve exec and its partial answer line.
    serve: Mutex<Option<(Exec, Vec<u8>)>>,
    destroyed: std::sync::atomic::AtomicBool,
}

impl SupervisorSandbox {
    async fn grant(
        &self,
    ) -> Result<crate::protocol::elitea::runtime::v1::SignedSandboxJobGrantV1, SandboxRunError>
    {
        self.grants
            .grant(&self.wire.name, spec_digest(&self.wire))
            .await
    }

    async fn open(
        &self,
        operation: WorkspaceRunOperationV1,
        policy_json: String,
    ) -> Result<Exec, SandboxRunError> {
        let grant = self.grant().await?;
        let (stdin, receiver) = mpsc::channel::<WorkspaceRunExecInputV1>(8);
        stdin
            .send(WorkspaceRunExecInputV1 {
                frame: Some(workspace_run_exec_input_v1::Frame::Open(
                    WorkspaceRunExecOpenV1 {
                        grant: Some(grant),
                        spec: Some(self.wire.clone()),
                        operation: operation as i32,
                        limits: Some(limits(&self.spec)),
                        policy_json,
                    },
                )),
            })
            .await
            .map_err(|_| SandboxRunError::Unavailable)?;
        let stdout = self
            .client
            .clone()
            .exec_workspace_run(tokio_stream::wrappers::ReceiverStream::new(receiver))
            .await
            .map_err(|status| status_error(&status))?
            .into_inner();
        Ok(Exec { stdin, stdout })
    }

    async fn start_serve(&self, env: &SecretEnv) -> Result<(), SandboxRunError> {
        let policy = serde_json::json!({
            "shell": self.spec.shell,
            // Proxy mode is the network on, through the proxy.
            "network": self.spec.network != super::sandbox::WorkspaceNetwork::None,
        })
        .to_string();
        let exec = self.open(WorkspaceRunOperationV1::Serve, policy).await?;
        // The handshake: values on stdin only.
        let vars: serde_json::Map<String, Value> = env
            .pairs()
            .map(|(name, value)| (name.to_owned(), Value::String(value.to_owned())))
            .collect();
        let mut line = zeroize::Zeroizing::new(
            serde_json::to_vec(&serde_json::json!({ "env": vars }))
                .map_err(|_| SandboxRunError::Protocol)?,
        );
        line.push(b'\n');
        for chunk in line.chunks(CHUNK_BYTES) {
            exec.send(chunk.to_vec()).await?;
        }
        *self.serve.lock().await = Some((exec, Vec::new()));
        Ok(())
    }
}

/// The serve session for one request: dropped out of step (an error, or a
/// caller that stopped waiting), it ends the session.
struct ServeSession<'a> {
    serve: &'a mut Option<(Exec, Vec<u8>)>,
    in_step: bool,
}

impl Drop for ServeSession<'_> {
    fn drop(&mut self) {
        if !self.in_step {
            *self.serve = None;
        }
    }
}

#[async_trait]
impl WorkspaceSandbox for SupervisorSandbox {
    async fn push_tree(&self, worktree: &Path) -> Result<(), SandboxRunError> {
        let exec = self
            .open(WorkspaceRunOperationV1::Unpack, String::new())
            .await?;
        let (chunks, mut receiver) = mpsc::channel::<Vec<u8>>(4);
        let root = worktree.to_path_buf();
        let limits = archive_limits(&self.spec);
        let writer = tokio::task::spawn_blocking(move || {
            let mut sink = ChannelWriter {
                sender: chunks,
                buffer: Vec::with_capacity(CHUNK_BYTES),
            };
            workspace_archive::write_tree(&root, &mut sink, limits)
                .map(|_| ())
                .map_err(|_| SandboxRunError::TooLarge)
        });
        let mut exec = exec;
        let pump = async {
            while let Some(chunk) = receiver.recv().await {
                exec.send(chunk).await?;
            }
            exec.end().await?;
            loop {
                match exec.next().await? {
                    Output::Chunk(_) => {}
                    Output::Exit { code, failure } => {
                        return exit_error(&self.wire.name, code, &failure);
                    }
                }
            }
        };
        let pumped = tokio::time::timeout(CALL_DEADLINE, pump)
            .await
            .map_err(|_| SandboxRunError::Timeout)?;
        writer.await.map_err(|_| SandboxRunError::Protocol)??;
        pumped
    }

    async fn call_tool(
        &self,
        tool: &str,
        call_id: &str,
        args: Value,
    ) -> Result<Value, SandboxRunError> {
        self.call_tool_approved(tool, call_id, args, false).await
    }

    async fn call_tool_approved(
        &self,
        tool: &str,
        call_id: &str,
        args: Value,
        approved: bool,
    ) -> Result<Value, SandboxRunError> {
        self.request(serde_json::json!({
            "tool": tool, "call_id": call_id, "args": args, "approved": approved,
        }))
        .await
    }

    async fn classify_command(
        &self,
        command: &str,
        cwd: Option<&str>,
    ) -> Result<bool, SandboxRunError> {
        let answer = self
            .request(serde_json::json!({"classify": {"command": command, "cwd": cwd}}))
            .await?;
        // Anything but an explicit `false` asks.
        Ok(answer.get("ask").and_then(Value::as_bool) != Some(false))
    }
    async fn pull_tree(&self, worktree: &Path) -> Result<(), SandboxRunError> {
        self.pull(worktree).await
    }

    async fn destroy(&self) -> Result<(), SandboxRunError> {
        self.teardown().await
    }
}

impl SupervisorSandbox {
    /// One request line on the serve exec, one answer line back, bounded by
    /// the turn's wall.
    async fn request(&self, request: Value) -> Result<Value, SandboxRunError> {
        let mut guard = self.serve.lock().await;
        // A caller that stops waiting (a take back's wrap-up drops a long
        // call after its grace) leaves the stream out of step: the session
        // ends, which closes the runner's stdin, and the runner kills the
        // call's command before the turn packs the workspace.
        let mut session = ServeSession {
            serve: &mut guard,
            in_step: false,
        };
        let (exec, pending) = session.serve.as_mut().ok_or(SandboxRunError::Unavailable)?;
        let mut line = serde_json::to_vec(&request).map_err(|_| SandboxRunError::Protocol)?;
        line.push(b'\n');
        for chunk in line.chunks(CHUNK_BYTES) {
            exec.send(chunk.to_vec()).await?;
        }
        let read = async {
            loop {
                if let Some(end) = pending.iter().position(|byte| *byte == b'\n') {
                    let answer: Vec<u8> = pending.drain(..=end).collect();
                    return serde_json::from_slice::<Value>(&answer[..end])
                        .map_err(|_| SandboxRunError::Protocol);
                }
                match exec.next().await? {
                    Output::Chunk(chunk) => {
                        if pending.len() + chunk.len() > MAX_ANSWER_BYTES {
                            return Err(SandboxRunError::Protocol);
                        }
                        pending.extend_from_slice(&chunk);
                    }
                    // The runner ended mid-turn.
                    Output::Exit { .. } => return Err(SandboxRunError::Unavailable),
                }
            }
        };
        let answer = tokio::time::timeout(self.spec.turn_wall, read)
            .await
            .unwrap_or(Err(SandboxRunError::Timeout));
        // An error leaves the serve stream out of step too.
        session.in_step = answer.is_ok();
        answer
    }

    async fn pull(&self, worktree: &Path) -> Result<(), SandboxRunError> {
        // Mid-turn too (a pull request's diff): the serve session stays; a
        // tool's writes are on the volume when its answer line is.
        let _serve = self.serve.lock().await;
        let mut exec = self
            .open(WorkspaceRunOperationV1::Pack, String::new())
            .await?;
        exec.end().await?;
        let (chunks, receiver) = mpsc::channel::<Vec<u8>>(4);
        let root = worktree.to_path_buf();
        let limits = archive_limits(&self.spec);
        let reader = tokio::task::spawn_blocking(move || {
            let mut source = ChannelReader {
                receiver,
                current: Vec::new(),
                offset: 0,
            };
            workspace_archive::read_tree(&mut source, &root, limits)
                .map(|_| ())
                .map_err(|_| SandboxRunError::TooLarge)
        });
        let pump = async {
            loop {
                match exec.next().await? {
                    Output::Chunk(chunk) => {
                        chunks
                            .send(chunk)
                            .await
                            .map_err(|_| SandboxRunError::Protocol)?;
                    }
                    Output::Exit { code, failure } => {
                        return exit_error(&self.wire.name, code, &failure);
                    }
                }
            }
        };
        let pumped = tokio::time::timeout(CALL_DEADLINE, pump)
            .await
            .map_err(|_| SandboxRunError::Timeout);
        drop(chunks);
        let read = reader.await.map_err(|_| SandboxRunError::Protocol)?;
        pumped??;
        read
    }

    async fn teardown(&self) -> Result<(), SandboxRunError> {
        if self
            .destroyed
            .swap(true, std::sync::atomic::Ordering::SeqCst)
        {
            return Ok(());
        }
        if let Some((exec, _)) = self.serve.lock().await.take() {
            let _ = exec.end().await;
        }
        let grant = self.grant().await;
        let result = match grant {
            Ok(grant) => self
                .client
                .clone()
                .destroy_workspace_run(DestroyWorkspaceRunRequestV1 {
                    grant: Some(grant),
                    spec: Some(self.wire.clone()),
                })
                .await
                .map(|_| ())
                .map_err(|status| status_error(&status)),
            Err(error) => Err(error),
        };
        if result.is_err() {
            // The supervisor's sweep removes it at its wall.
            tracing::warn!(
                event = "workspace_run_destroy_failed",
                run = self.wire.name.as_str(),
                "the workspace run could not be destroyed now; the supervisor sweeps it"
            );
            self.destroyed
                .store(false, std::sync::atomic::Ordering::SeqCst);
        }
        result
    }
}

#[async_trait]
impl WorkspaceSandboxLauncher for SupervisorWorkspaceLauncher {
    async fn launch(
        &self,
        spec: &WorkspaceRunSpec,
        env: &SecretEnv,
        grants: Arc<dyn WorkspaceRunGrants>,
    ) -> Result<Arc<dyn WorkspaceSandbox>, SandboxRunError> {
        let wire = spec.to_wire();
        let grant = grants.grant(&wire.name, spec_digest(&wire)).await?;
        tokio::time::timeout(
            CALL_DEADLINE,
            self.client
                .clone()
                .start_workspace_run(StartWorkspaceRunRequestV1 {
                    grant: Some(grant),
                    spec: Some(wire.clone()),
                }),
        )
        .await
        .map_err(|_| SandboxRunError::Timeout)?
        .map_err(|status| status_error(&status))?;
        let sandbox = Arc::new(SupervisorSandbox {
            client: self.client.clone(),
            spec: spec.clone(),
            wire,
            grants,
            serve: Mutex::new(None),
            destroyed: std::sync::atomic::AtomicBool::new(false),
        });
        if let Err(error) = sandbox.start_serve(env).await {
            let _ = sandbox.destroy().await;
            return Err(error);
        }
        Ok(sandbox)
    }
}

/// A blocking writer that hands 32 KiB chunks to the async side.
struct ChannelWriter {
    sender: mpsc::Sender<Vec<u8>>,
    buffer: Vec<u8>,
}

impl std::io::Write for ChannelWriter {
    fn write(&mut self, data: &[u8]) -> std::io::Result<usize> {
        let room = CHUNK_BYTES - self.buffer.len();
        let take = data.len().min(room);
        self.buffer.extend_from_slice(&data[..take]);
        if self.buffer.len() == CHUNK_BYTES {
            self.flush()?;
        }
        Ok(take)
    }

    fn flush(&mut self) -> std::io::Result<()> {
        if self.buffer.is_empty() {
            return Ok(());
        }
        let chunk = std::mem::replace(&mut self.buffer, Vec::with_capacity(CHUNK_BYTES));
        self.sender
            .blocking_send(chunk)
            .map_err(|_| std::io::Error::other("the exec stream closed"))
    }
}

/// A blocking reader over chunks the async side receives.
struct ChannelReader {
    receiver: mpsc::Receiver<Vec<u8>>,
    current: Vec<u8>,
    offset: usize,
}

impl std::io::Read for ChannelReader {
    fn read(&mut self, buffer: &mut [u8]) -> std::io::Result<usize> {
        while self.offset == self.current.len() {
            match self.receiver.blocking_recv() {
                Some(chunk) => {
                    self.current = chunk;
                    self.offset = 0;
                }
                None => return Ok(0),
            }
        }
        let count = buffer.len().min(self.current.len() - self.offset);
        buffer[..count].copy_from_slice(&self.current[self.offset..self.offset + count]);
        self.offset += count;
        Ok(count)
    }
}

/// Main's grants over the claim's sandbox authority (production).
pub(crate) struct ClaimWorkspaceGrants {
    pub(crate) control: Arc<
        crate::protocol::control::AgentControlClient<
            crate::transport::control_grpc::TonicControlRpc,
        >,
    >,
    pub(crate) authority: Arc<crate::protocol::control::ClaimBoundSandboxAuthority>,
    pub(crate) audience: String,
}

#[async_trait]
impl WorkspaceRunGrants for ClaimWorkspaceGrants {
    async fn grant(
        &self,
        activation_id: &str,
        digest: [u8; 32],
    ) -> Result<crate::protocol::elitea::runtime::v1::SignedSandboxJobGrantV1, SandboxRunError>
    {
        self.control
            .authorize_workspace_run(&self.authority, &self.audience, activation_id, &digest)
            .await
            .map_err(|_| SandboxRunError::Unavailable)
    }
}

#[cfg(test)]
mod exit_tests {
    use super::{SandboxRunError, exit_error};

    /// Every runner failure is a stable cause: the known ones keep their
    /// meaning, any other (`runner.git_baseline_failed`, or no code at all)
    /// is a runner failure, never "out of protocol".
    #[test]
    fn runner_failures_map_to_stable_causes() {
        assert_eq!(exit_error("r", 0, ""), Ok(()));
        assert_eq!(
            exit_error("r", 1, "runner.workspace_too_large"),
            Err(SandboxRunError::WorkspaceOverCap)
        );
        assert_eq!(
            exit_error("r", 1, "workspace_archive.too_large"),
            Err(SandboxRunError::TooLarge)
        );
        for failure in [
            "runner.git_baseline_failed",
            "runner.workspace_unavailable",
            "",
        ] {
            let error = exit_error("r", 1, failure).expect_err(failure);
            assert_eq!(error, SandboxRunError::RunnerFailed);
            assert_eq!(error.code(), "workspace_run.runner_failed");
        }
        assert_eq!(exit_error("r", -1, ""), Err(SandboxRunError::RunnerFailed));
    }
}

#[cfg(all(test, feature = "sandbox-supervisor"))]
#[path = "supervisor_tests.rs"]
pub(crate) mod tests;
