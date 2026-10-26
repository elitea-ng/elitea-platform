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
      result?: { ok: boolean; summary: string; truncated: boolean };
      process?: ToolProcess;
    };
