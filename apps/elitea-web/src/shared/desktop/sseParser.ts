/**
 * A WHATWG `text/event-stream` parser as a pure, incremental function of
 * bytes (HTML Living Standard, "Server-sent events" § parsing an event
 * stream). Ported in logic from the mobile client's `api/sse/parser.ts`; it
 * exists because the desktop client reads the stream with `fetch` (a bearer
 * header cannot be set on `EventSource`) and must therefore do the parsing the
 * browser does for it.
 *
 * - UTF-8 with a streaming decoder, so a multi-byte character split across
 *   chunks survives; a leading BOM is dropped.
 * - Lines end in CRLF, LF or CR (a CR at the end of a chunk waits for the next
 *   chunk to see whether an LF follows).
 * - `:` lines are comments (heartbeats); `event`, `data` (multi-line, joined
 *   with LF), `id` (ignored when it contains NUL) and `retry` (digits only).
 * - A blank line dispatches. An event with no `data` is not dispatched, but
 *   its `id` still moves `lastEventId` (the resume cursor).
 * - An event larger than `maxEventChars` is dropped, which bounds memory
 *   against a misbehaving peer.
 */

export interface SseEvent {
  /** The last event id at dispatch (the resume cursor), or `null` if none yet. */
  id: string | null;
  /** The `event:` name; `message` when absent. */
  event: string;
  data: string;
}

const DEFAULT_MAX_EVENT_CHARS = 256 * 1024;

export class EventStreamParser {
  lastEventId: string | null;
  /** The last valid `retry:` value in milliseconds, if any. */
  retryMs: number | null = null;
  /** Events dropped for exceeding the size bound. */
  oversized = 0;

  private readonly maxEventChars: number;
  private readonly decoder = new TextDecoder('utf-8', { ignoreBOM: true });
  private buffer = '';
  private skipLeadingLf = false;
  private atStreamStart = true;
  private eventType = '';
  private dataParts: string[] = [];
  private dataLength = 0;
  private dropping = false;

  constructor(options: { maxEventChars?: number; lastEventId?: string | null } = {}) {
    this.maxEventChars = options.maxEventChars ?? DEFAULT_MAX_EVENT_CHARS;
    this.lastEventId = options.lastEventId ?? null;
  }

  /** Feed a chunk of bytes; returns the events it completed. */
  push(chunk: Uint8Array): SseEvent[] {
    return this.feed(this.decoder.decode(chunk, { stream: true }), false);
  }

  /** Feed already-decoded text. */
  pushText(text: string): SseEvent[] {
    return this.feed(text, false);
  }

  /** End of stream: an unterminated final event is discarded, per the standard. */
  end(): SseEvent[] {
    return this.feed(this.decoder.decode(), true);
  }

  private feed(input: string, final: boolean): SseEvent[] {
    const text = this.stripBom(input);
    this.buffer += text;
    const out: SseEvent[] = [];
    const buf = this.buffer;
    let start = this.consumeSkippedLf(buf);
    for (let i = start; i < buf.length; i++) {
      const c = buf.charCodeAt(i);
      if (c !== 0x0a && c !== 0x0d) continue;
      this.processLine(buf.slice(start, i), out);
      if (c === 0x0d) i += this.crlfAdvance(buf, i, final);
      start = i + 1;
    }
    this.buffer = buf.slice(start);
    // A line that never ends must not grow without bound.
    if (this.buffer.length > this.maxEventChars) {
      this.buffer = '';
      this.startDropping();
    }
    return out;
  }

  private stripBom(text: string): string {
    if (!this.atStreamStart || text.length === 0) return text;
    this.atStreamStart = false;
    return text.charCodeAt(0) === 0xfeff ? text.slice(1) : text;
  }

  /** A CR ended the previous chunk: an LF opening this one belongs to it. */
  private consumeSkippedLf(buf: string): number {
    if (!this.skipLeadingLf || buf.length === 0) return 0;
    this.skipLeadingLf = false;
    return buf.charCodeAt(0) === 0x0a ? 1 : 0;
  }

  /** After a CR: swallow a following LF; at the end of a chunk, remember to. */
  private crlfAdvance(buf: string, i: number, final: boolean): number {
    if (i + 1 < buf.length) return buf.charCodeAt(i + 1) === 0x0a ? 1 : 0;
    if (!final) this.skipLeadingLf = true;
    return 0;
  }

  private startDropping(): void {
    if (!this.dropping) this.oversized += 1;
    this.dropping = true;
    this.dataParts = [];
    this.dataLength = 0;
  }

  private processLine(line: string, out: SseEvent[]): void {
    if (line === '') {
      this.dispatch(out);
      return;
    }
    if (line.charCodeAt(0) === 0x3a /* : */) return;
    const colon = line.indexOf(':');
    const field = colon === -1 ? line : line.slice(0, colon);
    let value = colon === -1 ? '' : line.slice(colon + 1);
    if (value.charCodeAt(0) === 0x20) value = value.slice(1);
    this.applyField(field, value);
  }

  private applyField(field: string, value: string): void {
    switch (field) {
      case 'event':
        this.eventType = value;
        break;
      case 'data':
        this.addData(value);
        break;
      case 'id':
        if (!value.includes('\0')) this.lastEventId = value;
        break;
      case 'retry':
        if (/^\d+$/.test(value)) this.retryMs = Number(value);
        break;
      default:
        break;
    }
  }

  private addData(value: string): void {
    if (this.dropping) return;
    this.dataLength += value.length + 1;
    if (this.dataLength > this.maxEventChars) {
      this.startDropping();
      return;
    }
    this.dataParts.push(value);
  }

  private dispatch(out: SseEvent[]): void {
    const hadData = this.dataParts.length > 0;
    const dropped = this.dropping;
    const event = this.eventType === '' ? 'message' : this.eventType;
    const data = this.dataParts.join('\n');
    this.eventType = '';
    this.dataParts = [];
    this.dataLength = 0;
    this.dropping = false;
    if (dropped || !hadData) return;
    out.push({ id: this.lastEventId, event, data });
  }
}
