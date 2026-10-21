import { FIXTURE_NAMES, loadFixtureText } from './loadFixture';

describe('SSE fixtures are well-formed', () => {
  it.each(FIXTURE_NAMES)('%s: LF framing, increasing ids, one JSON object per data line', (name) => {
    const text = loadFixtureText(name);
    expect(text).not.toContain('\r');
    expect(text.endsWith('\n\n')).toBe(true);
    let last = 0;
    let events = 0;
    for (const block of text.split('\n\n').filter(Boolean)) {
      if (block.startsWith(':')) continue;
      const lines = block.split('\n');
      const id = Number(lines.find((l) => l.startsWith('id: '))?.slice(4));
      expect(id).toBeGreaterThan(last);
      last = id;
      expect(lines.some((l) => l.startsWith('event: '))).toBe(true);
      const data = lines.find((l) => l.startsWith('data: '))?.slice(6) ?? '';
      expect(typeof JSON.parse(data)).toBe('object');
      events += 1;
    }
    expect(events).toBeGreaterThan(0);
  });
});
