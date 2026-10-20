/**
 * What a cloud turn can be waiting on, read off the newest message of the
 * thread, and the request bodies that answer it — the same contract the web
 * chat page uses (`agent.continue.hitl.v1`, `agent.continue.output_limit.v1`
 * on the continue route), so a turn the desktop started in the cloud never
 * stalls silently here.
 *
 * - an approval (HITL) with ONE card pending: answered inline;
 * - the "continue" after the model's token limit: answered inline;
 * - anything the desktop cannot answer itself — several cards at once, an
 *   authored static pause, node recovery, an MCP authorization — is shown as a
 *   card with a one-click "Answer in chat".
 */
import type { ChatMessage } from '@/features/chat-messages';

/** The decision a HITL card reports (the web chat's `HitlResumePayload`). */
export interface HitlDecision {
  readonly action: 'approve' | 'reject' | 'edit' | 'block_with_comment' | 'answer';
  readonly value?: string | undefined;
  readonly toolCallId?: string | undefined;
  readonly interruptId?: string | undefined;
}

export interface Card {
  readonly interrupt_id?: string;
  readonly tool_call_id?: string;
  readonly child_thread_id?: string;
  readonly decided?: boolean;
  readonly tool_name?: string;
  readonly action_label?: string;
  readonly message?: string;
  readonly policy_message?: string;
  readonly tool_args?: unknown;
  readonly guardrail_type?: string;
  readonly available_actions?: readonly string[];
  readonly questions?: unknown;
  readonly [key: string]: unknown;
}

/** What the thread is waiting for from the person. */
export interface CloudPrompt {
  /** The message that paused. */
  readonly message: ChatMessage;
  /** `approval`: a HITL card; `continue`: the token-limit prompt; `chat`: only the web chat can answer. */
  readonly kind: 'approval' | 'continue' | 'chat';
  /** What the island says ("Approve: run_command"). */
  readonly label: string;
  readonly cards: readonly Card[];
}

const ROOT_ACTIONS: readonly string[] = ['approve', 'reject', 'edit', 'block_with_comment', 'answer'];
const VALUED_ACTIONS: readonly string[] = ['edit', 'block_with_comment', 'answer'];

/** The cards of a message still owed an answer. */
function pendingCards(message: ChatMessage): Card[] {
  const all = Array.isArray(message.hitlInterrupts) && message.hitlInterrupts.length > 0 ? (message.hitlInterrupts as Card[]) : message.hitlInterrupt ? [message.hitlInterrupt as Card] : [];
  return all.filter((card) => card.decided !== true);
}

/** An approve / reject card: no questions to answer, no MCP sign-in, and a way to approve or reject. */
function answerableHere(card: Card): boolean {
  if (card.guardrail_type === 'mcp_auth' || card.guardrail_type === 'clarifying_question' || (Array.isArray(card.questions) && card.questions.length > 0)) return false;
  const actions = card.available_actions ?? ['approve', 'reject'];
  return actions.includes('approve') || actions.includes('reject');
}

/** The pause the newest message is in, if any. */
export function cloudPrompt(history: readonly ChatMessage[], labels: { approve: (tool: string) => string; continuePrompt: string; chat: string }): CloudPrompt | null {
  const message = history[history.length - 1];
  if (message === undefined || message.role === 'user' || message.isStreaming === true) return null;
  const cards = pendingCards(message);
  if (message.staticPause !== undefined || message.nodeRecoveryRequired !== undefined || cards.length > 1) {
    return { message, kind: 'chat', label: labels.chat, cards };
  }
  const [card] = cards;
  if (card !== undefined) {
    // Only an approve / reject card is answered here; a question, an edit or an MCP sign-in needs the chat.
    return answerableHere(card)
      ? { message, kind: 'approval', label: labels.approve(card.tool_name ?? 'tool'), cards }
      : { message, kind: 'chat', label: labels.chat, cards };
  }
  if (message.requiresConfirmation !== undefined) return { message, kind: 'continue', label: labels.continuePrompt, cards };
  return null;
}

/** An `answer` carries structured answers: a JSON object travels as one, anything else as the string it is. */
function answerValue(raw: string): unknown {
  try {
    const parsed: unknown = JSON.parse(raw);
    if (typeof parsed === 'string' || (typeof parsed === 'object' && parsed !== null && !Array.isArray(parsed))) return parsed;
  } catch {
    return raw;
  }
  return raw;
}

interface Identity {
  readonly projectId: string | number | undefined;
  readonly conversationUuid: string | undefined;
  readonly message: ChatMessage;
}

function identity({ projectId, conversationUuid, message }: Identity): Record<string, unknown> | undefined {
  const project = Number(projectId);
  if (!Number.isSafeInteger(project) || project <= 0 || !conversationUuid || !message.id) return undefined;
  return { project_id: project, conversation_uuid: conversationUuid, message_id: message.id };
}

const decisionValue = (decision: HitlDecision): unknown => (decision.action === 'answer' ? answerValue(decision.value ?? '') : (decision.value ?? ''));

/** A decision on a card of a fan-out set: routed by its interrupt id. */
function fanoutBody(withThread: Record<string, unknown>, decision: HitlDecision): Record<string, unknown> | undefined {
  if (!decision.interruptId) return undefined;
  return {
    ...withThread,
    hitl_decisions: [{ interrupt_id: decision.interruptId, tool_call_id: decision.toolCallId ?? '', action: decision.action, value: decisionValue(decision) }],
  };
}

/**
 * The body that answers one pending approval card, or `undefined` when the
 * contract has no body for it (the caller then sends the person to the chat).
 * The route admits no `mcp_tokens` alongside a HITL resume.
 */
export function hitlBody(input: Identity, decision: HitlDecision, card: Card | undefined): Record<string, unknown> | undefined {
  const base = identity(input);
  if (base === undefined) return undefined;
  const withThread = { ...base, ...(input.message.threadId ? { thread_id: input.message.threadId } : {}), hitl_resume: true };
  if (decision.toolCallId || card?.child_thread_id) return fanoutBody(withThread, decision);
  if (!ROOT_ACTIONS.includes(decision.action)) return undefined;
  const valued = VALUED_ACTIONS.includes(decision.action) ? { hitl_value: decisionValue(decision) } : {};
  return { ...withThread, hitl_action: decision.action, ...valued };
}

/** The body that continues a turn that stopped on the model's token limit. */
export function continueBody(input: Identity): Record<string, unknown> | undefined {
  return identity(input);
}

/** The message with its pending approval cards marked decided (or restored) and its streaming flags set. */
export function markDecided(message: ChatMessage, decided: boolean): ChatMessage {
  const cards = (Array.isArray(message.hitlInterrupts) && message.hitlInterrupts.length > 0 ? message.hitlInterrupts : message.hitlInterrupt ? [message.hitlInterrupt] : []) as Card[];
  const next = cards.map((card) => (decided ? { ...card, decided: true, hidden: true } : { ...card, decided: false, hidden: false }));
  return { ...message, hitlInterrupts: next, hitlInterrupt: decided ? undefined : next[0], isLoading: decided, isStreaming: decided };
}
