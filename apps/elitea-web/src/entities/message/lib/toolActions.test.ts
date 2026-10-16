import { describe, expect, it } from 'vitest';

import { buildToolActions } from './toolActions';

describe('buildToolActions', () => {
  it('returns an empty array when there are no thinking steps or tool calls', () => {
    expect(buildToolActions([], {}, '2026-01-01T00:00:00Z', undefined, undefined)).toEqual([]);
  });

  it('skips a thinking step whose text is empty or whitespace-only', () => {
    const result = buildToolActions(
      [{ text: '   ', timestamp_start: '2026-01-01T00:00:01Z' }],
      {},
      '2026-01-01T00:00:00Z',
      undefined,
      undefined,
    );
    expect(result).toEqual([]);
  });

  it('accepts tool_calls as either an object-map or an array, producing the same shape', () => {
    const step = { tool_name: 'search', tool_run_id: 'r1', timestamp_start: '2026-01-01T00:00:01Z' };
    const viaMap = buildToolActions([], { c1: step }, '2026-01-01T00:00:00Z', undefined, undefined);
    const viaArray = buildToolActions([], [step], '2026-01-01T00:00:00Z', undefined, undefined);
    expect(viaMap).toEqual(viaArray);
    expect(viaMap[0]).toMatchObject({ type: 'tool', name: 'search', id: 'r1' });
  });

  it('sorts thinking steps and tool calls into one chronological list by timestamp_start', () => {
    const result = buildToolActions(
      [{ text: 'second', timestamp_start: '2026-01-01T00:00:05Z' }],
      [{ tool_name: 'first-tool', tool_run_id: 'r1', timestamp_start: '2026-01-01T00:00:01Z' }],
      '2026-01-01T00:00:00Z',
      undefined,
      undefined,
    );
    expect(result.map((a) => a.type)).toEqual(['tool', 'llm']);
    expect(result[0]?.name).toBe('first-tool');
    expect(result[1]?.content).toBe('second');
  });

  it('derives toolkit_name from the old "toolkit___tool" naming format when nothing else provides it', () => {
    const result = buildToolActions(
      [],
      [{ tool_name: 'github___create_issue', tool_run_id: 'r1' }],
      '2026-01-01T00:00:00Z',
      undefined,
      undefined,
    );
    expect(result[0]?.toolMeta).toMatchObject({ toolkit_name: 'github' });
  });

  it('resolves toolkit_type from the participant\'s tools[] list by matching toolkit_name', () => {
    const result = buildToolActions(
      [],
      [{ toolkit_name: 'jira', tool_run_id: 'r1' }],
      '2026-01-01T00:00:00Z',
      undefined,
      { id: 'p1', meta: { tools: [{ name: 'jira', type: 'external' }] } },
    );
    expect(result[0]?.toolMeta).toMatchObject({ toolkit_type: 'external' });
  });

  it('marks isError true when the tool_call step carries an error, falling content back to the (string) error', () => {
    const result = buildToolActions([], [{ tool_run_id: 'r1', error: 'boom' }], '2026-01-01T00:00:00Z', undefined, undefined);
    expect(result[0]?.isError).toBe(true);
    expect(result[0]?.content).toBe('boom');
  });

  it('falls the first step\'s created_at back to first_tool_timestamp_start, not later steps', () => {
    const result = buildToolActions(
      [],
      [
        { tool_run_id: 'r1', timestamp_start: '2026-01-01T00:00:05Z' },
        { tool_run_id: 'r2', timestamp_start: '2026-01-01T00:00:06Z' },
      ],
      '2026-01-01T00:00:00Z',
      '2026-01-01T00:00:01Z',
      undefined,
    );
    expect(result[0]?.created_at).toBe('2026-01-01T00:00:01Z');
    expect(result[1]?.created_at).toBe('2026-01-01T00:00:06Z');
  });

  it('keeps the persisted ancestry so a reloaded turn groups like the live one', () => {
    const path = [{ name: 'Sub', call_id: 'call-b', sibling_ordinal: 2 }];
    const [action] = buildToolActions([], [{
      tool_run_id: 't1', tool_name: 'search',
      metadata: { parent_agent_name: 'Sub', parent_agent_call_id: 'call-b', parent_agent_path: path, sibling_ordinal: 2 },
    }], '2026-01-01T00:00:00Z', undefined, undefined);
    expect(action).toMatchObject({ parent_agent_path: path, sibling_ordinal: 2 });
  });

  it('drops an ancestry deeper than the 3-tier contract', () => {
    const tier = { name: 'X', call_id: 'c' };
    const [action] = buildToolActions([], [{ tool_run_id: 't1', tool_name: 'search', metadata: { parent_agent_path: [tier, tier, tier, tier] } }], '2026-01-01T00:00:00Z', undefined, undefined);
    expect(action).not.toHaveProperty('parent_agent_path');
  });

  it('drops tiers that are not objects and a path whose strings break the live bounds', () => {
    const run = (path: unknown) => buildToolActions([], [{ tool_run_id: 't1', tool_name: 'search', metadata: { parent_agent_path: path } }], '2026-01-01T00:00:00Z', undefined, undefined)[0];
    expect(run(['junk', { name: 'Sub', call_id: 'call-b' }, 7])).toMatchObject({ parent_agent_path: [{ name: 'Sub', call_id: 'call-b' }] });
    expect(run([{ name: 'Sub', call_id: 'c'.repeat(513) }])).not.toHaveProperty('parent_agent_path');
    expect(run([{ name: 'n'.repeat(2049), call_id: 'call-b' }])).not.toHaveProperty('parent_agent_path');
    expect(run([{ name: 'n'.repeat(2048), call_id: 'c'.repeat(512) }])).toHaveProperty('parent_agent_path');
  });
});
