/**
 * Decision-logic tests for stale-disclosure-core.mjs (issue 621).
 *
 * ci-web.yml holds scripts/lib/**\/*.mjs to 100% statements, branches,
 * functions and lines, so every rule below is exercised in both directions.
 */
import { describe, expect, it } from 'vitest';

import {
  CLAIM_MARKERS,
  DEFAULT_WINDOW,
  PROBE_VERBS,
  attachProbes,
  evaluateFile,
  findClaims,
  findProbes,
  literalOccurs,
} from './stale-disclosure-core.mjs';

const found = () => ({ matched: true, filesSearched: 3 });
const notFound = () => ({ matched: false, filesSearched: 3 });

describe('literalOccurs', () => {
  it('finds a literal on an ordinary line', () => {
    expect(literalOccurs('a\nkpis["tool_runs"] = 1\nb', 'kpis["tool_runs"]')).toBe(true);
  });

  it('does not read a probe line as evidence for itself', () => {
    const text = '// DISCLOSURE-CHECK: absent `kpis["tool_runs"]` in services\nnothing else\n';
    expect(literalOccurs(text, 'kpis["tool_runs"]')).toBe(false);
  });

  it('reports absence', () => {
    expect(literalOccurs('nothing here', 'kpis["tool_runs"]')).toBe(false);
  });
});

describe('findClaims', () => {
  it('names each marker', () => {
    const text = ['// NOT PORTED yet', '// NO PRODUCER for this', '// never emitted today', '// ordinary prose'].join(
      '\n',
    );
    expect(findClaims(text)).toEqual([
      { line: 1, marker: 'not-ported', text: '// NOT PORTED yet' },
      { line: 2, marker: 'no-producer', text: '// NO PRODUCER for this' },
      { line: 3, marker: 'never-emitted', text: '// never emitted today' },
    ]);
  });

  it('matches the lower-case Go form only behind a dash', () => {
    expect(findClaims('//\ttool_runs ABSENT — no producer.')).toHaveLength(1);
    expect(findClaims('// a figure with no producer is ABSENT rather than present')).toHaveLength(0);
  });

  it('never reads a probe line as a claim', () => {
    expect(findClaims('# DISCLOSURE-CHECK: absent `never emitted` in services')).toEqual([]);
  });

  it('exposes its marker table', () => {
    expect(CLAIM_MARKERS.map((m) => m.id)).toEqual(['not-ported', 'no-producer', 'never-emitted']);
  });
});

describe('findProbes', () => {
  it('parses a well-formed probe', () => {
    const text = '  # DISCLOSURE-CHECK: absent `kpis["x"]` in services/elitea-main/internal';
    expect(findProbes(text)).toEqual([
      {
        line: 1,
        raw: '# DISCLOSURE-CHECK: absent `kpis["x"]` in services/elitea-main/internal',
        verb: 'absent',
        literal: 'kpis["x"]',
        target: 'services/elitea-main/internal',
        error: null,
      },
    ]);
  });

  it('keeps an unparsable probe, with its reason', () => {
    const probes = findProbes('# DISCLOSURE-CHECK: absent kpis in services');
    expect(probes).toHaveLength(1);
    expect(probes[0].error).toMatch(/does not parse/);
  });

  it('rejects a verb it does not know', () => {
    const probes = findProbes('# DISCLOSURE-CHECK: maybe `x` in services');
    expect(probes[0].error).toMatch(/unknown verb "maybe"/);
  });

  it('ignores a line with no tag', () => {
    expect(findProbes('nothing here\nnor here')).toEqual([]);
  });

  it('exposes its verbs', () => {
    expect(PROBE_VERBS).toEqual(['absent', 'present']);
  });
});

describe('attachProbes', () => {
  const probe = (line) => ({ line, raw: 'p', verb: 'absent', literal: 'x', target: 't', error: null });

  it('attaches a probe to a claim below it, inside the window', () => {
    const claims = [{ line: 5, marker: 'not-ported', text: 'c' }];
    const { pairs, orphans } = attachProbes(claims, [probe(3)], 15);
    expect(pairs[0].probes).toHaveLength(1);
    expect(orphans).toEqual([]);
  });

  it('does not reach past the window, and the probe is then an orphan', () => {
    const claims = [{ line: 40, marker: 'not-ported', text: 'c' }];
    const { pairs, orphans } = attachProbes(claims, [probe(3)], 15);
    expect(pairs[0].probes).toEqual([]);
    expect(orphans).toHaveLength(1);
  });

  it('never attaches upward', () => {
    const claims = [{ line: 2, marker: 'not-ported', text: 'c' }];
    const { pairs } = attachProbes(claims, [probe(9)], 15);
    expect(pairs[0].probes).toEqual([]);
  });

  it('ignores a probe that did not parse', () => {
    const claims = [{ line: 5, marker: 'not-ported', text: 'c' }];
    const broken = { line: 3, raw: 'p', error: 'nope' };
    const { pairs, orphans } = attachProbes(claims, [broken], 15);
    expect(pairs[0].probes).toEqual([]);
    expect(orphans).toEqual([]);
  });

  it('defaults its window', () => {
    const claims = [{ line: 1 + DEFAULT_WINDOW, marker: 'not-ported', text: 'c' }];
    expect(attachProbes(claims, [probe(1)]).pairs[0].probes).toHaveLength(1);
  });
});

describe('evaluateFile', () => {
  it('passes a claim whose absent probe finds nothing', () => {
    const text = ['# DISCLOSURE-CHECK: absent `kpis["x"]` in services', '# NO PRODUCER for x.'].join('\n');
    const result = evaluateFile({ file: 'f', text, runProbe: notFound });
    expect(result).toEqual({ violations: [], claims: 1, probes: 1 });
  });

  it('passes a claim whose present probe finds its evidence', () => {
    const text = ['# DISCLOSURE-CHECK: present `INSERT INTO t` in services', '# never emitted today'].join('\n');
    expect(evaluateFile({ file: 'f', text, runProbe: found }).violations).toEqual([]);
  });

  it('fails an absent probe whose literal now exists', () => {
    const text = ['# DISCLOSURE-CHECK: absent `kpis["x"]` in services', '# NO PRODUCER for x.'].join('\n');
    const [violation] = evaluateFile({ file: 'f', text, runProbe: found }).violations;
    expect(violation.kind).toBe('stale');
    expect(violation.line).toBe(2);
    expect(violation.message).toMatch(/now exists under services/);
  });

  it('fails a present probe whose evidence is gone', () => {
    const text = ['# DISCLOSURE-CHECK: present `INSERT INTO t` in services', '# never emitted today'].join('\n');
    const [violation] = evaluateFile({ file: 'f', text, runProbe: notFound }).violations;
    expect(violation.kind).toBe('stale');
    expect(violation.message).toMatch(/not there any more/);
  });

  it('fails a claim with no probe', () => {
    const [violation] = evaluateFile({ file: 'f', text: '# NOT PORTED.', runProbe: found }).violations;
    expect(violation).toMatchObject({ kind: 'unverified', line: 1 });
  });

  it('fails a probe that covers no claim', () => {
    const text = '# DISCLOSURE-CHECK: absent `x` in services\n# ordinary prose';
    const [violation] = evaluateFile({ file: 'f', text, runProbe: notFound }).violations;
    expect(violation).toMatchObject({ kind: 'orphan', line: 1 });
  });

  it('fails a probe whose path matches no file', () => {
    const text = ['# DISCLOSURE-CHECK: absent `x` in gone/', '# NO PRODUCER for x.'].join('\n');
    const kinds = evaluateFile({
      file: 'f',
      text,
      runProbe: () => ({ matched: false, filesSearched: 0 }),
    }).violations.map((v) => v.kind);
    expect(kinds).toEqual(['blind']);
  });

  it('fails a malformed probe and still judges the claim beside it', () => {
    const text = ['# DISCLOSURE-CHECK: absent x in services', '# NOT PORTED.'].join('\n');
    const kinds = evaluateFile({ file: 'f', text, runProbe: found }).violations.map((v) => v.kind);
    expect(kinds).toEqual(['malformed', 'unverified']);
  });

  it('runs one probe once, however many claims it covers', () => {
    const text = [
      '# DISCLOSURE-CHECK: absent `x` in services',
      '# NO PRODUCER for x.',
      '# never emitted today',
    ].join('\n');
    let calls = 0;
    const runProbe = () => {
      calls += 1;
      return { matched: false, filesSearched: 1 };
    };
    expect(evaluateFile({ file: 'f', text, runProbe, window: 15 }).violations).toEqual([]);
    expect(calls).toBe(1);
  });
});
