/**
 * The run brief and the goal check of a chain (ADR-0045 decision 11), read
 * from where each origin carries them: the cloud answer's message meta
 * (`meta.run_brief`, `meta.goal_check`) and the desktop host's `run_brief`,
 * `goal_checked` and `chain_ended.goal` events. Main writes both, but they are
 * a model's words: each field is read as plain text and bounded here again,
 * and a shape that is not one reads as absent.
 */
import type {
  ChainGoal,
  GoalCheckCondition,
  GoalCheckOutcome,
  GoalCheckResult,
  RunBrief,
  RunBriefCondition,
  RunBriefState,
} from '@/shared/desktop/unattendedIpc';

/** A brief as the transcript shows it: its state, and the brief once it is `ready`. */
export interface RunBriefView {
  state: RunBriefState;
  brief?: RunBrief;
}

/** A goal check as the transcript shows it; `conditions` is empty when only the short form (`chain_ended.goal`) came. */
export type GoalCheckView = GoalCheckResult;

const STATES: readonly RunBriefState[] = ['none', 'off', 'pending', 'ready', 'failed'];
const OUTCOMES: readonly GoalCheckOutcome[] = ['met', 'not_met', 'blocked', 'not_checked'];
const VERDICTS: readonly GoalCheckCondition['verdict'][] = ['met', 'not_met', 'impossible'];
const AMBIGUITY: readonly RunBrief['ambiguity'][] = ['low', 'medium', 'high'];
const MAX_ITEMS = 5;
const LONG = 300;
const SHORT = 200;

const isRecord = (value: unknown): value is Readonly<Record<string, unknown>> => typeof value === 'object' && value !== null && !Array.isArray(value);

const oneOf = <T extends string>(values: readonly T[], value: unknown): T | undefined => values.find((item) => item === value);

/** A bounded line of text: trimmed, at most `max` characters; undefined when empty or not a string. */
function line(value: unknown, max: number): string | undefined {
  if (typeof value !== 'string') return undefined;
  const text = value.trim();
  return text === '' ? undefined : text.slice(0, max);
}

function lines(value: unknown, max: number): string[] {
  if (!Array.isArray(value)) return [];
  return value.flatMap((item) => line(item, max) ?? []).slice(0, MAX_ITEMS);
}

function conditions(value: unknown): RunBriefCondition[] {
  if (!Array.isArray(value)) return [];
  return value
    .flatMap((item) => {
      if (!isRecord(item)) return [];
      const [id, text] = [line(item['id'], 16), line(item['text'], LONG)];
      return id === undefined || text === undefined ? [] : [{ id, text }];
    })
    .slice(0, MAX_ITEMS);
}

/** A brief, or undefined when it has no goal. */
function readBrief(raw: unknown): RunBrief | undefined {
  if (!isRecord(raw)) return undefined;
  const goal = line(raw['goal'], LONG);
  if (goal === undefined) return undefined;
  return {
    goal,
    done_when: conditions(raw['done_when']),
    constraints: lines(raw['constraints'], SHORT),
    assumptions: lines(raw['assumptions'], SHORT),
    ambiguity: oneOf(AMBIGUITY, raw['ambiguity']) ?? 'low',
  };
}

/** `{state, brief?}` (cloud meta and the `run_brief` event). A `ready` state without a valid brief reads as `failed`. */
export function readRunBrief(raw: unknown): RunBriefView | undefined {
  if (!isRecord(raw)) return undefined;
  const state = oneOf(STATES, raw['state']);
  if (state === undefined) return undefined;
  if (state !== 'ready') return { state };
  const brief = readBrief(raw['brief']);
  return brief === undefined ? { state: 'failed' } : { state, brief };
}

const count = (value: unknown): number | undefined => (typeof value === 'number' && Number.isInteger(value) && value >= 0 ? value : undefined);

function checkedConditions(value: unknown): GoalCheckCondition[] {
  if (!Array.isArray(value)) return [];
  return value
    .flatMap((item) => {
      if (!isRecord(item)) return [];
      const [id, text, verdict] = [line(item['id'], 16), line(item['text'], LONG), oneOf(VERDICTS, item['verdict'])];
      return id === undefined || text === undefined || verdict === undefined ? [] : [{ id, text, verdict, reason: line(item['reason'], LONG) ?? '' }];
    })
    .slice(0, MAX_ITEMS);
}

/** A goal check result (cloud meta, `goal_checked`, or the short `chain_ended.goal`), or undefined for any other shape. */
export function readGoalCheck(raw: unknown): GoalCheckView | undefined {
  if (!isRecord(raw)) return undefined;
  const outcome = oneOf(OUTCOMES, raw['outcome']);
  if (outcome === undefined) return undefined;
  const checked = checkedConditions(raw['conditions']);
  const total = count(raw['total']) ?? checked.length;
  const met = Math.min(count(raw['met']) ?? checked.filter((item) => item.verdict === 'met').length, total);
  const reason = line(raw['reason'], LONG);
  return { outcome, conditions: checked, met, total, ...(reason === undefined ? {} : { reason }) };
}

/** The short goal of a `chain_ended`, as a check view. */
export function goalCheckOfChainEnd(goal: ChainGoal | undefined): GoalCheckView | undefined {
  return goal === undefined ? undefined : readGoalCheck(goal);
}
