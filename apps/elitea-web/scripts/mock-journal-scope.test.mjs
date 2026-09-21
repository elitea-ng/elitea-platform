/**
 * The model-journal scope guard (`scripts/lib/mock-journal-scope.mjs`).
 *
 * The rule it enforces is about an INVOCATION, not about the product, which is
 * exactly why it is worth pinning here: the bug it catches produced three
 * green-looking specs that failed with product-shaped messages, and the only
 * thing wrong was a missing `STANDALONE_MOCK_PORT` on the Playwright process.
 *
 * Tested as a pure function so no stack, browser or Playwright is involved.
 */
import { describe, expect, it } from 'vitest';

import {
  mockLlmJournalScopeFailure,
  mockProjectCredential,
} from './lib/mock-journal-scope.mjs';

const HOST = 'http://localhost:8090';

/** One journal entry, with only the field the rule reads. */
function entry(credential) {
  return { path: '/v1/chat/completions', credential };
}

describe('mockProjectCredential', () => {
  it('is the label the mock records a seeded project key under', () => {
    expect(mockProjectCredential(90107)).toBe('mock-key-project-90107');
    // A string id must produce the same label — the caller may hold either,
    // and a mismatch here would make the guard fire on a correct journal.
    expect(mockProjectCredential('90107')).toBe('mock-key-project-90107');
  });
});

describe('mockLlmJournalScopeFailure', () => {
  /*
   * ADDITIVE. Every reader that predates the guard passes no project and must
   * keep working unchanged — including against a journal that is genuinely
   * empty, which is a legitimate state for a reader asserting an ABSENCE.
   */
  it('checks nothing when the caller names no project', () => {
    for (const projectId of [undefined, null, '']) {
      expect(mockLlmJournalScopeFailure({ entries: [], projectId, host: HOST })).toBeUndefined();
    }
  });

  it('passes a journal holding a request for the project under test', () => {
    const entries = [entry('mock-key-project-1'), entry('mock-key-project-90107')];
    expect(mockLlmJournalScopeFailure({ entries, projectId: 90107, host: HOST })).toBeUndefined();
    expect(mockLlmJournalScopeFailure({ entries, projectId: '90107', host: HOST })).toBeUndefined();
  });

  /*
   * THE MEASURED CASE. A run against a stack on :8088 whose invocation forgot
   * STANDALONE_MOCK_PORT read :8090 — another stack's mock, busy with another
   * project. The journal is non-empty and entirely foreign, which is precisely
   * why the old code could not tell it apart from "the product did nothing".
   */
  it('refuses a journal that holds only another project’s traffic', () => {
    const failure = mockLlmJournalScopeFailure({
      entries: [entry('mock-key-project-1'), entry('mock-key-project-2')],
      projectId: 90107,
      host: HOST,
    });
    expect(failure).toBeDefined();
    // The three things a reader needs to act: which project was expected,
    // which host was actually read, and the variable that selects it.
    expect(failure).toContain('mock-key-project-90107');
    expect(failure).toContain(HOST);
    expect(failure).toContain('STANDALONE_MOCK_PORT');
    // And what was really there — a count would not have named the culprit.
    expect(failure).toContain('mock-key-project-1');
  });

  it('refuses an empty journal once a project is named', () => {
    const failure = mockLlmJournalScopeFailure({ entries: [], projectId: 90107, host: HOST });
    expect(failure).toBeDefined();
    expect(failure).toContain('<empty>');
  });

  /*
   * An entry with no credential at all (an unauthenticated probe) must not be
   * read as a match, and must not crash the summary either.
   */
  it('treats an entry with no credential as not the project’s', () => {
    const failure = mockLlmJournalScopeFailure({
      entries: [{ path: '/healthz' }],
      projectId: 90107,
      host: HOST,
    });
    expect(failure).toBeDefined();
    expect(failure).toContain('none');
  });

  /*
   * A near-miss must fail. `mock-key-project-9` is a PREFIX of
   * `mock-key-project-90107`, so a substring test here would pass a journal
   * belonging to a different project — the exact failure the guard exists to
   * catch, reintroduced one layer down.
   */
  it('does not accept a project whose id merely prefixes the one under test', () => {
    expect(
      mockLlmJournalScopeFailure({
        entries: [entry('mock-key-project-9')],
        projectId: 90107,
        host: HOST,
      }),
    ).toBeDefined();
    expect(
      mockLlmJournalScopeFailure({
        entries: [entry('mock-key-project-90107')],
        projectId: 9,
        host: HOST,
      }),
    ).toBeDefined();
  });

  /*
   * The summary is bounded: a journal holding hundreds of distinct credentials
   * must not paste all of them into a failure message.
   */
  it('names at most five distinct credentials in the summary', () => {
    const entries = Array.from({ length: 40 }, (_, index) => entry(`mock-key-project-${index}`));
    const failure = mockLlmJournalScopeFailure({ entries, projectId: 90107, host: HOST });
    expect(failure).toBeDefined();
    // Only the LIST, not the prose that follows it — the explanation after the
    // closing parenthesis carries commas of its own.
    const start = failure.indexOf('it holds: ') + 'it holds: '.length;
    const listed = failure.slice(start, failure.indexOf(')', start)).split(', ');
    expect(listed).toHaveLength(5);
  });
});
