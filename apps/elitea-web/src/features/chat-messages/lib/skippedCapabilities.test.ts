import { describe, expect, it } from 'vitest';
import { parseSkippedCapabilities, skippedCapabilitiesOf } from './skippedCapabilities';

const toolkit = { kind: 'toolkit', name: 'wiki', type: 'confluence', reason_code: 'unsupported_toolkit_family', reason: 'not available yet' };

describe('skipped capabilities', () => {
  it('parses, dedupes and drops malformed entries', () => {
    const parsed = parseSkippedCapabilities([toolkit, toolkit, { kind: 'bogus', name: 'x', reason: 'r' }, { kind: 'toolkit', name: '', reason: 'r' }, null, 'x']);
    expect(parsed).toEqual([{ kind: 'toolkit', name: 'wiki', type: 'confluence', reasonCode: 'unsupported_toolkit_family', reason: 'not available yet' }]);
    expect(parseSkippedCapabilities(undefined)).toEqual([]);
    expect(parseSkippedCapabilities({})).toEqual([]);
  });

  it('bounds the list', () => {
    const many = Array.from({ length: 80 }, (_, index) => ({ ...toolkit, name: `t${index}` }));
    expect(parseSkippedCapabilities(many)).toHaveLength(32);
  });

  it('prefers the live final frame and falls back to the persisted copy', () => {
    expect(skippedCapabilitiesOf({ responseMetadata: { skipped_capabilities: [toolkit] } })).toHaveLength(1);
    expect(skippedCapabilitiesOf({ skippedCapabilities: [toolkit] })).toHaveLength(1);
    expect(skippedCapabilitiesOf({ responseMetadata: { thread_id: 't' } })).toEqual([]);
  });
});
