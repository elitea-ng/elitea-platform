/** Defaults contain only fields in the reviewed extension schemas. Required selections stay empty. */
export const GraphExtensionDefaults: Readonly<Record<string, Readonly<Record<string, unknown>>>> = {
  map: {
    worker: '', source: '', item: 'item', index: 'item_index', outputs: [], destination: '',
    max_items: 64, max_concurrency: 1, reduction: 'ordered_collection', broadcast: [], transition: 'END',
  },
  split_out: {
    source: '', split: { mode: 'list' }, destination: 'item', retain: { mode: 'none' },
    missing_list: 'error', null_list: 'error', remove_source: true, output: [], transition: 'END',
  },
  aggregate: {
    source: '', layout: 'plain', group_by: [], operations: [{ operation: 'count_rows', output: 'count' }],
    output: [], transition: 'END',
  },
};
