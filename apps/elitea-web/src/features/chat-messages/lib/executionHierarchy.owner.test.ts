/**
 * Golden tests: ported verbatim from EliteaUI
 * `features/chat/lib/helpers/__tests__/executionHierarchy.helpers.test.js:36-173`
 * (owner path, sub-agent name and instance key).
 */
import { describe, expect, it } from 'vitest';

import { getActionOwnerPath, getSubAgentInstanceKey, getSubAgentName, normalizeExecutionHierarchy } from './executionHierarchy';

describe('execution hierarchy helpers (owner/instance)', () => {
  it('keeps parallel same-name invocations separate by call id', () => {
    expect(getSubAgentInstanceKey({ parent_agent_name: 'B', parent_agent_call_id: 'b1' })).toBe(
      '[["B","b1"]]',
    );
    expect(getSubAgentInstanceKey({ parent_agent_name: 'B', parent_agent_call_id: 'b2' })).toBe(
      '[["B","b2"]]',
    );
  });

  it('does not split a resumed leaf when its batch ordinal changes', () => {
    const beforePause = {
      parent_agent_path: [
        { name: 'B', call_id: 'b1' },
        { name: 'C', call_id: 'c1', sibling_ordinal: 2 },
      ],
    };
    const afterResume = {
      parent_agent_path: [
        { name: 'B', call_id: 'b1' },
        { name: 'C', call_id: 'c1', sibling_ordinal: 1 },
      ],
    };

    expect(getSubAgentInstanceKey(beforePause)).toBe(getSubAgentInstanceKey(afterResume));
  });

  it('keys a nested delegation wrapper by its owner path, not the called child id', () => {
    const wrapper = {
      type: 'tool',
      original_name: 'Name Resolver',
      parent_agent_call_id: 'call-c',
      parent_agent_path: [{ name: 'Full Name resolver', call_id: 'call-b' }],
      toolMeta: { toolkit_type: 'application' },
    };
    const inner = {
      parent_agent_name: 'Name Resolver',
      parent_agent_call_id: 'call-c',
      parent_agent_path: [
        { name: 'Full Name resolver', call_id: 'call-b' },
        { name: 'Name Resolver', call_id: 'call-c' },
      ],
    };

    expect(getSubAgentName(wrapper)).toBe('Full Name resolver');
    expect(getSubAgentName(inner)).toBe('Name Resolver');
    expect(getSubAgentInstanceKey(wrapper)).not.toBe(getSubAgentInstanceKey(inner));
  });

  it('chooses the deepest hierarchy path across live and persisted sources', () => {
    expect(
      getActionOwnerPath({
        parent_agent_path: [{ name: 'B', call_id: 'b1' }],
        toolMeta: {
          parent_agent_path: [
            { name: 'B', call_id: 'b1' },
            { name: 'C', call_id: 'c1' },
          ],
        },
      }),
    ).toEqual([
      { name: 'B', call_id: 'b1', sibling_ordinal: undefined },
      { name: 'C', call_id: 'c1', sibling_ordinal: undefined },
    ]);
  });

  it('partitions a container wrapper, its own work, and two leaves by action owner', () => {
    const b = { name: 'Full Name resolver', call_id: 'call-b', sibling_ordinal: 1 };
    const actions = [
      {
        type: 'tool',
        original_name: 'Full Name resolver',
        parent_agent_call_id: 'call-b',
        sibling_ordinal: 1,
        toolMeta: { toolkit_type: 'application' },
      },
      { parent_agent_name: b.name, parent_agent_call_id: b.call_id, parent_agent_path: [b] },
      {
        original_name: 'Name Resolver',
        parent_agent_call_id: 'call-name',
        parent_agent_path: [b],
        toolMeta: { toolkit_type: 'application' },
      },
      {
        parent_agent_name: 'Name Resolver',
        parent_agent_call_id: 'call-name',
        parent_agent_path: [b, { name: 'Name Resolver', call_id: 'call-name' }],
      },
      {
        parent_agent_name: 'Surname Resolver',
        parent_agent_call_id: 'call-surname',
        parent_agent_path: [b, { name: 'Surname Resolver', call_id: 'call-surname' }],
      },
    ];

    const grouped = new Map<string, (typeof actions)[number][]>();
    actions.forEach(action => {
      const key = getSubAgentInstanceKey(action);
      grouped.set(key, [...(grouped.get(key) || []), action]);
    });

    expect([...grouped.values()].map(group => group.length)).toEqual([3, 1, 1]);
    expect([...grouped.values()].map(group => getSubAgentName(group[0]))).toEqual([
      'Full Name resolver',
      'Name Resolver',
      'Surname Resolver',
    ]);
  });

  it('does not let an empty action path mask populated tool metadata', () => {
    expect(
      normalizeExecutionHierarchy(
        { parent_agent_path: [] },
        { parent_agent_path: [{ name: 'B', call_id: 'b1' }] },
      ).parent_agent_path,
    ).toEqual([{ name: 'B', call_id: 'b1', sibling_ordinal: undefined }]);
  });

  it('does not turn ordinary inherited agent actions into fake sub-agent groups', () => {
    const ordinaryTool = {
      type: 'tool',
      name: 'read_file',
      original_name: 'Full Name resolver',
      parent_agent_call_id: 'tool-1',
      toolMeta: { toolkit_type: 'github', agent_type: 'pipeline' },
    };
    const ordinaryLlm = {
      type: 'llm',
      name: 'Full Name resolver',
      original_name: 'Full Name resolver',
      parent_agent_call_id: 'llm-1',
      toolMeta: { agent_type: 'pipeline' },
    };

    expect(getSubAgentName(ordinaryTool)).toBe('');
    expect(getSubAgentName(ordinaryLlm)).toBe('');
    expect(getActionOwnerPath(ordinaryTool)).toEqual([]);
    expect(getActionOwnerPath(ordinaryLlm)).toEqual([]);
  });
});
