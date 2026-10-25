/**
 * The composer's context meter as the desktop host reports it (IPC.md
 * "Context window"): on `usage` and `context` agent events.
 */
/** A model call's request occupancy, as the host measured it before sending (IPC.md "Context window"): estimates, never content. */
export interface ContextMeter {
  used_tokens: number;
  /** The window the budget uses: the model's, at most 272k. */
  window_tokens: number;
  /** Used of the window, 0 to 100 (absent from hosts before #1227). */
  percent?: number;
  phase: 'measured' | 'compacting' | 'compacted';
  /** The turn compacted the context. */
  compacted: boolean;
  /** When it last did (RFC 3339). */
  compacted_at?: string;
  /** The catalogue did not name the model's window: the host's 128k default. */
  window_fallback: boolean;
  /** The estimate at which the host compacts: the window past it is the compaction's buffer (absent from older hosts). */
  compact_at_tokens?: number;
  /** What fills the request (absent from older hosts). The parts add up to `used_tokens`. */
  parts?: ContextParts;
  /** When the host measured the request (RFC 3339). */
  measured_at?: string;
}

/** What fills a measured request, in estimated tokens (IPC.md "Context window"). */
interface ContextParts {
  /** The system message: the agent's instructions, the environment and AGENTS.md. */
  system_tokens: number;
  /** The host's and the agent's tool declarations. */
  tool_tokens: number;
  /** The person's local MCP tools' declarations. */
  mcp_tool_tokens: number;
  /** The skill catalogue and the skills picked for the turn. */
  skill_tokens: number;
  /** The conversation: everything else. */
  message_tokens: number;
}
