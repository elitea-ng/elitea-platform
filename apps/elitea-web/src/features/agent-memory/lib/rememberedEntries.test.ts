import { describe, expect, it } from 'vitest';

import { rememberActions, rememberedEntryOf } from './rememberedEntries';

describe('rememberActions', () => {
  it('keeps only completed remember calls', () => {
    const actions = [
      { id: 'a', name: 'remember', status: 'complete' },
      { id: 'b', name: 'remember', status: 'processing' },
      { id: 'c', name: 'recall', status: 'complete' },
    ];
    expect(rememberActions(actions).map((action) => action.id)).toEqual(['a']);
    expect(rememberActions(undefined)).toEqual([]);
  });
});

describe('rememberedEntryOf', () => {
  it('reads the saved entry from the tool output', () => {
    expect(rememberedEntryOf({ kind: 'fact', content: ' Uses Go ' }, '{"status":"pending","entry_id":"12","note":"waits"}')).toEqual({
      entryId: 12,
      status: 'pending',
      content: 'Uses Go',
    });
  });

  it('decodes an output encoded more than once', () => {
    const output = JSON.stringify(JSON.stringify({ status: 'active', entry_id: 5 }));
    expect(rememberedEntryOf('{"content":"x"}', output)?.entryId).toBe(5);
  });

  it('is nothing when the call saved nothing', () => {
    expect(rememberedEntryOf({}, '{"status":"refused"}')).toBeUndefined();
    expect(rememberedEntryOf({}, '{"status":"active","entry_id":"0"}')).toBeUndefined();
    expect(rememberedEntryOf({}, 'not json')).toBeUndefined();
  });
});
