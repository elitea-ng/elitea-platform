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

  it('follows every name a body reads: casts, conditionals, spreads, calls', () => {
    const source = `
const A = { a: 1 };
const B = [1];
const C = { c: 1 };
const D = { d: 1 };
const E = { e: 1 };
const KEYED = { k: 1 };
const notRead = { n: 1 };
let unset;
export const handlers: RequestHandler[] = [
  http.get('/a', () => HttpResponse.json(A as Foo)),
  http.get('/b', ({ request }) => HttpResponse.json(request.ok ? B : [])),
  http.get('/c', () => HttpResponse.json({ ...C })),
  http.get('/d', () => HttpResponse.json(wrap(D))),
  http.get('/e', () => HttpResponse.json(table[E.e].name)),
  http.get('/f', () => HttpResponse.json({ [KEYED.k]: 1, notRead: 2 }.x)),
  http.get('/g', () => HttpResponse.json(unset)),
];`;
    const lines = checkHandlerSource('handlers.ts', source).map((f) => f.line);
    // A..KEYED flagged (2-7); notRead is only an object key / property name;
    // /c's spread is an inline literal body (13).
    expect(lines.filter((l) => l < 10)).toEqual([2, 3, 4, 5, 6, 7]);
    expect(lines.filter((l) => l >= 10)).toEqual([13]);
  });

  it('rejects loose annotations and double casts, at module level and inside functions', () => {
    const source = `
const ANY: any = { a: 1 };
const UNKNOWN: unknown = { a: 1 };
const OBJ: object = { a: 1 };
const DOUBLE = { a: 1 } as unknown as User;
const SINGLE = { a: 1 } as User;
const TYPED: User = { a: 1 };
export function make(): RequestHandler {
  const inner = { a: 1 };
  return http.get('/a', () => HttpResponse.json(pick(ANY, UNKNOWN, OBJ, DOUBLE, SINGLE, TYPED, inner)));
}`;
    const findings = checkHandlerSource('handlers.ts', source);
    expect(findings.map((f) => f.line)).toEqual([2, 3, 4, 5, 6, 9]);
    expect(findings[0].message).toMatch(/typed `any`/);
    expect(findings[1].message).toMatch(/typed `unknown`/);
    expect(findings[2].message).toMatch(/typed `object`/);
    expect(findings[3].message).toMatch(/double cast/);
    expect(findings[4].message).toMatch(/has no type/);
    expect(findings[5].message).toMatch(/body constant inner has no type/);
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
