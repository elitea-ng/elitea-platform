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
  it('parses and formats accepted HTTP status ranges and rejects 3xx, 401, and 403', () => {
    expect(parseAcceptedStatuses('200-299, 404')).toEqual([
      { first: 200, last: 299 },
      { first: 404, last: 404 },
    ]);
    expect(formatAcceptedStatuses([{ first: 200, last: 201 }, { first: 204, last: 204 }])).toBe('200-201, 204');
    expect(parseAcceptedStatuses('100-199')).toBeUndefined();
    expect(parseAcceptedStatuses('300-200')).toBeUndefined();
    expect(parseAcceptedStatuses('300-302')).toBeUndefined();
    expect(parseAcceptedStatuses('200-305')).toBeUndefined();
    expect(parseAcceptedStatuses('401')).toBeUndefined();
    expect(parseAcceptedStatuses('400-402')).toBeUndefined();
    expect(parseAcceptedStatuses('403')).toBeUndefined();
    expect(parseAcceptedStatuses('402-404')).toBeUndefined();
    expect(parseAcceptedStatuses('abc')).toBeUndefined();
  });

  it('round-trips path/query binding maps and strips prototype-polluting keys', () => {
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

    const polluted = writeBindingMapRows([
      { name: '__proto__', source: 'value', value: 'evil', optional: false },
      { name: 'constructor', source: 'value', value: 'evil', optional: false },
      { name: 'prototype', source: 'value', value: 'evil', optional: false },
      { name: 'safe', source: 'from', value: 'run_id', optional: false },
    ]);
    expect(polluted).toEqual({ safe: { from: 'run_id' } });
    expect(Object.hasOwn(polluted ?? {}, '__proto__')).toBe(false);
    expect(Object.hasOwn(polluted ?? {}, 'constructor')).toBe(false);
    expect(Object.hasOwn(polluted ?? {}, 'prototype')).toBe(false);
  });

  it('round-trips header bindings', () => {
    const raw = [
      { name: 'X-Trace-Id', from: 'trace_id', optional: true },
      { name: 'X-Env', value: 'prod' },
    ];
    const rows = readHeaderRows(raw);
    expect(writeHeaderRows(rows)).toEqual(raw);
  });

  it('round-trips json, text, and artifact body drafts and idempotency drafts', () => {
    const jsonFrom = readBodyDraft({ kind: 'json', from: 'payload', content_type: 'application/json' });
    expect(writeBodyDraft(jsonFrom)).toEqual({ kind: 'json', from: 'payload', content_type: 'application/json' });

    const jsonLiteral = readBodyDraft({ kind: 'json', value: { ok: true } });
    expect(writeBodyDraft(jsonLiteral)).toEqual({ kind: 'json', value: { ok: true } });

    const textFrom = readBodyDraft({ kind: 'text', from: 'notes' });
    expect(writeBodyDraft(textFrom)).toEqual({ kind: 'text', from: 'notes' });

    const textLiteral = readBodyDraft({ kind: 'text', value: 'hello', content_type: 'text/plain' });
    expect(writeBodyDraft(textLiteral)).toEqual({ kind: 'text', value: 'hello', content_type: 'text/plain' });

    const artifact = readBodyDraft({ kind: 'artifact', from: 'report_file', content_type: 'application/pdf' });
    expect(artifact.source).toBe('from');
    expect(writeBodyDraft(artifact)).toEqual({
      kind: 'artifact',
      from: 'report_file',
      content_type: 'application/pdf',
    });
    expect(writeBodyDraft({ kind: 'artifact', source: 'value', value: 'report_file', contentType: 'text/csv' })).toEqual({
      kind: 'artifact',
      from: 'report_file',
      content_type: 'text/csv',
    });

    expect(readBodyDraft({ kind: 'form', from: 'x' }).kind).toBe('none');

    const idem = readIdempotencyDraft({ from: 'run_id' });
    expect(writeIdempotencyDraft(idem)).toEqual({ from: 'run_id' });
    expect(writeIdempotencyDraft({ mode: 'none', value: '' })).toBeUndefined();
  });
});
