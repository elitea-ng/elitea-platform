/**
 * `unwrapList` / `unwrapListPage` — the one sanctioned response-envelope
 * unwrap (R-A6, issue #132).
 *
 * Each of the three body shapes asserted here was measured against the running
 * stack in the bug hunt that produced #132, and each was the shape some call
 * site got wrong: `{rows,total}` (empty members table), a bare array (roles
 * dropdown empty → invite impossible for every user), and `{items,total,…}`
 * (envelope handed to ChatBox → TypeError on every `/app/chat/:id` deep link).
 */
import { afterEach, describe, expect, it, vi } from 'vitest';

import { unwrapBody, unwrapList, unwrapListPage } from './unwrap';

interface Row {
  readonly id: string;
}

const ROWS: Row[] = [{ id: 'a' }, { id: 'b' }];

/** What `eliteaFetch` actually resolves — see `shared/api/generated/mutator.ts`. */
function envelope(body: unknown): unknown {
  return { data: body, status: 200, headers: new Headers() };
}

afterEach(() => {
  vi.unstubAllEnvs();
  vi.restoreAllMocks();
});

describe('unwrapList', () => {
  it('reads a {rows,total} body', () => {
    expect(unwrapList<Row>({ rows: ROWS, total: 2 }, 'userList')).toStrictEqual(ROWS);
  });

  it('reads an {items,total,page,page_size,total_pages} body', () => {
    const body = { items: ROWS, total: 2, page: 1, page_size: 10, total_pages: 1 };
    expect(unwrapList<Row>(body, 'conversation.messageList')).toStrictEqual(ROWS);
  });

  it('reads a bare-array body', () => {
    expect(unwrapList<Row>(ROWS, 'roleList')).toStrictEqual(ROWS);
  });

  it('reads the same three shapes through the {data,status,headers} transport envelope', () => {
    expect(unwrapList<Row>(envelope({ rows: ROWS }), 'userList')).toStrictEqual(ROWS);
    expect(unwrapList<Row>(envelope({ items: ROWS }), 'messageList')).toStrictEqual(ROWS);
    expect(unwrapList<Row>(envelope(ROWS), 'roleList')).toStrictEqual(ROWS);
  });

  it('returns [] — silently — for a query that has not resolved yet', () => {
    expect(unwrapList<Row>(undefined, 'userList')).toStrictEqual([]);
    expect(unwrapList<Row>(null, 'userList')).toStrictEqual([]);
    expect(unwrapList<Row>(envelope(null), 'userList')).toStrictEqual([]);
  });

  it('returns a fresh array, so an in-place .reverse()/.sort() at a call site cannot mutate the cache', () => {
    // Asserted against a LITERAL expected order, not against `ROWS`: if the
    // helper handed back the caller's own array, `out.reverse()` would mutate
    // the fixture too and an `expect(body.rows).toStrictEqual(ROWS)` would
    // compare the mutated array with itself and pass. (a transcript pager
    // reverses the result in place.)
    const paged = { rows: [{ id: 'a' }, { id: 'b' }] };
    unwrapList<Row>(paged, 'userList').reverse();
    expect(paged.rows.map((r) => r.id)).toStrictEqual(['a', 'b']);

    const bare = [{ id: 'a' }, { id: 'b' }];
    unwrapList<Row>(bare, 'roleList').reverse();
    expect(bare.map((r) => r.id)).toStrictEqual(['a', 'b']);
  });
});

describe('unwrapListPage', () => {
  it("carries the server's total when the body has one", () => {
    expect(unwrapListPage<Row>({ rows: ROWS, total: 57 }, 'userList')).toStrictEqual({ rows: ROWS, total: 57 });
    expect(unwrapListPage<Row>({ items: ROWS, total: 57 }, 'messageList')).toStrictEqual({ rows: ROWS, total: 57 });
  });

  it('falls back to the page length when the body carries no numeric total', () => {
    expect(unwrapListPage<Row>({ rows: ROWS }, 'userList').total).toBe(2);
    expect(unwrapListPage<Row>({ rows: ROWS, total: 'many' }, 'userList').total).toBe(2);
    expect(unwrapListPage<Row>(ROWS, 'roleList').total).toBe(2);
  });

  it('is 0/[] for an unresolved query', () => {
    expect(unwrapListPage<Row>(undefined, 'userList')).toStrictEqual({ rows: [], total: 0 });
  });
});

describe('an unrecognised shape', () => {
  it('throws under DEV so the next new envelope surfaces immediately', () => {
    vi.stubEnv('DEV', true);
    expect(() => unwrapList({ results: ROWS, count: 2 }, 'someNewEndpoint')).toThrow(TypeError);
    expect(() => unwrapList({ results: ROWS }, 'someNewEndpoint')).toThrow(/unrecognised list response shape/);
  });

  it('names the call site and the keys it actually got', () => {
    vi.stubEnv('DEV', true);
    expect(() => unwrapList({ results: ROWS, count: 2 }, 'someNewEndpoint')).toThrow(/someNewEndpoint/);
    expect(() => unwrapList({ results: ROWS, count: 2 }, 'someNewEndpoint')).toThrow(/keys \[results, count\]/);
  });

  it('describes the BODY, not the transport envelope wrapped around it', () => {
    vi.stubEnv('DEV', true);
    expect(() => unwrapList(envelope({ results: ROWS }), 'someNewEndpoint')).toThrow(/keys \[results\]/);
  });

  it('logs and degrades to [] outside DEV — a broken list, never a white screen', () => {
    vi.stubEnv('DEV', false);
    const error = vi.spyOn(console, 'error').mockImplementation(() => {});

    expect(unwrapList({ results: ROWS }, 'someNewEndpoint')).toStrictEqual([]);
    expect(error).toHaveBeenCalledTimes(1);
    expect(error.mock.calls[0]?.[0]).toMatch(/unrecognised list response shape/);
  });

  /*
   * The direction of the fallback is the whole point of the helper.
   * `useChatPageData` used to fall back to the RESPONSE OBJECT when no branch
   * matched; that unrecognised shape became the data, was spread by
   * convertMessagesToChatHistory, and threw — turning a quiet empty state into
   * a crash on every `/app/chat/:id` deep link. `[]` is the only legal
   * fallback, in both the throwing and the logging mode.
   */
  it('never falls back to the input, in either mode', () => {
    const weird = { results: ROWS };

    vi.stubEnv('DEV', false);
    vi.spyOn(console, 'error').mockImplementation(() => {});
    const degraded = unwrapList(weird, 'someNewEndpoint');
    expect(degraded).toStrictEqual([]);
    expect(degraded).not.toBe(weird);

    vi.stubEnv('DEV', true);
    try {
      unwrapList(weird, 'someNewEndpoint');
      expect.unreachable('an unrecognised shape must not resolve to a value under DEV');
    } catch (thrown) {
      expect(thrown).toBeInstanceOf(TypeError);
    }
  });

  it('treats a doubled {data:{…}} body as unrecognised rather than peeling twice', () => {
    vi.stubEnv('DEV', true);
    expect(() => unwrapList(envelope({ data: { rows: ROWS } }), 'doubled')).toThrow(/keys \[data\]/);
  });

  it('rejects a rows/items key that is not an array', () => {
    vi.stubEnv('DEV', true);
    expect(() => unwrapList({ rows: 'nope' }, 'userList')).toThrow(/unrecognised list response shape/);
  });

  it('reports the type for a scalar body', () => {
    vi.stubEnv('DEV', true);
    expect(() => unwrapList('not a list', 'userList')).toThrow(/got string/);
  });
});

/**
 * `unwrapBody` (unit A14) — the transport peel on its own, for bodies carrying
 * a sibling the rows do not describe (`/admin/auth_users`'s `counts`). Without
 * it such a call site re-derives `resp.data` by hand, which is the envelope
 * knowledge R-A6 keeps out of call sites.
 */
describe('unwrapBody', () => {
  it('peels the transport envelope and returns the body', () => {
    const body = { rows: ROWS, total: 2, counts: { platform: 2, system: 1 } };
    expect(unwrapBody(envelope(body))).toBe(body);
  });

  it('returns a bare body unchanged — "too shallow" is not expressible either', () => {
    const body = { rows: ROWS, total: 2 };
    expect(unwrapBody(body)).toBe(body);
  });

  it('peels exactly once, so a doubly-enveloped response yields the INNER envelope', () => {
    const body = { rows: ROWS };
    const inner = envelope(body);
    // Same rule as unwrapListPage: peeling recursively would let a body that
    // happens to look like an envelope be silently accepted, and this is the
    // input where the two readings differ — `{data:{data:…}}` alone does not,
    // because the intermediate object is not a full transport envelope.
    expect(unwrapBody(envelope(inner))).toBe(inner);
    expect(unwrapBody(envelope(inner))).not.toBe(body);
  });

  it('passes an unresolved query and non-objects straight through', () => {
    expect(unwrapBody(undefined)).toBeUndefined();
    expect(unwrapBody(null)).toBeNull();
    expect(unwrapBody('scalar')).toBe('scalar');
    expect(unwrapBody(ROWS)).toBe(ROWS);
  });

  it('does not peel an object that merely HAS a data key', () => {
    // Only the full {data,status,headers} triple is the transport envelope.
    const body = { data: 'payload' };
    expect(unwrapBody(body)).toBe(body);
  });
});
