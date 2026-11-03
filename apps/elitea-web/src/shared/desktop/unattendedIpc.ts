/**
 * Unattended runs on the desktop host (ADR-0045, IPC "unattended turns"): the
 * grant that `agent_turn_start.unattended` carries, and the
 * `agent://event` kinds of a chain (with the run brief and the goal check of
 * ADR-0045 decision 11). Kept beside `workspaceIpc.ts`. The wire shapes are
 * the host's (snake_case); the brief and the check are Main's (contract #1564).
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
  /** ADR-0045 decision 11: a run brief and a goal check before the chain ends. Absent = true. */
  goal_check?: boolean;
  chain?: UnattendedChain;
}

/** How an unattended chain ended. `waiting`: it stopped at a cap the grant does not cover; the person can click Continue. */
type ChainOutcome = 'completed' | 'failed' | 'waiting' | 'limit_reached' | 'cancelled';

/** The chain limit that ended a `limit_reached` chain. */
type ChainLimit = 'continuations' | 'spend' | 'deadline';

/** Why the host continued the chain: the step limit, the 512 tool-call cap, or a goal check that found the goal not met. */
export type ContinuationKind = 'step_limit' | 'tool_call_cap' | 'goal_not_met';

interface TurnContinuedPayload {
  next_turn_id: string;
  execution_id: string;
  continuation_index: number;
  chain_root_execution_id: string;
  kind?: ContinuationKind;
}

/** The goal check's outcome (ADR-0045 decision 11). */
export type GoalCheckOutcome = 'met' | 'not_met' | 'blocked' | 'not_checked';

/** The goal check in short, on `chain_ended`. */
export interface ChainGoal {
  outcome: GoalCheckOutcome;
  met: number;
  total: number;
  reason?: string;
}

export interface ChainEndedPayload {
  outcome: ChainOutcome;
  limit?: ChainLimit;
  message?: string;
  /** The last goal check of the chain, when it had one. */
  goal?: ChainGoal;
}

/** One "done when" condition of a run brief: ids `c1` to `c5`. */
export interface RunBriefCondition {
  id: string;
  text: string;
}

/** A run brief (ADR-0045 decision 11): a small model wrote it from the person's request. Plain text only. */
export interface RunBrief {
  goal: string;
  done_when: RunBriefCondition[];
  constraints: string[];
  assumptions: string[];
  ambiguity: 'low' | 'medium' | 'high';
}

/** `none`: no grant or no goal check; `off`: an administrator turned briefs off. */
export type RunBriefState = 'none' | 'off' | 'pending' | 'ready' | 'failed';

interface RunBriefPayload {
  turn_id: string;
  chain_root_execution_id: string;
  state: RunBriefState;
  brief?: RunBrief;
}

/** One condition as the goal check judged it. */
export interface GoalCheckCondition {
  id: string;
  text: string;
  verdict: 'met' | 'not_met' | 'impossible';
  reason: string;
}

/** A goal check's result (Main's section-3 object without `continue_text`). */
export interface GoalCheckResult {
  outcome: GoalCheckOutcome;
  conditions: GoalCheckCondition[];
  met: number;
  total: number;
  /** The first failing condition's reason (`not_met`, `blocked`). */
  reason?: string;
}

/** The chain's events, as `agent://event` carries them (without the turn id and seq). */
export type UnattendedEvent =
  /** On the finished turn, after its `done` and its commit: the host started `next_turn_id` by itself. */
  | { kind: 'turn_continued'; payload: TurnContinuedPayload }
  /** On the last turn of the chain, after its `done`. */
  | { kind: 'chain_ended'; payload: ChainEndedPayload }
  /** On the chain's first turn, when Main's brief call returns (it never blocks the turn). */
  | { kind: 'run_brief'; payload: RunBriefPayload }
  /** On the checked turn, before its `turn_continued` (`not_met`) or `chain_ended` (the other outcomes). */
  | { kind: 'goal_checked'; payload: GoalCheckResult & { turn_id: string } };

/** The fields a `status` `running` of an unattended turn adds. */
export interface UnattendedStatusFields {
  /** The host started this turn by itself (its prompt is `host_prompt`, never the person's message). */
  continued_automatically?: boolean;
  /** 1, 2, … for a turn the host started by itself. */
  continuation_index?: number;
  /** On the first `running` of any unattended turn: the grant it runs under. */
  unattended?: Omit<UnattendedGrant, 'chain'> & { grant_id: string };
}
