//! The supervisor's `workspace` profile (ADR-0034 decision 9): start a
//! workspace run, exec the runner's `unpack`, `serve` and `pack` in it with
//! stdin and stdout streamed, destroy it, and sweep runs past their wall.
//!
//! The service holds the policy (grant verification, the profile's image and
//! ceilings, the run's ownership binding, the runner's argv) and nothing that
//! touches a container: a [`WorkspaceRunBackend`] does that, on Docker
//! (`workspace_run_docker.rs`) or Kubernetes (`kubernetes/workspace_run.rs`).
//! Tests fake the backend.

use std::collections::BTreeMap;
use std::future::Future;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;
use tokio::io::{AsyncRead, AsyncReadExt as _, AsyncWrite, AsyncWriteExt as _};
use tokio::sync::mpsc;
use tokio_stream::wrappers::ReceiverStream;
use tonic::{Request, Response, Status, Streaming};

use super::workspace_run::{SWEEP_GRACE_SECONDS, WorkspaceRunContainer, runner_argv};
use crate::protocol::command::Ed25519PublicKeyResolver;
use crate::protocol::elitea::runtime::v1::{
    DestroyWorkspaceRunRequestV1, DestroyWorkspaceRunResponseV1, StartWorkspaceRunRequestV1,
    StartWorkspaceRunResponseV1, WorkspaceRunExecExitV1, WorkspaceRunExecInputV1,
    WorkspaceRunExecOutputV1, WorkspaceRunOperationV1, WorkspaceRunSpecV1,
    workspace_run_exec_input_v1, workspace_run_exec_output_v1,
    workspace_run_service_server::{WorkspaceRunService, WorkspaceRunServiceServer},
};
use crate::protocol::sandbox_grant::{AuthorizedWorkspaceRun, GrantVerifier};

/// Stream chunks, both directions.
pub const EXEC_CHUNK_BYTES: usize = 32 * 1024;
const PIPE_BYTES: usize = 128 * 1024;
const SWEEP_INTERVAL: Duration = Duration::from_secs(30);

/// What a run carries so a later call can prove its claim owns it.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RunBinding {
    pub request: String,
    pub owner: String,
    pub expires_at: u64,
}

/// One run to start: the container and its binding.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct WorkspaceRunLaunch {
    pub container: WorkspaceRunContainer,
    pub binding: RunBinding,
}

/// How one exec ended.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct ExecExit {
    pub exit_code: i32,
    /// The runner's `elitea-workspace-runner: <code>` stderr code, if any.
    pub failure_code: String,
}

/// Why a backend operation failed. Data-free.
#[derive(Clone, Copy, Debug, Eq, PartialEq, thiserror::Error)]
pub enum BackendError {
    #[error("the workspace run does not exist")]
    NotFound,
    #[error("a run of that name exists with another binding")]
    Conflict,
    #[error("the container backend is unavailable")]
    Unavailable,
    #[error("the container backend refused the run")]
    Refused,
    #[error("the exec timed out")]
    Timeout,
}

/// The container side of workspace runs.
#[async_trait]
pub trait WorkspaceRunBackend: Send + Sync + 'static {
    /// Create and start the run, idempotently: `false` when a run of that
    /// name with the same binding already exists; `Conflict` for another.
    async fn start(&self, launch: &WorkspaceRunLaunch) -> Result<bool, BackendError>;

    /// The binding of the run `name`; `None` when it does not exist.
    async fn binding(&self, name: &str) -> Result<Option<RunBinding>, BackendError>;

    /// Exec `argv` in the run, stdin and stdout streamed; stderr is read
    /// bounded for the runner's failure code only.
    async fn exec(
        &self,
        name: &str,
        argv: &[String],
        stdin: &mut (dyn AsyncRead + Unpin + Send),
        stdout: &mut (dyn AsyncWrite + Unpin + Send),
    ) -> Result<ExecExit, BackendError>;

    /// Remove the run and its workspace; `false` when it was gone.
    async fn destroy(&self, name: &str) -> Result<bool, BackendError>;

    /// Every workspace run this backend holds, with its binding.
    async fn runs(&self) -> Result<Vec<(String, RunBinding)>, BackendError>;
}

/// The runner's failure code in its stderr (`elitea-workspace-runner: <code>`),
/// when it is a stable code (no spaces, bounded).
#[must_use]
pub fn failure_code(stderr: &[u8]) -> String {
    let text = String::from_utf8_lossy(stderr);
    text.lines()
        .filter_map(|line| line.strip_prefix("elitea-workspace-runner: "))
        .map(str::trim)
        .find(|code| {
            !code.is_empty()
                && code.len() <= 64
                && code
                    .bytes()
                    .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'.' | b'_' | b'-'))
        })
        .map(str::to_owned)
        .unwrap_or_default()
}

/// The profile's image and ceilings.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct WorkspaceRunCeilings {
    /// The runner image, pinned by digest.
    pub image: String,
    pub max_workspace_bytes: u64,
    pub max_cpu_millis: u64,
    pub max_memory_bytes: u64,
    pub max_wall_seconds: u64,
    /// Runs alive at once.
    pub concurrency: usize,
}

fn now_millis() -> i64 {
    i64::try_from(
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map_or(0, |elapsed| elapsed.as_millis()),
    )
    .unwrap_or(i64::MAX)
}

fn backend_status(error: BackendError) -> Status {
    match error {
        BackendError::NotFound => Status::not_found("The workspace run does not exist."),
        BackendError::Conflict => {
            Status::permission_denied("The workspace run belongs to another claim.")
        }
        BackendError::Refused => Status::failed_precondition("The workspace run was refused."),
        BackendError::Timeout => Status::deadline_exceeded("The workspace run did not answer."),
        BackendError::Unavailable => Status::unavailable("The container backend is unavailable."),
    }
}

/// The workspace-run service of one profile.
pub struct WorkspaceRunSupervisor<R> {
    verifier: Arc<GrantVerifier<R>>,
    backend: Arc<dyn WorkspaceRunBackend>,
    ceilings: WorkspaceRunCeilings,
    live: Arc<Mutex<BTreeMap<String, RunBinding>>>,
    /// Tests only: the peer a plaintext test listener cannot authenticate.
    fixed_peer: Option<String>,
}

impl<R: Ed25519PublicKeyResolver + Send + Sync + 'static> WorkspaceRunSupervisor<R> {
    #[must_use]
    pub fn new(
        verifier: GrantVerifier<R>,
        backend: Arc<dyn WorkspaceRunBackend>,
        ceilings: WorkspaceRunCeilings,
    ) -> Self {
        Self {
            verifier: Arc::new(verifier),
            backend,
            ceilings,
            live: Arc::new(Mutex::new(BTreeMap::new())),
            fixed_peer: None,
        }
    }

    #[cfg(test)]
    #[must_use]
    pub(crate) fn with_fixed_peer(mut self, peer: &str) -> Self {
        self.fixed_peer = Some(peer.to_owned());
        self
    }

    fn peer<T>(&self, request: &Request<T>) -> Result<String, Status> {
        match &self.fixed_peer {
            Some(peer) => Ok(peer.clone()),
            None => super::peer_identity::authenticated_peer(request),
        }
    }

    fn authorize(
        &self,
        peer: &str,
        grant: Option<&crate::protocol::elitea::runtime::v1::SignedSandboxJobGrantV1>,
        spec: Option<&WorkspaceRunSpecV1>,
    ) -> Result<(AuthorizedWorkspaceRun, WorkspaceRunContainer), Status> {
        let (Some(grant), Some(spec)) = (grant, spec) else {
            return Err(Status::invalid_argument("A grant and a spec are required."));
        };
        let container = WorkspaceRunContainer::from_wire(spec, &self.ceilings.image)
            .map_err(|_| Status::invalid_argument("The workspace run spec is invalid."))?;
        if container.workspace_bytes > self.ceilings.max_workspace_bytes
            || container.cpu_millis > self.ceilings.max_cpu_millis
            || container.memory_bytes > self.ceilings.max_memory_bytes
            || container.wall_seconds > self.ceilings.max_wall_seconds
        {
            return Err(Status::invalid_argument(
                "The workspace run exceeds this profile's ceilings.",
            ));
        }
        let authorized = self
            .verifier
            .verify_workspace_run(grant, peer, spec, now_millis())
            .map_err(|_| {
                Status::permission_denied("The grant does not authorize this workspace run.")
            })?;
        Ok((authorized, container))
    }

    /// The run exists and carries exactly this grant's binding.
    async fn owned(&self, name: &str, authorized: &AuthorizedWorkspaceRun) -> Result<(), Status> {
        match self.backend.binding(name).await.map_err(backend_status)? {
            Some(binding)
                if binding.request == authorized.request && binding.owner == authorized.owner =>
            {
                Ok(())
            }
            Some(_) => Err(backend_status(BackendError::Conflict)),
            None => Err(backend_status(BackendError::NotFound)),
        }
    }

    /// Reload the live set from the backend, then remove every run past its
    /// wall (plus grace). Returns the runs removed.
    ///
    /// # Errors
    ///
    /// The backend could not be listed.
    pub async fn sweep(&self, now_unix_seconds: u64) -> Result<usize, BackendError> {
        let runs = self.backend.runs().await?;
        let mut removed = 0;
        let mut alive = BTreeMap::new();
        for (name, binding) in runs {
            if binding.expires_at <= now_unix_seconds {
                if self.backend.destroy(&name).await.is_ok() {
                    removed += 1;
                    tracing::warn!(
                        event = "workspace_run_swept",
                        run = name.as_str(),
                        "a workspace run outlived its wall and was removed"
                    );
                }
            } else {
                alive.insert(name, binding);
            }
        }
        if let Ok(mut live) = self.live.lock() {
            *live = alive;
        }
        Ok(removed)
    }

    /// Sweep every 30 seconds until `shutdown`.
    pub fn sweeper(self: &Arc<Self>) -> impl Future<Output = ()> + Send + 'static {
        let this = Arc::clone(self);
        async move {
            let mut interval = tokio::time::interval(SWEEP_INTERVAL);
            loop {
                interval.tick().await;
                let now = u64::try_from(now_millis() / 1_000).unwrap_or(0);
                if let Err(error) = this.sweep(now).await {
                    tracing::warn!(event = "workspace_run_sweep_failed", reason = %error);
                }
            }
        }
    }

    /// The gRPC service over this supervisor.
    #[must_use]
    pub fn into_service(self: Arc<Self>) -> WorkspaceRunServiceServer<SharedSupervisor<R>> {
        WorkspaceRunServiceServer::new(SharedSupervisor(self))
            .max_decoding_message_size(64 * 1024 + 4096)
            .max_encoding_message_size(64 * 1024 + 4096)
    }
}

/// The service handle tonic owns.
pub struct SharedSupervisor<R>(pub Arc<WorkspaceRunSupervisor<R>>);

type ExecStream = ReceiverStream<Result<WorkspaceRunExecOutputV1, Status>>;

#[tonic::async_trait]
impl<R: Ed25519PublicKeyResolver + Send + Sync + 'static> WorkspaceRunService
    for SharedSupervisor<R>
{
    async fn start_workspace_run(
        &self,
        request: Request<StartWorkspaceRunRequestV1>,
    ) -> Result<Response<StartWorkspaceRunResponseV1>, Status> {
        let this = &self.0;
        let peer = this.peer(&request)?;
        let request = request.into_inner();
        let (authorized, container) =
            this.authorize(&peer, request.grant.as_ref(), request.spec.as_ref())?;
        let now = u64::try_from(now_millis() / 1_000).unwrap_or(0);
        let binding = RunBinding {
            request: authorized.request.clone(),
            owner: authorized.owner.clone(),
            expires_at: now + container.wall_seconds + SWEEP_GRACE_SECONDS,
        };
        {
            let live = this
                .live
                .lock()
                .map_err(|_| Status::internal("The run table is unavailable."))?;
            if !live.contains_key(&container.name) && live.len() >= this.ceilings.concurrency {
                return Err(Status::resource_exhausted(
                    "The workspace-run profile is at its concurrency.",
                ));
            }
        }
        let name = container.name.clone();
        let created = this
            .backend
            .start(&WorkspaceRunLaunch {
                container,
                binding: binding.clone(),
            })
            .await
            .map_err(backend_status)?;
        if !created {
            // A retried start: the existing run must be this claim's.
            this.owned(&name, &authorized).await?;
        }
        if let Ok(mut live) = this.live.lock() {
            live.insert(name.clone(), binding);
        }
        tracing::info!(
            event = "workspace_run_started",
            run = name.as_str(),
            created
        );
        Ok(Response::new(StartWorkspaceRunResponseV1 { created }))
    }

    type ExecWorkspaceRunStream = ExecStream;

    #[allow(
        clippy::too_many_lines,
        reason = "one exec: open, authorize, pump stdin, stream stdout, report the exit"
    )]
    async fn exec_workspace_run(
        &self,
        request: Request<Streaming<WorkspaceRunExecInputV1>>,
    ) -> Result<Response<Self::ExecWorkspaceRunStream>, Status> {
        use workspace_run_exec_input_v1::Frame as In;
        use workspace_run_exec_output_v1::Frame as Out;
        let this = Arc::clone(&self.0);
        let peer = this.peer(&request)?;
        let mut inbound = request.into_inner();
        let Some(WorkspaceRunExecInputV1 {
            frame: Some(In::Open(open)),
        }) = inbound.message().await?
        else {
            return Err(Status::invalid_argument(
                "An exec starts with its open frame.",
            ));
        };
        let (authorized, container) =
            this.authorize(&peer, open.grant.as_ref(), open.spec.as_ref())?;
        let spec = open.spec.unwrap_or_default();
        let operation = WorkspaceRunOperationV1::try_from(open.operation)
            .map_err(|_| Status::invalid_argument("The operation is unknown."))?;
        let argv = runner_argv(
            operation,
            &spec,
            &open.limits.unwrap_or_default(),
            &open.policy_json,
        )
        .map_err(|_| Status::invalid_argument("The exec's limits or policy are invalid."))?;
        this.owned(&container.name, &authorized).await?;

        let (mut stdin_writer, mut stdin_reader) = tokio::io::duplex(PIPE_BYTES);
        let (stdout_writer, mut stdout_reader) = tokio::io::duplex(PIPE_BYTES);
        let (abort_tx, abort_rx) = tokio::sync::oneshot::channel::<()>();
        // The worker's stdin. A stream that ends without `stdin_end` aborts
        // the exec: the runner never sees a partial archive as complete.
        tokio::spawn(async move {
            loop {
                match inbound.message().await {
                    Ok(Some(WorkspaceRunExecInputV1 {
                        frame: Some(In::Stdin(chunk)),
                    })) if chunk.len() <= EXEC_CHUNK_BYTES => {
                        if stdin_writer.write_all(&chunk).await.is_err() {
                            return;
                        }
                    }
                    Ok(Some(WorkspaceRunExecInputV1 {
                        frame: Some(In::StdinEnd(true)),
                    })) => {
                        let _ = stdin_writer.shutdown().await;
                        // Keep reading so a closed stream is still noticed.
                        while let Ok(Some(_)) = inbound.message().await {}
                        return;
                    }
                    _ => {
                        let _ = abort_tx.send(());
                        return;
                    }
                }
            }
        });
        let (tx, rx) = mpsc::channel::<Result<WorkspaceRunExecOutputV1, Status>>(8);
        let backend = Arc::clone(&this.backend);
        let name = container.name;
        tokio::spawn(async move {
            let exec = async {
                let mut stdout_writer = stdout_writer;
                let result = backend
                    .exec(&name, &argv, &mut stdin_reader, &mut stdout_writer)
                    .await;
                drop(stdout_writer);
                result
            };
            let forward_tx = tx.clone();
            let forward = async move {
                let mut buffer = vec![0_u8; EXEC_CHUNK_BYTES];
                loop {
                    match stdout_reader.read(&mut buffer).await {
                        Ok(0) | Err(_) => return,
                        Ok(count) => {
                            let frame = WorkspaceRunExecOutputV1 {
                                frame: Some(Out::Stdout(buffer[..count].to_vec())),
                            };
                            if forward_tx.send(Ok(frame)).await.is_err() {
                                return;
                            }
                        }
                    }
                }
            };
            let outcome = tokio::select! {
                (result, ()) = async { tokio::join!(exec, forward) } => result,
                _ = abort_rx => {
                    tracing::warn!(event = "workspace_run_exec_aborted", run = name.as_str());
                    return;
                }
            };
            let frame = match outcome {
                Ok(exit) => Ok(WorkspaceRunExecOutputV1 {
                    frame: Some(Out::Exit(WorkspaceRunExecExitV1 {
                        exit_code: exit.exit_code,
                        failure_code: exit.failure_code,
                    })),
                }),
                Err(error) => Err(backend_status(error)),
            };
            let _ = tx.send(frame).await;
        });
        Ok(Response::new(ReceiverStream::new(rx)))
    }

    async fn destroy_workspace_run(
        &self,
        request: Request<DestroyWorkspaceRunRequestV1>,
    ) -> Result<Response<DestroyWorkspaceRunResponseV1>, Status> {
        let this = &self.0;
        let peer = this.peer(&request)?;
        let request = request.into_inner();
        let (authorized, container) =
            this.authorize(&peer, request.grant.as_ref(), request.spec.as_ref())?;
        let destroyed = match this.owned(&container.name, &authorized).await {
            Ok(()) => this
                .backend
                .destroy(&container.name)
                .await
                .map_err(backend_status)?,
            Err(status) if status.code() == tonic::Code::NotFound => false,
            Err(status) => return Err(status),
        };
        if let Ok(mut live) = this.live.lock() {
            live.remove(&container.name);
        }
        tracing::info!(
            event = "workspace_run_destroyed",
            run = container.name.as_str(),
            destroyed
        );
        Ok(Response::new(DestroyWorkspaceRunResponseV1 { destroyed }))
    }
}

/// An in-memory backend for tests: a run is a temporary directory; `unpack`
/// and `pack` move a workspace archive in and out of it, and `serve` answers
/// each request line with what the real runner's line protocol would carry
/// for `write_file`, `read_file` and an echo of anything else.
#[cfg(test)]
pub(crate) mod fake {
    use super::*;
    use elitea_agent_runtime::workspace_archive::{self, ArchiveLimits};
    use std::path::PathBuf;
    use tokio::io::AsyncBufReadExt as _;

    #[derive(Default)]
    pub(crate) struct FakeBackend {
        pub(crate) runs: Mutex<BTreeMap<String, (RunBinding, PathBuf, WorkspaceRunContainer)>>,
        pub(crate) argv_seen: Mutex<Vec<Vec<String>>>,
        pub(crate) env_seen: Mutex<Vec<String>>,
        pub(crate) destroyed: Mutex<Vec<String>>,
        pub(crate) root: Mutex<Option<tempfile::TempDir>>,
        /// Serve sessions that ended (stdin closed or the exec dropped).
        pub(crate) serves_ended: Arc<std::sync::atomic::AtomicUsize>,
    }

    struct CountOnDrop(Arc<std::sync::atomic::AtomicUsize>);

    impl Drop for CountOnDrop {
        fn drop(&mut self) {
            self.0.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
        }
    }

    impl FakeBackend {
        pub(crate) fn dir(&self, name: &str) -> Option<PathBuf> {
            self.runs
                .lock()
                .unwrap()
                .get(name)
                .map(|(_, dir, _)| dir.clone())
        }
    }

    const LIMITS: ArchiveLimits = ArchiveLimits {
        max_files: 1_000,
        max_file_bytes: 8 << 20,
        max_total_bytes: 64 << 20,
    };

    #[async_trait]
    impl WorkspaceRunBackend for FakeBackend {
        async fn start(&self, launch: &WorkspaceRunLaunch) -> Result<bool, BackendError> {
            let mut runs = self.runs.lock().unwrap();
            if let Some((binding, _, _)) = runs.get(&launch.container.name) {
                return if *binding == launch.binding
                    || (binding.request == launch.binding.request
                        && binding.owner == launch.binding.owner)
                {
                    Ok(false)
                } else {
                    Err(BackendError::Conflict)
                };
            }
            let mut root = self.root.lock().unwrap();
            let root = root.get_or_insert_with(|| tempfile::tempdir().unwrap());
            let dir = root.path().join(&launch.container.name);
            std::fs::create_dir_all(&dir).unwrap();
            runs.insert(
                launch.container.name.clone(),
                (launch.binding.clone(), dir, launch.container.clone()),
            );
            Ok(true)
        }

        async fn binding(&self, name: &str) -> Result<Option<RunBinding>, BackendError> {
            Ok(self
                .runs
                .lock()
                .unwrap()
                .get(name)
                .map(|(binding, _, _)| binding.clone()))
        }

        async fn exec(
            &self,
            name: &str,
            argv: &[String],
            stdin: &mut (dyn AsyncRead + Unpin + Send),
            stdout: &mut (dyn AsyncWrite + Unpin + Send),
        ) -> Result<ExecExit, BackendError> {
            self.argv_seen.lock().unwrap().push(argv.to_vec());
            let dir = self.dir(name).ok_or(BackendError::NotFound)?;
            match argv[1].as_str() {
                "unpack" => {
                    let mut archive = Vec::new();
                    stdin
                        .read_to_end(&mut archive)
                        .await
                        .map_err(|_| BackendError::Unavailable)?;
                    match workspace_archive::read_tree(&mut archive.as_slice(), &dir, LIMITS) {
                        Ok(_) => Ok(ExecExit::default()),
                        Err(error) => Ok(ExecExit {
                            exit_code: 1,
                            failure_code: error.code().to_owned(),
                        }),
                    }
                }
                "pack" => {
                    let mut archive = Vec::new();
                    workspace_archive::write_tree(&dir, &mut archive, LIMITS)
                        .map_err(|_| BackendError::Unavailable)?;
                    stdout
                        .write_all(&archive)
                        .await
                        .map_err(|_| BackendError::Unavailable)?;
                    Ok(ExecExit::default())
                }
                "serve" => {
                    let _ended = CountOnDrop(Arc::clone(&self.serves_ended));
                    let mut lines = tokio::io::BufReader::new(stdin).lines();
                    if let Ok(Some(env)) = lines.next_line().await {
                        self.env_seen.lock().unwrap().push(env);
                    }
                    while let Ok(Some(line)) = lines.next_line().await {
                        let request: serde_json::Value =
                            serde_json::from_str(&line).unwrap_or_default();
                        let arguments = &request["args"];
                        let answer = if let Some(classify) = request.get("classify") {
                            let command = classify["command"].as_str().unwrap_or_default();
                            serde_json::json!({"ask": command.contains("rm -rf") || command.contains("git push")})
                        } else if request["tool"] == "hang" {
                            // A command that never finishes: only the end
                            // of the session stops it.
                            let _ = lines.next_line().await;
                            break;
                        } else if request["tool"] == "run_command" {
                            let command = arguments["command"].as_str().unwrap_or_default();
                            let destructive =
                                command.contains("rm -rf") || command.contains("git push");
                            if destructive && request["approved"] != true {
                                serde_json::json!({"status": "error", "code": "approval_pending"})
                            } else {
                                serde_json::json!({"status": "ok", "ran": command})
                            }
                        } else {
                            match request["tool"].as_str() {
                                Some("write_file") => {
                                    let path = dir.join(arguments["path"].as_str().unwrap_or("x"));
                                    std::fs::create_dir_all(path.parent().unwrap()).unwrap();
                                    std::fs::write(
                                        path,
                                        arguments["content"].as_str().unwrap_or(""),
                                    )
                                    .unwrap();
                                    serde_json::json!({"status": "ok"})
                                }
                                Some("read_file") => {
                                    let path = dir.join(arguments["path"].as_str().unwrap_or("x"));
                                    serde_json::json!({"status": "ok", "content": std::fs::read_to_string(path).unwrap_or_default()})
                                }
                                _ => serde_json::json!({"status": "ok", "echo": request}),
                            }
                        };
                        let mut encoded = answer.to_string().into_bytes();
                        encoded.push(b'\n');
                        if stdout.write_all(&encoded).await.is_err() {
                            break;
                        }
                        let _ = stdout.flush().await;
                    }
                    Ok(ExecExit::default())
                }
                _ => Ok(ExecExit {
                    exit_code: 2,
                    failure_code: String::new(),
                }),
            }
        }

        async fn destroy(&self, name: &str) -> Result<bool, BackendError> {
            self.destroyed.lock().unwrap().push(name.to_owned());
            Ok(self.runs.lock().unwrap().remove(name).is_some())
        }

        async fn runs(&self) -> Result<Vec<(String, RunBinding)>, BackendError> {
            Ok(self
                .runs
                .lock()
                .unwrap()
                .iter()
                .map(|(name, (binding, _, _))| (name.clone(), binding.clone()))
                .collect())
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_failure_code_is_the_runners_stable_code_only() {
        assert_eq!(
            failure_code(b"noise\nelitea-workspace-runner: runner.workspace_too_large\n"),
            "runner.workspace_too_large"
        );
        assert_eq!(
            failure_code(b"elitea-workspace-runner: has spaces in it"),
            ""
        );
        assert_eq!(failure_code(b""), "");
    }
}
