/**
 * The wire shapes of the desktop host's background processes (#1223; IPC.md
 * "Background processes"): `ProcessInfo`, the `process://event` channel's
 * events, and the three kinds a turn's own `agent://event` stream carries for
 * its `run_command` card. Snake_case, exactly as the host serialises them.
 */

export type ProcessStatus = 'running' | 'exited' | 'killed' | 'timed_out';

/** `stdout`: a piped process (stderr shares the pipe); `pty`: a terminal process, escape codes included. */
type ProcessStream = 'stdout' | 'pty';

export interface ProcessInfo {
  /** `p_` + 8 hex digits. */
  process_id: string;
  /** The `run_command` call that started it. */
  call_id: string;
  command: string;
  /** Workspace-relative (`.` for the root). */
  cwd: string;
  /** On a pseudo-terminal. */
  tty: boolean;
  sandbox: 'read-only' | 'workspace-write' | 'full-access';
  network: boolean;
  loopback: boolean;
  /** `session`: the person approved keeping it past its turn. */
  keep_alive: 'turn' | 'session';
  pinned: boolean;
  status: ProcessStatus;
  exit_code?: number;
  /** For `killed`: `SIGTERM`, `SIGKILL`, … */
  signal?: string;
  /** Unix ms. */
  started_at: number;
  ended_at?: number;
  /** Output so far: the next byte's offset. */
  bytes: number;
}

/** `process_read`: the output from `since`; `screen` for a terminal process. */
export interface ProcessOutput {
  data: string;
  next_offset: number;
  /** What the buffer no longer holds between `since` and `data`. */
  dropped_bytes: number;
  screen?: string;
}

/** `process_output`: at most 20 a second per process, at most 16 KiB each; `offset` is `data`'s first byte. */
export interface ProcessOutputPayload {
  process_id: string;
  stream: ProcessStream;
  data: string;
  offset: number;
}

interface ProcessEventBase {
  workspace_id: string;
  seq: number;
}

/** One `process://event`. */
export type ProcessEvent =
  | (ProcessEventBase & { kind: 'process_started' | 'process_updated' | 'process_exited'; payload: ProcessInfo })
  | (ProcessEventBase & { kind: 'process_output'; payload: ProcessOutputPayload });

/** The kinds a turn's `agent://event` stream carries while the turn that started the process runs. */
export type ProcessTurnEvent =
  | { kind: 'process_started'; payload: Pick<ProcessInfo, 'process_id' | 'call_id' | 'command' | 'tty' | 'sandbox' | 'loopback'> }
  | { kind: 'tool_output_delta'; payload: { call_id: string; process_id: string; stream: ProcessStream; data: string; offset: number } }
  | { kind: 'process_exited'; payload: { process_id: string; call_id: string; status: Exclude<ProcessStatus, 'running'>; exit_code?: number } };

// CSI (ESC [ … final), OSC (ESC ] … BEL or ST), and the two-byte ESC forms.
// eslint-disable-next-line no-control-regex -- escape codes are control characters by definition
const ANSI = /\u001b\[[0-?]*[ -/]*[@-~]|\u001b\][^\u0007\u001b]*(?:\u0007|\u001b\\)|\u001b[@-Z\\-_]/g;

/**
 * A terminal stream as plain text: escape codes dropped, `\r\n` as `\n`, and a
 * bare `\r` (a progress bar redrawing its line) keeping only what came after it.
 */
export function plainText(data: string): string {
  return data
    .replace(ANSI, '')
    .replace(/\r\n/g, '\n')
    .split('\n')
    .map((line) => line.slice(line.lastIndexOf('\r') + 1))
    .join('\n');
}
