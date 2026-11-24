import { describe, expect, it } from 'vitest';

import {
  formatAcceptedStatuses,
  parseAcceptedStatuses,
  readBindingMapRows,
  readBodyDraft,
  readHeaderRows,
  readIdempotencyDraft,
  writeBindingMapRows,
  writeBodyDraft,
  writeHeaderRows,
  writeIdempotencyDraft,
} from './httpAction.helpers';

describe('httpAction.helpers', () => {
  it('parses and formats accepted HTTP status ranges', () => {
    expect(parseAcceptedStatuses('200-299, 404')).toEqual([
      { first: 200, last: 299 },
      { first: 404, last: 404 },
    ]);
    expect(formatAcceptedStatuses([{ first: 200, last: 201 }, { first: 204, last: 204 }])).toBe('200-201, 204');
    expect(parseAcceptedStatuses('100-199')).toBeUndefined();
    expect(parseAcceptedStatuses('300-200')).toBeUndefined();
    expect(parseAcceptedStatuses('abc')).toBeUndefined();
  });

  it('round-trips path/query binding maps with optional flags and literals', () => {
    const raw = {
      run_id: { from: 'run_id' },
      page: { from: 'page_no', optional: true },
      mode: { value: 'fast' },
    };
    const rows = readBindingMapRows(raw);
    expect(rows).toEqual([
      { name: 'run_id', source: 'from', value: 'run_id', optional: false },
      { name: 'page', source: 'from', value: 'page_no', optional: true },
      { name: 'mode', source: 'value', value: 'fast', optional: false },
    ]);
    expect(writeBindingMapRows(rows)).toEqual(raw);
  });

  it('round-trips header bindings', () => {
    const raw = [
      { name: 'X-Trace-Id', from: 'trace_id', optional: true },
      { name: 'X-Env', value: 'prod' },
    ];
    const rows = readHeaderRows(raw);
    expect(writeHeaderRows(rows)).toEqual(raw);
  });

  it('round-trips body and idempotency drafts', () => {
    const jsonFrom = readBodyDraft({ kind: 'json', from: 'payload', content_type: 'application/json' });
    expect(writeBodyDraft(jsonFrom)).toEqual({ kind: 'json', from: 'payload', content_type: 'application/json' });

    const jsonLiteral = readBodyDraft({ kind: 'json', value: { ok: true } });
    expect(writeBodyDraft(jsonLiteral)).toEqual({ kind: 'json', value: { ok: true } });

    const idem = readIdempotencyDraft({ from: 'run_id' });
    expect(writeIdempotencyDraft(idem)).toEqual({ from: 'run_id' });
    expect(writeIdempotencyDraft({ mode: 'none', value: '' })).toBeUndefined();
  });
});
