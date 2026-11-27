/**
 * The choices a folder's new thread makes in its composer before the first message (#3144): the mode (Plan, Bypass,
 * Full auto) and "Run to completion". With "Work in › New worktree" the first message moves to the worktree's own new
 * thread, which is a new session of another workspace: its composer starts from these, not from that workspace's kept
 * choices, so the thread runs from its first turn as chosen. "Respect AGENTS.md" moves with the message too
 * (`movePendingRespect`), and the agents (`SelectionSeed`, #3128).
 *
 * Full auto is in it only once the person confirmed it in this composer (ADR-0061): it is the thread being created
 * that gets it, never a default; nothing here is stored.
 */
import type { RunChoice } from '@/features/unattended';

export interface PreThreadChoices {
  readonly planMode: boolean;
  /** Bypass was on for the folder (its kept choice, confirmed there). */
  readonly bypass: boolean;
  /** Full auto, confirmed in this composer. */
  readonly fullAuto: boolean;
  /** "Run to completion" as chosen; absent when the control was not offered (grants turned off). */
  readonly runToCompletion?: RunChoice | undefined;
}
