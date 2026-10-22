import { describe, expect, it } from 'vitest';

import { checkImmutability, mergeManifest, unknownKeys, validateManifest } from './parity-manifest-core.mjs';

/** Every decision of the parity-manifest gate, RED and GREEN (the scripts/lib
 * 100% decision-logic floor). */

const header = () => ({
  $schema: './manifest.schema.json',
  version: 1,
  generatedFrom: { repo: 'apps/elitea-ui', commit: 'a'.repeat(40), date: '2026-08-28' },
});

const item = (overrides = {}) => ({
  id: 'ROUTE-001',
  domain: 'chat',
  kind: 'route',
  priority: 'must',
  title: 'Route renders the chat',
  source: ['apps/elitea-ui/src/routes.js:6', 'apps/deepwiki-ui/src/a.js:1-3'],
  acceptance: ['GIVEN a user', 'WHEN they open /chat', 'THEN the chat shows'],
  verify: { type: 'command', command: 'node x', testId: 'ROUTE-001' },
  unit: 'R1',
  status: 'todo',
  coverage: { file: 'src/app/router.tsx', min: 90 },
  waiver: null,
  ...overrides,
});

const waiver = { reason: 'retired', decidedBy: 'owner', date: '2026-09-01', replacesBehaviour: 'nothing' };

describe('unknownKeys', () => {
  it('names keys the kind does not allow and ignores non-objects', () => {
    expect(unknownKeys({ id: 1, extra: 2 }, 'verify', 'X')).toEqual(['X: unknown field "id"', 'X: unknown field "extra"']);
    expect(unknownKeys({ type: 'vitest' }, 'verify', 'X')).toEqual([]);
    expect(unknownKeys(null, 'verify', 'X')).toEqual([]);
    expect(unknownKeys(['a'], 'verify', 'X')).toEqual([]);
  });
});

describe('mergeManifest', () => {
  it('merges shards that agree with the index', () => {
    const root = { ...header(), shards: [{ path: 'manifest/chat.json', domain: 'chat', items: 1 }] };
    const result = mergeManifest(root, () => ({ $schema: 'x', domain: 'chat', items: [item()] }));
    expect(result.problems).toEqual([]);
    expect(result.items).toHaveLength(1);
  });

  it('accepts a monolithic manifest', () => {
    const result = mergeManifest({ ...header(), items: [item()] }, () => null);
    expect(result).toEqual({ items: [item()], problems: [] });
  });

  it('reports every bookkeeping disagreement', () => {
    const root = {
      ...header(),
      generatedFrom: { ...header().generatedFrom, branch: 'x' },
      bogus: true,
      items: [item()],
      shards: [
        { path: 'manifest/missing.json', domain: 'chat', items: 1 },
        { path: 'manifest/chat.json', domain: 'chat', items: 3, extra: 1 },
        { path: 'manifest/odd.json', domain: 'chat', items: 0 },
      ],
    };
    const shards = {
      'manifest/chat.json': { domain: 'agents', items: [item()], note: 'x' },
      'manifest/odd.json': { domain: 'chat' },
    };
    const { problems } = mergeManifest(root, (path) => shards[path] ?? null);
    expect(problems).toEqual([
      'manifest.json: unknown field "bogus"',
      'manifest.json generatedFrom: unknown field "branch"',
      'manifest.json: a manifest must have either items or shards, not both',
      'shard manifest/missing.json: file is missing',
      'manifest.json shard manifest/chat.json: unknown field "extra"',
      'shard manifest/chat.json: unknown field "note"',
      'shard manifest/chat.json: index says domain "chat", shard says "agents"',
      'shard manifest/chat.json: index says 3 items, shard has 1',
      'shard manifest/chat.json: item ROUTE-001 has domain "chat", expected "agents"',
    ]);
  });

  it('treats a root with neither items nor shards as empty', () => {
    expect(mergeManifest({ ...header() }, () => null)).toEqual({ items: [], problems: [] });
  });
});

describe('validateManifest', () => {
  it('passes a valid manifest', () => {
    expect(validateManifest(header(), [item()])).toEqual([]);
  });

  it('passes a waived item with a complete waiver', () => {
    expect(validateManifest(header(), [item({ priority: 'waived', status: 'waived', waiver })])).toEqual([]);
  });

  it('rejects a bad header and an empty item set', () => {
    const root = { $schema: 'x', version: 2, generatedFrom: { repo: 'r', commit: 'abc', date: '28-08-2026' } };
    expect(validateManifest(root, [])).toEqual([
      'header: $schema must be ./manifest.schema.json, got "x"',
      'header: version must be 1, got 2',
      'header: generatedFrom.repo must be apps/elitea-ui, got "r"',
      'header: generatedFrom.commit must be a 40-char sha, got "abc"',
      'header: generatedFrom.date must be YYYY-MM-DD, got "28-08-2026"',
      'items: empty',
    ]);
  });

  it('rejects a header with no generatedFrom', () => {
    const problems = validateManifest({ $schema: './manifest.schema.json', version: 1 }, [item()]);
    expect(problems).toHaveLength(3);
  });

  it('rejects every malformed item field', () => {
    const bad = {
      id: 'bad',
      domain: 'nowhere',
      kind: 'k',
      priority: 'p',
      title: 'short',
      source: ['src/x.js:1', 'apps/elitea-ui/x.js:5-2', 'apps/elitea-ui/x.js:0'],
      acceptance: ['WHEN src/ clicked', 'AND then'],
      verify: { type: 't', command: 'x', testId: 'y', more: 1 },
      unit: 'Z9',
      status: 's',
      coverage: { file: '', min: 101, extra: true },
      waiver: null,
      stray: 1,
    };
    expect(validateManifest(header(), [bad])).toEqual([
      'bad: unknown field "stray"',
      'bad: id does not match ^[A-Z]{3,8}-\\d{3}$',
      'bad: unknown domain "nowhere"',
      'bad: unknown kind "k"',
      'bad: unknown priority "p"',
      'bad: title too short',
      'bad: source "src/x.js:1" is not apps/elitea-ui|deepwiki-ui/<file>:<line>[-<line>]',
      'bad: source "apps/elitea-ui/x.js:5-2" is not apps/elitea-ui|deepwiki-ui/<file>:<line>[-<line>]',
      'bad: source "apps/elitea-ui/x.js:0" is not apps/elitea-ui|deepwiki-ui/<file>:<line>[-<line>]',
      'bad: acceptance has 2 lines, need >=3',
      'bad: acceptance line 1 contains implementation detail ("src/")',
      'bad: acceptance must start with a GIVEN line',
      'bad: acceptance must contain a WHEN line and a THEN line',
      'bad verify: unknown field "more"',
      'bad: unknown verify.type "t"',
      'bad: verify.command missing',
      'bad: verify.testId missing',
      'bad: unknown unit "Z9" (must be a §9.3 unit id)',
      'bad: unknown status "s"',
      'bad coverage: unknown field "extra"',
      'bad: coverage.file missing',
      'bad: coverage.min 101 out of range',
    ]);
  });

  it('rejects an item with missing structures', () => {
    const bare = { id: 'ROUTE-002' };
    const problems = validateManifest(header(), [bare]);
    expect(problems).toContain('ROUTE-002: source is empty — an item with no evidence is rejected');
    expect(problems).toContain('ROUTE-002: acceptance has 0 lines, need >=3');
    expect(problems).toContain('ROUTE-002: verify.command missing');
    expect(problems).toContain('ROUTE-002: coverage.min undefined out of range');
  });

  it('rejects acceptance lines without a keyword', () => {
    const problems = validateManifest(header(), [item({ acceptance: ['GIVEN a', 'WHEN b', 'THEN c', 'so d'] })]);
    expect(problems).toEqual(['ROUTE-001: acceptance line 4 does not start with GIVEN/WHEN/THEN/AND']);
  });

  it('rejects duplicate ids', () => {
    expect(validateManifest(header(), [item(), item()])).toEqual(['ROUTE-001: duplicate id']);
  });

  it('enforces the waiver rules', () => {
    expect(validateManifest(header(), [item({ priority: 'waived' })])).toEqual([
      'ROUTE-001: priority waived requires a non-null waiver',
    ]);
    expect(validateManifest(header(), [item({ waiver: { ...waiver, date: 'soon', why: 1 } })])).toEqual([
      'ROUTE-001: waiver must be null unless priority is waived',
      'ROUTE-001 waiver: unknown field "why"',
      'ROUTE-001: waiver object incomplete (need reason, decidedBy, date YYYY-MM-DD, replacesBehaviour)',
    ]);
    expect(validateManifest(header(), [item({ priority: 'waived', waiver: { ...waiver, reason: '' } })])).toEqual([
      'ROUTE-001: waiver object incomplete (need reason, decidedBy, date YYYY-MM-DD, replacesBehaviour)',
    ]);
    expect(validateManifest(header(), [item({ status: 'waived' })])).toEqual([
      'ROUTE-001: status waived requires priority waived',
    ]);
  });

  it('treats an absent waiver as null', () => {
    const noWaiver = item();
    delete noWaiver.waiver;
    expect(validateManifest(header(), [noWaiver])).toEqual([]);
  });
});

describe('checkImmutability', () => {
  it('passes unchanged and added items', () => {
    expect(checkImmutability([item()], [item(), item({ id: 'ROUTE-002' })])).toEqual([]);
  });

  it('refuses a deleted id', () => {
    expect(checkImmutability([item()], [])).toEqual([
      'ROUTE-001: id present in the base manifest but deleted — ids are immutable',
    ]);
  });

  it('refuses changed acceptance text without a waiver, allows it with one', () => {
    const changed = { acceptance: ['GIVEN a', 'WHEN b', 'THEN different'] };
    expect(checkImmutability([item()], [item(changed)])).toEqual([
      'ROUTE-001: acceptance text changed relative to the base manifest without a waiver',
    ]);
    expect(checkImmutability([item()], [item({ ...changed, waiver })])).toEqual([]);
  });

  it('compares absent acceptance and waiver as empty and null', () => {
    expect(checkImmutability([{ id: 'ROUTE-001' }], [{ id: 'ROUTE-001' }])).toEqual([]);
    expect(checkImmutability([{ id: 'ROUTE-001' }], [{ id: 'ROUTE-001', acceptance: ['GIVEN x'] }])).toEqual([
      'ROUTE-001: acceptance text changed relative to the base manifest without a waiver',
    ]);
  });
});
