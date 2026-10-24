import { describe, expect, it } from 'vitest';

import { hooksText, parseHooks } from './hooksText';

describe('parseHooks', () => {
  it('reads back what it wrote', () => {
    const hooks = [{ id: 'fmt', event: 'PostToolUse' as const, command: ['cargo', 'fmt'] }];
    expect(parseHooks(hooksText(hooks))).toEqual({ ok: true, hooks });
  });

  it('names the first problem', () => {
    expect(parseHooks('{').ok).toBe(false);
    expect(parseHooks('{}')).toEqual({ ok: false, problem: 'the hooks are a JSON array' });
    expect(parseHooks('[{"id":"a b","event":"Stop","command":["x"]}]')).toMatchObject({ ok: false });
    expect(parseHooks('[{"id":"a","event":"Nope","command":["x"]}]')).toMatchObject({ ok: false, problem: expect.stringContaining('event') as unknown });
    expect(parseHooks('[{"id":"a","event":"Stop","command":[]}]')).toMatchObject({ ok: false });
    expect(parseHooks('[{"id":"a","event":"Stop","command":["x"]},{"id":"a","event":"Stop","command":["x"]}]')).toMatchObject({
      ok: false,
      problem: expect.stringContaining('twice') as unknown,
    });
  });
});
