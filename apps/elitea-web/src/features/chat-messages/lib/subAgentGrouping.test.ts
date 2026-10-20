import { describe, expect, it } from 'vitest';

import type { SubAgentGroupable } from '@/entities/message/lib/subAgentGrouping';

import {
  buildPcidAnchorMap,
  inflightToolChipId,
  isInvocationId,
  INVOCATION_ID_RE,
  partitionActionsIntoBlocks,
  resolveExtraSubAgentKeys,
  resolveSubAgentLiveness,
  type PartitionedBlock,
} from './subAgentGrouping';

describe('isInvocationId', () => {
  it('matches call_ prefixed alphanumeric strings', () => {
    expect(isInvocationId('call_abc123')).toBe(true);
    expect(isInvocationId('call_ABCdef')).toBe(true);
  });

  it('rejects non-matching strings', () => {
    expect(isInvocationId('notacall')).toBe(false);
    expect(isInvocationId('call_')).toBe(false);
    expect(isInvocationId('call_abc-def')).toBe(false);
    expect(isInvocationId('')).toBe(false);
  });
});

describe('INVOCATION_ID_RE', () => {
  it('is anchored', () => {
    expect(INVOCATION_ID_RE.test('prefix_call_abc')).toBe(false);
    expect(INVOCATION_ID_RE.test('call_abc_suffix')).toBe(false);
  });
});

describe('partitionActionsIntoBlocks', () => {
  const deriveName = (a: SubAgentGroupable) => (a as unknown as Record<string, string>).agentName ?? '';
  const deriveInstanceKey = (a: SubAgentGroupable) => (a as unknown as Record<string, string>).pcid ?? '';
  const classifyWrapper = () => null;
  const opts = { deriveName, deriveInstanceKey, classifyWrapper };

  it('groups unnamed actions into coord blocks', () => {
    const actions = [{ agentName: '' }, { agentName: '' }] as unknown as SubAgentGroupable[];
    const blocks = partitionActionsIntoBlocks(actions, opts);
    expect(blocks).toHaveLength(1);
    expect(blocks[0]!.kind).toBe('coord');
    expect(blocks[0]!.actions).toHaveLength(2);
  });

  it('groups named actions into sub blocks by instanceKey', () => {
    const actions = [
      { agentName: 'bot', pcid: 'call_1' },
      { agentName: 'bot', pcid: 'call_1' },
    ] as unknown as SubAgentGroupable[];
    const blocks = partitionActionsIntoBlocks(actions, opts);
    expect(blocks).toHaveLength(1);
    expect(blocks[0]!.kind).toBe('sub');
    expect(blocks[0]!.actions).toHaveLength(2);
  });

  it('creates separate blocks for different instance keys', () => {
    const actions = [
      { agentName: 'bot', pcid: 'call_1' },
      { agentName: 'bot', pcid: 'call_2' },
    ] as unknown as SubAgentGroupable[];
    const blocks = partitionActionsIntoBlocks(actions, opts);
    expect(blocks).toHaveLength(2);
  });

  it('never folds a second same-name instance into a paused block (W5)', () => {
    const classifyWrapperPaused = (a: SubAgentGroupable) =>
      (a as unknown as Record<string, string>).phase === 'paused' ? 'paused' as const : null;
    const actions = [
      { agentName: 'bot', pcid: 'call_1', phase: 'paused' },
      { agentName: 'bot', pcid: 'call_2' },
    ] as unknown as SubAgentGroupable[];
    const blocks = partitionActionsIntoBlocks(actions, {
      deriveName,
      deriveInstanceKey,
      classifyWrapper: classifyWrapperPaused,
    });
    expect(blocks.map((block) => (block.kind === 'sub' ? block.instanceKey : ''))).toEqual(['call_1', 'call_2']);
  });

  it('handles empty/undefined list gracefully', () => {
    expect(partitionActionsIntoBlocks([], opts)).toEqual([]);
  });

  it('alternates coord and sub blocks', () => {
    const actions = [
      { agentName: '' },
      { agentName: 'bot', pcid: 'call_1' },
      { agentName: '' },
    ] as unknown as SubAgentGroupable[];
    const blocks = partitionActionsIntoBlocks(actions, opts);
    expect(blocks.map((b) => b.kind)).toEqual(['coord', 'sub', 'coord']);
  });
});

describe('buildPcidAnchorMap', () => {
  it('maps all alias keys to the anchor (first key)', () => {
    const blocks: PartitionedBlock[] = [
      { kind: 'sub', instanceKey: 'call_1', name: 'bot', actions: [], pausedForResume: false, aliasKeys: ['call_1', 'call_2', 'call_3'] },
    ];
    const map = buildPcidAnchorMap(blocks);
    expect(map.get('call_1')).toBe('call_1');
    expect(map.get('call_2')).toBe('call_1');
    expect(map.get('call_3')).toBe('call_1');
  });

  it('ignores coord blocks', () => {
    const blocks: PartitionedBlock[] = [{ kind: 'coord', actions: [] }];
    expect(buildPcidAnchorMap(blocks).size).toBe(0);
  });
});

describe('resolveExtraSubAgentKeys', () => {
  it('returns all alias keys except the first', () => {
    const block = { kind: 'sub' as const, instanceKey: 'k1', name: 'n', actions: [], pausedForResume: false, aliasKeys: ['k1', 'k2', 'k3'] };
    expect(resolveExtraSubAgentKeys(block)).toEqual(['k2', 'k3']);
  });

  it('returns empty when only one key', () => {
    const block = { kind: 'sub' as const, instanceKey: 'k1', name: 'n', actions: [], pausedForResume: false, aliasKeys: ['k1'] };
    expect(resolveExtraSubAgentKeys(block)).toEqual([]);
  });
});

describe('resolveSubAgentLiveness', () => {
  it('returns done=true, running=false for coord blocks', () => {
    const block: PartitionedBlock = { kind: 'coord', actions: [] };
    expect(resolveSubAgentLiveness(block, { done: false, hasError: false })).toEqual({ running: false, done: true });
  });

  it('running when not done and has actions', () => {
    const block: PartitionedBlock = { kind: 'sub', instanceKey: 'k', name: 'n', actions: [{}] as SubAgentGroupable[], pausedForResume: false, aliasKeys: ['k'] };
    expect(resolveSubAgentLiveness(block, { done: false, hasError: false })).toEqual({ running: true, done: false });
  });

  it('done when signals.done and not paused', () => {
    const block: PartitionedBlock = { kind: 'sub', instanceKey: 'k', name: 'n', actions: [{}] as SubAgentGroupable[], pausedForResume: false, aliasKeys: ['k'] };
    expect(resolveSubAgentLiveness(block, { done: true, hasError: false })).toEqual({ running: false, done: true });
  });

  it('not running when hasError', () => {
    const block: PartitionedBlock = { kind: 'sub', instanceKey: 'k', name: 'n', actions: [{}] as SubAgentGroupable[], pausedForResume: false, aliasKeys: ['k'] };
    expect(resolveSubAgentLiveness(block, { done: false, hasError: true })).toEqual({ running: false, done: false });
  });

  it('not done while paused even if signals.done', () => {
    const block: PartitionedBlock = { kind: 'sub', instanceKey: 'k', name: 'n', actions: [], pausedForResume: true, aliasKeys: ['k'] };
    expect(resolveSubAgentLiveness(block, { done: true, hasError: false }).done).toBe(false);
  });
});

describe('inflightToolChipId', () => {
  it('builds id from block kind, resolved pcid, and action id', () => {
    const block: PartitionedBlock = { kind: 'sub', instanceKey: 'k', name: 'n', actions: [], pausedForResume: false, aliasKeys: ['k'] };
    const action = { parent_agent_call_id: 'call_2', id: 'act1' } as unknown as SubAgentGroupable;
    const map = new Map([['call_2', 'call_1']]);
    expect(inflightToolChipId(block, action, map)).toBe('sub:call_1:act1');
  });

  it('falls back to pcid itself when not in anchor map', () => {
    const block: PartitionedBlock = { kind: 'coord', actions: [] };
    const action = { parent_agent_call_id: 'call_X', id: 'a' } as unknown as SubAgentGroupable;
    expect(inflightToolChipId(block, action, new Map())).toBe('coord:call_X:a');
  });

  it('handles missing pcid and id', () => {
    const block: PartitionedBlock = { kind: 'coord', actions: [] };
    const action = {} as SubAgentGroupable;
    expect(inflightToolChipId(block, action, new Map())).toBe('coord::');
  });
});
