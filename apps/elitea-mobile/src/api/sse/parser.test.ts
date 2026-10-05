import { EventStreamParser, type SseEvent } from '@/api/sse/parser';
import { FIXTURE_NAMES, loadFixtureBytes } from '@/features/chat/__fixtures__/loadFixture';

const enc = (s: string) => new TextEncoder().encode(s);

function parseAll(chunks: (string | Uint8Array)[], parser = new EventStreamParser()): SseEvent[] {
  const out: SseEvent[] = [];
  for (const chunk of chunks) out.push(...parser.push(typeof chunk === 'string' ? enc(chunk) : chunk));
  out.push(...parser.end());
  return out;
}

describe('EventStreamParser', () => {
  it.each([
    ['LF', '\n'],
    ['CRLF', '\r\n'],
    ['CR', '\r'],
  ])('parses %s line endings', (_name, eol) => {
    const text = ['id: 1', 'event: a', 'data: one', '', 'id: 2', 'data: two', '', ''].join(eol);
    expect(parseAll([text])).toEqual([
      { id: '1', event: 'a', data: 'one' },
      { id: '2', event: 'message', data: 'two' },
    ]);
  });

  it('handles a CRLF split across chunks without producing an extra blank line', () => {
    const events = parseAll(['data: a\r', '\ndata: b\r', '\n\r', '\n']);
    expect(events).toEqual([{ id: null, event: 'message', data: 'a\nb' }]);
  });

  it('handles a chunk boundary inside a line', () => {
    expect(parseAll(['da', 'ta: hel', 'lo\n', '\n'])).toEqual([{ id: null, event: 'message', data: 'hello' }]);
  });

  it('keeps a multi-byte UTF-8 character split across chunks', () => {
    const bytes = enc('data: café ☕ 𝄞\n\n');
    // split every byte
    const chunks = Array.from(bytes, (b) => new Uint8Array([b]));
    expect(parseAll(chunks)).toEqual([{ id: null, event: 'message', data: 'café ☕ 𝄞' }]);
  });

  it('joins multi-line data with LF', () => {
    expect(parseAll(['data: a\ndata:b\ndata\n\n'])).toEqual([{ id: null, event: 'message', data: 'a\nb\n' }]);
  });

  it('treats comment lines (heartbeats) as no events', () => {
    const parser = new EventStreamParser();
    expect(parser.push(enc(': heartbeat\n\n'))).toEqual([]);
  });

  it('ignores an id containing NUL', () => {
    const parser = new EventStreamParser();
    const events = parseAll(['id: 5\ndata: a\n\nid: 6\0x\ndata: b\n\n'], parser);
    expect(events.map((e) => e.id)).toEqual(['5', '5']);
    expect(parser.lastEventId).toBe('5');
  });

  it('parses retry and ignores a non-numeric retry', () => {
    const parser = new EventStreamParser();
    parseAll(['retry: 2000\n\nretry: soon\n\n'], parser);
    expect(parser.retryMs).toBe(2000);
  });

  it('drops a leading BOM once', () => {
    expect(parseAll([new Uint8Array([0xef, 0xbb, 0xbf]), 'data: x\n\n'])).toEqual([
      { id: null, event: 'message', data: 'x' },
    ]);
  });

  it('refuses an oversize event and keeps parsing after it', () => {
    const parser = new EventStreamParser({ maxEventBytes: 16 });
    const events = parseAll([`data: ${'x'.repeat(40)}\n\ndata: ok\n\n`], parser);
    expect(events).toEqual([{ id: null, event: 'message', data: 'ok' }]);
    expect(parser.oversized).toBe(1);
  });

  it('bounds an unterminated line', () => {
    const parser = new EventStreamParser({ maxEventBytes: 16 });
    const events = parseAll(['data: ' + 'y'.repeat(64), '\n\ndata: ok\n\n'], parser);
    expect(events).toEqual([{ id: null, event: 'message', data: 'ok' }]);
    expect(parser.oversized).toBe(1);
  });

  it('does not dispatch an event without data but still moves the cursor', () => {
    const parser = new EventStreamParser();
    expect(parseAll(['id: 9\nevent: x\n\n'], parser)).toEqual([]);
    expect(parser.lastEventId).toBe('9');
  });

  it('discards an unterminated final event', () => {
    expect(parseAll(['data: complete\n\ndata: partial'])).toEqual([
      { id: null, event: 'message', data: 'complete' },
    ]);
  });

  it('starts from a carried-over cursor', () => {
    const parser = new EventStreamParser({ lastEventId: '41' });
    expect(parser.push(enc('data: x\n\n'))).toEqual([{ id: '41', event: 'message', data: 'x' }]);
  });

  it.each(FIXTURE_NAMES)('parses fixture %s identically whole and byte-by-byte', (name) => {
    const bytes = loadFixtureBytes(name);
    const whole = parseAll([bytes]);
    const split = parseAll(Array.from(bytes, (b) => new Uint8Array([b])));
    expect(split).toEqual(whole);
    expect(whole.length).toBeGreaterThan(0);
    for (const e of whole) expect(() => JSON.parse(e.data)).not.toThrow();
  });
});
