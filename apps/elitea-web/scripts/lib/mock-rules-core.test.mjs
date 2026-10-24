import { describe, expect, it } from 'vitest';

import { checkHandlerSource } from './mock-rules-core.mjs';

describe('R-M2 — handler bodies are generated mocks or typed constants', () => {
  it('flags HttpResponse.json with an inline object literal', () => {
    const source = `
import { http, HttpResponse } from 'msw';
export const handlers: RequestHandler[] = [
  http.get('/api/v2/users', () => HttpResponse.json({ id: 1, name: 'inline' })),
];`;
    const findings = checkHandlerSource('handlers.ts', source);
    expect(findings).toHaveLength(1);
    expect(findings[0].rule).toBe('R-M2');
    expect(findings[0].message).toMatch(/inline literal/);
  });

  it('flags array literals and Response.json too', () => {
    const source = `
export const handlers: RequestHandler[] = [
  http.get('/api/v2/tags', () => Response.json([1, 2, 3])),
];`;
    expect(checkHandlerSource('handlers.ts', source)).toHaveLength(1);
  });

  it('passes a generated mock and a typed constant', () => {
    const source = `
import { http, HttpResponse } from 'msw';
import type { ErrorResponse } from '../../../shared/api/generated/model';
import { getListUsersResponseMock } from '../../../shared/api/generated/users/users.msw';
const NOT_FOUND: ErrorResponse = { error: 'not found' };
export const PAGE = { id: 1 } satisfies { id: number };
const HTML = '<html></html>';
let counter;
export function handlers() {
  return [
    http.get('/api/v2/users', () => HttpResponse.json(getListUsersResponseMock())),
    http.get('/api/v2/missing', () => HttpResponse.json(NOT_FOUND, { status: 404 })),
    http.get('/login', () => HttpResponse.html(HTML)),
  ];
}`;
    expect(checkHandlerSource('handlers.ts', source)).toEqual([]);
  });

  it('flags an untyped object or array constant used as a body, exported or not', () => {
    const source = `
const BODY = { id: 1 };
export const LIST = [1];
const [first] = [2];
const handlers = [http.get('/a', () => HttpResponse.json(BODY))];
export const more = [
  http.get('/b', () => HttpResponse.json(LIST[0])),
  http.get('/c', () => HttpResponse.json(first)),
  http.get('/d', () => HttpResponse.json(make().body)),
  http.get('/e', () => HttpResponse.json()),
];`;
    const findings = checkHandlerSource('handlers.ts', source);
    expect(findings.map((f) => f.line)).toEqual([2, 3]);
    expect(findings[0].message).toMatch(/body constant BODY has no type/);
  });

  it('flags a JSON fixture import', () => {
    const source = `import users from '../fixtures/users.list.200.json';\nexport { users };`;
    const findings = checkHandlerSource('handlers.ts', source);
    expect(findings).toHaveLength(1);
    expect(findings[0].message).toMatch(/JSON fixtures are retired/);
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
