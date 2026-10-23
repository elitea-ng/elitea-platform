import assert from 'node:assert/strict';
import { describe, it } from 'node:test';

import { EventStreamParser, type SseEvent } from './sseParser';

const enc = (text: string): Uint8Array => new TextEncoder().encode(text);

function parseAll(chunks: (string | Uint8Array)[], parser = new EventStreamParser()): SseEvent[] {
  const out: SseEvent[] = [];
  for (const chunk of chunks) out.push(...parser.push(typeof chunk === 'string' ? enc(chunk) : chunk));
  out.push(...parser.end());
  return out;
}

describe('EventStreamParser', () => {
  it('parses named events, multi-line data and the id cursor', () => {
    const p = new EventStreamParser();
    assert.deepEqual(p.pushText('id: 7\nevent: tick\ndata: a\ndata: b\n\n'), [{ id: '7', event: 'tick', data: 'a\nb' }]);
    assert.equal(p.lastEventId, '7');
  });

  for (const [name, eol] of [
    ['LF', '\n'],
    ['CRLF', '\r\n'],
    ['CR', '\r'],
  ] as const) {
    it(`parses ${name} line endings`, () => {
      const text = ['id: 1', 'event: a', 'data: one', '', 'id: 2', 'data: two', '', ''].join(eol);
      assert.deepEqual(parseAll([text]), [
        { id: '1', event: 'a', data: 'one' },
        { id: '2', event: 'message', data: 'two' },
      ]);
    });
  }

  it('defaults the event name to message and strips one leading space only', () => {
    assert.deepEqual(new EventStreamParser().pushText('data:  two spaces\n\n'), [{ id: null, event: 'message', data: ' two spaces' }]);
  });

  it('joins multi-line data with LF', () => {
    assert.deepEqual(parseAll(['data: a\ndata:b\ndata\n\n']), [{ id: null, event: 'message', data: 'a\nb\n' }]);
  });

  it('is independent of chunk boundaries, including inside CRLF and a multi-byte character', () => {
    const wire = enc('event: x\r\ndata: héllo 世界\r\n\r\n');
    for (let cut = 1; cut < wire.length; cut += 1) {
      const p = new EventStreamParser();
      const got = [...p.push(wire.slice(0, cut)), ...p.push(wire.slice(cut)), ...p.end()];
      assert.deepEqual(got, [{ id: null, event: 'x', data: 'héllo 世界' }]);
    }
  });

  it('keeps a multi-byte character split byte by byte', () => {
    const chunks = Array.from(enc('data: café ☕ 𝄞\n\n'), (b) => new Uint8Array([b]));
    assert.deepEqual(parseAll(chunks), [{ id: null, event: 'message', data: 'café ☕ 𝄞' }]);
  });

  it('handles a CRLF split across chunks without an extra blank line', () => {
    assert.deepEqual(parseAll(['data: a\r', '\ndata: b\r', '\n\r', '\n']), [{ id: null, event: 'message', data: 'a\nb' }]);
    const p = new EventStreamParser();
    assert.deepEqual(p.pushText('data: a\r'), []);
    assert.deepEqual(p.pushText('\n\r'), [{ id: null, event: 'message', data: 'a' }]);
  });

  it('handles a chunk boundary inside a line', () => {
    assert.deepEqual(parseAll(['da', 'ta: hel', 'lo\n', '\n']), [{ id: null, event: 'message', data: 'hello' }]);
  });

  it('ignores comments (heartbeats) and unknown fields', () => {
    assert.deepEqual(new EventStreamParser().pushText(': heartbeat\n\n'), []);
    assert.equal(new EventStreamParser().pushText(': heartbeat\nfoo: bar\ndata: x\n\n').length, 1);
  });

  it('drops a leading BOM once, also when it arrives alone', () => {
    assert.deepEqual(new EventStreamParser().push(enc('﻿data: x\n\n')), [{ id: null, event: 'message', data: 'x' }]);
    assert.deepEqual(parseAll([new Uint8Array([0xef, 0xbb, 0xbf]), 'data: x\n\n']), [{ id: null, event: 'message', data: 'x' }]);
  });

  it('ignores an id containing NUL', () => {
    const p = new EventStreamParser();
    const events = parseAll(['id: 5\ndata: a\n\nid: 6\0x\ndata: b\n\n'], p);
    assert.deepEqual(
      events.map((e) => e.id),
      ['5', '5'],
    );
    assert.equal(p.lastEventId, '5');
  });

  it('reads retry and ignores a non-numeric retry', () => {
    const p = new EventStreamParser({ lastEventId: '1' });
    p.pushText('id: a\0b\nretry: soon\ndata: x\n\n');
    assert.equal(p.lastEventId, '1');
    assert.equal(p.retryMs, null);
    p.pushText('retry: 1500\n\n');
    assert.equal(p.retryMs, 1500);
  });

  it('drops an oversized event, counts it, and keeps parsing the next one', () => {
    const p = new EventStreamParser({ maxEventChars: 16 });
    assert.deepEqual(parseAll([`data: ${'x'.repeat(40)}\n\ndata: ok\n\n`], p), [{ id: null, event: 'message', data: 'ok' }]);
    assert.equal(p.oversized, 1);
  });

  it('bounds an unterminated line', () => {
    const p = new EventStreamParser({ maxEventChars: 16 });
    assert.deepEqual(parseAll([`data: ${'y'.repeat(64)}`, '\n\ndata: ok\n\n'], p), [{ id: null, event: 'message', data: 'ok' }]);
    assert.equal(p.oversized, 1);
  });

  it('discards an unterminated final event at end of stream', () => {
    assert.deepEqual(parseAll(['data: complete\n\ndata: partial']), [{ id: null, event: 'message', data: 'complete' }]);
  });

  it('does not dispatch an event without data, but its id still moves the cursor', () => {
    const p = new EventStreamParser({ lastEventId: '1' });
    assert.deepEqual(p.pushText('id: 9\nevent: x\n\n'), []);
    assert.equal(p.lastEventId, '9');
  });

  it('commits the resume cursor at dispatch, not when the id line is read', () => {
    const p = new EventStreamParser({ lastEventId: '1' });
    assert.deepEqual(p.pushText('id: 2\ndata: b'), []);
    assert.deepEqual(p.end(), []);
    // The event never completed: resuming from 2 would skip an event the application never saw.
    assert.equal(p.lastEventId, '1');
  });

  it('starts from a carried-over cursor', () => {
    const p = new EventStreamParser({ lastEventId: '41' });
    assert.deepEqual(p.push(enc('data: x\n\n')), [{ id: '41', event: 'message', data: 'x' }]);
  });
});
