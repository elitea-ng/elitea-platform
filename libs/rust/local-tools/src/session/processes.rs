//! The background-process tools (#1223): `run_command`'s `background`,
//! `tty`, `loopback`, `yield_ms` and `keep_alive`, and `process_output`,
//! `process_write`, `process_wait`, `process_kill`, `process_list` over the
//! session's [`ProcessRegistry`](crate::process::ProcessRegistry).
//!
//! Local-only: the cloud executor keeps no process past a call, so its
//! mirrored list (`elitea_agent_runtime::workspace_tools`) has the plain
//! `run_command` and none of these (the workspace runner's contract test
//! names the exclusion).

use std::sync::Arc;
use std::time::Duration;

use serde::Deserialize;
use serde_json::{Value, json};

use super::{LocalSession, ToolSpec, parse, path_param};
use crate::approvals::{ToolCall, ToolKind};
use crate::budget;
use crate::error::{ToolError, ToolResult};
use crate::process::{KeepAlive, Process, ProcessEvents, ProcessInfo, SpawnSpec, strip_escapes};
use crate::shell::{self, CommandSpec, PreparedCommand};

/// How long a background start waits for first output by default.
pub(super) const DEFAULT_YIELD_MS: u64 = 1000;

/// The longest `yield_ms`.
pub(super) const MAX_YIELD_MS: u64 = 30_000;

/// How long `process_write` waits for the answer by default.
const DEFAULT_WRITE_YIELD_MS: u64 = 500;

/// The most text one `process_write` sends.
const MAX_INPUT_BYTES: usize = 64 * 1024;

/// The most `max_bytes` asks for.
const MAX_READ_BYTES: usize = 64 * 1024;

/// `process_wait`'s default and longest wait.
const DEFAULT_WAIT_SECS: u64 = 30;
const MAX_WAIT_SECS: u64 = 600;

const RUN_COMMAND_DESCRIPTION: &str = "Run a command in the workspace under the OS sandbox. Plain commands run without a shell; pipes, redirects and && run under /bin/sh. Routine commands run without a question; destructive ones (recursive deletes, history rewrites, pushes, deploys, privilege), network and full-access are confirmed by the person. With background (or tty) the command keeps running: you get a process_id at once, with the output of its first yield_ms (default 1000); read more with process_output, type into it with process_write, stop it with process_kill. tty runs it on a terminal, for interactive programs (a REPL, a prompt, a watcher); process_output with screen shows what the terminal shows. A background process is stopped when this turn ends unless keep_alive is \"session\" (confirmed by the person). loopback lets it listen on and connect to 127.0.0.1 only, with the network still off (confirmed): start a server with it, then check it with another loopback command.";

fn run_command_parameters() -> Value {
    json!({
        "type": "object",
        "properties": {
            "command": { "type": "string" },
            "cwd": path_param("Working directory relative to the workspace root."),
            "timeout_seconds": { "type": "integer", "minimum": 1, "maximum": 600 },
            "sandbox": { "type": "string", "enum": ["read-only", "workspace-write", "full-access"] },
            "network": { "type": "boolean" },
            "background": { "type": "boolean", "description": "Start and return a process_id at once; read output with process_output." },
            "tty": { "type": "boolean", "description": "Run on a pseudo-terminal (interactive programs, colour). Implies background." },
            "loopback": { "type": "boolean", "description": "Allow connecting to and listening on 127.0.0.1/::1 only. Asked." },
            "yield_ms": { "type": "integer", "minimum": 0, "maximum": MAX_YIELD_MS, "description": "Background only: wait this long and return the first output." },
            "keep_alive": { "type": "string", "enum": ["turn", "session"], "description": "Background only: \"session\" keeps the process running after this turn ends (asked); default \"turn\"." }
        },
        "required": ["command"]
    })
}

/// `run_command` with the background-process switches: what a session with
/// [`crate::shell::ShellConfig::background_processes`] offers in place of
/// [`super::TOOLS`]' own (same name and kind, a wider schema).
pub static RUN_COMMAND_PROCESSES: ToolSpec = ToolSpec {
    name: "run_command",
    kind: ToolKind::RunCommand,
    description: RUN_COMMAND_DESCRIPTION,
    parameters: run_command_parameters,
};

/// The process tools, offered after [`super::TOOLS`] with the shell.
pub static PROCESS_TOOLS: &[ToolSpec] = &[
    ToolSpec {
        name: "process_output",
        kind: ToolKind::ProcessRead,
        description: "Read a background process's output: the bytes after since (the next_offset of your previous read; 0 for the start), with its status. dropped_bytes says how much the 1 MiB buffer no longer holds. For a tty process, screen=true returns the rendered terminal screen instead of the stream.",
        parameters: || {
            json!({
                "type": "object",
                "properties": {
                    "process_id": { "type": "string" },
                    "since": { "type": "integer", "minimum": 0, "description": "Byte offset from the previous call's next_offset." },
                    "max_bytes": { "type": "integer", "minimum": 1, "maximum": MAX_READ_BYTES },
                    "screen": { "type": "boolean", "description": "TTY only: return the rendered screen instead of the stream." }
                },
                "required": ["process_id"]
            })
        },
    },
    ToolSpec {
        name: "process_write",
        kind: ToolKind::ProcessWrite,
        description: "Type text into a running background process (a tty's input, or stdin), then return what it printed within yield_ms (default 500; a tty also returns its screen). Lines that look like commands are classified like run_command: a destructive one is confirmed by the person. The process keeps the sandbox it started with.",
        parameters: || {
            json!({
                "type": "object",
                "properties": {
                    "process_id": { "type": "string" },
                    "input": { "type": "string", "description": "Text to write; use \\n for Enter, \\^C for Ctrl-C." },
                    "yield_ms": { "type": "integer", "minimum": 0, "maximum": MAX_YIELD_MS }
                },
                "required": ["process_id", "input"]
            })
        },
    },
    ToolSpec {
        name: "process_wait",
        kind: ToolKind::ProcessRead,
        description: "Wait until a background process exits, at most timeout_seconds (default 30), and return its status and the end of its output.",
        parameters: || {
            json!({
                "type": "object",
                "properties": {
                    "process_id": { "type": "string" },
                    "timeout_seconds": { "type": "integer", "minimum": 1, "maximum": MAX_WAIT_SECS }
                },
                "required": ["process_id"]
            })
        },
    },
    ToolSpec {
        name: "process_kill",
        kind: ToolKind::ProcessKill,
        description: "Stop a background process and everything it started (SIGTERM, then SIGKILL after 2 s).",
        parameters: || {
            json!({
                "type": "object",
                "properties": { "process_id": { "type": "string" } },
                "required": ["process_id"]
            })
        },
    },
    ToolSpec {
        name: "process_list",
        kind: ToolKind::ProcessRead,
        description: "List this session's background processes: id, command, status, whether on a terminal, and how much output each has.",
        parameters: || json!({ "type": "object", "properties": {} }),
    },
];

/// How a background start goes.
pub(super) struct Start {
    pub tty: bool,
    pub keep_alive: KeepAlive,
    /// It was approved as unconfined.
    pub unconfined: bool,
    pub yield_for: Duration,
}

#[derive(Deserialize)]
struct OutputArgs {
    process_id: String,
    #[serde(default)]
    since: u64,
    #[serde(default)]
    max_bytes: Option<usize>,
    #[serde(default)]
    screen: bool,
}

#[derive(Deserialize)]
struct WriteArgs {
    process_id: String,
    input: String,
    #[serde(default)]
    yield_ms: Option<u64>,
}

#[derive(Deserialize)]
struct WaitArgs {
    process_id: String,
    #[serde(default)]
    timeout_seconds: Option<u64>,
}

#[derive(Deserialize)]
struct IdArgs {
    process_id: String,
}

/// `\^X` (`X` a letter or one of `@[\]^_?`) as its control character
/// (`\^C` is Ctrl-C, `\^?` is DEL); everything else as written.
fn decode_input(text: &str) -> Vec<u8> {
    let bytes = text.as_bytes();
    let mut out = Vec::with_capacity(bytes.len());
    let mut at = 0;
    while at < bytes.len() {
        if bytes[at] == b'\\'
            && bytes.get(at + 1) == Some(&b'^')
            && let Some(&key) = bytes.get(at + 2)
        {
            let key = key.to_ascii_uppercase();
            if key == b'?' {
                out.push(0x7f);
                at += 3;
                continue;
            }
            if (b'@'..=b'_').contains(&key) {
                out.push(key & 0x1f);
                at += 3;
                continue;
            }
        }
        out.push(bytes[at]);
        at += 1;
    }
    out
}

/// The bytes `chunk` holds that end on a whole character when more follow
/// (the next read starts at the cut).
fn whole_characters(data: &[u8], more_follows: bool) -> usize {
    if !more_follows {
        return data.len();
    }
    match std::str::from_utf8(data) {
        Err(error) if error.error_len().is_none() && data.len() - error.valid_up_to() < 4 => {
            error.valid_up_to()
        }
        _ => data.len(),
    }
}

fn status_fields(info: &ProcessInfo, answer: &mut serde_json::Map<String, Value>) {
    answer.insert("process_id".to_owned(), json!(info.process_id));
    answer.insert("status".to_owned(), json!(info.status));
    if let Some(code) = info.exit_code {
        answer.insert("exit_code".to_owned(), json!(code));
    }
    if let Some(signal) = &info.signal {
        answer.insert("signal".to_owned(), json!(signal));
    }
}

/// A process's output from `since` within `max` bytes, as the model reads
/// it: a terminal's stream without its escape codes, and with `screen`
/// its rendered screen.
fn output_answer(process: &Process, since: u64, max: usize, screen: bool) -> Value {
    let info = process.info();
    let chunk = process.read(since, max);
    let more = chunk.next < info.bytes;
    let keep = whole_characters(&chunk.data, more);
    let text = String::from_utf8_lossy(&chunk.data[..keep]);
    let output = if process.is_tty() {
        strip_escapes(&text)
    } else {
        text.into_owned()
    };
    let mut answer = serde_json::Map::new();
    status_fields(&info, &mut answer);
    answer.insert("output".to_owned(), json!(output));
    answer.insert("next_offset".to_owned(), json!(chunk.start + keep as u64));
    answer.insert("dropped_bytes".to_owned(), json!(chunk.dropped));
    answer.insert(
        "more".to_owned(),
        json!(chunk.start + (keep as u64) < info.bytes),
    );
    if screen {
        match process.screen() {
            Some(rendered) => {
                answer.insert("screen".to_owned(), json!(rendered));
            }
            None => {
                answer.insert(
                    "note".to_owned(),
                    json!("not a tty process: it has no screen"),
                );
            }
        }
    }
    Value::Object(answer)
}

impl LocalSession {
    /// The budget a process read stays within (`run_command`'s), at most
    /// `asked` bytes.
    fn process_read_cap(&self, asked: Option<usize>) -> ToolResult<usize> {
        if asked.is_some_and(|bytes| !(1..=MAX_READ_BYTES).contains(&bytes)) {
            return Err(ToolError::invalid("`max_bytes` is 1 to 65536"));
        }
        let budget = budget::bytes(self.budgets.run_command);
        Ok(asked.map_or(budget, |bytes| bytes.min(budget)))
    }

    /// Spawn an approved background command and wait `yield_for` for its
    /// first output.
    pub(super) async fn start_process(
        self: &Arc<Self>,
        call_id: &str,
        spec: &CommandSpec,
        start: Start,
    ) -> ToolResult<Value> {
        let PreparedCommand { prepared, .. } =
            shell::prepare_command(&self.workspace, &self.shell, spec, start.unconfined)?;
        let enforcement = prepared.enforcement;
        let note = prepared.note;
        let process = self.processes.spawn(SpawnSpec {
            call_id: call_id.to_owned(),
            command: spec.command.clone(),
            cwd: spec.cwd.display_string(),
            cwd_path: self.workspace.absolute(&spec.cwd),
            program: prepared.program,
            args: prepared.args,
            env: shell::process_environment(&self.shell, start.tty),
            tty: start.tty,
            sandbox: spec.mode,
            network: spec.network,
            loopback: spec.loopback,
            keep_alive: start.keep_alive,
            max_wall: self.shell.max_process_wall,
        })?;
        process.settle(start.yield_for).await;
        let max = self.process_read_cap(None)?;
        let mut answer = output_answer(&process, 0, max, start.tty);
        if let Value::Object(map) = &mut answer {
            map.insert("tty".to_owned(), json!(start.tty));
            map.insert("loopback".to_owned(), json!(spec.loopback));
            map.insert("keep_alive".to_owned(), json!(start.keep_alive));
            map.insert("sandbox".to_owned(), json!(spec.mode));
            map.insert("enforcement".to_owned(), json!(enforcement));
            if let Some(note) = note {
                map.insert("enforcement_note".to_owned(), json!(note));
            }
        }
        Ok(answer)
    }

    /// One of the `process_*` tools.
    pub(super) async fn process_tool(
        self: &Arc<Self>,
        tool: &str,
        call_id: &str,
        args: Value,
    ) -> ToolResult<Value> {
        match tool {
            "process_output" => {
                let args: OutputArgs = parse(args)?;
                let max = self.process_read_cap(args.max_bytes)?;
                let process = self.processes.get(&args.process_id)?;
                self.authorize_process(call_id, ToolKind::ProcessRead, &process, None, tool)
                    .await?;
                Ok(output_answer(&process, args.since, max, args.screen))
            }
            "process_write" => {
                let args: WriteArgs = parse(args)?;
                if args.yield_ms.is_some_and(|ms| ms > MAX_YIELD_MS) {
                    return Err(ToolError::invalid("`yield_ms` is 0 to 30000"));
                }
                let input = decode_input(&args.input);
                if input.is_empty() || input.len() > MAX_INPUT_BYTES {
                    return Err(ToolError::invalid("`input` is 1 byte to 64 KiB"));
                }
                let process = self.processes.get(&args.process_id)?;
                let typed = String::from_utf8_lossy(&input).into_owned();
                self.authorize_process(
                    call_id,
                    ToolKind::ProcessWrite,
                    &process,
                    Some(typed),
                    "type into a process",
                )
                .await?;
                let before = process.end();
                process.write(&input)?;
                process
                    .settle(Duration::from_millis(
                        args.yield_ms.unwrap_or(DEFAULT_WRITE_YIELD_MS),
                    ))
                    .await;
                let max = self.process_read_cap(None)?;
                Ok(output_answer(&process, before, max, process.is_tty()))
            }
            "process_wait" => {
                let args: WaitArgs = parse(args)?;
                let secs = args.timeout_seconds.unwrap_or(DEFAULT_WAIT_SECS);
                if !(1..=MAX_WAIT_SECS).contains(&secs) {
                    return Err(ToolError::invalid("`timeout_seconds` is 1 to 600"));
                }
                let process = self.processes.get(&args.process_id)?;
                self.authorize_process(call_id, ToolKind::ProcessRead, &process, None, tool)
                    .await?;
                let ended = process.wait_exit(Duration::from_secs(secs)).await;
                let max = self.process_read_cap(None)?;
                let since = process.end().saturating_sub(max as u64);
                let mut answer = output_answer(&process, since, max, process.is_tty());
                answer["waited_out"] = json!(!ended);
                Ok(answer)
            }
            "process_kill" => {
                let args: IdArgs = parse(args)?;
                let process = self.processes.get(&args.process_id)?;
                self.authorize_process(call_id, ToolKind::ProcessKill, &process, None, tool)
                    .await?;
                process.stop().await;
                let mut answer = serde_json::Map::new();
                status_fields(&process.info(), &mut answer);
                Ok(Value::Object(answer))
            }
            _ => {
                self.authorize(call_id, ToolCall::new(ToolKind::ProcessRead), tool)
                    .await?;
                let processes: Vec<ProcessInfo> = self
                    .processes
                    .list()
                    .iter()
                    .map(|process| process.info())
                    .collect();
                Ok(json!({ "processes": processes }))
            }
        }
    }

    async fn authorize_process(
        &self,
        call_id: &str,
        kind: ToolKind,
        process: &Process,
        typed: Option<String>,
        message: &str,
    ) -> ToolResult<()> {
        let call = ToolCall {
            paths: vec![process.cwd().to_owned()],
            command: typed,
            sandbox: Some(process.sandbox()),
            ..ToolCall::new(kind)
        };
        self.authorize(call_id, call, message).await
    }

    /// Where the session's process events go (the host's pane and the
    /// turn's tool card); `None`: nowhere.
    pub fn set_process_events(&self, events: Option<Arc<dyn ProcessEvents>>) {
        self.processes.set_events(events);
    }

    /// The session's background processes, oldest first.
    #[must_use]
    pub fn processes(&self) -> Vec<ProcessInfo> {
        self.processes
            .list()
            .iter()
            .map(|process| process.info())
            .collect()
    }

    /// One background process (for the host's pane: its output, screen,
    /// input and size).
    ///
    /// # Errors
    ///
    /// [`ErrorCode::NotFound`](crate::error::ErrorCode::NotFound) for an id the session does not hold.
    pub fn process(&self, id: &str) -> ToolResult<Arc<Process>> {
        self.processes.get(id)
    }

    /// Stop one process (the person's Stop).
    ///
    /// # Errors
    ///
    /// [`ErrorCode::NotFound`](crate::error::ErrorCode::NotFound).
    pub async fn kill_process(&self, id: &str) -> ToolResult<()> {
        self.processes.get(id)?.stop().await;
        Ok(())
    }

    /// Pin or unpin one (the person's Pin): a pinned process outlives its
    /// turn's end. `turn_running`: a turn of the session runs now (so an
    /// unpinned process of it waits for that turn's end).
    ///
    /// # Errors
    ///
    /// [`ErrorCode::NotFound`](crate::error::ErrorCode::NotFound).
    pub async fn pin_process(&self, id: &str, pinned: bool, turn_running: bool) -> ToolResult<()> {
        self.processes.pin(id, pinned, turn_running).await
    }

    /// The turn ended: stop its processes that are neither pinned nor kept.
    pub async fn end_turn_processes(&self) {
        self.processes.end_turn().await;
    }

    /// Stop every process (a handoff starts, the person signs out, the
    /// workspace is removed), the local MCP servers included (#1230; a
    /// later turn starts them again if the policy still allows them).
    pub async fn stop_processes(&self) {
        self.processes.stop_all().await;
        self.mcp.stop_all().await;
    }

    /// [`Self::stop_processes`] from synchronous code (the app quits),
    /// waiting at most `grace` before `SIGKILL`.
    pub fn stop_processes_blocking(&self, grace: Duration) {
        self.processes.stop_all_blocking(grace);
        self.mcp.stop_all_blocking(grace);
    }
}

#[cfg(test)]
mod tests {
    use super::{decode_input, whole_characters};

    #[test]
    fn control_keys_are_decoded_and_text_is_not() {
        assert_eq!(decode_input("1+1\n"), b"1+1\n");
        assert_eq!(decode_input("\\^C"), [3]);
        assert_eq!(decode_input("a\\^d\\^?"), [b'a', 4, 0x7f]);
        assert_eq!(decode_input("\\^"), b"\\^");
        assert_eq!(decode_input("\\n"), b"\\n");
        let euro = "€".as_bytes();
        assert_eq!(whole_characters(&euro[..2], true), 0);
        assert_eq!(whole_characters(&euro[..2], false), 2);
    }
}
