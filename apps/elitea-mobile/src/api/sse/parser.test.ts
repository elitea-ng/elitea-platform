import { EventStreamParser, type SseEvent } from '@elitea/client-core/stream';

import { FIXTURE_NAMES, loadFixtureBytes } from '@/features/chat/__fixtures__/loadFixture';

/**
 * The parser and its unit tests live in libs/ts/client-core (shared with the
 * desktop client). This file keeps the one check that needs this app's
 * recorded streams: every fixture parses the same whole and byte by byte.
 */

function parseAll(chunks: Uint8Array[], parser = new EventStreamParser()): SseEvent[] {
  const out: SseEvent[] = [];
  for (const chunk of chunks) out.push(...parser.push(chunk));
  out.push(...parser.end());
  return out;
}

describe('EventStreamParser on the recorded streams', () => {
  it.each(FIXTURE_NAMES)('parses fixture %s identically whole and byte-by-byte', (name) => {
    const bytes = loadFixtureBytes(name);
    const whole = parseAll([bytes]);
    const split = parseAll(Array.from(bytes, (b) => new Uint8Array([b])));
    expect(split).toEqual(whole);
    expect(whole.length).toBeGreaterThan(0);
    for (const e of whole) expect(() => JSON.parse(e.data)).not.toThrow();
  });
});
