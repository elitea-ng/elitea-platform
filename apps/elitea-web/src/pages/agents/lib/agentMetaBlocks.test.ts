// The editor's meta-block controls (issue 3137): the validation that gates
// Save mirrors v2.yaml's limits and the tier's run caps, and the write field.
import { describe, expect, it } from 'vitest';

import {
  areMetaBlocksEqual,
  metaBlockProblems,
  metaBlocksWriteField,
  numberInput,
  readMetaBlocks,
  withMetaValue,
  type MetaBlocks,
} from './agentMetaBlocks';

const NONE: MetaBlocks = { memory: null, proactivity: null, tool_envelope: null, capabilities: null, run_caps: null };

function problemsOf(blocks: Partial<MetaBlocks>, tierCaps = {}): string[] {
  return [...metaBlockProblems({ ...NONE, ...blocks }, tierCaps).keys()];
}

describe('metaBlockProblems', () => {
  it('accepts no blocks and in-range values', () => {
    expect(problemsOf({})).toEqual([]);
    expect(
      problemsOf({
        memory: { enabled: true, recall: { max_entries: 50, max_tokens: 100 } },
        proactivity: { mode: 'heartbeat', interval: '1h30m', jitter: 50, quiet_hours: '22:00-07:00 Europe/Berlin', limits: { wakes_per_day: 96, acts_per_day: 0, spend_per_day_usd: 2.5 } },
        tool_envelope: { tool_limits: { 'github/search': 5 }, unattended_tools: ['github/search'] },
        run_caps: { max_tool_calls: 10, max_spend: 0.0001 },
      }),
    ).toEqual([]);
  });

  it('names memory recall values out of range or not numbers', () => {
    expect(problemsOf({ memory: { recall: { max_entries: 51, max_tokens: 99 } } })).toEqual(['memory.recall.max_entries', 'memory.recall.max_tokens']);
    expect(problemsOf({ memory: { recall: { max_entries: 'ten' } } })).toEqual(['memory.recall.max_entries']);
    expect(problemsOf({ memory: { recall: { max_entries: 2.5 } } })).toEqual(['memory.recall.max_entries']);
  });

  it('holds the heartbeat interval to 15m..24h in whole minutes', () => {
    expect(problemsOf({ proactivity: { interval: '15m' } })).toEqual([]);
    expect(problemsOf({ proactivity: { interval: '24h' } })).toEqual([]);
    for (const interval of ['14m', '25h', '30s', 'soon', '']) {
      expect(problemsOf({ proactivity: { interval } }), interval).toEqual(['proactivity.interval']);
    }
  });

  it('wants quiet hours as HH:MM-HH:MM and a real time zone', () => {
    expect(problemsOf({ proactivity: { quiet_hours: '22:00-07:00 Mars/Olympus' } })).toEqual(['proactivity.quiet_hours']);
    expect(problemsOf({ proactivity: { quiet_hours: '25:00-07:00 UTC' } })).toEqual(['proactivity.quiet_hours']);
    expect(problemsOf({ proactivity: { quiet_hours: '22:00-07:00 UTC' } })).toEqual([]);
  });

  it('wants a tool name on a toolkit_read watch and daily limits in range', () => {
    expect(problemsOf({ proactivity: { watch: [{ kind: 'toolkit_read', tool: '' }] } })).toEqual(['proactivity.watch']);
    expect(problemsOf({ proactivity: { watch: [{ kind: 'home_thread' }] } })).toEqual([]);
    expect(problemsOf({ proactivity: { limits: { wakes_per_day: 0, spend_per_day_usd: 1001 } } })).toEqual([
      'proactivity.limits.wakes_per_day',
      'proactivity.limits.spend_per_day_usd',
    ]);
  });

  it('refuses a tool limit that is not a whole number from 1 to 10000', () => {
    expect(problemsOf({ tool_envelope: { tool_limits: { a: 0 } } })).toEqual(['tool_envelope.tool_limits']);
    expect(problemsOf({ tool_envelope: { tool_limits: { a: 10001 } } })).toEqual(['tool_envelope.tool_limits']);
    expect(problemsOf({ tool_envelope: { subagent_tools: Array.from({ length: 257 }, (_, i) => `t${String(i)}`) } })).toEqual([
      'tool_envelope.subagent_tools',
    ]);
  });

  it('holds a run cap to its own bounds and under the tier cap; 0 at the tier is no cap', () => {
    expect(problemsOf({ run_caps: { max_tool_calls: 0, max_spend: 0.00001 } })).toEqual(['run_caps.max_tool_calls', 'run_caps.max_spend']);
    const tier = { max_tool_calls: 50, max_model_turns: 0 };
    expect(problemsOf({ run_caps: { max_tool_calls: 51, max_model_turns: 99999 } }, tier)).toEqual(['run_caps.max_tool_calls']);
    expect(metaBlockProblems({ ...NONE, run_caps: { max_tool_calls: 51 } }, tier).get('run_caps.max_tool_calls')).toMatch(/50/);
    expect(problemsOf({ run_caps: { max_tool_calls: 50 } }, tier)).toEqual([]);
  });
});

describe('meta block state', () => {
  it('reads only object blocks from the version meta', () => {
    expect(readMetaBlocks({ memory: { enabled: true }, run_caps: 'x', step_limit: 5 })).toEqual({ ...NONE, memory: { enabled: true } });
    expect(readMetaBlocks(undefined)).toEqual(NONE);
  });

  it('compares blocks by value, not key order', () => {
    expect(areMetaBlocksEqual({ ...NONE, memory: { a: 1, b: 2 } }, { ...NONE, memory: { b: 2, a: 1 } })).toBe(true);
    expect(areMetaBlocksEqual({ ...NONE, memory: { a: 1 } }, NONE)).toBe(false);
  });

  it('sets and clears a nested value; an emptied block is null', () => {
    const block = withMetaValue(null, ['recall', 'max_entries'], 5);
    expect(block).toEqual({ recall: { max_entries: 5 } });
    expect(withMetaValue(block, ['recall', 'max_entries'], undefined)).toBeNull();
  });

  it('keeps unreadable number text so validation can name it', () => {
    expect(numberInput(' 12 ')).toBe(12);
    expect(numberInput('')).toBeUndefined();
    expect(numberInput('12x')).toBe('12x');
  });

  it('sends a set block, and null for a block cleared that the stored meta had', () => {
    expect(metaBlocksWriteField({ memory: { enabled: true }, step_limit: 5 }, { ...NONE, run_caps: { max_tool_calls: 3 } })).toEqual({
      memory: null,
      run_caps: { max_tool_calls: 3 },
    });
  });
});
