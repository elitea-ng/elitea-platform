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
    };
