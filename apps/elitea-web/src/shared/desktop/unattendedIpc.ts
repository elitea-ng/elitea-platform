/**
 * Unattended runs on the desktop host (ADR-0045, IPC "unattended turns"): the
 * grant that `agent_turn_start.unattended` carries, and the two
 * `agent://event` kinds of a chain. Kept beside `workspaceIpc.ts`. The wire
 * shapes are the host's (snake_case).
 */

/** The chain limits of a grant. An absent field takes the platform default (20 continuations, the per-run cap x10, 12 h). */
export interface UnattendedChain {
  /** A whole number from 0; the platform sets the maximum. */
  max_continuations?: number;
  /** US dollars, above 0, at most 4 decimals. */
  max_spend?: number;
  /** At least 1 second; the platform sets the maximum. */
  deadline_seconds?: number;
}

/** `agent_turn_start.unattended`: what the grant answers for the person. It does not bypass the local rules: only `bypass_approvals` does. */
export interface UnattendedGrant {
  /** The chain's grant id; the host makes one when it is absent and keeps it for its own continuations. */
  grant_id?: string;
  /** Answers "Continue?" at the step limit, then continues the chain. */
  continue_step_limit: boolean;
  /** Continues the chain after the 512 tool-call cap. */
  continue_tool_call_cap: boolean;
  /** Answers a PreToolUse hook's ask with "allow". */
  hook_asks: boolean;
  /** Answers a remote toolkit confirmation with "approve". */
  remote_confirmations: boolean;
  chain?: UnattendedChain;
}

/** How an unattended chain ended. `waiting`: it stopped at a cap the grant does not cover; the person can click Continue. */
type ChainOutcome = 'completed' | 'failed' | 'waiting' | 'limit_reached' | 'cancelled';

/** The chain limit that ended a `limit_reached` chain. */
type ChainLimit = 'continuations' | 'spend' | 'deadline';

interface TurnContinuedPayload {
  next_turn_id: string;
  execution_id: string;
  continuation_index: number;
  chain_root_execution_id: string;
}

export interface ChainEndedPayload {
  outcome: ChainOutcome;
  limit?: ChainLimit;
  message?: string;
}

/** The chain's events, as `agent://event` carries them (without the turn id and seq). */
export type UnattendedEvent =
  /** On the finished turn, after its `done` and its commit: the host started `next_turn_id` by itself. */
  | { kind: 'turn_continued'; payload: TurnContinuedPayload }
  /** On the last turn of the chain, after its `done`. */
  | { kind: 'chain_ended'; payload: ChainEndedPayload };

/** The fields a `status` `running` of an unattended turn adds. */
export interface UnattendedStatusFields {
  /** The host started this turn by itself (its prompt is `host_prompt`, never the person's message). */
  continued_automatically?: boolean;
  /** 1, 2, … for a turn the host started by itself. */
  continuation_index?: number;
  /** On the first `running` of any unattended turn: the grant it runs under. */
  unattended?: Omit<UnattendedGrant, 'chain'> & { grant_id: string };
}
