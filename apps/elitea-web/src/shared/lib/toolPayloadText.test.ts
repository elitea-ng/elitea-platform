import { describe, expect, it } from 'vitest';

import { toolPayloadText } from './toolPayloadText';

describe('toolPayloadText', () => {
  it('shows a string payload exactly as the server sent it', () => {
    // THE DEFECT (#990 review 6): the run-history trace used to pretty-print
    // by parsing and re-serialising, which cannot represent an identifier
    // beyond 2^53 — `9007199254740993` came back as `9007199254740992`, and
    // `1e400` as `null`. The chat pane showed the same row verbatim, so the
    // two views of ONE value disagreed about what the tool returned.
    const payload = '{"id": 9007199254740993, "ratio": 1e400}';
    expect(toolPayloadText(payload)).toBe(payload);
    expect(JSON.stringify(JSON.parse(payload))).not.toBe(payload);
  });

  it('pretty-prints a structure the client already holds', () => {
    expect(toolPayloadText({ title: 'A bug' })).toBe('{\n  "title": "A bug"\n}');
  });

  it('renders an absent payload as nothing', () => {
    expect(toolPayloadText(undefined)).toBe('');
    expect(toolPayloadText(null)).toBe('');
  });
});
