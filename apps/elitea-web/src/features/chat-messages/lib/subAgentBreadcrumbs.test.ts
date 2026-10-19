/**
 * Golden tests: ported verbatim from EliteaUI
 * `features/chat/lib/helpers/__tests__/subAgentGrouping.helpers.test.js:656-885`
 * (`computeBreadcrumbs`, `selectRichestAgentPath`). The expectations must
 * stay byte-identical to the reference — they are the label contract.
 */
import { describe, expect, it } from 'vitest';

import { computeBreadcrumbs, selectRichestAgentPath } from './subAgentBreadcrumbs';

describe('computeBreadcrumb — #5778 depth-3 per-tier numbering', () => {
  it('emits stable labels for depth-1 entries and skips empty paths', () => {
    const entries = [
      { instanceKey: 'cB1', agentPath: [{ name: 'B', call_id: 'cB1' }] },
      { instanceKey: 'cB2', agentPath: [{ name: 'B', call_id: 'cB2' }] },
      { instanceKey: 'noPath', agentPath: [] },
    ];
    const map = computeBreadcrumbs(entries);
    expect(map.get('cB1')).toBe('B (1)');
    expect(map.get('cB2')).toBe('B (2)');
    expect(map.has('noPath')).toBe(false);
  });

  it('disambiguates two parallel same-name containers, each with its own leaf', () => {
    const entries = [
      {
        instanceKey: 'cLeafA',
        agentPath: [
          { name: 'container', call_id: 'cC1' },
          { name: 'leaf', call_id: 'cLeafA' },
        ],
      },
      {
        instanceKey: 'cLeafB',
        agentPath: [
          { name: 'container', call_id: 'cC2' },
          { name: 'leaf', call_id: 'cLeafB' },
        ],
      },
    ];
    const map = computeBreadcrumbs(entries);
    expect(map.get('cLeafA')).toBe('container (1) ▸ leaf');
    expect(map.get('cLeafB')).toBe('container (2) ▸ leaf');
  });

  it('is stable across re-computation and independent of relative block order', () => {
    const containerA = { name: 'container', call_id: 'cC1' };
    const containerB = { name: 'container', call_id: 'cC2' };
    const entries1 = [
      { instanceKey: 'cLeafA', agentPath: [containerA, { name: 'leaf', call_id: 'cLeafA' }] },
      { instanceKey: 'cLeafB', agentPath: [containerB, { name: 'leaf', call_id: 'cLeafB' }] },
    ];
    // Same two blocks, fed in the opposite relative order alongside an unrelated
    // depth-1 block interleaved between them. Call-id ordering is the deterministic
    // compatibility fallback, so relative arrival order cannot change labels.
    const entries2 = [
      { instanceKey: 'cLeafB', agentPath: [containerB, { name: 'leaf', call_id: 'cLeafB' }] },
      { instanceKey: 'unrelated', agentPath: [{ name: 'Other', call_id: 'cOther' }] },
      { instanceKey: 'cLeafA', agentPath: [containerA, { name: 'leaf', call_id: 'cLeafA' }] },
    ];
    const map1 = computeBreadcrumbs(entries1);
    const map2 = computeBreadcrumbs(entries2);
    expect(computeBreadcrumbs(entries1)).toEqual(map1);
    expect(map1.get('cLeafA')).toBe('container (1) ▸ leaf');
    expect(map1.get('cLeafB')).toBe('container (2) ▸ leaf');
    expect(map2.get('cLeafB')).toBe('container (2) ▸ leaf');
    expect(map2.get('cLeafA')).toBe('container (1) ▸ leaf');
  });

  it('combines a depth-1 run and a depth-3 run in one action list without cross-interference', () => {
    const entries = [
      // Depth-1 uses the same breadcrumb contract as deeper paths.
      { instanceKey: 'cB', agentPath: [{ name: 'B', call_id: 'cB' }] },
      // Depth-3: root -> container -> leaf.
      {
        instanceKey: 'cLeaf',
        agentPath: [
          { name: 'container', call_id: 'cC' },
          { name: 'leaf', call_id: 'cLeaf' },
        ],
      },
    ];
    const map = computeBreadcrumbs(entries);
    expect(map.get('cB')).toBe('B');
    // Lone container instance -> no "(n)" suffix (per-tier ">1" guard).
    expect(map.get('cLeaf')).toBe('container ▸ leaf');
  });

  it('uses backend sibling ordinals for same-name invocations', () => {
    const entries = [
      {
        instanceKey: 'b-z',
        agentPath: [{ name: 'B', call_id: 'z', sibling_ordinal: 1 }],
      },
      {
        instanceKey: 'b-a',
        agentPath: [{ name: 'B', call_id: 'a', sibling_ordinal: 2 }],
      },
    ];
    const map = computeBreadcrumbs(entries);
    expect(map.get('b-z')).toBe('B (1)');
    expect(map.get('b-a')).toBe('B (2)');
  });

  it('uses sparse backend ordinals only for ordering and displays contiguous ranks', () => {
    const map = computeBreadcrumbs([
      { instanceKey: 'b1', agentPath: [{ name: 'B', call_id: 'b1', sibling_ordinal: 1 }] },
      { instanceKey: 'b3', agentPath: [{ name: 'B', call_id: 'b3', sibling_ordinal: 3 }] },
    ]);
    expect(map.get('b1')).toBe('B (1)');
    expect(map.get('b3')).toBe('B (2)');
  });

  it('does not number a lone root or any leaf from stale leaf ordinals', () => {
    const map = computeBreadcrumbs([
      {
        instanceKey: 'leaf',
        agentPath: [
          { name: 'Full Name resolver', call_id: 'b1', sibling_ordinal: 2 },
          { name: 'Surname Resolver', call_id: 'c1', sibling_ordinal: 7 },
        ],
      },
    ]);
    expect(map.get('leaf')).toBe('Full Name resolver ▸ Surname Resolver');
  });

  it('numbers only two repeated root suborchestrators and propagates that label to leaves', () => {
    const map = computeBreadcrumbs([
      {
        instanceKey: 'b1',
        agentPath: [{ name: 'Full Name resolver', call_id: 'b1', sibling_ordinal: 1 }],
      },
      {
        instanceKey: 'b1-name',
        agentPath: [
          { name: 'Full Name resolver', call_id: 'b1', sibling_ordinal: 1 },
          { name: 'Name Resolver', call_id: 'n1', sibling_ordinal: 9 },
        ],
      },
      {
        instanceKey: 'b2-name',
        agentPath: [
          { name: 'Full Name resolver', call_id: 'b2', sibling_ordinal: 2 },
          { name: 'Name Resolver', call_id: 'n2', sibling_ordinal: 4 },
        ],
      },
    ]);
    expect(map.get('b1')).toBe('Full Name resolver (1)');
    expect(map.get('b1-name')).toBe('Full Name resolver (1) ▸ Name Resolver');
    expect(map.get('b2-name')).toBe('Full Name resolver (2) ▸ Name Resolver');
  });

  it('keeps a lone pending root numbered from completed sibling trace context', () => {
    const pending = [
      {
        instanceKey: 'b1-name-current',
        agentPath: [
          { name: 'Full Name resolver', call_id: 'b1', sibling_ordinal: 1 },
          { name: 'Name Resolver', call_id: 'n1', sibling_ordinal: 1 },
        ],
      },
    ];
    const traceContext = [
      ...pending,
      {
        instanceKey: 'b2-completed',
        agentPath: [
          { name: 'Full Name resolver', call_id: 'b2', sibling_ordinal: 2 },
          { name: 'Name Resolver', call_id: 'n2', sibling_ordinal: 1 },
        ],
      },
    ];

    const map = computeBreadcrumbs(pending, traceContext);

    expect(map.get('b1-name-current')).toBe('Full Name resolver (1) ▸ Name Resolver');
  });

  it('keeps the second root numbered when it is the only pending sibling', () => {
    const pending = [
      {
        instanceKey: 'b2-name-current',
        agentPath: [
          { name: 'Full Name resolver', call_id: 'b2', sibling_ordinal: 2 },
          { name: 'Name Resolver', call_id: 'n2', sibling_ordinal: 1 },
        ],
      },
    ];
    const map = computeBreadcrumbs(pending, [
      {
        instanceKey: 'b1-completed',
        agentPath: [{ name: 'Full Name resolver', call_id: 'b1', sibling_ordinal: 1 }],
      },
      ...pending,
    ]);

    expect(map.get('b2-name-current')).toBe('Full Name resolver (2) ▸ Name Resolver');
  });

  it('does not number one root repeated across multiple trace paths', () => {
    const pending = [
      {
        instanceKey: 'b1-name-current',
        agentPath: [
          { name: 'Full Name resolver', call_id: 'b1', sibling_ordinal: 1 },
          { name: 'Name Resolver', call_id: 'n1', sibling_ordinal: 1 },
        ],
      },
    ];
    const map = computeBreadcrumbs(pending, [
      ...pending,
      {
        instanceKey: 'b1-surname-trace',
        agentPath: [
          { name: 'Full Name resolver', call_id: 'b1', sibling_ordinal: 1 },
          { name: 'Surname Resolver', call_id: 's1', sibling_ordinal: 2 },
        ],
      },
    ]);

    expect(map.get('b1-name-current')).toBe('Full Name resolver ▸ Name Resolver');
  });
});

describe('selectRichestAgentPath', () => {
  it('prefers complete depth and identity over action arrival order', () => {
    expect(
      selectRichestAgentPath([
        [{ name: 'B', call_id: 'b1' }],
        [
          { name: 'B', call_id: 'b1', sibling_ordinal: 2 },
          { name: 'C', call_id: 'c1' },
        ],
      ]),
    ).toEqual([
      { name: 'B', call_id: 'b1', sibling_ordinal: 2 },
      { name: 'C', call_id: 'c1' },
    ]);
  });
});
