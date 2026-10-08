import { describe, expect, it } from 'vitest';

import { EventStreamParser } from './sseParser';

const enc = (text: string): Uint8Array => new TextEncoder().encode(text);

describe('EventStreamParser', () => {
  it('parses named events, multi-line data and the id cursor', () => {
    const p = new EventStreamParser();
    const events = p.pushText('id: 7\nevent: tick\ndata: a\ndata: b\n\n');
    expect(events).toEqual([{ id: '7', event: 'tick', data: 'a\nb' }]);
    expect(p.lastEventId).toBe('7');
  });

  it('defaults the event name to message and strips one leading space only', () => {
    const events = new EventStreamParser().pushText('data:  two spaces\n\n');
    expect(events).toEqual([{ id: null, event: 'message', data: ' two spaces' }]);
  });

  it('is independent of chunk boundaries, including inside CRLF and a multi-byte character', () => {
    const wire = enc('event: x\r\ndata: héllo 世界\r\n\r\n');
    for (let cut = 1; cut < wire.length; cut += 1) {
      const p = new EventStreamParser();
      const got = [...p.push(wire.slice(0, cut)), ...p.push(wire.slice(cut)), ...p.end()];
      expect(got).toEqual([{ id: null, event: 'x', data: 'héllo 世界' }]);
    }
  });

  it('handles bare CR line endings and a CR split from its LF', () => {
    const p = new EventStreamParser();
    expect(p.pushText('data: a\r')).toEqual([]);
    expect(p.pushText('\n\r')).toEqual([{ id: null, event: 'message', data: 'a' }]);
  });

  it('ignores comments (heartbeats) and unknown fields', () => {
    const events = new EventStreamParser().pushText(': heartbeat\nfoo: bar\ndata: x\n\n');
    expect(events).toHaveLength(1);
  });

  it('drops a leading BOM once', () => {
    const events = new EventStreamParser().push(enc('﻿data: x\n\n'));
    expect(events).toEqual([{ id: null, event: 'message', data: 'x' }]);
  });

  it('an event with no data is not dispatched but its id still moves the cursor', () => {
    const p = new EventStreamParser();
    expect(p.pushText('id: 9\n\n')).toEqual([]);
    expect(p.lastEventId).toBe('9');
  });

  it('ignores an id containing NUL and a non-numeric retry', () => {
    const p = new EventStreamParser({ lastEventId: '1' });
    p.pushText('id: a\0b\nretry: soon\ndata: x\n\n');
    expect(p.lastEventId).toBe('1');
    expect(p.retryMs).toBeNull();
    p.pushText('retry: 1500\n\n');
    expect(p.retryMs).toBe(1500);
  });

  it('drops an oversized event, counts it, and keeps parsing the next one', () => {
    const p = new EventStreamParser({ maxEventChars: 32 });
    const events = p.pushText(`data: ${'x'.repeat(100)}\n\ndata: ok\n\n`);
    expect(events).toEqual([{ id: null, event: 'message', data: 'ok' }]);
    expect(p.oversized).toBe(1);
  });

  it('discards an unterminated final event at end of stream', () => {
    const p = new EventStreamParser();
    p.pushText('data: partial');
    expect(p.end()).toEqual([]);
  });
});
