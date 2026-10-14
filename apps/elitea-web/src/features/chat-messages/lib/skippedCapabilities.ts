/**
 * What a turn was configured with but could not serve.
 *
 * The Rust worker reports it as `skipped_capabilities` in the
 * `response_metadata` of the turn's `agent_start` and final `full_message`
 * node events; elitea-main persists the final list on the message group's
 * `meta`, so a reload shows what the live stream showed. Before it, an
 * unported toolkit, a refused toolkit setting or an unserved built-in tool was
 * one worker log line and the agent quietly did less.
 *
 * The list is display data. It is parsed into a closed shape, deduplicated and
 * bounded here, and anything malformed is dropped rather than rendered.
 */
import type { ChatMessage } from './convertMessagesToChatHistory.types';

export type SkippedCapabilityKind = 'toolkit' | 'internal_tool' | 'application';

export interface SkippedCapability {
  readonly kind: SkippedCapabilityKind;
  readonly name: string;
  readonly type?: string;
  readonly reasonCode: string;
  readonly reason: string;
}

const MAX_ENTRIES = 32;
const KINDS: readonly string[] = ['toolkit', 'internal_tool', 'application'];

function text(value: unknown): string | undefined {
  return typeof value === 'string' && value.trim() !== '' && value.length <= 512 ? value : undefined;
}

export interface SkippedCapabilities {
  readonly items: readonly SkippedCapability[];
  /** How many more were left out of `items` (the list is bounded); 0 when none. */
  readonly more: number;
}

const NONE: SkippedCapabilities = { items: [], more: 0 };

/** The wire value is `{items: [...], more?: N}`. */
export function parseSkippedCapabilities(raw: unknown): SkippedCapabilities {
  if (raw === null || typeof raw !== 'object' || Array.isArray(raw)) return NONE;
  const record = raw as Record<string, unknown>;
  const items = parseItems(record['items']);
  const more = record['more'];
  const count = typeof more === 'number' && Number.isSafeInteger(more) && more > 0 && more <= 1_000_000 ? more : 0;
  return items.length === 0 ? NONE : { items, more: count };
}

function parseItems(raw: unknown): readonly SkippedCapability[] {
  if (!Array.isArray(raw)) return [];
  const seen = new Set<string>();
  const out: SkippedCapability[] = [];
  for (const entry of raw as readonly unknown[]) {
    if (out.length >= MAX_ENTRIES) break;
    if (entry === null || typeof entry !== 'object') continue;
    const record = entry as Record<string, unknown>;
    const kind = record['kind'];
    const name = text(record['name']);
    const reason = text(record['reason']);
    if (typeof kind !== 'string' || !KINDS.includes(kind) || !name || !reason) continue;
    const type = text(record['type']);
    const reasonCode = text(record['reason_code']) ?? '';
    const key = `${kind}\u0000${name}\u0000${type ?? ''}\u0000${reasonCode}`;
    if (seen.has(key)) continue;
    seen.add(key);
    out.push({ kind: kind as SkippedCapabilityKind, name, ...(type ? { type } : {}), reasonCode, reason });
  }
  return out;
}

/** The turn's skipped capabilities: the live final frame first, the persisted copy otherwise. */
export function skippedCapabilitiesOf(message: Pick<ChatMessage, 'responseMetadata' | 'skippedCapabilities'>): SkippedCapabilities {
  const live = parseSkippedCapabilities(message.responseMetadata?.['skipped_capabilities']);
  return live.items.length > 0 ? live : parseSkippedCapabilities(message.skippedCapabilities);
}
