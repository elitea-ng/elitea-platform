/**
 * The cloud copy's messages in the local transcript's own terms, so a cloud
 * thread reads exactly like a local one: your prompts, then what the agent
 * did — its text and its tool steps (`TurnView`) — with no avatars, no
 * "to <agent>", no reactions and no timestamps (the local transcript has none).
 */
import type { ChatMessage } from '@/features/chat-messages';
import type { ComponentProps } from 'react';

import type { TurnTranscript } from '@/features/workspace';

/** What the local transcript renders for one turn, and one entry of it. */
type TurnView = ComponentProps<typeof TurnTranscript>['view'];
type TranscriptItem = TurnView['items'][number];

const SUMMARY_CHARS = 120;
const RESULT_CHARS = 4000;

/** One tool run's inputs, as a short line ("path: README.md"). */
function argsSummary(inputs: unknown): string {
  if (inputs === undefined || inputs === null || inputs === '') return '';
  const line = typeof inputs === 'string' ? inputs : JSON.stringify(inputs);
  return line.length <= SUMMARY_CHARS ? line : `${line.slice(0, SUMMARY_CHARS - 1)}…`;
}

function resultText(output: unknown): string {
  if (output === undefined || output === null) return '';
  const raw = typeof output === 'string' ? output : JSON.stringify(output, null, 2);
  return raw.length <= RESULT_CHARS ? raw : raw.slice(0, RESULT_CHARS);
}

interface RawAction {
  readonly id?: unknown;
  readonly type?: string;
  readonly name?: string;
  readonly original_name?: string | null;
  readonly status?: unknown;
  readonly isError?: boolean;
  readonly toolInputs?: unknown;
  readonly toolOutputs?: unknown;
  readonly content?: unknown;
}

function toolItem(action: RawAction, index: number): TranscriptItem | undefined {
  if (action.type !== undefined && action.type !== 'tool') return undefined;
  const callId = typeof action.id === 'string' ? action.id : `tool-${String(index)}`;
  const done = action.status === 'complete' || action.status === 'error' || action.isError === true;
  const summary = resultText(action.isError === true ? action.content : (action.toolOutputs ?? action.content));
  const tool = action.name ?? action.original_name ?? 'tool';
  return {
    type: 'tool',
    key: `c${callId}`,
    callId,
    tool,
    argsSummary: argsSummary(action.toolInputs),
    // The cloud runs it: the row says "Remote".
    remote: true,
    ...(done ? { result: { ok: action.isError !== true, summary, truncated: false } } : {}),
  };
}

/** What an assistant message shows: its tool steps, then its text, and its error. */
export function turnViewOf(message: ChatMessage): TurnView {
  const tools = ((message.toolActions ?? []) as readonly RawAction[]).map(toolItem).filter((item): item is TranscriptItem => item !== undefined);
  const items: TranscriptItem[] = [...tools, ...(message.content === '' ? [] : [{ type: 'text' as const, key: `t-${message.id}`, text: message.content }])];
  const running = message.isStreaming === true;
  // A turn paused on an approval is not "done": its card below says what it waits for.
  const paused = ((message.hitlInterrupts ?? []) as readonly { decided?: boolean }[]).some((card) => card.decided !== true) || message.hitlInterrupt !== undefined;
  const exception = message.exception;
  return {
    phase: running ? 'running' : paused ? null : 'done',
    items,
    approvals: [],
    ...(exception !== undefined && exception !== null && exception !== '' ? { error: { code: message.failureCode ?? 'error', message: typeof exception === 'string' ? exception : JSON.stringify(exception) } } : {}),
  };
}

/** A user prompt followed by the answers to it: one exchange, as the local transcript groups them. */
export interface Exchange {
  readonly key: string;
  readonly prompt: ChatMessage | undefined;
  readonly answers: readonly ChatMessage[];
}

export function exchangesOf(history: readonly ChatMessage[]): Exchange[] {
  const out: { key: string; prompt: ChatMessage | undefined; answers: ChatMessage[] }[] = [];
  for (const message of history) {
    if (message.role === 'user') out.push({ key: message.id, prompt: message, answers: [] });
    else if (out.length === 0) out.push({ key: message.id, prompt: undefined, answers: [message] });
    else out[out.length - 1]?.answers.push(message);
  }
  return out;
}
