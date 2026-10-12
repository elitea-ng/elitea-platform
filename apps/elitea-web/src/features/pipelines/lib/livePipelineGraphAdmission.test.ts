/**
 * The three states of the live document, judged on the STRING that gets
 * stored. `graphAdmission.helpers.test.ts` owns the rule catalogue; what is
 * asserted here is only what this module adds on top of it — the unseeded
 * exemption, the unparseable refusal, and the fact that a scalar or a comment
 * is neither of those by accident.
 */
import { describe, expect, it } from 'vitest';

import { judgeLivePipelineGraph } from './livePipelineGraphAdmission';

const ADMISSIBLE = [
  'state:',
  '  input: str',
  '  messages: list',
  '  summary: str',
  'entry_point: LLM_1',
  'nodes:',
  '  - id: LLM_1',
  '    type: llm',
  '    input: [messages]',
  '    output: [summary]',
  '    transition: END',
  '',
].join('\n');

describe('judgeLivePipelineGraph', () => {
  /*
   * `usePipelineGraphDraft` bails on exactly this string and stores nothing,
   * so gating on it would block saves that have nothing to do with the graph
   * — the editor simply has not been seeded yet.
   */
  it('exempts an unseeded editor rather than calling it an empty pipeline', () => {
    for (const blank of ['', '   ', '\n\n']) {
      const verdict = judgeLivePipelineGraph(blank);
      expect(verdict.hasGraph).toBe(false);
      expect(verdict.isAdmissible).toBe(true);
      expect(verdict.parseFailed).toBe(false);
    }
  });

  it('admits a graph the compiler would accept', () => {
    const verdict = judgeLivePipelineGraph(ADMISSIBLE);

    expect(verdict.isAdmissible).toBe(true);
    expect(verdict.issues).toEqual([]);
    expect(verdict.hasGraph).toBe(true);
  });

  it('admits a graph whose only issue is the YAML-only reducer notice', () => {
    const verdict = judgeLivePipelineGraph(ADMISSIBLE.replace('  summary: str\n', '  summary: str\n  findings: {type: list, value: [], reducer: append}\n'));

    expect(verdict.isAdmissible).toBe(true);
    expect(verdict.issues.map((issue) => [issue.rule, issue.severity])).toEqual([['state.reducer', 'warning']]);
  });

  it('refuses a graph the compiler would reject, and names the rule', () => {
    // The same document with the `summary` state key deleted: the LLM node
    // now writes nowhere.
    const verdict = judgeLivePipelineGraph(ADMISSIBLE.replace('  summary: str\n', ''));

    expect(verdict.isAdmissible).toBe(false);
    expect(verdict.issues.map((issue) => issue.rule)).toContain('node.state-reference');
  });

  /*
   * Reachable only from the Yaml tab, which is the one place the stored
   * string is edited directly. `serde_yaml` refuses before any transcribed
   * rule runs, so this is its own flag rather than a `GraphAdmissionIssue` —
   * `GraphAdmissionRuleId` is a closed union of rules that each cite a
   * compiler `file:line`, and there is no line to cite for "not YAML".
   */
  it('refuses text that is not YAML at all, with no issue to show for it', () => {
    const verdict = judgeLivePipelineGraph('nodes: [\n  - id: "unterminated');

    expect(verdict.parseFailed).toBe(true);
    expect(verdict.isAdmissible).toBe(false);
    expect(verdict.hasGraph).toBe(true);
    expect(verdict.issues).toEqual([]);
  });

  /**
   * A scalar or a list parses fine and is still not a document the compiler
   * could read a node out of. Without the shape check these fell through to
   * `collectGraphAdmissionIssues` as `Object.keys('text').length > 0` — a
   * string has indexed keys — which is a rule pass over a non-document.
   */
  it('refuses a document that parses to something other than a mapping', () => {
    for (const scalar of ['just some text', '- a\n- b', '42']) {
      expect(judgeLivePipelineGraph(scalar).parseFailed).toBe(true);
    }
  });

  /**
   * `load` answers `undefined` for a comment-only or explicitly-null
   * document. That is NOT the unseeded exemption: the editor holds text the
   * user typed, and `usePipelineGraphDraft` would store it (its bail-out is
   * `yamlCode.trim() === ''`, which this is not). The compiler cannot read a
   * node out of it, so it is refused.
   */
  it('refuses a comment-only document, which the save path would otherwise store', () => {
    expect(judgeLivePipelineGraph('# nothing here yet\n').parseFailed).toBe(true);
    expect(judgeLivePipelineGraph('# nothing here yet\n').isAdmissible).toBe(false);
  });
});
