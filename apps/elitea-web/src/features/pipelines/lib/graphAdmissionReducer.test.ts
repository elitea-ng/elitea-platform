/**
 * Typed state reducers are admitted in every build: a valid reducer is a non-blocking, YAML-only notice, and every
 * compiler refusal of a reducer declaration is mirrored as a save-blocking issue.
 */
import { describe, expect, it } from 'vitest';

import type { YamlPipelineDocument } from './flow-editor/helpers/pipelineFlow.types';
import { blockingIssues, collectGraphAdmissionIssues } from './graphAdmission.helpers';

const BASE = {
  state: { input: 'str', messages: 'list', summary: 'str' } as Record<string, unknown>,
  entry_point: 'LLM_1',
  nodes: [{ id: 'LLM_1', type: 'llm', input: ['messages'], output: ['summary'], transition: 'END' }],
};

/** Deliberately loose: the refusal cases carry shapes the YAML type does not allow (`reducer: 5`). */
function withState(extra: Record<string, unknown>): YamlPipelineDocument {
  return { ...BASE, state: { ...BASE.state, ...extra } } as YamlPipelineDocument;
}

describe('typed state reducers', () => {
  it('turns every valid reducer into a non-blocking notice', () => {
    const issues = collectGraphAdmissionIssues(
      withState({
        findings: { type: 'list', value: [], reducer: 'append' },
        total: { type: 'int', value: 0, reducer: 'sum_int' },
        seen: { type: 'dict', value: {}, reducer: 'merge' },
        plain: { type: 'str', reducer: 'overwrite' },
      }),
    );

    expect(issues.map((issue) => [issue.field, issue.subject, issue.severity])).toEqual([
      ['state.findings', 'append', 'warning'],
      ['state.total', 'sum_int', 'warning'],
      ['state.seen', 'merge', 'warning'],
      ['state.plain', 'overwrite', 'warning'],
    ]);
    expect(issues.every((issue) => issue.message.includes('YAML only'))).toBe(true);
    expect(blockingIssues(issues)).toEqual([]);
  });

  it('mirrors each compiler refusal of a reducer declaration', () => {
    const cases: readonly (readonly [Record<string, unknown>, string, string])[] = [
      [{ input: { type: 'str', reducer: 'overwrite' } }, 'compiler.rs:2780', 'cannot declare a reducer'],
      [{ findings: { type: 'list', reducer: 'extend' } }, 'compiler.rs:2784', 'must be one of'],
      [{ findings: { type: 'dict', reducer: 'append' } }, 'compiler.rs:2791', 'needs a "list" variable'],
      [{ total: { type: 'number', reducer: 'merge' } }, 'compiler.rs:2791', 'needs a "dict" variable'],
      [{ findings: { type: 'list', reducer: 5 } }, 'compiler.rs:2724', 'must be one of'],
    ];
    for (const [state, citation, message] of cases) {
      const blocking = blockingIssues(collectGraphAdmissionIssues(withState(state)));
      expect(blocking.map((issue) => issue.rule), citation).toEqual(['state.reducer']);
      expect(blocking[0]?.citation).toBe(citation);
      expect(blocking[0]?.message).toContain(message);
    }
  });

  it('treats `reducer: null` as no reducer, as the compiler does', () => {
    expect(collectGraphAdmissionIssues(withState({ findings: { type: 'list', reducer: null } }))).toEqual([]);
  });
});
