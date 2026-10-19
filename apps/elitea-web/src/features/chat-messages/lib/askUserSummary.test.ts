/**
 * `findAskUserAction` / `buildAskUserSummary` — ported from the baseline's
 * `hitl.helpers.js`. The baseline ships no unit tests for these two, so the
 * cases below pin the baseline's behaviour as read from its source.
 */
import { describe, expect, it } from 'vitest';

import { buildAskUserSummary, findAskUserAction } from './askUserSummary';

describe('findAskUserAction', () => {
  it('returns the first COMPLETED ask_user action', () => {
    const running = { name: 'ask_user', status: 'processing' };
    const done = { name: 'ask_user', status: 'complete', id: 'a' };
    const other = { name: 'search', status: 'complete' };
    expect(findAskUserAction([other, running, done])).toBe(done);
  });

  it('returns null for a non-array or when none matches', () => {
    expect(findAskUserAction(undefined)).toBeNull();
    expect(findAskUserAction(null)).toBeNull();
    expect(findAskUserAction([{ name: 'ask_user', status: 'error' }])).toBeNull();
  });
});

describe('buildAskUserSummary', () => {
  const output = 'User answered:\n- Which environment?: Staging\n- Notes: a: b';

  it('matches each question spec to its answer line by prefix', () => {
    const inputs = { questions: [{ question: 'Which environment?' }, { question: 'Notes' }] };
    expect(buildAskUserSummary(inputs, output)).toEqual([
      { question: 'Which environment?', answer: 'Staging' },
      { question: 'Notes', answer: 'a: b' },
    ]);
  });

  it('accepts a JSON string or a bare array of questions, and falls back to header then id', () => {
    const asString = JSON.stringify({ questions: [{ header: 'Which environment?' }] });
    expect(buildAskUserSummary(asString, output)).toEqual([{ question: 'Which environment?', answer: 'Staging' }]);
    expect(buildAskUserSummary([{ id: 'Notes' }], output)).toEqual([{ question: 'Notes', answer: 'a: b' }]);
  });

  it('keeps an unanswered question with an empty answer and drops label-less specs', () => {
    expect(buildAskUserSummary({ questions: [{ question: 'Missing?' }, {}] }, output)).toEqual([
      { question: 'Missing?', answer: '' },
    ]);
  });

  it('parses the answer lines alone when the question specs are unavailable', () => {
    expect(buildAskUserSummary('not json', output)).toEqual([
      { question: 'Which environment?', answer: 'Staging' },
      { question: 'Notes', answer: 'a: b' },
    ]);
  });

  it('returns null when there is nothing to summarise', () => {
    expect(buildAskUserSummary(undefined, undefined)).toBeNull();
    expect(buildAskUserSummary({ questions: [{}] }, output)).toBeNull();
    expect(buildAskUserSummary(null, { not: 'a string' })).toBeNull();
    expect(buildAskUserSummary(null, 'User answered:\n- no separator')).toBeNull();
  });
});

describe('askUserSummary on the native runtime shapes (pm1084 browser run)', () => {
  it('reads a JSON-encoded tool output string', () => {
    const output = JSON.stringify('User answered:\n- Which region do you want?: EU');
    expect(buildAskUserSummary({ questions: [{ id: 'region', question: 'Which region do you want?' }] }, output)).toEqual([
      { question: 'Which region do you want?', answer: 'EU' },
    ]);
  });

  it('ignores the paused call and picks the answered one', () => {
    const paused = { name: 'ask_user', status: 'complete', toolMeta: { pause: { interrupt_id: 'i1' } } };
    const answered = { name: 'ask_user', status: 'complete', toolMeta: {} };
    expect(findAskUserAction([paused])).toBeNull();
    expect(findAskUserAction([paused, answered])).toBe(answered);
  });
});
