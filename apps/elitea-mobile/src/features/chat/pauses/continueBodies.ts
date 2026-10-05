import type { components } from '@/api/generated/elitea.gen';
import {
  interruptsOf,
  parseAuthRequest,
  type AuthRequest,
  type HitlInterrupt,
  type TurnPause,
} from '@/features/chat/toolActivity';

/**
 * The bodies of `continueChatExecution` (client contract 1.1), pure. Ported
 * from the web client's documented rules (elitea-web
 * useChatBoxHandlers.hitl.ts / .authorization.ts, written afresh here) and
 * the contract's API_CONTRACT "Answering a pause" table:
 *
 * - A root pause (no tool call, not a fan-out child): `hitl_resume: true`,
 *   `hitl_action`, and `hitl_value` for the valued actions.
 * - A tool-call pause, a parallel pause or fan-out children:
 *   `hitl_decisions: [{interrupt_id, tool_call_id, action, value}]`, one per
 *   interrupt; `thread_id` stays at the top level.
 * - `hitl_value` by action: approve/reject none; edit a non-empty STRING
 *   (the edited arguments as JSON text); block_with_comment a non-empty
 *   string; answer an OBJECT keyed by question id (or a plain string).
 * - A toolkit sign-in Skip: `authorization_request_id` +
 *   `authorization_action: skip` for one request; several go as
 *   `hitl_decisions` with `guardrail_type: mcp_auth`. Mobile never sends
 *   tokens (`mcp_tokens` is left out).
 * - The output-limit Continue: the paused answer's `message_id`.
 */

export type ChatContinueRequest = components['schemas']['ChatContinueRequest'];

export type { ContinueContract } from '@/api/queries/sendMessage';
export const HITL_CONTRACT = 'agent.continue.hitl.v1';
export const AUTHORIZATION_CONTRACT = 'agent.continue.authorization.v1';
export const OUTPUT_LIMIT_CONTRACT = 'agent.continue.output-limit.v1';

const ROOT_ACTIONS = ['approve', 'reject', 'edit', 'block_with_comment', 'answer'];
const VALUED_ACTIONS = ['edit', 'block_with_comment', 'answer'];

export interface PauseTarget {
  projectId: number;
  conversationUuid: string;
  /** The paused answer's message id. */
  messageId: string;
  threadId: string | null;
}

export interface HitlDecision {
  interrupt: HitlInterrupt;
  action: string;
  /** edit / block_with_comment: a string; answer: an object keyed by question id (or a string). */
  value?: unknown;
}

export class ContinueBodyError extends Error {
  constructor(readonly reason: 'target' | 'action' | 'value' | 'interrupt') {
    super(`cannot answer this pause: ${reason}`);
    this.name = 'ContinueBodyError';
  }
}

function identity(target: PauseTarget): Pick<ChatContinueRequest, 'project_id' | 'conversation_uuid' | 'message_id' | 'thread_id'> {
  if (!Number.isSafeInteger(target.projectId) || target.projectId <= 0) throw new ContinueBodyError('target');
  if (!/^[0-9a-fA-F-]{36}$/.test(target.conversationUuid) || target.messageId.trim() === '') {
    throw new ContinueBodyError('target');
  }
  return {
    project_id: target.projectId,
    conversation_uuid: target.conversationUuid,
    message_id: target.messageId,
    ...(target.threadId ? { thread_id: target.threadId } : {}),
  };
}

function checkValue(action: string, value: unknown): unknown {
  if (!VALUED_ACTIONS.includes(action)) return undefined;
  if (action === 'answer') {
    if (typeof value === 'string' && value.trim() !== '') return value;
    if (value !== null && typeof value === 'object' && !Array.isArray(value) && Object.keys(value).length > 0) return value;
    throw new ContinueBodyError('value');
  }
  if (typeof value !== 'string' || value.trim() === '') throw new ContinueBodyError('value');
  return value;
}

function allowed(decision: HitlDecision): boolean {
  const offered = decision.interrupt.availableActions;
  return offered.includes(decision.action) || (decision.action === 'answer' && isAskUser(decision.interrupt));
}

/** The continuation body for one or several HITL decisions (agent.continue.hitl.v1). */
export function buildHitlContinueBody(target: PauseTarget, decisions: readonly HitlDecision[]): ChatContinueRequest {
  const base = { ...identity(target), hitl_resume: true };
  if (decisions.length === 0) throw new ContinueBodyError('action');
  for (const decision of decisions) if (!allowed(decision)) throw new ContinueBodyError('action');

  const [first] = decisions;
  const root = decisions.length === 1 && !first.interrupt.toolCallId && !first.interrupt.childThreadId;
  if (root) {
    if (!ROOT_ACTIONS.includes(first.action)) throw new ContinueBodyError('action');
    const value = checkValue(first.action, first.value);
    return { ...base, hitl_action: first.action, ...(value === undefined ? {} : { hitl_value: value }) };
  }
  return {
    ...base,
    hitl_decisions: decisions.map((decision) => {
      if (!decision.interrupt.interruptId) throw new ContinueBodyError('interrupt');
      const value = checkValue(decision.action, decision.value);
      return {
        interrupt_id: decision.interrupt.interruptId,
        tool_call_id: decision.interrupt.toolCallId ?? '',
        action: decision.action,
        value: value ?? '',
      };
    }),
  };
}

/** Skip every pending toolkit sign-in (agent.continue.authorization.v1). */
export function buildAuthSkipBody(target: PauseTarget, requests: readonly AuthRequest[]): ChatContinueRequest {
  const base = { ...identity(target), thread_id: target.threadId ?? '' };
  if (requests.length === 0) throw new ContinueBodyError('action');
  if (requests.length === 1) {
    return {
      ...base,
      hitl_resume: false,
      hitl_decisions: [],
      authorization_request_id: requests[0].requestId,
      authorization_action: 'skip',
    };
  }
  return {
    ...base,
    hitl_resume: true,
    hitl_decisions: requests.map((request) => ({
      interrupt_id: request.requestId,
      ...(request.toolCallId ? { tool_call_id: request.toolCallId } : {}),
      guardrail_type: 'mcp_auth',
      action: 'skip',
    })),
    authorization_request_id: '',
    authorization_action: '',
  };
}

/** Continue an answer that stopped at the output limit (agent.continue.output-limit.v1). */
export function buildOutputLimitBody(target: PauseTarget): ChatContinueRequest {
  return identity(target);
}

// ---------------------------------------------------------------------------
// ask_user

export interface AskUserQuestion {
  /** The key the answer is sent under (`q{n}` when the question has no id). */
  key: string;
  question: string;
  header: string | null;
  options: { label: string; description: string | null }[];
  multiSelect: boolean;
  /** A free-text "Other" row is offered (also when there are no options). */
  allowOther: boolean;
  /** May be left unanswered; absent means required. */
  optional: boolean;
}

const asRecord = (v: unknown): Record<string, unknown> | null =>
  v !== null && typeof v === 'object' && !Array.isArray(v) ? (v as Record<string, unknown>) : null;
const asText = (v: unknown): string | null => (typeof v === 'string' && v.trim() !== '' ? v.trim().slice(0, 2000) : null);

export function isAskUser(interrupt: HitlInterrupt): boolean {
  return (
    interrupt.availableActions.includes('answer') ||
    interrupt.guardrailType === 'clarifying_question' ||
    interrupt.questions.length > 0
  );
}

/** The questions of an ask_user pause; one free-text question when it carries none. */
export function askUserQuestions(interrupt: HitlInterrupt): AskUserQuestion[] {
  const out: AskUserQuestion[] = [];
  interrupt.questions.forEach((raw, index) => {
    const q = asRecord(raw);
    if (!q) return;
    const options = (Array.isArray(q.options) ? q.options : [])
      .map(asRecord)
      .map((o) => ({ label: asText(o?.label) ?? '', description: asText(o?.description) }))
      .filter((o) => o.label !== '')
      .slice(0, 20);
    out.push({
      key: asText(q.id) ?? `q${index + 1}`,
      question: asText(q.question) ?? asText(q.header) ?? 'Your answer',
      header: asText(q.header),
      options,
      multiSelect: q.multiSelect === true || q.multi_select === true,
      allowOther: q.allow_other === true || options.length === 0,
      optional: q.optional === true,
    });
  });
  if (out.length === 0) {
    out.push({ key: 'q1', question: interrupt.message ?? 'Your answer', header: null, options: [], multiSelect: false, allowOther: true, optional: false });
  }
  return out;
}

/**
 * The `answer` value from what the user chose and typed: an object keyed by
 * question; an unanswered question is left out (never an empty string). A
 * pause that carried no questions answers with the bare text.
 */
export function buildAnswerValue(
  interrupt: HitlInterrupt,
  picked: Record<string, string[]>,
  typed: Record<string, string>,
): Record<string, string | string[]> | string {
  const questions = askUserQuestions(interrupt);
  if (interrupt.questions.length === 0) return (typed.q1 ?? '').trim();
  const answers: Record<string, string | string[]> = {};
  for (const question of questions) {
    const chosen = picked[question.key] ?? [];
    const text = (typed[question.key] ?? '').trim();
    if (question.multiSelect) {
      const values = text ? [...chosen, text] : [...chosen];
      if (values.length > 0) answers[question.key] = values;
    } else {
      const value = text || chosen[0] || '';
      if (value) answers[question.key] = value;
    }
  }
  return answers;
}

/** The required questions still unanswered. */
export function missingAnswers(interrupt: HitlInterrupt, picked: Record<string, string[]>, typed: Record<string, string>): string[] {
  return askUserQuestions(interrupt)
    .filter((q) => !q.optional && (picked[q.key] ?? []).length === 0 && (typed[q.key] ?? '').trim() === '')
    .map((q) => q.key);
}

// ---------------------------------------------------------------------------
// Rebuilding a pause after a reload

/**
 * The pause a persisted answer still waits for, from its group metadata
 * (`hitl_interrupt(s)`, `thread_id`, `authorization_requests`,
 * `output_limit_reached`). Only the newest answer can be pending; the
 * caller passes that one. `messageId` is the group's uuid.
 */
export function pauseFromGroup(group: Record<string, unknown> | null | undefined): TurnPause | null {
  if (!group || group.is_streaming === true) return null;
  const meta = asRecord(group.meta) ?? asRecord(group.metadata);
  const messageId = typeof group.uuid === 'string' && group.uuid !== '' ? group.uuid : typeof group.uid === 'string' ? group.uid : null;
  if (!meta || messageId === null) return null;
  const threadId = asText(meta.thread_id);
  if (Array.isArray(meta.authorization_requests) && meta.authorization_requests.length > 0) {
    const requests = meta.authorization_requests.map(parseAuthRequest).filter((r): r is AuthRequest => r !== null);
    if (requests.length > 0) return { kind: 'auth', requests, threadId, messageId };
  }
  if ((Array.isArray(meta.hitl_interrupts) && meta.hitl_interrupts.length > 0) || asRecord(meta.hitl_interrupt)) {
    const interrupts = interruptsOf({ hitl_interrupts: meta.hitl_interrupts, hitl_interrupt: meta.hitl_interrupt ?? {} });
    if (interrupts.length > 0) {
      const fanout = interrupts.some((i) => i.childThreadId);
      return { kind: 'hitl', shape: fanout ? 'fanout' : interrupts.length > 1 ? 'parallel' : 'single', interrupts, threadId, messageId };
    }
  }
  if (meta.output_limit_reached === true) return { kind: 'output_limit', threadId, messageId, action: 'Continue' };
  return null;
}
