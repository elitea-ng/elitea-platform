/**
 * The answered-`ask_user` summary, ported from the baseline's
 * `features/chat/lib/helpers/hitl.helpers.js` (`findAskUserAction`,
 * `buildAskUserSummary` and their private helpers), behaviour unchanged.
 */
import { ToolActionStatus } from '@/shared/lib/chat';

const ASK_USER_TOOL_NAME = 'ask_user';
const ANSWER_LINE_PREFIX = '- ';

/** One question and the answer the user gave it. */
export interface AskUserSummaryItem {
  readonly question: string;
  readonly answer: string;
}

interface AskUserActionLike {
  readonly name?: unknown;
  readonly status?: unknown;
  readonly toolMeta?: unknown;
}

interface QuestionSpec {
  readonly question?: unknown;
  readonly header?: unknown;
  readonly id?: unknown;
}

// Splits the ask_user tool result string ("User answered:\n- <label>: <value>")
// into its answer-line bodies (prefix stripped), tolerating a non-string input.
function extractAnswerLines(raw: unknown): string[] {
  const text = decodedOutput(raw);
  if (typeof text !== 'string') return [];
  return text
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => line.startsWith(ANSWER_LINE_PREFIX))
    .map((line) => line.slice(ANSWER_LINE_PREFIX.length));
}

// The native runtime persists a string tool output JSON-encoded
// (`"User answered:\n- …"`); decode it so its lines can be read.
function decodedOutput(raw: unknown): unknown {
  if (typeof raw !== 'string' || !raw.startsWith('"')) return raw;
  try {
    const parsed: unknown = JSON.parse(raw);
    return typeof parsed === 'string' ? parsed : raw;
  } catch {
    return raw;
  }
}

// A call that paused for the user settles as complete but carries its pause;
// it has no answer yet, so the summary skips it.
function isPausedCall(action: AskUserActionLike): boolean {
  const meta = action.toolMeta;
  return meta !== null && typeof meta === 'object' && 'pause' in meta && Boolean((meta as { pause?: unknown }).pause);
}

// Fallback { label: value } map for when the question specs are unavailable.
function parseAskUserAnswerLines(raw: unknown): Map<string, string> {
  const map = new Map<string, string>();
  extractAnswerLines(raw).forEach((body) => {
    const sepIndex = body.indexOf(': ');
    if (sepIndex === -1) return;
    const label = body.slice(0, sepIndex).trim();
    const value = body.slice(sepIndex + 2).trim();
    if (label) map.set(label, value);
  });
  return map;
}

// Reads the questions array from an ask_user tool-inputs value, tolerating a raw
// object ({ questions: [...] }), a bare array, or a JSON string of either.
function extractAskUserQuestions(toolInputs: unknown): readonly unknown[] {
  let src: unknown = toolInputs;
  if (typeof src === 'string') {
    try {
      src = JSON.parse(src);
    } catch {
      src = null;
    }
  }
  if (src !== null && typeof src === 'object' && !Array.isArray(src)) {
    const questions = (src as { readonly questions?: unknown }).questions;
    if (Array.isArray(questions)) return questions;
  }
  if (Array.isArray(src)) return src;
  return [];
}

// Matches a question to its answer by prefix: the runtime writes each line as
// "- <full question text>: <value>", and the question text itself may contain
// ": ", so splitting on the first separator is unreliable.
function matchAnswer(label: string, answerLines: readonly string[]): string {
  const line = answerLines.find((body) => body.startsWith(label));
  return line ? line.slice(label.length).replace(/^:\s*/, '').trim() : '';
}

/** The first label a question spec carries (`question`, then `header`, then `id`). */
function questionLabel(spec: unknown): string {
  if (spec === null || typeof spec !== 'object') return '';
  const { question, header, id } = spec as QuestionSpec;
  for (const value of [question, header, id]) {
    if (typeof value === 'string' && value) return value;
  }
  return '';
}

/** Finds a completed ask_user tool action (live-answered or history-reloaded). */
export function findAskUserAction<T extends AskUserActionLike>(toolActions: readonly T[] | null | undefined): T | null {
  if (!Array.isArray(toolActions)) return null;
  const list: readonly T[] = toolActions;
  return (
    list.find((item) => item?.name === ASK_USER_TOOL_NAME && item?.status === ToolActionStatus.complete && !isPausedCall(item)) ??
    null
  );
}

/**
 * Builds the display list from an ask_user action's tool inputs (questions)
 * and tool output (answer string). Returns null when there is nothing to
 * summarise.
 */
export function buildAskUserSummary(toolInputs: unknown, toolOutputs: unknown): AskUserSummaryItem[] | null {
  const questions = extractAskUserQuestions(toolInputs);
  const answerLines = extractAnswerLines(toolOutputs);

  if (questions.length) {
    const items = questions
      .map((spec) => {
        const label = questionLabel(spec);
        return { question: label, answer: label ? matchAnswer(label, answerLines) : '' };
      })
      .filter((item) => item.question);
    return items.length ? items : null;
  }

  const fallbackItems = [...parseAskUserAnswerLines(toolOutputs)].map(([question, answer]) => ({ question, answer }));
  return fallbackItems.length ? fallbackItems : null;
}
