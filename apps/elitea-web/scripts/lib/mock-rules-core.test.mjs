import { describe, expect, it } from 'vitest';

import { checkHandlerSource } from './mock-rules-core.mjs';

describe('R-M2 — no inline handler bodies', () => {
  it('flags HttpResponse.json with an inline object literal', () => {
    const source = `
import { http, HttpResponse } from 'msw';
export const handlers = [
  http.get('/api/v2/users', () => HttpResponse.json({ id: 1, name: 'inline' })),
];`;
    const findings = checkHandlerSource('handlers.ts', source);
    expect(findings).toHaveLength(1);
    expect(findings[0].rule).toBe('R-M2');
  });

  it('flags array literals and Response.json too', () => {
    const source = `
export const handlers = [
  http.get('/api/v2/tags', () => Response.json([1, 2, 3])),
];`;
    expect(checkHandlerSource('handlers.ts', source)).toHaveLength(1);
  });

  it('passes fixture-derived bodies', () => {
    const source = `
import { http, HttpResponse } from 'msw';
import usersFixture from '../fixtures/users.list.200.json';
export const handlers = [
  http.get('/api/v2/users', () => HttpResponse.json(usersFixture.body)),
];`;
    expect(checkHandlerSource('handlers.ts', source)).toEqual([]);
  });

  it('ignores unrelated .json() calls', () => {
    const source = 'export async function read(r: Response) { return r.json(); }';
    expect(checkHandlerSource('read.ts', source)).toEqual([]);
  });

  it('parses .tsx handler modules (jsx plugin branch)', () => {
    const source = `
import { HttpResponse } from 'msw';
export const preview = <div />;
export const bad = () => HttpResponse.json({ inline: true });`;
    expect(checkHandlerSource('handlers.tsx', source)).toHaveLength(1);
  });
});
