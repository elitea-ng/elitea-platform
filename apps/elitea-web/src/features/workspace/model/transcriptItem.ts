import type { ProcessStatus } from '@/shared/desktop/processTypes';

/** A background process a `run_command` call started (#1223): its id, how it ended, and the last lines of its output. */
export interface ToolProcess {
  processId: string;
  status: ProcessStatus;
  exitCode?: number;
  /** The last 200 lines, escape codes dropped. */
  output: string;
  /** Where the next output byte is expected: an earlier delta (a replay out of order) is not appended twice. */
  end: number;
}

/** One row of a turn's transcript: the agent's text, a step's reasoning or a tool call with its result. */
export type TranscriptItem =
  | { type: 'text'; key: string; text: string }
  /** A model step's reasoning (`thinking_delta`); `chars` once its `thinking_end` came. */
  | { type: 'thinking'; key: string; text: string; chars?: number }
  | {
      type: 'tool';
      key: string;
      callId: string;
      tool: string;
      argsSummary: string;
      remote: boolean;
      /** `thumbnail`: a computer screenshot's `data:image/jpeg` URL (ADR-0047 decision 9). */
      /** `code`: a failed call's error code (`sandbox_unavailable`), when the host sent one. */
      result?: { ok: boolean; summary: string; truncated: boolean; thumbnail?: string; code?: string };
      /** A page the step produced (a pushed branch, a pull request): an `https:` link the row shows. */
      link?: { href: string; label: string };
      process?: ToolProcess;
    }
  /** A message the person sent while the turn ran (`steer_injected`): the agent got it at this point of the turn. */
  | { type: 'steer'; key: string; steerId: string; text: string };

/** A tool row's result; `thumbnail` kept only when it is a JPEG data URL (a computer screenshot, ADR-0047 decision 9), never a page address. */
export function toolResult(ok: boolean, summary: string, truncated: boolean, thumbnail?: unknown): NonNullable<Extract<TranscriptItem, { type: 'tool' }>['result']> {
  const shown = typeof thumbnail === 'string' && thumbnail.startsWith('data:image/jpeg;base64,') && thumbnail.length <= 16 * 1024;
  return shown ? { ok, summary, truncated, thumbnail } : { ok, summary, truncated };
}

/** The result with a failed call's error code, when the host sent one. */
export function withCode<R extends { ok: boolean }>(result: R, code: unknown): R & { code?: string } {
  return !result.ok && typeof code === 'string' && code !== '' ? { ...result, code } : result;
}
