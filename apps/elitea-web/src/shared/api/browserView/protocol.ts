/**
 * The cloud browser view stream's messages (client contract 1.40,
 * `viewBrowserSession`, ADR-0049 decision 6). Down: one JSON text message per
 * event, and each `frame` header is followed by exactly ONE binary message
 * holding its JPEG. Up: JSON text messages of at most 4 KiB.
 *
 * The server's side is `services/elitea-main/internal/api/v2/browsersessions`
 * (`wire.go`). Everything here is parsed defensively: an unknown message is
 * dropped, never thrown.
 */

/** Who drives the page. */
export type BrowserControl = 'agent' | 'user';

export interface BrowserViewHello {
  readonly type: 'hello';
  readonly session_id: string;
  /** Main decided this viewer may drive (the run's owner, or a participant with `models.chat.messages.create`). */
  readonly can_control: boolean;
  /** The run ended: the browser stays up for its linger, watch-only. */
  readonly lingering: boolean;
}

export interface BrowserViewFrameHeader {
  readonly type: 'frame';
  readonly seq: number;
  /** Logical pixels: input positions are in these units. */
  readonly width: number;
  readonly height: number;
  readonly bytes: number;
  readonly url: string;
  readonly title: string;
  readonly control: BrowserControl;
}

/** `notice` (`input_refused`, `session_lingering`, `control_revoked`, `engine_switched`), `refused` (`input_refused`, `malformed`) and `end`. */
export interface BrowserViewCode {
  readonly type: 'notice' | 'refused' | 'end';
  readonly code: string;
}

export type BrowserViewDown = BrowserViewHello | BrowserViewFrameHeader | BrowserViewCode;

/** CDP's modifier bits. */
export const MODIFIER = { alt: 1, ctrl: 2, meta: 4, shift: 8 } as const;

export type BrowserViewUp =
  | { readonly type: 'ack'; readonly seq: number }
  | { readonly type: 'pointer'; readonly kind: 'move' | 'down' | 'up'; readonly x: number; readonly y: number; readonly button: number; readonly click_count: number; readonly modifiers: number }
  | { readonly type: 'wheel'; readonly x: number; readonly y: number; readonly delta_x: number; readonly delta_y: number; readonly modifiers: number }
  | { readonly type: 'key'; readonly kind: 'down' | 'up' | 'char'; readonly key: string; readonly code: string; readonly text: string; readonly modifiers: number }
  | { readonly type: 'take_over' }
  | { readonly type: 'hand_back'; readonly note: string };

/** The bounds Main and the supervisor hold an up message to. */
export const MAX_KEY_FIELD_BYTES = 32;
export const MAX_NOTE_BYTES = 2048;

/** Close codes the relay ends a view with. */
export const CLOSE_ACCESS_REVOKED = 4403;
export const CLOSE_TRY_AGAIN_LATER = 1013;

function record(value: unknown): Record<string, unknown> | undefined {
  return typeof value === 'object' && value !== null && !Array.isArray(value) ? (value as Record<string, unknown>) : undefined;
}

const num = (value: unknown): number | undefined => (typeof value === 'number' && Number.isFinite(value) ? value : undefined);
const str = (value: unknown): string => (typeof value === 'string' ? value : '');

function parseFrame(value: Record<string, unknown>): BrowserViewFrameHeader | undefined {
  const seq = num(value['seq']);
  const width = num(value['width']);
  const height = num(value['height']);
  const bytes = num(value['bytes']);
  if (seq === undefined || width === undefined || height === undefined || bytes === undefined) return undefined;
  return { type: 'frame', seq, width, height, bytes, url: str(value['url']), title: str(value['title']), control: value['control'] === 'user' ? 'user' : 'agent' };
}

/** One down text message, or `undefined` for anything this client does not know. */
export function parseDown(raw: string): BrowserViewDown | undefined {
  let value: Record<string, unknown> | undefined;
  try {
    value = record(JSON.parse(raw));
  } catch {
    return undefined;
  }
  if (value === undefined) return undefined;
  switch (value['type']) {
    case 'hello':
      return { type: 'hello', session_id: str(value['session_id']), can_control: value['can_control'] === true, lingering: value['lingering'] === true };
    case 'frame':
      return parseFrame(value);
    case 'notice':
    case 'refused':
    case 'end':
      return { type: value['type'], code: str(value['code']) };
    default:
      return undefined;
  }
}

/** Cut `text` to at most `max` UTF-8 bytes, never inside a character. */
export function clipUtf8(text: string, max: number): string {
  const encoder = new TextEncoder();
  if (encoder.encode(text).length <= max) return text;
  let out = '';
  let used = 0;
  for (const char of text) {
    const size = encoder.encode(char).length;
    if (used + size > max) break;
    out += char;
    used += size;
  }
  return out;
}
