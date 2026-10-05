/**
 * A WHATWG `text/event-stream` parser (HTML Living Standard, "Server-sent
 * events" § parsing an event stream), as a pure, incremental function of bytes.
 *
 * - Bytes are decoded as UTF-8 with `TextDecoder(…, { stream: true })`, so a
 *   multi-byte character split across chunks survives; a leading BOM is dropped.
 * - Lines end in CRLF, LF or CR (a CR at the end of a chunk waits for the next
 *   chunk to see whether an LF follows).
 * - `:` lines are comments (heartbeats); `event`, `data` (multi-line, joined
 *   with LF), `id` (ignored when it contains NUL) and `retry` (digits only) are
 *   the fields; anything else is ignored.
 * - A blank line dispatches the event. An event with no `data` is not
 *   dispatched, but its `id` still moves `lastEventId` (the resume cursor).
 * - An event whose data exceeds `maxEventBytes` is dropped and counted; the
 *   server caps frames at 64 KiB, so 256 Ki is a generous ceiling that still
 *   bounds memory against a misbehaving peer. (The bound is applied to decoded
 *   UTF-16 code units, which never exceed the UTF-8 byte count.)
 */

export interface SseEvent {
  /** The last event id at dispatch (the resume cursor), or null if none seen yet. */
  id: string | null;
  /** The `event:` name, `message` when absent. */
  event: string;
  data: string;
}

export const DEFAULT_MAX_EVENT_BYTES = 256 * 1024;

export interface ParserOptions {
  maxEventBytes?: number;
  /** Resume cursor carried over from an earlier connection. */
  lastEventId?: string | null;
}

export class EventStreamParser {
  /** The last `id:` seen (persisting across events, like the browser's). */
  lastEventId: string | null;
  /** The last valid `retry:` value in milliseconds, if any. */
  retryMs: number | null = null;
  /** Events dropped for exceeding `maxEventBytes`. */
  oversized = 0;

  private readonly maxEventBytes: number;
  // ignoreBOM: keep the BOM so it is stripped exactly once, below.
  private readonly decoder = new TextDecoder('utf-8', { ignoreBOM: true });
  private buffer = '';
  private skipLeadingLf = false;
  private atStreamStart = true;
  private eventType = '';
  private dataParts: string[] = [];
  private dataLength = 0;
  private dropping = false;

  constructor(options: ParserOptions = {}) {
    this.maxEventBytes = options.maxEventBytes ?? DEFAULT_MAX_EVENT_BYTES;
    this.lastEventId = options.lastEventId ?? null;
  }

  /** Feeds a chunk of bytes; returns the events it completed. */
  push(chunk: Uint8Array): SseEvent[] {
    return this.feed(this.decoder.decode(chunk, { stream: true }), false);
  }

  /** Feeds already-decoded text (tests, or a transport that decodes). */
  pushText(text: string): SseEvent[] {
    return this.feed(text, false);
  }

  /**
   * End of stream. Per the standard an unterminated final event is discarded,
   * so this only flushes the decoder and a trailing CR-terminated line.
   */
  end(): SseEvent[] {
    return this.feed(this.decoder.decode(), true);
  }

  private feed(text: string, final: boolean): SseEvent[] {
    if (this.atStreamStart && text.length > 0) {
      this.atStreamStart = false;
      if (text.charCodeAt(0) === 0xfeff) text = text.slice(1);
    }
    this.buffer += text;
    const out: SseEvent[] = [];
    let start = 0;
    const buf = this.buffer;
    if (this.skipLeadingLf && buf.length > 0) {
      this.skipLeadingLf = false;
      if (buf.charCodeAt(0) === 0x0a) start = 1;
    }
    for (let i = start; i < buf.length; i++) {
      const c = buf.charCodeAt(i);
      if (c !== 0x0a && c !== 0x0d) continue;
      this.processLine(buf.slice(start, i), out);
      if (c === 0x0d) {
        if (i + 1 < buf.length) {
          if (buf.charCodeAt(i + 1) === 0x0a) i++;
        } else if (!final) {
          this.skipLeadingLf = true;
        }
      }
      start = i + 1;
    }
    this.buffer = buf.slice(start);
    // A line that never ends must not grow without bound.
    if (this.buffer.length > this.maxEventBytes) {
      this.buffer = '';
      this.startDropping();
    }
    return out;
  }

  private startDropping() {
    if (!this.dropping) this.oversized += 1;
    this.dropping = true;
    this.dataParts = [];
    this.dataLength = 0;
  }

  private processLine(line: string, out: SseEvent[]) {
    if (line === '') {
      this.dispatch(out);
      return;
    }
    if (line.charCodeAt(0) === 0x3a /* : */) return;
    const colon = line.indexOf(':');
    let field: string;
    let value: string;
    if (colon === -1) {
      field = line;
      value = '';
    } else {
      field = line.slice(0, colon);
      value = line.slice(colon + 1);
      if (value.charCodeAt(0) === 0x20) value = value.slice(1);
    }
    switch (field) {
      case 'event':
        this.eventType = value;
        break;
      case 'data':
        if (this.dropping) break;
        this.dataLength += value.length + 1;
        if (this.dataLength > this.maxEventBytes) {
          this.startDropping();
          break;
        }
        this.dataParts.push(value);
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

  private dispatch(out: SseEvent[]) {
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
