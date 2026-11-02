/**
 * The grounding indicator of a citing agent's answer (ADR-0043 decision 5).
 *
 * The Rust worker reports it as `grounding` in the `response_metadata` of the
 * final `full_message` event when the agent's version sets `meta.citations`
 * to `required` and the model saw at least one tool result. elitea-main keeps
 * it on the message group's `meta`, so a reload shows what the live stream
 * showed. It is absent, not zero, for any other answer.
 *
 * It holds counts and UTF-16 offsets into the answer text only. There is no
 * confidence score, and this module never derives one.
 */
import type { ChatMessage } from './convertMessagesToChatHistory.types';

interface GroundingSpan {
  readonly start: number;
  readonly end: number;
}

export interface Grounding {
  readonly sourcesAvailable: number;
  readonly sourcesCited: number;
  readonly claimSpans: number;
  readonly uncitedSpans: number;
  readonly uncited: readonly GroundingSpan[];
}

const MAX_SPANS = 64;

function count(value: unknown): number | null {
  return typeof value === 'number' && Number.isInteger(value) && value >= 0 ? value : null;
}

function parseSpans(raw: unknown): readonly GroundingSpan[] | null {
  if (!Array.isArray(raw) || raw.length > MAX_SPANS) return null;
  const spans: GroundingSpan[] = [];
  for (const entry of raw) {
    const start = count((entry as { start?: unknown } | null)?.start);
    const end = count((entry as { end?: unknown } | null)?.end);
    if (start === null || end === null || end <= start) return null;
    spans.push({ start, end });
  }
  return spans;
}

/** The indicator in a closed shape, or `null` when absent or malformed. */
export function parseGrounding(raw: unknown): Grounding | null {
  if (raw === null || typeof raw !== 'object') return null;
  const value = raw as Record<string, unknown>;
  const sourcesAvailable = count(value['sources_available']);
  const sourcesCited = count(value['sources_cited']);
  const claimSpans = count(value['claim_spans']);
  const uncitedSpans = count(value['uncited_spans']);
  const uncited = parseSpans(value['uncited']);
  if (value['version'] !== 1 || sourcesAvailable === null || sourcesAvailable < 1 || sourcesCited === null
    || claimSpans === null || uncitedSpans === null || uncited === null || uncitedSpans > claimSpans) return null;
  return { sourcesAvailable, sourcesCited, claimSpans, uncitedSpans, uncited };
}

/** The live indicator from the final frame, else the persisted one. */
export function groundingOf(message: Pick<ChatMessage, 'responseMetadata' | 'grounding'>): Grounding | null {
  return parseGrounding(message.responseMetadata?.['grounding']) ?? parseGrounding(message.grounding);
}

/** The text of each uncited span inside `answer`; spans outside the text are left out. */
export function uncitedStatements(answer: string, grounding: Grounding): readonly string[] {
  return grounding.uncited
    .filter((span) => span.end <= answer.length)
    .map((span) => answer.slice(span.start, span.end).trim())
    .filter((text) => text.length > 0);
}
